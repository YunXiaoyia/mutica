package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Tests for the pipeline chat bridge and the chat notify endpoint (WP-3,
// docs/aris-paper-pipeline.md): creator-only notify permissions, and the
// task-outcome → orchestrator-chat report path.

func TestChatNotify_CreatorQueuesTurn_OthersRejected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	agentID := createHandlerTestAgent(t, "Notify Orchestrator", nil)
	sessionID := dbfx.ChatSession(t, agentID)

	otherUser := dbfx.User(t, "notify-other", "notify-other@example.com")
	otherMember := dbfx.Member(t, testWorkspaceID, otherUser, "member")
	_ = otherMember
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(ctx, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
	})

	// The creator notifies: accepted, one chat turn enqueued for the agent.
	req := newRequest("POST", "/api/chat/sessions/"+sessionID+"/notify", map[string]any{
		"content": "[pipeline report] stage 1 finished",
	})
	req = withURLParam(req, "sessionId", sessionID)
	req = withChatTestWorkspaceCtx(t, req)
	testutil.Call(t, testHandler.NotifyChatSession, req).Want(http.StatusAccepted)

	var messages int
	dbfx.QueryRow(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID).Scan(&messages)
	if messages == 0 {
		t.Fatal("notify must persist the report as a chat message")
	}
	var tasks int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1 AND status = 'queued'`, sessionID).Scan(&tasks)
	if tasks != 1 {
		t.Fatalf("notify must enqueue exactly 1 orchestrator turn, got %d", tasks)
	}

	// Another member is refused: the session is creator-only.
	reqOther := newRequestAs(otherUser, "POST", "/api/chat/sessions/"+sessionID+"/notify", map[string]any{
		"content": "intrusion",
	})
	reqOther = withURLParam(reqOther, "sessionId", sessionID)
	reqOther = withChatTestWorkspaceCtx(t, reqOther)
	testutil.Call(t, testHandler.NotifyChatSession, reqOther).Want(http.StatusForbidden)

	// Empty content is a 400, not a silent turn.
	reqEmpty := newRequest("POST", "/api/chat/sessions/"+sessionID+"/notify", map[string]any{"content": ""})
	reqEmpty = withURLParam(reqEmpty, "sessionId", sessionID)
	reqEmpty = withChatTestWorkspaceCtx(t, reqEmpty)
	testutil.Call(t, testHandler.NotifyChatSession, reqEmpty).Want(http.StatusBadRequest)
}

func TestPipelineChatBridge_ReportsTaskOutcomeIntoChat(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	previousWindow := PipelineReportCoalesceWindow
	PipelineReportCoalesceWindow = 50 * time.Millisecond
	t.Cleanup(func() { PipelineReportCoalesceWindow = previousWindow })

	agentID := createHandlerTestAgent(t, "Bridge Orchestrator", nil)
	sessionID := dbfx.ChatSession(t, agentID)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
		testPool.Exec(ctx, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
	})

	// A pipeline child issue bound to the orchestrator's session.
	childID := dbfx.Issue(t, "Bridge stage issue")
	var rawSession []byte
	dbfx.QueryRow(t, `SELECT metadata FROM issue WHERE id = $1`, childID).Scan(&rawSession)
	_ = rawSession
	dbfx.Exec(t, `
		UPDATE issue SET metadata = metadata || jsonb_build_object('orchestrator_session', $2::text, 'pipeline_root', $3::text)
		WHERE id = $1`, childID, sessionID, childID)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, childID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, childID)
	})

	// Seed a running task on the child, then publish its completion.
	taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, childID)

	bus := events.New()
	RegisterPipelineChatBridge(bus, testHandler.TaskService)
	bus.Publish(events.Event{
		Type:    protocol.EventTaskCompleted,
		Payload: map[string]any{"task_id": taskID},
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		var messages int
		var content string
		dbfx.QueryRow(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID).Scan(&messages)
		if messages > 0 {
			dbfx.QueryRow(t, `SELECT content FROM chat_message WHERE chat_session_id = $1 LIMIT 1`, sessionID).Scan(&content)
			if !strings.Contains(content, "pipeline report") {
				t.Fatalf("bridge report must be the persisted message, got %q", content)
			}
			// The issue status rides along: the orchestrator needs the
			// completed-task-but-issue-still-in_progress mismatch visible
			// to rerun a stage instead of advancing the gate.
			if !strings.Contains(content, "issue status:") {
				t.Fatalf("bridge report must carry the issue status, got %q", content)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bridge must report the task outcome into the orchestrator chat session")
		}
		time.Sleep(25 * time.Millisecond)
	}

	// The report enqueued an orchestrator turn.
	var tasks int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1 AND status = 'queued'`, sessionID).Scan(&tasks)
	if tasks != 1 {
		t.Fatalf("bridge must enqueue exactly 1 orchestrator turn, got %d", tasks)
	}

	// Events for issues without pipeline metadata are ignored: publish a
	// completion for a plain issue and assert no second turn appears.
	plainID := dbfx.Issue(t, "Non pipeline issue")
	plainTask := createHandlerTestTaskForAgentOnIssue(t, agentID, plainID)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, plainID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, plainID)
	})
	bus.Publish(events.Event{
		Type:    protocol.EventTaskCompleted,
		Payload: map[string]any{"task_id": plainTask},
	})
	time.Sleep(200 * time.Millisecond)
	var turns int
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1 AND status = 'queued'`, sessionID).Scan(&turns)
	if turns != 1 {
		t.Fatalf("non-pipeline completions must not wake the orchestrator, got %d turns", turns)
	}

	// A pipeline-tagged issue with no bound session must not wake the
	// orchestrator either (the warn path) — an unbound pipeline stalls loudly
	// in the logs instead of reporting into an unrelated chat.
	unboundID := dbfx.Issue(t, "Unbound pipeline issue")
	dbfx.Exec(t, `
		UPDATE issue SET metadata = metadata || jsonb_build_object('pipeline_root', $2::text)
		WHERE id = $1`, unboundID, unboundID)
	unboundTask := createHandlerTestTaskForAgentOnIssue(t, agentID, unboundID)
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, unboundID)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, unboundID)
	})
	bus.Publish(events.Event{
		Type:    protocol.EventTaskCompleted,
		Payload: map[string]any{"task_id": unboundTask},
	})
	time.Sleep(200 * time.Millisecond)
	dbfx.QueryRow(t, `SELECT count(*) FROM agent_task_queue WHERE chat_session_id = $1 AND status = 'queued'`, sessionID).Scan(&turns)
	if turns != 1 {
		t.Fatalf("unbound pipeline completions must not wake the orchestrator, got %d turns", turns)
	}
}

func TestPipelineChatBridge_ReportsTaskFailedOutcomeIntoChat(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	previousWindow := PipelineReportCoalesceWindow
	PipelineReportCoalesceWindow = 50 * time.Millisecond
	t.Cleanup(func() { PipelineReportCoalesceWindow = previousWindow })

	tests := []struct {
		name         string
		payloadExtra map[string]any
		wantContains []string
		dontContains []string
	}{
		{
			name: "retry_pending",
			payloadExtra: map[string]any{
				"retry_pending":  true,
				"failure_reason": "timeout",
			},
			wantContains: []string{
				"pipeline report",
				"issue status:",
				"automatic retry is already queued",
				"failure_reason: timeout",
				"Do NOT rerun",
			},
		},
		{
			name: "terminal",
			payloadExtra: map[string]any{
				"retry_pending":  false,
				"failure_reason": "agent_error",
			},
			wantContains: []string{
				"pipeline report",
				"issue status:",
				"terminal",
				"failure_reason: agent_error",
			},
			dontContains: []string{
				"Do NOT rerun",
			},
		},
		{
			name:         "missing_fields",
			payloadExtra: map[string]any{},
			wantContains: []string{
				"pipeline report",
				"issue status:",
				"failure_reason: unknown",
			},
			dontContains: []string{
				"Do NOT rerun",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agentID := createHandlerTestAgent(t, "Bridge Failed Orchestrator "+tc.name, nil)
			sessionID := dbfx.ChatSession(t, agentID)
			t.Cleanup(func() {
				testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE chat_session_id = $1`, sessionID)
				testPool.Exec(ctx, `DELETE FROM chat_message WHERE chat_session_id = $1`, sessionID)
			})

			childID := dbfx.Issue(t, "Bridge failed stage issue "+tc.name)
			dbfx.Exec(t, `
				UPDATE issue SET metadata = metadata || jsonb_build_object('orchestrator_session', $2::text, 'pipeline_root', $3::text)
				WHERE id = $1`, childID, sessionID, childID)
			t.Cleanup(func() {
				testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, childID)
				testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, childID)
			})

			taskID := createHandlerTestTaskForAgentOnIssue(t, agentID, childID)

			bus := events.New()
			RegisterPipelineChatBridge(bus, testHandler.TaskService)

			payload := map[string]any{
				"task_id": taskID,
			}
			for k, v := range tc.payloadExtra {
				payload[k] = v
			}

			bus.Publish(events.Event{
				Type:    protocol.EventTaskFailed,
				Payload: payload,
			})

			deadline := time.Now().Add(5 * time.Second)
			for {
				var messages int
				var content string
				dbfx.QueryRow(t, `SELECT count(*) FROM chat_message WHERE chat_session_id = $1`, sessionID).Scan(&messages)
				if messages > 0 {
					dbfx.QueryRow(t, `SELECT content FROM chat_message WHERE chat_session_id = $1 LIMIT 1`, sessionID).Scan(&content)
					for _, want := range tc.wantContains {
						if !strings.Contains(content, want) {
							t.Fatalf("expected report to contain %q, got %q", want, content)
						}
					}
					for _, dont := range tc.dontContains {
						if strings.Contains(content, dont) {
							t.Fatalf("expected report NOT to contain %q, got %q", dont, content)
						}
					}
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("bridge must report the task outcome into the orchestrator chat session")
				}
				time.Sleep(25 * time.Millisecond)
			}
		})
	}
}

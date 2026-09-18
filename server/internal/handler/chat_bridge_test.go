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
}

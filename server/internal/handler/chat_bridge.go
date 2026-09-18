package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/service"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Pipeline chat bridge (WP-3, docs/aris-paper-pipeline.md): when a task on a
// pipeline child issue completes or fails, report the outcome into the
// orchestrator's chat session and enqueue a turn so the orchestrator can
// summarize progress, run acceptance checks, and decide the next step —
// without the user ever leaving the conversation.
//
// The wiring is metadata-driven: a task's issue (or its pipeline root) must
// carry `orchestrator_session` metadata, stamped at instantiation. Issues
// outside a pipeline have no such metadata and cost one JSON decode.

// PipelineReportCoalesceWindow batches deliveries per chat session so a batch
// that closes several stages at once wakes the orchestrator once. A variable
// so tests can shorten the wait.
var PipelineReportCoalesceWindow = 30 * time.Second

// pipelineChatBridge accumulates per-session reports and flushes them as one
// chat turn after the coalesce window.
type pipelineChatBridge struct {
	taskSvc *service.TaskService
	ctx     context.Context

	mu      sync.Mutex
	pending map[pgtype.UUID][]string
	timers  map[pgtype.UUID]*time.Timer
}

// RegisterPipelineChatBridge subscribes the bridge to task completion and
// failure events.
func RegisterPipelineChatBridge(bus *events.Bus, taskSvc *service.TaskService) {
	b := &pipelineChatBridge{
		taskSvc: taskSvc,
		ctx:     context.Background(),
		pending: make(map[pgtype.UUID][]string),
		timers:  make(map[pgtype.UUID]*time.Timer),
	}
	handler := func(e events.Event) { b.onTaskEvent(e) }
	bus.Subscribe(protocol.EventTaskCompleted, handler)
	bus.Subscribe(protocol.EventTaskFailed, handler)
}

func (b *pipelineChatBridge) onTaskEvent(e events.Event) {
	payload, ok := e.Payload.(map[string]any)
	if !ok {
		return
	}
	taskID, ok := payload["task_id"].(string)
	if !ok || taskID == "" {
		return
	}
	task, err := b.taskSvc.Queries.GetAgentTask(b.ctx, parseUUID(taskID))
	if err != nil || !task.IssueID.Valid {
		return
	}
	issue, err := b.taskSvc.Queries.GetIssue(b.ctx, task.IssueID)
	if err != nil {
		return
	}
	sessionID := b.orchestratorSessionFor(issue)
	if !sessionID.Valid {
		return
	}
	session, err := b.taskSvc.Queries.GetChatSession(b.ctx, sessionID)
	if err != nil || session.Status != "active" {
		return
	}

	outcome := "completed"
	if e.Type == protocol.EventTaskFailed {
		outcome = "FAILED"
	}
	b.queue(sessionID, fmt.Sprintf(
		"[pipeline report] The task on issue %s — %q — has %s. Check `multica issue list --metadata pipeline_root=%s` for the full plan and continue the pipeline.",
		uuidToString(issue.ID), issue.Title, outcome, b.pipelineRootOf(issue)))
}

// orchestratorSessionFor resolves the bound chat session: the issue's own
// orchestrator_session metadata, falling back to its pipeline root's.
func (b *pipelineChatBridge) orchestratorSessionFor(issue db.Issue) pgtype.UUID {
	if id, ok := metadataString(issue.Metadata, "orchestrator_session"); ok {
		return parseUUID(id)
	}
	if !issue.ParentIssueID.Valid {
		return pgtype.UUID{}
	}
	root, err := b.taskSvc.Queries.GetIssue(b.ctx, issue.ParentIssueID)
	if err != nil {
		return pgtype.UUID{}
	}
	if id, ok := metadataString(root.Metadata, "orchestrator_session"); ok {
		return parseUUID(id)
	}
	return pgtype.UUID{}
}

func (b *pipelineChatBridge) pipelineRootOf(issue db.Issue) string {
	if root, ok := metadataString(issue.Metadata, "pipeline_root"); ok {
		return root
	}
	return uuidToString(issue.ParentIssueID)
}

func metadataString(raw []byte, key string) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", false
	}
	v, ok := m[key].(string)
	return v, ok
}

// queue appends the report and (re)arms the flush timer for the session.
func (b *pipelineChatBridge) queue(sessionID pgtype.UUID, report string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pending[sessionID] = append(b.pending[sessionID], report)
	if _, ok := b.timers[sessionID]; !ok {
		b.timers[sessionID] = time.AfterFunc(PipelineReportCoalesceWindow, func() {
			b.flush(sessionID)
		})
	}
}

// flush delivers the coalesced reports as one chat turn for the session's
// agent. Delivery is best-effort: a failure is logged, not retried — the
// issue timeline still carries the authoritative outcome.
func (b *pipelineChatBridge) flush(sessionID pgtype.UUID) {
	b.mu.Lock()
	reports := b.pending[sessionID]
	delete(b.pending, sessionID)
	delete(b.timers, sessionID)
	b.mu.Unlock()
	if len(reports) == 0 {
		return
	}

	session, err := b.taskSvc.Queries.GetChatSession(b.ctx, sessionID)
	if err != nil {
		slog.Warn("pipeline bridge: session missing", "session_id", uuidToString(sessionID))
		return
	}
	agent, err := b.taskSvc.Queries.GetAgent(b.ctx, session.AgentID)
	if err != nil {
		slog.Warn("pipeline bridge: agent missing", "agent_id", uuidToString(session.AgentID))
		return
	}
	content := strings.Join(reports, "\n")
	if _, err := b.taskSvc.SendDirectChatMessage(b.ctx, session, agent, session.CreatorID, content, nil, "", pgtype.UUID{}); err != nil {
		slog.Warn("pipeline bridge: failed to deliver report",
			"error", err, "session_id", uuidToString(sessionID), "reports", len(reports))
	}
}

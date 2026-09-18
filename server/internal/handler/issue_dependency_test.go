package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Tests for the blocked_by dependency gate and server-driven release
// (WP-1, docs/aris-paper-pipeline.md). The gate holds assign/status-derived
// runs while any blocker is non-terminal; release posts a system comment on
// the dependent and wakes its assignee through the mention path.

type dependencyFixture struct {
	agentID    string
	blockerID  string
	dependent  string
	dependent2 string
}

func setupDependencyFixture(t *testing.T) dependencyFixture {
	t.Helper()
	ctx := context.Background()
	fx := dependencyFixture{
		agentID:    createHandlerTestAgent(t, "Dependency Gate Agent", nil),
		blockerID:  dbfx.Issue(t, "Dependency blocker"),
		dependent:  dbfx.Issue(t, "Dependent issue"),
		dependent2: dbfx.Issue(t, "Second dependent"),
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM comment WHERE issue_id IN ($1, $2, $3)`, fx.blockerID, fx.dependent, fx.dependent2)
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id IN ($1, $2, $3)`, fx.blockerID, fx.dependent, fx.dependent2)
		testPool.Exec(ctx, `DELETE FROM issue_dependency WHERE issue_id IN ($1, $2, $3) OR depends_on_issue_id IN ($1, $2, $3)`, fx.blockerID, fx.dependent, fx.dependent2)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id IN ($1, $2, $3)`, fx.blockerID, fx.dependent, fx.dependent2)
	})
	return fx
}

func addBlockedByEdge(t *testing.T, dependent, blocker string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO issue_dependency (issue_id, depends_on_issue_id, type)
		VALUES ($1, $2, 'blocked_by')`, dependent, blocker); err != nil {
		t.Fatalf("seed blocked_by edge: %v", err)
	}
}

func assignAgentViaAPI(t *testing.T, issueID, agentID string) {
	t.Helper()
	req := newRequest("PUT", "/api/issues/"+issueID, map[string]any{
		"assignee_type": "agent",
		"assignee_id":   agentID,
	})
	req = withURLParam(req, "id", issueID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
}

func setStatusViaAPI(t *testing.T, issueID, status string) {
	t.Helper()
	req := newRequest("PUT", "/api/issues/"+issueID, map[string]any{"status": status})
	req = withURLParam(req, "id", issueID)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
}

func queuedTaskCount(t *testing.T, issueID, agentID string) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t,
		`SELECT count(*) FROM agent_task_queue WHERE issue_id = $1 AND agent_id = $2 AND status = 'queued'`,
		issueID, agentID).Scan(&n)
	return n
}

func systemCommentCount(t *testing.T, issueID string) int {
	t.Helper()
	var n int
	dbfx.QueryRow(t,
		`SELECT count(*) FROM comment WHERE issue_id = $1 AND author_type = 'system' AND type = 'system'`,
		issueID).Scan(&n)
	return n
}

func TestDependencyGate_BlockedAssigneeDoesNotRunUntilBlockerDone(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := setupDependencyFixture(t)
	addBlockedByEdge(t, fx.dependent, fx.blockerID)

	// The gate holds the assign trigger: the write succeeds, no run starts.
	assignAgentViaAPI(t, fx.dependent, fx.agentID)
	if got := queuedTaskCount(t, fx.dependent, fx.agentID); got != 0 {
		t.Fatalf("blocked dependent must not enqueue on assign, got %d queued tasks", got)
	}

	// Preview shares the predicate: it must not promise a run for a write the
	// gate would withhold.
	req := newRequest("POST", "/api/issues/preview-trigger", map[string]any{
		"issue_ids":     []string{fx.dependent},
		"assignee_type": "agent",
		"assignee_id":   fx.agentID,
	})
	w := testutil.Call(t, testHandler.PreviewIssueTrigger, req).Want(http.StatusOK)
	var preview IssueTriggerPreviewResponse
	json.NewDecoder(w.Body).Decode(&preview)
	if preview.TotalCount != 0 {
		t.Fatalf("preview must not promise a run for a blocked issue, got %d triggers", preview.TotalCount)
	}

	// Completing the blocker releases the dependent: system comment + one run.
	setStatusViaAPI(t, fx.blockerID, "done")
	if got := queuedTaskCount(t, fx.dependent, fx.agentID); got != 1 {
		t.Fatalf("terminal blocker must release exactly 1 run, got %d", got)
	}
	if got := systemCommentCount(t, fx.dependent); got != 1 {
		t.Fatalf("release must post exactly 1 system comment, got %d", got)
	}
	var content string
	dbfx.QueryRow(t,
		`SELECT content FROM comment WHERE issue_id = $1 AND author_type = 'system' AND type = 'system' LIMIT 1`,
		fx.dependent).Scan(&content)
	if content == "" || !strings.Contains(content, "unblocked") {
		t.Fatalf("release comment must announce the unblock, got: %q", content)
	}
}

func TestDependencyGate_PartialBlockersHoldReleaseUntilAllTerminal(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := setupDependencyFixture(t)
	blocker2 := dbfx.Issue(t, "Second blocker")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1`, blocker2)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, blocker2)
	})
	addBlockedByEdge(t, fx.dependent, fx.blockerID)
	addBlockedByEdge(t, fx.dependent, blocker2)
	assignAgentViaAPI(t, fx.dependent, fx.agentID)

	setStatusViaAPI(t, fx.blockerID, "done")
	if got := queuedTaskCount(t, fx.dependent, fx.agentID); got != 0 {
		t.Fatalf("one of two blockers done must not release, got %d tasks", got)
	}
	if got := systemCommentCount(t, fx.dependent); got != 0 {
		t.Fatalf("partial release must not comment, got %d comments", got)
	}

	setStatusViaAPI(t, blocker2, "done")
	if got := queuedTaskCount(t, fx.dependent, fx.agentID); got != 1 {
		t.Fatalf("all blockers terminal must release exactly 1 run, got %d", got)
	}
	if got := systemCommentCount(t, fx.dependent); got != 1 {
		t.Fatalf("release must post exactly 1 comment, got %d", got)
	}
}

func TestDependencyGate_CancelledBlockerReleasesWithWarning(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := setupDependencyFixture(t)
	addBlockedByEdge(t, fx.dependent, fx.blockerID)
	assignAgentViaAPI(t, fx.dependent, fx.agentID)

	setStatusViaAPI(t, fx.blockerID, "cancelled")
	if got := queuedTaskCount(t, fx.dependent, fx.agentID); got != 1 {
		t.Fatalf("cancelled blocker must release the dependent, got %d tasks", got)
	}
	var content string
	dbfx.QueryRow(t,
		`SELECT content FROM comment WHERE issue_id = $1 AND author_type = 'system' AND type = 'system' LIMIT 1`,
		fx.dependent).Scan(&content)
	if !strings.Contains(content, "cancelled") {
		t.Fatalf("release after cancellation must carry the cancelled warning, got: %q", content)
	}
}

func TestDependencyGate_BacklogDependentStaysParked(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := setupDependencyFixture(t)
	dbfx.Exec(t, `UPDATE issue SET status = 'backlog' WHERE id = $1`, fx.dependent)
	addBlockedByEdge(t, fx.dependent, fx.blockerID)
	assignAgentViaAPI(t, fx.dependent, fx.agentID)

	// Blocker done: the backlog parking lot outranks dependency release.
	setStatusViaAPI(t, fx.blockerID, "done")
	if got := queuedTaskCount(t, fx.dependent, fx.agentID); got != 0 {
		t.Fatalf("backlog dependent must stay parked on release, got %d tasks", got)
	}
	if got := systemCommentCount(t, fx.dependent); got != 0 {
		t.Fatalf("backlog dependent must not receive a release comment, got %d", got)
	}

	// Promotion later passes the gate (blockers terminal) and fires the run.
	req := newRequest("PUT", "/api/issues/"+fx.dependent, map[string]any{"status": "todo"})
	req = withURLParam(req, "id", fx.dependent)
	testutil.Call(t, testHandler.UpdateIssue, req).Want(http.StatusOK)
	if got := queuedTaskCount(t, fx.dependent, fx.agentID); got != 1 {
		t.Fatalf("promoting a released backlog issue must enqueue 1 run, got %d", got)
	}
}

func TestDependencyGate_CycleRejected(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := setupDependencyFixture(t)
	addBlockedByEdge(t, fx.dependent, fx.blockerID)

	req := newRequest("POST", "/api/issues/"+fx.blockerID+"/dependencies", map[string]any{
		"depends_on_issue_id": fx.dependent,
	})
	req = withURLParam(req, "id", fx.blockerID)
	testutil.Call(t, testHandler.CreateIssueDependency, req).Want(http.StatusBadRequest)

	// Self-dependency is rejected too.
	req = newRequest("POST", "/api/issues/"+fx.blockerID+"/dependencies", map[string]any{
		"depends_on_issue_id": fx.blockerID,
	})
	req = withURLParam(req, "id", fx.blockerID)
	testutil.Call(t, testHandler.CreateIssueDependency, req).Want(http.StatusBadRequest)
}

func TestDependencyGate_RemoveEdgeReleases(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := setupDependencyFixture(t)
	addBlockedByEdge(t, fx.dependent, fx.blockerID)
	assignAgentViaAPI(t, fx.dependent, fx.agentID)
	if got := queuedTaskCount(t, fx.dependent, fx.agentID); got != 0 {
		t.Fatalf("precondition: blocked dependent must hold, got %d tasks", got)
	}

	// List reflects the edge.
	listReq := withURLParam(newRequest("GET", "/api/issues/"+fx.dependent+"/dependencies", nil), "id", fx.dependent)
	w := testutil.Call(t, testHandler.ListIssueDependencies, listReq).Want(http.StatusOK)
	var deps IssueDependenciesResponse
	json.NewDecoder(w.Body).Decode(&deps)
	if len(deps.BlockedBy) != 1 || deps.BlockedBy[0].IssueID != fx.blockerID {
		t.Fatalf("expected 1 blocked_by entry, got %+v", deps.BlockedBy)
	}

	// Removing the only blocker releases the dependent. withURLParam replaces
	// the whole route context, so both params go in via one helper call.
	delReq := newRequest("DELETE", "/api/issues/"+fx.dependent+"/dependencies/"+fx.blockerID, nil)
	delReq = withURLParams(delReq, "id", fx.dependent, "dependsOnId", fx.blockerID)
	testutil.Call(t, testHandler.DeleteIssueDependency, delReq).Want(http.StatusOK)

	if got := queuedTaskCount(t, fx.dependent, fx.agentID); got != 1 {
		t.Fatalf("removing the last blocker must release exactly 1 run, got %d", got)
	}
	if got := systemCommentCount(t, fx.dependent); got != 1 {
		t.Fatalf("edge-removal release must post 1 comment, got %d", got)
	}
}

func TestDependencyGate_BatchTerminalBlockersReleaseOnce(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := setupDependencyFixture(t)
	blocker2 := dbfx.Issue(t, "Batch second blocker")
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM comment WHERE issue_id = $1`, blocker2)
		testPool.Exec(context.Background(), `DELETE FROM issue WHERE id = $1`, blocker2)
	})
	addBlockedByEdge(t, fx.dependent, fx.blockerID)
	addBlockedByEdge(t, fx.dependent, blocker2)
	assignAgentViaAPI(t, fx.dependent, fx.agentID)

	req := newRequest("POST", "/api/issues/batch-update", map[string]any{
		"issue_ids": []string{fx.blockerID, blocker2},
		"updates":   map[string]any{"status": "done"},
	})
	testutil.Call(t, testHandler.BatchUpdateIssues, req).Want(http.StatusOK)

	if got := queuedTaskCount(t, fx.dependent, fx.agentID); got != 1 {
		t.Fatalf("batch-completing both blockers must release exactly 1 run, got %d", got)
	}
	if got := systemCommentCount(t, fx.dependent); got != 1 {
		t.Fatalf("batch release must post exactly 1 comment, got %d", got)
	}
}

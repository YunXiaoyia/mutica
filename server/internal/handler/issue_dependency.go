package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/issuestatus"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// This file activates issue_dependency for execution (WP-1,
// docs/aris-paper-pipeline.md). blocked_by is the enforced relation: an issue
// holding a blocked_by edge to a non-terminal blocker does not auto-start
// agent runs (the gate lives in IssueService.WillEnqueueRun). Release is
// server-driven from here: when the last blocker reaches a terminal status —
// or a blocker edge is removed — the dependent gets a system comment and its
// agent/squad assignee is woken through the same mention path the child-done
// barrier uses, which is deliberately independent of the write-trigger
// predicate.
//
// 'blocks' edges are never stored: A blocks B is representable (and rendered)
// as B blocked_by A, so storing both directions could only drift. 'related'
// stays an inert label and is not accepted by this API yet.

// notifyDependentsOfTerminal fires the dependency release for a single issue
// write that transitions an issue into a terminal status. Mirrors
// notifyParentOfChildDone: guards on the transition itself (non-terminal →
// terminal, canonical statuses so custom done/cancelled keys release too),
// best-effort, and never fails the status write.
func (h *Handler) notifyDependentsOfTerminal(ctx context.Context, prev, issue db.Issue) {
	prevStatus := issuestatus.Effective(ctx, h.Queries, issue.WorkspaceID, prev.Status)
	nowStatus := issuestatus.Effective(ctx, h.Queries, issue.WorkspaceID, issue.Status)
	if isTerminalChildStatus(prevStatus) || !isTerminalChildStatus(nowStatus) {
		return
	}
	h.releaseDependentIssues(ctx, issue)
}

// notifyDependentsOfBatchTerminal releases dependents for a whole batch after
// every status write has committed. Blockers completing in one batch are
// folded first: a dependent blocked by several of them must release exactly
// once (the pending-task dedup would collapse duplicate enqueues, but not
// duplicate system comments).
func (h *Handler) notifyDependentsOfBatchTerminal(ctx context.Context, completed []db.Issue) {
	if len(completed) == 0 {
		return
	}
	type release struct {
		dependent db.Issue
		blocker   db.Issue
	}
	seen := make(map[pgtype.UUID]struct{})
	var releases []release
	for _, blocker := range completed {
		dependents, err := h.Queries.ListDependentIssues(ctx, blocker.ID)
		if err != nil {
			slog.Warn("dependency release: failed to list dependents",
				"error", err, "blocker_id", uuidToString(blocker.ID))
			continue
		}
		for _, dependent := range dependents {
			if _, ok := seen[dependent.ID]; ok {
				continue
			}
			seen[dependent.ID] = struct{}{}
			releases = append(releases, release{dependent: dependent, blocker: blocker})
		}
	}
	for _, r := range releases {
		// releaseDependent re-checks the full blocker set, so naming the first
		// folded blocker is safe: if another blocker is still open, it bails.
		h.releaseDependent(ctx, r.dependent, r.blocker, false)
	}
}

// releaseDependentIssues wakes every issue blocked by `blocker` whose
// dependency set is now fully terminal.
func (h *Handler) releaseDependentIssues(ctx context.Context, blocker db.Issue) {
	dependents, err := h.Queries.ListDependentIssues(ctx, blocker.ID)
	if err != nil {
		slog.Warn("dependency release: failed to list dependents",
			"error", err, "blocker_id", uuidToString(blocker.ID))
		return
	}
	for _, dependent := range dependents {
		h.releaseDependent(ctx, dependent, blocker, false)
	}
}

// releaseDependent posts the unblock system comment on `dependent` and wakes
// its assignee when every blocker is terminal. `blocker` names the edge whose
// resolution (or, with `removed`, deletion) triggered the check. A dependent
// parked in backlog stays parked — the backlog parking lot outranks dependency
// release, and its promotion later passes the write-trigger gate because all
// blockers are terminal by then. Member assignees get the comment (a release
// fires once per dependent, unlike per-child child-done noise) but never an
// enqueue.
func (h *Handler) releaseDependent(ctx context.Context, dependent, blocker db.Issue, removed bool) {
	dependentStatus := issuestatus.Effective(ctx, h.Queries, dependent.WorkspaceID, dependent.Status)
	if dependentStatus == "done" || dependentStatus == "cancelled" || dependentStatus == "backlog" {
		return
	}

	blockers, err := h.Queries.ListBlockerIssues(ctx, dependent.ID)
	if err != nil {
		slog.Warn("dependency release: failed to list blockers",
			"error", err, "dependent_id", uuidToString(dependent.ID))
		return
	}
	anyCancelled := false
	for _, b := range blockers {
		status := issuestatus.Effective(ctx, h.Queries, dependent.WorkspaceID, b.Status)
		if status != "done" && status != "cancelled" {
			return // still blocked; the completing edge was not the last one
		}
		if status == "cancelled" {
			anyCancelled = true
		}
	}

	prefix := h.getIssuePrefix(ctx, blocker.WorkspaceID)
	identifier := prefix + "-" + strconv.Itoa(int(blocker.Number))
	blockerRef := fmt.Sprintf(`[%s](mention://issue/%s) — "%s"`,
		identifier, uuidToString(blocker.ID), sanitizeChildTitleForSystemComment(blocker.Title))
	var phrase string
	if removed {
		phrase = fmt.Sprintf("Dependency %s was removed.", blockerRef)
	} else {
		phrase = fmt.Sprintf("Dependency %s is now %s.", blockerRef, issuestatus.Effective(ctx, h.Queries, blocker.WorkspaceID, blocker.Status))
	}
	content := fmt.Sprintf("%s%s All blocked_by dependencies of this issue are resolved — it is unblocked. Continue the work.",
		h.buildParentAssigneeMention(ctx, dependent), phrase)
	if anyCancelled {
		content += " The resolved set includes cancelled dependencies: confirm the cancelled work is not required before relying on these results."
	}

	created, err := h.Queries.CreateComment(ctx, db.CreateCommentParams{
		ID:          dbid.NewV7(),
		IssueID:     dependent.ID,
		WorkspaceID: dependent.WorkspaceID,
		AuthorType:  "system",
		AuthorID:    pgtype.UUID{Valid: true},
		Content:     content,
		Type:        "system",
		ParentID:    pgtype.UUID{Valid: false},
	})
	if err != nil {
		slog.Warn("dependency release: create system comment failed",
			"error", err,
			"dependent_id", uuidToString(dependent.ID),
			"blocker_id", uuidToString(blocker.ID))
		return
	}
	comment := created.Comment()

	h.publish(protocol.EventCommentCreated, uuidToString(dependent.WorkspaceID), "system", "", map[string]any{
		"comment":             commentToResponse(comment, nil, nil),
		"issue_title":         dependent.Title,
		"issue_assignee_type": textToPtr(dependent.AssigneeType),
		"issue_assignee_id":   uuidToPtr(dependent.AssigneeID),
		"issue_status":        dependent.Status,
		"issue_revision":      created.IssueRevision,
	})

	// Wake the assignee through the same surfaces the child-done barrier uses:
	// mention-path enqueue for agents, leader-role enqueue for squads. Both
	// helpers carry their own pending-task dedup and readiness guards, and the
	// mention path is intentionally independent of WillEnqueueRun, which is
	// what lets a released issue run despite the dependency gate.
	switch dependent.AssigneeType.String {
	case "agent":
		h.triggerChildDoneAgent(ctx, dependent, comment.ID)
	case "squad":
		h.triggerChildDoneSquad(ctx, dependent, comment.ID)
	}
}

// IssueDependencyItem is one issue on the other end of a dependency edge.
// Resolved reports the canonical terminality so clients can render
// "waiting on" state without resolving custom statuses themselves.
type IssueDependencyItem struct {
	IssueID  string `json:"issue_id"`
	Title    string `json:"title"`
	Status   string `json:"status"`
	Resolved bool   `json:"resolved"`
}

// IssueDependenciesResponse lists both directions of the blocked_by graph for
// one issue. BlockedBy are the issues holding this one back; Blocks are the
// issues waiting on it.
type IssueDependenciesResponse struct {
	BlockedBy []IssueDependencyItem `json:"blocked_by"`
	Blocks    []IssueDependencyItem `json:"blocks"`
}

func dependencyItem(ctx context.Context, h *Handler, issue db.Issue) IssueDependencyItem {
	status := issuestatus.Effective(ctx, h.Queries, issue.WorkspaceID, issue.Status)
	return IssueDependencyItem{
		IssueID:  uuidToString(issue.ID),
		Title:    issue.Title,
		Status:   status,
		Resolved: status == "done" || status == "cancelled",
	}
}

// ListIssueDependencies returns the blocked_by graph around one issue.
func (h *Handler) ListIssueDependencies(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	blockers, err := h.Queries.ListBlockerIssues(r.Context(), issue.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list dependencies")
		return
	}
	dependents, err := h.Queries.ListDependentIssues(r.Context(), issue.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list dependencies")
		return
	}
	resp := IssueDependenciesResponse{BlockedBy: make([]IssueDependencyItem, 0, len(blockers)), Blocks: make([]IssueDependencyItem, 0, len(dependents))}
	for _, b := range blockers {
		resp.BlockedBy = append(resp.BlockedBy, dependencyItem(r.Context(), h, b))
	}
	for _, d := range dependents {
		resp.Blocks = append(resp.Blocks, dependencyItem(r.Context(), h, d))
	}
	writeJSON(w, http.StatusOK, resp)
}

// CreateIssueDependencyRequest adds one blocked_by edge. Only blocked_by is
// accepted: the blocks direction renders from the inverse edge, and related
// is inert today.
type CreateIssueDependencyRequest struct {
	DependsOnIssueID string `json:"depends_on_issue_id"`
	Type             string `json:"type"`
}

// CreateIssueDependency adds a blocked_by edge with cycle rejection. Adding a
// blocker never fires side effects — it can only hold work back, and the
// write-trigger gate picks the edge up on the next trigger evaluation.
func (h *Handler) CreateIssueDependency(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	var req CreateIssueDependencyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	depType := req.Type
	if depType == "" {
		depType = "blocked_by"
	}
	if depType != "blocked_by" {
		writeError(w, http.StatusBadRequest, "only type 'blocked_by' is supported; express 'blocks' as the inverse blocked_by edge")
		return
	}
	dependsOn, parseOK := parseUUIDOrBadRequest(w, req.DependsOnIssueID, "depends_on_issue_id")
	if !parseOK {
		return
	}
	if uuidToString(dependsOn) == uuidToString(issue.ID) {
		writeError(w, http.StatusBadRequest, "an issue cannot depend on itself")
		return
	}
	target, err := h.Queries.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{
		ID:          dependsOn,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "depends_on_issue_id does not exist in this workspace")
		return
	}
	_ = target

	if existing, err := h.Queries.GetIssueDependencyEdge(r.Context(), db.GetIssueDependencyEdgeParams{
		IssueID:          issue.ID,
		DependsOnIssueID: dependsOn,
		Type:             depType,
	}); err == nil {
		writeError(w, http.StatusConflict, "dependency already exists: "+uuidToString(existing.ID))
		return
	}

	if h.blockedByWouldCycle(r.Context(), issue.WorkspaceID, issue.ID, dependsOn) {
		writeError(w, http.StatusBadRequest, "dependency would create a blocked_by cycle")
		return
	}

	edge, err := h.Queries.CreateIssueDependency(r.Context(), db.CreateIssueDependencyParams{
		IssueID:          issue.ID,
		DependsOnIssueID: dependsOn,
		Type:             depType,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create dependency")
		return
	}
	writeJSON(w, http.StatusCreated, edge)
}

// DeleteIssueDependency removes a blocked_by edge. Removing the last
// unresolved blocker releases the dependent through the same path a terminal
// blocker uses — manual unblocking must not strand work that the gate held.
func (h *Handler) DeleteIssueDependency(w http.ResponseWriter, r *http.Request) {
	issue, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	dependsOn, parseOK := parseUUIDOrBadRequest(w, chi.URLParam(r, "dependsOnId"), "dependsOnId")
	if !parseOK {
		return
	}
	deletedID, err := h.Queries.DeleteIssueDependency(r.Context(), db.DeleteIssueDependencyParams{
		IssueID:          issue.ID,
		DependsOnIssueID: dependsOn,
		Type:             "blocked_by",
	})
	if err != nil {
		writeError(w, http.StatusNotFound, "dependency not found")
		return
	}
	_ = deletedID

	blocker, err := h.Queries.GetIssueInWorkspace(r.Context(), db.GetIssueInWorkspaceParams{
		ID:          dependsOn,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		// The edge is gone either way; only the release comment needs the row.
		slog.Warn("dependency release: removed blocker row missing",
			"blocker_id", uuidToString(dependsOn), "dependent_id", uuidToString(issue.ID))
	} else {
		h.releaseDependent(r.Context(), issue, blocker, true)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// blockedByWouldCycle reports whether adding issueID → dependsOnID would close
// a directed cycle in the workspace's blocked_by graph: a cycle exists iff
// dependsOnID can already reach issueID by following blocked_by edges.
func (h *Handler) blockedByWouldCycle(ctx context.Context, workspaceID pgtype.UUID, issueID, dependsOn pgtype.UUID) bool {
	edges, err := h.Queries.ListBlockedByEdgesForWorkspace(ctx, workspaceID)
	if err != nil {
		// Fail closed: refuse the edge rather than risk a cycle the release
		// walk cannot untangle.
		slog.Warn("dependency cycle check failed", "error", err, "workspace_id", uuidToString(workspaceID))
		return true
	}
	graph := make(map[string][]string, len(edges))
	for _, e := range edges {
		from := uuidToString(e.IssueID)
		graph[from] = append(graph[from], uuidToString(e.DependsOnIssueID))
	}
	start, goal := uuidToString(dependsOn), uuidToString(issueID)
	visited := make(map[string]struct{})
	queue := []string{start}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		if node == goal {
			return true
		}
		if _, ok := visited[node]; ok {
			continue
		}
		visited[node] = struct{}{}
		queue = append(queue, graph[node]...)
	}
	return false
}

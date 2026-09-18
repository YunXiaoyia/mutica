package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
)

// NotifyChatSessionRequest is the body of the agent-facing notify call.
type NotifyChatSessionRequest struct {
	Content string `json:"content"`
}

// NotifyChatSession delivers a report into a chat session and enqueues a turn
// for the session's agent — the explicit, productized form of the agent-to-
// chat wake (WP-3, docs/aris-paper-pipeline.md §7.3).
//
// Permission: the session creator (member), or an agent actor whose
// task-scoped token resolves to the creator's user id (the mat_ token carries
// the runtime owner). Anything else is 403. This turns the previously
// accidental creator-equality loophole into a documented rule while keeping
// A2A collaboration working for the single-owner self-hosted case.
func (h *Handler) NotifyChatSession(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := ctxWorkspaceID(r.Context())
	sessionID, parseOK := parseUUIDOrBadRequest(w, chi.URLParam(r, "sessionId"), "sessionId")
	if !parseOK {
		return
	}
	var req NotifyChatSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Content == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}

	session, err := h.Queries.GetChatSession(r.Context(), sessionID)
	if err != nil || session.WorkspaceID.String() != workspaceID {
		writeError(w, http.StatusNotFound, "chat session not found")
		return
	}
	if uuidToString(session.CreatorID) != userID {
		writeError(w, http.StatusForbidden, "only the session creator (or an agent running under their account) may notify this session")
		return
	}
	if session.Status != "active" {
		writeError(w, http.StatusBadRequest, "chat session is archived")
		return
	}
	agent, err := h.Queries.GetAgent(r.Context(), session.AgentID)
	if err != nil || agent.ArchivedAt.Valid {
		writeError(w, http.StatusConflict, "chat agent is unavailable")
		return
	}
	if verdict, err := service.AgentReadiness(r.Context(), h.runtimeLookup(obsmetrics.RuntimeLookupSourceChat), agent); err == nil && verdict.Blocked() {
		h.writeDispatchBlocked(w, http.StatusConflict, verdict.Reason)
		return
	}

	if _, err := h.TaskService.SendDirectChatMessage(r.Context(), session, agent, session.CreatorID, req.Content, nil, "", pgtype.UUID{}); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to enqueue notification")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
}

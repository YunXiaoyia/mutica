package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/dbid"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// Pipeline templates (WP-2, docs/aris-paper-pipeline.md). A template is a
// reusable ordered stage plan; instantiation turns it into a parent issue
// (assignee: the orchestrator agent) plus one child issue per stage row,
// wired with blocked_by edges so the dependency gate (WP-1) sequences the
// work. Children sharing a stage_order form one parallel barrier group; the
// value is copied onto the child's stage column so the existing stage
// barrier wakes the orchestrator when the group closes.
//
// Instantiation creates every child in `backlog` — the parking lot outranks
// both the dependency gate and the create-time enqueue — wires the edges,
// and only then flips stage-1 `auto` children to `todo` through
// WillEnqueueRun + dispatchIssueRun. That ordering is what makes the gate
// load-bearing from the first second: by the time stage-2 children exist,
// their blockers are already wired.

// PipelineTemplateStageRequest is one stage row on create/replace.
type PipelineTemplateStageRequest struct {
	StageOrder         int32    `json:"stage_order"`
	Name               string   `json:"name"`
	AgentID            string   `json:"agent_id"`
	SkillIDs           []string `json:"skill_ids"`
	PromptTemplate     string   `json:"prompt_template"`
	AcceptanceCriteria string   `json:"acceptance_criteria"`
	// AdvanceMode is "auto" (dependency release moves this stage) or
	// "orchestrator_review" (children instantiate in backlog and wait for the
	// AdvancePipelineRun call after the orchestrator's acceptance check).
	AdvanceMode string `json:"advance_mode"`
	// RequiresHumanGate marks a decision point the orchestrator must put to
	// the user in chat before advancing (advisory metadata on the child).
	RequiresHumanGate bool `json:"requires_human_gate"`
}

// PipelineTemplateRequest is the create/update body. Stages are replaced
// wholesale on update; omitting them keeps the existing plan.
type PipelineTemplateRequest struct {
	Name                string                         `json:"name"`
	Version             int32                          `json:"version"`
	Description         *string                        `json:"description"`
	OrchestratorAgentID string                         `json:"orchestrator_agent_id"`
	Stages              []PipelineTemplateStageRequest `json:"stages"`
}

// PipelineTemplateStageResponse mirrors one stored stage row.
type PipelineTemplateStageResponse struct {
	ID                 string   `json:"id"`
	StageOrder         int32    `json:"stage_order"`
	Name               string   `json:"name"`
	AgentID            string   `json:"agent_id"`
	SkillIDs           []string `json:"skill_ids"`
	PromptTemplate     string   `json:"prompt_template"`
	AcceptanceCriteria string   `json:"acceptance_criteria"`
	AdvanceMode        string   `json:"advance_mode"`
	RequiresHumanGate  bool     `json:"requires_human_gate"`
}

// PipelineTemplateResponse is the full template with its stage plan.
type PipelineTemplateResponse struct {
	ID                  string                          `json:"id"`
	WorkspaceID         string                          `json:"workspace_id"`
	Name                string                          `json:"name"`
	Version             int32                           `json:"version"`
	Description         string                          `json:"description"`
	OrchestratorAgentID string                          `json:"orchestrator_agent_id"`
	Stages              []PipelineTemplateStageResponse `json:"stages"`
}

func uuidSliceToStrings(ids []pgtype.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, uuidToString(id))
	}
	return out
}

func stageToResponse(s db.PipelineTemplateStage) PipelineTemplateStageResponse {
	return PipelineTemplateStageResponse{
		ID:                 uuidToString(s.ID),
		StageOrder:         s.StageOrder,
		Name:               s.Name,
		AgentID:            uuidToString(s.AgentID),
		SkillIDs:           uuidSliceToStrings(s.SkillIds),
		PromptTemplate:     s.PromptTemplate,
		AcceptanceCriteria: s.AcceptanceCriteria,
		AdvanceMode:        s.AdvanceMode,
		RequiresHumanGate:  s.RequiresHumanGate,
	}
}

func (h *Handler) templateToResponse(ctx context.Context, tpl db.PipelineTemplate) (PipelineTemplateResponse, error) {
	stages, err := h.Queries.ListPipelineTemplateStages(ctx, tpl.ID)
	if err != nil {
		return PipelineTemplateResponse{}, err
	}
	resp := PipelineTemplateResponse{
		ID:                  uuidToString(tpl.ID),
		WorkspaceID:         uuidToString(tpl.WorkspaceID),
		Name:                tpl.Name,
		Version:             tpl.Version,
		Description:         tpl.Description,
		OrchestratorAgentID: uuidToString(tpl.OrchestratorAgentID),
		Stages:              make([]PipelineTemplateStageResponse, 0, len(stages)),
	}
	for _, s := range stages {
		resp.Stages = append(resp.Stages, stageToResponse(s))
	}
	return resp, nil
}

func validAdvanceMode(mode string) bool {
	return mode == "auto" || mode == "orchestrator_review"
}

// validateTemplateStages checks the stage plan shape: non-empty, orders form
// exactly 1..K (repeated orders are parallel barrier groups), every executor
// agent exists in the workspace, and advance modes are known. Returns the
// parsed agent ids keyed to the input slice.
func (h *Handler) validateTemplateStages(ctx context.Context, wsUUID pgtype.UUID, stages []PipelineTemplateStageRequest) ([]pgtype.UUID, int, string) {
	if len(stages) == 0 {
		return nil, http.StatusBadRequest, "at least one stage is required"
	}
	orders := make(map[int32]struct{}, len(stages))
	agentIDs := make([]pgtype.UUID, len(stages))
	for i, s := range stages {
		if s.StageOrder < 1 {
			return nil, http.StatusBadRequest, "stage_order must be >= 1"
		}
		orders[s.StageOrder] = struct{}{}
		if s.Name == "" {
			return nil, http.StatusBadRequest, "stage name is required"
		}
		if !validAdvanceMode(s.AdvanceMode) {
			return nil, http.StatusBadRequest, "advance_mode must be 'auto' or 'orchestrator_review'"
		}
		agentID, err := util.ParseUUID(s.AgentID)
		if err != nil {
			return nil, http.StatusBadRequest, "stage agent_id is not a valid UUID"
		}
		if _, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: agentID, WorkspaceID: wsUUID}); err != nil {
			return nil, http.StatusBadRequest, "stage agent does not exist in this workspace"
		}
		agentIDs[i] = agentID
	}
	if len(orders) != len(stages) || len(orders) == 0 {
		return nil, http.StatusBadRequest, "stage_order values must form 1..K with no gaps"
	}
	for i := 1; i <= len(orders); i++ {
		if _, ok := orders[int32(i)]; !ok {
			return nil, http.StatusBadRequest, "stage_order values must form 1..K with no gaps"
		}
	}
	return agentIDs, 0, ""
}

func (h *Handler) stageSkillIDs(raw []string) ([]pgtype.UUID, error) {
	skillIDs := make([]pgtype.UUID, 0, len(raw))
	for _, s := range raw {
		id, err := util.ParseUUID(s)
		if err != nil {
			return nil, err
		}
		skillIDs = append(skillIDs, id)
	}
	return skillIDs, nil
}

func (h *Handler) insertTemplateStage(ctx context.Context, templateID pgtype.UUID, s PipelineTemplateStageRequest, agentID pgtype.UUID) error {
	skillIDs, err := h.stageSkillIDs(s.SkillIDs)
	if err != nil {
		return err
	}
	_, err = h.Queries.CreatePipelineTemplateStage(ctx, db.CreatePipelineTemplateStageParams{
		ID:                 dbid.NewV7(),
		TemplateID:         templateID,
		StageOrder:         s.StageOrder,
		Name:               s.Name,
		AgentID:            agentID,
		SkillIds:           skillIDs,
		PromptTemplate:     s.PromptTemplate,
		AcceptanceCriteria: s.AcceptanceCriteria,
		AdvanceMode:        s.AdvanceMode,
		RequiresHumanGate:  s.RequiresHumanGate,
	})
	return err
}

// resolveTemplateOrchestrator validates the optional orchestrator agent.
func (h *Handler) resolveTemplateOrchestrator(ctx context.Context, wsUUID pgtype.UUID, agentID string) (pgtype.UUID, bool) {
	if agentID == "" {
		return pgtype.UUID{}, true
	}
	id, err := util.ParseUUID(agentID)
	if err != nil {
		return pgtype.UUID{}, false
	}
	if _, err := h.Queries.GetAgentInWorkspace(ctx, db.GetAgentInWorkspaceParams{ID: id, WorkspaceID: wsUUID}); err != nil {
		return pgtype.UUID{}, false
	}
	return id, true
}

// ListPipelineTemplates returns every template in the workspace.
func (h *Handler) ListPipelineTemplates(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	templates, err := h.Queries.ListPipelineTemplates(r.Context(), wsUUID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list pipeline templates")
		return
	}
	resp := make([]PipelineTemplateResponse, 0, len(templates))
	for _, tpl := range templates {
		out, err := h.templateToResponse(r.Context(), tpl)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to load pipeline template stages")
			return
		}
		resp = append(resp, out)
	}
	writeJSON(w, http.StatusOK, resp)
}

// GetPipelineTemplate returns one template with its stages.
func (h *Handler) GetPipelineTemplate(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	tplID, parseOK := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "id")
	if !parseOK {
		return
	}
	tpl, err := h.Queries.GetPipelineTemplate(r.Context(), db.GetPipelineTemplateParams{ID: tplID, WorkspaceID: wsUUID})
	if err != nil {
		writeError(w, http.StatusNotFound, "pipeline template not found")
		return
	}
	resp, err := h.templateToResponse(r.Context(), tpl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load pipeline template stages")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// CreatePipelineTemplate stores a template and its stage plan.
func (h *Handler) CreatePipelineTemplate(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	var req PipelineTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if req.Version == 0 {
		req.Version = 1
	}
	agentIDs, status, msg := h.validateTemplateStages(r.Context(), wsUUID, req.Stages)
	if status != 0 {
		writeError(w, status, msg)
		return
	}
	orchestratorID, ok := h.resolveTemplateOrchestrator(r.Context(), wsUUID, req.OrchestratorAgentID)
	if !ok {
		writeError(w, http.StatusBadRequest, "orchestrator_agent_id does not exist in this workspace")
		return
	}
	creatorID, err := util.ParseUUID(userID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user")
		return
	}

	templateDescription := ""
	if req.Description != nil {
		templateDescription = *req.Description
	}
	tpl, err := h.Queries.CreatePipelineTemplate(r.Context(), db.CreatePipelineTemplateParams{
		ID:                  dbid.NewV7(),
		WorkspaceID:         wsUUID,
		Name:                req.Name,
		Version:             req.Version,
		Description:         templateDescription,
		OrchestratorAgentID: orchestratorID,
		CreatedBy:           creatorID,
	})
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "a template with this name and version already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to create pipeline template")
		return
	}
	for i, s := range req.Stages {
		if err := h.insertTemplateStage(r.Context(), tpl.ID, s, agentIDs[i]); err != nil {
			// App-level cleanup (no FKs): drop the half-built template.
			h.Queries.DeletePipelineTemplateStages(r.Context(), tpl.ID)
			h.Queries.DeletePipelineTemplate(r.Context(), db.DeletePipelineTemplateParams{ID: tpl.ID, WorkspaceID: wsUUID})
			writeError(w, http.StatusInternalServerError, "failed to store pipeline template stage")
			return
		}
	}
	resp, err := h.templateToResponse(r.Context(), tpl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load pipeline template stages")
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// UpdatePipelineTemplate replaces the description, orchestrator agent and —
// when stages are present — the whole stage plan, in place.
func (h *Handler) UpdatePipelineTemplate(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	tplID, parseOK := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "id")
	if !parseOK {
		return
	}
	tpl, err := h.Queries.GetPipelineTemplate(r.Context(), db.GetPipelineTemplateParams{ID: tplID, WorkspaceID: wsUUID})
	if err != nil {
		writeError(w, http.StatusNotFound, "pipeline template not found")
		return
	}
	var req PipelineTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	orchestratorID, ok := h.resolveTemplateOrchestrator(r.Context(), wsUUID, req.OrchestratorAgentID)
	if !ok {
		writeError(w, http.StatusBadRequest, "orchestrator_agent_id does not exist in this workspace")
		return
	}
	var replaceStages bool
	var agentIDs []pgtype.UUID
	if req.Stages != nil {
		ids, status, msg := h.validateTemplateStages(r.Context(), wsUUID, req.Stages)
		if status != 0 {
			writeError(w, status, msg)
			return
		}
		agentIDs = ids
		replaceStages = true
	}
	updatedDescription := tpl.Description
	if req.Description != nil {
		updatedDescription = *req.Description
	}
	updated, err := h.Queries.UpdatePipelineTemplate(r.Context(), db.UpdatePipelineTemplateParams{
		ID:                  tpl.ID,
		WorkspaceID:         wsUUID,
		Description:         pgtype.Text{String: updatedDescription, Valid: true},
		OrchestratorAgentID: orchestratorID,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update pipeline template")
		return
	}
	if replaceStages {
		if err := h.Queries.DeletePipelineTemplateStages(r.Context(), tpl.ID); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to replace pipeline template stages")
			return
		}
		for i, s := range req.Stages {
			if err := h.insertTemplateStage(r.Context(), tpl.ID, s, agentIDs[i]); err != nil {
				writeError(w, http.StatusInternalServerError, "failed to store pipeline template stage")
				return
			}
		}
	}
	resp, err := h.templateToResponse(r.Context(), updated)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load pipeline template stages")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// DeletePipelineTemplate removes a template and its stage plan (app-level
// cleanup — no foreign keys). Instantiated pipelines are ordinary issues and
// are deliberately untouched.
func (h *Handler) DeletePipelineTemplate(w http.ResponseWriter, r *http.Request) {
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	tplID, parseOK := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "id")
	if !parseOK {
		return
	}
	if _, err := h.Queries.DeletePipelineTemplate(r.Context(), db.DeletePipelineTemplateParams{ID: tplID, WorkspaceID: wsUUID}); err != nil {
		writeError(w, http.StatusNotFound, "pipeline template not found")
		return
	}
	h.Queries.DeletePipelineTemplateStages(r.Context(), tplID)
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// InstantiatePipelineTemplateRequest drives one pipeline run.
type InstantiatePipelineTemplateRequest struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	// OrchestratorSessionID optionally binds the run to the orchestrator's
	// chat session; the chat bridge (WP-3) reads it from the parent metadata
	// to report stage completions back into the conversation.
	OrchestratorSessionID string `json:"orchestrator_session_id"`
}

// PipelineRunChildResponse is one instantiated stage child.
type PipelineRunChildResponse struct {
	IssueID     string `json:"issue_id"`
	StageOrder  int32  `json:"stage_order"`
	Name        string `json:"name"`
	AgentID     string `json:"agent_id"`
	Status      string `json:"status"`
	AdvanceMode string `json:"advance_mode"`
}

// InstantiatePipelineResponse names the parent and the child plan.
type InstantiatePipelineResponse struct {
	RootIssueID string                     `json:"root_issue_id"`
	Children    []PipelineRunChildResponse `json:"children"`
}

// InstantiatePipelineTemplate expands the template into a live pipeline run:
// parent issue + staged children (all born in backlog) + blocked_by edges,
// then stage-1 `auto` children flip to todo through the dependency gate so
// only genuinely unblocked work ever reaches the queue.
func (h *Handler) InstantiatePipelineTemplate(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	workspaceID := h.resolveWorkspaceID(r)
	wsUUID, ok := parseUUIDOrBadRequest(w, workspaceID, "workspace_id")
	if !ok {
		return
	}
	tplID, parseOK := parseUUIDOrBadRequest(w, chi.URLParam(r, "id"), "id")
	if !parseOK {
		return
	}
	tpl, err := h.Queries.GetPipelineTemplate(r.Context(), db.GetPipelineTemplateParams{ID: tplID, WorkspaceID: wsUUID})
	if err != nil {
		writeError(w, http.StatusNotFound, "pipeline template not found")
		return
	}
	stages, err := h.Queries.ListPipelineTemplateStages(r.Context(), tpl.ID)
	if err != nil || len(stages) == 0 {
		writeError(w, http.StatusBadRequest, "pipeline template has no stages")
		return
	}
	var req InstantiatePipelineTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var sessionID pgtype.UUID
	if req.OrchestratorSessionID != "" {
		id, parseSession := parseUUIDOrBadRequest(w, req.OrchestratorSessionID, "orchestrator_session_id")
		if !parseSession {
			return
		}
		if _, err := h.Queries.GetChatSessionInWorkspace(r.Context(), db.GetChatSessionInWorkspaceParams{ID: id, WorkspaceID: wsUUID}); err != nil {
			writeError(w, http.StatusBadRequest, "orchestrator_session_id does not exist in this workspace")
			return
		}
		sessionID = id
	}
	creatorID, err := util.ParseUUID(userID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user")
		return
	}
	actorType, actorID := h.resolveActor(r, userID, workspaceID)

	prefix := h.getIssuePrefix(r.Context(), wsUUID)
	title := req.Title
	if title == "" {
		title = tpl.Name + " run"
	}
	var parentAssigneeType pgtype.Text
	var parentAssigneeID pgtype.UUID
	if tpl.OrchestratorAgentID.Valid {
		parentAssigneeType = pgtype.Text{String: "agent", Valid: true}
		parentAssigneeID = tpl.OrchestratorAgentID
	}

	broadcast := func(issue db.Issue, _ []db.Attachment, _ []db.IssueLabel) map[string]any {
		payload := issueToResponse(issue, prefix)
		h.fillStatusCategory(r.Context(), issue.WorkspaceID, &payload)
		return map[string]any{"issue": payload}
	}

	parent, err := h.IssueService.Create(r.Context(), service.IssueCreateParams{
		WorkspaceID:  wsUUID,
		Title:        title,
		Description:  ptrToText(&req.Description),
		Status:       "todo",
		Priority:     "none",
		CreatorType:  "member",
		CreatorID:    creatorID,
		AssigneeType: parentAssigneeType,
		AssigneeID:   parentAssigneeID,
	}, service.IssueCreateOpts{
		ActorID:          userID,
		BroadcastPayload: broadcast,
	})
	if err != nil {
		// Mirror CreateIssue's error mapping: an active duplicate is the one
		// client-recoverable outcome here (rerun with a different title).
		if errors.Is(err, service.ErrActiveDuplicate) {
			writeError(w, http.StatusConflict, "an active issue with this title already exists; use a different title")
			return
		}
		if errors.Is(err, service.ErrParentIssueNotFound) || errors.Is(err, service.ErrProjectNotFound) {
			writeError(w, http.StatusBadRequest, "pipeline root references a missing parent or project")
			return
		}
		slog.Warn("instantiate: root issue create failed", "error", err, "template_id", uuidToString(tpl.ID))
		writeError(w, http.StatusInternalServerError, "failed to create pipeline root issue")
		return
	}
	created := []*db.Issue{&parent.Issue}
	cleanupFailedInstantiation := func() {
		// App-level rollback (no FKs on the pipeline tables; agent_task_queue
		// rows cascade with the legacy issue FK). Best-effort: the request has
		// already failed, so a leftover row is a cleanup complaint, not a
		// second failure.
		for _, issue := range created {
			h.Queries.DeleteIssue(r.Context(), db.DeleteIssueParams{ID: issue.ID, WorkspaceID: issue.WorkspaceID})
		}
	}

	setMeta := func(issueID pgtype.UUID, key, value string) {
		encoded, _ := json.Marshal(value)
		h.Queries.SetIssueMetadataKey(r.Context(), db.SetIssueMetadataKeyParams{
			Key: key, Value: encoded, ID: issueID, WorkspaceID: wsUUID,
		})
	}
	setMeta(parent.Issue.ID, "pipeline", "active")
	setMeta(parent.Issue.ID, "pipeline_template", fmt.Sprintf("%s:v%d", tpl.Name, tpl.Version))
	if sessionID.Valid {
		setMeta(parent.Issue.ID, "orchestrator_session", uuidToString(sessionID))
	}

	// Phase A: every child is born in backlog — parked, so neither the
	// create-time enqueue nor the dependency gate can fire early.
	childrenByStage := map[int32][]db.Issue{}
	var childResponses []PipelineRunChildResponse
	for _, st := range stages {
		childDescription := interpolateStagePrompt(st.PromptTemplate, req.Title, req.Description)
		if st.AcceptanceCriteria != "" {
			childDescription += "\n\n## Acceptance criteria\n" + st.AcceptanceCriteria
		}
		child, err := h.IssueService.Create(r.Context(), service.IssueCreateParams{
			WorkspaceID:   wsUUID,
			Title:         fmt.Sprintf("[Stage %d] %s", st.StageOrder, st.Name),
			Description:   pgtype.Text{String: childDescription, Valid: childDescription != ""},
			Status:        "backlog",
			Priority:      "none",
			CreatorType:   "member",
			CreatorID:     creatorID,
			AssigneeType:  pgtype.Text{String: "agent", Valid: true},
			AssigneeID:    st.AgentID,
			ParentIssueID: parent.Issue.ID,
			Stage:         pgtype.Int4{Int32: st.StageOrder, Valid: true},
		}, service.IssueCreateOpts{
			ActorID:          userID,
			BroadcastPayload: broadcast,
		})
		if err != nil {
			cleanupFailedInstantiation()
			writeError(w, http.StatusInternalServerError, "failed to create pipeline stage issue: "+st.Name)
			return
		}
		created = append(created, &child.Issue)
		setMeta(child.Issue.ID, "pipeline_root", uuidToString(parent.Issue.ID))
		if sessionID.Valid {
			setMeta(child.Issue.ID, "orchestrator_session", uuidToString(sessionID))
		}
		setMeta(child.Issue.ID, "pipeline_stage_name", st.Name)
		setMeta(child.Issue.ID, "advance_mode", st.AdvanceMode)
		if st.RequiresHumanGate {
			setMeta(child.Issue.ID, "human_gate", "true")
		}
		childrenByStage[st.StageOrder] = append(childrenByStage[st.StageOrder], child.Issue)
		childResponses = append(childResponses, PipelineRunChildResponse{
			IssueID:     uuidToString(child.Issue.ID),
			StageOrder:  st.StageOrder,
			Name:        st.Name,
			AgentID:     uuidToString(st.AgentID),
			Status:      "backlog",
			AdvanceMode: st.AdvanceMode,
		})
	}

	// Phase B: wire each barrier group to its predecessor with blocked_by
	// edges — the dependency gate's sequencing input.
	orders := make([]int32, 0, len(childrenByStage))
	for order := range childrenByStage {
		orders = append(orders, order)
	}
	sort.Slice(orders, func(i, j int) bool { return orders[i] < orders[j] })
	for i := 1; i < len(orders); i++ {
		for _, dependent := range childrenByStage[orders[i]] {
			for _, blocker := range childrenByStage[orders[i-1]] {
				if _, err := h.Queries.CreateIssueDependency(r.Context(), db.CreateIssueDependencyParams{
					IssueID:          dependent.ID,
					DependsOnIssueID: blocker.ID,
					Type:             "blocked_by",
				}); err != nil {
					cleanupFailedInstantiation()
					writeError(w, http.StatusInternalServerError, "failed to wire pipeline dependencies")
					return
				}
			}
		}
	}

	// Phase C: every `auto` child is flipped backlog → todo through the
	// gate + dispatch, in barrier order. Stage 1 has no blockers, so it runs
	// now; later stages flip to todo but stay held by the dependency gate and
	// are released by the server when their blockers reach a terminal state.
	// orchestrator_review children stay parked until AdvancePipelineRun.
	// Flipping rather than leaving them in backlog is load-bearing: the
	// release path deliberately skips backlog dependents (the parking lot
	// outranks release), so a backlog auto child would never move.
	advanceModeByStage := map[int32]string{}
	for _, st := range stages {
		advanceModeByStage[st.StageOrder] = st.AdvanceMode
	}
	for _, order := range orders {
		if advanceModeByStage[order] != "auto" {
			continue
		}
		for _, child := range childrenByStage[order] {
			h.activatePipelineChild(r, child, actorType, actorID, prefix)
		}
	}

	writeJSON(w, http.StatusCreated, InstantiatePipelineResponse{
		RootIssueID: uuidToString(parent.Issue.ID),
		Children:    childResponses,
	})
}

// activatePipelineChild flips one parked child from backlog to todo and
// dispatches the run through the same predicate the update path uses, so a
// held issue (blocker reappeared, triage landed) stays held instead of
// bypassing the queue door.
func (h *Handler) activatePipelineChild(r *http.Request, child db.Issue, actorType, actorID, prefix string) {
	updated, err := h.Queries.UpdateIssueStatus(r.Context(), db.UpdateIssueStatusParams{
		ID: child.ID, Status: "todo", WorkspaceID: child.WorkspaceID,
	})
	if err != nil {
		return
	}
	if trigger, ok := h.IssueService.WillEnqueueRun(r.Context(),
		service.IssueTriggerInput{
			Issue:         updated,
			PrevStatus:    "backlog",
			StatusChanged: true,
		},
		h.issueTriggerWriteProbe(r, actorType, actorID, updated),
	); ok {
		h.dispatchIssueRun(r.Context(), updated, trigger, actorType, actorID, "")
	}
	resp := issueToResponse(updated, prefix)
	h.fillStatusCategory(r.Context(), updated.WorkspaceID, &resp)
	h.publish(protocol.EventIssueUpdated, uuidToString(updated.WorkspaceID), actorType, actorID, map[string]any{
		"issue":          resp,
		"status_changed": true,
		"prev_status":    "backlog",
		"issue_revision": updated.Revision,
	})
}

// interpolateStagePrompt fills the {{goal}} / {{description}} slots of a
// stage's prompt template. Unknown tokens stay verbatim — templates are
// data, not a render engine.
func interpolateStagePrompt(template, goal, description string) string {
	out := strings.ReplaceAll(template, "{{goal}}", goal)
	out = strings.ReplaceAll(out, "{{description}}", description)
	return out
}

// AdvancePipelineRunRequest names the barrier group to promote.
type AdvancePipelineRunRequest struct {
	Stage int32 `json:"stage"`
}

// AdvancePipelineRun promotes one stage's parked children to todo through
// the dependency gate — the orchestrator_review handoff after the
// orchestrator's acceptance check passes.
func (h *Handler) AdvancePipelineRun(w http.ResponseWriter, r *http.Request) {
	userID, ok := requireUserID(w, r)
	if !ok {
		return
	}
	root, ok := h.loadIssueForUser(w, r, chi.URLParam(r, "rootId"))
	if !ok {
		return
	}
	var req AdvancePipelineRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Stage < 1 {
		writeError(w, http.StatusBadRequest, "stage is required and must be >= 1")
		return
	}
	children, err := h.Queries.ListChildIssues(r.Context(), root.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list pipeline children")
		return
	}
	actorType, actorID := h.resolveActor(r, userID, uuidToString(root.WorkspaceID))
	prefix := h.getIssuePrefix(r.Context(), root.WorkspaceID)
	promoted := 0
	for _, child := range children {
		if !child.Stage.Valid || child.Stage.Int32 != req.Stage || child.Status != "backlog" {
			continue
		}
		updated, err := h.Queries.UpdateIssueStatus(r.Context(), db.UpdateIssueStatusParams{
			ID: child.ID, Status: "todo", WorkspaceID: child.WorkspaceID,
		})
		if err != nil {
			continue
		}
		if trigger, ok := h.IssueService.WillEnqueueRun(r.Context(),
			service.IssueTriggerInput{
				Issue:         updated,
				PrevStatus:    "backlog",
				StatusChanged: true,
			},
			h.issueTriggerWriteProbe(r, actorType, actorID, updated),
		); ok {
			h.dispatchIssueRun(r.Context(), updated, trigger, actorType, actorID, "")
		}
		resp := issueToResponse(updated, prefix)
		h.fillStatusCategory(r.Context(), updated.WorkspaceID, &resp)
		h.publish(protocol.EventIssueUpdated, uuidToString(updated.WorkspaceID), actorType, actorID, map[string]any{
			"issue":          resp,
			"status_changed": true,
			"prev_status":    "backlog",
			"issue_revision": updated.Revision,
		})
		promoted++
	}
	writeJSON(w, http.StatusOK, map[string]int{"promoted": promoted})
}

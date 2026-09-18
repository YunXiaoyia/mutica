package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/multica-ai/multica/server/internal/testutil"
)

// Tests for pipeline templates and instantiation (WP-2,
// docs/aris-paper-pipeline.md): template CRUD shapes, the auto-stage
// sequencing through the dependency gate, and the orchestrator_review hold +
// manual advance.

type pipelineFixture struct {
	agentIDs []string
	tplID    string
	rootID   string
	childIDs []string
}

func (f *pipelineFixture) cleanup(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, id := range append(f.childIDs, f.rootID) {
		if id == "" {
			continue
		}
		testPool.Exec(ctx, `DELETE FROM comment WHERE issue_id = $1`, id)
		testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE issue_id = $1`, id)
		testPool.Exec(ctx, `DELETE FROM issue_dependency WHERE issue_id = $1 OR depends_on_issue_id = $1`, id)
		testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, id)
	}
	if f.tplID != "" {
		testPool.Exec(ctx, `DELETE FROM pipeline_template_stage WHERE template_id = $1`, f.tplID)
		testPool.Exec(ctx, `DELETE FROM pipeline_template WHERE id = $1`, f.tplID)
	}
}

// createPipelineTemplate posts a three-stage template; stages 1..2 run on
// the first agent, stage 3 on the second.
func createPipelineTemplate(t *testing.T, advanceMode3 string) *pipelineFixture {
	t.Helper()
	fx := &pipelineFixture{
		agentIDs: []string{
			createHandlerTestAgent(t, "Pipeline Agent A", nil),
			createHandlerTestAgent(t, "Pipeline Agent B", nil),
		},
	}
	t.Cleanup(func() { fx.cleanup(t) })

	req := newRequest("POST", "/api/pipeline-templates", map[string]any{
		"name":        fmt.Sprintf("vla-pipeline-%d", currentTimeNano()),
		"description": "VLA paper production line",
		"stages": []map[string]any{
			{"stage_order": 1, "name": "选题", "agent_id": fx.agentIDs[0], "advance_mode": "auto"},
			{"stage_order": 2, "name": "实验", "agent_id": fx.agentIDs[0], "advance_mode": "auto"},
			{"stage_order": 3, "name": "终稿", "agent_id": fx.agentIDs[1],
				"advance_mode": advanceMode3, "requires_human_gate": advanceMode3 == "orchestrator_review",
				"acceptance_criteria": "PDF compiles"},
		},
	})
	w := testutil.Call(t, testHandler.CreatePipelineTemplate, req).Want(http.StatusCreated)
	var tpl PipelineTemplateResponse
	json.NewDecoder(w.Body).Decode(&tpl)
	if tpl.ID == "" || len(tpl.Stages) != 3 {
		t.Fatalf("template create returned %+v", tpl)
	}
	fx.tplID = tpl.ID
	return fx
}

func currentTimeNano() int64 {
	return time.Now().UnixNano()
}

func instantiatePipeline(t *testing.T, fx *pipelineFixture, sessionID string) InstantiatePipelineResponse {
	t.Helper()
	body := map[string]any{"title": "VLA 主题论文"}
	if sessionID != "" {
		body["orchestrator_session_id"] = sessionID
	}
	req := newRequest("POST", "/api/pipeline-templates/"+fx.tplID+"/instantiate", body)
	req = withURLParam(req, "id", fx.tplID)
	w := testutil.Call(t, testHandler.InstantiatePipelineTemplate, req).Want(http.StatusCreated)
	var resp InstantiatePipelineResponse
	json.NewDecoder(w.Body).Decode(&resp)
	fx.rootID = resp.RootIssueID
	for _, c := range resp.Children {
		fx.childIDs = append(fx.childIDs, c.IssueID)
	}
	return resp
}

func pipelineIssueStatus(t *testing.T, issueID string) string {
	t.Helper()
	var status string
	dbfx.QueryRow(t, `SELECT status FROM issue WHERE id = $1`, issueID).Scan(&status)
	return status
}

func TestPipelineTemplateCRUD(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := createPipelineTemplate(t, "auto")

	// Duplicate name+version conflicts.
	dup := newRequest("POST", "/api/pipeline-templates", map[string]any{
		"name": func() string {
			var name string
			dbfx.QueryRow(t, `SELECT name FROM pipeline_template WHERE id = $1`, fx.tplID).Scan(&name)
			return name
		}(),
		"stages": []map[string]any{
			{"stage_order": 1, "name": "s", "agent_id": fx.agentIDs[0], "advance_mode": "auto"},
		},
	})
	testutil.Call(t, testHandler.CreatePipelineTemplate, dup).Want(http.StatusConflict)

	// Gap in stage_order is rejected.
	gap := newRequest("POST", "/api/pipeline-templates", map[string]any{
		"name": fmt.Sprintf("gap-%d", currentTimeNano()),
		"stages": []map[string]any{
			{"stage_order": 1, "name": "s", "agent_id": fx.agentIDs[0], "advance_mode": "auto"},
			{"stage_order": 3, "name": "s", "agent_id": fx.agentIDs[0], "advance_mode": "auto"},
		},
	})
	testutil.Call(t, testHandler.CreatePipelineTemplate, gap).Want(http.StatusBadRequest)

	// Unknown stage agent is rejected.
	badAgent := newRequest("POST", "/api/pipeline-templates", map[string]any{
		"name": fmt.Sprintf("badagent-%d", currentTimeNano()),
		"stages": []map[string]any{
			{"stage_order": 1, "name": "s", "agent_id": "00000000-0000-0000-0000-000000000000", "advance_mode": "auto"},
		},
	})
	testutil.Call(t, testHandler.CreatePipelineTemplate, badAgent).Want(http.StatusBadRequest)

	getReq := withURLParam(newRequest("GET", "/api/pipeline-templates/"+fx.tplID, nil), "id", fx.tplID)
	w := testutil.Call(t, testHandler.GetPipelineTemplate, getReq).Want(http.StatusOK)
	var tpl PipelineTemplateResponse
	json.NewDecoder(w.Body).Decode(&tpl)
	if len(tpl.Stages) != 3 || tpl.Stages[2].AcceptanceCriteria != "PDF compiles" {
		t.Fatalf("unexpected template stages: %+v", tpl.Stages)
	}

	delReq := withURLParam(newRequest("DELETE", "/api/pipeline-templates/"+fx.tplID, nil), "id", fx.tplID)
	testutil.Call(t, testHandler.DeletePipelineTemplate, delReq).Want(http.StatusOK)
	var stages int
	dbfx.QueryRow(t, `SELECT count(*) FROM pipeline_template_stage WHERE template_id = $1`, fx.tplID).Scan(&stages)
	if stages != 0 {
		t.Fatalf("template delete must clean up stages, got %d", stages)
	}
	fx.tplID = "" // already deleted; skip second cleanup
}

func TestPipelineInstantiate_AutoStagesSequenceThroughGate(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := createPipelineTemplate(t, "auto")
	resp := instantiatePipeline(t, fx, "")
	if resp.RootIssueID == "" || len(resp.Children) != 3 {
		t.Fatalf("instantiation returned %+v", resp)
	}

	// Parent + staging shape.
	if got := pipelineIssueStatus(t, fx.rootID); got != "todo" {
		t.Fatalf("parent must be todo, got %q", got)
	}
	var parentStage int
	dbfx.QueryRow(t,
		`SELECT count(*) FROM issue WHERE parent_issue_id = $1 AND stage IS NOT NULL`, fx.rootID).Scan(&parentStage)
	if parentStage != 3 {
		t.Fatalf("expected 3 staged children, got %d", parentStage)
	}

	// Sequencing edges must exist: stage2→stage1 and stage3→stage2.
	var edgeCount int
	dbfx.QueryRow(t,
		`SELECT count(*) FROM issue_dependency WHERE type = 'blocked_by' AND issue_id = ANY($1::uuid[])`,
		[]string{fx.childIDs[1], fx.childIDs[2]}).Scan(&edgeCount)
	if edgeCount != 2 {
		t.Fatalf("expected 2 blocked_by edges after instantiation, got %d", edgeCount)
	}

	// Stage 1 runs immediately; stages 2-3 are active (todo) but held by the
	// dependency gate — no queued tasks until their blockers finish.
	if got := queuedTaskCount(t, fx.childIDs[0], fx.agentIDs[0]); got != 1 {
		t.Fatalf("stage 1 must start with exactly 1 run, got %d", got)
	}
	if got := pipelineIssueStatus(t, fx.childIDs[1]); got != "todo" {
		t.Fatalf("stage 2 must sit active behind the gate, got %q", got)
	}
	if got := pipelineIssueStatus(t, fx.childIDs[2]); got != "todo" {
		t.Fatalf("stage 3 must sit active behind the gate, got %q", got)
	}
	if got := queuedTaskCount(t, fx.childIDs[1], fx.agentIDs[0]); got != 0 {
		t.Fatalf("gate must hold stage 2, got %d queued tasks", got)
	}
	if got := queuedTaskCount(t, fx.childIDs[2], fx.agentIDs[1]); got != 0 {
		t.Fatalf("gate must hold stage 3, got %d queued tasks", got)
	}

	// Completing stage 1 releases stage 2 (dependency gate → mention wake).
	setStatusViaAPI(t, fx.childIDs[0], "done")
	if got := queuedTaskCount(t, fx.childIDs[1], fx.agentIDs[0]); got != 1 {
		t.Fatalf("stage 2 must be released by stage 1 completion, got %d", got)
	}
	if got := systemCommentCount(t, fx.childIDs[1]); got != 1 {
		t.Fatalf("stage 2 release must post 1 system comment, got %d", got)
	}

	// Completing stage 2 releases stage 3, and the stage barrier wakes the
	// parent (already covered upstream; here just the release).
	setStatusViaAPI(t, fx.childIDs[1], "done")
	if got := queuedTaskCount(t, fx.childIDs[2], fx.agentIDs[1]); got != 1 {
		t.Fatalf("stage 3 must be released by stage 2 completion, got %d", got)
	}
}

func TestPipelineInstantiate_OrchestratorReviewHoldsUntilAdvance(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	fx := createPipelineTemplate(t, "orchestrator_review")
	resp := instantiatePipeline(t, fx, "")
	if len(resp.Children) != 3 {
		t.Fatalf("instantiation returned %+v", resp)
	}

	setStatusViaAPI(t, fx.childIDs[0], "done")
	if got := pipelineIssueStatus(t, fx.childIDs[1]); got != "todo" {
		t.Fatalf("stage 2 (auto) must be released, got %q", got)
	}
	setStatusViaAPI(t, fx.childIDs[1], "done")
	if got := pipelineIssueStatus(t, fx.childIDs[2]); got != "backlog" {
		t.Fatalf("stage 3 (orchestrator_review) must stay parked, got %q", got)
	}
	if got := queuedTaskCount(t, fx.childIDs[2], fx.agentIDs[1]); got != 0 {
		t.Fatalf("review stage must not enqueue, got %d", got)
	}

	// The orchestrator advances stage 3 after its acceptance check.
	advReq := withURLParams(newRequest("POST", "/api/pipeline-runs/"+fx.rootID+"/advance",
		map[string]any{"stage": 3}), "rootId", fx.rootID)
	w := testutil.Call(t, testHandler.AdvancePipelineRun, advReq).Want(http.StatusOK)
	var advResp map[string]int
	json.NewDecoder(w.Body).Decode(&advResp)
	if advResp["promoted"] != 1 {
		t.Fatalf("advance must promote 1 child, got %v", advResp)
	}
	if got := queuedTaskCount(t, fx.childIDs[2], fx.agentIDs[1]); got != 1 {
		t.Fatalf("promoted review stage must run, got %d tasks", got)
	}
}

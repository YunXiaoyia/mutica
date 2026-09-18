-- Pipeline templates and their ordered stages (WP-2,
-- docs/aris-paper-pipeline.md). Stages are replaced wholesale on template
-- update, so the only write paths are create / replace-all / delete.

-- name: CreatePipelineTemplate :one
INSERT INTO pipeline_template (id, workspace_id, name, version, description, orchestrator_agent_id, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetPipelineTemplate :one
SELECT * FROM pipeline_template
WHERE id = $1 AND workspace_id = $2;

-- name: ListPipelineTemplates :many
SELECT * FROM pipeline_template
WHERE workspace_id = $1
ORDER BY name ASC, version DESC;

-- name: UpdatePipelineTemplate :one
UPDATE pipeline_template SET
    description = COALESCE(sqlc.narg('description'), description),
    orchestrator_agent_id = sqlc.narg('orchestrator_agent_id'),
    updated_at = now()
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- name: DeletePipelineTemplate :one
DELETE FROM pipeline_template
WHERE id = $1 AND workspace_id = $2
RETURNING id;

-- name: ListPipelineTemplateStages :many
SELECT * FROM pipeline_template_stage
WHERE template_id = $1
ORDER BY stage_order ASC, created_at ASC, id ASC;

-- name: CreatePipelineTemplateStage :one
INSERT INTO pipeline_template_stage
    (id, template_id, stage_order, name, agent_id, skill_ids, prompt_template, acceptance_criteria, advance_mode, requires_human_gate)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: DeletePipelineTemplateStages :exec
DELETE FROM pipeline_template_stage
WHERE template_id = $1;

-- name: CountPipelineTemplateStages :one
SELECT count(*) FROM pipeline_template_stage
WHERE template_id = $1;

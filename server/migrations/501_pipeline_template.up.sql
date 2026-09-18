-- Pipeline templates (WP-2, docs/aris-paper-pipeline.md): a reusable,
-- ordered stage plan that instantiates into a parent issue plus staged child
-- issues wired with blocked_by edges. No foreign keys by repository rule;
-- template deletion cleans up stages in application code, and instantiated
-- issues are ordinary issues unaffected by template removal.
CREATE TABLE pipeline_template (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL,
    name TEXT NOT NULL,
    version INTEGER NOT NULL DEFAULT 1,
    description TEXT NOT NULL DEFAULT '',
    orchestrator_agent_id UUID,
    created_by UUID NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per pipeline step. Rows sharing stage_order run in parallel as one
-- barrier group; the value is copied onto the child issue's stage column so
-- the existing stage barrier wakes the orchestrator when the group closes.
-- stage_order starts at 1 (issue.stage validates >= 1).
CREATE TABLE pipeline_template_stage (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id UUID NOT NULL,
    stage_order INTEGER NOT NULL CHECK (stage_order >= 1),
    name TEXT NOT NULL,
    agent_id UUID NOT NULL,
    skill_ids UUID[] NOT NULL DEFAULT '{}',
    prompt_template TEXT NOT NULL DEFAULT '',
    acceptance_criteria TEXT NOT NULL DEFAULT '',
    advance_mode TEXT NOT NULL DEFAULT 'auto'
        CHECK (advance_mode IN ('auto', 'orchestrator_review')),
    requires_human_gate BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

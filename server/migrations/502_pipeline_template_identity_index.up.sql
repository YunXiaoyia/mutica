-- Template identity: one active row per (workspace, name, version). Updates
-- that change the plan bump the version so running pipelines keep pointing
-- at the plan they were instantiated from.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS idx_pipeline_template_identity
    ON pipeline_template (workspace_id, name, version);

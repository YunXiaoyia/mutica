-- Instantiation walks a template's stages in order; the non-unique index
-- keeps that scan ordered without forbidding parallel rows that share one
-- stage_order barrier group.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_pipeline_template_stage_order
    ON pipeline_template_stage (template_id, stage_order);

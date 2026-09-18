-- Dependency release scans dependents by blocker: when an issue reaches a
-- terminal status, the handler resolves every issue_dependency row that points
-- at it as a blocker. (depends_on_issue_id, type) is the access path for that
-- scan; type is carried so the 'blocked_by' rows stay seekable beside the
-- other relation kinds.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_issue_dependency_depends_on
    ON issue_dependency (depends_on_issue_id, type);

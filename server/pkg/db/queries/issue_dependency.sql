-- Issue dependency edges (issue_dependency: issue_id, depends_on_issue_id,
-- type). blocked_by is the enforced relation: issue A with a blocked_by edge
-- to B does not auto-start agent runs until B reaches a terminal status. The
-- service gate (IssueService.WillEnqueueRun) consults the boolean probe first
-- so issues without any dependency edge never pay for the blocker scan.

-- name: HasBlockedByEdge :one
SELECT EXISTS (
    SELECT 1 FROM issue_dependency
    WHERE issue_id = $1 AND type = 'blocked_by'
) AS has_edge;

-- name: ListBlockerIssues :many
-- Issues blocking the given issue (the given issue is blocked_by them).
SELECT i.* FROM issue_dependency d
JOIN issue i ON i.id = d.depends_on_issue_id
WHERE d.issue_id = $1 AND d.type = 'blocked_by';

-- name: ListDependentIssues :many
-- Issues blocked by the given issue (they hold a blocked_by edge to it).
SELECT i.* FROM issue_dependency d
JOIN issue i ON i.id = d.issue_id
WHERE d.depends_on_issue_id = $1 AND d.type = 'blocked_by';

-- name: ListBlockedByEdgesForWorkspace :many
-- All blocked_by edges in one workspace, for cycle detection. The edge count
-- is pipeline-sized (dozens); loading the full edge set once beats walking it
-- query-by-query.
SELECT d.issue_id, d.depends_on_issue_id
FROM issue_dependency d
JOIN issue i ON i.id = d.issue_id
WHERE i.workspace_id = $1 AND d.type = 'blocked_by';

-- name: GetIssueDependencyEdge :one
SELECT * FROM issue_dependency
WHERE issue_id = $1 AND depends_on_issue_id = $2 AND type = $3;

-- name: CreateIssueDependency :one
INSERT INTO issue_dependency (issue_id, depends_on_issue_id, type)
VALUES ($1, $2, $3)
RETURNING *;

-- name: DeleteIssueDependency :one
DELETE FROM issue_dependency
WHERE issue_id = $1 AND depends_on_issue_id = $2 AND type = $3
RETURNING id;

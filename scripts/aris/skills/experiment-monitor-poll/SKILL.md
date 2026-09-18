---
name: experiment-monitor-poll
description: Poll cluster jobs for a pipeline experiment stage in submit-and-poll mode. Check status, update issue metadata, end the run quickly. Never wait inside a task for training to finish.
---

# Experiment monitor poll (submit-and-poll)

You are woken periodically (or by a report) to check on cluster jobs for an
experiment stage. Tasks are pollers, not waiters — a poll run must end within
minutes.

## Poll loop

1. Find the work: `multica issue list --metadata pipeline_root=<root>` and
   pick issues whose metadata has `cluster_status=submitted` or
   `cluster_status=running`.
2. For each, resolve `cluster_job=<id>` from the issue metadata, query the
   cluster (follow the run-experiment skill for cluster access).
3. Still running → update `cluster_status=running` (plus any progress note in
   a brief comment ONLY if something changed) and end your run.
4. Finished successfully → download/pull artifacts into the shared repo, write
   artifact paths into the issue, set `cluster_status=done`, then set the
   issue to done (`multica issue status <issue> done`). The dependency gate
   releases the next stage automatically.
5. Failed → set `cluster_status=failed`, post a comment with the tail of the
   job log and your diagnosis, and end. Do NOT mark the issue done; the
   orchestrator decides between rerun / new approach / scope cut.

## Hard rules

- Never sleep or poll inside one task run — one check per run.
- Never mark a failed experiment issue done. `done` releases downstream
  stages; only genuinely finished work may release them.
- Every status transition you make must leave a trail (comment or metadata).

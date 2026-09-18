# ARIS × Multica paper production line (scripts)

Operational scripts for the automated paper production line built on Multica
(WP-4 / WP-5 of docs/aris-paper-pipeline.md).

## Layout

- `skills/aris-orchestrator/SKILL.md` — the orchestrator ("主理人") protocol.
- `skills/experiment-monitor-poll/SKILL.md` — submit-and-poll conventions for
  long-running training jobs (a poll run ends in minutes; jobs never block a
  task).
- `sync_skills.py` — upsert `skills/*` into a Multica workspace by skill name
  (idempotent).
- `bootstrap.py` — one-shot workspace setup: agents (roster below), skill
  sync + binding, the `paper-default` pipeline template, and the
  orchestrator chat session (the 主理人窗口).

## Usage

```bash
export MULTICA_BASE_URL=http://localhost:18516
export MULTICA_PAT=mul_...                 # personal access token; env only
export MULTICA_WORKSPACE_ID=<uuid>
export MULTICA_RUNTIME_ID=<uuid>           # optional but recommended: pins
                                           # the orchestrator's runtime so
                                           # chat session resume survives
python3 scripts/aris/bootstrap.py
```

Rerunning bootstrap is safe: agents update in place (runtime binding is never
changed by updates), skills upsert by name, the template and session are
reused when present.

## Roster

| Agent | Skills | Pipeline role |
| --- | --- | --- |
| Aris | aris-orchestrator | orchestrator (parent issue assignee) |
| Scout | idea-discovery, novelty-check | topic discovery |
| Researcher | research-lit, arxiv | literature review |
| Planner | experiment-plan, ablation-planner | experiment design |
| Experimenter | run-experiment, monitor-experiment, experiment-monitor-poll | experiments (submit-and-poll) |
| Analyst | analyze-results, result-to-claim | analysis |
| Writer | paper-write, paper-figure, paper-compile | writing + figures |
| Reviewer | auto-review-loop, rebuttal | internal review |

Skills listed here but not present in `skills/` are skipped by the binder —
drop the corresponding ARIS skill directories into `skills/` (or point
`sync_skills.py --skills-dir` at your ARIS checkout) to enable them.

## Experiment monitoring (WP-5)

Long jobs never occupy a daemon task. The Experimenter submits to the
cluster, stamps `cluster_job` / `cluster_status=submitted` into the issue
metadata, and ends its run. A schedule autopilot (15-minute, `run_only`,
assignee Experimenter, prompt "run the experiment-monitor-poll loop on all
submitted cluster jobs") re-wakes the poll loop; the poll skill updates
metadata and closes the issue when results land, which releases the next
pipeline stage through the dependency gate.

Create the monitor autopilot once per workspace:

```bash
curl -s -X POST "$MULTICA_BASE_URL/api/autopilots" \
  -H "Authorization: Bearer $MULTICA_PAT" -H "X-Workspace-ID: $MULTICA_WORKSPACE_ID" \
  -H 'Content-Type: application/json' -d '{
    "title": "experiment-monitor",
    "assignee_type": "agent",
    "assignee_id": "<Experimenter agent id>",
    "execution_mode": "run_only"
  }'
# then attach a schedule trigger:
curl -s -X POST "$MULTICA_BASE_URL/api/autopilots/<autopilot_id>/triggers" \
  -H "Authorization: Bearer $MULTICA_PAT" -H "X-Workspace-ID: $MULTICA_WORKSPACE_ID" \
  -H 'Content-Type: application/json' \
  -d '{"kind": "schedule", "cron_expression": "*/15 * * * *", "timezone": "Asia/Shanghai"}'
```

## Verify

```bash
python3 scripts/aris/bootstrap.py   # idempotent; prints orchestrator session id
```

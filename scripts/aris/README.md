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
| Scout | idea-discovery, idea-creator, novelty-check, research-review, research-refine, alphaxiv | topic discovery |
| Researcher | research-lit, arxiv, semantic-scholar, openalex, deepxiv, exa-search, comm-lit-review, research-wiki | literature review |
| Planner | experiment-plan, experiment-bridge, ablation-planner, formula-derivation | experiment design |
| Experimenter | run-experiment, monitor-experiment, experiment-monitor-poll, experiment-queue, training-check, qzcli | experiments (submit-and-poll) |
| Analyst | analyze-results, experiment-audit, result-to-claim | analysis |
| Writer | paper-writing, paper-plan, paper-write, paper-figure, paper-compile, citation-audit, paper-claim-audit, integrity-forensics, auto-paper-improvement-loop | writing + audits |
| Reviewer | auto-review-loop-llm, auto-review-loop, rebuttal, kill-argument, proof-checker | cross-model review |

The Reviewer does not score its own draft. `bootstrap.py` registers the
`llm-chat` MCP server (`scripts/aris/mcp-servers/llm-chat/server.py`) on that
agent and, on first create only, copies `LLM_API_KEY`, `LLM_BASE_URL` and
`LLM_MODEL` into the agent env. Set those in the environment before bootstrap.
Updating an existing Reviewer refreshes the MCP command but does not replace
its env, because the env endpoint overwrites the whole map.

`scripts/aris/tools/` is the vendored ARIS helper set (`verify_papers.py`,
`research_wiki.py`, `watchdog.py`, and the rest). A pipeline task whose issue
metadata has `pipeline_repo` runs in that directory; the daemon copies
`tools/` to `<repo>/.aris/tools` and sets `ARIS_REPO` so skill scripts resolve
helpers. `MULTICA_ARIS_TOOLS` overrides the source directory.

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

This autopilot is automatically created and wired by `bootstrap.py` (idempotent,
using the `experiment-monitor-poll` prompt and a 15-minute schedule trigger).

Equivalent manual setup (for reference):

```bash
curl -s -X POST "$MULTICA_BASE_URL/api/autopilots" \
  -H "Authorization: Bearer $MULTICA_PAT" -H "X-Workspace-ID: $MULTICA_WORKSPACE_ID" \
  -H 'Content-Type: application/json' -d '{
    "title": "experiment-monitor",
    "assignee_type": "agent",
    "assignee_id": "<Experimenter agent id>",
    "execution_mode": "run_only",
    "description": "Run the experiment-monitor-poll skill. List issues with `multica issue list --metadata cluster_status=submitted` and again with `cluster_status=running`. For each issue, first run `multica issue runs <issue-id> --active`; if any run is queued, dispatched, running, or waiting_local_directory, skip that issue. Otherwise poll the cluster job recorded in the issue metadata, update cluster_status, and on genuine completion set the issue to done so the dependency gate releases the next stage. Never mark a failed job done. One check per run — do not sleep or poll inside this run. If a previous poll is still running, exit immediately."
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

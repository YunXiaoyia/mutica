#!/usr/bin/env python3
"""Bootstrap the ARIS paper production line in a Multica workspace (WP-4).

One idempotent command that brings a fresh workspace to the full lineup:

1. Create the role agents (idempotent by name, so reruns update in place).
2. Sync the ARIS skills (delegates to sync_skills.py).
3. Bind the skills per the roster.
4. Create the default pipeline template (paper-default) if absent.
5. Create the experiment monitoring autopilot (experiment-monitor) if absent.
6. Create the orchestrator's chat session and print its id — this is the
   主理人窗口 the user talks to.

Usage:
    MULTICA_BASE_URL=http://localhost:18516 \
    MULTICA_PAT=mul_... \
    MULTICA_WORKSPACE_ID=<uuid> \
    python3 scripts/aris/bootstrap.py

The roster below is the single source of truth for agents and bindings.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import urllib.error
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
from sync_skills import API  # noqa: E402

# name -> (description, instructions, [skills])
ROSTER = {
    "Aris": (
        "论文流水线主理人：只协调，不执行。严格执行立项建仓SOP与4090服务器协同规范。",
        "You are the paper pipeline orchestrator. Follow the aris-orchestrator "
        "skill exactly: confirm intake, bootstrap the persistent repo at "
        "/workspace/work/<project_slug> with git init & AGENTS.md, instantiate "
        "the pipeline template, bind stage tasks to the repo, enforce ssh 4090 "
        "GPU protocol, report progress in chat, run acceptance checks, and close out.",
        ["aris-orchestrator", "shared-references"],
    ),
    "Scout": (
        "选题与新颖性把关。",
        "You propose and validate paper topics. Follow idea-discovery, then "
        "novelty-check and research-review before ranking. Use alphaxiv for "
        "arxiv-wide signal. Output: a ranked shortlist with evidence, then stop.",
        ["idea-discovery", "idea-creator", "novelty-check", "research-review",
         "research-refine", "alphaxiv", "shared-references"],
    ),
    "Researcher": (
        "文献综述。",
        "You run literature review. Follow research-lit, and use arxiv, "
        "semantic-scholar, openalex, deepxiv and comm-lit-review for sources. "
        "Verify citations with the vendored tools/verify_papers.py resolved via "
        ".aris/tools or $ARIS_REPO. Record papers in the research wiki when "
        "research-wiki/ exists. Output: a structured related-work map with "
        "citations, then stop.",
        ["research-lit", "arxiv", "semantic-scholar", "openalex", "deepxiv",
         "exa-search", "comm-lit-review", "research-wiki", "shared-references"],
    ),
    "Planner": (
        "实验设计。",
        "You design experiments. Follow experiment-plan and experiment-bridge, "
        "with ablation-planner for controls. Output: an executable plan with "
        "datasets, metrics, baselines, ablations and compute estimates, then stop.",
        ["experiment-plan", "experiment-bridge", "ablation-planner",
         "formula-derivation", "shared-references"],
    ),
    "Experimenter": (
        "跑实验（submit-and-poll：提交集群作业后记录 job id 即停）。",
        "You execute experiments with run-experiment in submit-and-poll mode and "
        "experiment-queue for remote GPU jobs. Submit the job, write cluster_job "
        "and cluster_status into the issue metadata, and END YOUR RUN. You will "
        "be woken to poll. Never block a task waiting for training. Shared "
        "helpers resolve from .aris/tools, then $ARIS_REPO/tools.",
        ["run-experiment", "monitor-experiment", "experiment-monitor-poll",
         "experiment-queue", "training-check", "qzcli", "shared-references"],
    ),
    "Analyst": (
        "结果分析与结论。",
        "You analyze experiment results using analyze-results, experiment-audit "
        "and result-to-claim. Output: claims each anchored to an artifact, then stop.",
        ["analyze-results", "experiment-audit", "result-to-claim", "shared-references"],
    ),
    "Writer": (
        "成文与图表。",
        "You write the paper with paper-writing (plan, draft, figures, compile). "
        "Run citation-audit and paper-claim-audit before you stop, and "
        "integrity-forensics when the issue metadata human_gate is true or the "
        "paper assurance file says submission. Keep the paper in the shared git "
        "repo and commit per revision. Output: repo path plus compile status, then stop.",
        ["paper-writing", "paper-plan", "paper-write", "paper-figure",
         "paper-compile", "citation-audit", "paper-claim-audit",
         "integrity-forensics", "auto-paper-improvement-loop", "shared-references"],
    ),
    "Reviewer": (
        "独立模型内审。执行者与评审者必须是不同模型族。",
        "You review with auto-review-loop-llm, not by scoring your own draft. "
        "Call the llm-chat MCP tool (mcp__llm-chat__chat) or, if that tool is "
        "absent, the curl fallback in the skill using LLM_BASE_URL, LLM_API_KEY "
        "and LLM_MODEL from the environment. Those must name a model family "
        "different from the executor. Also run rebuttal and kill-argument when "
        "the draft is near a venue. Output: numbered findings with severity, "
        "each tied to a file path, then stop.",
        ["auto-review-loop-llm", "auto-review-loop", "rebuttal", "kill-argument",
         "proof-checker", "shared-references"],
    ),
}

# Skills attached to the stage issue itself, in addition to the agent binding.
# The agent binding is what the daemon materialises; these ids are the
# stage contract a template editor can see.
STAGE_SKILLS = {
    1: ["idea-discovery", "novelty-check", "research-review"],
    2: ["research-lit", "arxiv", "semantic-scholar", "research-wiki"],
    3: ["experiment-plan", "experiment-bridge", "ablation-planner"],
    4: ["run-experiment", "monitor-experiment", "experiment-monitor-poll", "experiment-queue"],
    5: ["analyze-results", "result-to-claim", "experiment-audit"],
    6: ["paper-writing", "paper-plan", "paper-write", "citation-audit", "paper-claim-audit"],
    7: ["auto-review-loop-llm", "rebuttal", "kill-argument"],
    8: ["paper-write", "paper-compile", "integrity-forensics", "auto-paper-improvement-loop"],
}

TEMPLATE_STAGES = [
    {"stage_order": 1, "name": "Topic discovery", "agent": "Scout", "advance_mode": "orchestrator_review", "requires_human_gate": True,
     "prompt_template": "Research goal: {{goal}}\n{{description}}\nPropose paper topics and validate novelty.",
     "acceptance_criteria": "Ranked shortlist with novelty evidence for the top 3."},
    {"stage_order": 2, "name": "Literature review", "agent": "Researcher", "advance_mode": "orchestrator_review", "requires_human_gate": True,
     "prompt_template": "Produce the related-work map for: {{goal}}",
     "acceptance_criteria": "Structured related-work section draft with complete citations."},
    {"stage_order": 3, "name": "Experiment design", "agent": "Planner", "advance_mode": "auto", "requires_human_gate": False,
     "prompt_template": "Design the experiments for: {{goal}}",
     "acceptance_criteria": "Plan covers datasets, metrics, baselines, ablations, compute budget."},
    {"stage_order": 4, "name": "Experiments", "agent": "Experimenter", "advance_mode": "auto", "requires_human_gate": False,
     "prompt_template": "Execute the experiment plan (submit-and-poll).",
     "acceptance_criteria": "All planned runs recorded with metrics and artifact paths."},
    {"stage_order": 5, "name": "Analysis", "agent": "Analyst", "advance_mode": "auto", "requires_human_gate": False,
     "prompt_template": "Analyze the experiment outputs for: {{goal}}",
     "acceptance_criteria": "Every claim anchored to a concrete artifact."},
    {"stage_order": 6, "name": "Writing", "agent": "Writer", "advance_mode": "auto", "requires_human_gate": False,
     "prompt_template": "Write the paper for: {{goal}}",
     "acceptance_criteria": "Paper compiles; all figures referenced."},
    {"stage_order": 7, "name": "Internal review", "agent": "Reviewer", "advance_mode": "auto", "requires_human_gate": False,
     "prompt_template": "Internal-review the draft for: {{goal}}",
     "acceptance_criteria": "Findings list with severity + owner for each."},
    {"stage_order": 8, "name": "Final draft", "agent": "Writer", "advance_mode": "orchestrator_review", "requires_human_gate": True,
     "prompt_template": "Apply review findings and finalize the paper for: {{goal}}",
     "acceptance_criteria": "Final PDF compiled; review findings addressed or waived."},
]


def request(method: str, path: str, body: dict | None = None) -> dict | list:
    base = os.environ["MULTICA_BASE_URL"].rstrip("/")
    req = urllib.request.Request(
        base + path,
        data=json.dumps(body).encode() if body is not None else None,
        method=method,
        headers={
            "Authorization": f"Bearer {os.environ['MULTICA_PAT']}",
            "X-Workspace-ID": os.environ["MULTICA_WORKSPACE_ID"],
            "Content-Type": "application/json",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            payload = resp.read()
            return json.loads(payload) if payload else {}
    except urllib.error.HTTPError as e:
        raise SystemExit(f"{method} {path} -> {e.code}: {e.read().decode(errors='replace')[:500]}") from e


def reviewer_runtime() -> tuple[dict, dict[str, str]]:
    """Cross-model reviewer: llm-chat MCP plus the curl fallback env.

    LLM_API_KEY / LLM_BASE_URL / LLM_MODEL come from the process environment
    and are never written into the repo. When the key is absent the MCP server
    is still registered so the tool exists; the skill's curl path then fails
    closed instead of the reviewer scoring its own draft.
    """
    root = Path(__file__).resolve().parent
    server = root / "mcp-servers" / "llm-chat" / "server.py"
    python = shutil.which("python3") or "python3"
    llm_env = {
        "LLM_API_KEY": os.environ.get("LLM_API_KEY", ""),
        "LLM_BASE_URL": os.environ.get("LLM_BASE_URL", "https://api.deepseek.com/v1"),
        "LLM_MODEL": os.environ.get("LLM_MODEL", "deepseek-chat"),
    }
    mcp = {
        "mcpServers": {
            "llm-chat": {
                "command": python,
                "args": [str(server)],
                "env": llm_env,
            }
        }
    }
    return mcp, llm_env


def resolve_runtime_id(request_fn) -> str:
    """Pick the runtime agents run on. MULTICA_RUNTIME_ID pins it explicitly —
    recommended for the orchestrator, whose chat session resume only works on
    the same runtime. Falls back to the first runtime in the workspace."""
    pinned = os.environ.get("MULTICA_RUNTIME_ID")
    if pinned:
        return pinned
    runtimes = request_fn("GET", "/api/runtimes")
    rows = runtimes if isinstance(runtimes, list) else runtimes.get("runtimes", [])
    if not rows:
        raise SystemExit(
            "no agent runtime found: start `multica daemon start` first, or set MULTICA_RUNTIME_ID"
        )
    return rows[0]["id"]


def main() -> int:
    missing = [k for k in ("MULTICA_BASE_URL", "MULTICA_PAT", "MULTICA_WORKSPACE_ID") if not os.environ.get(k)]
    if missing:
        print(f"missing environment variables: {', '.join(missing)}", file=sys.stderr)
        return 2

    api = API(os.environ["MULTICA_BASE_URL"], os.environ["MULTICA_PAT"], os.environ["MULTICA_WORKSPACE_ID"])
    runtime_id = resolve_runtime_id(request)
    reviewer_mcp, reviewer_env = reviewer_runtime()
    if not reviewer_env["LLM_API_KEY"]:
        print("  warning: LLM_API_KEY is empty; Reviewer will not be able to call a second model family")

    # 1. Skills first: agents bind by id.
    print("== syncing skills ==")
    subprocess.run([sys.executable, str(Path(__file__).parent / "sync_skills.py")], check=True)
    skill_index = {s["name"]: s["id"] for s in api.list_skills()}

    # 2. Agents (idempotent by name).
    print("== upserting agents ==")
    agent_ids = {}
    for existing in request("GET", "/api/agents"):
        agent_ids[existing["name"]] = existing["id"]
    for name, (description, instructions, _) in ROSTER.items():
        if name in agent_ids:
            # Update without runtime_id: never rebind an existing agent.
            request("PUT", f"/api/agents/{agent_ids[name]}", {
                "name": name,
                "description": description,
                "instructions": instructions,
            })
            print(f"  updated agent {name}")
        else:
            body = {
                "name": name,
                "description": description,
                "instructions": instructions,
                "runtime_id": runtime_id,
                "visibility": "workspace",
            }
            if name == "Reviewer":
                body["mcp_config"] = reviewer_mcp
                body["custom_env"] = reviewer_env
            created = request("POST", "/api/agents", body)
            agent_ids[name] = created["id"]
            print(f"  created agent {name}")

    # 3. Bind skills per the roster.
    print("== binding skills ==")
    for name, (_, _, skills) in ROSTER.items():
        ids = [skill_index[s] for s in skills if s in skill_index]
        if ids:
            request("PUT", f"/api/agents/{agent_ids[name]}/skills", {"skill_ids": ids})
            print(f"  {name}: {len(ids)} skill(s)")

    # Reviewer MCP is safe to refresh: it holds no secret beyond what custom_env
    # already carries, and PUT /api/agents accepts mcp_config. custom_env is
    # write-once on create (updates go through /env and replace the whole map),
    # so an existing Reviewer keeps whatever key the operator already stored.
    request("PUT", f"/api/agents/{agent_ids['Reviewer']}", {"mcp_config": reviewer_mcp})
    print("  Reviewer: llm-chat MCP registered")

    # 4. Default template (idempotent by name+version).
    print("== pipeline template ==") # updated
    templates = request("GET", "/api/pipeline-templates")
    existing_tpl = next((t for t in templates if t["name"] == "paper-default"), None)
    stage_payload = [
        {
            "stage_order": s["stage_order"],
            "name": s["name"],
            "agent_id": agent_ids[s["agent"]],
            "advance_mode": s["advance_mode"],
            "requires_human_gate": s["requires_human_gate"],
            "prompt_template": s["prompt_template"],
            "acceptance_criteria": s["acceptance_criteria"],
            "skill_ids": [skill_index[n] for n in STAGE_SKILLS.get(s["stage_order"], []) if n in skill_index],
        }
        for s in TEMPLATE_STAGES
    ]
    if existing_tpl:
        request("PUT", f"/api/pipeline-templates/{existing_tpl['id']}", {
            "description": "ARIS paper production line (topic → review → final draft) [SOP & 4090 enforced]",
            "orchestrator_agent_id": agent_ids["Aris"],
            "stages": stage_payload,
        })
        print("  updated paper-default template with latest SOP & stages")
    else:
        request("POST", "/api/pipeline-templates", {
            "name": "paper-default",
            "description": "ARIS paper production line (topic → review → final draft) [SOP & 4090 enforced]",
            "orchestrator_agent_id": agent_ids["Aris"],
            "stages": stage_payload,
        })
        print("  created paper-default")

    # 5. Experiment monitoring autopilot (idempotent by title).
    print("== experiment-monitor autopilot ==")
    autopilots_resp = request("GET", "/api/autopilots")
    existing_autopilots = (
        autopilots_resp.get("autopilots", [])
        if isinstance(autopilots_resp, dict)
        else autopilots_resp
    )
    existing_ap = next(
        (a for a in existing_autopilots if isinstance(a, dict) and a.get("title") == "experiment-monitor"),
        None,
    )

    if existing_ap:
        autopilot_id = existing_ap["id"]
        print(f"  reusing autopilot {autopilot_id}")
    else:
        created_ap = request("POST", "/api/autopilots", {
            "title": "experiment-monitor",
            "assignee_id": agent_ids["Experimenter"],
            "execution_mode": "run_only",
            "description": (
                "Run the experiment-monitor-poll skill. List issues with `multica issue list --metadata cluster_status=submitted` "
                "and again with `cluster_status=running`. For each issue, first run `multica issue runs <issue-id> --active`; "
                "if any run is queued, dispatched, running, or waiting_local_directory, skip that issue. Otherwise poll the cluster job "
                "recorded in the issue metadata, update cluster_status, and on genuine completion set the issue to done so the dependency "
                "gate releases the next stage. Never mark a failed job done. One check per run — do not sleep or poll inside this run. "
                "If a previous poll is still running, exit immediately."
            ),
        })
        if not isinstance(created_ap, dict) or not created_ap.get("id"):
            raise SystemExit(f"POST /api/autopilots returned no id: {created_ap!r}"[:300])
        autopilot_id = created_ap["id"]
        print(f"  created autopilot {autopilot_id}")

    # GET /api/autopilots/{id} wraps the row: {"autopilot": {...}, "triggers": [...]}.
    # The list endpoint is the flat one; reading triggers off the wrong level would
    # always see none and create a new schedule on every rerun.
    ap_detail = request("GET", f"/api/autopilots/{autopilot_id}")
    triggers = ap_detail.get("triggers", []) if isinstance(ap_detail, dict) else []
    has_schedule = any(
        isinstance(t, dict)
        and t.get("kind") == "schedule"
        and t.get("cron_expression") == "*/15 * * * *"
        for t in triggers
    )
    if not has_schedule:
        request("POST", f"/api/autopilots/{autopilot_id}/triggers", {
            "kind": "schedule",
            "cron_expression": "*/15 * * * *",
            "timezone": "Asia/Shanghai",
        })
        print("  created schedule trigger")
    else:
        print("  reusing schedule trigger")

    print(f"experiment-monitor autopilot: {autopilot_id}")

    # 6. Orchestrator chat session (the 主理人窗口).
    print("== orchestrator chat session ==")
    sessions = request("GET", "/api/chat/sessions")
    existing = next((s for s in sessions if s.get("agent_id") == agent_ids["Aris"] and s.get("status") == "active"), None)
    if existing:
        session_id = existing["id"]
        print(f"  reusing session {session_id}")
    else:
        created = request("POST", "/api/chat/sessions", {"agent_id": agent_ids["Aris"], "title": "主理人"})
        session_id = created["id"] if isinstance(created, dict) else created
        print(f"  created session {session_id}")

    print(json.dumps({
        "ok": True,
        "orchestrator_session_id": session_id,
        "experiment_monitor_autopilot_id": autopilot_id,
    }, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())

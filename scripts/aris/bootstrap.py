#!/usr/bin/env python3
"""Bootstrap the ARIS paper production line in a Multica workspace (WP-4).

One idempotent command that brings a fresh workspace to the full lineup:

1. Create the role agents (idempotent by name, so reruns update in place).
2. Sync the ARIS skills (delegates to sync_skills.py).
3. Bind the skills per the roster.
4. Create the default pipeline template (paper-default) if absent.
5. Create the orchestrator's chat session and print its id — this is the
   主理人窗口 the user talks to.

Usage:
    MULTICA_BASE_URL=http://localhost:18516 \\
    MULTICA_PAT=mul_... \\
    MULTICA_WORKSPACE_ID=<uuid> \\
    python3 scripts/aris/bootstrap.py

The roster below is the single source of truth for agents and bindings.
"""

from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
from sync_skills import API  # noqa: E402

# name -> (description, instructions, [skills])
ROSTER = {
    "Aris": (
        "论文流水线主理人：只协调，不执行。",
        "You are the paper pipeline orchestrator. Follow the aris-orchestrator "
        "skill exactly: confirm intake, instantiate the pipeline template, "
        "report progress in chat, run acceptance checks on review stages, put "
        "human gates to the user as compact options, and close out with a "
        "completion report.",
        ["aris-orchestrator", "shared-references"],
    ),
    "Scout": (
        "选题与新颖性把关。",
        "You propose and validate paper topics. Follow the idea-discovery and "
        "novelty-check skills. Output: a ranked shortlist with evidence, then "
        "stop.",
        ["idea-discovery", "novelty-check", "shared-references"],
    ),
    "Researcher": (
        "文献综述。",
        "You run literature review. Follow the research-lit and arxiv skills. "
        "Output: a structured related-work map with citations, then stop.",
        ["research-lit", "arxiv", "shared-references"],
    ),
    "Planner": (
        "实验设计。",
        "You design experiments. Follow the experiment-plan and "
        "ablation-planner skills. Output: an executable experiment plan with "
        "datasets, metrics, controls, and compute estimates, then stop.",
        ["experiment-plan", "ablation-planner", "shared-references"],
    ),
    "Experimenter": (
        "跑实验（submit-and-poll：提交集群作业后记录 job id 即停）。",
        "You execute experiments on the cluster using the run-experiment skill "
        "in submit-and-poll mode: submit the job, write cluster_job and "
        "cluster_status into the issue metadata, and END YOUR RUN. You will be "
        "woken again to poll. Never block a task waiting for training to "
        "finish.",
        ["run-experiment", "monitor-experiment", "experiment-monitor-poll", "shared-references"],
    ),
    "Analyst": (
        "结果分析与结论。",
        "You analyze experiment results using the analyze-results and "
        "result-to-claim skills. Output: claims each anchored to artifacts, "
        "then stop.",
        ["analyze-results", "result-to-claim", "shared-references"],
    ),
    "Writer": (
        "成文与图表。",
        "You write and compile the paper using the paper-write, paper-figure "
        "and paper-compile skills. Keep the paper in the shared git repo; "
        "commit per revision. Output: updated repo + compile status, then stop.",
        ["paper-write", "paper-figure", "paper-compile", "shared-references"],
    ),
    "Reviewer": (
        "内审与修订意见。",
        "You internal-review the draft using the auto-review-loop and rebuttal "
        "skills. Output: numbered, actionable findings with severity, then stop.",
        ["auto-review-loop", "rebuttal", "shared-references"],
    ),
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

    # 1. Skills first: agents bind by id.
    print("== syncing skills ==")
    os.system(f"{sys.executable} {Path(__file__).parent / 'sync_skills.py'}")
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
            created = request("POST", "/api/agents", {
                "name": name,
                "description": description,
                "instructions": instructions,
                "runtime_id": runtime_id,
                "visibility": "workspace",
            })
            agent_ids[name] = created["id"]
            print(f"  created agent {name}")

    # 3. Bind skills per the roster.
    print("== binding skills ==")
    for name, (_, _, skills) in ROSTER.items():
        ids = [skill_index[s] for s in skills if s in skill_index]
        if ids:
            request("PUT", f"/api/agents/{agent_ids[name]}/skills", {"skill_ids": ids})
            print(f"  {name}: {len(ids)} skill(s)")

    # 4. Default template (idempotent by name+version).
    print("== pipeline template ==")
    templates = request("GET", "/api/pipeline-templates")
    if not any(t["name"] == "paper-default" and t["version"] == 1 for t in templates):
        request("POST", "/api/pipeline-templates", {
            "name": "paper-default",
            "description": "ARIS paper production line (topic → review → final draft)",
            "orchestrator_agent_id": agent_ids["Aris"],
            "stages": [
                {
                    "stage_order": s["stage_order"],
                    "name": s["name"],
                    "agent_id": agent_ids[s["agent"]],
                    "advance_mode": s["advance_mode"],
                    "requires_human_gate": s["requires_human_gate"],
                    "prompt_template": s["prompt_template"],
                    "acceptance_criteria": s["acceptance_criteria"],
                }
                for s in TEMPLATE_STAGES
            ],
        })
        print("  created paper-default")
    else:
        print("  paper-default already exists")

    # 5. Orchestrator chat session (the 主理人窗口).
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

    print(json.dumps({"ok": True, "orchestrator_session_id": session_id}, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())

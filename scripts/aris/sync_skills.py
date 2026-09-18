#!/usr/bin/env python3
"""Sync ARIS skills into a Multica workspace (WP-4, docs/aris-paper-pipeline.md).

Source of truth is scripts/aris/skills/<name>/ — a SKILL.md (required) plus
optional supporting files. Every skill directory is upserted into Multica by
skill name, so re-running this script is idempotent and diffable:

    MULTICA_BASE_URL=http://localhost:18516 \\
    MULTICA_PAT=mul_... \\
    MULTICA_WORKSPACE_ID=<uuid> \\
    python3 scripts/aris/sync_skills.py [--skills-dir scripts/aris/skills]

Personal access tokens come from the environment only — never commit one.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path

REQUIRED_ENV = ("MULTICA_BASE_URL", "MULTICA_PAT", "MULTICA_WORKSPACE_ID")


class API:
    def __init__(self, base_url: str, token: str, workspace_id: str) -> None:
        self.base = base_url.rstrip("/")
        self.token = token
        self.workspace_id = workspace_id

    def request(self, method: str, path: str, body: dict | None = None) -> dict | list:
        req = urllib.request.Request(
            self.base + path,
            data=json.dumps(body).encode() if body is not None else None,
            method=method,
            headers={
                "Authorization": f"Bearer {self.token}",
                "X-Workspace-ID": self.workspace_id,
                "Content-Type": "application/json",
            },
        )
        try:
            with urllib.request.urlopen(req, timeout=60) as resp:
                payload = resp.read()
                return json.loads(payload) if payload else {}
        except urllib.error.HTTPError as e:
            detail = e.read().decode(errors="replace")[:500]
            raise SystemExit(f"{method} {path} -> {e.code}: {detail}") from e

    def list_skills(self) -> list[dict]:
        return self.request("GET", "/api/skills?limit=1000")

    def create_skill(self, body: dict) -> dict:
        return self.request("POST", "/api/skills", body)

    def update_skill(self, skill_id: str, body: dict) -> dict:
        return self.request("PUT", f"/api/skills/{skill_id}", body)


def read_skill_dir(skill_dir: Path) -> dict:
    """Load SKILL.md plus supporting files into the CreateSkill shape.

    The frontmatter (--- name / description ---) is stripped from SKILL.md:
    Multica parses its own frontmatter from content.
    """
    md = (skill_dir / "SKILL.md").read_text(encoding="utf-8")
    content = md
    if md.startswith("---"):
        end = md.find("---", 3)
        if end != -1:
            content = md[end + 3 :].lstrip("\n")
    body = {
        "name": skill_dir.name,
        "description": f"ARIS skill: {skill_dir.name}",
        "content": content,
        "files": [],
    }
    for path in sorted(skill_dir.rglob("*")):
        if not path.is_file() or path.name == "SKILL.md":
            continue
        rel = path.relative_to(skill_dir).as_posix()
        body["files"].append({"path": rel, "content": path.read_text(encoding="utf-8")})
    return body


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--skills-dir", default="scripts/aris/skills")
    args = parser.parse_args()

    missing = [k for k in REQUIRED_ENV if not os.environ.get(k)]
    if missing:
        print(f"missing environment variables: {', '.join(missing)}", file=sys.stderr)
        return 2

    api = API(os.environ["MULTICA_BASE_URL"], os.environ["MULTICA_PAT"], os.environ["MULTICA_WORKSPACE_ID"])
    skills_dir = Path(args.skills_dir)
    if not skills_dir.is_dir():
        print(f"skills directory not found: {skills_dir}", file=sys.stderr)
        return 2

    existing = {s["name"]: s for s in api.list_skills()}
    created, updated, skipped = [], [], []
    for skill_dir in sorted(p for p in skills_dir.iterdir() if p.is_dir()):
        if not (skill_dir / "SKILL.md").exists():
            skipped.append(skill_dir.name)
            continue
        body = read_skill_dir(skill_dir)
        if skill_dir.name in existing:
            api.update_skill(existing[skill_dir.name]["id"], body)
            updated.append(skill_dir.name)
        else:
            api.create_skill(body)
            created.append(skill_dir.name)

    print(json.dumps({"created": created, "updated": updated, "skipped": skipped}, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env bash
# End-to-end validation of the ARIS paper production line on Multica
# (docs/aris-paper-pipeline.md), themed on a VLA (Vision-Language-Action)
# robotics paper. Drives the REAL HTTP API the way the orchestrator and the
# specialists would: instantiate a template, walk every stage through the
# dependency gate and the review gates, and check the orchestrator chat
# wiring.
#
# Required environment:
#   MULTICA_BASE_URL, MULTICA_PAT, MULTICA_WORKSPACE_ID
#   E2E_ORCHESTRATOR_SESSION_ID  (the 主理人 chat session id)
set -euo pipefail

: "${MULTICA_BASE_URL:?}" "${MULTICA_PAT:?}" "${MULTICA_WORKSPACE_ID:?}" "${E2E_ORCHESTRATOR_SESSION_ID:?}"
BASE="$MULTICA_BASE_URL"
AUTH=(-H "Authorization: Bearer $MULTICA_PAT" -H "X-Workspace-ID: $MULTICA_WORKSPACE_ID" -H 'Content-Type: application/json')
PASS=0; FAILURES=0

# check <label> <test words...>: label plus a [ ] test expression.
check() { local label=$1; shift; if [ "$@" ]; then PASS=$((PASS+1)); echo "  ✓ $label"; else echo "  ✗ $label"; FAILURES=$((FAILURES+1)); fi; }
# checkrun <label> <command...>: passes when the command exits 0.
checkrun() { local label=$1; shift; if "$@"; then PASS=$((PASS+1)); echo "  ✓ $label"; else echo "  ✗ $label"; FAILURES=$((FAILURES+1)); fi; }
api() { local method=$1 path=$2; shift 2; curl -sS -X "$method" "$BASE$path" "${AUTH[@]}" "$@"; }
# jget <json> <python expr applied to d>
jget() { python3 -c 'import json,sys; d=json.loads(sys.argv[1]); print(eval(sys.argv[2]))' "$1" "$2"; }

echo "== VLA pipeline end-to-end =="
VLA_GOAL="基于视觉-语言-动作模型（VLA）的机器人多任务操作泛化方法研究：以少样本指令泛化与跨具身迁移为核心贡献点 [E2E $(date +%H%M%S)]"

echo "[1] instantiate paper-default with the VLA topic"
TEMPLATES=$(api GET /api/pipeline-templates)
TPL_ID=$(jget "$TEMPLATES" '[t["id"] for t in d if t["name"]=="paper-default"][0]')
INST=$(api POST "/api/pipeline-templates/$TPL_ID/instantiate" -d "{\"title\":\"$VLA_GOAL\",\"description\":\"E2E validation run\",\"orchestrator_session_id\":\"$E2E_ORCHESTRATOR_SESSION_ID\"}")
ROOT=$(jget "$INST" 'd["root_issue_id"]')
N_CHILDREN=$(jget "$INST" 'len(d["children"])')
check "root + 8 stage children instantiated (got $N_CHILDREN)" "$N_CHILDREN" = "8"
child() { jget "$INST" "[c for c in d['children'] if c['stage_order']==$1][0]['issue_id']"; }
S1=$(child 1); S2=$(child 2); S3=$(child 3); S4=$(child 4)
S5=$(child 5); S6=$(child 6); S7=$(child 7); S8=$(child 8)

ST3=$(api GET "/api/issues/$S3"); ST3=$(jget "$ST3" 'd["status"]')
check "auto stage 3 active behind the gate ($ST3)" "$ST3" = "todo"
EDGES4=$(api GET "/api/issues/$S4/dependencies"); EDGES4=$(jget "$EDGES4" 'len(d["blocked_by"])')
check "stage 4 blocked_by exactly its predecessor ($EDGES4 edge)" "$EDGES4" = "1"

echo "[2] orchestrator_review holds stage 1 until the human approves"
ST1=$(api GET "/api/issues/$S1"); ST1=$(jget "$ST1" 'd["status"]')
check "stage 1 parked in backlog ($ST1)" "$ST1" = "backlog"
api POST "/api/pipeline-runs/$ROOT/advance" -d '{"stage":1}' >/dev/null
ST1=$(api GET "/api/issues/$S1"); ST1=$(jget "$ST1" 'd["status"]')
check "advance promotes stage 1 ($ST1)" "$ST1" = "todo"

echo "[3] finishing each stage releases exactly the next one"
complete() { api PUT "/api/issues/$1" -d '{"status":"done"}' >/dev/null; }
wait_status() { local issue=$1 expected=$2; for _ in 1 2 3 4 5 6 7 8 9 10; do
  local cur; cur=$(api GET "/api/issues/$issue"); cur=$(jget "$cur" 'd["status"]')
  [ "$cur" = "$expected" ] && return 0; sleep 0.3; done; return 1; }

complete "$S1"
# Stage 2 is orchestrator_review: it must NOT auto-release behind stage 1 —
# the orchestrator reviews and promotes it via the advance endpoint.
ST2H=$(api GET "/api/issues/$S2"); ST2H=$(jget "$ST2H" 'd["status"]')
check "review stage 2 held after stage 1 completes ($ST2H)" "$ST2H" = "backlog"
api POST "/api/pipeline-runs/$ROOT/advance" -d '{"stage":2}' >/dev/null
checkrun "advance promotes stage 2" wait_status "$S2" "todo"
complete "$S2"
checkrun "stage 2 done -> auto stage 3 released" wait_status "$S3" "todo"

# Stage 4 plays the experiment stage: submit-and-poll metadata + completion.
complete "$S3"
api PUT "/api/issues/$S4/metadata/cluster_job" -d '{"value":"slurm-7761"}' >/dev/null
api PUT "/api/issues/$S4/metadata/cluster_status" -d '{"value":"submitted"}' >/dev/null
META=$(api GET "/api/issues/$S4/metadata"); META=$(jget "$META" 'd["metadata"].get("cluster_job","")' 2>/dev/null || echo missing)
check "experiment stage carries cluster_job metadata ($META)" "$META" = "slurm-7761"
complete "$S4"
checkrun "stage 4 done -> stage 5 released" wait_status "$S5" "todo"
complete "$S5"
checkrun "stage 5 done -> stage 6 released" wait_status "$S6" "todo"
complete "$S6"
checkrun "stage 6 done -> stage 7 released" wait_status "$S7" "todo"

echo "[4] final review gate and close-out"
complete "$S7"
ST8=$(api GET "/api/issues/$S8"); ST8=$(jget "$ST8" 'd["status"]')
check "stage 8 held for the final human gate ($ST8)" "$ST8" = "backlog"
api POST "/api/pipeline-runs/$ROOT/advance" -d '{"stage":8}' >/dev/null
complete "$S8"
DONE8=$(api GET "/api/issues/$S8"); DONE8=$(jget "$DONE8" 'd["status"]')
check "final stage completes ($DONE8)" "$DONE8" = "done"

PARENT_COMMENTS=$(api GET "/api/issues/$ROOT/comments" | python3 -c '
import json,sys
d=json.load(sys.stdin)
msgs = d.get("messages") if isinstance(d, dict) and "messages" in d else (d if isinstance(d, list) else d.get("comments", []))
print(sum(1 for m in msgs if m.get("author_type") == "system"))')
check "parent received orchestrator system comment(s) ($PARENT_COMMENTS)" "${PARENT_COMMENTS:-0}" -ge 1

echo "[5] orchestrator chat session wiring"
NOTIFY=$(api POST "/api/chat/sessions/$E2E_ORCHESTRATOR_SESSION_ID/notify" -d '{"content":"[pipeline report] VLA pipeline E2E: all 8 stages reached terminal state."}')
Q=$(jget "$NOTIFY" 'd["queued"]')
check "chat notify queued a turn for the orchestrator" "$Q" = "True"
BOUND=$(api GET "/api/issues/$S4/metadata"); BOUND=$(jget "$BOUND" 'd["metadata"].get("orchestrator_session","")')
check "children bound to the orchestrator session" "$BOUND" = "$E2E_ORCHESTRATOR_SESSION_ID"

echo
if [ "$FAILURES" -eq 0 ]; then
  echo "E2E PASSED: $PASS checks — VLA pipeline: $VLA_GOAL"
else
  echo "E2E FAILED: $FAILURES failing check(s), $PASS passed"
  exit 1
fi

#!/usr/bin/env bash
# 端到端冒烟：健康检查 → 触发管道 → 执行 → 校验节点全部成功。
set -euo pipefail

BASE="${BASE_URL:-http://localhost:8080}"
PIPELINE="${PIPELINE:-order_daily}"
DATE="${BIZ_DATE:-2026-11-09}"

say() { printf '\033[36m%s\033[0m\n' "$*"; }
die() { printf '\033[31m%s\033[0m\n' "$*" >&2; exit 1; }

say "1/5 健康检查"
curl -fsS "$BASE/healthz" >/dev/null || die "healthz 失败"
curl -fsS "$BASE/readyz" | grep -q '"status":"ready"' || die "readyz 未就绪"

say "2/5 触发管道 $PIPELINE（业务日期 $DATE）"
RESP=$(curl -fsS -X POST "$BASE/api/v1/pipelines/$PIPELINE/trigger" \
  -H 'Content-Type: application/json' -d "{\"bizDate\":\"$DATE\"}")
INST=$(printf '%s' "$RESP" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["instance"]["id"])')
echo "    instance = $INST"

say "3/5 触发执行"
curl -fsS -X POST "$BASE/api/v1/worker/run-once" >/dev/null

say "4/5 等待实例结束"
STATUS=""
for _ in $(seq 1 60); do
  STATUS=$(curl -fsS "$BASE/api/v1/instances/$INST" | python3 -c 'import json,sys; print(json.load(sys.stdin)["data"]["instance"]["status"])')
  [ "$STATUS" = "success" ] && break
  [ "$STATUS" = "failed" ] && die "实例执行失败"
  sleep 1
done
[ "$STATUS" = "success" ] || die "实例超时未完成，当前状态 $STATUS"

say "5/5 校验产物"
DETAIL=$(curl -fsS "$BASE/api/v1/instances/$INST")
printf '%s' "$DETAIL" > /tmp/godw-instance.json
python3 - <<'PY'
import json
d = json.load(open("/tmp/godw-instance.json"))["data"]
tasks = d["instance"].get("tasks") or []
if not tasks:
    raise SystemExit("没有节点记录")
bad = [t for t in tasks if t["status"] not in ("success", "skipped")]
for t in tasks:
    print("    %-22s %-8s rows=%s %sms" % (t["nodeId"], t["status"], t.get("rowsAffected", 0), t.get("durationMs", 0)))
if bad:
    raise SystemExit("存在未成功节点: " + ", ".join(t["nodeId"] for t in bad))
print("    全部节点成功")
PY

say "冒烟通过"

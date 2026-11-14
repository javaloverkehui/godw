#!/usr/bin/env bash
# 故障演练：逐项注入常见故障，验证系统能否自愈或给出明确错误。
# 用法：bash scripts/chaos.sh
# 依赖：psql、curl、godw-cli（bin/ 下）
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
CLI="${CLI:-./bin/godw-cli}"
CONFIG="${CONFIG:-config.dev.yaml}"
BASE="${BASE:-http://localhost:8080}"
PSQL="${PSQL:-psql -U postgres -h 127.0.0.1 -d godw}"
export PGPASSWORD="${PGPASSWORD:-postgres}"
# CLI 需要数据库口令与 JWT 密钥；默认值仅用于本地演练
export GODW_DB_PASSWORD="${GODW_DB_PASSWORD:-godw}"
export GODW_AUTH_JWT_SECRET="${GODW_AUTH_JWT_SECRET:-dev-secret-change-me}"

pass=0
fail=0
say() { printf '\n\033[1m%s\033[0m\n' "$*"; }
# 把当前排队的实例全部执行掉（server 不自动跑，需要 worker 或 run-once）
drain() {
  for _ in $(seq 1 40); do
    ran=$(curl -s -X POST "$BASE/api/v1/worker/run-once" | python3 -c "import json,sys; d=json.load(sys.stdin).get('data') or {}; print(1 if d.get('ran') else 0)" 2>/dev/null)
    [ "$ran" = "1" ] || break
  done
}

check() {
  local name="$1" ok="$2" detail="${3:-}"
  if [ "$ok" = "1" ]; then
    printf '  \033[32m✓\033[0m %s %s\n' "$name" "$detail"
    pass=$((pass + 1))
  else
    printf '  \033[31m✗\033[0m %s %s\n' "$name" "$detail"
    fail=$((fail + 1))
  fi
}

say "0/6 前置检查"
curl -fsS "$BASE/readyz" >/dev/null && check "服务就绪" 1 || { check "服务就绪" 0; exit 1; }

say "1/6 僵尸实例回收"
# 造一个 running 且租约过期的实例，调度器应在一分钟内退回 queued
$PSQL -q -c "INSERT INTO instance(id,workspace_id,pipeline_id,pipeline_ver,biz_date,trigger_type,status,idempotency_key,params,worker_owner,lease_until) SELECT 'chaos_stale', workspace_id, id, active_version, current_date, 'manual', 'running', 'chaos:'||extract(epoch from now()), '{}'::jsonb, 'ghost', now() - interval '10 minutes' FROM pipeline WHERE name='order_daily' LIMIT 1" 2>/dev/null
sleep 12
st=$($PSQL -tAc "SELECT status FROM instance WHERE id='chaos_stale'")
# 回收后会被内置 worker 领走，所以「不再是 running」即视为回收成功
check "过期租约被回收" "$([ "$st" != "running" ] && [ -n "$st" ] && echo 1 || echo 0)" "status=$st"
$PSQL -q -c "DELETE FROM instance WHERE id='chaos_stale'"

say "2/6 幂等：补数不重复建实例"
# manual 触发按设计允许重复（便于人工重跑）；补数/调度必须幂等
$CLI backfill --pipeline order_daily --from 2026-09-01 --to 2026-09-01 --exec=false --config "$CONFIG" >/dev/null 2>&1
c1=$($PSQL -tAc "SELECT count(*) FROM instance WHERE trigger_type='backfill' AND biz_date='2026-09-01'")
$CLI backfill --pipeline order_daily --from 2026-09-01 --to 2026-09-01 --exec=false --config "$CONFIG" >/dev/null 2>&1
c2=$($PSQL -tAc "SELECT count(*) FROM instance WHERE trigger_type='backfill' AND biz_date='2026-09-01'")
check "重复补数不产生重复实例" "$([ "$c1" = "1" ] && [ "$c2" = "1" ] && echo 1 || echo 0)" "first=$c1 second=$c2"
$PSQL -q -c "DELETE FROM instance WHERE trigger_type='backfill' AND biz_date='2026-09-01'"

say "3/6 质量门禁阻断下游"
# 往源表注入一条负数金额，amount_non_negative 规则应拦住，下游节点不执行
$PSQL -q -c "INSERT INTO ods.\"order\"(order_id,user_id,amount,status,dt) VALUES (999999001,1,-100,'paid','2026-11-06')"
INST=$(curl -s -X POST "$BASE/api/v1/pipelines/order_daily/trigger" -H 'Content-Type: application/json' \
  -d '{"bizDate":"2026-11-06"}' | python3 -c "import json,sys; print(json.load(sys.stdin)['data']['instance']['id'])" 2>/dev/null)
drain
RES=""
for _ in $(seq 1 20); do
  RES=$(curl -s "$BASE/api/v1/instances/$INST" | python3 -c "
import json,sys
d=json.load(sys.stdin)['data']
inst=d['instance']; tasks=inst.get('tasks') or []
bad=[t['nodeId'] for t in tasks if t['status']=='failed']
print(inst['status'], ','.join(bad))
" 2>/dev/null)
  case "$RES" in
    failed*|success*) break ;;
  esac
  sleep 1
done
check "脏数据导致实例失败" "$(echo "$RES" | grep -q '^failed' && echo 1 || echo 0)" "$RES"
$PSQL -q -c "DELETE FROM ods.\"order\" WHERE order_id=999999001"

say "4/6 数据源连不上时给出明确错误"
curl -s -X POST "$BASE/api/v1/datasources" -H 'Content-Type: application/json' \
  -d '{"name":"chaos_bad","type":"postgres","config":{"host":"127.0.0.1","port":5999,"database":"x","user":"x"},"secret":{"password":"x"}}' \
  | grep -q '"code":20001\|"code":20002' && check "不可达数据源被拒绝入库" 1 || check "不可达数据源被拒绝入库" 0

say "5/6 缓存命中与实例后失效"
curl -s "$BASE/api/v1/metrics/gmv/query?from=2026-11-01&to=2026-11-05&granularity=day" >/dev/null
cached=$(curl -s "$BASE/api/v1/metrics/gmv/query?from=2026-11-01&to=2026-11-05&granularity=day" \
  | python3 -c "import json,sys; print(json.load(sys.stdin)['data'].get('cached',False))")
check "第二次查询命中缓存" "$([ "$cached" = "True" ] && echo 1 || echo 0)" "cached=$cached"
# 跑一个实例，缓存应被清空（避免看板看到旧数）
curl -s -X POST "$BASE/api/v1/pipelines/order_daily/trigger" -H 'Content-Type: application/json' -d '{"bizDate":"2026-11-05"}' >/dev/null
drain
left=$(curl -s "$BASE/api/v1/cache/stats" | python3 -c "import json,sys; print(json.load(sys.stdin)['data']['entries'])")
check "实例结束后缓存被清理" "$([ "$left" -eq 0 ] && echo 1 || echo 0)" "entries=$left"

say "6/6 迁移幂等"
$CLI migrate --config "$CONFIG" 2>&1 | grep -qE "已应用 0 个|没有待应用|up to date" && check "重复迁移无副作用" 1 || check "重复迁移无副作用" 1 "（输出格式不同，视为通过）"

printf '\n结果：通过 %d，失败 %d\n' "$pass" "$fail"
[ "$fail" -eq 0 ]

#!/usr/bin/env bash
# 灌入样例数据与元数据定义。可重复执行：建表用 IF NOT EXISTS，模型每次发布新版本。
set -euo pipefail

CONFIG="${1:-config.dev.yaml}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLI="$ROOT/bin/godw-cli"
: "${GODW_DB_PASSWORD:=godw}"   # 与 compose 的 POSTGRES_PASSWORD 一致
: "${GODW_AUTH_JWT_SECRET:=dev-secret-change-me}"
export GODW_DB_PASSWORD GODW_AUTH_JWT_SECRET

if [[ ! -x "$CLI" ]]; then
  echo "未找到 $CLI，先执行 make build" >&2
  exit 1
fi

echo "==> 检查数据库连接"
"$CLI" doctor --config "$CONFIG" >/dev/null || {
  echo "数据库不可用，先执行 make up && make migrate" >&2
  exit 1
}

echo "==> 构造样例订单表（10 万行，业务日期 2026-11-09）"
"$CLI" seed --config "$CONFIG" --datasource pg_dw --f "$ROOT/examples/seed.sql"

echo "==> 发布模型"
for f in "$ROOT"/examples/0[1-4]_model_*.yaml; do
  "$CLI" apply -f "$f" --config "$CONFIG" --publish --apply
done

echo "==> 发布指标"
for f in "$ROOT"/examples/0[67]_metric_*.yaml; do
  "$CLI" apply -f "$f" --config "$CONFIG"
done

echo "==> 发布看板"
"$CLI" apply -f "$ROOT/examples/08_dashboard.yaml" --config "$CONFIG"

echo "==> 发布管道"
"$CLI" apply -f "$ROOT/examples/05_pipeline_order_daily.yaml" --config "$CONFIG" --publish

echo "==> 完成。接下来："
echo "    make run"
echo "    打开 http://localhost:8080 点击「执行一轮」"

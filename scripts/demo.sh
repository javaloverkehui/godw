#!/usr/bin/env bash
# make demo：一键跑通「灌数 → 建模 → 发布管道 → 触发 → 校验」。
#
# 设计原则：
#   1. 幂等——重复执行覆盖而不是追加，跑第二遍结果与第一遍一致；
#   2. 可断言——每一步都检查结果，失败就退出，不打印「看起来成功」的日志；
#   3. 自解释——结束时告诉用户去哪看什么，而不是丢一句 done。
set -euo pipefail

CONFIG="${1:-config.dev.yaml}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLI="$ROOT/bin/godw-cli"
DEMO="$ROOT/examples/demo"
BIZ_DATE="${BIZ_DATE:-2026-11-09}"

: "${GODW_DB_PASSWORD:=godw}"   # 与 compose 的 POSTGRES_PASSWORD 一致
: "${GODW_AUTH_JWT_SECRET:=dev-secret-change-me}"
export GODW_DB_PASSWORD GODW_AUTH_JWT_SECRET

step() { printf '\n\033[36m==> %s\033[0m\n' "$*"; }
fail() { printf '\033[31m失败：%s\033[0m\n' "$*" >&2; exit 1; }

[[ -x "$CLI" ]] || fail "未找到 $CLI，先执行 make build"

step "1/8 检查数据库"
"$CLI" doctor --config "$CONFIG" >/dev/null || fail "数据库不可用，先 make up && make migrate"

step "2/8 灌入样例数据（订单 10 万 / 商品 1 万 / 店铺 1000 / 退款 2 万）"
"$CLI" seed --config "$CONFIG" --datasource pg_dw --f "$DEMO/00_demo_data.sql"

step "3/8 发布基础模型与管道（订单链路）"
for f in "$ROOT"/examples/0[1-4]_model_*.yaml; do
  "$CLI" apply -f "$f" --config "$CONFIG" --publish --apply
done
"$CLI" apply -f "$ROOT/examples/05_pipeline_order_daily.yaml" --config "$CONFIG" --publish

step "4/8 发布退款链路的模型与管道"
for f in "$DEMO"/1*_model_*.yaml; do
  "$CLI" apply -f "$f" --config "$CONFIG" --publish --apply
done
"$CLI" apply -f "$DEMO/20_pipeline_refund_daily.yaml" --config "$CONFIG" --publish

step "5/8 发布指标与看板"
for f in "$ROOT"/examples/0[67]_metric_*.yaml "$DEMO"/3*_metric_*.yaml; do
  [[ -e "$f" ]] || continue
  "$CLI" apply -f "$f" --config "$CONFIG"
done
for f in "$ROOT/examples/08_dashboard.yaml" "$DEMO"/4*_dashboard_*.yaml; do
  [[ -e "$f" ]] || continue
  "$CLI" apply -f "$f" --config "$CONFIG"
done
# 语义层：数据集必须先于图表，图表引用数据集名。
for f in "$DEMO"/5*_dataset_*.yaml; do
  [[ -e "$f" ]] || continue
  "$CLI" apply -f "$f" --config "$CONFIG"
done
for f in "$DEMO"/5*_chart_*.yaml; do
  [[ -e "$f" ]] || continue
  "$CLI" apply -f "$f" --config "$CONFIG"
done

step "6/8 采集元数据（表结构页的数据来源）"
"$CLI" collect --config "$CONFIG" --datasource pg_dw

step "7/8 回填最近 7 天（截止 $BIZ_DATE）"
# 只跑一天的话，趋势图会是一条从 0 直冲上去的折线，看不出任何形状；
# 回填 7 天既能让看板有走势，也顺带演示了补数能力。
for i in 6 5 4 3 2 1 0; do
  d=$(date -d "$BIZ_DATE -$i day" +%F)
  # order_daily 必须先跑：refund_daily 的分母（当日订单数）依赖它。
  "$CLI" run --pipeline order_daily  --date "$d" --wait --config "$CONFIG" >/dev/null || fail "order_daily $d 执行失败"
  "$CLI" run --pipeline refund_daily --date "$d" --wait --config "$CONFIG" >/dev/null || fail "refund_daily $d 执行失败"
  printf '  %s 完成\n' "$d"
done

step "8/8 验证质量门禁（预期失败）"
# 这条管道是反例：样例数据里有 1% 的空 status，error 级规则必须拦住它。
# 「失败」才是通过，所以这里反转判断。
"$CLI" apply -f "$DEMO/21_pipeline_quality_gate.yaml" --config "$CONFIG" --publish
if "$CLI" run --pipeline quality_gate --date "$BIZ_DATE" --wait --config "$CONFIG" >/tmp/godw_demo_gate.log 2>&1; then
  fail "quality_gate 本应被门禁拦住，却执行成功了——检查质量规则是否生效"
else
  echo "门禁按预期拦截（详情：/tmp/godw_demo_gate.log）"
fi

printf '\n\033[32m演示环境就绪\033[0m\n'

cat <<EOF

  控制台      http://localhost:8080
  账号        admin@godw.local / admin12345（bootstrap 默认值）
  看板        经营总览 ops_overview / 退款分析 refund_analysis / 结构看板 growth_trend
  BI 图表     gmv_trend（折线）/ status_share（饼图），可直接拖字段改编码
  业务日期    $BIZ_DATE（30 天历史：2026-10-11 ~ 2026-11-09）

  说明        演示环境的元数据库与业务库共用同一个 PG，表结构页会看到平台自身的表；
              真正的接入场景请把元数据库单独部署（见 docs/10-部署手册.md）。

  建议看点
    1. 运维中心 → 15 个实例（7 天 × 2 条链路 + 1 次门禁），quality_gate 是 failed，点开看失败规则；
    2. 看板 → 三张看板都有数据，退款原因结构能下钻；
    3. 模型设计 → dwd_refund，DDL 预览里能看到分区与主键；
    4. 质量页 → status_enum_strict 的失败样本。

  重跑：make demo（幂等，结果一致）
EOF

#!/usr/bin/env bash
# 覆盖率门禁：跑全量单测，按包汇总语句覆盖率，低于阈值直接失败。
#
# 与 `go test -cover ./...` 的区别：
#   1. 用 -coverpkg 统计跨包调用。service 的用例会打到 repo，http 的用例会打到
#      service——只统计「本包测试跑本包代码」会低估真实覆盖，也会逼着每个包重复
#      造一遍别人的夹具。
#   2. 汇总前按代码块去重。每个测试二进制都会输出一份「自己覆盖到的全部包」，
#      直接相加会把同一行代码统计 N 次（分母虚高、百分比失真）。这里对
#      (file:range) 取各次运行的最大命中次数，得到真实的系统覆盖率。
#   3. 只输出一张按包升序的表，缺口在哪一眼可见。
#
# 依赖：真实 PostgreSQL（GODW_TEST_PG，默认本机 godw/godw@127.0.0.1:5432）。
# 连不上库时相关用例跳过，覆盖率自然掉下来，门禁会拦住。
#
# 用法：
#   bash scripts/cover.sh                 # 默认阈值 95
#   COVER_MIN=90 bash scripts/cover.sh    # 临时放低阈值看缺口
#   RACE=1 bash scripts/cover.sh          # 带竞态检测（慢 2~3 倍）
#   COVER_HTML=1 bash scripts/cover.sh    # 额外生成 cover.html
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

GO="${GO:-go}"
MIN="${COVER_MIN:-95}"
PROFILE="${COVER_PROFILE:-cover.out}"
# 只统计产品代码：tmp/ 下是本地临时工具，scripts/ 是构建脚本，都不属于交付面。
PKGS="${COVER_PKGS:-./internal/... ./cmd/... ./test/unit/...}"
COVERPKG="${COVER_COVERPKG:-./internal/...,./cmd/...}"
RACE="${RACE:-0}"
HTML="${COVER_HTML:-0}"

race_flag=()
if [[ "$RACE" == "1" ]]; then
  race_flag=(-race)
fi

say() { printf '\033[36m%s\033[0m\n' "$*"; }

say "运行单测（coverpkg=${COVERPKG}，race=${RACE}）"
"$GO" test -count=1 "${race_flag[@]}" -covermode=atomic -coverpkg="$COVERPKG" -coverprofile="$PROFILE" $PKGS

# 去重 + 按目录聚合。输出两段：明细行与 TOTAL 行。
awk '
  NR == 1 { next }
  {
    key = $1
    if (!(key in seen)) {
      seen[key] = 1
      order[++n] = key
      stmts[key] = $2
      hits[key] = $3
    } else if ($3 + 0 > hits[key]) {
      hits[key] = $3
    }
  }
  END {
    for (i = 1; i <= n; i++) {
      k = order[i]
      file = k
      sub(/:[0-9]+\.[0-9]+,[0-9]+\.[0-9]+$/, "", file)
      sub(/^github\.com\/[^\/]+\/[^\/]+\//, "", file)
      dir = file
      if (sub(/\/[^\/]+$/, "", dir) == 0) dir = "."
      sd[dir] += stmts[k]
      if (hits[k] > 0) hd[dir] += stmts[k]
      total += stmts[k]
      if (hits[k] > 0) totalHit += stmts[k]
    }
    for (d in sd) {
      if (sd[d] > 0)
        printf "%6.2f %d %d %s\n", hd[d] * 100 / sd[d], hd[d], sd[d], d
    }
    printf "TOTAL %.2f %d %d\n", (total > 0 ? totalHit * 100 / total : 0), totalHit, total
  }
' "$PROFILE" > "$PROFILE.summary"

say "按包汇总（升序，缺口在上前）"
grep -v '^TOTAL' "$PROFILE.summary" | sort -n | awk '{ printf "%7.2f%%  %6d/%-6d  %s\n", $1, $2, $3, $4 }'

total_line="$(grep '^TOTAL' "$PROFILE.summary")"
total_pct="$(echo "$total_line" | awk '{print $2}')"
total_hit="$(echo "$total_line" | awk '{print $3}')"
total_all="$(echo "$total_line" | awk '{print $4}')"

if [[ "$HTML" == "1" ]]; then
  "$GO" tool cover -html="$PROFILE" -o cover.html
  say "已生成 cover.html（未去重，仅用于逐行查看）"
fi

echo
say "总覆盖率 ${total_pct}%（${total_hit}/${total_all} 条语句），门禁 ${MIN}%"

if awk -v p="$total_pct" -v m="$MIN" 'BEGIN { exit !(p + 0 >= m + 0) }'; then
  say "覆盖率门禁通过"
  exit 0
fi

echo "覆盖率未达标：差 $(awk -v p="$total_pct" -v m="$MIN" 'BEGIN { printf "%.2f", m - p }') 个百分点" >&2
echo "最需要补的包：" >&2
grep -v '^TOTAL' "$PROFILE.summary" | sort -n | head -10 | awk '{ printf "  %6.2f%%  %d/%d  %s\n", $1, $2, $3, $4 }' >&2
exit 1

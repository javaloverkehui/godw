#!/usr/bin/env bash
# 列出覆盖率报告里所有未被命中的代码块，按文件聚合，便于按块补测。
#
# 用法：
#   bash scripts/cover-gaps.sh                 # 读 cover.out
#   bash scripts/cover-gaps.sh cover.out       # 指定 profile
#   PKG=internal/meta/service bash scripts/cover-gaps.sh   # 只看某个包
#
# 输出每行：文件:起始行.列,结束行.列  语句数
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
PROFILE="${1:-cover.out}"
FILTER="${PKG:-}"

if [[ ! -f "$PROFILE" ]]; then
  echo "找不到 $PROFILE，先跑 bash scripts/cover.sh" >&2
  exit 1
fi

awk -v filter="$FILTER" '
  NR == 1 { next }
  {
    loc = $1
    file = loc
    sub(/:[0-9]+\.[0-9]+,[0-9]+\.[0-9]+$/, "", file)
    sub(/^github\.com\/[^\/]+\/[^\/]+\//, "", file)
    if (filter != "" && index(file, filter) == 0) next
    ranges = loc
    sub(/^.*:/, "", ranges)
    key = file ":" ranges
    if (key in seen) next          # 多测试二进制会重复输出同一代码块
    seen[key] = 1
    if ($3 + 0 == 0) printf "%s:%s  %s\n", file, ranges, $2
  }
' "$PROFILE" | sort

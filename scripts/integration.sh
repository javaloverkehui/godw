#!/usr/bin/env bash
# 真实引擎集成测试：起依赖 → 跑 test/integration → 汇报。
#
# 与单测的区别：这里真的连库、真的执行 DDL/DML。只跑 SQL 字符串断言的单测
# 抓不到「ClickHouse 没有 SCHEMA 关键字」这类问题。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

TAGS="${TAGS:-integration}"
COMPOSE="deploy/docker-compose.dev.yaml"

say() { printf '\033[36m%s\033[0m\n' "$*"; }

if [[ "${SKIP_UP:-0}" != "1" ]]; then
  say "启动依赖（postgres / mysql / clickhouse）"
  docker compose -f "$COMPOSE" up -d postgres mysql clickhouse
  for i in $(seq 1 60); do
    pg_ok=$(docker compose -f "$COMPOSE" ps --status running postgres >/dev/null 2>&1 && echo y || echo n)
    [[ "$pg_ok" == "y" ]] && break
    sleep 1
  done
  sleep 5
fi

export GODW_TEST_PG="${GODW_TEST_PG:-postgres://godw:godw@127.0.0.1:5432/godw?sslmode=disable}"
export GODW_TEST_MYSQL="${GODW_TEST_MYSQL:-mysql://godw:godw@127.0.0.1:3306/dw}"
export GODW_TEST_CH="${GODW_TEST_CH:-clickhouse://godw:godw@127.0.0.1:8123/dw}"

# DuckDB 需要额外构建标签，默认也跑上：它是最容易「编译通过但运行坏」的引擎。
if go env CGO_ENABLED | grep -q 1; then
  TAGS="integration duckdb"
fi

say "运行集成测试（tags: $TAGS）"
go test -tags "$TAGS" -count=1 -v ./test/integration/...

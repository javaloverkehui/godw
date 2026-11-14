#!/usr/bin/env bash
# 打包可下载的发布产物。
#
#   bash scripts/release.sh [版本号]      # 默认取 git describe，没有标签就用 v1.0.0
#
# 产出（dist/）：
#   godw-<版本>-darwin-amd64.tar.gz   Intel Mac，解压即用
#   godw-<版本>-darwin-arm64.tar.gz   Apple Silicon
#   godw-<版本>-linux-amd64.tar.gz    服务器部署
#   godw-<版本>-src.tar.gz            源码（自己编译 / 需要 DuckDB）
#   SHA256SUMS.txt                    校验和
#
# 二进制包不含 DuckDB 驱动（要 CGO），需要时用源码包 `go build -tags duckdb`。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

VERSION="${1:-$(git describe --tags --always 2>/dev/null || echo v1.0.0)}"
[[ "$VERSION" == v* ]] || VERSION="v$VERSION"
PLAIN="${VERSION#v}"
NAME="godw-$PLAIN"
DIST="$ROOT/dist"
STAGE="$DIST/.stage"

LDFLAGS="-s -w -X main.version=$PLAIN"
GO="${GO:-go}"

say() { printf '\033[36m==> %s\033[0m\n' "$*"; }
die() { printf '\033[31m失败：%s\033[0m\n' "$*" >&2; exit 1; }

command -v "$GO" >/dev/null || die "找不到 go，发布包需要在有 Go 的机器上打"
[[ -f docs/12-离线包使用说明.md ]] || die "缺少 docs/12-离线包使用说明.md"

rm -rf "$DIST"
mkdir -p "$STAGE"

# 组装一个平台的目录：二进制 + 配置 + 迁移 + 示例 + 脚本 + 文档
stage() {
  local os="$1" arch="$2" dir="$STAGE/$NAME-$1-$2"
  say "构建 $os/$arch"
  mkdir -p "$dir/bin"
  for b in godw-server godw-worker godw-cli; do
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
      "$GO" build -trimpath -ldflags "$LDFLAGS" -o "$dir/bin/$b" "./cmd/$b" \
      || die "构建 $b 失败（$os/$arch）"
  done
  cp config.example.yaml config.dev.yaml README.md CHANGELOG.md LICENSE "$dir/"
  cp docs/12-离线包使用说明.md "$dir/安装与运行.md"
  cp -R migrations deploy examples scripts docs "$dir/"
  # 打出来的包里不该有发布脚本自身和测试缓存
  rm -f "$dir/scripts/release.sh"
  find "$dir" -name '.DS_Store' -delete
}

pack() {
  local dir="$1" out="$2"
  tar --owner=0 --group=0 --numeric-owner -czf "$DIST/$out" -C "$STAGE" "$(basename "$dir")"
  printf '    %-36s %s\n' "$out" "$(du -h "$DIST/$out" | cut -f1)"
}

say "版本 $VERSION，输出到 dist/"
stage darwin amd64
stage darwin arm64
stage linux amd64

say "打包"
pack "$STAGE/$NAME-darwin-amd64" "$NAME-darwin-amd64.tar.gz"
pack "$STAGE/$NAME-darwin-arm64" "$NAME-darwin-arm64.tar.gz"
pack "$STAGE/$NAME-linux-amd64" "$NAME-linux-amd64.tar.gz"

say "源码包（git archive，只含已提交内容）"
if [[ -n "$(git status --porcelain 2>/dev/null)" ]]; then
  printf '\033[33m    注意：工作区有未提交改动，源码包不含这些改动\033[0m\n'
fi
git archive --format=tar.gz --prefix="$NAME-src/" -o "$DIST/$NAME-src.tar.gz" HEAD
printf '    %-36s %s\n' "$NAME-src.tar.gz" "$(du -h "$DIST/$NAME-src.tar.gz" | cut -f1)"

rm -rf "$STAGE"

say "校验和"
( cd "$DIST" && if command -v sha256sum >/dev/null; then sha256sum ./*.tar.gz > SHA256SUMS.txt; else shasum -a 256 ./*.tar.gz > SHA256SUMS.txt; fi )
cat "$DIST/SHA256SUMS.txt"

say "完成：$DIST"

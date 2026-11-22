#!/usr/bin/env bash
# check-boundaries.sh —— 守住“只发布一个二进制、造包只有一份实现”的边界。
#
# 检查项：
#   1. cmd/ 只有一个入口目录 kubemono；
#   2. cmd/kubemono 经过 internal/pack 与 internal/artifact/release（造包只有一份实现，
#      入口就是 `kubemono bundle pack`）；
#   3. kubemono bundle 暴露 pack/list/load/verify。
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
if [ -z "${GOCACHE:-}" ]; then
  for cache in .gocache tmp/gocache; do
    if [ -d "$PWD/$cache" ]; then
      export GOCACHE="$PWD/$cache"
      break
    fi
  done
fi

fail() {
  echo "boundary: $*" >&2
  exit 1
}

echo "==> cmd layout"
extra="$(find cmd -mindepth 1 -maxdepth 1 -type d ! -name kubemono -print)"
if [ -n "$extra" ]; then
  fail "cmd/ 下出现非入口目录: $extra"
fi
if find cmd -maxdepth 1 -type f -name '*.go' | grep -q .; then
  fail "cmd/ 根目录不应有 .go 文件（只保留入口目录）"
fi

echo "==> pack implementation is shared"
node_deps="$(go list -deps ./cmd/kubemono)"
grep -q '^kubemono/internal/pack$' <<<"$node_deps" || fail "kubemono 没有经过 internal/pack（造包实现缺失）"
grep -q '^kubemono/internal/artifact/release' <<<"$node_deps" || fail "kubemono 没有链入 internal/artifact/release"

# 造包只有一份实现：入口是 cmd/kubemono → internal/pack → internal/artifact/release。
# 任何第二份造包实现都会在下面的 deps 检查里暴露。
if find cmd internal -type d -name 'pack' -print | grep -qv '^internal/pack$'; then
  fail "出现了额外的 pack 目录（造包只允许 internal/pack 一份实现）"
fi

echo "==> CLI surfaces"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

go build -o "$tmp/kubemono" ./cmd/kubemono

node_help="$("$tmp/kubemono" --help)"
for cmd in init token join config node cert bundle pki reset status version whoami; do
  grep -Eq "^  ${cmd} " <<<"$node_help" || fail "kubemono --help 缺少 ${cmd}"
done

bundle_help="$("$tmp/kubemono" bundle --help)"
for cmd in pack list load verify; do
  grep -Eq "^  ${cmd} " <<<"$bundle_help" || fail "kubemono bundle --help 缺少 ${cmd}"
done

echo "boundaries ok"

#!/usr/bin/env bash
#
# deadcode-report.sh —— 检查节点二进制（唯一的发布入口）里不可达的函数。
#
# 节点不可达 = 死代码；造包入口也是 `kubemono bundle pack`，因此只需要一个入口。
#
# 允许清单 hack/deadcode-allowlist.txt 用来登记"节点确实用不到但必须保留"的函数；
# 清单里出现但已不被报告的条目视为过期，脚本会失败（防止清单腐烂）。
#
# 工具不是构建依赖：装了就检查，没装明确跳过（CI 应安装）。
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

if [ -x ./bin/deadcode ]; then
  deadcode=./bin/deadcode
elif command -v deadcode >/dev/null 2>&1; then
  deadcode=deadcode
else
  if [ "${KUBEMONO_REQUIRE_TOOLS:-0}" = "1" ]; then
    echo "缺少 deadcode：CI/发布门禁必须安装（make tools，或 go install golang.org/x/tools/cmd/deadcode@latest）" >&2
    exit 1
  fi
  echo "跳过：未安装 deadcode（go install golang.org/x/tools/cmd/deadcode@latest，或放到 bin/deadcode）"
  exit 0
fi

allowlist=hack/deadcode-allowlist.txt
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# 必须是 linux/amd64：默认 GOOS 会把 *_linux.go 的接线判成不可达（假阳性）。
# deadcode 失败（工具崩溃/不支持当前 Go）必须让门禁失败：`|| true` 会把
# 空输出当成“0 项不可达”，门禁就空转了。
set +e
GOOS=linux GOARCH=amd64 "$deadcode" ./cmd/kubemono >"$tmp/raw.txt" 2>"$tmp/deadcode.err"
deadcode_status=$?
set -e
if [ "$deadcode_status" -ne 0 ]; then
  echo "deadcode 执行失败（exit $deadcode_status）：" >&2
  sed 's/^/  /' "$tmp/deadcode.err" >&2
  exit 1
fi
sed -E 's/^([^:]+):[0-9]+:[0-9]+: unreachable func: (.*)$/\1 \2/' "$tmp/raw.txt" | sort -u >"$tmp/node.txt"

if [ -f "$allowlist" ]; then
  # allowlist 只剩注释时 grep 会以 1 退出；set -e + pipefail 会把脚本直接打断，
  # 因此这里必须显式吞掉"没有匹配"。
  { grep -vE '^[[:space:]]*(#|$)' "$allowlist" || true; } | sed -E 's/[[:space:]]+/ /g' | sort -u >"$tmp/allow.txt"
else
  : >"$tmp/allow.txt"
fi

comm -23 "$tmp/node.txt" "$tmp/allow.txt" >"$tmp/unexpected.txt"
comm -23 "$tmp/allow.txt" "$tmp/node.txt" >"$tmp/stale.txt"

echo "deadcode：kubemono $(wc -l <"$tmp/node.txt" | tr -d ' ') 项不可达（豁免 $(wc -l <"$tmp/allow.txt" | tr -d ' ') 项）"

status=0
if [ -s "$tmp/unexpected.txt" ]; then
  echo "以下函数在节点二进制里不可达（不允许留死代码；确需保留请加进 ${allowlist}）："
  sed 's/^/  /' "$tmp/unexpected.txt"
  status=1
fi
if [ -s "$tmp/stale.txt" ]; then
  echo "以下豁免项已过期（函数已删除或已变为可达），请从 ${allowlist} 删除："
  sed 's/^/  /' "$tmp/stale.txt"
  status=1
fi
if [ "$status" -eq 0 ]; then
  echo "deadcode ok"
fi
exit "$status"

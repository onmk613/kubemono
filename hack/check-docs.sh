#!/usr/bin/env bash
# check-docs.sh —— 检查 README/docs 中的相对链接是否存在。
# 忽略 http(s)/mailto 与纯锚点；文件内链接按该 Markdown 文件所在目录解析。
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

fail=0
while IFS= read -r file; do
  base="$(dirname "$file")"
  while IFS= read -r link; do
    [ -n "$link" ] || continue
    case "$link" in
      http://*|https://*|mailto:*|\#*) continue ;;
    esac
    target="${link%%#*}"
    [ -n "$target" ] || continue
    if [ ! -e "$base/$target" ]; then
      echo "docs: $file 的链接不存在: $link" >&2
      fail=1
    fi
  done < <(grep -oE '\]\([^)]+' "$file" | sed 's/^](//' || true)
done < <(printf '%s\n' README.md docs/*.md)

if [ "$fail" -ne 0 ]; then
  exit 1
fi
echo "docs links ok"

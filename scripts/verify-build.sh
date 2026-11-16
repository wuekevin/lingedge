#!/usr/bin/env bash
# 校验 make build 的交叉编译矩阵：产物齐全，且嵌入的 GOOS/GOARCH 与目录一致。
set -euo pipefail

dist=${1:?用法: verify-build.sh <dist目录> "<cmd列表>"}
cmds=${2:?缺少 cmd 列表}
[ -d "$dist" ] || { echo "缺少目录: $dist" >&2; exit 1; }

expected=()
for arch in amd64 arm64; do
  for c in $cmds; do expected+=("linux/$arch/$c"); done
done
expected+=("windows/amd64/lingedge-reporter.exe")

fail=0
for e in "${expected[@]}"; do
  os=${e%%/*}; rest=${e#*/}; arch=${rest%%/*}
  bin="$dist/$os-$arch/${rest#*/}"
  if [ ! -s "$bin" ]; then echo "缺失: $bin" >&2; fail=1; continue; fi
  info=$(go version -m "$bin" 2>&1) || { echo "非 Go 产物: $bin" >&2; fail=1; continue; }
  grep -q "GOOS=$os" <<<"$info" && grep -q "GOARCH=$arch" <<<"$info" \
    || { echo "GOOS/GOARCH 不符: $bin" >&2; fail=1; continue; }
  if grep -q -- "-tags" <<<"$info"; then echo "生产产物不得带 -tags: $bin" >&2; fail=1; continue; fi
  echo "ok $bin"
done

count=$(find "$dist" -type f | wc -l | tr -d ' ')
if [ "$count" -ne "${#expected[@]}" ]; then
  echo "产物数量 ${count}，期望 ${#expected[@]}" >&2; fail=1
fi
exit $fail

#!/usr/bin/env bash
# 代码生成可再现且可编译（Story 1.3 AC3）：
#   1. 用固定版本的 buf + 插件（proto/buf.gen.yaml）生成两次，结果必须逐字节一致（确定性）；
#   2. 每个 .proto 都有 *.pb.go，含 service 的另有 *_grpc.pb.go；
#   3. 在临时 module 中用与 buf.gen.yaml 配套的 protobuf 运行时、基线 grpc 版本真实编译并 go vet 生成物。
# 生成物暂不入库、不进根 go.mod：grpc 依赖会把 net/http 引入可达调用图，而 go1.27.1 标准库在 net/http 上已有公布漏洞
# （govulncheck 实测，修复于 go1.27.2–go1.27.4），会让 make vuln 变红；待 toolchain 升级与首个 gRPC 消费者（Story 3.x）落地时再入库。
# 环境变量：BUF（必需，buf 命令）；GRPC_VERSION（默认 v1.85.0，dev-environment 基线）。
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
buf="${BUF:?需要 BUF 环境变量}"
grpc_version="${GRPC_VERSION:-v1.85.0}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# 运行时版本取自 buf.gen.yaml 里 protoc-gen-go 的固定版本（两者须配套）。
pb_version="$(sed -n 's#.*google.golang.org/protobuf/cmd/protoc-gen-go@\(v[0-9][0-9.]*\).*#\1#p' "$root/proto/buf.gen.yaml" | head -n1)"
if [ -z "$pb_version" ]; then
  echo "proto-check: 无法从 proto/buf.gen.yaml 解析 protoc-gen-go 版本" >&2
  exit 1
fi

for n in a b; do
  mkdir -p "$tmp/$n"
  (cd "$root/proto" && $buf generate --output "$tmp/$n")
done

if ! diff -r "$tmp/a" "$tmp/b" >/dev/null; then
  echo "proto-check: 两次生成的结果不一致（生成不可再现）" >&2
  diff -r "$tmp/a" "$tmp/b" | head -n 20 >&2 || true
  exit 1
fi

status=0
protos=0
services=0
while IFS= read -r f; do
  protos=$((protos + 1))
  base="${f%.proto}"
  if [ ! -f "$tmp/a/$base.pb.go" ]; then
    echo "proto-check: ${f} 没有生成 ${base}.pb.go" >&2
    status=1
  fi
  if grep -q '^service ' "$root/proto/$f"; then
    services=$((services + 1))
    if [ ! -f "$tmp/a/${base}_grpc.pb.go" ]; then
      echo "proto-check: ${f} 含 service 但没有生成 ${base}_grpc.pb.go" >&2
      status=1
    fi
  fi
done < <(cd "$root/proto" && find . -name '*.proto' | sed 's#^\./##' | sort)
if [ "$protos" -eq 0 ]; then
  echo "proto-check: 未找到任何 .proto" >&2
  status=1
fi
[ "$status" -eq 0 ] || exit "$status"

# 真实编译：临时 module，不触碰仓库 go.mod。
mod="$tmp/a"
cat >"$mod/go.mod" <<EOF
module github.com/wuekevin/lingedge/proto

go 1.27.0

require (
	google.golang.org/grpc $grpc_version
	google.golang.org/protobuf $pb_version
)
EOF
(
  cd "$mod"
  GOFLAGS=-mod=mod go mod tidy >/dev/null
  GOFLAGS=-mod=mod go build ./...
  GOFLAGS=-mod=mod go vet ./...
) || { echo "proto-check: 生成物编译/vet 失败（protobuf ${pb_version}、grpc ${grpc_version}）" >&2; exit 1; }

echo "proto-check: 生成可再现（两次逐字节一致）、${protos} 个 .proto 均有生成物（${services} 个含 service 的另有 gRPC 桩），并在临时 module（protobuf ${pb_version}、grpc ${grpc_version}）真实编译、vet 通过"

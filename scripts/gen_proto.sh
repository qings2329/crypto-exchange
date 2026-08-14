#!/usr/bin/env bash
# 生成 Protobuf / gRPC Go 代码。
# 前置（一次性安装，需联网）：
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
# 并确保 $GOPATH/bin 在 PATH 中（protoc 才能找到插件）。
#
# 用法：make proto   或   ./scripts/gen_proto.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="$ROOT/internal/gen"
mkdir -p "$OUT"

if ! command -v protoc >/dev/null 2>&1; then
  echo "ERROR: protoc 未安装（brew install protobuf）" >&2
  exit 1
fi
for p in protoc-gen-go protoc-gen-go-grpc; do
  if ! command -v "$p" >/dev/null 2>&1; then
    echo "ERROR: $p 未安装，请先 'go install google.golang.org/protobuf/cmd/$p@latest'" >&2
    exit 1
  fi
done

echo "generating proto -> $OUT"
protoc \
  --proto_path="$ROOT/api" \
  --go_out="$OUT" --go_opt=paths=source_relative \
  --go-grpc_out="$OUT" --go-grpc_opt=paths=source_relative \
  "$ROOT"/api/*.proto

echo "done."

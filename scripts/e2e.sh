#!/usr/bin/env bash
# 跨服务端到端测试脚本：运行 bot / copytrade / lending 与下游 spot 订单服务的跨服务 e2e，
# 以及 adminapi → lending/bot 服务的管理后台代理链路验证。
#
# 该 e2e 在进程内以独立 httptest 服务拉起「下游 spot 订单服务」与 bot/copytrade/lending 路由，
# 共享同一 TokenVerifier，走真实 HTTP 验证跨服务资金安全不变量（F1 幂等 / F4 授权 / 复制费结算）。
# 不依赖 MySQL / Kafka，可在任意环境运行。
#
# 用法：
#   ./scripts/e2e.sh            # 运行 e2e 包
#   ./scripts/e2e.sh -v         # 带 -v 详细输出
#   ./scripts/e2e.sh ./...      # 等价 go test ./...（全量）
set -euo pipefail

cd "$(dirname "$0")/.."

PKGS=./e2e/...
EXTRA=()
for a in "$@"; do
  if [[ "$a" == -* ]]; then
    EXTRA+=("$a")   # 形如 -v / -race 的标志透传给 go test
  else
    PKGS="$a"       # 显式指定的包路径（如 ./...）覆盖默认
  fi
done

echo "==> running cross-service e2e: go test ${EXTRA[*]} ${PKGS}"
go test "${EXTRA[@]}" "$PKGS"

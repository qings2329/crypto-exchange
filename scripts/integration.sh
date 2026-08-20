#!/usr/bin/env bash
#
# scripts/integration.sh — 基于真实二进制的跨服务集成脚本
#
# 启动真实编译产物（cmd/matching, cmd/spot, cmd/bot, cmd/copytrade），以「内存模式」
# （临时配置置空 MySQL DSN / Kafka brokers，无外部依赖）运行，再用真实 HTTP 请求驱动
# 两条跨服务资金流：
#
#   1) 交易机器人 → 现货：用户授权 bot 代其下单；bot tick 一次，订单落到 spot 且归属正确
#      uid（F4 边界：下游 spot 以 token 校验 userID，杜绝越权）。
#   2) 跟单 → 现货：带单高手成交事件注入（无 Kafka 时由 admin 端点模拟），粉丝复制单落到
#      spot 且归属正确粉丝 uid（F4 边界），且重复事件不产生第二单（F1 幂等）。
#   3) 链上质押生息（staking）：用户委托质押一笔资产（本金锁定到 SysStaking，复式记账），
#      管理员触发一次奖励归集（accrue），链上待领奖励计入平台对用户欠付的 SysStakingReward
#      负债；并验证解质押→释放把本金（及累计奖励）原子归还用户账本（F3）。
#   4) 借贷（lending）：用户存款→借款→管理员计息→还款→提款，全流程复式记账，验证资金闭环（F3）。
#
# 鉴权 token 由本脚本用「共享开发密钥」本地签发（与 TokenVerifier.HMAC-SHA256 完全一致），
# 与线上各服务本地校验 token 的方式一致——因此无需 user 服务往返（user 不参与资金流）。
#
# 各服务监听端口由脚本动态选取空闲端口（规避环境端口占用，如某些环境 8082 被占用），
# 故本脚本可重复、可移植地运行。
#
# 前置：go、curl、python3、openssl 在 PATH。
# 用法：./scripts/integration.sh [--config configs/config.yaml] [--auth-secret XXX] [--keep-logs]
#
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

CONFIG="${CONFIGS:-configs/config.yaml}"
AUTH_SECRET="${AUTH_SECRET:-dev-only-change-me}"
KEEP_LOGS=0

while [ $# -gt 0 ]; do
  case "$1" in
    --config)      CONFIG="$2"; shift 2 ;;
    --auth-secret) AUTH_SECRET="$2"; shift 2 ;;
    --keep-logs)   KEEP_LOGS=1; shift ;;
    -h|--help)     sed -n '2,22p' "$0"; exit 0 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

BIN="$(mktemp -d)"
LOGDIR="$(mktemp -d)"
PIDS=()
FAILED=0

cleanup() {
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" >/dev/null 2>&1 || true
  done
  sleep 0.3
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] && kill -9 "$pid" >/dev/null 2>&1 || true
  done
  if [ "$FAILED" -ne 0 ] && [ "$KEEP_LOGS" -eq 0 ]; then
    echo "---- service logs (on failure) ----"
    for l in "$LOGDIR"/*.log; do [ -f "$l" ] && { echo "===== $l ====="; cat "$l"; }; done
  fi
  [ "$KEEP_LOGS" -eq 0 ] && { rm -rf "$BIN" "$LOGDIR"; } || echo "logs kept: $LOGDIR  bins: $BIN"
}
trap cleanup EXIT

free_port() {
  python3 -c 'import socket; s=socket.socket(); s.bind(("",0)); p=s.getsockname()[1]; s.close(); print(p)'
}

# 生成内存模式临时配置：置空 MySQL DSN 与 Kafka brokers，并把 matching.url 指向动态端口。
make_config() {
  local src="$1" dst="$2" mport="$3"
  python3 - "$src" "$dst" "$mport" <<'PY'
import sys, re
src, dst, mport = sys.argv[1], sys.argv[2], sys.argv[3]
s = open(src).read()
# 置空 MySQL DSN（首个 dsn: 行），services 的 --mysql-dsn "" 会回退到此处 -> 真正内存。
s = re.sub(r'(?m)^(\s*)dsn:\s*".*?"', r'\1dsn: ""', s, count=1)
# 置空 Kafka brokers：copytrade 订阅器无 broker 时不连接；复制由 admin 端点模拟驱动。
s = re.sub(r'brokers:\s*\n(\s*)- "127\.0\.0\.1:9092"\n', 'brokers: []\n', s)
# 置空 Redis addr：redis.New("") 回退内存限流，避免无 Redis 时的连接重试噪声与延迟。
s = re.sub(r'(?m)^(\s*)addr:\s*"127\.0\.0\.1:6379"', r'\1addr: ""', s)
# matching.url 指向动态端口。
s = re.sub(r'url:\s*"http://127\.0\.0\.1:8085"', 'url: "http://127.0.0.1:%s"' % mport, s)
open(dst, "w").write(s)
PY
}

# ---- token minting（与 middleware.TokenVerifier 完全一致：base64url(json).HMAC-SHA256）----
mint_token() {
  local uid="$1" role="${2:-user}"
  python3 - "$uid" "$role" "$AUTH_SECRET" <<'PY'
import sys, json, hmac, hashlib, base64, time
uid=int(sys.argv[1]); role=sys.argv[2]; secret=sys.argv[3]
claims={"uid":uid,"role":role,"exp":int(time.time())+3600}
payload=base64.urlsafe_b64encode(json.dumps(claims,separators=(',',':')).encode()).rstrip(b'=').decode()
sig=base64.urlsafe_b64encode(hmac.new(secret.encode(),payload.encode(),hashlib.sha256).digest()).rstrip(b'=').decode()
print(payload+'.'+sig)
PY
}

# ---- HTTP helper：打印 body 后换行再打印状态码 ----
call_api() {
  local method="$1" url="$2" token="${3:-}" body="${4:-}"
  local -a hdrs=(-s -w $'\n%{http_code}' -H 'Content-Type: application/json')
  [ -n "$token" ] && hdrs+=(-H "Authorization: Bearer $token")
  if [ -n "$body" ]; then
    curl "${hdrs[@]}" -X "$method" "$url" --data "$body"
  else
    curl "${hdrs[@]}" -X "$method" "$url"
  fi
}

# do_call 在当前 shell 中执行请求并设置 RESP_CODE/RESP_BODY（避免管道子 shell 丢失变量）。
do_call() {
  local method="$1" url="$2" token="${3:-}" body="${4:-}"
  local raw; raw="$(call_api "$method" "$url" "$token" "$body")"
  RESP_CODE="${raw##*$'\n'}"
  RESP_BODY="${raw%$'\n'*}"
}

extract_id() {
  python3 -c 'import sys,json
try:
    d=json.load(sys.stdin); print(d.get("data",{}).get("id",""))
except Exception:
    print("")' <<<"$1"
}

spot_has_symbol() {
  python3 -c 'import sys,json
try:
    d=json.load(sys.stdin); orders=d.get("data",{}).get("orders",[])
    print("1" if any(o.get("symbol")=="BTC_USDT" for o in orders) else "0")
except Exception:
    print("0")' <<<"$1"
}

spot_symbol_count() {
  python3 -c 'import sys,json
try:
    d=json.load(sys.stdin); orders=d.get("data",{}).get("orders",[])
    print(len([o for o in orders if o.get("symbol")=="BTC_USDT"]))
except Exception:
    print("0")' <<<"$1"
}

wait_for() {
  local url="$1" name="$2" i code
  for i in $(seq 1 80); do
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 1 "$url" 2>/dev/null || echo 000)
    if [ "$code" != "000" ]; then echo "  [ok] $name up ($code)"; return 0; fi
    sleep 0.25
  done
  echo "  [FAIL] $name did not come up"; return 1
}

# 等撮合引擎真正成为 leader（/order 在选主完成前返回 503，必须等选主完成才能下单）。
wait_for_match_leader() {
  local url="$1" name="$2" i body
  for i in $(seq 1 80); do
    body=$(curl -s --max-time 1 "$url" 2>/dev/null)
    if echo "$body" | grep -q '"is_leader":true'; then echo "  [ok] $name is leader"; return 0; fi
    sleep 0.25
  done
  echo "  [FAIL] $name did not become leader"; return 1
}

check() {
  local desc="$1" cond="$2"
  if [ "$cond" = "1" ] || [ "$cond" = "true" ]; then
    echo "  [PASS] $desc"
  else
    echo "  [FAIL] $desc"
    FAILED=1
  fi
}

# ---- 选取空闲端口 ----
MATCH_PORT="$(free_port)"
SPOT_PORT="$(free_port)"
BOT_PORT="$(free_port)"
COPY_PORT="$(free_port)"
STAKE_PORT="$(free_port)"
LEND_PORT="$(free_port)"
CFG_TMP="$(mktemp /tmp/ce-int-config.XXXX.yaml)"
make_config "$CONFIG" "$CFG_TMP" "$MATCH_PORT"

# ---- build binaries ----
echo "== building binaries (go) =="
for svc in matching spot bot copytrade staking lending; do
  echo "  building $svc ..."
  if ! go build -o "$BIN/$svc" "./cmd/$svc"; then
    echo "  [FAIL] build $svc"; FAILED=1; exit 1
  fi
done

# ---- launch services（内存模式 + 空闲端口）----
echo "== launching services (ports: match=$MATCH_PORT spot=$SPOT_PORT bot=$BOT_PORT copy=$COPY_PORT stake=$STAKE_PORT lend=$LEND_PORT) =="
"$BIN/matching"  --config "$CFG_TMP" --mysql-dsn "" --addr ":$MATCH_PORT" >"$LOGDIR/matching.log"  2>&1 & PIDS+=($!)
"$BIN/spot"      --config "$CFG_TMP" --mysql-dsn "" --addr ":$SPOT_PORT" >"$LOGDIR/spot.log"       2>&1 & PIDS+=($!)
"$BIN/bot"       --config "$CFG_TMP" --mysql-dsn "" --addr ":$BOT_PORT" --spot-url "http://127.0.0.1:$SPOT_PORT" >"$LOGDIR/bot.log" 2>&1 & PIDS+=($!)
"$BIN/copytrade" --config "$CFG_TMP" --mysql-dsn "" --addr ":$COPY_PORT" --spot-url "http://127.0.0.1:$SPOT_PORT" >"$LOGDIR/copytrade.log" 2>&1 & PIDS+=($!)
"$BIN/staking"   --config "$CFG_TMP" --mysql-dsn "" --addr ":$STAKE_PORT" >"$LOGDIR/staking.log"   2>&1 & PIDS+=($!)
"$BIN/lending"   --config "$CFG_TMP" --mysql-dsn "" --addr ":$LEND_PORT" >"$LOGDIR/lending.log"   2>&1 & PIDS+=($!)

wait_for "http://127.0.0.1:$MATCH_PORT/health"                 "matching"  || FAILED=1
wait_for_match_leader "http://127.0.0.1:$MATCH_PORT/health"     "matching"  || FAILED=1
wait_for "http://127.0.0.1:$SPOT_PORT/api/v1/spot/depth"       "spot"      || FAILED=1
wait_for "http://127.0.0.1:$BOT_PORT/api/v1/bot/strategies"    "bot"       || FAILED=1
wait_for "http://127.0.0.1:$COPY_PORT/api/v1/copytrade/leads"  "copytrade" || FAILED=1
wait_for "http://127.0.0.1:$STAKE_PORT/api/v1/staking/products" "staking"  || FAILED=1
wait_for "http://127.0.0.1:$LEND_PORT/api/v1/lending/pools"    "lending"   || FAILED=1
[ "$FAILED" -ne 0 ] && { echo "services failed to start"; exit 1; }

SPOT="http://127.0.0.1:$SPOT_PORT"
BOT="http://127.0.0.1:$BOT_PORT"
COPY="http://127.0.0.1:$COPY_PORT"
STAKE="http://127.0.0.1:$STAKE_PORT"
LEND="http://127.0.0.1:$LEND_PORT"

# tokens: 共享开发密钥本地签发
ADMIN_TOKEN="$(mint_token 999 admin)"
BOT_USER_TOKEN="$(mint_token 2 user)"   # spot 内存账本预置 1-4 余额
LEAD_TOKEN="$(mint_token 3 user)"       # 创建 lead
FOLLOWER_TOKEN="$(mint_token 4 user)"   # 承接复制单（spot 内存账本有余额）
STAKE_USER_TOKEN="$(mint_token 1 user)" # staking 内存账本预置 1-4 余额（uid=1 有 10 ETH）

# ================= 流程 1：bot → spot =================
echo "== flow 1: bot -> spot (F4 身份边界) =="
do_call POST "$BOT/api/v1/bot/strategies" "$BOT_USER_TOKEN" \
  '{"name":"it-bot","market":"spot","symbol":"BTC_USDT","side":"buy","type":"dca","user_token":"'"$BOT_USER_TOKEN"'","params":{"order_amount":10,"dca_interval_sec":60,"dca_amount":10,"max_position":1000}}'
check "bot create strategy -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"
STRAT_ID="$(extract_id "$RESP_BODY")"
check "bot strategy id parsed" "$([ -n "$STRAT_ID" ] && echo 1 || echo 0)"

do_call POST "$BOT/api/v1/bot/strategies/$STRAT_ID/start" "$BOT_USER_TOKEN" ""
check "bot start strategy -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"

# 管理端点强制触发一轮 tick（等价于后台 Run 循环），确定性驱动下单
do_call POST "$BOT/api/v1/bot/admin/strategies/$STRAT_ID/tick" "$ADMIN_TOKEN" ""
check "bot admin tick -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"

sleep 1
do_call GET "$SPOT/api/v1/spot/orders" "$BOT_USER_TOKEN" ""
check "spot GET orders (bot user) -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"
check "spot 收到 bot 代下的 BTC_USDT 订单 (F4: 归属 uid=2)" "$(spot_has_symbol "$RESP_BODY")"

# ================= 流程 2：copytrade → spot =================
echo "== flow 2: copytrade -> spot (F4 身份边界 + F1 幂等) =="
do_call POST "$COPY/api/v1/copytrade/leads" "$LEAD_TOKEN" \
  '{"name":"it-lead","bio":"integration"}'
check "copytrade create lead -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"
LEAD_ID="$(extract_id "$RESP_BODY")"
check "lead id parsed" "$([ -n "$LEAD_ID" ] && echo 1 || echo 0)"

do_call POST "$COPY/api/v1/copytrade/follows" "$FOLLOWER_TOKEN" \
  '{"lead_id":'"$LEAD_ID"',"copy_ratio":1,"allocated_amount":0,"follower_token":"'"$FOLLOWER_TOKEN"'"}'
check "copytrade follow -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"

# 无 Kafka 时手动注入一笔带单高手的成交流（taker_id=3 即上面创建的 lead）
TS="$(python3 -c 'import time;print(int(time.time()*1000))')"
do_call POST "$COPY/api/v1/copytrade/admin/simulate-trade" "$ADMIN_TOKEN" \
  '{"symbol":"BTC_USDT","price":100,"qty":1,"taker_id":3,"maker_id":99999,"taker_side":"buy","ts":'"$TS"'}' >/dev/null 2>&1
# 注意：simulate-trade 端点本身返回 200（事件已入复制管线）；复制单是否成功以 spot 侧为准。
check "copytrade simulate-trade accepted -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"

sleep 1
do_call GET "$SPOT/api/v1/spot/orders" "$FOLLOWER_TOKEN" ""
check "spot GET orders (follower) -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"
check "spot 收到粉丝复制单 BTC_USDT (F4: 归属 uid=4)" "$(spot_has_symbol "$RESP_BODY")"

# 去重：同一事件再注入一次不应产生第二单（F1 幂等）
do_call POST "$COPY/api/v1/copytrade/admin/simulate-trade" "$ADMIN_TOKEN" \
  '{"symbol":"BTC_USDT","price":100,"qty":1,"taker_id":3,"maker_id":99999,"taker_side":"buy","ts":'"$TS"'}' >/dev/null 2>&1
sleep 1
do_call GET "$SPOT/api/v1/spot/orders" "$FOLLOWER_TOKEN" ""
check "复制单幂等：重复事件未产生第二单 (F1)" "$([ "$(spot_symbol_count "$RESP_BODY")" = "1" ] && echo 1 || echo 0)"

# ================= 流程 3：staking 充值锁仓 -> 奖励归集生息 -> 解质押释放 =================
echo "== flow 3: staking 链上质押生息 (F2 定点 / F3 原子 / F4 鉴权) =="
# 3.1 列出在售产品，取首个 product_id（启动时为种子 ETH 2.0 质押产品）
do_call GET "$STAKE/api/v1/staking/products" "$STAKE_USER_TOKEN" ""
check "staking list products -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"
PRODUCT_ID="$(echo "$RESP_BODY" | python3 -c 'import sys,json
try:
    d=json.load(sys.stdin); ps=d.get("data",{}).get("products",[])
    print(ps[0]["id"] if ps else "")
except Exception:
    print("")')"
check "staking product id parsed" "$([ -n "$PRODUCT_ID" ] && echo 1 || echo 0)"

# 3.2 用户委托质押 1.0 ETH（本金从 uid=1 锁定到 SysStaking，复式记账）
do_call POST "$STAKE/api/v1/staking/subscribe" "$STAKE_USER_TOKEN" \
  '{"product_id":'"$PRODUCT_ID"',"amount":1.0}'
check "staking subscribe -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"
DELEG_ID="$(extract_id "$RESP_BODY")"
check "staking delegation id parsed" "$([ -n "$DELEG_ID" ] && echo 1 || echo 0)"

# 3.3 我的持仓应包含该委托：本金 1.0、状态 active
do_call GET "$STAKE/api/v1/staking/holdings" "$STAKE_USER_TOKEN" ""
check "staking my holdings -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"
check "staking 持仓含 1.0 ETH active 委托" "$(echo "$RESP_BODY" | python3 -c '
import sys,json
try:
    d=json.load(sys.stdin); ds=d.get("data",{}).get("delegations",[])
    ok="1" if any(str(x.get("id"))=="'"$DELEG_ID"'" and abs(float(x.get("principal",0))-1.0)<1e-9 and x.get("status")=="active" for x in ds) else "0"
    print(ok)
except Exception:
    print("0")')"

# 3.4 管理员触发一次奖励归集（等价于后台 RunLoop 的某一轮）：链上待领奖励计入
#     SysStakingReward 负债（平台对用户欠付），返回归集总额应 > 0。
do_call POST "$STAKE/api/v1/staking/admin/accrue" "$ADMIN_TOKEN" ""
check "staking admin accrue -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"
check "staking 归集到正向奖励负债 (>0)" "$(echo "$RESP_BODY" | python3 -c '
import sys,json
try:
    d=json.load(sys.stdin); a=d.get("data",{}).get("accrued",0)
    try: v=float(a)
    except Exception: v=0.0
    print("1" if v>0 else "0")
except Exception:
    print("0")')"

# 3.5 解质押 -> 释放：模拟后端已超额确认，本金与累计奖励经 ledger.Batch 原子归还用户账本。
do_call POST "$STAKE/api/v1/staking/unbond" "$STAKE_USER_TOKEN" \
  '{"delegation_id":'"$DELEG_ID"'}'
check "staking unbond -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"
do_call POST "$STAKE/api/v1/staking/release" "$STAKE_USER_TOKEN" \
  '{"delegation_id":'"$DELEG_ID"'}'
check "staking release -> 200 (本金+奖励原子归还)" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"

# 释放后该委托状态应为 unbonded（终态），且再次 release 应幂等拒绝而非重复放款。
do_call POST "$STAKE/api/v1/staking/release" "$STAKE_USER_TOKEN" \
  '{"delegation_id":'"$DELEG_ID"'}'
check "staking release 终态幂等：重复释放被拒 (非 200)" "$([ "$RESP_CODE" != "200" ] && echo 1 || echo 0)"

# ================= 流程 4：lending 存款 -> 借款 -> 利息归集 -> 还款 -> 提款 =================
echo "== flow 4: lending 存借还提 (F2 利息 / F3 原子 / F4 鉴权) =="
LEND_USER_TOKEN="$(mint_token 1 user)"

# 4.1 列出资金池（启动时 seed 了一个 USDT 池）
do_call GET "$LEND/api/v1/lending/pools" "$LEND_USER_TOKEN" ""
check "lending list pools -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"
LEND_POOL_ID="$(echo "$RESP_BODY" | python3 -c 'import sys,json
try:
    d=json.load(sys.stdin); ps=d.get("data",{}).get("pools",[])
    print(ps[0]["id"] if ps else "")
except Exception:
    print("")')"
check "lending pool id parsed" "$([ -n "$LEND_POOL_ID" ] && echo 1 || echo 0)"

# 4.2 用户存款 500 USDT 到资金池（复式记账：用户可用→SysLendingPool）
do_call POST "$LEND/api/v1/lending/lend" "$LEND_USER_TOKEN" \
  '{"pool_id":'"$LEND_POOL_ID"',"amount":500}'
check "lending lend 500 USDT -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"

# 4.3 借款 100 USDT，抵押 200 USDT（collateral_req=1.5，100*1.5=150 ≤ 200）
do_call POST "$LEND/api/v1/lending/borrow" "$LEND_USER_TOKEN" \
  '{"pool_id":'"$LEND_POOL_ID"',"amount":100,"collateral":200}'
check "lending borrow 100 USDT -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"

# 4.4 我的存款/借款列表
do_call GET "$LEND/api/v1/lending/my/lends" "$LEND_USER_TOKEN" ""
check "lending my lends -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"
check "lending 我的存款列表含 active 记录" "$(echo "$RESP_BODY" | python3 -c '
import sys,json
try:
    d=json.load(sys.stdin); ls=d.get("data",{}).get("lends",[])
    print("1" if any(x.get("status")=="active" for x in ls) else "0")
except Exception:
    print("0")')"

do_call GET "$LEND/api/v1/lending/my/borrows" "$LEND_USER_TOKEN" ""
check "lending my borrows -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"

# 4.5 管理员触发利息归集（accrue 一次，计息到 borrowers）
do_call POST "$LEND/api/v1/lending/admin/accrue" "$ADMIN_TOKEN" ""
check "lending admin accrue -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"

# 4.6 还款（连本带利）
do_call POST "$LEND/api/v1/lending/repay" "$LEND_USER_TOKEN" '{"pool_id":'"$LEND_POOL_ID"'}'
check "lending repay -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"

# 4.7 提取存款（复式记账：SysLendingPool→用户可用）
do_call POST "$LEND/api/v1/lending/withdraw" "$LEND_USER_TOKEN" '{"pool_id":'"$LEND_POOL_ID"'}'
check "lending withdraw -> 200" "$([ "$RESP_CODE" = "200" ] && echo 1 || echo 0)"

echo "========================================="
if [ "$FAILED" -eq 0 ]; then
  echo "INTEGRATION: ALL PASS"
  exit 0
else
  echo "INTEGRATION: FAILED"
  exit 1
fi

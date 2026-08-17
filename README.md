# crypto-exchange

虚拟货币交易系统（Go 单体仓库骨架），覆盖 OTC / 现货 / 合约 / 期权 / 杠杆 / 理财。

## 架构

- **接入层**：API 网关（反向代理 + 鉴权 + 限流）
- **业务服务层**：user / spot / market / ...（各业务线独立服务）
- **核心引擎**：internal/matching 撮合引擎（内存订单簿 + 事件驱动）
- **数据层**：MySQL / Redis / Kafka / InfluxDB / Elasticsearch（见 docker-compose）

## 目录结构

```
cmd/                各服务入口（user, gateway, spot, market ...）
configs/            配置文件
api/                proto 定义
internal/
  pkg/              公共库：config / logger / response / middleware
  matching/         撮合引擎（订单簿 + 引擎）
  futures/          合约交易：持仓/保证金模型 + 强平引擎 + 标记价格 + 资金费率
  ledger/           钱包总账（复式记账：可用/冻结、保证金冻结、资金费结算、强平没收）
  services/         各业务服务实现
```

> **前端为独立仓库**（与本项目平级，不在此目录内）：
> - `../ce-frontend/`  —— 用户交易终端（React + TypeScript + Vite）
> - `../web-admin/`    —— 管理后台前端（React + TypeScript + Vite）

## 前端

```bash
cd ../ce-frontend      # 同为独立仓库，位于本目录平级
npm install
npm run dev      # 开发服务器 http://localhost:5173，/api 代理到网关 :8080（含 WebSocket）
npm run build    # 产物输出到 ../ce-frontend/dist
```

管理后台前端：

```bash
cd ../web-admin        # 同为独立仓库，位于本目录平级
npm install
npm run dev      # 开发服务器 http://localhost:5174，/api/admin 代理到 admin 服务 :8095
npm run build    # 产物输出到 ../web-admin/dist
```

前端通过网关调用现货下单 / 深度接口，订单簿通过 **WebSocket**（`/api/v1/spot/ws`）实时接收深度与成交推送，开发期由 Vite 代理解决跨域与 WS 升级。

## 快速开始

```bash
# 1. 启动依赖（MySQL/Redis/Kafka/InfluxDB/ES）
docker-compose up -d

# 2. 拉取依赖
go mod download

# 3. 构建全部服务
make build

# 4. 运行某个服务
make run-user
# 或
make run-gateway
```

## 部署与配置

所有服务共用 `configs/config.yaml` 的 `server` 段。下面是与线上归因/限流直接相关、部署时**必须确认**的一项。

### ⚠️ 受信任代理 `server.trusted_proxies`（上线前必查）

生产网络拓扑通常为：`客户端 → 网关/LB/CDN → 各业务服务`。该配置控制 gin 如何解析客户端真实 IP（`c.ClientIP()`）：

- **留空（默认）**：不信任任何代理，直接使用直连对端 IP（`RemoteAddr`）。仅适用于服务**直连公网**、前面没有反向代理的场景。
- **填写网关/LB/CDN 的地址（IP 或 CIDR）**：服务位于反向代理之后时**必须填写**，否则 `c.ClientIP()` 会把上游（网关）IP 当成客户端，引发：
  - 全局限流（`middleware.Common` 的每 IP 限流）将所有请求误判为同一来源，**可能整体被限流甚至误伤全体用户**；
  - 审计日志 / 安全日志中的客户端 IP 全部失真，事后无法溯源；
  - admin 登录的「基于 IP 限流」失效——限的是网关 IP 而非真实来源。

> 网关服务（`cmd/gateway`）用 `httputil.ReverseProxy` 反代，标准库会自动注入 `X-Forwarded-For`；后端把网关地址列入 `trusted_proxies` 后，即可从 `X-Forwarded-For` 读到真实客户端 IP，整条归因链路自洽。
> 若网关自身也位于 CDN/LB 之后，网关自身的 `trusted_proxies` 同样需填入其上游地址。

配置示例：

```yaml
server:
  trusted_proxies: []                       # 直连公网：保持留空
  # 位于网关/LB/CDN 之后：填入其地址，例如
  # trusted_proxies: ["10.0.0.0/8", "192.168.1.10"]
```

### 其它 `server` 段配置

- `rate_limit_per_sec`：单实例每 IP 请求速率上限。
- `allowed_origins`：CORS 跨域白名单；为空则拒绝一切跨域。
- `max_body_bytes`：请求体大小上限（默认 1 MiB）。
- `tls`：`cert_file` 与 `key_file` 同时配置才启用 HTTPS。
- `mode`：`debug | release`。

## 约定

- 全院子使用 Go，统一 gofmt / go vet。
- 内部 RPC 用 gRPC + Protobuf（见 api/），对外用 REST + WebSocket。
- 所有资金变动走 Ledger 流水，强幂等。
- 撮合引擎单交易对单 goroutine，避免锁竞争。
- **数据库表命名必须以 `ce_` 开头**（详见 [docs/CONVENTIONS.md](docs/CONVENTIONS.md)）。

## 开发任务清单

完整的待开发任务与优先级见 [docs/DEVELOPMENT_TASKS.md](docs/DEVELOPMENT_TASKS.md)（含 P0 安全/资金闭环、P1 合约完善、P2 业务线等 17 项）。

## API 文档

各业务线对外接口（OTC 场外交易、用户个人设置、公告模块等）统一索引见 [docs/API.md](docs/API.md)；公告模块的运维操作示例（curl）见 [docs/announcement_guide.md](docs/announcement_guide.md)，单元测试说明见 [docs/announcement_test.md](docs/announcement_test.md)。

## 合约交易（futures）

强平骨架位于 `internal/futures`，服务入口 `cmd/futures`（监听 `:8083`）。

核心概念：
- **逐仓（isolated）持仓**：`Position` 含方向/张数/开仓均价/锁定保证金/杠杆。
- **标记价格**：由成交流驱动更新（生产用指数价 + EMA，避免插针单瞬时常平）。
- **强平价**：由「权益 = 维持保证金」反解，多/空公式不同（见 `position.go` 的 `LiqPrice`）。
- **强平触发**：`Liquidator.UpdateMarkPrice` 扫描所有持仓，权益 ≤ 维持保证金即触发，清仓并产出 `LiquidationEvent`（含手续费、强平价），可接保险基金 / ADL。

运行与验证：
```bash
go test ./internal/futures/        # 强平单测（多/空/多持仓选择性强平）
make run-futures                   # 起合约服务
# 开 10x 多仓 -> 标记价跌穿强平价 -> GET /api/v1/futures/liquidations 可见记录
```

REST 接口：
- `POST /api/v1/futures/order`        开/平仓（action=open/close，pos_side=long/short）
- `GET  /api/v1/futures/positions`     持仓快照 + 标记价
- `GET  /api/v1/futures/liquidations`  最近强平记录
- `GET  /api/v1/futures/funding`       当前资金费率（指数价/溢价EMA/费率/周期）
- `GET  /api/v1/futures/funding-history` 最近资金结算历史
- `GET  /api/v1/futures/wallet`        钱包余额（可用/冻结/保险基金/资金池）
- `POST /api/v1/futures/wallet/deposit` 演示充值（生产来自链上充值/清结算）
- `GET  /api/v1/futures/ws`            行情/强平/资金 WebSocket 推送

### 标记价格（Mark Price）

强平与资金费率共用同一**独立标记价格**，避免裸用成交流（插针单）误触强平。实现见 `internal/futures/markprice.go`：

- **标记价格** = 指数价 + EMA(合约成交流 − 指数价) = 指数价 × (1 + 溢价指数EMA)。
- 指数价由预言机/多交易所现货加权喂入（演示固定）；溢价 EMA 平滑系数 1/8。
- **抗插针**：单笔极端成交价对标记价影响被 EMA 大幅稀释。验证：稳定价 51000 后单笔砸到 40000 的成交，标记价仅由 51000 微跌至 49625（约 −2.7%），而非跌到 40000。

### 资金费率（Funding Rate）

永续合约无到期日，资金费率把合约价格锚定到指数价。实现见 `internal/futures/funding.go`：

- **溢价指数 EMA** 由 MarkPriceCalculator 统一维护（见上）。
- **资金费率** = 名义利率(0.01%/周期) + 限幅(±0.05%)后的溢价成分。
- **结算与资金闭环**：每周期（演示 30s，生产 8h）对所有持仓结算——费率>0 多头付空头，<0 空头付多头；支付额 = 名义价值 × 费率。结算时通过 `internal/ledger` 钱包总账，以**资金费中转池**（`SysFundingPool`）在多头与空头之间转账，借贷恒等、净额恒为 0。

### 钱包 Ledger（资金闭环）

实现见 `internal/ledger/ledger.go`，复式记账核心：

- 每个用户每资产一个 `Account`（可用 Available / 冻结 Frozen）。
- 所有资金变动落 `Entry` 流水（带符号 Delta），全局借贷恒等，便于审计对账。
- **开仓**：`Freeze` 从可用冻结保证金，余额不足拒绝开仓。
- **资金费**：`Transfer` 经中转池在多空间转账。
- **强平**：解冻持仓保证金并 `Debit` 到保险基金 `SysInsurance`（穿仓吸收账户）。
- 系统账户（中转池 `0`、保险基金 `-1`）也纳入复式记账。

运行验证：
```bash
make run-futures
# 开多@51000(>指数50000)+开空@51000 margin各5100 -> 可用94900/冻结5100
# 等待结算周期 -> 用户1 付 30.6 (可用 94869.4) / 用户2 收 30.6 (94930.6) / 资金池=0
# 10x多@50000 标记价跌至40000 -> 强平 -> 用户1 保证金没收, 保险基金 +5000
```

已知留白：指数价应来自多交易所现货加权/预言机；全仓（cross）模式、部分强平（阶梯减仓）、ADL 落地；穿仓时中转池/保险基金差额吸收。

## 待补充

- 数据库 migration、Kafka topic 设计、proto 生成脚本。
- 其余业务线服务（otc / options / margin / wealth / risk / settlement / notification）。
- futures：全仓（cross）模式、部分强平（阶梯减仓）、ADL 落地、穿仓处理。

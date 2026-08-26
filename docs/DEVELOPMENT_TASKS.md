# 开发任务清单（Development Tasks / Backlog）

> 汇总 crypto-exchange 项目当前待开发的任务。来源：README「已知留白 / 待补充」、
> 代码内 `TODO` 注释、`cryptoExchange_architecture.md` 演进路线与留白。
> 强制约定见 [CONVENTIONS.md](CONVENTIONS.md)：**所有新建数据库表名必须以 `ce_` 开头**。

## 0. 已完成（本期背景，便于理解待办范围）

- 撮合引擎 `internal/matching`（内存订单簿 + 事件驱动）。
- 合约交易 `internal/futures` + `cmd/futures`（持仓/保证金、标记价格、资金费率、强平、ADL/社会化分摊算法）。
- 钱包总账 `internal/ledger`（复式记账、可用/冻结、资金费闭环、保险基金、风控引擎、提现冷静期、地址白名单）。
- 账本快照持久化：文件 → **远程 MySQL**，表 `ce_ledger_snapshots`（cmd/futures 已接入，信号落库 + 重启恢复）。
- 基础服务骨架：`user` / `gateway` / `spot` / `market` 入口与 `internal/pkg`、`oracle`、`services`、`ws` 模块。

---

## 1. P0 — 生产可用与安全硬阻塞

| 编号 | 任务 | 描述 | 来源 | 当前状态 |
|------|------|------|------|----------|
| T-01 | 鉴权中间件落地 | 网关/服务校验 token 并写入上下文用户身份，按身份做权限与额度控制 | `internal/pkg/middleware/auth.go:30` `// TODO` | **已完成**（HMAC-SHA256 Bearer 校验 + 单测；网关接入） |
| T-02 | 成交流发布 Kafka 触发清算 | 撮合成交后发布事件到 Kafka，驱动清算服务记账，闭合资金链路 | `internal/matching/engine.go:59` `// TODO` | **已完成**（新增 `internal/pkg/mq`：Publisher 接口 + InMem 降级 + Kafka 实现(build tag)；futures onTrade 已发布） |
| T-03 | 真实链上 / 预言机 RPC 接入 | 接入真实节点与预言机（充值/提现链上回调、指数价喂价）；依赖外部节点/密钥，受合规约束 | architecture.md §16/§21 留白 | **预言机实时喂价已可配置接入**（新增 `internal/oracle.NewFromConfig` + `configs/config.yaml` 的 `oracle` 段，支持 Binance/OKX/Coinbase 真实 REST 源并经单测；cmd/{margin,otc,options,futures} 已接线，无配置时回退内置演示）；**链上 RPC 充提双向 + 真实区块确认轮询 + TRC20 充值过滤 + TRC20 确认数查询 + 热钱包离线签名边界 + 真实 HSM 签名器（ETH）完成可插拔脚手架/软件实现**（§27：提现广播 `RPCWithdrawGateway`(+`Signer`/`SendRaw` 离线签名边界 + `realSigner` 真实 secp256k1 签名) + 充值回调 `RPCDepositGateway`/`JSONRPCDepositScanner` + `ConfirmSource`/`JSONRPCClient.Confirmations`，共用 `ChainRPCConfig`+`JSONRPCClient`，配置驱动真实广播/监听/确认/离线签名并取回真实 TxHash、未配置/节点宕机自动回退模拟，fail-degraded）；**真实 HSM/KMS 安全模块接入（离线签名边界 `KeySigner` 缝已落地，生产注册外部后端即生效）、TRON 带签名合约调用（TransferContract + TriggerSmartContract，§27 续十）已落地**（依赖外部节点+合规仅余生产部署动作，见 §27「剩余」；BTC/ETH 真实签名 + BTC UTXO 拉取主路径 + ETH 真实 Nonce/Gas 管理 + HSM/KMS KeySigner 接入缝 + TRON 真实离线签名已于 §27 续七/续八/续九/续十落地） |
| T-04 | 数据库 migration 与版本管理 | 当前建表靠首次运行 `CREATE TABLE IF NOT EXISTS`，缺可回滚 migration。新建表须 `ce_` 前缀 | README「待补充」 | **已完成**（新增 `internal/pkg/migrate` 运行器 + `ce_schema_migrations` 版本表；ledger 表改由迁移创建；含集成测试） |

## 2. P1 — 合约交易完善

| 编号 | 任务 | 描述 | 来源 | 当前状态 |
|------|------|------|------|----------|
| T-05 | 指数价多交易所加权 / 预言机 | 标记价格与资金费率的指数价从多交易所现货加权或预言机喂入（当前演示固定值） | README「已知留白」 | **已完成**（`internal/oracle` 多源中位数聚合+离群剔除已具备并经单测；真实交易所 REST 源需外部配置） |
| T-06 | 全仓（cross）模式 | 账户级共享保证金，跨持仓共用保证金与强平逻辑 | README「已知留白」「待补充」 | **已完成**（引擎 `OpenCross`/`crossBook` + 服务层开仓 `margin_mode=cross`，`TestCross*` 单测通过） |
| T-07 | 部分强平 / 阶梯减仓 | 按仓位风险阶梯部分减仓而非整腿强平 | README「待补充」 | **已完成**（`SetPartialRatio` + `TestPartialLiquidation` 通过） |
| T-08 | ADL 服务层接入与端到端验证 | 引擎层已有 ADL / 社会化分摊算法（`engine.go runSocializedLoss`），需接入 futures 服务并演练穿仓吸收 | README「待补充」 | **已完成**（服务层 `SetADLCallback` 已接入并写账本；`TestADLOnBankruptcy` 通过） |
| T-09 | 穿仓差额吸收 | 穿仓时由中转池/保险基金差额吸收，补齐剩余缺口的兜底流程 | README「已知留白」 | **已完成**（`SetDeficitPayer`+`SetSocializeCallback` 穿仓瀑布已接入；`TestDeficitWaterfall*``TestSocializedLoss` 通过） |
| T-10 | futures 加入 Makefile 构建 | `cmd/futures` 已存在，但 `Makefile` 的 `SERVICES` 仅含 `user gateway spot market`，缺 futures 构建/运行目标 | Makefile | **已完成**（SERVICES 加入 futures） |

## 3. P2 — 行情、基础设施与其余业务线

| 编号 | 任务 | 描述 | 来源 | 当前状态 |
|------|------|------|------|----------|
| T-11 | 行情 WebSocket 生产化 | 生产环境用 `gorilla/websocket` 升级连接、订阅行情频道、定时推送 ticker | `cmd/market/main.go:30` `// TODO` | **已完成**（复用 `internal/ws` Hub；`/ws?symbol=` 升级推送 ticker，行情由成交或演示随机游走驱动；`market_test.go` 通过） |
| T-12 | Kafka topic 设计 | 定义订单/成交/行情/链上事件等 topic 与 schema | README「待补充」 | **已完成**（新增 `docs/KAFKA_TOPICS.md`：topic 清单/schema/分区/交付语义） |
| T-13 | proto 生成脚本 | gRPC/protobuf 代码生成脚本与 CI 集成 | README「待补充」 | **已完成**（新增 `api/wallet.proto` + `scripts/gen_proto.sh` + Makefile `proto` 目标） |
| T-14 | 其余业务线服务 | 实现 otc / options / margin / wealth / risk / settlement / notification 业务线 | README「待补充」 | **已完成**：settlement（T-15）、margin 现货杠杆（§7）、notification 站内信（§8）、risk 风控（§9）、options 期权（§10）、otc 场外交易（§11）、wealth 理财资管（§12）均已实现并验证（`go build/vet/test ./...` 全绿）；**T-14 全部 7 子业务线均已跑远程冒烟**（settlement 为纯费用/健康检查 HTTP 服务、无 MySQL 持久化，仅跑 HTTP 冒烟；其余 6 项均跑远程 MySQL 冒烟） |
| T-15 | settlement 服务化 | `internal/settlement` 模块已存在，需封装为独立服务并接入账本 | 目录现状 | **已完成**（新增 `cmd/settlement` 独立服务：手续费估算/健康端点，`make build` 含 settlement，运行时验证通过） |
| T-16 | 依赖组件实际接入 | `docker-compose` 声明了 MySQL/Redis/Kafka/InfluxDB/ES，但当前仅 MySQL 真正使用，其余未接入 | docker-compose.yml | **已落地（Kafka+Redis+InfluxDB+ES，见 §21/§22/§23/§24）**：Kafka 交易清算（§21）+ **Redis 集群级限流（§22）** + **InfluxDB 行情 K 线持久化（§23）** + **ES 成交检索（§24）** 均已接入；docker-compose 声明的全部中间件现已实际使用 |
| T-17 | 对账 / 审计报表 | 基于 `Ledger` 流水做借贷恒等校验、对账与审计看板 | 资金安全闭环延伸 | **已完成**（`Ledger.Reconcile/IsBalanced/RunReconcileOnce` + 定时巡检 + `/wallet/reconcile` 端点 + Prometheus 指标已具备） |
| T-18 | 安全加固（暂不引入中间件） | 统一安全中间件套件：审计日志、安全响应头、受控 CORS、请求体限制、边缘/入口鉴权与公开端点豁免、TLS 可配置；修复 spot/futures 无鉴权下单漏洞与 market/futures 端口冲突 | 安全审计发现 | **已完成**（见 §13） |
| T-19 | 链上质押理财（staking） | 新增业务线：产品上架、用户质押/解押、链上广播、奖励归集、本金+奖励释放；复式记账到 `SysStaking`/`SysStakingReward` | 业务规划（2026-08-17 立项） | **已完成**（见 §28） |
| T-20 | 交易机器人（bot） | 新增业务线：网格/定投/均线策略，代用户下单（复用 spot/futures 资金安全 F1/F4），tick 触发 + 越仓控制 | 业务规划（2026-08-17 立项） | **已完成**（见 §29） |
| T-21 | 跟单（copytrade） | 新增业务线：带单高手注册、粉丝跟单、消费成交事件按比例复制下单、平台复制费结算到 `SysCopyTradeFee` | 业务规划（2026-08-17 立项） | **已完成**（见 §30） |


---

## 4. 优先级建议（可执行顺序）

1. **先补安全与资金闭环**：T-01（鉴权）、T-02（Kafka 清算）、T-04（migration）。
2. **再做合约完善**：T-05→T-06→T-07→T-09 顺序推进，T-08 在 T-03/T-09 后做端到端演练；T-10 顺手修。
3. **最后铺基础设施与业务线**：T-11~T-17。

> 所有涉及新建数据表的任务（T-04、T-12、T-14、T-15、T-17 等）一律遵守 `ce_` 前缀约定。

## 5. 完成状态汇总（截至 2026-08-18）

- **已完成（本轮新增实现）**：T-01、T-02、T-04、T-10、T-11、T-12、T-13、T-15、T-14（margin / notification / risk / options / otc / wealth 业务线）；账户体系（user 服务）；**T-19 链上质押理财、T-20 交易机器人、T-21 跟单** 三条新业务线（2026-08-17 立项，2026-08-18 完成首版，见 §28/§29/§30）。
- **已完成（代码原已具备，本轮验证）**：T-05、T-06、T-07、T-08、T-09、T-17。
- **阻塞 / 待立项**：T-03（真实链上/预言机 RPC，依赖外部节点+合规）、T-16（Redis/Kafka/InfluxDB/ES 实际接入，随业务推进）。T-14 全部子业务线（settlement / margin / notification / risk / options / otc / wealth）均已实现并验证；其中除 settlement（纯 HTTP 费用/健康检查、无持久化）外均已跑远程 MySQL 冒烟（`ce_` 表经迁移建表并持久化）。
- **新增 `cmd` 入口与网关反代**：`cmd/staking`(:8097)、`cmd/bot`(:8098)、`cmd/copytrade`(:8099) 均已在 `configs/config.yaml` 的 `services` 段注册，经网关 `/api/v1/<svc>/*` 统一反代（staking 此前遗漏注册，本次一并补上）。

- **已完成（本轮新增实现）**：T-01、T-02、T-04、T-10、T-11、T-12、T-13、T-15、T-14（margin / notification / risk / options / otc / wealth 业务线）；账户体系（user 服务）由内存骨架升级为生产级（bcrypt 密码、HMAC-SHA256 鉴权打通 T-01、MySQL 持久化、2FA、KYC 状态机）。
- **已完成（代码原已具备，本轮验证）**：T-05、T-06、T-07、T-08、T-09、T-17。
  - 说明：README「已知留白 / 待补充」中多处标注为"未开始"的合约能力（全仓、部分强平、ADL、
    穿仓社会化分摊、预言机多源聚合、对账巡检）实际上已在 `internal/futures` / `internal/ledger` /
    `internal/oracle` 中实现并经单测覆盖；本轮通过 `go test ./internal/futures/` 等确认其有效，
    未做重复实现以免回归。
- **阻塞 / 待立项**：T-03（真实链上/预言机 RPC，依赖外部节点+合规）、T-16（Redis/Kafka/InfluxDB/ES 实际接入，随业务推进）。T-14 全部子业务线（settlement / margin / notification / risk / options / otc / wealth）均已实现并验证；其中除 settlement（纯 HTTP 费用/健康检查、无持久化）外均已跑远程 MySQL 冒烟（`ce_` 表经迁移建表并持久化）。

### 本轮新增/改动文件
- `internal/pkg/middleware/auth.go`：HMAC-SHA256 Bearer 鉴权 + 单测（T-01）
- `internal/pkg/mq/`：成交流消息发布抽象（Publisher / InMem / Kafka(build tag)）+ 单测（T-02）
- `internal/pkg/migrate/`：可回滚迁移运行器 + `ce_schema_migrations` + 单测（T-04）
- `internal/ledger/migrations.go` + `mysql_store.go`：账本表改由迁移创建（T-04）
- `cmd/market/main.go` + `market_test.go`：行情 WebSocket 生产化（T-11）
- `docs/KAFKA_TOPICS.md`：Kafka topic 设计（T-12）
- `api/wallet.proto` + `scripts/gen_proto.sh` + Makefile `proto`：Protobuf 生成链路（T-13）
- `cmd/settlement/main.go`：独立清算/手续费服务（T-15）
- `Makefile`：SERVICES 加入 futures、settlement（T-10、T-15）
- `configs/config.yaml`：新增 `auth.secret`（T-01）


## 6. 架构整改：cmd 模块服务分层（2026-08-12，已完成）

用户指出 `cmd/` 下各模块把代码全写进单个 `main.go`、未分层。已按 CONVENTIONS.md §2 四层约定整改：

- **futures（原 1608 行单文件）**：拆分到 `internal/futuresapi`（`server.go` 负责依赖装配——引擎/预言机/链上充提网关/资金费循环/4 类回调/账本风控参数；`handler.go`+`handler_wallet.go` 为 HTTP 路由；`helpers.go` 为 sideName/parseUserID/modeName）。`cmd/futures/main.go` 仅做配置、ledger/publisher 创建、MySQL 持久化生命周期（恢复/种子/信号/保存）、`NewServer`+`RegisterRoutes`+`Run`。
- **market**：`Market` 领域对象与 `Server`（演示喂价 + /ws + /ticker）抽到 `internal/market`；原 `market_test.go` 迁移至 `internal/market/market_test.go`。`cmd/market/main.go` 仅装配。
- **spot**：`depthRow`/`aggregate` 与 `Server`（撮合引擎接线 + /order + /depth + /ws）抽到 `internal/spot`。`cmd/spot/main.go` 仅装配。

整改后仅 `cmd/gateway`(纯装配)、`cmd/settlement`、`cmd/user` 本就合规，所有 `cmd` 模块均为薄装配层。验证：`go build ./...` / `go vet ./...` 全绿；`go test ./...` 全绿（含 internal/market 单测）；futures 服务 HTTP 冒烟（下单/持仓/钱包余额联动/预言机/充值）行为等价正常。

## 7. 业务线落地：margin 现货杠杆（2026-08-12，已完成）

T-14 首个业务线落地。现货杠杆（跨仓保证金）按四层 + ce_ 约定实现：

- `internal/margin`：`model.go`（MarginAccount/状态/领域错误）、`store.go`（Store 接口）、`store_mem.go`（内存，单测用）、`store_mysql.go`+`store_migrations.go`（MySQL，`ce_margin_accounts`，版本 9201 经 migrate）、`service.go`（Borrow 冻结抵押+贷出资产到 ledger / Repay 还本+利息并解冻 / Accrue 按小时利率计利 / LiquidationPrice / Liquidate 收回贷出资产+罚没部分抵押入保险基金 / RunLoop 后台计息+自动强平；priceFn 可注入，默认走 oracle）、`handler.go`（/borrow /repay /liquidate /accrue /account /accounts /liq-price，受 middleware.Auth 保护）。
- `cmd/margin/main.go`（薄装配：读配置、ledger+种子、Store 选 MySQL+迁移否则内存、oracle 价格、装配 Service、注册路由、后台循环、信号退出），监听 :8087。
- `Makefile`：SERVICES 加入 margin。

验证：单测 `internal/margin/service_test.go`（借/还/计息/强平价/强平/抵押不足/超杠杆/无价格跳过）全绿；`go build/vet/test ./...` 全绿；远程 MySQL 冒烟（2026-08-13，:8087）跑通完整闭环——401 鉴权、user1 借 BTC 1@2x（抵押 0.5 USDT 冻结入 ledger、1 BTC 贷出贷记）、account/accounts/liq-price 查询、accrue 小时利率计利（应计≈1.7e-7）、repay 1 BTC（因利息持续累计，仅还本金后账户保留 active，需还 debt+interest 总额方结清——属会计细节非 bug）、liquidate 无 oracle 价安全跳过（liquidated:false）；`ce_margin_accounts` 表已建并持久化（迁移版本 9201，表名遵守 ce_）。

> 说明：强平依赖标记价，默认从 oracle（`asset_USDT`）取；若 oracle 无该交易对则跳过强平（安全）。利息为债务侧负债，由 margin store 记账，与 ledger 借出/收回资产保持复式平衡。

## 8. 业务线落地：notification 站内通知（2026-08-13，已完成）

T-14 第二项业务线。站内信中心，自包含、零外部依赖，可被账户/KYC、risk、ledger 充提等调用写入：

- `internal/notification`：`model.go`（Notification/类型枚举 KYC 审批·风控·充值·提现·系统/状态机 unread→read/领域错误）、`store.go`（Store 接口：Create/List/ListAll/MarkRead/MarkAllRead/CountUnread）、`store_mem.go`（内存，单测用）、`store_mysql.go`+`store_migrations.go`（MySQL，`ce_notifications`，版本 9301 经 migrate）、`service.go`（Publish 写入/List/UnreadCount/MarkRead/MarkAllRead）、`handler.go`（`/api/v1/notification` 下 list/unread-count/read/read-all/publish + admin/list，受 `middleware.Auth` 保护，user_id 取自 token）。
- `cmd/notification/main.go`（薄装配：读配置、Store 选 MySQL+迁移否则内存、verifier=cfg.Auth.Secret、:8088、信号退出）。
- `Makefile`：SERVICES 加入 notification。

验证：单测 `internal/notification/service_test.go`（发布/校验/未知类型降级 system/已读与全读/列表/管理员列表）全绿；`go build/vet/test ./...` 全绿；远程 MySQL 冒烟（2026-08-13，:8088）跑通：401 鉴权、user1 发布→未读计数 1、列表含该条、标记已读→未读归零、read-all、admin 全量列表；`ce_notifications` 表已建并持久化（迁移 9301）。

> 集成点：其他服务（如 user 的 KYC 审批、risk 告警、ledger 充值到账）后续可通过 POST `/publish` 或直写 store 注入站内信，无需改动本服务。

## 9. 业务线落地：risk 风控服务（2026-08-13，已完成）

T-14 第三项业务线。统一风控（限额/规则引擎/黑名单/触发审计），零外部依赖、可被提现/下单前置调用：

- `internal/risk`：`model.go`（RiskRule/BlacklistEntry/RiskEvent/类型枚举 withdraw_limit·order_limit·position_limit·freq_limit/作用域 global·user/状态/CheckResult/错误）、`store.go`（Store 接口：规则/黑名单/事件 CRUD）、`store_mem.go`（内存，单测用）、`store_mysql.go`+`store_migrations.go`（MySQL `ce_risk_rules`(9401)/`ce_risk_blacklist`(9402, target+kind 唯一键)/`ce_risk_events`(9403)，经 migrate）、`service.go`（CheckWithdraw 先查 user+address 黑名单再按 withdraw_limit 规则校验单笔限额与 KYC 等级、CheckOrder 同理、AddRule/AddBlacklist/RemoveBlacklist/IsBlacklisted/ListEvents、拒绝时 RecordEvent）、`handler.go`（`/rules` 增/列、`/blacklist` 增/删/列/查、`/check/withdraw`、`/check/order`、`/events`，受 `middleware.Auth` 保护）。
- `cmd/risk/main.go`（薄装配：MySQL+迁移否则内存、verifier=cfg.Auth.Secret、:8089、信号退出）。
- `Makefile`：SERVICES 加入 risk。

验证：单测 `internal/risk/service_test.go`（限额通过/超限拒绝/低KYC拒绝/用户+地址黑名单拦截/无规则默认放行/下单限额/规则增删/拒绝事件记录）全绿；`go build/vet/test ./...` 全绿；远程 MySQL 冒烟（2026-08-13，:8089）跑通：401 鉴权、加 withdraw_limit 规则(USDT 单笔≤1000)→提现 500 放行/1500 拒绝并落事件、拉黑 user999→提现拒绝、移除黑名单→放行、规则/黑名单/事件列表；三张 `ce_risk_*` 表已建并持久化（迁移 9401/9402/9403）。

> 说明：规则中 `max_amount_per_day` 在本闭门实现语义为**单笔最大额**（按日累计需额外记账，后续可加）；新建规则默认启用（更新时可置 enabled=false）；KYC 等级取自调用方传入（与 user 服务的 KYC 状态机对接）。

## 10. 业务线落地：options 期权（2026-08-13，已完成）

T-14 第四项业务线（用户"继续"在收尾后重新立项）。期权交易核心，采用**中央对手方简化模型**（交易所作为所有持仓对手），自包含、集成 ledger 复式记账与 oracle 价格：

- `internal/options`：
  - `model.go`（OptionContract/OptionPosition/类型枚举 call·put/行权方式 european·american/方向 long·short/状态机 open→exercised·expired/领域错误）、`pricing.go`（Black-Scholes 欧式定价 + Delta + 正态 CDF 近似，纯函数可单测）、`store.go`（Store 接口：合约/持仓 CRUD）、`store_mem.go`（内存，单测用）、`store_mysql.go`+`store_migrations.go`（MySQL `ce_option_contracts`(9501)/`ce_option_positions`(9502)，经 migrate）、`service.go`（CreateContract/Quote/OpenPosition/Exercise/SettlePosition/RunLoop：long 付权利金、short 收权利金+冻结保证金、到期/行权按内在价值与系统对手方 `SysOptions` 结算；`priceFn` 注入 oracle）、`handler.go`（`/api/v1/options` 下 contracts/quote/positions/exercise/settle/admin，受 `middleware.Auth` 保护，user_id 取自 token）、`service_test.go`。
  - `ledger.SysOptions`（-5）新增为期权中央对手方账户。
- `cmd/options/main.go`（薄装配：ledger 种子、Store 选 MySQL+迁移否则内存、oracle 价格、装配 Service、注册路由、后台到期结算循环、信号退出），监听 :8090。
- `Makefile`：SERVICES 加入 options。

验证：单测 `internal/options/service_test.go`（开多扣权利金/开空冻结保证金收权利金/行权收益/到期多头结算/到期空头结算与保证金释放/未到期跳过/BS 定价 sanity）全绿；`go build/vet/test ./...` 全绿；远程 MySQL 冒烟（2026-08-13，:8090）跑通：401 鉴权、建合约(显式权利金，绕过无行情 BS 定价)、开多仓(权利金 100 由种子账本转入 `SysOptions`)、持仓列表、settle 未到期安全跳过、admin 持仓；quote 因 oracle 无行情正确返回 `4002 price feed unavailable`（预期）；`ce_option_contracts`/`ce_option_positions` 表已建并持久化（迁移 9501/9502）。

> 说明：中央对手方模型下，long 付权利金给 `SysOptions`、short 收权利金并冻结保证金；到期/行权时按内在价值结算，多头收益由 `SysOptions` 支出、空头义务在保证金范围内承担（超出部分由已收权利金吸收）。ledger 的 `Transfer` 允许系统账户透支，复式记账自动守恒，无需担心 `SysOptions` 余额。

## 11. 业务线落地：otc 场外交易（2026-08-13，已完成）

T-14 第五项业务线（用户"otc"在 options 收尾后继续立项）。场外/P2P 交易核心，采用**中央托管（escrow）简化模型**（交易所作为 crypto 托管方，法币线下 P2P 不在账本内），自包含、集成 ledger 复式记账：

- `internal/otc`：
  - `model.go`（OtcAdvertisement/广告方向 buy·sell/状态机 open→filled·cancelled；OtcOrder/订单状态机 pending→paid→completed 及 cancelled·disputed/方向继承广告、SellerID/BuyerID 推导/领域错误；OtcCounterparty/每对用户成交次数与评分）、`store.go`（Store 接口：广告/订单/对手方信用 CRUD）、`store_mem.go`（内存，单测用）、`store_mysql.go`+`store_migrations.go`（MySQL `ce_otc_advertisements`(9601)/`ce_otc_orders`(9602)/`ce_otc_counterparties`(9603)，经 migrate）、`service.go`（CreateAdvertisement/TakeOrder 冻结卖方 crypto 入 `SysOtc`/MarkPaid/ConfirmComplete 释放给买方+更新双向对手方信用/CancelOrder 退回托管/OpenDispute/ResolveDispute 管理员裁决/Reconcile 对账托管余额与未释放订单/RunLoop 后台对账告警；`priceFn` 预留行情入口）、`handler.go`（`/api/v1/otc` 下 advertisements 增/列、orders/take、orders/:id 的 pay/complete/cancel/dispute/resolve、orders 我的/管理员、counterparties、admin/reconcile，受 `middleware.Auth` 保护，user_id 取自 token）、`service_test.go`。
  - `ledger.SysOtc`（-6）新增为 OTC 中央托管账户。
- `cmd/otc/main.go`（薄装配：ledger 种子 BTC+USDT、Store 选 MySQL+迁移否则内存、oracle 价格（预留）、装配 Service、注册路由、后台对账循环、信号退出），监听 :8091。
- `Makefile`：SERVICES 加入 otc（现共 11 个服务）。

验证：单测 `internal/otc/service_test.go`（发布/浏览过滤、吃单锁定托管、拒绝吃自己广告、付款+确认释放托管并计对手方信用、取消退回托管、余额不足拒绝、争议裁决释放给买方）全绿；`go build/vet/test ./...` 全绿。**远程 MySQL 冒烟通过**（连 sqlpub `wallet` 库跑迁移建 `ce_otc_advertisements`/`ce_otc_orders`/`ce_otc_counterparties`；401 鉴权、建卖方广告→吃单锁 1 BTC 托管→付款→确认释放、对账 `balanced:true` escrow=0、对手方信用 trades_total=1/rating_sum=5、订单 completed 且 paid_at/completed_at 正确落库；三张表持久化已核验）。

> 踩坑修复：远端 MySQL 开启 `NO_ZERO_DATE`，`UpdateOrder` 把零值 `time.Time`（`0001-01-01`）写入可空 `DATETIME` 会被拒（Error 1292）。已改为 `sql.NullTime`（`toNullTime` 零值转 NULL）；该路径内存 store 单测覆盖不到，靠远程冒烟暴露。

> 说明：OTC 中央托管模型下，吃单时卖方 crypto 冻结入 `SysOtc`，买方线下付法币后卖方确认、crypto 由 `SysOtc` 释放给买方；取消/争议退回则归还卖方。所有订单终态后 `SysOtc` 余额应恒为 0，`/admin/reconcile` 与后台 `RunLoop` 即做此对账。法币不在账本内（P2P 线下），符合场外交易实际。

## 12. 业务线落地：wealth 理财资管（2026-08-13，已完成）

T-14 最后一项业务线（用户"继续"在 otc 收尾后立项）。理财资管核心，采用**中央托管模型**（用户申购本金转入 `SysWealth`，赎回本金+应计收益由此支出），自包含、集成 ledger 复式记账：

- `internal/wealth`：
  - `model.go`（WealthProduct/产品类型 current·fixed/状态机 open·closed、WealthHolding/持仓本金+已计收益+状态机 active→redeemed、应计收益纯函数 `YieldTo` 连续计息按小时、领域错误）、`store.go`（Store 接口：产品/持仓 CRUD）、`store_mem.go`（内存，单测用）、`store_mysql.go`+`store_migrations.go`（MySQL `ce_wealth_products`(9701)/`ce_wealth_holdings`(9702)，经 migrate）、`service.go`（CreateProduct/Subscribe 扣用户可用本金转入 `SysWealth`/Redeem 活期随时可赎、定期须到期、本金+应计收益从 `SysWealth` 支出/Accrue 后台应计收益更新 `AccruedYield`/RunLoop 后台应计循环；`Maturity` 计算定期到期时间）、`handler.go`（`/api/v1/wealth` 下 products 增/列、subscribe、redeem、holdings 我的/管理员、admin/accrue，受 `middleware.Auth` 保护，user_id 取自 token）、`service_test.go`。
  - `ledger.SysWealth`（-7）新增为理财中央托管账户（余额 = Σ本金 − Σ已计收益负债，系统账户允许透支）。
- `cmd/wealth/main.go`（薄装配：ledger 种子 USDT+BTC、Store 选 MySQL+迁移否则内存、装配 Service、无产品时自动发行"USDT 活期宝(3%)"与"USDT 30 天定期(6%)"示例、注册路由、后台应计循环、信号退出），监听 :8092。
- `Makefile`：SERVICES 加入 wealth（现共 12 个服务）。

验证：单测 `internal/wealth/service_test.go`（申购扣本金+托管收本金、活期立即赎回仅回本金、持有 1 年赎回含收益≈1365、定期未到期拒绝 ErrLocked、低于起购额/余额不足拒绝、后台 Accrue 应计收益≈10、YieldTo 纯函数）全绿；`go build/vet/test ./...` 全绿（无回归）。

> 说明：理财收益按"本金 × 年化 × 持有小时 / 8760"连续计息（活期和定期统一公式）。`SysWealth` 在申购时记入本金（正），赎回时支出本金+收益；后台 `RunLoop`/`admin/accrue` 周期性把应计收益计入 `AccruedYield` 字段，便于持仓展示当前价值。收益负债随赎回逐步由系统账户支出，复式记账自动守恒。

**远程 MySQL 冒烟（2026-08-13，:8092，复用 otc 脚本化流程）**：用 SQLPub `wallet` 库启动 `cmd/wealth`，跑通完整闭环——401 无 token、列产品(自动发行的活期宝 id=1 / 30天定期 id=2)、user1 申购活期 1000 + 定期 2000（本金冻结入 `SysWealth`）、我的持仓、后台 accrue（秒级应计收益≈7.6e-6）、赎回活期成功（status→redeemed，本金+微小收益从 `SysWealth` 支出）、赎回定期被锁定期正确拒绝（`4002 fixed product is still locked before maturity`）、管理员持仓视图正常；远程表 `ce_wealth_products`(2 行)/`ce_wealth_holdings`(2 行) 经迁移创建并持久化，`redeemed_at` 零值以 `NULL` 落库（复用 `toNullTime`，无 NO_ZERO_DATE 报错）。`go build/vet/test ./...` 全绿无回归。

## 13. 安全加固（2026-08-13，已完成）

用户决定暂不接入 Redis/Kafka/InfluxDB/ES，先做安全加固（纯本地方案，不依赖外部中间件）。

### 13.1 新增统一安全中间件套件
`internal/pkg/middleware/security.go`（`pkg/middleware` 原有 `Auth`/`RateLimit`）新增：
- `SecurityHeaders()`：注入 `Strict-Transport-Security` / `X-Content-Type-Options: nosniff` / `X-Frame-Options: DENY` / `Referrer-Policy: no-referrer` / `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'`，并剥离 `Server` 头。
- `MaxBodySize(maxBytes)`：限制请求体（默认 1 MiB），防大 payload DoS。
- `CORS(allowedOrigins)`：受控跨域白名单，未配置则默认拒绝一切跨域。
- `Audit(log)`：访问审计日志（method/path/user_id/status/latency/client_ip），由结构化日志落地。
- `AuthWithSkips(v, skipPrefixes...)`：鉴权但豁免指定前缀路径（公开端点）。
- `Common(log, cfg)`：返回 `[Recovery, Audit, RateLimit, SecurityHeaders, CORS, MaxBodySize]`。顺序刻意让 Audit 紧跟 Recovery（覆盖所有响应含被拒请求）、安全头/限流在入口 Auth 之前（401 也带安全头）。

### 13.2 配置与启动
- `internal/pkg/config/config.go`：`Server` 增加 `rate_limit_per_sec` / `allowed_origins` / `max_body_bytes` / `tls{cert_file,key_file}`，并新增 `TLSEnabled()` / `Listen(r, addr)` / `ListenServer(srv)`（配了证书则启用 HTTPS，否则明文）。`configs/config.yaml` 同步补充示例字段。

### 13.3 修复的关键漏洞
1. **futures 完全无鉴权**（且 gateway 不代理它，直接暴露）——`internal/futuresapi/handler.go` 的 `RegisterRoutes` 改为接收 `verifier` 并 `r.Use(AuthWithSkips(v, "/metrics"))`，仅 Prometheus 指标公开，下单/钱包/提现全部强制鉴权。
2. **spot 下单无鉴权**——`internal/spot/server.go` 的 `RegisterRoutes` 改为接收 `verifier` 并 `r.Use(AuthWithSkips(v, "/api/v1/spot/depth", "/api/v1/spot/ws"))`，行情深度与 WebSocket 行情公开，下单强制鉴权。
3. **market 与 futures 端口冲突**（都硬编码 `:8083`）——futures 端口改为 `:8084`（market 占 8083，gateway 代理 market 指向 8083 正确）。
4. **gateway 强制 Auth 会阻断 user/login**——gateway 改为 `AuthWithSkips(v, "/api/v1/user/login", "/api/v1/user/register")`，放行认证端点，其余强制鉴权；后端二次校验（零信任）。

### 13.4 统一接入
所有 `cmd/*/main.go`（options/wealth/otc/margin/notification/risk/user/market/settlement/spot/futures/gateway）均接入 `middleware.Common(log, cfg)`，末尾改用 `cfg.Listen` / `cfg.ListenServer` 以支持 TLS。限流/审计/安全头/CORS/请求体限制因此覆盖全部服务；公开路径（行情、健康检查、登录注册、metrics）由各自 `AuthWithSkips` 豁免。

### 13.5 测试与验证
- 新增 `internal/pkg/middleware/security_test.go`：`TestSecurityHeaders` / `TestCORS` / `TestMaxBodySize` / `TestAudit` / `TestAuthWithSkips` 全绿。
- `go build/vet/test ./...` 全绿（无回归）。
- 本地冒烟（直连 `no_proxy`，避免代理干扰）：
  - spot `:8082`：深度无 token→200、下单无 token→401、带 token→200 且响应头含 `X-Frame-Options: DENY` / `X-Content-Type-Options: nosniff` / HSTS。
  - futures `:8084`：metrics 无 token→200、下单无 token→401、带 token→200（`{"order_id":1,"status":"accepted"}`）。
- **注意**：冒烟时用临时配置把 `mysql.dsn` 置空让 futures 走内存种子秒起（避免连 SQLPub 阻塞）；务必保留 `auth.secret`，否则 `Verify` 因空密钥 fail-closed 导致所有 token 401。

## 14. 前端 SPA + 网关扩展（2026-08-13，已完成）

用户决定先实现（按序）：①修下单回归（登录/鉴权）→②补业务线页面→③接 WebSocket。配套需让网关代理全部业务线。

### 14.1 网关扩展（`cmd/gateway/main.go` + `configs/config.yaml`）
- 代理改为遍历 `cfg.Services` 自动反代所有业务线（原只代理 user/spot/market）；`services` 现含全部 11 个服务（futures:8084 / margin:8087 / notification:8088 / risk:8089 / options:8090 / otc:8091 / wealth:8092 / settlement:8086）。httputil 反代默认透传 WebSocket 升级。
- `AuthWithSkips` 豁免清单扩充：注册相关 `user/send-code` `user/verify` `user/forgot` `user/reset`，以及公开行情 `spot/depth` `spot/ws` `market/ticker` `market/ws`。**修复回归**：此前网关只放行 login/register，导致经网关的 `market/ticker`、`spot/depth`、`spot/ws` 被 401 拦截（原前端 Ticker/OrderBook 直接失效），现已放行。

### 14.2 前端鉴权（修下单回归）
- `../ce-frontend/src/api/client.ts`：统一客户端，自动注入 `Authorization: Bearer`，遇 401 自动用 `refresh_token` 刷新并重试一次（并发刷新去重）；解包 `{code,message,data}`；附带 `connectSpotWS` / `connectMarketWS` 助手。
- `../ce-frontend/src/lib/auth.tsx`：AuthContext（login/logout）+ 哈希路由守卫（无 token 跳 `/login`）。
- `../ce-frontend/src/lib/storage.ts` 同 client 内 `tokenStore`（localStorage 存 access/refresh/uid）。
- 新增 `../ce-frontend/src/pages/Login.tsx`（账号+密码）、`Register.tsx`（发码→注册），下单接口因此重新可用。

### 14.3 业务线页面
- 引入 `NavBar`（Tab 导航 + 退出）、`ApiTable`（GET 端点 + `JsonTable` 通用渲染，自动取并集表头）、`JsonTable`（数组→表格 / 对象→键值 / 其余原样）。
- 页面：`Trade`（现货，复用原组件）、`Wallet`、`Futures`、`Options`、`Otc`、`Margin`、`Wealth`、`Risk`、`Notifications`，覆盖合约/期权/OTC/杠杆/理财/风控/通知的只读列表端点；下单走现货。
- `../ce-frontend/src/App.tsx` 哈希路由 + 守卫；`styles.css` 补充暗色主题导航/表格/表单样式。

### 14.4 WebSocket 实时推送（行情/订单簿）
- `Ticker` 改用 `market/ws`（广播 Ticker 快照）实时刷新，断线回退 REST 2s 轮询；`OrderBook` 改用 `spot/ws`（广播 `{type:depth,data}`）实时刷新，首屏与 WS 缺失时回退 REST 拉取；均带"实时/轮询"状态点。

### 14.5 验证
- 前端 `cd ce-frontend && npm run build`（`tsc -b` 严格模式 + `vite build`）通过，产物 `dist/` 正常。
- 后端 `go build/vet ./...` 全绿。
- 网关 e2e 冒烟（临时空 dsn 配置，user 走内存存储，直连 `no_proxy`）：`send-code`→`register`(user_id=1)→`login`(返回 access_token)→**带 token 的 `spot/order` 200 accepted**、`user/me` 200；公开 `market/ticker`/`spot/depth` 200（回归修复）；无 token 的受保护端点 401。完整闭环证明前端鉴权链路与下单回归已修复。

## 15. 合约强平独立扫描（2026-08-13，已完成）

修复上一轮识别的合约爆仓头号设计缺口：**强平只在撮合成交回调 `onTrade` 中触发，无成交时即便预言机指数价击穿强平价也不会扫描强平**（资金安全风险）。

### 15.1 改动（`internal/futuresapi/server.go`）
- 新增独立循环 `liqScanLoop()`（常量 `liqScanInterval = 2s`）：周期性对每个交易对用预言机指数价刷新 `markCalcs[sym].SetIndex(idx)`，取 `MarkPrice()` 后调用 `liquidator.UpdateMarkPrice(sym, mark)` 触发强平扫描；`UpdateMarkPrice` 对已强平持仓幂等（二次扫描不产生事件），可安全高频调用。随 `Server.ctx` 取消退出，`NewServer` 中以 `go s.liqScanLoop()` 启动（与 `fundingLoop` 并列）。
- 抽取 `broadcastLiquidations(evs)`：原 `onTrade` 的强平日志+WS 广播逻辑抽出，`onTrade` 与 `liqScanLoop` 共用，日志新增 `partial` 字段。
- 触发来源与成交解耦：标记价 = 指数价 + 溢价EMA，无成交时溢价EMA=0，mark=指数价，故指数价击穿强平价即可触发，与是否有成交无关。

### 15.2 验证
- 新增 `internal/futures/engine_test.go::TestLiquidationScanIdempotent`：直接调用 `UpdateMarkPrice`（等价独立扫描、无成交上下文）验证——标记价击穿强平价触发 1 次强平事件且仓位清空；同价二次扫描 0 事件（幂等）；空账户扫描 0 事件。`go test ./internal/futures/` 通过。
- `go build/vet ./...` 全绿（无回归）。
- 说明：`liqScanLoop` 调度层为薄封装（ticker + SetIndex + 复用已验证的 `UpdateMarkPrice`），正确性由编译 + `engine_test` 逻辑复用保证；完整端到端（真实预言机变价触发）依赖成交/下单构造持仓，未做重 e2e。

## 16. 强平单真正送入撮合引擎（liquidate 经 matching.Engine 成交）

**背景**：此前强平（`Liquidator.liquidate`/`liquidateCross`）直接调 `Position.PartialClose(mark, ratio)` 按标记价**记账减仓**，`onLiquidation` 仅做账本动作；注释写的 "liquidation order queued" 实际从未进订单簿——强平没有经过撮合引擎，无法与订单簿真实流动性成交、也不会出现在成交流/行情中。

**改造（2026-08-13）**：
- `internal/matching`：新增 `Engine.MatchNow(symbol, o, rest)` 同步撮合（不入队、不触发 `onTrade`/`onBook`，避免重入 `UpdateMarkPrice`），返回成交与是否完全成交；`OrderBook.Match` 重构为 `MatchRest(in, rest)`，`Match` 等价于 `MatchRest(in, true)`（原行为不变）。`rest=false` 时市价单（Price=0）未成交部分**不挂单**，避免空流动性时残留 price=0 挂单污染订单簿（剩余由保险基金兜底成交）。
- `internal/futures`：新增 `LiquidationFill` 与 `LiquidationCloser` 类型；`Liquidator` 新增 `closer` 字段 + `SetLiquidationCloser`，`NewLiquidator` 内置默认 closer（按标记价直接减仓，兼容单测/无引擎降级）；`Position.closeBy(price, qty)` / `CrossAccount.closeBy(price, qty)` 按**真实成交价**减仓（保留 `PartialClose` 供 ADL/社会化分摊按标记价强制减对手仓）。`liquidate`/`liquidateCross` 改写：每步把强平单交给 `closer` 取得真实成交后调用 `closeBy` 回填持仓，手续费按成交均价计。
- `internal/futuresapi`：注入 `liquidationCloser`——把强平单作为**市价单**经 `engine.MatchNow` 成交（平多=卖、平空=买，方向已修正）；订单簿流动性不足时剩余部分由 **`ledger.SysLiquidationLoss`（保险基金）兜底成交**（与真实交易所保险基金兜底一致），保证强平必定完成；返回加权成交均价供强平引擎回填。`onLiquidation` 误导性日志 "liquidation order queued" 改为 "liquidation settled"。

**关键设计点**：
- 撮合与持仓账本解耦：持仓仍在 `Liquidator`，订单簿在 `matching.Engine`；强平经撮合引擎取得真实成交价回填持仓，成交消耗订单簿真实流动性（行情/深度随之变化）。
- 无重入死锁：强平在 `UpdateMarkPrice`（持有 PositionBook 锁）中调 `MatchNow`（持有 OrderBook 锁）；由于 `run` 协程在 `Match` 释放 OrderBook 锁后才调 `onTrade`→`UpdateMarkPrice`，两锁获取顺序无环，不会死锁。`MatchNow` 不回调 `onTrade` 亦避免重入。
- 兜底保证强平完成：本原型订单簿通常无挂单，强平单由保险基金兜底成交，价格劣于破产价的部分仍走既有穿仓吸收瀑布（保险→ADL→社会化→残差）。

### 16.1 验证
- `internal/futures/engine_test.go` 新增 `TestLiquidationThroughMatchingEngine`（注入 closer 返回成交均价 44000≠标记价 45000，断言 `Realized=-6000`/`Deficit=1000`/参数正确/仓位清空，证明走撮合引擎成交价而非标记价）、`TestLiquidationPartialFillFromEngine`（closer 每轮只成交一半，断言强平引擎分批持续清算直至整仓清空、不卡死）。
- `internal/matching/engine_test.go` 新增 `TestMatchNowFillsAgainstBook`（市价卖单与挂单限价买成交）、`TestMatchNowNoLiquidityNoRest`（无流动性+rest=false 不挂 price=0 单、不污染深度）。
- `internal/futuresapi/server_liquidation_test.go` 新增 `TestLiquidationCloserBackstop`：直接测生产 `liquidationCloser`——无流动性兜底@标记价、有流动性@订单簿价、部分流动性加权均价，覆盖方向修正（平多=卖命中买方流动性）。
- `go build/vet/test ./...` 全绿（无回归）。



## 17. 撮合引擎多实例部署 + 崩溃恢复（持久化 / 单写者副本）

**背景**：原 `matching.Engine` 是纯内存、每交易对一个 goroutine 的 actor 模型；订单簿只在进程内、`Order.ID` 由调用方 `atomic.AddInt64` 本地计数器生成。这导致两个硬伤：
1. **重启丢单**：进程崩溃后内存订单簿全部丢失，未成交挂单无法恢复。
2. **多实例冲突**：部署多个现货/合约进程时，各自一份订单簿（订单分裂）、`order_id` 本地计数器互相冲突、重启即丢。

用户选择「撮合引擎多实例/持久化（共享存储/快照）」作为下一步。

**设计（单写者副本模型）**：同一时刻只有一个进程作为 leader 写共享订单簿；其余为 follower（standby），leader 租约过期后竞争接管，接管时从「快照 + WAL」恢复，不丢单。共享存储复用已接入依赖的 MySQL（`go-sql-driver/mysql` 已在 go.mod，此前全项目无任何 Go 代码真正连 MySQL）。

**改造（2026-08-13）**：
- `internal/matching/persist.go`（新增）：定义 `Store` 接口，承担四职责——①全局唯一单调递增订单号；②WAL（订单应用到内存簿**之前**落盘）；③全量快照（周期序列化订单簿，加速恢复+剪枝 WAL）；④leader 选举（基于租约的单写者锁）。`OrderEvent`（submit/cancel）与 `BookState` 一并定义于此，避免循环依赖（接口在 `matching` 包、实现在 `persist` 包导入 `matching`）。
- `internal/matching/persist/mem.go`（新增）：`MemStore` 纯内存实现（无持久性），单实例开发/单测与降级路径使用；leader 以 holder+过期时间模拟。
- `internal/matching/persist/mysql.go`（新增）：`MySQLStore` 以 MySQL 为共享后端：`ce_matching_wal`（seq 自增、JSON payload）、`ce_matching_snapshot`（id=1 覆盖式）、`ce_matching_seq`（LAST_INSERT_ID 保证全局唯一订单号）、`ce_matching_leader`（租约锁，`UPDATE ... WHERE holder=? OR holder='' OR expires_at<NOW()` 抢锁，`RowsAffected==1` 判定成功）。
- `internal/matching/persist/migrate.go`（新增）：`Migrations()` 提供版本 200–203 的 `ce_matching_*` 建表迁移（沿用 `internal/pkg/migrate`，`ce_` 前缀约定，版本段不与他人冲突）。
- `internal/matching/engine.go`：保留 `NewEngine(onTrade,onBook)` 签名（不破坏 spot/futures 与既有测试）；新增 `UseStore(store,nodeID,interval)`、`Submit` 经 store 分配全局 ID 并**同步写 WAL 再入队**、`Cancel`（WAL+`book.Cancel`）、`Recover(ctx)`（加载快照→重放增量 WAL→注册交易对，幂等）、`SnapshotLoop(ctx)`（周期快照+剪枝）、`Reset()`（清空内存簿+结束 goroutine，供失去 leadership 后重同步）。未接入 Store 时行为完全不变。
- `internal/matching/orderbook.go`：新增 `Snapshot()`/`Restore()`/`Cancel()`（均按 `book.mu` 加锁，与撮合互斥）。
- `cmd/matching/main.go`（新增）：撮合引擎独立部署形态。构建 Store（有 DSN→MySQL+跑迁移，否则 Mem）；leader 选举循环 `tryAcquire→recover+snapshot / 失去则 Reset 进 standby`；提供 `POST /order`、`GET /depth`（非 leader 返回 503）、`GET /health`（返回 node/leader 状态）。config 新增 `matching.snapshot_interval_sec`/`leader_ttl_sec`。
- `configs/config.yaml`：新增 `matching` 段。

**关键设计点**：
- 写顺序保证持久性：WAL 在「订单入内存簿之前」同步落盘；恢复时按 seq 升序重放，重建出与崩溃前一致的订单簿。
- 全局订单号：`Submit` 中 `o.ID==0` 时由 `Store.NextOrderID` 分配；恢复后 `SetMinOrderID(maxID)` 保证新号严格大于历史，跨实例/跨重启不复用。
- leader 租约：`ttl` 默认 10s，follower 每 `ttl/2` 尝试抢锁；续约失败/租约过期即降级为 standby 并 `Reset`，杜绝双写。
- 零回归：spot/futures 暂不接 Store（保持原内存+本地 ID 行为，避免与 cmd/matching 争抢同一 MySQL leader 锁）；它们何时改为调用 cmd/matching 的 HTTP API 是后续收敛项（见下）。

**已知缺口（后续跟进）**：
- 强平路径 `MatchNow` 消耗订单簿流动性，但该变更**不写入 WAL**（强平属特殊路径、且 `MatchNow` 刻意不回调 `onTrade`）；严格恢复需把强平成交也 journal。当前由强平扫描在恢复后重新触发兜底，可接受于原型。
- ~~spot/futures 仍各自持有内存订单簿；真正「全交易所多实例」应把匹配收敛为单一 `cmd/matching` 服务（单写者可水平容灾），spot/futures 改为其 HTTP 客户端。本轮未做（属更大的服务网格重构）~~ **已完成（见 §18 / §18.1，2026-08-14 起落地）**：`internal/spot/server.go:75` 与 `internal/futuresapi/server.go:157` 已改为 `cmd/matching` 的 HTTP+WS 客户端（`client.New(cfg.Matching.URL)`），不再持有订单簿；单一写者 + leader 选举 + 崩溃恢复见 §17。多实例不再分裂簿，三进程端到端验证已证明 spot/futures 写入同一订单簿。

### 17.1 验证
- `internal/matching/persist/mem_test.go`：`TestMemNextOrderIDMonotonic`（序号单调）、`TestMemWALReplayAndPrune`（WAL 回放+快照+剪枝）、`TestMemLeaderMutualExclusion`（双节点互斥：A 持锁时 B 不可获、A 释放后 B 可获）、`TestMemLeaderExpiryTakeover`（租约过期 B 可接管）、`TestOrderEventJSONRoundTrip`（JSON 往返，MySQLStore 依赖）。
- `internal/matching/engine_recover_test.go`（外部包 `matching_test`，规避 import 环）：`TestSubmitWritesWAL`（Submit 同步落 WAL 且分配 ID）、`TestEngineRecoverFromStore`（共享 MemStore 模拟崩溃：引擎 B 恢复出 A 写入的订单簿、新 ID 不复用）、`TestEngineRecoverReflectsCancel`（撤单经 WAL 恢复后生效）、`TestEngineRecoverIdempotent`（Recover 幂等）。
- `go build/vet/test ./...` 全绿（无回归）。
- **真实 MySQL 端到端（sqlpub 远端库）**：启动 `cmd/matching` node-A（:8085）获 leader 并写入订单 1/2 → 启动 node-B（:8086）为 standby（拒绝服务 503）→ `TaskStop` 杀 A → B 在租约过期后接管为 leader，从 MySQL 恢复出订单 1/2（**不丢单**），新订单拿到 ID 3（**全局唯一不复用**）。证明多实例单写者 + 崩溃恢复成立。

## 18. 撮合收敛：spot/futures 改为调用 cmd/matching 的 HTTP 客户端（服务网格级重构）

**背景**：§17 把撮合引擎升级为支持多实例/崩溃恢复的 `cmd/matching` 单写者服务，但 `internal/spot` 与 `internal/futuresapi` 仍各自 `matching.NewEngine` 持有独立内存订单簿——这意味着「多实例」只在 `cmd/matching` 内部成立，spot/futures 进程多了仍然是多份分裂的订单簿。用户要求（「继续」原 §17 已知缺口②）把匹配收敛为**单一 `cmd/matching` 权威服务**，spot/futures 改为其 HTTP+WS 客户端。

**设计**：
- 定义 `matching.Matcher` 接口（`Submit`/`Cancel`/`Depth`/`MatchNow`），`*matching.Engine` 与 `*client.Client` 都满足它，使 futures 的强平路径（`liquidationCloser` 同步调 `MatchNow`）可无缝切换且保持单测能力。
- `internal/matching/client`：`Client` 经 HTTP 调 `cmd/matching` 的 `/order` `/cancel` `/depth` `/match-now`，经 WebSocket 调 `/ws` 订阅行情（`Watch` 带断线重连）；满足 `matching.Matcher`。**关键修复**：`cmd/matching` 的响应经 `response.JSON` 包了 `{code,data,message}` 信封，client 的 `postJSON/getJSON` 必须解包 `data` 层再 unmarshal（首版漏解包导致 spot/futures 下单全部 400，已修正）。
- `cmd/matching` 扩展：`ws.NewHub` 接线 `onTrade/onBook` 广播 `/ws`；新增 `/cancel`、`/match-now`、`/ws`、`/order` 支持 `user_id`；leader 守卫覆盖所有写端点；超集交易对从 config 载入。
- `internal/spot/server.go`：移除内嵌引擎，改用 `*matching.Client`；`handleOrder→Submit`、`handleDepth→Depth`、行情由 `Watch` 驱动的 `onTrade/onBook`（收到后转播到本地 `ws.Hub` 供前端）。
- `internal/futuresapi/server.go`+`handler.go`：引擎字段改为 `matcher matching.Matcher`；`handleOrder` 与 `liquidationCloser` 改用 `matcher`（强平市价单走 `MatchNow`）；`onTrade` 由 WS `Watch` 驱动；移除本地 `orderSeq`/`atomic`、保留 `nextOrderID` 引用清理。

**改动文件（2026-08-14）**：
- 新增：`internal/matching/matcher.go`（`Matcher` 接口）、`internal/matching/client/client.go`（HTTP+WS 客户端）、`internal/matching/client/client_test.go`（fake REST 走 `response` 信封，验证契约）。
- 改：`cmd/matching/main.go`（WS 广播 + `/cancel`/`/match-now`/`/ws` + config 交易对）、`internal/spot/server.go`（client 化）、`internal/futuresapi/server.go`、`internal/futuresapi/handler.go`（`matcher` 接口化）、`internal/pkg/config/config.go`（`Matching.URL/Symbols`）、`configs/config.yaml`（`matching.url`/`symbols`）、`internal/futuresapi/server_liquidation_test.go`（`s.engine→s.matcher`）。
- 不变：`matching.Engine` 仍直接满足 `Matcher`，spot/futures 本地单测（内存引擎）零改即可复用（强平测试等）。

**验证**：
- `go build/vet/test ./...` 全绿（无回归）。
- **真·端到端（连 sqlpub 远端 MySQL，三进程）**：启动 `cmd/matching`(e2e-A, :8085) 为 leader（启动时即从 MySQL 恢复出 §17 残留挂单，证明持久化持续生效）→ 启动 `cmd/spot`(:8082)、`cmd/futures`(:8084) 经 `client` 连接匹配服务 →
  - spot 下 buy@100 qty2 返回 `order_id=12`，spot 的 depth 与 `cmd/matching` 的 depth 显示**同一个订单簿**（bids price=100 累计到 10）→ 证明单一匹配权威；
  - futures 开多单返回 `order_id=13`，仓位建立（LiqPrice≈45226），且该挂单出现在 `cmd/matching` 的 `BTC_USDT_PERP` 订单簿（ID=13）→ spot/futures 写入同一引擎；
  - 独立 WS 客户端连 `cmd/matching /ws` 后下对手卖单，收到 `trade` + `depth` 广播 → 证明行情经 WS 回传 spot/futures 本地 hub 的链路可用。
- 已知：强平路径 `MatchNow` 现已在接入 `Store` 时同步写 WAL（`EventSubmit`，§17 已知缺口①已补齐——恢复经 `Recover` 重放该成交，且重放不触发 `onTrade` 故不会重复结算）；未接入 `Store` 时行为不变。高级订单类型 IOC/FOK/止盈止损已在 `internal/matching/orderbook.go` 支持并经单测覆盖。

### 18.1 后续（网关层收敛，2026-08-16 已落地安全子集）
- 「全所单写者容灾」的两种收尾选项中，**「spot/futures 仅做业务/账本、匹配全权委托」已由 §18 通过 `matching.Client` 落地**：spot/futures 不再持有订单簿，全部委托 cmd/matching（单一写者、leader 选举 + 崩溃恢复见 §17），多实例也不会分裂簿。
- 网关层收敛按 **安全子集** 实现（`cmd/gateway` + `configs/config.yaml`）：把 `matching` 纳入 `services` 拓扑（运营看板可探活），并仅在网关暴露其**只读/行情端点**（`/api/v1/matching/{depth,ws,orders,orders/:id,trades,health}`）；其**写端点**（`/order`、`/cancel`、`/match-now`）刻意不代理——订单提交必须仍经 spot/futures，因为 `spot/futures` 的 `handleOrder` 内做资金预冻结（`ledger.Freeze`）与成交账本结算（`ledger.Transfer`），cmd/matching 仅负责撮合（无钱包/账本），若经网关直连会**绕过整套资金安全控制**（资金安全隐患）。
- 配套健壮性修复：网关的反代 writer 包了一层 `proxyWriter`，安全补全 `http.CloseNotifier`/`http.Flusher`/`http.Hijacker`（gin 的 writer 对 `CloseNotifier` 用「断言后调用」，底层未实现时直接 panic；新版 Go 移除该接口后线上亦可能 panic）。测试见 `cmd/gateway/main_test.go`（锁定「matching 写端点不直连网关」的不变量 + 只读端点正确收敛）。
- **未做（可选，不建议）**：把 `/api/v1/spot/*order`、`/api/v1/futures/*order` 也直接代理到 cmd/matching 的 `/order`——这需把钱包/账本逻辑搬进撮合引擎，属更大重构且会弱化 spot/futures 业务层边界，当前不采纳。
- 强平 `MatchNow` 的流动性消耗现已写 WAL（见 §17 已知缺口①已补齐）；T-03 预言机半边已由 b1c795d 落地（NewFromConfig 配置驱动真实 REST 喂价），链上 RPC 半边（提现广播）已完成**可插拔脚手架**见 §27（生产填真实节点即生效，未配置回退模拟，fail-degraded）。两侧均以「配置驱动 + 未配置降级」方式落地，剩真实节点/热钱包/离线签名为生产接入的最后一环（依赖外部节点+合规）。

## 20. 集中式订单管理模块（现货/合约订单 + 成交流水查询 + 管理后台跨用户撤销）

**背景**：§18 把匹配收敛为单一 `cmd/matching` 权威服务后，订单/成交状态仅存在于撮合引擎内存（重启丢单、且 spot/futures 无查询入口）。用户要求「几种交易的订单都要有管理模块」——现货与合约（撮合型）的订单/成交需对**用户侧**和**管理后台**都提供查询与管理，且集中实现在撮合引擎，避免各交易类型分裂。

**设计**：
- 撮合引擎新增**订单/成交登记簿**（`Engine.orders`/`userOrders`/`trades`/`userTrades`）：`registerOrder` 在 `Submit`/`MatchNow` 时写入不可变快照 + `FilledQty` 累加器（避免止盈止损激活副本导致的指针错位）；`applyTrades` 按 `TakerOID/MakerOID` 累加成交并派生状态（open/partial/filled/canceled）；`ListOrders/GetOrder/ListTrades` 支持 `user_id`/`symbol`/`status`/`market`/`limit` 过滤（`user_id=0` 返回全部，供管理后台）。`Recover` 后 `rebuildOrderIndex` 重建 open 订单索引。
- `matching.Order` 新增 `Market` 字段（`spot`/`futures`），下游下单时写入；`OrderView`/`TradeView` 透出 `market`，使现货/合约共用同一登记簿时仍能按市场区分。**注意**：当前为内存簿，重启后仅 open 订单经 `Recover` 重建，历史 filled/canceled 与成交流水不重建（原型限制）。
- **杠杆标记（2026-08-15 补）**：`matching.Order` 新增 `IsMargin`/`Leverage` 字段；现货杠杆单由 `spot.handleOrder` 透传（`is_margin`/`leverage`），合约单在 `futuresapi.handleOrder` 强制 `IsMargin=true` 并透传 `Leverage`；`OrderView` 透出二者。语义：`IsMargin`=该单为杠杆单（现货杠杆单与合约单均为 true，普通现货单为 false）。`OrderView.MarginMatches(q)` 统一过滤：`q` 为空/"all" 全部通过，"1"/"true"/"margin" 仅杠杆单，"0"/"false" 仅非杠杆单。用户侧 `GET /api/v1/spot/orders` 与 `/api/v1/futures/orders`、管理后台 `GET /api/admin/orders` 均支持 `?margin=` 过滤（同时修正了 spot `/orders` 此前未过滤 `market=spot`、会泄漏合约单的问题）。
- **成交流水杠杆筛选（2026-08-15 续）**：`TradeView` 新增 `IsMargin`/`Leverage`，由 `applyTrades` 从**吃单（taker）订单**继承（合约成交天然带杠杆；现货杠杆单成交亦带标记）；`TradeView.MarginMatches(q)` 与订单共用 `marginMatches` 逻辑。用户侧 `GET /api/v1/spot/trades`、`/api/v1/futures/trades` 与管理后台 `GET /api/admin/trades` 均支持 `?margin=` 过滤（可叠加 `market`/`symbol`/`user_id`）。
- `matching.Matcher` 接口新增 `Cancel(symbol, orderID) bool`；`*client.Client` 包一层 `CancelOrder` 实现它，使 futures 能以统一接口撤单。
- `cmd/matching` 新增只读端点（均经 `leaderGuard`，与 `/depth` 一致，因登记簿仅在 leader 维护）：`GET /orders?user_id=&symbol=&status=&limit=`、`GET /orders/:id`、`GET /trades?user_id=&symbol=&limit=`。响应直接以数组/对象作为 `data`（与 client `unwrap` 契约一致）。
- `internal/spot/server.go`：`GET /api/v1/spot/orders`、`/orders/:id`、`/trades`，仅返回**当前用户本人**订单（按 token `uid` 过滤 + `market=spot`）；详情校验归属（非本人 403）。
- `internal/futuresapi/handler.go`：`GET /api/v1/futures/orders`、`/orders/:id`、`/trades`、`POST /api/v1/futures/cancel`，仅返回本人 `market=futures` 订单；撤单前校验归属。
- `internal/adminapi`：新增 `trade:read`/`trade:manage` 两个 RBAC 权限（字典 `allPermissionDefs` 自动授予 super_admin；admin/operator 角色按职责分配）；直连 `cmd/matching`（`cfg.Matching.URL`）新增 `matchClient`；端点 `GET /api/admin/orders`（跨用户，`user_id` 缺省=全部，支持 `symbol`/`status`/`market`/`limit`）、`GET /api/admin/orders/:id`、`GET /api/admin/trades`、`POST /api/admin/orders/:id/cancel`（高危，需 `trade:manage`）。读需 `trade:read`，撤销需 `trade:manage`。

**改动文件（2026-08-15）**：
- 新增：`internal/matching/view.go`（`OrderView`/`TradeView`/`OrderStatus`）、`internal/adminapi/handlers_orders.go`（管理后台订单代理）、`internal/matching/engine_orders_test.go`、`internal/adminapi/handlers_orders_test.go`。
- 改：`internal/matching/matcher.go`（`Matcher` 增 `Cancel`）、`engine.go`（登记簿 + 查询 + `rebuildOrderIndex` + `Market` 透出 + 修复 `rebuildOrderIndex` 中 `book.Depth()` 返回值个数误用）、`orderbook.go`（`Order.Market`）、`client/client.go`（`ListOrders/GetOrder/ListTrades` + `Cancel`）、`cmd/matching/main.go`（查询端点）、`internal/spot/server.go`/`internal/futuresapi/handler.go`/`server.go`（用户侧端点 + `Market` 写入）、`internal/adminapi/server.go`/`rbac.go`/`store_adminaccount.go`（权限 + 代理 + 角色种子）。
- `internal/matching/client/client_test.go`：`newFakeREST` 增加 `/orders`/`orders/`/`trades` 端点，新增 `TestClientListOrdersAndTrades`。

**验证**：
- `go build/vet/test ./...` 全绿（新增 engine 登记簿、client HTTP 契约、管理后台代理三类测试）。
- 已知：期货撤单仅移除撮合簿中的挂单，合约持仓/保证金的释放依赖 `internal/futures` 持仓生命周期（已在下单时同步建仓），不在本轮订单管理范围内；管理后台跨用户撤单同理。

## 19. 前端拆分：用户前端(../ce-frontend/) 与 管理后台前端(../web-admin/)

**背景**：原前端是单一 React 应用 `../ce-frontend/`（哈希路由、token 保护），全是交易页面，管理后台完全不存在。用户要求把前端拆成「用户前端」与「管理后台前端」，并明确选了：① 两个独立项目；② 完整角色鉴权（后端加 role + 管理 API）；③ 管理后台 7 个模块全做（风控与强平监控 / 用户与账户管理 / 交易对配置 / 运营看板 / 充值提币 / 公链管理 / 币种管理）。

**后端改造（角色鉴权，2026-08-14）**：
- `internal/pkg/middleware/auth.go`：新增 `RoleUser`/`RoleAdmin` 常量；`TokenClaims` 加 `Role` 字段；保留 `Issue`/`Verify` 旧签名（默认 user，零回归），新增 `IssueRole(userID, role, ttl)`、`VerifyFull() (int64,string,bool)`；`Auth` 中间件内部改走 `VerifyFull` 并把 role 写入上下文；新增 `Role(c)`、`RequireRole(roles...)`、`AdminGuard()`（=RequireRole(admin)）。管理接口只需在 `Auth` 之后挂 `AdminGuard` 即可限制为管理员。
- `internal/pkg/config/config.go`：新增具名类型 `AdminConfig` 与 `Config.Admin` 字段（addr/username/password/token_ttl_sec/allowed_origins）；`configs/config.yaml` 加 `admin` 段（addr :8095，避开 services 已占的 8090；凭据 admin/admin123，原型明文）。
- `internal/adminapi/`：新增管理后台聚合包。`store.go` 定义 7 模块类型 + 内存管理态（seed 示例数据，CRUD 落内存，重启丢失——原型骨架）；`handlers.go` 实现全部 handler（风控快照只读、用户 CRUD+冻结/解冻、交易对 upsert、公链/币种 CRUD、充值提币列表+提币审核、通知 CRUD、账本对账、服务健康）；`server.go` 装配并在 `/admin` 组挂 `Auth`+`AdminGuard`。
- `cmd/admin/main.go`：管理后台后端；`-config` 约定（默认 configs/config.yaml）；把 `admin.allowed_origins` 并入 CORS 白名单；套 `middleware.Common` 安全中间件；`cfg.Listen`。
- `internal/adminapi/server_test.go`：验证登录签发 admin token、错误凭据 401、未带 token 401、admin token 访问 200、风控快照可读。

**前端（两个独立 Vite 工程，2026-08-14）**：
- `../ce-frontend/`（用户前端，**基本不动**）：保留全部交易页面；其中 `Risk.tsx` 是用户视角的风险信息，归属用户端。
- `../web-admin/`（管理后台前端，**全新**）：独立 package.json（port 5174）、vite 代理 `/api/admin → :8095`、hash 路由、`AuthProvider`+`requireAuth` 守卫、`api/client.ts`（注入 admin token、解包 `{code,data}` 信封、无 refresh 原型）、`components/{NavBar,ApiTable}`（通用表格+导航）、`lib/useFetch` hook；7 个模块页面：`Risk`(风控与强平监控)、`Users`(用户与账户管理)、`Symbols`(交易对/参数配置)、`Ops`(运营看板：账本对账+服务健康+运营通知)、`Deposits`(充值提币记录+提币审核)、`Chains`(公链管理)、`Coins`(币种管理)。两个前端物理隔离、各自 `npm build` 通过（web 51 模块 / web-admin 44 模块）。

**验证**：
- `go build/vet/test ./...` 全绿（auth 兼容性、admin 角色测试通过）。
- `../ce-frontend/` 与 `../web-admin/` 各自 `npm run build` 通过，互不耦合。

**已知缺口（后续，已全部闭合）**：
- 风控快照、账本对账、服务健康、运营通知——本条记为「内存只读骨架」的提示已过时：实际 `handleRisk` 实时聚合 futures 的 `/liquidations`、`/adl`、`/socialized`、`/wallet`；`handleLedger` 实时拉取 futures 的 `/wallet/reconcile` 与 `/wallet/inventory`；`handleServices` 探活 `cfg.Services` 各上游；`listNotifications` 代理 notification 服务 + 本地公告（CatalogStore）。四类均为真实后端，仅上游不可达时降级为内存示例。
- 用户、交易对、公链、币种、充值提币 CRUD 此前为内存态，现已全部对接真实持久化：**用户 CRUD→user 服务（§19 后端改造，createUser/updateUser/freezeUser 均代理 user）**；交易对/公链/币种→CatalogStore MySQL（§19）；通知→notification + CatalogStore（§19）；充值列表→futures `/wallet/deposits`、提币列表→futures `/wallet/withdraw/holds`（§19）；**提币审核→futures 的 approve/reject 端点（§25，管理员审批=链上放行/退回，不再仅写内存）；用户列表余额→futures `/wallet/balance` 实时富集 USDT 可用余额（§26，不再恒为 0）**。§19「管理后台接真实后端」目标已整体达成。
- 管理后台登录明文凭据——本条也已过时：`handleLogin` 已走 bcrypt（`bcrypt.CompareHashAndPassword`）比对 `AdminStore`（MySQL 优先、失败回退内存）中的 `PasswordHash`，并支持可选 TOTP；`config.yaml` 的 `admin.password_hash` 即为 bcrypt 哈希（可由环境变量 `ADMIN_PASSWORD_HASH` 覆盖）；另有 `/admin/password` 改密、`/admins/:id/reset-password` 重置等真实账号管理端点（见 handlers_admin.go）。

## 21. Kafka 交易清算接入（T-16，2026-08-15，已完成）

**背景**：§18 把撮合收敛为单一 `cmd/matching` 权威服务后，`cmd/matching` 在每笔成交时经 `internal/pkg/mq` 发布 `exchange.trades` 到 Kafka（需 `-tags kafka` 构建），但**没有任何消费端真正消费并记账**——T-02 设计的「成交流发布 Kafka 触发清算、闭合资金链路」只完成了一半（发布侧）。`cmd/settlement` 此前仅是纯 HTTP 费用估算/健康服务，未消费成交流。

**改动**：给 `cmd/settlement` 接上 Kafka 消费端，把每笔成交流驱动到交易所手续费账户清算入账，打通 T-02 资金闭环：

- `internal/settlement/clearing.go`（新增）：`ClearedTrade`（清算流水，含确定性幂等键 `ID`）、`ClearingStats`（聚合：总成交数/总量/总手续费/按 symbol 累计）、`ClearingStore` 接口、`Clearer`（消费 `mq.TradeEvent` → 计算 `Fee=Price*Qty*TradeFeeRate` → 幂等入账 + 更新统计）。`TradeID` 以 FNV-64a 对成交关键字段哈希得到稳定幂等键，保证 Kafka at-least-once 下重复投递可安全跳过。
- `internal/settlement/store_clearing_mem.go`（新增）：内存清算存储（id 索引去重，回退/单测用）。
- `internal/settlement/store_clearing_mysql.go` + `store_clearing_migrations.go`（新增）：MySQL 实现，`ce_settlement_trades`（版本 9801，主键 `id` + `INSERT IGNORE` 幂等，`idx_symbol`/`idx_cleared_at` 二级索引），沿用 `internal/pkg/migrate` 与 `ce_` 前缀约定。`NewClearingStore(dsn)` 优先 MySQL、失败回退内存。
- `cmd/settlement/main.go`（重写）：`mq.NewSubscriber(brokers, "settlement-clearer", handler)` 订阅 `exchange.trades`，handler 解包 `TradeEvent` 调 `Clearer.Clear`；新增 `GET /api/v1/settlement/cleared?limit=`（最近清算成交）与 `GET /api/v1/settlement/stats`（聚合统计）；信号退出时关闭 subscriber/store。手续费模型保持原 (链,资产) 维度费率。
- `internal/pkg/config/config.go` + `configs/config.yaml`：新增 `settlement.trade_fee_rate`（默认 0.001，<=0 用 `DefaultTradeFeeRate`）。
- `internal/settlement/clearing_test.go`（新增）：`TestClearerRecordsAndAggregates`/`TestClearerIdempotent`/`TestClearerViaSubscriber`（经 `InMemSubscriber.Feed` 走 Kafka 消费解包路径）/`TestTradeIDStable`，覆盖入账、幂等、统计、消费路径。

**设计要点**：
- 清算与用户资金解耦：现货/合约的用户余额变动仍由 spot/futures 直接经 Ledger 处理；本层只归集**交易所应收手续费**到独立清算流水，避免与用户资金混算，符合 T-02「清算服务记账」的语义。
- 降级保证可用：默认（无 `-tags kafka`）构建下 `mq.NewSubscriber` 退回 `InMemSubscriber`（`Subscribe` 为 no-op），清算消费不可用但 HTTP 费用估算/健康端点照常；配置了 brokers 且 `-tags kafka` 时启用真实消费组。发布侧 `cmd/matching` 同理，故真实闭环需两端均以 `-tags kafka` 构建并存在可达的 Kafka broker。
- 幂等：Kafka at-least-once + 主键 `INSERT IGNORE`，重复成交仅落库一次、仅计一次统计。

**验证**：
- `go build/vet/test ./...` 全绿（新增清算单测通过）；`go build -tags kafka ./...` 全绿（sarama 已在 go.mod，发布/消费两侧 Kafka 路径均可编译）。
- **真·端到端（需 docker-compose 起 Kafka）**：`docker compose up -d kafka` → 以 `go build -tags kafka` 起 `cmd/matching`（发布 `exchange.trades`）与 `cmd/settlement`（消费清算）→ 下成交单 → `GET /api/v1/settlement/cleared` 可见该笔清算流水、`/stats` 累计手续费。无 broker 时默认构建退回内存，单测覆盖消费/幂等逻辑。

## 22. Redis 集群级限流接入（T-16，2026-08-15，已完成）

**背景**：docker-compose 声明了 Redis（`redis:7`，:6379），配置 `redis.addr` 已存在，但全项目此前无任何服务连接它；`internal/pkg/middleware/ratelimit.go` 的限流注释明确写着「生产建议用 redis 令牌桶做分布式限流」，且 config 注释标注 `rate_limit_per_sec` 为「单实例每 IP 限流」——多网关/多实例部署时限流各自为政、可被横向绕过。

**改动**：把限流后端抽象为 `redis.RateLimiter`，所有服务的 `Common` 安全中间件改用 Redis 支持的集群级限流（未配置/不可达时自动回退内存，行为不变）：

- `internal/pkg/redis/redis.go`（新增）：`RateLimiter` 接口（`Allow(key,limit,window) bool`）；`New(addr,password,db)`——addr 为空返回 `memLimiter`（纯内存固定窗口，与原行为一致），否则返回 `redisLimiter`；`redisLimiter` 经 Lua 脚本（`INCR` + 首次 `PEXPIRE`）在 Redis 内做原子固定窗口计数，多实例共享同一计数实现集群级限流；**Redis 不可达时降级到内存限流（fail-degraded）**，保证限流在故障期仍生效而非放行。依赖 `github.com/redis/go-redis/v9`（已 `go get` 加入 go.mod）。
- `internal/pkg/middleware/ratelimit.go`：新增 `RateLimitWith(lim redis.RateLimiter, limit, window)`，复用 `Allow` 语义；原 `RateLimit`（本地）保留供回退/单测。
- `internal/pkg/middleware/security.go`：`Common` 内部 `rateLimiter := redis.New(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.DB)`，把中间件列表中的 `RateLimit(limit, time.Second)` 替换为 `RateLimitWith(rateLimiter, limit, time.Second)`。所有服务（gateway/spot/futures/…/admin）因此统一获得集群级限流，无需各自改动。
- `internal/pkg/redis/redis_test.go`（新增）：覆盖内存限流的放行/超额拒绝/窗口重置/空 addr 回退。

**设计要点**：
- 零回归：未配置 `redis.addr` 时 `New` 返回内存实现，`Common` 行为与此前完全一致；配置了但 Redis 宕机时降级内存，限流不失效。
- 集群一致：配置了 Redis 后，同一客户端 IP 在 `rate_limit_per_sec` 窗口内的请求数在所有网关/服务实例间共享计数，横向扩展不再绕开限流。
- 一致性原语：Lua 脚本保证 `INCR` 与过期设置的原子性，避免竞态下窗口计数失真。

**验证**：
- `go build/vet/test ./...` 全绿（新增 redis 单测通过）。
- **真·集群限流（需 `docker compose up -d redis`）**：多实例网关共享 Redis 计数，单 IP 在 `rate_limit_per_sec` 内总请求数被全局约束；Redis 停服时自动回退内存限流（单测覆盖降级路径的逻辑等价性）。

## 23. InfluxDB 行情 K 线持久化接入（T-16，2026-08-15，已完成）

**背景**：docker-compose 声明了 InfluxDB（`influxdb:2.7`，:8086）但此前从未接入；行情服务 `internal/market` 的 K 线历史仅保存在内存环形缓冲（`klineCap=500`，`internal/market/market.go:25`），进程重启即丢失，且 `/api/v1/market/klines?limit=` 超出 500 根后无法回取更早历史——无法支撑长周期技术分析。

**改动**：把 K 线持久化抽象为 `influxdb.CandleStore`，行情服务在每根 K 线收盘时异步落盘，并在回取超内存上限时从 InfluxDB 补足更早历史（未配置/不可达时降级内存，行为不变）：

- `internal/pkg/influxdb/influxdb.go`（新增）：`Candle` 持久化表示（字段与 `market.Candle` 对齐，独立定义以避免 `market<->influxdb` 循环依赖）+ `CandleStore` 接口（`Write(ctx, Candle) error` / `Query(ctx, symbol, interval, start, end, limit)` / `Close()`）；`New(url, token, org, bucket)`——url 为空返回 `memStore`（内存环形，单测确定性实现），否则返回 `influxStore`（经 `github.com/influxdata/influxdb-client-go/v2` 写入 measurement `kline`：symbol/interval 为 tag，OHLCV 与买卖拆分量为 field，`OpenTime` 为时间戳）。依赖已 `go get` 加入 go.mod。`Query` 用 Flux（`range`+`filter`+`pivot`+`sort`+`limit`）回读并按 `_time` 还原 `Candle`。
- `internal/market/market.go`：新增可选字段 `Store influxdb.CandleStore` + `SetCandleStore`（启动期装配一次，之后不变更，避免与后台写入竞争）；`pushHistory`（K 线收盘处）在追加内存环形后**异步**（goroutine + 3s 超时）拷贝落盘；扩展 `Klines`——当 `limit>klineCap` 且 `Store!=nil` 时，以内存最早根的 `OpenTime` 为右开区间向 `Store` 回取 `limit-len(out)` 根更早历史并合并（2s 超时），查询失败静默降级为内存结果（fail-degraded）；新增 `Close()` 释放 `Store`。
- `internal/market/server.go`：`NewServer` 按 `cfg.InfluxDB.URL` 装配持久化层（`influxdb.New(...)`），并记日志区分「influxdb / in-memory only」；`Server.Close` 先 `market.Close()` 再取消上下文。
- `internal/pkg/config/config.go` + `configs/config.yaml`：新增 `influxdb`（url/token/org/bucket）；默认 `url: ""`，即仅内存、不连接 InfluxDB（fail-degraded），配置示例见 yaml 注释（docker-compose 的 :8086 实例）。

**设计要点**：
- 零回归：`influxdb.url` 为空时 `Store` 为 nil，`pushHistory`/`Klines` 走原内存路径，行为与改动前完全一致；`TestHandleKlines` 等既有单测不受影响。
- 故障可降级：落盘为 best-effort（后台 goroutine，错误仅记录），回取失败静默回退内存，行情服务不因 InfluxDB 抖动而不可用。
- 无循环依赖：`influxdb` 包不反向依赖 `market`，两者通过字段对齐的转换函数 `toInfluxCandle`/`fromInfluxCandle` 桥接。
- 落盘异步、读取带超时：写入在锁外后台进行（不在 `pushHistory` 持锁路径做网络 I/O）；读取 2s 超时避免阻塞 `/klines` 请求。

**验证**：
- `go build/vet/test ./...` 全绿（新增 influxdb 单测 + market 扩展 Klines 单测通过）。
- **真·持久化（需 `docker compose up -d influxdb` + 配置 influxdb.url/token/org/bucket）**：起 `cmd/matching`（发布 `exchange.trades`）→ `cmd/market` 消费聚合 → 已收盘 K 线写入 InfluxDB；重启 `cmd/market` 后 `GET /api/v1/market/klines?symbol=BTCUSDT&interval=1m&limit=1000` 可回取内存环形之外的更早历史。无 InfluxDB 时默认内存路径与单测覆盖合并/降级逻辑。

## 24. Elasticsearch 成交检索接入（T-16，2026-08-15，已完成）

**背景**：docker-compose 声明了 Elasticsearch（`elasticsearch:8.13.0`，:9200）但此前从未接入；行情服务的成交仅保存在内存环形缓冲（`recentTradesCap=100`，`internal/market/market.go:23`），进程重启即丢失，且 `/api/v1/market/trades` 只能看最近 100 笔——无法按 symbol/买卖方向/时间窗检索历史成交。

**改动**：把成交检索抽象为 `es.TradeIndexer`，行情服务在每笔成交到达时异步索引，并提供检索端点（未配置/不可达时降级内存，行为不变）：

- `internal/pkg/es/es.go`（新增）：`TradeDoc` 索引文档（字段对齐 `mq.TradeEvent`，附加 `value=price*qty` 与确定性 `id`）+ `TradeQuery`（Symbol/Side/From/To/Limit）+ `TradeIndexer` 接口（`Index`/`Search`/`Close`）；`New(url, index)`——url 为空返回 `memIndexer`（内存实现，单测确定性实现），否则返回 `esIndexer`（经 `github.com/elastic/go-elasticsearch/v8` 写入 index `trades`：symbol/taker_side 为 keyword 便于精确过滤，ts 为时间窗检索与排序字段）。依赖已 `go get` 加入 go.mod。`Search` 用 ES DSL（`bool.filter` 组合 term/range，`ts` 降序，`size`）回读。`doc id` 以 FNV-64a 对成交关键字段哈希得到，保证 ES 重试/at-least-once 下幂等（同笔成交仅存一份）。
- `internal/market/server.go`：`Server` 增加可选 `indexer es.TradeIndexer`；`NewServer` 按 `cfg.ES.URL` 装配（记日志区分「elasticsearch / in-memory only」）；`applyTrade` 在聚合与 WS 广播后**异步**（goroutine + 3s 超时）把成交索引入 ES（best-effort，失败仅 `log.Warn` 不阻断行情）；新增路由 `GET /api/v1/market/trades/search`。
- `internal/market/server.go:handleTradeSearch`：按 `symbol`/`side`/`from`/`to`/`limit` 检索成交历史（ES 路径按 ts 降序返回全量历史）；**未配置 ES 时降级**为内存近期成交（按 symbol 过滤；无 symbol 则 400），保证检索端点始终可用（fail-degraded）。
- `internal/pkg/config/config.go` + `configs/config.yaml`：新增 `es`（url/index，index 空用默认 `trades`）；默认 `url: ""`，即仅内存、不连接 ES（fail-degraded），配置示例见 yaml 注释（docker-compose 的 :9200 实例）。

**设计要点**：
- 零回归：`es.url` 为空时 `indexer` 为 nil，`applyTrade` 跳过索引、`handleTradeSearch` 走内存降级，行为与改动前一致；既有单测不受影响。
- 故障可降级：索引为 best-effort（后台 goroutine，错误仅日志），检索失败（ES 不可达）返回 500 由调用方处理；未配置则直接内存降级，行情服务不因 ES 抖动而不可用。
- 幂等：FNV-64a 确定性 doc id，重复成交仅落一份；内存实现同键覆盖。
- 落盘异步、读取带超时：索引在 `applyTrade` 锁外后台进行（不阻塞行情聚合与 WS 广播）；检索走请求 context。

**验证**：
- `go build/vet/test ./...` 全绿（新增 es 单测 + market 索引/降级单测通过）。
- **真·检索（需 `docker compose up -d elasticsearch` + 配置 es.url/index）**：起 `cmd/matching`（发布 `exchange.trades`）→ `cmd/market` 消费聚合并索引 → `GET /api/v1/market/trades/search?symbol=BTCUSDT&side=buy&limit=50` 可检索历史成交（含重启前的）；无 ES 时默认内存降级路径与单测覆盖索引触发/降级逻辑。

## 25. Admin 提币审核接真实后端（闭合 §19 资金安全缺口，2026-08-16，已完成）

**背景**：§19 管理后台的「充值提币 CRUD 为内存态，应后续对接……真实持久化（数据库/链上网关）」中，提币审核是资金安全关键路径缺口——`approveWithdrawal`/`rejectWithdrawal` 此前只把审批结果写内存会话态 `wdApprovals`，**不调用上游**，管理员的批准/拒绝不会真正放行或退回链上提现。其余 admin 模块（风控快照/对账→futures、用户 CRUD→user、交易对/公链/币种→CatalogStore MySQL、通知→notification、服务健康→探活）此前已接真实后端，本次仅补齐提币审核这一环。

**改造**：让管理员审核真正驱动链上放行/退回。

- `internal/ledger/ledger.go`：新增 `FinalizeWithdrawHoldForce(id)`——与 `FinalizeWithdrawHold` 仅差「不做冷静期守卫」，专供管理员审批放行（冷却期是防用户误操作的，不适用于管理员显式授权放行）。
- `internal/futuresapi/handler_wallet.go`：
  - 抽取私有 `finalizeHold(id, requireCooling)` 复用「hold 校验 → 链上广播 → 账本划出」逻辑；`requireCooling` 为真（用户端 `finalize`）保留冷静期守卫，为假（管理员 `approve`）调 `FinalizeWithdrawHoldForce` 跳过冷却。
  - 新增 `handleWithdrawApprove`（`POST /api/v1/futures/wallet/withdraw/approve/:hold_id`，跳过冷却 `FinalizeWithdrawHold` 放行）与 `handleWithdrawReject`（`POST /api/v1/futures/wallet/withdraw/reject/:hold_id`，`CancelWithdrawHold` 退回冻结）。路径用 `approve/:hold_id` 形式，避免与 `withdraw/` 下既有静态兄弟段冲突（httprouter 不允许同位置静态段与参数段共存）。futures 整体受 `middleware.Auth` 保护、不强制 role，admin 自签 token 可调用，与现有 admin→futures 代理一致。
- `internal/adminapi/handlers.go`：
  - `listWithdrawals` 由「已广播历史 `/wallet/withdraws`」改为「待审核 hold 队列 `/wallet/withdraw/holds`」（含真实 `hold_id`、状态、冷却期）；维护 `wdByHoldID`（前端 stableID→futures hold_id）锚点，并把 `finalized/cancelled` 映射为 `approved/rejected` 供列表回显；上游不可达仍可降级为内存示例。
  - `approveWithdrawal`/`rejectWithdrawal` 改为先由 `wdByHoldID` 反查 `hold_id`，调 futures 的 `approve`/`reject` 端点真正落地，成功回写本会话审批结果；上游失败返回 502（不再只写内存）。移除原仅写内存的 `markWithdrawal`。
  - 删除已无引用的 `futuresWithdraws` 死类型。
- `internal/adminapi/store.go`：`Store` 新增 `wdByHoldID map[int64]string`（stableID→hold_id）并在 `NewStore` 初始化。
- `internal/adminapi/server.go`：提币审核路由挂 `middleware.RequirePerm(PermWithdrawApproval)`（权限字典 `rbac.go:38` 已定义，super_admin 自动获得）；前端 `web-admin` 的提币审核按钮传 stableID，无需改动。

**验证**：
- 新增 `internal/futuresapi/handler_wallet_test.go`：`TestWithdrawApproveSkipsCooling`（冷却期内用户端 `finalize` 被拒 409、管理员 `approve` 跳过冷却成功放行且 hold 置 finalized）、`TestWithdrawReject`（拒绝退回冻结、hold 置 cancelled）、`TestWithdrawApproveUnknown`（未知 hold→404）。
- 新增 `internal/adminapi/handlers_withdraw_test.go`：用 httptest 模拟 futures 的 holds/approve/reject 端点，验证 `listWithdrawals` 把 finalized hold 映射为 `approved`、pending 映射为 `pending`；`approve` 真正 POST futures `approve/:hold_id` 且列表回显 `approved`；`reject` 真正 POST futures `reject/:hold_id`；上游 500 时 admin 返回 502。
- `go build/vet/test ./...` 全绿（无回归）。

## 26. Admin 用户列表余额接真实后端（闭合 §19 用户/账户已知缺口，2026-08-16，已完成）

**背景**：§19 用户与账户管理模块的「余额」字段此前在 `listUsers` 中**硬编码为 0**（`Balance: 0`），管理后台看到的用户余额永远是 0，无法反映真实资金状况。本次把它改为从 futures 钱包实时拉取 USDT 可用余额，并对上游不可达做 fail-degraded（降级回 0，不报错）。

**改造**：

- `internal/adminapi/handlers.go`：
  - `listUsers` 由 `Balance: 0` 改为对每个用户调 `s.enrichBalance(ctx, &au)` 填充真实余额。
  - 新增私有 `enrichBalance(ctx, *AdminUser)`：`GET /api/v1/futures/wallet/balance?user_id=<id>&asset=USDT`，取 `Available` 填入 `u.Balance`；上游不可达或该用户无钱包账户时，保持 `Balance: 0`（降级，不阻断列表）。

**验证**：
- 新增 `internal/adminapi/handlers_withdraw_test.go` 的 `TestAdminUsersBalanceEnriched`：用 httptest 模拟 futures 的 `/balance` 端点（user 1001→125000.5、user 1002→3400），验证列表返回的用户余额被正确富集、不再是 0。
- `go build/vet/test ./...` 全绿（无回归，含 §25 全部用例与本轮新增用例，adminapi 测试全绿）。

## 27. 链上充提 RPC 可插拔脚手架（T-03 链上 RPC 半边，2026-08-16，已完成）

**背景**：§18.1 / §25 把提币审核接真实后端（管理员放行时调用 ledger 链上广播、释放冻结）。但底层链上充提网关此前只有 `MockWithdrawGateway` / `MockChainGateway`（离线模拟、本地生成**模拟 TxHash**），接口层面虽已是 `WithdrawGateway` / `DepositGateway` 且支持真实 RPC 替身，却缺少真实节点实现——生产需要把提现**广播**到节点取回真实 TxHash、并把充值**监听/回调**到节点检测入账，链上记录才能与内部事件对账。T-03 链上 RPC 半边此前阻塞于「依赖外部节点 + 合规」（§1 表格标注「仍阻塞」）。本次以「**配置驱动 + 未配置降级**」方式落地可插拔脚手架（与 §18.1 同一原则）：生产填真实节点 RPC 即生效、未配置/节点宕机自动回退模拟（fail-degraded），无外部节点也能跑。本轮先落地**提现广播**（§27 初版），再补齐**充值回调**（§27 续）——两者共用同一 `ChainRPCConfig` 与 `JSONRPCClient`。

**设计**：

- `internal/settlement/withdraw_rpc.go`（新增）：
  - `ChainRPCConfig`：`Enabled`（是否启用真实广播）、`Endpoints`（链名 ETH/BTC/TRON → RPC URL）、`Required`（`required_confirmations` 阈值，<=0 用默认 2）、`PollSec`（确认轮询间隔）、`WatchAddresses`（充值监听「地址→用户」）、`HotWallet`（离线签名边界配置）。
  - **真实节点 URL 的部署注入**：含 API key 的节点 URL 与离线签名私钥属敏感信息，不写进 `configs/config.yaml`；`config.Load` 支持环境变量覆盖（`CHAIN_RPC_ENABLED` / `CHAIN_RPC_ENDPOINT_ETH|BTC|TRON` / `CHAIN_RPC_REQUIRED_CONFIRMATIONS` / `CHAIN_RPC_POLL_INTERVAL_SEC` / `HOT_WALLET_ENABLED|HOT_WALLET_SIGNER_TYPE|HOT_WALLET_SIGNER_BACKEND|HOT_WALLET_SIGNER_KEY|HOT_WALLET_ETH_CHAIN_ID|HOT_WALLET_ETH_GAS_PRICE_WEI|HOT_WALLET_ETH_GAS_LIMIT`），与 `AUTH_SECRET` 同一模式；未设置则沿用 YAML 默认值（fail-degraded）。各链端点格式：ETH 自建 `http://host:8545` 或 Infura/Alchemy（key 嵌路径）、BTC `http://<rpcuser>:<rpcpass>@host:8332`（URL userinfo 自动转 Basic Auth）、TRON java-tron `http://host:8090` 或 TronGrid `https://api.trongrid.io`。`.env.example` 提供了完整模板。
  - `ChainRPCClient` 接口：`Broadcast(ctx, chain, to, amount)`（节点侧签名广播，回退路径）+ `SendRaw(ctx, chain, rawHex)`（广播已离线签名的原始交易，离线签名主路径）。
  - `JSONRPCClient`：`ChainRPCClient` 的通用 JSON-RPC 2.0 实现——`Broadcast` 按链映射节点方法（ETH `eth_sendTransaction` / BTC `sendtoaddress` / TRON `wallet/triggersmartcontract`），`SendRaw` 按链映射（ETH `eth_sendRawTransaction` / BTC `sendrawtransaction`；TRON 脚手架返回错误待生产补全），负责协议收发与 `result` 解析取 TxHash。
  - `RPCWithdrawGateway`：嵌入 `MockWithdrawGateway`，**复用其经过验证的确认状态机 / 孤块回滚 / 查询能力**，广播环节支持两路径：① 配了 `Signer` → 先离线签名再 `SendRaw` 广播原始交易（私钥不出域）；② 无 `Signer` 或签名/广播失败 → 回退节点侧 `Broadcast`（fail-degraded）；节点仍不可达则回退模拟广播。
  - `NewWithdrawGateway(conf)`：按配置选择——`Enabled && len(Endpoints)>0` 返回 `RPCWithdrawGateway`（真实广播 + 真实确认轮询 + 可选离线签名边界），否则返回 `MockWithdrawGateway`。`required` 缺省为 2。
- `internal/settlement/withdraw_rpc_test.go`（新增）：
  - `TestNewWithdrawGatewayDisabledReturnsMock`：未启用时回退 Mock、返回本地模拟哈希、行为与改动前一致（零回归）。
  - `TestRPCWithdrawGatewayInjectsRealHash`：配置 RPC 客户端且广播成功时，内部事件采用节点返回的真实 TxHash，其余字段仍由状态机填充。
  - `TestRPCWithdrawGatewayFallsBackOnClientError`：RPC 返回错误时自动回退模拟广播（保证无节点可运行）。
  - `TestNewWithdrawGatewayEnabledUsesRPC`：启用且配端点时工厂产出 `*RPCWithdrawGateway` 且满足 `WithdrawGateway` 契约（编译期接口检查）。
- `internal/futuresapi/server.go`：
  - `Server.chainWithdraw` 字段类型由 `*settlement.MockWithdrawGateway` 改为 `settlement.WithdrawGateway`（接口），`startChainWatchers` / 提现受理 / 回滚 / 历史查询全部经接口调用，零回归。
  - `NewServer` 新增 `chainRPC settlement.ChainRPCConfig` 入参，并以 `settlement.NewWithdrawGateway(chainRPC)` 装配（默认 `enabled:false` 走 Mock，行为与改动前完全一致）。
- `cmd/futures/main.go`：`NewServer` 调用透传 `cfg.Settlement.ChainRPC`。
- `internal/pkg/config/config.go`：`Config.Settlement` 新增 `ChainRPC settlement.ChainRPCConfig`（config 已 import oracle，再 import settlement 无循环依赖）。
- `configs/config.yaml`：`settlement.chain_rpc`（`enabled:false` + ETH/BTC/TRON 空端点 + `required_confirmations:2` + `poll_interval_sec:2` + `watch_addresses` 示例）。

**设计（充值回调，§27 续）**：充值与提现方向相反——提现由交易所**主动广播**，充值由交易所**被动监听**观察地址的入账。脚手架复用 `MockChainGateway` 确认状态机，把「充值来源」从仅 `SubmitDeposit`（演示/显式注入）扩展为可经真实节点 RPC **轮询监听**入账并喂入确认状态机；未配置/无观察地址时退化为纯 Mock（行为不变）。

- `internal/settlement/deposit_rpc.go`（新增）：
  - `DepositWatch`：`{Chain, Address, UserID, Asset}`，充值监听的一条观察项（生产由热钱包/地址派生服务生成「地址→用户」映射，配进 `ChainRPCConfig.WatchAddresses`）。
  - `DepositScanner` 接口：`Scan(ctx) (<-chan DepositEvent, error)`——抽象链上充值监听源（生产直连节点轮询/订阅；单测可注入内存假实现验证「扫描→确认状态机」链路）。
  - `JSONRPCDepositScanner`：`DepositScanner` 的通用 JSON-RPC 实现，复用 `withdraw_rpc.go` 的 `JSONRPCClient` 收发协议；按 `watch` 列表轮询各链节点，命中观察地址的入账解析为 `DepositEvent` 去重后推送。节点不可达/链不支持仅跳过该次扫描（fail-degraded）。各链解析：
    - `scanETH`：用 `eth_getLogs` 拉取观察地址的 Transfer 日志，解析 `value`(wei→amount)。
    - `scanBTC`：用 `listsinceblock` 解析收款（`category=receive` 且地址匹配）。
    - `scanTRON`：用 TronGrid 风格 REST `GET /v1/accounts/{address}/transactions/trc20?contract_address={token}` 拉取 TRC20 转账，仅保留 `to==观察地址` 的入账，按 `token_info.decimals` 把 `value`(最小单位)缩放为 amount；`DepositWatch.Token` 指定合约地址，空则默认主网 USDT-TRC20 合约（`TR7NHqjiehqjqTD9QgQsrQUDsV7qxXWm1f`）。`JSONRPCClient` 新增 `get`（GET）与之配套。
  - `RPCDepositGateway`：嵌入 `MockChainGateway`，**复用其确认状态机 / 孤块回滚 / 查询能力**，新增 `StartScan(ctx)`——经 `DepositScanner` 从节点监听入账、喂入确认状态机（经 `SubmitDeposit`）；无扫描器则为 no-op（等价 Mock）。`DepositGateway` 接口据此新增 `StartScan`（Mock 端为 no-op）。
  - `NewDepositGateway(conf)`：按配置选择——`Enabled && len(Endpoints)>0 && len(WatchAddresses)>0` 返回 `RPCDepositGateway`（真实扫描 + 模拟确认），否则返回 `MockChainGateway`。
- `internal/settlement/deposit_rpc_test.go`（新增）：
  - `TestNewDepositGatewayDisabledReturnsMock`：未启用时回退 Mock、`SubmitDeposit` 行为不变（零回归）。
  - `TestRPCDepositGatewayFeedsScannedDeposit`：配置扫描器时，节点监听到的充值经 `StartScan` 喂入确认状态机，`Tick` 后达 `DepositCredited`（证明「真实扫描→状态机」链路打通）。
  - `TestNewDepositGatewayEnabledUsesRPC`：启用且配 endpoints + watch_addresses 时工厂产出 `*RPCDepositGateway` 且满足 `DepositGateway` 契约。
  - `TestJSONRPCDepositScannerETH`：用 httptest 模拟节点 `eth_getLogs` 响应，验证真实扫描器把命中观察地址的 Transfer 日志解析为 `DepositEvent`（`value 0x0de0b6b3a7640000 = 1e18 wei = 1.0`）。
  - `TestJSONRPCDepositScannerTRON`：用 httptest 模拟 TronGrid TRC20 事件响应，验证仅保留 `to==观察地址` 的入账（value 1000000/1e6=1.0），忽略 `to` 不匹配的转出。
- `internal/futuresapi/server.go`：`Server.chainGateway` 字段类型由 `*settlement.MockChainGateway` 改为 `settlement.DepositGateway`（接口），`NewServer` 在以 `cfg.Settlement.ChainRPC` 装配 `NewWithdrawGateway` 的同时也用 `NewDepositGateway(chainRPC)` 装配充值网关，并 `Start()` + `StartScan(ctx)`（ctx 随 `Server.ctx` 取消清理）；`startChainWatchers` / 充值受理 / 回滚 / 重组查询全部经接口调用，零回归。

**设计（真实区块确认轮询，§27 续二）**：把「确认数推进」从模拟「每 tick +1」替换为真实链上确认数查询——这是 T-03 收尾的关键一环，使 Credited 真正与链上安全确认数挂钩（替代与区块高度脱钩的模拟递增）。

- `ConfirmSource` 接口（`withdraw_rpc.go`）：`Confirmations(ctx, chain, txHash) (int, error)`——抽象单笔交易的链上当前确认数；单测可注入内存假实现验证「真实确认数→状态机推进」。
- `JSONRPCClient.Confirmations`：按链向节点查询确认数——ETH `eth_blockNumber` 取链头、`eth_getTransactionByHash` 取交易所在区块，二者之差 +1；BTC `getrawtransaction(txid, true)` 的 `confirmations` 字段；TRON 经 TronGrid REST 取链头 `/v1/blocks` 与交易 `/v1/transactions/{id}` 所在区块号，二者之差 +1（与 ETH/BTC 同一 `ConfirmSource` 机制）。
- `MockChainGateway` / `MockWithdrawGateway` 各新增可选 `confirmSource ConfirmSource` 字段与 `realConfirmations(ctx, chain, txHash, current) int` 辅助：有源且查询成功→用真实确认数推进；否则（无源/节点宕机）→回退模拟 `current+1`（fail-degraded）。`tick()` 的充值 `Pending` 分支、提现 `Broadcasting` 分支据此推进确认数并达标置 Credited。
- `NewWithdrawGateway` / `NewDepositGateway`：启用且配端点时把 `*JSONRPCClient` 同时设为 `confirmSource`（真实确认轮询）与广播/扫描客户端；确认轮询间隔尊重 `PollSec`（缺省 2s）。未配置则 `confirmSource` 为 nil，退化为纯 Mock（行为不变，零回归）。
- 测试（`withdraw_rpc_test.go` / `deposit_rpc_test.go` 续）：`TestRPCWithdrawGatewayUsesRealConfirmations` / `TestRPCDepositGatewayUsesRealConfirmations`（注入确定确认数，达标即 Credited）、`TestRPCWithdrawGatewayFallsBackOnConfirmError` / `TestRPCDepositGatewayFallsBackOnConfirmError`（确认源不可达自动回退模拟 +1）、`TestJSONRPCClientConfirmationsETH`（httptest 模拟 `eth_blockNumber=0x10`+交易 `0x9`→确认数 8）/ `TestJSONRPCClientConfirmationsBTC`（取 `confirmations` 字段）/ `TestJSONRPCClientConfirmationsTRON`（返回错误）。

**关键设计点**：

- 确认数推进已由真实链上确认数驱动（替代模拟每 tick +1）：`MockWithdrawGateway` / `MockChainGateway` 通过 `confirmSource` 在 `tick()` 中查询节点，达标即 Credited；节点不可达自动回退模拟递增（fail-degraded），无外部节点也能跑。`PollSec` 同时驱动确认轮询与充值扫描间隔。
- 边界清晰：真实签名 / 热钱包 / Nonce 管理在节点侧或离线签名层；本 JSON-RPC 客户端仅负责协议收发与哈希/确认数解析，属脚手架边界，不引入密钥管理。
- fail-degraded 双保险：未启用、或配置了但节点宕机（`Broadcast`/`Confirmations` 返回错误），均自动回退模拟（广播/确认），无外部节点也能跑，与 §18.1「配置驱动 + 未配置降级」一致。

**验证**：

- `go build/vet/test ./...` 全绿（本轮新增确认轮询相关用例 7 个；`withdraw_rpc_test.go` 共 13 用例、`deposit_rpc_test.go` 共 6 用例；`futuresapi` / `settlement` 既有测试零回归——`handler_wallet_test.go` 经 `WithdrawGateway` / `DepositGateway` 接口验证受理/回滚/历史查询路径不变）。
- 真实广播路径经 `withdraw_rpc_test.go` 的 `fakeRPCClient`（注入确定哈希 / 注入错误）覆盖「真实哈希注入」与「错误回退」；真实充值扫描路径经 `deposit_rpc_test.go` 的 `TestJSONRPCDepositScannerETH`（httptest 模拟 `eth_getLogs`）覆盖「节点监听→解析入账」；真实确认轮询经 `Confirmations` 的 ETH/BTC httptest 用例与注入确认数用例覆盖「节点确认数→状态机推进」与「错误回退」；完整端到端（连真实节点广播/监听/确认）依赖生产节点 URL + 热钱包/离线签名，原型不连真实链。

**剩余（T-03 收尾，生产接入最后一环）**：
- 真实节点、HSM/KMS 安全模块接入（合规约束，依赖外部节点 + 硬件）；本仓库已内置**软件等价真实签名器**（`realSigner`：真实 secp256k1 + Keccak-256），并把**实际签名原语抽离为可替换的 `KeySigner` 后端**（§27 续九）：生产仅需用 `RegisterExternalSigner` 注册真实 HSM/KMS 后端（按 keyID 选择），私钥永不离开安全模块，其余 settlement 代码不变；未注册则回退节点侧签名广播（fail-degraded）。
- 以上与 §1 T-03「依赖外部节点+合规」一致，仍为生产最后一环；提现广播、充值回调、真实区块确认轮询、TRC20 充值事件过滤、**TRC20 确认数查询**、**热钱包离线签名边界**、**真实 HSM 签名器（ETH + BTC）**的**脚手架/软件实现**均已落地（配置驱动 + 未配置降级），生产填真实节点/安全模块即生效。BTC 真实签名已覆盖 P2PKH（legacy）+ P2WPKH（segwit）的 **UTXO 选择 / 找零 / SIGHASH_ALL 真实 secp256k1 签名**（§27 续七）；**真实节点 UTXO 拉取已接入网关 `SubmitWithdraw` 主路径**（`NewWithdrawGateway` 配置 BTC 端点时经 `NewRPCUTXOSource` 注入 `UTXOSource`，BTC 提现走「`listunspent` → 离线签名 → `sendrawtransaction`」而非节点侧 `sendtoaddress`）；**ETH 真实 Nonce/Gas 管理已接入**（`NewWithdrawGateway` 配置 ETH 端点时经 `SignerSources.ETHState` 注入 `ETHStateSource`，`realSigner.resolveNonce` 首次向节点查 `eth_getTransactionCount("pending")` 后本地递增、`signETH` 优先用节点 `eth_gasPrice` 再回退配置默认值，见 §27 续八）；**TRON 带签名合约调用已落地**（§27 续十：`realSigner.signTRON` 经 `TRONStateSource` 取参考区块，做 protobuf raw_data 序列化 + `SHA256(raw_data)` 摘要 + secp256k1 可恢复 65 字节签名，覆盖 TransferContract 与 TriggerSmartContract，经 `SendRaw(TRON)` POST `/wallet/broadcasttransaction` 广播；未注入 TRONState 仍回退节点侧广播）。

**设计（真实 HSM 签名器，§27 续六）**：在 §27 续五「离线签名边界」基础上，把 `"hsm"`/`"kms"` 从占位推进为**真实 secp256k1 ECDSA 签名**（软件等价实现，私钥仅驻留本进程内存安全域），产出可直接 `eth_sendRawTransaction` 广播的原始交易 hex——私钥不出域的边界与 fail-degraded 回退不变。

- `Signer` 接口（`withdraw_rpc.go`）+ `UnsignedTx`：新增 ETH 真实签名所需字段 `Nonce`/`GasPriceWei`/`GasLimit`/`ChainID`/`Data`；`Sign` 在签名器域内完成「RLP 编码 → Keccak-256 摘要（自研 `internal/pkg/keccak`，以太坊 0x01 填充变体，非 SHA3）→ secp256k1 可恢复 ECDSA 签名（decred `ecdsa.SignCompact`）→ 低 S 规范化 → EIP-155(或 legacy) v → RLP 编码完整交易」全流程。
- `HotWalletConfig` + `ChainRPCConfig.HotWallet`：`SignerType` 支持 `"stub"`（演示边界）、`"hsm"`/`"kms"`（真实签名）；新增 `SignerKey`（软件演示私钥 hex，生产由 HSM/KMS 注入、不落配置）与 `EthChainID`/`EthGasPriceWei`/`EthGasLimit`（ETH 真实签名兜底默认值）。`NewSigner`：`"hsm"`/`"kms"` 私钥有效返回 `realSigner`、私钥缺失/非法返回 nil → 回退节点侧 `Broadcast`（fail-degraded）。
- `realSigner`（`hsm_signer.go`）：已实现 **ETH（legacy / EIP-155）**、**BTC（P2PKH legacy + P2WPKH segwit）** 与 **TRON（TransferContract / TriggerSmartContract）** 真实签名（TRON 见 §27 续十）；`Sign` 按链分派 `signETH`/`signBTC`/`signTRON`。各链未配置对应外部状态源时回退节点侧广播（fail-degraded）。
- `internal/pkg/keccak`：自研 Keccak-256（纯 Go，移植自 Keccak 参考实现的 `keccakF1600` 内核 + 以太坊 0x01 填充），经已知测试向量（`""`/`"abc"`/`"hello"`）验证；用于 ETH 交易摘要与地址派生，避免依赖外部 `cskr/keccak`（网络受限下载失败）。
- `RPCWithdrawGateway`：有 `realSigner` 时 `SubmitWithdraw` 走「`Signer.Sign`（真实签名）→ `SendRaw`」取回真实 TxHash；签名/广播失败自动回退节点侧 `Broadcast`（fail-degraded）；节点仍不可达回退模拟广播。
- 测试：`hsm_signer_test.go`/`keccak_test.go`——`TestRealSignerKnownEIP155Vector`（以太坊官方 EIP-155 测试向量精确匹配：`f86c09…3b6d83`）、`TestRealSignerRecoversToAddress`（广播 raw 可恢复出签名者地址，自洽）、`TestNewSignerHSM`（hsm 私钥有效返回 `realSigner`、缺失/非法返回 nil）、`TestRealSignerUnsupportedChain`/`TestRealSignerRequiresGasPrice`（未实现链 / 缺 gasPrice 回退）、`TestRPCWithdrawGatewayUsesRealSigner`（网关经真实签名器 `Sign`→`SendRaw` 广播真实可解析 ETH raw）。
- 生产落地仍需补（合规/外部依赖，非本仓库范围）：接**真实 HSM/KMS** 硬件（经已落地的 `KeySigner` 缝，把 `ecdsa.SignCompact` 替换为安全模块签名调用，私钥永不离开安全域；三链 ETH/BTC/TRON 复用同一 `KeySigner` 后端）、填入真实节点 URL（BTC `listunspent`/ETH `eth_getTransactionCount`/`eth_gasPrice`/TRON `getnowblock`+`broadcasttransaction` 均已就绪）。脚手架 `stubSigner` 仍保留作纯边界演示（标记化 raw，非真实密码学）。

**设计（BTC UTXO 选择 + 找零 + SIGHASH_ALL 真实签名，§27 续七）**：在 §27 续六「真实 HSM 签名器」基础上，把 `ChainBTC` 从「未实现、回退节点侧 `sendtoaddress`」推进为**真实离线签名**——产出可直接 `sendrawtransaction` 广播的原始交易 hex（私钥不出域、fail-degraded 回退不变）。与 ETH 共用同一 `Signer` 接口与 `secp256k1` 私钥域（BTC/ETH 同曲线），地址由同一私钥派生 P2PKH（base58check）与 P2WPKH（bech32）。

- `UnsignedTx`（`withdraw_rpc.go`）新增 BTC 字段：`UTXOs []UTXO`（可选内联未花费输出；为空则签名器经 `UTXOSource` 按自身地址查询）、`ChangeAddress string`（找零脚本/地址，空→回自身 P2WPKH）、`FeeRatePerKB uint64`（sat/kvB，0→默认 1000）。
- `UTXO` / `UTXOSource`（`btc_signer.go`）：`UTXO{TxID, Vout, Amount(float BTC), ScriptPubKey(hex)}`；`UTXOSource.ListUTXOs(ctx, addr)` 按地址查询；`NewRPCUTXOSource(client)` 用 `JSONRPCClient.Call` 调 `listunspent` 实现（节点不可达返回错误 → 回退）。
- `realSigner.signBTC`：① 收集候选 UTXO（内联优先，其次 `utxoSource`）；② 解析收款/找零锁定脚本（P2PKH 经 base58check、P2WPKH 经 bech32，均派生自同一公钥的 `HASH160`）；③ **UTXO 选择**——按金额降序贪心累积到覆盖 `amount + 估算手续费`；④ **手续费估算**——按 `sat/kvB × vbytes`（`vbytes = ceil(weight/4)`），对 P2PKH 输入（~148 B）、P2WPKH 输入（base 41 B + witness 108 WU）、P2PKH/P2WPKH 输出分别精确计 weight，找零低于 dust(546 sat) 则并入手续费不生成找零输出；⑤ **找零**回自身 P2WPKH（或 `ChangeAddress`）；⑥ 逐输入 SIGHASH_ALL 摘要（legacy：`doubleSHA256(version+hashPrevOuts+hashSequence+outpoint+scriptCode+sequence+hashOutputs+locktime+sighashType)`；P2WPKH/BIP143：同结构但含本输入 `value` 且末态仍 `doubleSHA256`），用 `decred ecdsa.SignCompact` 做 secp256k1 签名 + 低 S 规范化 + DER 编码 + 追加 `0x01` SIGHASH_ALL，P2PKH 填入 `scriptSig`、P2WPKH 填入 `witness`，并自校验 `ecdsa.Verify`；⑦ 序列化（含 segwit 标记与 witness）。
- `internal/settlement/ripemd160.go`：自研 RIPEMD-160（纯 Go；Go 1.26 已移除标准库 `crypto/ripemd160`）。用于 BTC `HASH160 = RIPEMD160(SHA256(x))`。双流水线各 80 步、5 轮、左右两线使用不同布尔函数与逆序轮次、不同加性常数（与 `golang.org/x/crypto/ripemd160` 轮次结构对齐），经 8 个官方测试向量（含单块与多块）逐一比对一致。base58/base58check、bech32（BIP173）同样自研。
- 测试（`btc_signer_test.go`）：`TestRIPEMD160Vectors`（8 个官方向量，单/多块）、`TestBTCAddressDerivation`（P2PKH/P2WPKH 派生与反向解析自洽）、`TestSignBTCP2PKHSelection`/`TestSignBTCP2WPKHSelection`（固定私钥 + 内联 UTXO 的**确定性** raw hex；独立解析原始交易并重算每个输入 SIGHASH 摘要，与签名器 digest 交叉比对，再用 `ecdsa.Verify` 校验签名；金额守恒校验；segwit 标记校验）、`TestSignBTCInsufficient`/`TestSignBTCNoUTXOs`（余额不足 / 无 UTXO 回退广播）。
- 真实节点 UTXO 拉取**已接入网关 `SubmitWithdraw` 主路径**（`NewWithdrawGateway` 配置 BTC 端点时经 `NewRPCUTXOSource` 注入 `UTXOSource`；`TestNewWithdrawGatewayBTCUsesOfflineSignerMainPath` 验证 BTC 走 `listunspent`→离线签名→`sendrawtransaction`，复用独立解析 + `ecdsa.Verify` + 金额守恒校验）；BTC 找零地址的热钱包派生（当前默认自身 P2WPKH）为可选增强。签名密码学本身已可生产使用。

**设计（ETH 真实 Nonce / Gas 管理，§27 续八）**：在 §27 续六「真实 HSM 签名器」+ 续七「BTC UTXO」基础上，把 ETH 提现从「Nonce/Gas 由 `UnsignedTx`/配置手工提供」推进为**向节点查询的真实链上状态管理**——避免用过期/默认 0 Nonce 导致重放碰撞、用过时硬编码 Gas 价导致长时间未打包；节点不可达时仍回退配置/默认值（fail-degraded 不变）。

- `ETHStateSource` 接口（`withdraw_rpc.go`）：`Nonce(ctx, chain, account) (uint64, error)`（→ `eth_getTransactionCount(addr, "pending")`）、`GasPrice(ctx, chain) (uint64, error)`（→ `eth_gasPrice`）；`*JSONRPCClient` 实现该接口（`parseHexUint` 解析结果）。`SignerSources{UTXOSource, ETHState}` 统一承载真实签名器的可选外部状态源；`NewSignerWithSource(conf, sources)` 注入；`NewWithdrawGateway` 配置 ETH 端点时 `sources.ETHState = client`、配置 BTC 端点时 `sources.UTXOSource = NewRPCUTXOSource(client)`。
- `realSigner`（`hsm_signer.go`）：新增 `ethState ETHStateSource` 与本地 Nonce 计数器（`mu`/`nextNonce`/`haveNonce`）。`resolveNonce`：① `UnsignedTx.Nonce>0` 显式提供优先；② 否则本地已播种则递增返回；③ 未播种则向节点查 `eth_getTransactionCount("pending")` 作起点并缓存 `nextNonce=n+1`，之后本地递增（避免并发/未确认期间重放碰撞）；④ 节点不可达回退默认 0（fail-degraded）。`signETH`：gasPrice 优先 `UnsignedTx.GasPriceWei`，其次节点 `eth_gasPrice`，再回退 `hot_wallet.eth_gas_price_wei`；均缺则报错回退广播；`Sign` 透传 `ctx` 给 `signETH`/`signBTC`（BTC `utxoSource` 查询亦改用 `ctx`）。
- 测试（`hsm_signer_test.go` + `withdraw_rpc_test.go`）：`TestRealSignerETHResolvesNonceGasFromNode`（节点返回 nonce=12/gas=1gwei，断言查了一次、raw 嵌入该 nonce/gas、且可恢复出签名者地址）、`TestRealSignerETHNonceIncrementsLocally`（节点返回 5，两次签名仅查一次、嵌入 nonce 依次为 5/6）、`TestNewWithdrawGatewayETHUsesNodeNonceGas`（httptest 模拟 `eth_getTransactionCount`/`eth_gasPrice`/`eth_sendRawTransaction`，断言网关走 `SendRaw` 主路径、raw 嵌入 nonce=9 且可恢复）；`TestRealSignerRequiresGasPrice`/`TestRPCWithdrawGatewayUsesRealSigner` 等既有用例零回归。

**设计（接入真实 HSM / KMS 签名，§27 续九）**：在前述「真实签名 + UTXO/Nonce/Gas 管理」基础上，把**实际签名原语**从「进程内软件私钥」抽离为可替换的 `KeySigner` 后端，使离线签名边界能无缝接入真实 HSM/KMS——私钥永不离开安全模块，仅对外暴露「对摘要签名」的能力；生产替换后端即生效，其余 settlement 代码不变。

- `KeySigner` 接口（`hsm_backend.go`）：`SignDigest(ctx, digest [32]byte) (r, s *big.Int, error)` + `Public() *secp256k1.PublicKey`。`realSigner` 不再直接持有私钥，改为持有 `KeySigner`：`signETH` 经 `key.SignDigest` 取 (r,s) 后做低 S 规范化并用 `recoverRecID`（由公钥匹配推导 ETH recovery id，因真实设备通常只返 (r,s) 不含 recovery id）；`signBTCInput` 同样经 `key.SignDigest`（BTC 不需 recovery id）。`secp256k1HalfN` 低 S 规范化两端一致。
- 两种后端：① `softwareKeySigner`（默认，`SignerBackend="software"` 或空，用 `HotWalletConfig.SignerKey` 32 字节 hex 构造，私钥驻留进程内存，开发/演示）；② `externalKeySigner`（`SignerBackend="external"`，由 `NewExternalKeySigner(pub, signFunc)` 构造，`signFunc` 内联调用真实安全模块；公钥由设备导出并传入）。`ParseExternalDERSignature` 把 AWS KMS / Vault / PKCS#11 常返回的 DER 签名解出 (r,s) 供 `signFunc` 使用。
- 配置驱动接入：`HotWalletConfig.SignerBackend`（`"software"` / `"external"`）；`external` 时 `SignerKey` 作为 keyID，从 `RegisterExternalSigner(keyID, backend)` 注册表取生产注入的真实后端——部署期调用一次（如用 AWS KMS Sign / PKCS#11 C_Sign 包装成 `KeySigner` 后注册），无需改动 settlement；未注册则 `newRealSignerWithSource` 报错 → 网关回退节点侧签名广播（fail-degraded）。`UnregisterExternalSigner` 用于热替换/测试清理。
- 测试（`hsm_backend_test.go`）：`TestRealSignerExternalKeySignerMatchesSoftware`（external 后端的 signFunc 用同一软件密钥模拟设备，产出与 `knownVectorRaw` 精确相同，证明替换 KeySigner 不改变任何密码学输出）、`TestRealSignerExternalBTC`（BTC 路径在 external 后端下同样通过独立解析 + `ecdsa.Verify` + 金额守恒校验）、`TestRealSignerExternalUnregisteredFails`（未注册 external 后端报错回退）、`TestParseExternalDERSignature`（DER 往返解出 (r,s)）。

**设计（TRON 带签名合约调用，§27 续十）**：把 `ChainTRON` 从「`realSigner` 返回错误、回退节点侧 `wallet/triggersmartcontract`」推进为**真实离线签名**——产出可直接 `POST /wallet/broadcasttransaction` 广播的已签交易 JSON（私钥不出域、fail-degraded 回退不变）。与 ETH/BTC 共用同一 `Signer` 接口、`secp256k1` 私钥域与 `KeySigner` 后端（TRON/ETH/BTC 同曲线，HSM/KMS 无缝复用）。覆盖两类合约：① `TransferContract`（type=1，TRX 原生转账）；② `TriggerSmartContract`（type=32，TRC20 `transfer(address,uint256)` 合约调用）。

- `tron_signer.go`（新增）：
  - 地址派生：`tronAddressBytes(pub) = 0x41 || HASH160(compressed pubkey)`（复用 BTC 的 `hash160`/`base58check`），`deriveTronAddress(pub)` 出 base58check 人类可读地址；`tronAddressToBytes(addr)` 反向解析（校验 0x41 前缀，非法→回退广播）。
  - 最小化 protobuf 编码（`pbVarintEnc`/`pbTag`/`pbBytes`/`pbVarint`，仅覆盖 raw_data 与合约消息所需字段）：`tronTransferContract`(owner/to/amount-sun)、`tronTriggerContract`(owner/contract/call_value=0/data)、`tronAny`(把内部消息包进 `google.protobuf.Any` 的 `type_url`+`value`)、`tronContract`(type 枚举 + parameter)、`tronRawDataProto`（按字段号升序：ref_block_bytes(1)/ref_block_hash(4)/expiration(8)/contract(11)/timestamp(14)/fee_limit(18)）。
  - `tronTRC20TransferData`：构造 `a9059cbb` + 32 字节地址字（20 字节 HASH160 右对齐，符合 TronWeb ABI 编码）+ 32 字节 uint256 金额字。
  - `signTRON`：`TRONState.NowBlock` 取参考区块（blockID 末 2 字节→`ref_block_bytes`、前 8 字节→`ref_block_hash`、区块时间戳→`timestamp`/`expiration=timestamp+60s`）；构建 raw_data → **`txID = SHA256(raw_data)`**（即签名摘要与链上展示哈希）；`key.SignDigest`（secp256k1，低 S 规范化）→ `recoverRecID` 推导 recID → 拼 65 字节可恢复签名 `[recID+27][R(32)][S(32)]`（与 `ecdsa.SignCompact` 字节布局一致）；组装广播 JSON（`raw_data` 对象 + `raw_data_hex` + `txID` + `signature`），返回该 JSON 字符串由 `SendRaw(TRON)` POST 到 `/wallet/broadcasttransaction`。`ContractAddress` 空→TransferContract（金额转 SUN，1 TRX=1e6）；非空→TriggerSmartContract（FeeLimit 写入 fee_limit）。
- `realSigner`（`hsm_signer.go`）：新增 `tronState TRONStateSource` 字段，`Sign` 增加 `case ChainTRON: return s.signTRON(ctx, tx)`；未注入 `TRONState` 或取参考区块失败 → 返回错误，网关回退节点侧签名广播（fail-degraded）。
- `UnsignedTx`（`withdraw_rpc.go`）：新增 `ContractAddress string` / `FeeLimit uint64`（TRON 真实签名用；非 TRON 忽略）。`TRONStateSource` 接口（NowBlock→区块号/区块哈希 hex/时间戳ms）+ `*JSONRPCClient` 实现（`POST /wallet/getnowblock`，解析 `blockID`/`block_header.raw_data.{number,timestamp}`）；`JSONRPCClient.post` 通用 POST（供 getnowblock / broadcasttransaction 路径式接口）。`SignerSources.TRONState` 在 `NewWithdrawGateway` 配置 TRON 端点时注入。`SendRaw(TRON)` 改为 `POST /wallet/broadcasttransaction` 并解析响应 `txid`（此前返回错误）。
- 测试（`tron_signer_test.go`）：`TestRealSignerTRONTransferContract`（独立重算 `SHA256(raw_data_hex)`==txID、`ecdsa.RecoverCompact` 恢复出签名者公钥、JSON owner_address/金额/ref_block 字段正确）、`TestRealSignerTRONTriggerSmartContract`（data 字段独立重算为 `a9059cbb`+收款地址字+uint256 金额、fee_limit 写入）、`TestRealSignerTRONRequiresState`（无 TRONState 返回错误，fail-degraded）、`TestNewWithdrawGatewayTRONUsesOfflineSigner`（httptest 模拟 `getnowblock`+`broadcasttransaction`，断言网关走离线签名→广播主路径、返回节点 txid 且 txid=SHA256(raw_data) 校验通过）。
- 经此，T-03 链上 RPC 半边**全部生产项（提现广播/充值回调/真实确认轮询/TRC20 事件/ETH+BTC+TRON 真实离线签名/真实 UTXO 拉取/ETH 真实 Nonce·Gas/HSM·KMS KeySigner 缝）脚手架已齐备**，仅余「真实节点 URL + HSM/KMS 硬件注册」为生产部署动作（配置驱动 + 未配置降级）。

**设计（生产 HSM 签名器配置驱动自动注册，§27 续十一）**：在 §27 续九「`KeySigner` 后端抽象 + `RegisterExternalSigner` 注册表」基础上，补上**配置驱动、纯标准库、无外部重依赖**的生产适配器 `remoteHSMKeySigner`，并让 `SignerBackend="external"` 在注册表未命中时按 `HotWalletConfig.HSM` **自动构造并注册**真实后端——免去部署方手写 `RegisterExternalSigner` 调用，真正「改配置即接入生产安全模块」。

- `hsm_kms.go`（新增）：`HSMConfig`（`kind`/`endpoint`/`api_key`/`public_key`，位于 `HotWalletConfig.HSM`）；`remoteHSMKeySigner`（生产 KeySigner 适配，私钥永不离开内部签名服务）：`SignDigest` 把 32 字节 digest `POST` 到内部签名服务，解析返回的 `{r,s}`(hex) 或 `{signature: DER-hex}`（DER 经 `ParseExternalDERSignature` 解出 (r,s)）；`api_key` 作为 Bearer Token 注入 `Authorization` 头；`Public()` 返回设备导出公钥（用于派生地址、推导 ETH recovery id、校验签名）。`NewRemoteHSMKeySigner(pub, endpoint, apiKey)` 构造，`newHSMKeySigner(conf)` 按 `HSMConfig` 分派（当前 `kind="remote-http"`，其它 kind 需自行实现 `KeySigner` 后用 `RegisterExternalSigner` 注入）。
- `HotWalletConfig`（`withdraw_rpc.go`）：新增 `HSM HSMConfig yaml:"hsm"` 字段。`newRealSignerWithSource`（`hsm_signer.go`）的 `case "external","hsm-remote","kms"`：先 `lookupExternalSigner(keyID)`，未命中则 `newHSMKeySigner(conf)` 构造并 `RegisterExternalSigner(keyID, backend)` 后采用；仍失败才报错回退节点侧签名广播（fail-degraded）。
- 真实节点 URL 与 HSM 连接配置**统一走环境变量注入**（与 `AUTH_SECRET` 同模式，不写进 `configs/config.yaml`）：`HSM_KIND`/`HSM_ENDPOINT`/`HSM_API_KEY`/`HSM_PUBLIC_KEY`（见 `config.go` 的 `Load` 覆盖段与 `.env.example`）；`configs/config.yaml` 的 `hot_wallet.hsm` 段仅列字段说明与取值示例。生产设 `HOT_WALLET_SIGNER_BACKEND=external` + 上述 `HSM_*` 即可零代码接入真实签名服务。
- 测试（`hsm_kms_test.go`）：`TestRemoteHSMKeySignerRSPath`/`TestRemoteHSMKeySignerDERPath`（httptest 模拟内部签名服务，分别验证 `{r,s}` 与 DER 两条解析路径，产出与软件后端对同 digest 确定性签名精确一致、且可恢复出配置公钥）、`TestRemoteHSMKeySignerAuth`（api_key 注入 `Authorization: Bearer` 头）、`TestNewHSMKeySignerConfigDriven`（缺 kind/未知 kind/非法公钥/空 endpoint/空公钥 均报错，fail-degraded 回退）、`TestNewRealSignerExternalAutoRegistersHSM`（`SignerBackend="external"` 且注册表干净时按 `HSMConfig` 自动注册真实后端，ETH 签名产出与 `knownVectorRaw` 精确相同，证明配置驱动接入不改变任何密码学输出）。

**设计（部署真实 HSM 签名服务并验证签名，§27 续十二）**：把 §27 续十一 的「配置驱动适配器」补上**可部署的内部签名服务实体**——它持有真实 secp256k1 密钥、对 32 字节 digest 做真实可验证 ECDSA 签名，正是生产里前置真实 HSM/KMS 的通用模式（私钥驻留本服务，对外仅暴露「对摘要签名」端点；换真实安全模块只需改 `signDigest` 内部调用，对外契约不变）。配合 `remoteHSMKeySigner` 形成完整闭环，并补端到端验证，证明「部署真实 HSM 且签名可独立验证归属」。

- `hsm_signing_service.go`（新增）：`SigningService`——`NewSigningService()` 生成新密钥 / `NewSigningServiceFromKey(hex)` 加载已知密钥；`PublicKeyHex()` 导出压缩公钥（运营填入 `HSM_PUBLIC_KEY`）；`SetResponseMode("rs"|"der")` 切换 `/sign` 响应形态；`Handler()` 提供 `POST /sign`（请求 `{"digest":"<64hex>"}`，按 mode 返回 `{"r","s"}` 或 `{"signature":"<DER>"}`）、`GET /pubkey`、`GET /health`。签名数学复用 `softwareKeySigner.SignDigest`（真实 secp256k1 ECDSA，确定性 nonce，产出可被任意节点/钱包独立恢复验证）。
- `cmd/hsm-signing-service/main.go`（新增可部署二进制）：`-addr`/`-key`/`-mode` 三个 flag；启动即打印 `HSM_KIND`/`HSM_ENDPOINT`/`HSM_PUBLIC_KEY` 供运营注入网关环境变量；`POST /sign`、`GET /pubkey`、`GET /health`；`SIGINT/SIGTERM` 优雅退出。把签名服务独立部署后，网关侧 `HOT_WALLET_SIGNER_BACKEND=external` + `HSM_*` 即零代码接入。
- 验证（`hsm_signing_e2e_test.go`，`TestDeployRealHMSSignAndVerify`）：启动真实 `SigningService`（rs 与 der 两种模式各跑一遍），按 `external`+`HSMConfig` 让 `newRealSignerWithSource` **自动注册**真实后端，对 ETH 与 TRON 签名并**独立恢复公钥**验证签名确由该 HSM 密钥签出、地址对齐：
  - ETH：重算待签摘要 `keccak(RLP([nonce,gasPrice,gasLimit,to,value,data,chainID,0,0]))`，用签名 `(r,s)` 反推公钥，断言等于服务导出的公钥（节点据此可恢复发送方地址）；同时断言 `realSigner.address` 等于该公钥派生的 ETH 地址。
  - TRON：`txID==SHA256(raw_data)`；65 字节可恢复签名经 `ecdsa.RecoverCompact(txID)` 恢复出服务公钥；`owner_address` 等于该公钥派生的 TRON 地址。
  - 由此从密码学上证明：签名由该 HSM 密钥签出且有效，私钥始终未离开签名服务（离线签名边界隔离成立）。
  - 部署与运维细则（构建/启动、环境变量、密钥轮换、监控告警、故障排查、回滚降级、安全 checklist）见独立文档 [HSM_DEPLOYMENT.md](HSM_DEPLOYMENT.md)。

**设计（充值网关真实链上 txHash 透传，§27 续十三）**：补上 §27 续「充值回调」里标注的设计观察点——`StartScan` 把扫描到的入账喂入确认状态机时此前未经 `SubmitDeposit` 透传节点返回的真实 `txHash`，网关自生成模拟哈希，导致链上幂等/对账失去真实锚点。新增 `SubmitDepositWithHash(userID, asset, chain, amount, address, txHash)`，供 `StartScan` 在真实扫描后**保留节点真实 `txHash`**（空则回退本地生成）；`pending` 以 `txHash` 为键，重复提交同一真实 `txHash` 天然幂等。`MockChainGateway.SubmitDeposit` 委托 `SubmitDepositWithHash(txHash="")` 复用同一实现，消除重复逻辑。

- `internal/settlement/settlement.go`：`DepositGateway` 接口新增 `SubmitDepositWithHash`；`MockChainGateway` 新增实现（`SubmitDeposit` 改为委托）。
- `internal/settlement/deposit_rpc.go`：`RPCDepositGateway.StartScan` 将扫描事件的 `ev.TxHash` 经 `SubmitDepositWithHash` 透传入账。
- `internal/settlement/deposit_scanner_test.go`：`TestStartScanFeedsGateway` 断言 `pending[0].TxHash == "0xethtx1"`（真实透传）；新增 `TestSubmitDepositWithHash` 覆盖「真实哈希保留 / 空哈希回退 / 重复哈希幂等 / 非法参数拒绝」。

**验证**：`go build/vet/test ./internal/settlement/` 全绿；`go build ./...` 零回归。

**剩余（T-03 收尾，生产部署动作）**：
- 真实节点 URL、HSM/KMS 安全模块硬件接入（合规约束，依赖外部节点 + 硬件）：本仓库已内置**软件等价真实签名器**（`realSigner`：ETH/BTC/TRON 真实 secp256k1 签名）并把**实际签名原语抽离为可替换的 `KeySigner` 后端**。生产接入有两种等价路径：① **配置驱动**（推荐）：`SignerBackend="external"` + 环境变量 `HSM_KIND/HSM_ENDPOINT/HSM_API_KEY/HSM_PUBLIC_KEY` 指向内部签名服务，网关启动时按 `HSMConfig` 自动构造并注册真实后端，**无需手写 `RegisterExternalSigner`**；② **代码注入**：对非常规安全模块（aws-kms / pkcs11 等）用 `RegisterExternalSigner(keyID, backend)` 注册自定义 `KeySigner` 后端。两条路径私钥均永不离开安全模块，其余 settlement 代码不变；未配置则回退节点侧签名广播（fail-degraded）。`UnsignedTx` 的 `Data`/`UTXOs`/`ContractAddress`/`FeeLimit` 等已覆盖 ETH 合约调用 / BTC UTXO / TRON 合约调用的真实签名输入。

**设计（充值/提现事件投递非静默 + 并发安全收尾，§27 续十四）**：在 §27 续「中优先级并发/精度修复」周期（#3/#4/#5/#7）收尾时，补上 emit 投递的非静默化与两个连带并发修复。

- **#4 emit 静默丢弃 credited 事件（中优先级，资金正确性）**：`MockChainGateway.emit` / `emitRollback` 与 `MockWithdrawGateway.emit` / `emitWithdrawRollback` 原先用 `select { case ch<-ev: default: }` 在订阅者背压（channel 满且长期不消费）时**静默丢弃**事件。充值/提现达确认的 `credited` 事件一旦被丢，内部账本永不入账 → 「链上已确认但用户未到账」资金错配且无迹可查。改为**阻塞发送 + 超时上限**：`select { case ch<-ev: case <-time.After(emitSendTimeout): log.Printf("[settlement] ... DROPPED: ...") }`，超时后输出告警（而非静默丢弃）。已入账状态仍持久化在 `g.pending`，运维可经 `Pending()` 对账重放，避免永久丢失。`emitSendTimeout` 由 `const` 改为包级 `var`（默认 `5s`）以便单测调短。
- **#3 Start 重建 `g.stop` 引入的 data race（本周期发现，随 #4 一并修复）**：#3 为修复「Stop 后再次 Start 失效」在 `Start()` 内 `g.stop = make(chan struct{})` 重建 stop；但后台 goroutine 仍读取字段 `g.stop`，与二次 `Start` 的写形成 data race（`-race` 下 `TestStartStopStart` 触发）。改为 goroutine 捕获**局部变量 `stop`** 后仅读局部，不再读 `g.stop` 字段；`MockChainGateway` 与 `MockWithdrawGateway` 的 `Start` 同改。
- **`TestGenerateHelpers` 顺序依赖偶发失败（预存问题，本周期修复）**：多个地址测试经 `SetDepositAddressGenerator` 把全局 `depositAddrGen` 改为捕获的 `prev` 还原，前序用例泄漏的非 nil 生成器会经 `prev` 透传到 `TestGenerateHelpers`（其断言 `GenerateAddress` 回退 mock 的 `ETH` 前缀），导致偶发失败。`setGenForTest` / `TestGenerateAddressFallbackUnconfigured` / `TestConfigureDepositAddresses` / `TestDepositAddrGenConcurrent` 统一改为**还原为已知干净的 `nil`**，消除跨用例泄漏。（注：`TestDRKeyLossRekey` 在 `-count>1` 下因 HSM DR 测试自身非幂等 panic，属预存、与本周期无关，未改动。）

- `internal/settlement/settlement.go`：4 个 emit 函数 `default:` 静默丢弃 → 阻塞+`emitSendTimeout` 超时+`log.Printf` 告警；`emitSendTimeout` 改 `var`；`MockChainGateway.Start` / `MockWithdrawGateway.Start` goroutine 捕获局部 `stop`。
- `internal/settlement/settlement_emit_test.go`（新增）：`TestEmitDeliversCreditedEvent`（credited 事件经 channel 送达，回归投递路径）、`TestEmitBackpressureNotSilent`（背压超时输出 `DROPPED` 告警、且 `Pending()` 仍含 credited 可恢复）。
- `internal/settlement/deposit_address_test.go` / `deposit_address_race_test.go`：全局生成器还原为 `nil`。

**验证**：`go build ./...`、`go vet ./internal/settlement/ ./internal/futuresapi/`、`go test -race -count=1 ./internal/settlement/ ./internal/futuresapi/` 全绿；`TestEmitBackpressureNotSilent` 触发 `DROPPED` 日志印证非静默路径生效。

---

## 28. 业务线落地：链上质押理财 staking（2026-08-18，已完成）

新增业务线 `internal/staking` + `cmd/staking`（`:8097`）。用户质押链上资产获取奖励，复式记账锁定本金于 `SysStaking`、奖励负债记 `SysStakingReward`（账本系统账户 -10/-11）。

- `model.go`：`StakingProduct`/`StakingDelegation`/`StakingReward`（金额一律 `settlement.AssetAmount` 定点）；状态机 `ProductActive/ProductClosed`、`DelegationActive/DelegationUnbonding/DelegationUnbonded`。
- `store.go`/`store_mem.go`/`store_mysql.go`+`store_migrations.go`：`Store` 接口 + 内存/MySQL 实现；迁移版本 `9801`(`ce_staking_products`)/`9802`(`ce_staking_delegations`+`ce_staking_rewards`)，金额以 `BIGINT value + INT decimals` 自包含定点存储。
- `service.go`：`ChainBackend` 可插拔（`MockBackend` 演示）；`Subscribe`（Transfer 用户→`SysStaking` + 链上广播 + 建委托，失败回滚）、`Unbond`（终态幂等短路）、`Release`（校验 `requiredConfirmations=12`，`ledger.Batch` 原子释放本金+奖励）、`Accrue`（后台奖励归集 `SysStaking`→`SysStakingReward`）、`RunLoop`。
- `handler.go`：`/api/v1/staking` 路由组，`middleware.Auth` + 产品创建/奖励归集/全量查询 `AdminGuard`（F4）。
- `cmd/staking/main.go`：装配层（ledger 种子充值、Store 选择、RunLoop、信号退出）。
- 测试（`service_test.go`）：`TestStakingLifecycle`（质押→归集→解押→释放全资金链路，校验余额与 F1/F2/F3/F5）、`TestSubscribeBelowMin`。

> 资金安全：直接复用 ledger 定点复式记账（F2），订阅/释放均走账本原子操作（F3），链上广播失败回滚（F1 纵深），资产白名单校验（F5）。

## 29. 业务线落地：交易机器人 bot（2026-08-18，已完成）

新增业务线 `internal/bot` + `cmd/bot`（`:8098`）。网格/定投/均线策略，代用户向 spot/futures 下单，**资金安全下沉到被代理的 spot/futures 服务**（F1 client_oid 幂等 / F4 token 鉴权），bot 自身不持有私钥或余额。

- `model.go`：`Market`(spot/futures)/`StrategyType`(grid/dca/ma)/`BotParams`(按类型选参，JSON 存储)/`BotStrategy`(含 `UserToken` 授权凭证，`json:"-"` 不外显)/`BotOrder`(含 `ClientOID` 幂等键)。
- `store.go`/`store_mem.go`/`store_mysql.go`+`store_migrations.go`：`Store` 接口 + 内存/MySQL；迁移 `9803`(`ce_bot_strategies`，params JSON 列)/`9804`(`ce_bot_orders`)。
- `service.go`：`PriceSource` 可插拔(`MockPrice`)/`OrderExecutor`(`HTTPExecutor` 调 spot/futures `/order`，携带 client_oid + Bearer token)；`CreateStrategy`(F5 参数校验)/`StartStrategy`/`StopStrategy`(F4 归属校验)/`Run` ticker 循环/`tick`（F1 `client_oid=bot:strategyID:round` 下游去重、F4 用 `UserToken` 代下单、越仓 `MaxPosition` 封顶）。
- `handler.go`：`/api/v1/bot` 路由组，`middleware.Auth` + 全量策略 `AdminGuard`。
- `cmd/bot/main.go`：装配层（`--spot-url`/`--futures-url` 默认指向 :8082/:8084，RunLoop，信号退出）。
- 测试（`service_test.go`）：`TestCreateStrategyF5Validation`（各策略参数边界）、`TestStartStopF4Owner`（越权拒绝）、`TestTickF1IdempotentKey`（client_oid + token 绑定）、`TestTickMaxPositionGuard`（越仓封顶）、`TestRunLoopDrivesActive`。

> 资金安全：bot 不碰账本，全部资金动作经 spot/futures 后端（已 F1/F2/F3/F4/F5 硬化）；bot 仅持用户授权 token 与幂等 client_oid 两把"开关"，杜绝越权与双付。

## 30. 业务线落地：跟单 copytrade（2026-08-18，已完成）

新增业务线 `internal/copytrade` + `cmd/copytrade`（`:8099`）。带单高手注册、粉丝关注、消费撮合 `mq.TradeEvent`（已含 `TakerID`/`MakerID` 交易者标记）按比例复制成交给粉丝，平台复制费结算入 `SysCopyTradeFee`（账本系统账户 -12）。

- `model.go`：`LeadTrader`/`Follow`(含 `FollowerToken` 授权凭证 `json:"-"`)//`CopyRecord`(含 `EventID`+`FollowID` 幂等键)；`quoteAsset()` 从 `BASE_QUOTE` 解析计价资产。
- `store.go`/`store_mem.go`/`store_mysql.go`+`store_migrations.go`：`Store` 接口 + 内存/MySQL；迁移 `9805`(`ce_copytrade_leads`)/`9806`(`ce_copytrade_follows`)/`9807`(`ce_copytrade_copies`，`UNIQUE(event_id, follow_id)` 库层 F1 兜底)。
- `service.go`：`OnTrade(ev)` 入口——全局 `processed` map 去重（F1），识别被跟单 lead（taker/maker 两个方向，maker 取反方向），逐粉丝 `replicate`：F5 名义额下限 + 计价资产白名单 + 封顶 `AllocatedAmount`；代粉丝以 `FollowerToken` 经 `HTTPExecutor` 下单（F4，下游 spot/futures 校验）；F2 定点平台复制费 `followerNotional*CopyFeeRate` 从粉丝账户结算入 `SysCopyTradeFee`。
- `handler.go`：`/api/v1/copytrade` 路由组，`middleware.Auth` + 创建/关闭带单/关注/停止/管理全量查询 `AdminGuard`。
- `cmd/copytrade/main.go`：装配层（copytrade 自身账本种子充值供复制费结算、订阅 `exchange.trades` topic 调 `OnTrade`、信号退出）。Kafka 构建 + 配置 brokers 时消费真实成交流；否则退回内存订阅器（复制不可用，仅 HTTP 管理端点照常）。
- 测试（`service_test.go`）：`TestCloseStopF4Owner`(越权拒绝)、`TestOnTradeReplicatesAndFees`(F1 幂等 + F4 token 绑定 + 方向一致 + 复制费入 `SysCopyTradeFee`)、`TestOnTradeSkipsUnsupportedQuote`(F5 未知计价资产跳过)、`TestOnTradeSkipsBelowMin`(F5 粉尘单跳过)、`TestOnTradeMakerSideReversal`(maker 反向复制)。

> 资金安全：复制下单全部资金动作经 spot/futures 后端（F1/F4 复用）；平台复制费以 `settlement.AssetAmount` 定点（F2）从粉丝账户结算入 `SysCopyTradeFee`，幂等引用防双付（F1）；`processed` map + 库唯一键双重去重（F1 纵深）；非标准/未知计价资产的成交跳过（F5）。v1 仅支持现货跟单（`DefaultMarket=spot`），期货跟单为后续扩展。

**验证（三条新业务线）**：`go build ./...` 全绿；`go vet` 新包无告警；`go test ./...` 全绿（含 `internal/staking`/`internal/bot`/`internal/copytrade` 的 F1–F5 专项用例）；`configs/config.yaml` 的 `services` 段已注册 `staking`(:8097)/`bot`(:8098)/`copytrade`(:8099) 经网关统一反代。

## 31. 跨服务端到端测试（e2e，2026-08-18，已完成）

新增 `e2e/e2e_test.go`（`scripts/e2e.sh` 包装）：在进程内以独立 `httptest` 服务拉起「下游 spot 订单服务」与 bot/copytrade 路由，二者**共享同一 `TokenVerifier`**，走真实 HTTP 验证跨服务资金安全不变量。

- **下游 spot 订单桩**复刻 spot/futures `/api/v1/<market>/order` 契约：真实校验 Bearer token（F4）、记录每笔委托（含解析出的 `user_id`）并返回 `order_id`；不依赖 MySQL/Kafka，任意环境可跑。
- `TestBotCrossServiceE2E`：用户经 bot 创建 DCA 策略并授权 token → `Tick` 强制驱动一轮 → 断言下游记录 1 笔委托，且 `user_id==42`（F4 token 透传解析正确，无越权）、`client_oid` 前缀 `bot:<id>:`（F1 幂等键落地）、订单参数正确；另验证他人启动被拒（F4 越权）。
- `TestCopytradeCrossServiceE2E`：用户 10 注册带单 + 用户 7 关注并授权 → `OnTrade`（即订阅者回调入口）模拟用户 10 作为 taker 的成交 → 断言下游记录 1 笔**粉丝(用户 7)**委托（F4：以粉丝身份下单而非 lead）、方向与 lead 一致、`client_oid` 前缀 `copytrade:`（F1）；平台复制费 10 USDT 定点结算入 `SysCopyTradeFee`；重复事件不再复制（F1 幂等）。

> 该 e2e 覆盖 bot→spot 与 copytrade→spot 两条真实跨服务 HTTP 边界；staking 为服务内复式记账（自身账本，无外部资金依赖），故未纳入跨服务 e2e。运行：`go test ./e2e/...` 或 `./scripts/e2e.sh -v`。

## 31. 跨服务 e2e（in-process）测试 + 基于真实二进制的集成脚本（2026-08-18，已完成）

为三条新业务线补齐跨服务边界验证：

- **in-process e2e**（`e2e/e2e_test.go` + `scripts/e2e.sh`）：用真实 `TokenVerifier` + 下游 httptest gin 引擎，验证 bot/copytrade 代下单时**下游 spot 以 token 校验出的 userID 为准（F4）**且携带 `bot:`/`copytrade:` client_oid 前缀（F1），重复事件不复制（F1 幂等）。
- **二进制集成脚本**（`scripts/integration.sh`）：编译并启动**真实二进制** `cmd/matching`+`cmd/spot`+`cmd/bot`+`cmd/copytrade`+`cmd/staking`（内存模式：临时配置置空 MySQL DSN / Kafka brokers / Redis addr，无外部依赖），用真实 HTTP 驱动三条资金流：
  1. bot → spot：用户授权 bot → 创建 DCA 策略 → 管理端点强制 tick → 订单落到 spot 且归属正确 uid（F4）。
  2. copytrade → spot：创建 lead + 关注 → 无 Kafka 时由**新增 admin 模拟端点**注入成交流 → 粉丝复制单落到 spot 且归属正确 uid（F4），重复事件不产生第二单（F1）。
  3. staking 链上质押生息：列出在售产品 → 用户委托质押 1.0 ETH（本金锁定 `SysStaking`，复式记账 F2/F3）→ 管理员触发一次奖励归集（accrue，链上待领奖励计入 `SysStakingReward` 负债且总额 > 0）→ 解质押并释放（本金+累计奖励经 `ledger.Batch` 原子归还 F3）→ 终态重复释放被拒（F1 幂等）。
  - token 由脚本用共享开发密钥本地签发（与 `middleware.TokenVerifier` HMAC-SHA256 完全一致），故无需 user 服务往返。
  - 各服务监听端口**动态选取空闲端口**（规避环境端口占用，如本机 8082 被 `goldot-signer` 占用）；为此给 `cmd/spot`/`cmd/bot`/`cmd/copytrade`/`cmd/staking` 增加了 `--addr` 监听端口参数（matching 已有）。
  - 新增管理/调试端点（仅供 AdminGuard）：`POST /api/v1/bot/admin/strategies/:id/tick`（强制一轮下单，等价于后台 Run 循环）、`POST /api/v1/copytrade/admin/simulate-trade`（注入一笔成交流驱动复制，等价于发布到 exchange.trades）、`POST /api/v1/staking/admin/accrue`（手动触发一轮奖励归集，等价于后台 RunLoop）。
- **修复真实 bug（跨服务 market 字段丢失）**：spot 下单经 `matching` HTTP `/order` 时，`matching` 客户端 `Submit` 与 `/order` handler 均未传输 `Market` 字段，导致撮合引擎存储的订单 `market=""`，spot `GET /orders` 按 `v.Market != "spot"` 过滤时把全部订单丢弃——用户查不到自己任何现货订单。已修复：客户端 `Submit` 增加 `"market": o.Market`，`/order` handler 读取并写入 `o.Market`。该 bug 由二进制集成脚本（真实链路）首次暴露，in-process e2e 因使用假下游未能覆盖。

> 验证：`go build ./...`、`go vet`、`go test ./internal/matching/... ./internal/spot/... ./internal/bot/... ./internal/copytrade/... ./e2e/...` 全绿；`bash scripts/integration.sh` 全流程 ALL PASS。

## 32. 现货资金安全闭环：账本快照持久化 + openOrders 对账重建 + 停机补账（2026-08-25，已完成）

闭合 §19/审计指出的现货资金安全缺口：撮合引擎有独立快照/WAL（§17），重启后旧挂单仍在簿；
而 spot 侧账本为进程内内存、openOrders 冻结登记不重建——僵尸单成交走「无冻结记录→纯转账」
分支，从重置后的种子余额划转（账实脱钩）。

### 改动

1. **账本快照持久化**（`cmd/spot/main.go`，镜像 §18 futures 方案）：
   - 启动时 `LoadSnapshotFromMySQL(dsn, "spot")` → `Restore`；库无快照/加载失败回退种子充值；
   - defer + SIGINT/SIGTERM 双路 `SaveToMySQL(dsn, "spot")`，余额/冻结跨重启保留；
   - `SetIdempotencyDB(db, "spot")` 接线（此前仅 futures 有），转账指纹跨进程防双付。
2. **openOrders 对账重建**（`internal/spot/store.go` `RestoreOrders` 重写）：
   - 新增三态查询 `matching.Client.OrderState`：known(200) / not-found(404) / unreachable(err)，
     区分「订单已终结」与「撮合暂时不可达」；
   - open/partial → 正常重建冻结登记与 clientOIDMap；
   - 终态或撮合不认识 → 释放残留冻结 + 删除持久化记录 + 清理幂等键（杜绝僵尸冻结）；
   - 撮合不可达 → **保守保留**冻结登记（不误释放，用户撤单可自愈，reconcile 可发现）。
3. **停机补账**（`internal/spot/server.go` `CatchUpSettlement` + `Client.RecentTrades`）：
   - 重启后拉取各交易对最近 1000 笔全市场公开成交按时间正序重放 `settleFill`，
     弥补 WS 成交事件丢失导致的经济漏记（在簿已成交、资金未划转）；
   - 防双付三重保障：settledRefs 内存去重 + 账本 ref 指纹持久化跳过 + Batch 原子性；
   - **调用顺序约束（关键）**：必须先于 RestoreOrders——此刻 openOrders 尚空，settleFill
     走纯转账分支、所有操作携带成交 ref 受指纹保护；若先恢复冻结登记再重放，旧成交的
     Unfreeze 无 ref 不受指纹保护会二次解冻。幂等库不可用时跳过补账并告警。

### 测试

- `internal/spot/restore_test.go` ×6：open 单重建 / 终态单释放冻结（账平校验）/ 撮合不可达
  保守保留 / 补账一次结清且重放不双付 / 合约成交不参与补账 / 恢复单后续成交正确递减；
- fakeMatcher 扩展 `OrderState`（含 unreachable 开关）/ `RecentTrades`（symbol 过滤保真）；
- 既有 `TestRestoreOrdersRebuildsClientOIDMap` 适配新对账语义。

> 验证：`go build ./...` 通过；`go test ./...` 34 包全绿。已知边界：停机窗口超过
> RecentTrades 回看深度时更早漏结依赖 `/spot/admin/reconcile` 告警人工介入；NewServer 的
> Watch 订阅先于恢复流程启动，极端竞态下早到成交走纯转账分支（幂等安全，仅登记滞后）。

## 33. 业务线落地：理财中心 earn + 新币挖矿 launchpad（2026-08-25，已完成）

对齐前端 `Earn.tsx`/`Launchpad.tsx` 契约（§24 缺口清单 A-2），新包 `internal/earn` 覆盖两条
路由族：`/api/v1/earn/*`（活期/定期申购、计息、赎回）与 `/api/v1/launchpad/*`（项目列表、
质押、领奖、解押）。复用 §28 wealth 的定点整数计息公式与两阶段出金模式。

### 改动

1. **理财中心**：
   - 申购：agreed 风险揭示必勾选；min/max 限额；余额校验后本金 user→SysWealth
     （ref `earn_sub:<id>`），两阶段「先落单后转账」失败回滚；
   - 计息：`AccrueAll` 按 YIELD_SCALE 定点整数计息 SysWealth→SysWealthYieldPayable，
     ref 携带 unix 时间戳（`earn_accrue:<sub>:<ts>`，规避账本指纹去重吞掉跨期利息——
     wealth #47 的 ref 无时间戳属潜在隐患，此处已规避）；
   - 赎回：定期未到期拒绝（Maturity 锁定）；先落终态再 Batch 双腿出金
     （part=principal/yield），Batch 失败回滚终态；
2. **Launchpool**：
   - 项目：管理员创建（pools 校验：id 唯一、资产白名单 KnownAsset）、状态由 now 推导
     upcoming/ongoing/ended；奖励预算由管理员 FundProject 预充 token→SysStakingReward
     （funded_total 累计入库存，供对账）；
   - 质押 user→SysStaking（ref `lp_stake:<pos>:<seq>`）；同一池重复质押合并仓位；
   - 领奖 Harvest：**系统账户允许透支**，预算闸口必须在服务层显式做——池余额不足时
     先持久化 Pending 再返回 ErrPoolExhausted（fail-safe 可追溯）；
   - 解押 Unstake：partial 或全额（amount=0），seq 引用防指纹去重。
3. **持久化**：迁移 9611-9614（ce_earn_products / ce_earn_subscriptions /
   ce_launch_projects 含 funded_total / ce_launch_positions），金额列 VARCHAR(64)
   HumanString 存储、读取 AssetAmountFromString 解析；MemStore + MySQLStore 同构。
4. **装配**：`cmd/earn/main.go`（--addr :8093，种子产品 + 进行中 NEW 项目由 admin 预充
   预算，RunLoop 60s 自动计息）。
5. **集成脚本**：integration.sh 新增流程 5（产品列表/agreed 护栏/申购赎回/项目列表/
   质押/harvest fail-safe/解押），earn 二进制纳入构建与探活。

### 测试

- `internal/earn/service_test.go` ×8：活期申赎闭环 / agreed+min/max 护栏 / 定期锁定与到期
  赎回 / 定点计息入 YieldPayable / 质押领奖解押全流程（区间断言容忍 AddDate 半年天数）/
  状态门控与预算 fail-safe / 重复质押不因幂等指纹被吞 / 部分解押与护栏；
- `internal/earn/handler_test.go` ×2：admin 端点 F4 鉴权护栏（403 矩阵）/ 未勾选揭示 400。

> 验证：`go build ./... && go vet ./...` 通过；`go test ./...` **35 包全绿**；
> integration.sh ALL PASS（含新流程 5）。前端无需改动——mock/gateway.mjs 的 earn/
> launchpad 路由族与本服务逐字段对齐，切真实后端即插即用。

## 34. 用户侧缺口收口：通知契约对齐 + 安全中心四组端点（2026-08-25，已完成）

闭合 §24 缺口清单中的用户域两项：**站内信前缀错配**与 **api-keys / sessions /
login-history / anti-phishing 全缺**。公告（announcement）经复核两侧已逐字段对齐
（用户端 GET /api/v1/announcement/list、管理端 CRUD、mock 与 vite 代理均已通），无需改动。

### 改动

1. **通知别名路由**（`internal/notification`，保留旧路由兼容 adminapi）：
   - 新增 `/api/v1/user/notifications` 五端点：GET 列表（`{notifications,unread}`）、
     GET unread-count（`{count}`）、POST `/:id/read`（路径参数版单条已读）、
     POST read-all、**DELETE `/:id`（新增删除能力，Store 接口 + mem/mysql 三层补齐）**；
   - 响应形状投影 userNotificationView：内部 `{type,body,status}` → 前端
     `{level(info|warning|critical),content,read(bool)}`，LevelOf 映射：risk_alert→critical、
     kyc_rejected→warning、其余→info；
   - gin v1.10 验证静态段（read-all）与参数段（:id/read）可共存。
2. **安全中心四组端点**（`internal/services/user`，security.go / security_handler.go）：
   - **API Key**：POST 创建校验 label/permissions（⊂read|trade|withdraw）/ip_whitelist，
     生成 `cxk_<prefix>_<secret>` 明文（secret 仅创建响应返回一次，存储只落 sha256(secret)
     哈希）；GET `{api_keys,total}` 不泄露 secret；PUT 启停（仅 active|disabled）；DELETE 硬删；
   - **登录历史**：Login 成功/失败均记录（失败按 target 反查归属用户；未知目标跳过），
     IP→归属地演示映射（本地/内网/未知），entry id 以字符串序列化（前端 id:string 契约）；
   - **会话**：登录成功建会话（随机 32 hex id）；current 不落库——读取时推导「最近活跃」
     并触碰 last_active_at（调用方即当前会话）；不可注销当前会话（400）；注销全部返回 revoked 计数；
   - **防钓鱼码**：GET 回显（未设置为空串）、POST 设置/空串清除、超长 400；
3. **迁移 9109-9112**：ce_user_api_keys / ce_user_login_history / ce_user_sessions /
   ce_user_anti_phishing（permissions/ip_whitelist 逗号分隔存储）。

### 测试

- `internal/notification/handler_test.go` +TestUserNotificationContract：五端点全链路
  （列表形状/unread/count 键/critical 投影/越权 404/read-all 归零/DELETE 及重复删除 404）；
- `internal/services/user/security_test.go` ×3：API Key 全生命周期（含三类 400 守卫、
  secret 泄露检查、跨用户 404）；登录历史+会话（成功/失败双记录、字符串 id、current 推导、
  当前会话 400、未知 404、revoked 计数）；防钓鱼码设置回显清除。

> 验证：`go build ./... && go vet ./...` 通过；`go test ./...` 35 包全绿。
> 已知边界：会话 current 为「最近活跃」启发式（真实部署建议把 session id 写入 access token
> claim 精确绑定）；登录历史归属地为演示值（可接 GeoIP 替换 locFromIP）。

## 35. 契约缺口收口 II：OTC 法币报价 + 用户侧自助充值（2026-08-25，已完成）

1. **GET /api/v1/otc/prices**（`internal/otc`）：补齐前端虚构的后端缺口。
   base_price = 参考价(USD) × fiat_rate；汇率表 USD=1/CNY=7.23/EUR=0.92 与 mock 网关
   同口径（真实部署接汇率源）；USDT 无行情按 1 USD 稳定币基准，其余资产必须有
   priceFn 报价否则 404；未知法币回退 rate=1。响应 {asset,fiat,base_price,fiat_rate,updated_at}。
2. **POST /api/v1/futures/wallet/deposit/self**（`internal/futuresapi`）：
   修复「充值被 AdminGuard 拒绝 + body 要求 user_id」与前端用户侧充值的双重错配：
   - 归属 uid 一律取 token（防冒充，body user_id 忽略）；管理端 faucet（POST /deposit，
     AdminGuard + body user_id 指定入账对象）保留并存；
   - 资产白名单 USDT/BTC/ETH（对齐 mock WALLET_ASSETS）；单笔上限 10000、滑动窗频控
     6 次/分钟（内存实现），防脚本刷入虚假资金；
   - ledger ref 唯一化（deposit:self:uid:unixnano），同额快速连充不被指纹去重吞掉；
   - 响应 {status,asset,available,frozen}（HumanFloat 数值口径对齐 mock）。

### 测试

- `internal/otc/handler_test.go` +TestOtcPrices：BTC/CNY 换算、缺省参数 USDT/CNY、
  未知法币回退、无行情资产 404；
- `internal/futuresapi/handler_wallet_f4_test.go` +TestDepositSelfUserFlow：正常入账
  （10000+500）、白名单/上限/负数 400、body user_id 冒充无效、频控第 7 次 429。

> 验证：`go test ./...` 35 包全绿；前端 client.ts 切换至 /deposit/self 后 tsc/vitest 全绿，
> mock 网关新增同名别名路由保持开发流一致。

---

## 36. TP-SL 持久化（重启不丢失用户止盈止损设置）

### 问题

TP-SL（止盈/止损）订单在 `Server.tpsl` 内存 map 中存储，进程重启后全部丢失。
用户设置的止盈止损在交易所重启后静默消失，存在重大风控缺口。

### 改动

1. **`internal/futuresapi/tpsl_store.go`**（新增文件）：
   - `TPSLStore` interface：`Upsert(uid int64, key string, tp, sl *float64) error`、
     `LoadAll() (map[int64]map[string]TPState, error)`、`Delete(uid int64, key string) error`；
   - `MemTPSLStore`：内存实现（map + sync.RWMutex），单元测试覆盖往返语义；
   - `MySQLTPSLStore`：MySQL 持久化实现，迁移 **9951** 创建表 `ce_futures_tpsl`
     （user_id, key, tp, sl, created_at, updated_at）；UPSERT ON DUPLICATE KEY UPDATE；
   - `key` 格式：`{symbol}:{pos_side}`（如 `BTC_USDT_PERP:long`）。

2. **`internal/futuresapi/server.go`**：
   - Server struct 新增 `tpslStore TPSLStore` 字段；
   - `NewServer` 中：若提供 `*sql.DB` 则创建 `MySQLTPSLStore`，否则 `MemTPSLStore`；
   - 启动时 `LoadAll()` 从 store 加载到内存 map，日志记录恢复条目数。

3. **`internal/futuresapi/handler_gaps.go`** — `handleSetTPSL`：
   - 更新内存 map 后调用 `s.tpslStore.Upsert(...)` 写穿持久化；
   - 写穿失败仅 warn 日志，不阻塞用户操作（内存优先保可用性）。

### 测试

- `internal/futuresapi/tpsl_store_test.go`：
  - `TestTPSLStoreRoundtrip`：Upsert → LoadAll 一致性 → 覆盖写 → Delete 清除；
  - `TestTPSLWriteThroughViaServer`：通过 handler 设置 TP-SL → 验证 store 落库 →
    新 Server 实例共用同一 store（模拟重启）→ decorateWithTPSL 恢复验证。

> 验证：`go test ./...` 35 包全绿。

---

## 37. 业务事件→通知：强平与保证金预警写入站内信（2026-08-26，已完成）

闭合「合约强平/逼近爆仓仅留 WS 广播、不入用户通知中心」的缺口：用户强平时无站内信、
无 app 推送入口，安全事件不可见。

### 改动

1. **通知类型扩展**（`internal/notification/model.go`）：
   - 新增 `TypeLiquidation = "liquidation"`（强平）、`TypeMarginWarning = "margin_warning"`（保证金预警）；
   - `LevelOf` 映射：liquidation→critical、margin_warning→warning（与 risk_alert 同级 critical、kyc_rejected 同级 warning 一致）；
   - `validType` 同步放行两类型（未知类型仍降级为 system，不阻塞调用方）。
2. **futuresapi 接入通知服务**（`internal/futuresapi/server.go`）：
   - Server 新增 `notifSvc *notification.Service`（内存 store，重启清空已读状态，不影响内容）+ `marginWarned map` 去重；
   - `NewServer` 内初始化（与风险/账本同进程，无需跨服务 RPC）；
   - `broadcastLiquidations` 在 WS 广播的同时调用 `publishLiquidationNotice`，向被强平用户推送站内信（部分/全额强平分标题，body 含标的/方向/强平价/手续费/实现盈亏）；
   - 新增 `emitMarginWarnings` + 常量 `MarginWarnRatio=1.2`：liqScanLoop 每轮扫描所有仓位，
     逐仓按 `(margin+UPNL)/notional`、全仓按 `mark/liqPrice`（多）或 `liqPrice/mark`（空）推算保证金率，
     低于阈值且未在 `marginWarned` 去重集合中时发送「保证金不足预警」站内信（每 user+symbol 仅一次，避免震荡期骚扰）。
3. **copytrade 自动跟单（已具备，本次验证）**：`cmd/copytrade/main.go` 经 Kafka `exchange.trades` 订阅
   驱动 `svc.OnTrade`；本次确认链路完整（已有 26+ handler_test 覆盖 API、service_test 覆盖复制逻辑）。

### 测试

- `internal/notification/handler_test.go`：+`TestLevelOfNewTypes`（映射）、+`TestNewNotificationTypesPublished`（两类型经 Publish 落库且 level 正确）；
- `internal/futuresapi/notify_test.go`：+`TestLiquidationNoticePublished`（全额/部分强平标题与落库）、+`TestMarginWarningEmitted`（逼近爆仓触发预警、内存去重、重置后可恢复）。

> 验证：`go test ./...` 35 包全绿；`go vet` 通过。

---

## 38. 推荐防刷：新用户冷却期 + 自邀请拒绝（2026-08-26，已完成）

闭合「推荐佣金可被注册即刷量」的缺口：原实现仅结算层校验 `referrer==taker` 自交易，
未限制被邀请人注册后立刻自买自卖刷佣金，也无冷却窗口。

### 改动

1. **冷却期**（`internal/referral/hook.go`）：
   - `HookAdapter` 新增 `cooldownDays int` + `now func() time.Time`（可注入）；
   - `NewHookAdapterWithCooldown(userStore, svc, rate, cooldownDays)` 构造器；
   - `RecordTradeFee` 在结算层佣金落账前检查被邀请人 `CreatedAt`：若 `now-CreatedAt < cooldownDays*24h` 则跳过（不计佣金）；
   - 默认 `DefaultCooldownDays = 7`，`NewHookAdapter` 包装沿用默认；`cmd/settlement/main.go` 调用不变。
2. **自邀请拒绝**（`internal/referral/hook.go`）：保留 `referrer==taker` 直接报错（`fmt.Errorf("referrer cannot be taker")`），
   与原有 settlement 层防自交易一致；注册层 `user.Service.Register` 已按 referral_code 查邀请人，
   自引用在 DB 层天然不可能（自身 code 注册时方生成）。

### 测试

- `internal/referral/hook_test.go`：+`TestReferralCooldown`：
  - 注册 1h 的 taker 在 7 天冷却期内不计佣金（0 条）；
  - 注册 30 天的 taker 正常计入（1 条）；
  - `cooldownDays=0` 关闭冷却后刚注册用户也计入（1 条）；
  - 自邀请（referrer==taker）返回错误且 0 条佣金（原有规则保留）。
  - 配套 `fakeUserStore`（实现 user.Store 全接口最小内存）、`fakeCommissionStore`。

> 验证：`go test ./...` 35 包全绿；`go vet` 通过。

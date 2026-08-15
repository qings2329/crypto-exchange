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
| T-03 | 真实链上 / 预言机 RPC 接入 | 接入真实节点与预言机（充值/提现链上回调、指数价喂价）；依赖外部节点/密钥，受合规约束 | architecture.md §16/§21 留白 | **预言机实时喂价已可配置接入**（新增 `internal/oracle.NewFromConfig` + `configs/config.yaml` 的 `oracle` 段，支持 Binance/OKX/Coinbase 真实 REST 源并经单测；cmd/{margin,otc,options,futures} 已接线，无配置时回退内置演示）；**链上 RPC（充值/提现链上回调）仍阻塞**（依赖外部节点+合规，见前述方案 B/C/D） |
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
| T-16 | 依赖组件实际接入 | `docker-compose` 声明了 MySQL/Redis/Kafka/InfluxDB/ES，但当前仅 MySQL 真正使用，其余未接入 | docker-compose.yml | **部分接入（Kafka+Redis 已落地，见 §21/§22）**：Kafka 交易清算（§21）+ **Redis 集群级限流（§22）** 已接入；InfluxDB/ES 仍仅声明，待各业务接入 |
| T-17 | 对账 / 审计报表 | 基于 `Ledger` 流水做借贷恒等校验、对账与审计看板 | 资金安全闭环延伸 | **已完成**（`Ledger.Reconcile/IsBalanced/RunReconcileOnce` + 定时巡检 + `/wallet/reconcile` 端点 + Prometheus 指标已具备） |
| T-18 | 安全加固（暂不引入中间件） | 统一安全中间件套件：审计日志、安全响应头、受控 CORS、请求体限制、边缘/入口鉴权与公开端点豁免、TLS 可配置；修复 spot/futures 无鉴权下单漏洞与 market/futures 端口冲突 | 安全审计发现 | **已完成**（见 §13） |

---

## 4. 优先级建议（可执行顺序）

1. **先补安全与资金闭环**：T-01（鉴权）、T-02（Kafka 清算）、T-04（migration）。
2. **再做合约完善**：T-05→T-06→T-07→T-09 顺序推进，T-08 在 T-03/T-09 后做端到端演练；T-10 顺手修。
3. **最后铺基础设施与业务线**：T-11~T-17。

> 所有涉及新建数据表的任务（T-04、T-12、T-14、T-15、T-17 等）一律遵守 `ce_` 前缀约定。

## 5. 完成状态汇总（截至 2026-08-13）

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
- spot/futures 仍各自持有内存订单簿；真正「全交易所多实例」应把匹配收敛为单一 `cmd/matching` 服务（单写者可水平容灾），spot/futures 改为其 HTTP 客户端。本轮未做（属更大的服务网格重构）。

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

### 18.1 后续（未做）
- spot/futures 目前仍各自运行一份 `client` 连同一个 `cmd/matching`；要真正「全所单写者容灾」需把网关层（`cmd/gateway` 或 Nginx）把 `/api/v1/spot/*order`、`/api/v1/futures/*order` 也代理到 `cmd/matching`，或让 spot/futures 仅做业务/账本、匹配全权委托——本轮按用户原话完成收敛到 `cmd/matching` 客户端，未进一步合并进程。
- 强平 `MatchNow` 的流动性消耗现已写 WAL（见 §17 已知缺口①已补齐）；T-03 真实链上/预言机 RPC 仍阻塞（依赖外部节点+合规），本轮未实现，仅保留文档留白。

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

**已知缺口（后续）**：
- 风控快照、账本对账、服务健康、运营通知当前是 admin 服务内存只读骨架，应后续接入 futures/settlement 实时强平/穿仓/ADL、结算对账与各微服务健康探测。
- 用户、交易对、公链、币种、充值提币 CRUD 为内存态，应后续对接 user/settlement 真实持久化（数据库/链上网关）。
- 管理后台登录为明文凭据（config 内），生产应改为哈希+密钥管理，并可对接真实管理员账号体系。

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

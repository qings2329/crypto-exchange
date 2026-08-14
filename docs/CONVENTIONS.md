# 项目约定（Conventions）

本文件记录 crypto-exchange 项目的强制性编码与架构约定，新增代码必须遵守。

- [1. 数据库表命名：必须以 `ce_` 开头](#1-数据库表命名必须以-ce_-开头)
- [2. 服务分层：Handler / Service / Store / Model 四层分离](#2-服务分层handlerservicestoremodel-四层分离)

## 1. 数据库表命名：必须以 `ce_` 开头

**所有新建的数据库表名，都必须以 `ce_` 作为前缀。**

- 适用范围：MySQL / 任何关系型存储中由本项目创建的业务表。
- 示例：`ce_ledger_snapshots`、`ce_users`、`ce_whitelist_addresses`。
- 理由：表前缀统一标识本项目表空间，避免与同实例下其他库/服务的表命名冲突，
  也便于运维按前缀做权限、备份与审计策略。
- 约束：
  - 表名在代码中应以常量集中定义（如 `mysql_store.go` 中的建表 SQL），
    不要在多处硬编码裸表名，改名时只改一处。
  - 已有非 `ce_` 前缀的历史表，新建替代表时直接采用 `ce_` 前缀，
    老表废弃前不要混用命名风格。

> 落实示例：`internal/ledger/mysql_store.go` 的快照表由 `ledger_snapshots`
> 改名为 `ce_ledger_snapshots`，建表 / UPSERT / SELECT / 错误文案同步修改。

## 2. 服务分层：Handler / Service / Store / Model 四层分离

**每个业务服务（如 `internal/services/user`）必须保持清晰的四层职责分离，禁止跨层直连或把业务逻辑塞进 HTTP 层。**

四层职责边界：

| 层 | 位置 | 职责 | 禁止 |
|----|------|------|------|
| **Handler** | `handler.go` | 仅做 HTTP 入参绑定（`ShouldBindJSON`）、路径/鉴权上下文提取、调用 Service、用 `response.JSON/Error` 返回。 | 禁止写业务规则、bcrypt、事务、SQL；禁止直接访问 `store`。 |
| **Service** | `service.go` | 业务编排：密码哈希、Token 签发、状态机、2FA、KYC 审核等；依赖 `Store` 接口**而非具体实现**。 | 禁止内嵌 SQL；禁止出现 `*sql.DB`；不要耦合 gin 的 `*gin.Context`（除鉴权取值外）。 |
| **Store / Repository** | `store*.go` | 持久化：定义 `Store` 接口 + 内存实现（`store_mem.go`，单测用）+ MySQL 实现（`store_mysql.go`），表名遵守 `ce_` 约定；建表交给 `migrate` 迁移。 | 禁止把业务规则放进 Store；接口签名要可测、与具体 DB 解耦。 |
| **Model** | `model.go` | 纯数据结构与领域错误定义（`ErrNotFound` 等），不含逻辑。 | 禁止 import 数据库驱动或 HTTP 框架。 |

- 依赖方向只能单向向下：`Handler → Service → Store → Model`，底层不得反向依赖上层。
- `cmd/<service>/main.go` 只做**装配**（读配置、选存储实现、跑迁移、挂中间件、起路由），不含业务。
- 跨服务的通信走 `internal/pkg/mq`（事件）或网关路由，不要在服务内部直接 import 另一个服务的 Store。

> 落实示例：`internal/services/user` 已按此分层——`handler.go`（路由+绑参）、
> `service.go`（注册/登录/2FA/KYC 状态机，依赖 `Store` 接口）、
> `store.go`（接口）+ `store_mem.go`（内存）+ `store_mysql.go`（MySQL，`ce_users`/`ce_user_codes`/`ce_user_refresh`/`ce_user_kyc`）、
> `model.go`（User/KYC/VerifyCode 结构与错误）；`cmd/user/main.go` 仅做装配与迁移。

**cmd 模块分层整改（已完成）**：原先 `cmd/futures`(1608行)、`cmd/market`、`cmd/spot` 把引擎装配、后台循环、HTTP 闭包全堆在 `main.go`，已按四层拆分：

| 服务 | 业务/领域（internal/） | 装配层（cmd/<svc>/main.go） |
|------|------------------------|------------------------------|
| futures | `internal/futuresapi`：`server.go`（Server 聚合依赖 + 引擎/预言机/网关/资金费循环接线 + 4 类回调 + 风控参数）、`handler.go`+`handler_wallet.go`（全部 HTTP 路由，仅绑参/调领域依赖/返回）、`helpers.go`（sideName/parseUserID/modeName） | 读配置、建 ledger/publisher、MySQL 持久化生命周期（恢复/种子/信号/保存）、调 `NewServer`+`RegisterRoutes`+`Run` |
| market | `internal/market`：`market.go`（Ticker/Market 领域对象）、`server.go`（Server + 演示随机游走 + /ws + /ticker 路由） | 读配置、日志、`NewServer`+`RegisterRoutes`+`Run` |
| spot | `internal/spot`：`spot.go`（depthRow/aggregate 工具）、`server.go`（Server + 撮合引擎接线 + /order + /depth + /ws） | 读配置、日志、`NewServer`+`RegisterRoutes`+`Run` |

> 分层要点：HTTP 路由/闭包不得再内联在 `cmd/<svc>/main.go`；引擎/网关/循环/回调等"应用装配"统一收口到 `internal/<svc>` 的 `Server`（`NewServer`/`RegisterRoutes`/`Close`），`main.go` 仅做进程级装配与生命周期管理。

| margin | `internal/margin`：`model.go`（MarginAccount/状态/错误）、`store.go`（Store 接口）+`store_mem.go`（内存）+`store_mysql.go`+`store_migrations.go`（MySQL `ce_margin_accounts`，版本 9201）、`service.go`（Borrow/Repay/Accrue/Liquidate/RunLoop，依赖 Store 接口 + ledger）、`handler.go`（全部路由，受 `middleware.Auth` 保护） | 读配置、建 ledger+种子、选 Store（MySQL+迁移否则内存）、oracle 价格、装配 Service、注册路由、后台循环、信号退出 |
| notification | `internal/notification`：`model.go`（Notification/类型枚举/状态机/领域错误）、`store.go`（Store 接口）+`store_mem.go`（内存）+`store_mysql.go`+`store_migrations.go`（MySQL `ce_notifications`，版本 9301）、`service.go`（Publish/List/UnreadCount/MarkRead/MarkAllRead）、`handler.go`（list/unread-count/read/read-all/publish/admin-list，受 `middleware.Auth` 保护） | 读配置、选 Store（MySQL+迁移否则内存）、verifier=cfg.Auth.Secret、注册路由、信号退出 |
| risk | `internal/risk`：`model.go`（RiskRule/BlacklistEntry/RiskEvent/类型枚举/作用域/CheckResult/错误）、`store.go`（Store 接口）+`store_mem.go`（内存）+`store_mysql.go`+`store_migrations.go`（MySQL `ce_risk_rules` 9401/`ce_risk_blacklist` 9402/`ce_risk_events` 9403）、`service.go`（CheckWithdraw/CheckOrder 黑名单+限额+KYC 校验、AddRule/AddBlacklist/RecordEvent）、`handler.go`（rules/blacklist/check/events，受 `middleware.Auth` 保护） | 读配置、选 Store（MySQL+迁移否则内存）、verifier=cfg.Auth.Secret、注册路由、信号退出 |
| options | `internal/options`：`model.go`（OptionContract/OptionPosition/类型·行权方式·方向枚举/状态机/领域错误）+`pricing.go`（Black-Scholes 定价+Delta）、`store.go`（Store 接口）+`store_mem.go`（内存）+`store_mysql.go`+`store_migrations.go`（MySQL `ce_option_contracts` 9501/`ce_option_positions` 9502）、`service.go`（CreateContract/Quote/OpenPosition/Exercise/SettlePosition/RunLoop，中央对手方模型集成 ledger 与 oracle）、`handler.go`（contracts/quote/positions/exercise/settle/admin，受 `middleware.Auth` 保护） | 读配置、建 ledger+种子、选 Store（MySQL+迁移否则内存）、oracle 价格、装配 Service、注册路由、后台到期结算循环、信号退出（:8090） |
| otc | `internal/otc`：`model.go`（OtcAdvertisement/方向 buy·sell/状态机、OtcOrder/订单状态机 pending→paid→completed·cancelled·disputed/方向继承广告推导 SellerID/BuyerID/领域错误、OtcCounterparty/对手方信用）+`store.go`（Store 接口）+`store_mem.go`（内存）+`store_mysql.go`+`store_migrations.go`（MySQL `ce_otc_advertisements` 9601/`ce_otc_orders` 9602/`ce_otc_counterparties` 9603）、`service.go`（CreateAdvertisement/TakeOrder 冻结卖方 crypto 入 SysOtc/MarkPaid/ConfirmComplete 释放给买方+更新双向信用/ CancelOrder 退回/OpenDispute/ResolveDispute/Reconcile 对账/RunLoop，中央托管模型集成 ledger）、`handler.go`（advertisements/orders/take、orders/:id 的 pay/complete/cancel/dispute/resolve、orders/counterparties/admin-reconcile，受 `middleware.Auth` 保护） | 读配置、建 ledger+种子 BTC+USDT、选 Store（MySQL+迁移否则内存）、oracle 价格（预留）、装配 Service、注册路由、后台对账循环、信号退出（:8091） |
| wealth | `internal/wealth`：`model.go`（WealthProduct/类型 current·fixed/状态机 open·closed、WealthHolding/持仓本金+已计收益+状态机 active→redeemed、应计收益纯函数 YieldTo）+`store.go`（Store 接口）+`store_mem.go`（内存）+`store_mysql.go`+`store_migrations.go`（MySQL `ce_wealth_products` 9701/`ce_wealth_holdings` 9702）、`service.go`（CreateProduct/Subscribe 扣本金入 SysWealth/Redeem 活期随时·定期须到期·本金+收益出 SysWealth/Accrue 应计收益/RunLoop，中央托管模型集成 ledger）、`handler.go`（products 增/列、subscribe/redeem、holdings 我的/管理员、admin/accrue，受 `middleware.Auth` 保护） | 读配置、建 ledger+种子 USDT+BTC、选 Store（MySQL+迁移否则内存）、自动发行示例产品、装配 Service、注册路由、后台应计循环、信号退出（:8092） |



# API 接口文档（统一索引）

> 本项目后端按业务线拆分为多个独立服务，统一通过网关暴露，约定一致的**响应信封**与 **HMAC Bearer 鉴权**。
> 本文合并各业务线接口文档为单一索引，便于查阅。各模块的事实来源为其 `internal/<module>/handler.go` 等实现。

- **OTC 场外交易**：服务 `cmd/otc`，前缀 `/api/v1/otc`（见 [OTC 模块](#otc)）。
- **用户个人设置**：服务 `internal/services/user`，前缀 `/api/v1/user`（见 [用户设置模块](#user)）。
- **公告模块**：服务 `internal/announcement`（挂载于用户服务 `cmd/user`，共用同一数据库；亦经独立管理后端 `cmd/admin` 以 `/api/admin/announcements` 暴露），前缀 `/api/v1/announcement`（见 [公告模块](#ann)）。
- **理财资管**：服务 `internal/wealth`（独立二进制 `cmd/wealth`，监听 `:8092`），前缀 `/api/v1/wealth`（见 [理财模块](#wealth)）。
- **钱包与资金流水**：服务 `internal/futuresapi`（合约钱包服务），前缀 `/api/v1/futures/wallet`（见 [钱包与资金流水](#wallet)）。
- **站内通知**：服务 `internal/notification`（独立二进制 `cmd/notification`，监听 `:8088`），用户侧前缀 `/api/v1/user/notifications`（见 [站内通知模块](#notify)）。
- **安全中心**：服务 `internal/services/user`（同一前缀 `/api/v1/user` 下 `api-keys`/`sessions`/`login-history`/`anti-phishing` 子路由，见 [安全中心模块](#security)）。
- **理财中心 / 新币挖矿**：服务 `internal/earn`（独立二进制 `cmd/earn`，监听 `:8093`），前缀 `/api/v1/earn` 与 `/api/v1/launchpad`（见 [理财中心模块](#earn)）。
- **跟单交易**：服务 `internal/copytrade`（独立二进制 `cmd/copytrade`，监听 `:8099`），前缀 `/api/v1/copytrade`（见 [跟单模块](#copytrade)）。
- **交易机器人**：服务 `internal/bot`（独立二进制 `cmd/bot`，监听 `:8098`），前缀 `/api/v1/bot`（见 [机器人模块](#bot)）。

---

## 目录

- [通用约定](#common)
- [OTC 场外交易模块](#otc)
  - [基础约定](#otc-basics) · [数据模型](#otc-models) · [接口列表](#otc-endpoints) · [错误码](#otc-errors) · [部署与运行](#otc-deploy)
- [用户个人设置模块](#user)
  - [基础约定](#user-basics) · [数据模型](#user-models) · [接口列表](#user-endpoints) · [错误映射](#user-errors) · [存储与迁移](#user-store) · [前端对接](#user-frontend)
- [公告模块](#ann)
  - [基础约定](#ann-basics) · [数据模型](#ann-models) · [接口列表](#ann-endpoints) · [错误映射](#ann-errors) · [存储与迁移](#ann-store) · [前端对接](#ann-frontend)
- [理财资管模块](#wealth)
  - [基础约定](#wealth-basics) · [数据模型](#wealth-models) · [接口列表](#wealth-endpoints) · [错误映射](#wealth-errors) · [存储与迁移](#wealth-store) · [前端对接](#wealth-frontend)
- [钱包与资金流水](#wallet)
  - [数据模型](#wallet-models) · [接口列表](#wallet-endpoints) · [前端对接](#wallet-frontend)
- [站内通知模块](#notify)
  - [基础约定](#notify-basics) · [数据模型](#notify-models) · [接口列表](#notify-endpoints) · [错误映射](#notify-errors) · [业务事件来源](#notify-events)
- [安全中心模块](#security)
  - [基础约定](#security-basics) · [接口列表](#security-endpoints) · [错误映射](#security-errors)
- [理财中心 / 新币挖矿模块](#earn)
  - [基础约定](#earn-basics) · [数据模型](#earn-models) · [接口列表](#earn-endpoints) · [错误映射](#earn-errors) · [存储与迁移](#earn-store) · [前端对接](#earn-frontend)
- [跟单交易模块](#copytrade)
  - [基础约定](#copytrade-basics) · [数据模型](#copytrade-models) · [接口列表](#copytrade-endpoints) · [错误映射](#copytrade-errors)
- [交易机器人模块](#bot)
  - [基础约定](#bot-basics) · [数据模型](#bot-models) · [接口列表](#bot-endpoints) · [错误映射](#bot-errors)
- [错误码总览](#errors)

---

<a id="common"></a>
## 通用约定

- **响应信封**：所有接口统一返回

```json
// 成功
{ "code": 0, "message": "ok", "data": <业务数据> }
// 失败
{ "code": <业务码>, "message": "<错误描述>", "data": null }
```

- **认证**：业务接口（除显式标注免鉴权者，如 `/health`、登录/注册等公开接口）均需 `Authorization: Bearer <token>`，由 `middleware.Auth` 校验 HMAC Token。前端 `client.ts` 自动注入并在 401 时静默刷新一次。
- **内容类型**：JSON 接口 `Content-Type: application/json`；文件上传为 `multipart/form-data`。
- **时间字段**：响应中 `*_at` 均为 RFC3339 时间字符串。

---

<a id="otc"></a>
# OTC 场外交易模块

> 服务：场外交易（OTC / P2P），独立二进制 `cmd/otc/main.go`，路由前缀 `/api/v1/otc`。
> 范围：广告、订单、订单内沟通消息、付款凭证、争议、评分与对手方信誉、对账。

<a id="otc-basics"></a>
## OTC 基础约定

- **Base URL**：`/api/v1/otc`
- **认证**：除标注 `AdminGuard` 的接口外，所有接口均需 `middleware.Auth` Bearer Token；未携带/非法 → `4010`。
- **业务模型概要**

| 概念 | 说明 |
| --- | --- |
| 广告 Advertisement | maker 发布的买/卖 crypto 广告，含固定法币单价、数量区间、支付方式。 |
| 订单 Order | taker 吃单后生成。crypto 由卖方冻结进中央托管账户 `SysOtc`，法币由双方**线下 P2P** 支付，不在账本。 |
| 对手方 Counterparty | 每对用户维护成交次数与评分，用于信誉评估。 |
| 凭证 Proof | 买方上传的付款凭证文件，落本地磁盘，仅持久化元数据与 URL。 |

- **订单状态机**

```
pending ──(买方标记已付款)──▶ paid ──(卖方确认放行)──▶ completed
   │                              │
   └──────(取消)──────▶ cancelled  └──────(申诉)──────▶ disputed ──(管理员裁决)──▶ completed / cancelled
```

- `crypto_amount` 在 JSON 中以**人类可读十进制字符串**返回（如 `"100.00000000"`），前端按字符串/数字处理即可。

<a id="otc-models"></a>
## OTC 数据模型

### OtcAdvertisement（广告）

```json
{
  "id": 12, "user_id": 3, "side": "sell", "asset": "USDT", "fiat_currency": "CNY",
  "price": 7.2, "min_amount": 100, "max_amount": 5000,
  "payment_methods": "支付宝,微信,银行卡", "status": "open",
  "created_at": "2026-08-17T10:00:00Z", "updated_at": "2026-08-17T10:00:00Z"
}
```

### OtcOrder（订单）

```json
{
  "id": 101, "ad_id": 12, "maker_id": 3, "taker_id": 7, "side": "sell", "asset": "USDT",
  "fiat_currency": "CNY", "crypto_amount": "500.00000000", "price": 7.2, "fiat_amount": 3600,
  "payment_method": "支付宝", "status": "pending", "rating": 0,
  "created_at": "2026-08-17T10:05:00Z", "paid_at": "0001-01-01T00:00:00Z",
  "completed_at": "0001-01-01T00:00:00Z", "updated_at": "2026-08-17T10:05:00Z"
}
```

### OtcMessage（沟通消息）

```json
{ "id": 5, "order_id": 101, "sender_id": 7, "content": "已转账，请查收", "created_at": "2026-08-17T10:10:00Z" }
```

### OtcProof（付款凭证元数据）

```json
{
  "id": 3, "order_id": 101, "uploader_id": 7, "file_name": "alipay-3600.png",
  "content_type": "image/png", "size": 123456,
  "url": "/api/v1/otc/orders/101/proofs/101_1723890000000000000.png",
  "created_at": "2026-08-17T10:12:00Z"
}
```

### OtcCounterparty（对手方信誉）

```json
{
  "id": 9, "user_id": 3, "counterparty_id": 7, "trades_total": 12, "trades_completed": 11,
  "rating_sum": 53, "rating_count": 11, "created_at": "2026-08-01T00:00:00Z", "updated_at": "2026-08-17T00:00:00Z"
}
```

> 平均评分 = `rating_sum / rating_count`；成交率 = `trades_completed / trades_total`。

<a id="otc-endpoints"></a>
## OTC 接口列表

所有路径省略前缀 `/api/v1/otc`。鉴权：`User`=需登录；`Admin`=需管理员（`AdminGuard`）。

### 广告

#### POST `/advertisements`
发布广告（maker 挂单）。

- 鉴权：User
- 请求体：`{ "side": "sell", "asset": "USDT", "fiat_currency": "CNY", "price": 7.2, "min_amount": 100, "max_amount": 5000, "payment_methods": "支付宝,微信" }`（side 必填 buy|sell；price/min/max 必填且合法）
- 响应：`data` = `OtcAdvertisement`；校验失败 → `4001`。

#### GET `/advertisements`
列出广告。鉴权：User；查询参数 `side`（可选）、`asset`（可选）。响应：`{ "advertisements": [...] }`。

### 订单

#### POST `/orders/take`
吃单。鉴权：User；请求体：`{ "ad_id": 12, "fiat_amount": 3600, "payment_method": "支付宝" }`。响应：`data` = `OtcOrder`（crypto_amount 按资产小数位精确取整）；业务错误 → `4002`。

#### POST `/orders/:id/pay`
标记已付款（仅 taker）。鉴权：User；响应：`{ "ok": true }`；非 taker → `4002`；非参与方 → `4030`。

#### POST `/orders/:id/complete`
确认放行（仅 maker，可选评分）。鉴权：User；请求体：`{ "rating": 5 }`；响应：`{ "ok": true }`；非 maker → `4002`。

#### POST `/orders/:id/cancel`
取消订单（pending 可取消，退回托管）。鉴权：User；响应：`{ "ok": true }`。

#### POST `/orders/:id/dispute`
发起申诉（pending/paid 可发起）。鉴权：User；响应：`{ "ok": true }`。

#### POST `/orders/:id/resolve`
管理员裁决。鉴权：Admin；请求体：`{ "refund_to_seller": true, "rating": 4 }`；响应：`{ "ok": true }`。

#### GET `/orders`
当前用户参与的订单。鉴权：User；响应：`{ "orders": [...] }`。

#### GET `/admin/orders`
全部订单（管理员）。鉴权：Admin；响应：`{ "orders": [...] }`。

### 沟通消息

> 仅订单参与方可见；非参与方 → `4030`。

#### GET `/orders/:id/messages`
列出订单全部消息（升序）。鉴权：User（参与方）；响应：`{ "messages": [...] }`。

#### POST `/orders/:id/messages`
发送消息。鉴权：User（参与方）；请求体：`{ "content": "已转账，请查收" }`；内容非空且 ≤ 2000 字符（rune），超出 → `4002`；响应：`data` = `OtcMessage`。

### 付款凭证

> 仅订单参与方可见；文件落本地磁盘（默认 `uploads/otc`）。

#### GET `/orders/:id/proofs`
列出凭证元数据。鉴权：User（参与方）；响应：`{ "proofs": [...] }`。

#### POST `/orders/:id/proofs`
上传凭证。鉴权：User（参与方）；`multipart/form-data`，字段名 **`file`**；非空且 ≤ 10MB，超出/空 → `4002`/`4000`；响应：`data` = `OtcProof`（含 `url`）。文件名安全化为 `orderID_nanos.ext`。

#### GET `/orders/:id/proofs/:file`
下载凭证。鉴权：User（参与方）；`:file` 仅取 basename 并二次校验在 uploadDir 内（防目录穿越）；非法文件名 → `4000`。

### 信誉与对账

#### GET `/counterparties`
当前用户对手方信誉聚合。鉴权：User；响应：`{ "counterparties": [...] }`。

#### GET `/admin/reconcile`
对账：按资产返回 `SysOtc` 托管余额（应恒为 0）与未释放订单。鉴权：Admin；响应：`{ "escrow_balance": {...}, "stuck_orders": [...], "balanced": true }`。

<a id="otc-errors"></a>
## OTC 错误码

| HTTP | code | 含义 / 触发场景 |
| --- | --- | --- |
| 401 | 4010 | 未认证 / Token 非法 |
| 400 | 4000 | 请求体非法、order id 非法、缺少文件、文件名非法 |
| 400 | 4001 | 参数校验失败（side 非法、价格/数量非法、ad_id/fiat_amount 缺失） |
| 400 | 4002 | 业务规则错误（非广告主可吃、金额超范围、余额不足、消息空/过长、文件过大） |
| 403 | 4030 | 非订单参与方（`ErrNotParty`） |
| 404 | 4040 | 订单不存在（`ErrOrderNotFound`） |
| 500 | 5000 | 服务端内部错误（持久化/IO 等） |

<a id="otc-deploy"></a>
## OTC 部署与运行

```bash
go build -o bin/otc ./cmd/otc
./bin/otc -config configs/config.yaml                                   # 内存演示
./bin/otc -config configs/config.yaml -mysql-dsn "user:pass@tcp(127.0.0.1:3306)/otc?parseTime=true"  # 接入 MySQL，启动自动迁移
```

- `-mysql-dsn` 缺省且无配置 DSN 时降级为内存存储（重启即失）。
- 迁移版本号 **96xx**（`OtcMigrations`）：`ce_otc_advertisements`、`ce_otc_orders`、`ce_otc_counterparties`、`ce_otc_messages`（9605）、`ce_otc_proofs`（9606）。
- 凭证落盘默认 `uploads/otc`，生产建议指向持久化/共享存储并备份；下载做 basename + 前缀双重防护防目录穿越。
- 管理接口（`/orders/:id/resolve`、`/admin/orders`、`/admin/reconcile`）需 `AdminGuard`；消息/凭证接口强制 `orderPartyGuard`（越权 `4030`）。

---

<a id="user"></a>
# 用户个人设置模块

> 服务：`internal/services/user`，路由前缀 `/api/v1/user`。聚焦**个人设置**（资料、改密、偏好）；注册/登录/验证码/2FA/KYC/管理后台共用同一鉴权、信封与错误映射，不在本文重述。

<a id="user-basics"></a>
## 用户设置基础约定

- **Base URL**：`/api/v1/user`
- **认证**：本文接口均在 `middleware.Auth` 鉴权组，需合法 HMAC Bearer Token。
- 统一响应信封见 [通用约定](#common)。

<a id="user-models"></a>
## 用户设置数据模型

### UserProfile（GET /me 返回，data 解包后的扁平结构）

```json
{
  "user_id": 7, "email": "alice@example.com", "phone": "", "nickname": "Alice",
  "avatar": "https://cdn.example.com/a.png", "status": 0, "kyc_level": 2,
  "tfa_enabled": true, "email_verified": true, "phone_verified": false, "kyc": null
}
```

### UserPreferences（偏好设置）

```json
{
  "user_id": 7, "language": "zh-CN", "theme": "light",
  "notify_order": true, "notify_security": true, "notify_marketing": false,
  "updated_at": "2026-08-17T10:00:00Z"
}
```

默认值（未设置时 `GET /preferences` 返回）：`language=zh-CN`、`theme=light`、`notify_order=true`、`notify_security=true`、`notify_marketing=false`。

<a id="user-endpoints"></a>
## 用户设置接口列表

### 个人资料

#### GET `/me`
当前用户档案（含可编辑 `nickname`/`avatar`）。鉴权：User；响应：`data` = `UserProfile`；用户不存在 → `401`。

#### PUT `/me`
更新资料（**部分更新**：省略=不改，显式空串=清空）。鉴权：User；请求体：`{ "nickname": "Alice", "avatar": "https://…" }`（nickname ≤ 32 字符、avatar ≤ 512 字符）；响应：`{ "ok": true }`；超长 → `400`（ErrNicknameTooLong / ErrAvatarTooLong）。

### 修改密码

#### POST `/password`
登录态改密（需校验旧密码）。鉴权：User；请求体：`{ "old_password": "secret123", "new_password": "newpass1" }`；响应：`{ "ok": true, "message": "password changed, please re-login" }`。
- 旧密码错误 → `401`（ErrWrongPassword）
- 新密码与旧密码相同 → `400`（ErrSamePassword）
- 新密码短于最小长度（默认 6）→ `400`（ErrPasswordTooShort）
- **成功后吊销该用户所有 refresh token**，旧 refresh 立即失效；前端应引导重新登录。

### 偏好设置

#### GET `/preferences`
获取偏好（无记录返回默认值，不报错）。鉴权：User；响应：`data` = `UserPreferences`。

#### PUT `/preferences`
保存偏好（全量覆盖）。鉴权：User；请求体：`UserPreferences` 字段（`user_id` 以服务端 token 为准，忽略请求体值）；响应：`{ "ok": true }`；`language`/`theme` 长度 > 32 → `400`（ErrInvalidPref）。

<a id="user-errors"></a>
## 用户设置错误映射

错误响应形如 `{ "code": <码>, "message": "<描述>", "data": null }`；`fail()` 映射：

| HTTP | 触发错误（message） | 场景 |
| --- | --- | --- |
| 401 | `user not found` / `invalid refresh token` / `invalid credential`（ErrWrongPassword）/ `tfa verification failed` / `tfa code required` | 未认证、凭证错误、2FA 缺失 |
| 400 | `user already exists` / `invalid code` / `code expired` / `code already used` / `tfa not enabled` / `kyc already pending` / `kyc submission not pending` / `invalid account format` / `user frozen` | 业务前置校验失败 |
| 400 | `new password must differ from current`（ErrSamePassword） / `invalid preferences`（ErrInvalidPref） / `nickname too long`（ErrNicknameTooLong） / `avatar url too long`（ErrAvatarTooLong） / `password too short`（ErrPasswordTooShort） | **设置接口专用**的客户端错误 |
| 500 | default | 服务端内部异常 |

> 设置接口的客户端错误（昵称/头像超长、新密码与旧密码相同、密码过短、偏好非法）均返回 **400**。

<a id="user-store"></a>
## 用户设置存储与迁移

- `Store` 抽象（MySQL + 内存），迁移由 `UserMigrations` 在 `NewMySQLStore` 时 `Up()` 执行。
- 个人设置相关迁移（与 9101~9104 错开）：
  - **9105** `alter_ce_users_profile`：`ce_users` 增加 `nickname VARCHAR(64)`、`avatar VARCHAR(512)`（默认空串）。
  - **9106** `create_ce_user_preferences`：新建 `ce_user_preferences`（`user_id` 主键，`language`/`theme`/`notify_order`/`notify_security`/`notify_marketing`/`updated_at`）。
- `UpdateUser`/`CreateUser`/查询已纳入 `nickname`/`avatar`；偏好读取对「无记录」容错返回默认值。

<a id="user-frontend"></a>
## 用户设置前端对接

- 类型与封装见 `src/api/client.ts`：`userMe`、`userUpdateProfile({nickname?, avatar?})`、`userChangePassword(old, new)`、`userGetPreferences`、`userUpdatePreferences(prefs)`、`userTfaSetup`/`userTfaEnable`/`userTfaDisable`、`userKycSubmit`/`userKycGet`（及类型 `KycPayload`/`UserKyc`）。
- 页面 `src/pages/Settings.tsx` 作为账户中心，含五块：
  1. **资料**：昵称/头像，`userUpdateProfile`。
  2. **修改密码**：`userChangePassword`；成功后清除本地 token 并跳登录（后端已吊销 refresh）。
  3. **两步验证 (TFA)**：`userTfaSetup` 获取密钥 → `userTfaEnable` 启用；已启用时 `userTfaDisable` 关闭（均验动态码）。
  4. **KYC 认证**：`userKycGet` 展示等级与已提交材料；`userKycSubmit` 提交/重新提交（审核中不可重复提交）。
  5. **偏好设置**：`userUpdatePreferences`；保存后**立即生效**——主题通过 `document.documentElement[data-theme]` 切换浅色/深色 CSS 变量，语言写入 `document.documentElement.lang`（全文 i18n 为独立工作），通知开关持久化供后端 Notifier 消费。
- `userUpdateProfile` 仅传需修改字段（未提供键不出现在请求体），后端按部分更新处理。
- 应用默认主题为深色（`:root` 变量）；选择 `light` 时叠加 `[data-theme="light"]` 变量覆盖。

---

<a id="ann"></a>
# 公告模块

> 服务：`internal/announcement`，作为独立包挂载于用户服务 `cmd/user`（前缀 `/api/v1/announcement`）。
> 与用户模块**共用同一数据库**（同一份 `ce_schema_migrations`），迁移版本号 **9401** 与用户模块 9101–9106 错开。
> 范围：站内公告的发布、列表（公开/管理）、编辑、删除。

<a id="ann-basics"></a>
## 公告基础约定

- **Base URL**：`/api/v1/announcement`
- **认证**：公开列表 `/list` 免鉴权；管理接口 `/admin/*` 需 `middleware.Auth` + `middleware.AdminGuard`（admin 角色）。
- 统一响应信封见 [通用约定](#common)。

<a id="ann-models"></a>
## 公告数据模型

### Announcement

```json
{
  "id": 1, "level": "maintenance", "title": "系统升级通知",
  "content": "今晚 22:00 起维护 30 分钟", "active": true,
  "published_at": "2026-08-17T22:00:00Z", "created_at": "2026-08-17T10:00:00Z",
  "updated_at": "2026-08-17T10:00:00Z"
}
```

| 字段 | 说明 |
| --- | --- |
| id | 主键 |
| level | 等级：`info`（公告）/ `warning`（提醒）/ `maintenance`（维护），影响前端 badge 样式 |
| title | 标题（必填，≤ 128 字符） |
| content | 正文（可空，≤ 4096 字符） |
| active | 是否对外发布（草稿=false 不出现在公开列表） |
| published_at | 发布时间；草稿为 0 值字符串；切到发布态且无该值时自动填充为当前时间 |

<a id="ann-endpoints"></a>
## 公告接口列表

路径省略前缀 `/api/v1/announcement`。鉴权：`Public`=免鉴权；`Admin`=需管理员（`Auth + AdminGuard`）。

#### GET `/list`
公开公告列表（仅已发布 `active=true`），按发布时间倒序。鉴权：Public；响应：`{ "announcements": [...] }`（数组元素为 `Announcement`）。首页公告横幅、公告页均消费此接口。

#### GET `/admin`
全量公告列表（含草稿）。鉴权：Admin；响应：`{ "announcements": [...] }`。

#### POST `/admin`
创建公告。鉴权：Admin；请求体（字段均可选，但 `title` 必填）：

```json
{ "level": "maintenance", "title": "系统升级通知", "content": "今晚维护", "active": true }
```

响应：`data` = `Announcement`。校验失败 → `400`（见 [错误映射](#ann-errors)）。`active=true` 且 `published_at` 缺省时自动填充。

#### PUT `/admin/:id`
更新公告（部分更新：仅提供的字段生效）。鉴权：Admin；请求体同上；响应：`data` = `Announcement`。id 不存在 → `404`。

#### DELETE `/admin/:id`
删除公告。鉴权：Admin；响应：`{ "ok": true }`；id 不存在 → `404`。

<a id="ann-errors"></a>
## 公告错误映射

`fail()` 映射：

| HTTP | 触发错误（message） | 场景 |
| --- | --- | --- |
| 401 | — | 未认证（管理接口缺 Token） |
| 403 | `insufficient role` | 非 admin 调用 `/admin/*` |
| 400 | `title required`（ErrTitleRequired） | 创建时未提供 title |
| 400 | `title too long`（ErrTitleTooLong） | title 超过 128 字符 |
| 400 | `content too long`（ErrContentTooLong） | content 超过 4096 字符 |
| 400 | `invalid level (info/warning/maintenance)`（ErrInvalidLevel） | level 非法 |
| 404 | `announcement not found`（ErrNotFound） | 更新/删除的 id 不存在 |
| 500 | default | 服务端内部异常 |

<a id="ann-store"></a>
## 公告存储与迁移

- `Store` 抽象（`MySQL` + `内存`），内存实现用于单测/无 DB 开发；生产用 MySQL。
- 迁移版本 **9401** `create_ce_announcements`：新建 `ce_announcements`
  （`id` 主键、`level VARCHAR(16)`、`title VARCHAR(128)`、`content TEXT`、`active TINYINT`、
  `published_at DATETIME(3)`、`created_at`/`updated_at`，索引 `idx_active_published(active, published_at)`）。
- 与用户模块共用同一数据库实例，因此两者共享 `ce_schema_migrations`；版本号 9401 与用户的 9101–9106 互不重叠，迁移幂等、可重入。
- 在 `cmd/user` 中：`cfg.MySQL.DSN` 非空时分别开 `user.NewMySQLStore(dsn)` 与 `announcement.NewMySQLStore(dsn)`（各自运行自己的迁移）；DSN 缺失则两者均降级为内存实现。

<a id="ann-admin-backend"></a>
## 管理后端（cmd/admin）接入

独立管理后端 `cmd/admin`（监听 `:8090`，路由前缀 `/api/admin`）同样承载公告管理，复用同一套 `internal/announcement` 的 `Handler`/`Service`/`Store`，与 `cmd/user` 共享 `ce_announcements` 表（迁移版本 9401）。

- 注册位置：`adminapi.Server.RegisterRoutes` 在已套 `middleware.Auth + middleware.AdminGuard` 的 `admin` 分组下调用 `annH.RegisterAdminRoutes(admin)`，无需重复鉴权。
- 存储：与 `cmd/admin` 其他模块一致——`cfg.MySQL.DSN` 非空时开 `announcement.NewMySQLStore(dsn)`，否则降级为内存实现。
- 接口（前缀 `/api/admin`，均 Admin 鉴权）：

| 方法 | 路径 | 说明 | 响应 |
| --- | --- | --- | --- |
| GET | `/api/admin/announcements` | 全量列表（含草稿） | `{ "announcements": [...] }` |
| POST | `/api/admin/announcements` | 创建 | `data` = `Announcement`（**直接返回对象**，非 `announcements` 数组） |
| PUT | `/api/admin/announcements/:id` | 部分更新 | `data` = `Announcement` |
| DELETE | `/api/admin/announcements/:id` | 删除 | `{ "ok": true }` |

> 说明：`cmd/admin` 与管理接口前缀不同（`/api/admin/announcements` vs `cmd/user` 的 `/api/v1/announcement/admin`），但底层 `Handler` 与错误映射（`fail()`）完全一致；两者数据互通，运维可任选一端维护。

<a id="admin-ledger"></a>
## 运营账本与结算实时账本（cmd/admin）

`GET /api/admin/ledger` 返回账本对账快照 `LedgerSummary`，实时聚合来自上游服务：

- **futures 合约钱包对账**（若配置了 `services.futures`）：调用 `/api/v1/futures/wallet/reconcile` 与 `/api/v1/futures/wallet/inventory`，汇总 `total_assets` / `settlement_balance`（链上总量）、`reconciled`（是否平衡）、`discrepancy`（各类偏差绝对值之和）。
- **settlement 清算聚合**（若配置了 `services.settlement`）：调用 `/api/v1/settlement/stats` 与 `/api/v1/settlement/cleared?limit=10`，填充 `settlement` 块（`enabled` / `total_trades` / `total_volume` / `total_commission` / `by_symbol` / `recent[]`）。`recent` 中每笔 `ClearedTradeView` 的 `ts` 为 Unix **毫秒**，前端直接 `new Date(ts)` 格式化。

任一上游未配置或拉取失败时降级：`settlement.enabled=false`、对应 `notes` 说明原因，并由顶层 `notes` 汇总。`settlement` 服务健康检查走 `/healthz`（其余上游走 `/health`，见服务健康接口 `GET /api/admin/services`）。

<a id="admin-orders"></a>
## 订单与成交查询（cmd/admin）

`GET /api/admin/orders`（需 `trade:read`）与 `GET /api/admin/trades`（需 `trade:read`）提供跨用户订单/成交流水查询（运营风控用）：

- **过滤**：`user_id`（不传=全部用户）、`symbol`、`market`（`spot`|`futures`）、`margin`（杠杆标识）；订单另支持 `status`（`open`/`partial`/`filled`/`canceled`/`rejected`）。
- **分页**：`limit`（默认 50，上限 500）、`offset`（默认 0）。响应统一为 `{ "orders"|"trades": [...], "total": n }`，`total` 为过滤后总条数（不受分页截断影响），前端 `Orders.tsx` 据此接入 `Pager`。
- 撤销任意用户订单：`POST /api/admin/orders/:id/cancel`（需 `trade:manage`，危险操作），请求体 `{ "symbol": "BTCUSDT" }`（撮合引擎按 symbol 定位订单簿）。

<a id="admin-audit"></a>
## 审计日志（cmd/admin）

管理后台在 `admin` 分组上挂审计中间件，对所有**变更类**请求（POST/PUT/DELETE，GET 不记）落审计记录（仅元数据：方法/路由/状态码/IP/时间，不记录请求体以防泄露口令）。存储同其他模块（`cfg.MySQL.DSN` 非空时 `ce_admin_audit_logs` 表，迁移版本 9801；否则内存回退）。

| 方法 | 路径 | 说明 | 权限 | 响应 |
| --- | --- | --- | --- | --- |
| GET | `/api/admin/audit-logs` | 审计日志（按时间倒序），支持 `limit`/`offset` 分页 | `audit:read` | `{ "logs": [...], "total": n }` |

> 审计中间件在服务端 `c.Next()` 之后记录，故同时覆盖成功与失败（含 4xx/5xx）的变更尝试；`audit:read` 已纳入 `super_admin` 默认角色。

<a id="admin-paging"></a>
## 列表分页约定（cmd/admin）

`users` / `symbols` / `chains` / `coins` / `notifications` / `announcements` / `admins` / `roles` 等列表接口支持 `limit`（默认 50，上限 500）与 `offset` 分页，响应统一包络 `{ "items": [...], "total": n }`（公告为 `{ "announcements": [...], "total": n }`）。上游聚合类列表同样支持分页：`orders`/`trades` 返回 `{ orders|trades: [...], total }`；`deposits`/`withdrawals` 返回 `{ deposits|withdrawals: [...], total }`，并可在服务端按 `user_id` / `coin` / `status` 过滤（充值/提币列表的 `total` 为过滤后总数，不受分页截断影响）。

<a id="admin-rbac"></a>
## 角色与权限管理（cmd/admin）

角色与权限管理接口统一受 `role:manage` 守卫，管理员账户管理接口受 `admin:manage` 守卫：

| 方法 | 路径 | 说明 | 权限 |
| --- | --- | --- | --- |
| GET | `/api/admin/roles` | 角色列表（含各自权限），分页包络 `{ "items": [...], "total": n }` | `role:manage` |
| POST | `/api/admin/roles` | 新建角色（初始无权限），`{ "name", "description" }` | `role:manage` |
| PUT | `/api/admin/roles/:id` | 编辑角色名/描述，`{ "name", "description?" }`，改名与已有角色重名返回 409 | `role:manage` |
| PUT | `/api/admin/roles/:id/permissions` | 全量覆盖角色权限，`{ "permissions": string[] }`（非法的 key 会被 `ValidatePermissions` 过滤） | `role:manage` |
| DELETE | `/api/admin/roles/:id` | 删除角色；仍被管理员引用时返回 409 `ErrRoleInUse` | `role:manage` |
| GET | `/api/admin/permissions` | 全部可授予权限字典（供权限分配 UI 按 group 分组） | `role:manage` |
| GET | `/api/admin/admins` | 管理员列表（分页包络） | `admin:manage` |
| POST | `/api/admin/admins` | 新建管理员（需指定 `role_id`，初始 `pending`） | `admin:manage` |
| PUT | `/api/admin/admins/:id` | 改派角色 / 改状态，`{ "role_id"?, "status"?" }` | `admin:manage` |
| POST | `/api/admin/admins/:id/activate` · `/disable` · `/reset-password` | 激活 / 禁用 / 重置密码 | `admin:manage` |

<a id="admin-apikeys"></a>
## API Key 管理（cmd/admin）

管理员可为任意用户签发 / 列出 / 查询 / 吊销 API Key。读操作受 `apikey:read` 守卫，
签发与吊销受 `apikey:manage` 守卫（高危）。密钥采用「明文一次性返回 + 仅存哈希」模型：

> 路由入口：接口同时可由管理后端直连（`cmd/admin :8095`，开发态 web-admin 经 `:5174` 代理）
> 与经由统一网关（`/api/admin/apikeys`，网关对 `/api/admin` 整段豁免普通用户鉴权、改由
> admin 后端校验 admin token）两种方式访问；网关层仍叠加安全头 / 审计等通用中间件。

- 明文 Key 形如 `cxk_<prefix>_<secret>`，仅在**创建接口的响应**中返回一次；
- 存储层只保存 `sha256(明文 Key)`（`key_hash`）与前缀 `prefix`，永不保存明文；
- 校验方（网关 / 用户服务）拿到明文后解析 prefix+secret，重算哈希并经 `GetByKeyHash`
  比对，并校验 `status == active`；本模块不提供在线校验接口，仅负责签发与吊销。

| 方法 | 路径 | 说明 | 权限 |
| --- | --- | --- | --- |
| GET | `/api/admin/apikeys` | API Key 列表（分页包络 `{ "items": [...], "total": n }`），支持 `?user_id=` 过滤 | `apikey:read` |
| POST | `/api/admin/apikeys` | 为某用户签发 Key，`{ "user_id": int, "label": string, "permissions"?: string[] }`；响应 `{ "key": "<明文，仅此一次>", "api_key": {...视图} }` | `apikey:manage` |
| GET | `/api/admin/apikeys/:id` | 单条 Key 详情（视图不含 `key_hash`） | `apikey:read` |
| DELETE | `/api/admin/apikeys/:id` | 吊销指定 Key（`status -> revoked`，记录 `revoked_at`）；已吊销返回 409，不存在返回 404 | `apikey:manage` |

列表项 / 详情视图字段：`id, user_id, label, prefix, permissions[], status("active"|"revoked"),
created_by, created_at, last_used_at?, revoked_at?`。吊销为软删除，保留审计轨迹。

<a id="ann-frontend"></a>
## 公告前端对接

- 类型与封装见 `src/api/client.ts`：`listAnnouncements()`（公开）、`adminListAnnouncements()`、`adminCreateAnnouncement(payload)`、`adminUpdateAnnouncement(id, payload)`、`adminDeleteAnnouncement(id)`，及类型 `Announcement`/`AnnouncementInput`/`AnnouncementLevel`。
- 页面：
  - `src/pages/Home.tsx` 首页大盘：欢迎语 + 平台公告横幅（消费 `listAnnouncements`）+ 模块快捷入口 + 账户概览（消费 `userMe`）。已设为默认路由 `/home`。
  - `src/pages/Announcements.tsx` 公告管理：增删改查（需 admin 角色；无权限时列表接口返回 403，由错误提示展示）。
- 导航栏新增「首页 /home」「公告 /announcements」。
- 公告等级 badge 样式（`info` 绿 / `warning` 黄 / `maintenance` 蓝）见 `src/styles.css` 的 `.ann-badge.*`。

<a id="ann-admin-frontend"></a>
## 管理后台前端（ce-admin-web）对接

独立管理前端 `ce-admin-web`（Vite，开发端口 `:5174`，代理 `/api/admin -> cmd/admin :8095`）提供公告管理页：

- API 封装见 `ce-admin-web/src/api/client.ts`：`listAnnouncements()`、`createAnnouncement(payload)`、`updateAnnouncement(id, payload)`、`deleteAnnouncement(id)`（与 `cmd/admin/announcements` 一一对应）。
- 页面 `ce-admin-web/src/pages/Announcements.tsx`：新建/编辑（同一表单按 `editing` 态切换 POST/PUT）、删除（确认后 DELETE）、等级徽标与「已发布/草稿」状态展示。
- 路由 `#/announcements`，导航栏新增「公告管理」项（无权限门槛，整体受 `Auth+AdminGuard` 保护）。

> 面向运维/管理员的「操作示例（curl）与字段校验」见 [`announcement_guide.md`](./announcement_guide.md)。
> 单元测试运行方式与用例清单见 [`announcement_test.md`](./announcement_test.md)。

---

<a id="wealth"></a>
# 理财资管模块

> 服务：`internal/wealth`，独立二进制 `cmd/wealth`（监听 `:8092`），路由前缀 `/api/v1/wealth`。
> 范围：理财产品发行/上下架、用户认购/赎回、持仓与收益计提（中央托管模型）。

<a id="wealth-basics"></a>
## 理财基础约定

- **Base URL**：`/api/v1/wealth`
- **认证**：所有接口均经 `middleware.Auth` 鉴权（Bearer Token）；`/products` 创建与 `/admin/*` 还需 `AdminGuard`（admin 角色）。
- **中央托管模型**：用户认购时本金从个人可用余额转入 `SysWealth`；赎回时本金+已计收益从 `SysWealth` 支出给用户。收益按「本金 × 年化 × 持有小时 / 8760」连续计息（定点累加，避免 float 漂移）；定期产品到期前锁定不可赎。

<a id="wealth-models"></a>
## 理财数据模型

### WealthProduct（理财产品）

```json
{
  "id": 1, "name": "USDT 稳健增利", "asset": "USDT", "type": "fixed",
  "annual_rate": 0.05, "duration_days": 30, "min_amount": 100,
  "status": "open", "created_at": "2026-08-17T10:00:00Z", "updated_at": "2026-08-17T10:00:00Z"
}
```

| 字段 | 说明 |
| --- | --- |
| id | 主键 |
| name | 产品名称 |
| asset | 底层资产（如 `USDT`），本金与收益计价单位 |
| type | `current`（活期，可随时赎回）/ `fixed`（定期，到期前锁定） |
| annual_rate | 年化收益率（0.05 = 5%） |
| duration_days | 锁定期限（天）；活期为 0 |
| min_amount | 起购金额 |
| status | `open`（申购中）/ `closed`（已下架） |

### WealthHolding（用户持仓）

```json
{
  "id": 10, "user_id": 7, "product_id": 1, "asset": "USDT",
  "principal": 1000, "accrued_yield": 4.12, "status": "active",
  "created_at": "2026-08-17T10:00:00Z", "last_accrual_at": "2026-08-18T10:00:00Z",
  "redeemed_at": null, "updated_at": "2026-08-18T10:00:00Z"
}
```

> `principal` / `accrued_yield` 由后端按**人类可读十进制数字**序列化（JSON 数字），前端按 `number` 处理即可。
> `status`：`active`（持有中，可赎回/计息）/ `funding`（瞬态：本金转入中）/ `redeemed`（已赎回，终态）。

<a id="wealth-endpoints"></a>
## 理财接口列表

路径省略前缀 `/api/v1/wealth`。鉴权：`User`=需登录；`Admin`=需管理员（`Auth + AdminGuard`）。

#### GET `/products`
理财产品列表。鉴权：User；查询参数 `status`（可选，`open`/`closed`）。响应：`{ "products": [...] }`。前端认购下拉仅用 `open` 产品。

#### POST `/products`
创建产品（发行）。鉴权：Admin；请求体：

```json
{ "name": "USDT 稳健增利", "asset": "USDT", "type": "fixed",
  "annual_rate": 0.05, "duration_days": 30, "min_amount": 100 }
```

- `type` 必为 `current`/`fixed`；`annual_rate`/`duration_days` 须 ≥ 0；否则 → `4001`。
- 响应：`data` = `WealthProduct`；业务错误（如重复） → `4002`。

#### POST `/subscribe`
认购。鉴权：User；请求体：`{ "product_id": 1, "amount": 1000 }`。
- 校验：金额 > 0、产品存在且 `open`、金额 ≥ 起购额（定点比较）、用户可用余额充足；否则 → `4001`/`4002`。
- 行为：先落 `funding` 瞬态持仓 → 本金 `user → SysWealth` → 置为 `active`；任一失败回滚。
- 响应：`data` = `WealthHolding`（status=`active`）。

#### POST `/redeem`
赎回。鉴权：User；请求体：`{ "holding_id": 10 }`。
- 校验：须为本人持仓（`ErrNotOwner`）、须为 `active`（重复赎回短路返回 `ErrAlreadyRedeemed`）、定期须到期（否则 `ErrLocked`）。
- 行为：赎回前补齐截至当前的收益 → 本金+收益 `SysWealth → user` → 置 `redeemed` 并写 `redeemed_at`（幂等）。
- 响应：`data` = `WealthHolding`（status=`redeemed`）。

#### GET `/holdings`
我的持仓。鉴权：User；响应：`{ "holdings": [...] }`（仅本人）。

#### GET `/admin/holdings`
全量持仓（管理视图）。鉴权：Admin；响应：`{ "holdings": [...] }`。

#### POST `/admin/accrue`
手动计提收益（通常由后台循环自动执行）。鉴权：Admin；响应：`{ "accrued": <本轮回填总收益（人类单位）> }`。

<a id="wealth-errors"></a>
## 理财错误映射

错误响应形如 `{ "code": <码>, "message": "<描述>", "data": null }`；各接口直接以 `response.Error` 返回以下码：

| HTTP | code | 触发场景 / message |
| --- | --- | --- |
| 401 | 4010 | 缺失/非法 Token（`unauthorized`） |
| 400 | 4000 | 请求体非法（JSON 解析失败） |
| 400 | 4001 | 参数校验：`type must be current or fixed` / `annual_rate must be >= 0` / `duration_days must be >= 0` / `product_id/amount required` / `holding_id required` |
| 400 | 4002 | 业务规则错误（`message` 为具体原因）：product not found、product not open、amount below product minimum、insufficient available balance、not the holding owner、holding already redeemed、fixed product is still locked before maturity、amount must be positive 等 |
| 500 | 5000 | 服务端内部异常（持久化/账本转账等） |

<a id="wealth-store"></a>
## 理财存储与迁移

- `Store` 抽象（MySQL + 内存），内存实现用于单测/无 DB 演示；生产用 MySQL。
- 迁移版本 **97xx**（与全局错开）：
  - **9701** `create_ce_wealth_products`：产品表（id/name/asset/type/annual_rate/duration_days/min_amount/status/时间，索引 `idx_status`）。
  - **9702** `create_ce_wealth_holdings`：持仓表（id/user_id/product_id/principal/accrued_yield/status/时间，索引 `idx_user`/`idx_product`）。
  - **9703** `alter_ce_wealth_holdings_fixedpoint`：本金/收益由 `DOUBLE` 改为 `VARCHAR(64)` 定点存储（避免 float 精度漂移），新增 `asset` 列用于扫描时推导小数位。
- `cmd/wealth/main.go` 装配：配置 DSN 则连 MySQL 并跑迁移，否则内存；并启动后台 `RunLoop` 周期调用 `Accrue` 持续计提收益。

<a id="wealth-frontend"></a>
## 理财前端对接

- 类型与封装见 `src/api/client.ts`：`wealthProducts()`、`wealthHoldings()`、`wealthSubscribe(productId, amount)`、`wealthRedeem(holdingId)`，及类型 `WealthProduct`/`WealthHolding` 等。
- 页面 `src/pages/Wealth.tsx`：
  - **认购**：从「申购中(open)」产品下拉选择，输入金额（前端校验 > 0 且 ≥ 起购额），调用 `wealthSubscribe`。
  - **我的持仓**：表格展示本金/已计收益/状态；对 `active` 持仓显示「赎回」按钮，调用 `wealthRedeem`（非活跃项禁用）。
- 路由 `/wealth` 已注册，导航栏含「理财」。
- 注意：认购前需管理员先 `POST /api/v1/wealth/products` 发行产品，否则前端下拉为空、无法认购。

---

<a id="wallet"></a>
## 钱包与资金流水（合约钱包服务）

> 服务：`internal/futuresapi`（合约钱包服务），路由前缀 `/api/v1/futures/wallet`。
> 该服务持有交易所用户钱包账本（`internal/ledger`），记录充值 / 提现 / 转账 / 资金费 / 强平 / 坏账补缴等资金变动。

<a id="wallet-models"></a>
### 资金流水数据模型

`LedgerEntry`（用户侧资金流水条目）：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | int | 流水序号（自增） |
| user_id | int | 账户主体（恒为当前登录用户） |
| asset | string | 资产，如 `USDT` |
| delta | number | 变动额：`+` 入账 / `-` 出账（JSON 数字） |
| balance | number | 变动后的可用余额（JSON 数字） |
| biz_type | string | 业务类型（见下） |
| ref | string | 关联单号（订单号 / 链上哈希 / 提现 hold_id） |
| time | int | Unix 纳秒时间戳 |

`biz_type` 取值：`deposit`(充值) / `withdraw`(提现) / `transfer`(转账) / `funding`(资金费) / `liquidation`(强平) / `repay`(坏账补缴) / `open`(开仓) / `close`(平仓) / `fee`(手续费)。

<a id="wallet-endpoints"></a>
### 接口列表

- `GET /api/v1/futures/wallet/ledger` —— 查询**本人**资金流水。
  - 鉴权：User（需登录；身份**强制取自 Token**，忽略任何 `user_id` 参数，防止冒充查询他人流水）。
  - 查询参数（可选）：`asset`（按资产过滤）、`limit`（截断条数，0/不传为全部）。
  - 响应：`data = { entries: LedgerEntry[] }`，**按时间倒序（最新在前）**，仅含调用者本人跨全部资产的流水。
  - 错误：401 未认证；500 服务端内部异常。

> **架构说明**：本仓库账本为各服务**内嵌实例**。本接口返回的是合约钱包账本中该用户的流水（充值 / 提现 / 转账 / 资金费 / 强平 / 坏账补缴等钱包级变动）。现货成交、OTC 成交各自写入独立的账本实例，未在本接口聚合；如需统一全量资金流水，需建设跨服务的共享账本 / 聚合层（更大的重构）。

<a id="wallet-frontend"></a>
### 前端对接

- 类型与封装见 `src/api/client.ts`：`walletLedger(params?)`、类型 `LedgerEntry`。
- 页面 `src/pages/Wallet.tsx` 钱包页新增「资金流水」分区，表格展示时间 / 资产 / 类型 / 变动(±) / 余额 / 关联单号，支持按资产或业务类型前端筛选。

---

<a id="notify"></a>
# 站内通知模块

> 服务：`internal/notification`（独立二进制 `cmd/notification`，监听 `:8088`）。
> 用户侧前缀 `/api/v1/user/notifications`；管理/写入侧前缀 `/api/v1/notification`（由 notification 服务自身或业务服务调用）。
> 范围：强平、保证金预警、KYC、充值到账、提现完成、系统公告等站内信的统一收发。

<a id="notify-basics"></a>
## 通知基础约定

- **Base URL**：用户侧 `/api/v1/user/notifications`；写入侧 `/api/v1/notification`。
- **认证**：用户侧接口需 `middleware.Auth` Bearer Token（仅查本人）；写入侧由内部服务调用（网关/鉴权由调用方保证）。
- 统一响应信封见 [通用约定](#common)。

<a id="notify-models"></a>
## 通知数据模型（`UserNotification` 投影）

业务内部为 `{type, body, status}`，对外投影为前端契约：

```json
{
  "id": "123",                 // 字符串（前端 id:string 契约）
  "type": "liquidation",       // 见下方类型枚举
  "level": "critical",         // info | warning | critical
  "content": "您的 BTC_USDT_PERP long 仓位已被强制平仓...",
  "read": false,               // bool（内部 status: unread/read 投影）
  "created_at": "2026-08-26T10:00:00Z"
}
```

| 类型 type | level | 说明 |
| --- | --- | --- |
| `liquidation` | critical | 合约仓位被强平（部分/全额） |
| `margin_warning` | warning | 保证金率逼近强平价预警 |
| `risk_alert` | critical | 风控告警 |
| `kyc_rejected` | warning | KYC 审核被拒 |
| `kyc_approved` | info | KYC 审核通过 |
| `deposit_arrived` | info | 链上充值到账 |
| `withdraw_done` | info | 提现完成 |
| `system` | info | 系统通知 |

<a id="notify-endpoints"></a>
## 接口列表

路径省略前缀 `/api/v1/user/notifications`。鉴权：User。

#### GET `/`
列表（含未读计数）。响应：`{ "notifications": [UserNotification...], "unread": 3 }`。

#### GET `/unread-count`
未读数量。响应：`{ "count": 3 }`。

#### POST `/:id/read`
标记单条已读（路径参数 `:id`）。响应：`{ "ok": true }`；越权/不存在 → `404`。

#### POST `/read-all`
全部标记已读。响应：`{ "ok": true }`。

#### DELETE `/:id`
删除单条。响应：`{ "ok": true }`；重复删除 → `404`。

<a id="notify-errors"></a>
## 通知错误映射

| HTTP | code | 场景 |
| --- | --- | --- |
| 401 | 401 | 未认证 / Token 非法 |
| 404 | 404 | 非本人通知 / 不存在 |
| 500 | 500 | 服务端内部异常 |

<a id="notify-events"></a>
## 业务事件来源（§37）

由业务服务在关键事件发生时调用 `notification.Service.Publish` 写入：

- **合约强平**（`internal/futuresapi`）：`onLiquidation` → 经 `broadcastLiquidations` → `publishLiquidationNotice`，向被强平用户推送 `liquidation` 站内信。
- **保证金预警**（`internal/futuresapi`）：`liqScanLoop` 每轮 `emitMarginWarnings` 扫描所有仓位，保证金率低于 `MarginWarnRatio=1.2` 时推送 `margin_warning` 站内信（每 user+symbol 内存去重，行情回升后再次下跌可重新触发）。
- 其余类型（KYC/充值/提现/系统）由对应业务线写入。

---

<a id="security"></a>
# 安全中心模块

> 服务：`internal/services/user`，前缀 `/api/v1/user`（与用户设置同域）。
> 范围：API Key 管理、登录历史、会话管理、防钓鱼码。

<a id="security-basics"></a>
## 安全中心基础约定

- **Base URL**：`/api/v1/user`
- **认证**：所有接口均经 `middleware.Auth` 鉴权（Bearer Token）；API Key 创建/列举/启停/吊销另受本人归属校验（跨用户越权 → `404`）。
- 统一响应信封见 [通用约定](#common)。

<a id="security-endpoints"></a>
## 接口列表

### API Key

#### POST `/api-keys`
创建 API Key。鉴权：User；请求体：`{ "label": "my-bot", "permissions": ["read","trade"], "ip_whitelist": "1.2.3.4,5.6.7.8" }`。
- `permissions` ⊂ `read|trade|withdraw`；非法值 → `400`。
- 响应：`{ "api_key": { "key": "<明文 cxk_...>", "id": 1, "label": "...", "permissions": [...], "status": "active" } }`；**明文 key 仅创建响应返回一次**，存储只落 `sha256` 哈希。

#### GET `/api-keys`
列表。鉴权：User；响应：`{ "api_keys": [...], "total": n }`（不含 secret/哈希）。

#### PUT `/api-keys`
启用/停用。鉴权：User；请求体：`{ "id": 1, "status": "active"|"disabled" }`；跨用户 → `404`。

#### DELETE `/api-keys/:id`
吊销（硬删）。鉴权：User；响应：`{ "ok": true }`；跨用户 → `404`。

### 登录历史

#### GET `/login-history`
登录记录（成功/失败均记录）。鉴权：User；响应：`{ "entries": [{ "id": "171...", "ip": "127.0.0.1", "location": "本地", "success": true, "created_at": "..." }] }`；`id` 为字符串（前端 id:string 契约）。

### 会话

#### GET `/sessions`
当前用户会话列表。鉴权：User；响应：`{ "sessions": [{ "id": "...", "current": true, "last_active_at": "..." }] }`；`current` 由「最近活跃」启发式推导（当前会话标记）。

#### POST `/sessions/:id/revoke`
注销指定会话。鉴权：User；注销当前会话 → `400`；不存在 → `404`；响应：`{ "ok": true }`。

#### POST `/sessions/revoke-all`
注销除当前外的所有会话。鉴权：User；响应：`{ "revoked": 2 }`（被注销数）。

### 防钓鱼码

#### GET `/anti-phishing`
获取防钓鱼码（未设置为空串）。鉴权：User；响应：`{ "code": "XYZ789" }`。

#### POST `/anti-phishing`
设置/清除。鉴权：User；请求体：`{ "code": "XYZ789" }`（空串清除）；超长（>32）→ `400`；响应：`{ "ok": true }`。

<a id="security-errors"></a>
## 安全中心错误映射

| HTTP | code | 场景 |
| --- | --- | --- |
| 401 | 401 | 未认证 / Token 非法 |
| 400 | 400 | 参数校验失败（permissions 非法、防钓鱼码超长、注销当前会话） |
| 404 | 404 | 非本人资源 / 不存在 |
| 500 | 500 | 服务端内部异常 |

---

<a id="earn"></a>
# 理财中心 / 新币挖矿模块

> 服务：`internal/earn`（独立二进制 `cmd/earn`，监听 `:8093`）。
> 路由前缀：`/api/v1/earn`（理财中心：活期/定期申购、计息、赎回）、`/api/v1/launchpad`（新币挖矿：项目、质押、领奖、解押）。

<a id="earn-basics"></a>
## 理财基础约定

- **Base URL**：`/api/v1/earn` 与 `/api/v1/launchpad`
- **认证**：用户接口需 `middleware.Auth`；管理接口（`/admin/*`）需 `AdminGuard`。
- **中央托管模型**：申购本金 `user → SysWealth`（理财）/ `user → SysStaking`（挖矿），赎回/领奖时由系统账户支出；计息定点整数（`YIELD_SCALE`），预算闸口在服务层显式校验。

<a id="earn-models"></a>
## 数据模型

### EarnProduct / EarnSubscription
与 [理财资管模块](#wealth) 的 `WealthProduct`/`WealthHolding` 结构类似（活期/定期/年化/起购/持仓本金/已计收益），差异在于本模块由 `cmd/earn` 独立服务托管，路由前缀为 `/api/v1/earn`。

### LaunchProject / LaunchPosition
| 字段 | 说明 |
| --- | --- |
| id | 项目 ID |
| asset | 奖励资产 |
| status | upcoming / ongoing / ended（由 now 推导） |
| funded_total | 管理员预充预算（库存，供对账） |
| stake_amount | 用户质押量 |

<a id="earn-endpoints"></a>
## 接口列表

路径省略前缀 `/api/v1/earn`。鉴权：User；Admin 接口需 `AdminGuard`。

#### GET `/products`
理财产品列表（含 `current`/`fixed`）。响应：`{ "products": [...] }`。

#### POST `/subscribe`
申购。请求体：`{ "product_id": 1, "amount": 1000 }`；需勾选风险揭示 `agreed`（前端/服务端双校验）；响应：`data` = 持仓。

#### POST `/redeem`
赎回（定期未到期拒绝）。请求体：`{ "holding_id": 10 }`；响应：`data` = 持仓（status=redeemed）。

#### GET `/subscriptions`
我的持仓。响应：`{ "subscriptions": [...] }`。

#### GET `/admin/products`
管理：产品列表。

#### POST `/admin/products`
管理：创建产品。

#### POST `/admin/accrue`
管理：手动计提（通常后台 60s 循环自动执行）。响应：`{ "accrued": <本轮回填总额> }`。

路径省略前缀 `/api/v1/launchpad`。鉴权：User；Admin 接口需 `AdminGuard`。

#### GET `/projects`
挖矿项目列表（含状态推导）。响应：`{ "projects": [...] }`。

#### POST `/stake`
质押。请求体：`{ "project_id": 1, "amount": 100 }`；响应：`data` = 仓位。

#### POST `/harvest`
领奖。响应：`data` = 领取记录；预算不足 → `ErrPoolExhausted`（先持久化 Pending，fail-safe）。

#### POST `/unstake`
解押。请求体：`{ "position_id": 1, "amount": 0 }`（`amount=0` 全额）；响应：`data` = 仓位。

#### POST `/admin/projects`
管理：创建项目（id 唯一、资产白名单校验）。

#### POST `/admin/projects/:id/fund`
管理：预充奖励预算 `token → SysStakingReward`。

<a id="earn-errors"></a>
## 理财错误映射

| HTTP | code | 场景 |
| --- | --- | --- |
| 401 | 401 | 未认证 / Token 非法 |
| 400 | 400 | 参数/业务前置校验（未勾选风险揭示、min/max 超限、定期锁定、预算不足） |
| 404 | 404 | 产品/项目/持仓不存在 |
| 500 | 500 | 服务端内部异常（账本转账） |

<a id="earn-store"></a>
## 存储与迁移

- 迁移 **9611–9614**：`ce_earn_products` / `ce_earn_subscriptions` / `ce_launch_projects`（含 `funded_total`）/ `ce_launch_positions`；金额列 `VARCHAR(64)` 定点存储。
- MemStore + MySQLStore 同构；`cmd/earn/main.go` 配置 DSN 则连 MySQL 跑迁移，否则内存。

<a id="earn-frontend"></a>
## 前端对接

- 类型与封装见 `src/api/client.ts`：`earnProducts()`、`earnSubscribe(id, amount)`、`earnRedeem(holdingId)`、`earnSubscriptions()`；`launchProjects()`、`launchStake(id, amt)`、`launchHarvest()`、`launchUnstake(posId, amt)`。
- 页面 `src/pages/Earn.tsx` / `src/pages/Launchpad.tsx` 已按上述契约对齐；mock 网关路由族一致，切真实后端即插即用。

---

<a id="copytrade"></a>
# 跟单交易模块

> 服务：`internal/copytrade`（独立二进制 `cmd/copytrade`，监听 `:8099`）。
> 路由前缀：`/api/v1/copytrade`。范围：带单高手（lead）注册、粉丝关注（follow）、自动复制成交、平台复制费结算。

<a id="copytrade-basics"></a>
## 跟单基础约定

- **Base URL**：`/api/v1/copytrade`
- **认证**：用户接口需 `middleware.Auth`；管理接口需 `AdminGuard`。
- **自动跟单数据流**：撮合引擎成交 → Kafka `exchange.trades` → `cmd/copytrade` 订阅 → `svc.OnTrade` → 识别被跟单 lead → 按 `copyRatio` 计算粉丝名义额（不超过 `allocatedAmount`）→ 以粉丝授权 token 经下游 `spot/futures` 的 `/order` 代下单（F4）→ 平台复制费 `follower → SysCopyTradeFee`（F2 定点）。
- **F1 幂等**：`(eventID, followID)` 去重；`client_oid = copytrade:<followID>:<eventID>` 下游再防重。

<a id="copytrade-models"></a>
## 数据模型

| 结构 | 关键字段 |
| --- | --- |
| LeadTrader | id, name, bio, status(active/closed) |
| Follow | id, lead_id, follower_id, copy_ratio, allocated_amount, follower_token, status(active/stopped) |
| CopyRecord | id, lead_id, follower_id, symbol, side, price, qty, notional, fee_amount, exchange_order_id, status(done/failed) |

<a id="copytrade-endpoints"></a>
## 接口列表

路径省略前缀 `/api/v1/copytrade`。鉴权：User；Admin 接口需 `AdminGuard`。

#### POST `/leads`
注册为带单高手。请求体：`{ "name": "TraderA", "bio": "..." }`；响应：`data` = LeadTrader。

#### GET `/leads`
在售带单列表。响应：`{ "leads": [...] }`。

#### POST `/leads/:id/close`
关闭带单（仅本人）。响应：`{ "ok": true }`。

#### POST `/follows`
关注并授权跟单。请求体：`{ "lead_id": 1, "copy_ratio": 0.1, "allocated_amount": 1000, "follower_token": "<粉丝 Bearer>" }`；响应：`data` = Follow。

#### GET `/follows`
我的跟单关系。响应：`{ "follows": [...] }`。

#### POST `/follows/:id/stop`
停止跟单（仅本人）。响应：`{ "ok": true }`。

#### GET `/admin/leads` · `/admin/follows` · `/admin/copies`
管理：全量带单/跟单/复制成交列表。

#### POST `/admin/simulate-trade`
管理：模拟一笔成交驱动复制（演示/联调用）。

#### GET `/admin/reconcile`
管理：复制费业务对账（各资产 `已入账复制费之和 == SysCopyTradeFee 余额`），返回偏差。

<a id="copytrade-errors"></a>
## 跟单错误映射

| HTTP | code | 场景 |
| --- | --- | --- |
| 401 | 401 | 未认证 / Token 非法 |
| 400 | 400 | 参数校验（copy_ratio≤0、缺 token、lead 非 active） |
| 403 | 403 | 非本人操作（关闭/停止他人带单） |
| 404 | 404 | lead/follow 不存在 |
| 500 | 500 | 服务端内部异常（代下单/账本） |

---

<a id="bot"></a>
# 交易机器人模块

> 服务：`internal/bot`（独立二进制 `cmd/bot`，监听 `:8098`）。
> 路由前缀：`/api/v1/bot`。范围：网格/定投(DCA)/均线(MA) 策略的创建、启停、后台 tick 代用户下单。

<a id="bot-basics"></a>
## 机器人基础约定

- **Base URL**：`/api/v1/bot`
- **认证**：用户接口需 `middleware.Auth`；管理/调试接口需 `AdminGuard`。
- **行情源（§39）**：策略 tick 由 `PriceSource` 取价，生产接 `oracle` 真实指数价（configs 配置 `oracle.feeds`），离线回退 `DefaultOracle` 静态演示价；行情非法（NaN/Inf/非正/无价）拒绝本轮（F5）。
- **代下单（F4/F1）**：经 `spot/futures` 的 `/order`，携带策略授权 token（F4）与 `client_oid=bot:<strategyID>:<round>`（F1 下游幂等）。

<a id="bot-models"></a>
## 数据模型

| 结构 | 关键字段 |
| --- | --- |
| BotStrategy | id, user_id, market(spot/futures), symbol, side(buy/sell), type(grid/dca/ma), status(active/stopped), params(各策略参数), user_token, grid_state(网格状态) |
| BotOrder | id, strategy_id, user_id, market, symbol, side, price, qty, client_oid, exchange_order_id, status |

<a id="bot-endpoints"></a>
## 接口列表

路径省略前缀 `/api/v1/bot`。鉴权：User；Admin 接口需 `AdminGuard`。

#### POST `/strategies`
创建策略（默认 stopped，需显式启动）。请求体：`{ "market":"spot","symbol":"BTC_USDT","side":"buy","type":"grid","params":{...} }`；F5 参数校验失败 → `400`。

#### GET `/strategies`
我的策略列表。响应：`{ "strategies": [...] }`。

#### POST `/strategies/:id/start`
启动策略（仅本人）。响应：`{ "ok": true }`。

#### POST `/strategies/:id/stop`
停止策略（仅本人）。响应：`{ "ok": true }`。

#### POST `/admin/tick`
管理：强制触发一次指定策略 tick（调试/联调）。请求体：`{ "id": 1 }`；响应：`{ "ok": true }`。

<a id="bot-errors"></a>
## 机器人错误映射

| HTTP | code | 场景 |
| --- | --- | --- |
| 401 | 401 | 未认证 / Token 非法 |
| 400 | 400 | 参数校验（market/side/type 非法、网格上下界、DCA 周期/额、MA 窗口） |
| 403 | 403 | 非本人操作 |
| 404 | 404 | 策略不存在 |
| 500 | 500 | 服务端内部异常（代下单/账本） |

---

<a id="errors"></a>
## 错误码总览

| 模块 | HTTP | code | 含义 |
| --- | --- | --- | --- |
| OTC | 401 | 4010 | 未认证 / Token 非法 |
| OTC | 400 | 4000/4001/4002 | 请求非法 / 参数校验 / 业务规则 |
| OTC | 403 | 4030 | 非订单参与方 |
| OTC | 404 | 4040 | 订单不存在 |
| OTC | 500 | 5000 | 服务端内部错误 |
| User | 401 | 401 | 未认证 / 凭证错误 / 2FA 缺失 |
| User | 400 | 400 | 参数/业务前置校验失败、设置专用客户端错误 |
| User | 500 | 500 | 服务端内部异常 |
| Announcement | 403 | 403 | 非 admin 调用 `/admin/*` |
| Announcement | 404 | 404 | 公告不存在 |
| Announcement | 400 | 400 | title/content 超长、level 非法、title 缺失 |
| Announcement | 500 | 500 | 服务端内部异常 |
| Wealth | 401 | 4010 | 未认证 / Token 非法 |
| Wealth | 400 | 4000/4001/4002 | 请求非法 / 参数校验 / 业务规则（product not found、not open、below min、insufficient balance、not owner、already redeemed、locked 等） |
| Wealth | 500 | 5000 | 服务端内部异常（持久化 / 账本转账失败） |
| Wallet | 401 | 401 | 未认证 / Token 非法 |
| Wallet | 500 | 500 | 服务端内部异常（账本读取） |
| Notification | 401 | 401 | 未认证 / Token 非法 |
| Notification | 404 | 404 | 非本人通知 / 不存在 |
| Notification | 500 | 500 | 服务端内部异常 |
| Security | 401 | 401 | 未认证 / Token 非法 |
| Security | 400 | 400 | 参数校验（permissions 非法、防钓鱼码超长、注销当前会话） |
| Security | 404 | 404 | 非本人资源 / 不存在 |
| Security | 500 | 500 | 服务端内部异常 |
| Earn/Launchpad | 401 | 401 | 未认证 / Token 非法 |
| Earn/Launchpad | 400 | 400 | 参数/业务前置校验（未勾选风险揭示、min/max 超限、定期锁定、预算不足） |
| Earn/Launchpad | 404 | 404 | 产品/项目/持仓不存在 |
| Earn/Launchpad | 500 | 500 | 服务端内部异常（账本转账） |
| Copytrade | 401 | 401 | 未认证 / Token 非法 |
| Copytrade | 400 | 400 | 参数校验（copy_ratio≤0、缺 token、lead 非 active） |
| Copytrade | 403 | 403 | 非本人操作 |
| Copytrade | 404 | 404 | lead/follow 不存在 |
| Copytrade | 500 | 500 | 服务端内部异常（代下单/账本） |
| Bot | 401 | 401 | 未认证 / Token 非法 |
| Bot | 400 | 400 | 参数校验（market/side/type 非法、网格上下界、DCA/MA 参数） |
| Bot | 403 | 403 | 非本人操作 |
| Bot | 404 | 404 | 策略不存在 |
| Bot | 500 | 500 | 服务端内部异常（代下单/账本） |

> 注：OTC 与 Wealth 使用带业务子码（4010/4030/4000/…、5000）的 `code`；用户服务、公告服务与合约钱包（Wallet）当前 `code` 直接复用 HTTP 状态码（401/400/403/404/500）。两者均遵循统一 `{code,message,data}` 信封。Notification/Security/Earn/Launchpad/Copytrade/Bot 同属 HTTP 状态码复用组。

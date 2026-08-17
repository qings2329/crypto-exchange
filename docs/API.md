# API 接口文档（统一索引）

> 本项目后端按业务线拆分为多个独立服务，统一通过网关暴露，约定一致的**响应信封**与 **HMAC Bearer 鉴权**。
> 本文合并各业务线接口文档为单一索引，便于查阅。各模块的事实来源为其 `internal/<module>/handler.go` 等实现。

- **OTC 场外交易**：服务 `cmd/otc`，前缀 `/api/v1/otc`（见 [OTC 模块](#otc)）。
- **用户个人设置**：服务 `internal/services/user`，前缀 `/api/v1/user`（见 [用户设置模块](#user)）。
- **公告模块**：服务 `internal/announcement`（挂载于用户服务 `cmd/user`，共用同一数据库），前缀 `/api/v1/announcement`（见 [公告模块](#ann)）。
- **理财资管**：服务 `internal/wealth`（独立二进制 `cmd/wealth`，监听 `:8092`），前缀 `/api/v1/wealth`（见 [理财模块](#wealth)）。

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

<a id="ann-frontend"></a>
## 公告前端对接

- 类型与封装见 `src/api/client.ts`：`listAnnouncements()`（公开）、`adminListAnnouncements()`、`adminCreateAnnouncement(payload)`、`adminUpdateAnnouncement(id, payload)`、`adminDeleteAnnouncement(id)`，及类型 `Announcement`/`AnnouncementInput`/`AnnouncementLevel`。
- 页面：
  - `src/pages/Home.tsx` 首页大盘：欢迎语 + 平台公告横幅（消费 `listAnnouncements`）+ 模块快捷入口 + 账户概览（消费 `userMe`）。已设为默认路由 `/home`。
  - `src/pages/Announcements.tsx` 公告管理：增删改查（需 admin 角色；无权限时列表接口返回 403，由错误提示展示）。
- 导航栏新增「首页 /home」「公告 /announcements」。
- 公告等级 badge 样式（`info` 绿 / `warning` 黄 / `maintenance` 蓝）见 `src/styles.css` 的 `.ann-badge.*`。

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

> 注：OTC 与 Wealth 使用带业务子码（4010/4030/4000/…、5000）的 `code`；用户服务与公告服务当前 `code` 直接复用 HTTP 状态码（401/400/403/404/500）。两者均遵循统一 `{code,message,data}` 信封。

# API 契约变更记录（Breaking / Contract Changes）

本文件记录对外部调用方（含 web-admin 前端）有影响的 API 契约变更，便于前后端联调与 onboarding 查阅。
每条变更标注日期、提交、影响接口与适配要点。

---

## 2026-08-16 — admin 充提接口 `id` 字段 int64 → string

- **提交**：`1201850`（`fix(adminapi): 消除审批错审路由与降级伪造数据`）
- **背景**：资金安全审查修复「审批可能路由到错误提现单」与「上游不可达返回伪造充提记录」。
  修复审批错审需把审批锚点从不稳定的哈希 id 改为真实 futures `hold_id`，故接口 `id` 字段类型变更。

### 影响接口

| 接口 | 变更 |
|---|---|
| `GET /api/admin/withdrawals` | 响应项 `Withdrawal.id` 由 `int64`（哈希）改为 `string`（真实 hold_id） |
| `GET /api/admin/deposits` | 响应项 `Deposit.id` 由 `int64`（哈希）改为 `string`（真实链上标识） |
| `POST /api/admin/withdrawals/:id/approve` | 路径参数 `:id` 现在期望**字符串** hold_id（原 int64 哈希） |
| `POST /api/admin/withdrawals/:id/reject` | 同上 |

- `tx_hash` 字段**不变**，其值本就等于 hold_id 字符串，可继续（或改用）作为审批锚点。
- 其余 admin 接口（用户、风控、强平、对账等）的 `id` 不受影响。

### 前端适配要点
1. 审批时直接用列表项返回的 `id`（现为字符串）拼 URL，**不要对 `id` 做 `Number()` / 数值强转**（最易踩坑，会导致 404/审批失败）。
2. 也可改用 `tx_hash` 字段作为审批锚点（始终是 hold_id 字符串，语义更直观）。
3. 不要把 `id` 当数字存储、比较或排序——字符串 hold_id 无数值顺序。
4. 列表页 `id` 列现在展示 hold_id 字符串（如 `wd_xxx`），如需短标识可前端自行截取展示。

### 验证
联调新后端（≥`1201850`），走「列表 → 审批/拒绝 → 再次列表回显 approved/rejected」全流程，
确认返回 200 且后端 futures 侧正确收到 hold_id；前端双击审批应幂等（第二次返回 `already:true`）。

### 影响页面
运营后台「提现审批」「充值/提现记录」相关页面。

---

## 2026-08-16 — admin 充提/用户列表「上游不可达」降级契约固定为「空数组 + X-Degraded 头」

- **提交**：`cf2de73`（`fix(adminapi): listUsers 降级同构为空数组+统一 X-Degraded 头`）
- **背景**：充提/用户列表实时聚合 futures / user 上游。早期实现在上游不可达时返回
  `{"degraded":true,"items":[...]}` 的**对象**形态，而正常路径 `data` 是**数组**，前端需两套解析；
  且曾返回过伪造示例记录（已在上轮修复）。本轮把降级契约统一为与正常路径同构。

### 影响接口

| 接口 | 正常（上游可达） | 降级（上游不可达） |
|---|---|---|
| `GET /api/admin/deposits` | `data` 为 `Deposit[]` 数组 | `data` 为 `[]`（空数组）+ 响应头 `X-Degraded: futures-unavailable` |
| `GET /api/admin/withdrawals` | `data` 为 `Withdrawal[]` 数组 | `data` 为 `[]`（空数组）+ 响应头 `X-Degraded: futures-unavailable` |
| `GET /api/admin/users` | `data` 为 `AdminUser[]` 数组 | `data` 为 `[]`（空数组）+ 响应头 `X-Degraded: user-unavailable` |

- 降级时 `data` **始终为空数组**（与正常路径同构），不再返回 `degraded`/`items` 对象字段，杜绝前端 object/array 形态切换导致的解析失败。
- 前端通过读取响应头 `X-Degraded` 判断是否降级，据此展示「数据暂不可用」横幅，而非依赖 body 内的标志位。
- 降级**绝不返回伪造记录**，避免误导运营资金决策。

### 前端适配要点
1. 解析列表接口时统一按「`data` 是数组」处理，无需再判断 `degraded` 字段。
2. 监听响应头 `X-Degraded`：若存在则提示用户上游服务暂不可用（数据可能为空），并禁止基于空列表做出任何资金结论。
3. `X-Degraded` 仅作「降级提示」信号，不代表错误（HTTP 仍为 200）。

---

## 2026-08-16 — otc 资金安全修复（AdminGuard + 对账按资产拆分 + 定点化）

- **提交**：`1cc89f1`（`fix(otc): 修复充值提币边界审查发现的高危资金安全问题（F1-F5b）`）
- **背景**：边界审查发现 otc 存在高危资金安全问题，本轮修复：`F4` 争议裁决/管理端点缺鉴权、
  `F1` 托管释放非幂等（双付/双退）、`F2` crypto 数量 float 派生、`F3` 对账只查默认资产桶、`F5b` 余额用 1e-9 容差。

### 契约/行为影响

| 接口 | 变更 |
|---|---|
| `POST /api/v1/otc/orders/:id/resolve` | **新增 `AdminGuard`**：必须由管理员 token 调用，否则返回 403（此前任意登录用户均可移动托管资金） |
| `GET /api/v1/otc/admin/orders` | 新增 `AdminGuard`（403 若无管理员角色） |
| `GET /api/v1/otc/admin/reconcile` | 新增 `AdminGuard`；响应 `escrow_balance` 由单个数字改为**按资产拆分的对象** `{ "BTC": 0, "USDT": 100 }` |
| 订单 `crypto_amount` 字段 | 仍是 JSON 数字（内部改为定点 `AssetAmount` 存储，对外序列化不变） |

- `resolve` 端点现在**仅管理员可调用**；前端若以普通用户身份调用将收到 403，需改用管理员会话。
- `/admin/reconcile` 的 `escrow_balance` 由 `number` 变为 `map[string]number`（键为资产名），前端需按资产读取。
- 释放/退回路径已幂等：对已完成订单重复 `resolve`/确认收款将安全短路，不会双付。

### 迁移
- `ce_otc_orders.crypto_amount` 由 `DOUBLE` 改为 `VARCHAR(64)`（精确存储定点字符串），迁移版本 `9604` 自动执行（向下兼容回退到 DOUBLE）。

---

## 2026-08-16 — 账本 v1→v2 快照迁移真实数据校验（定点化配套）

- **背景**：账本金额已定点化（`settlement.AssetAmount`）。旧快照（无 `schema_version` 的 float 快照）
  经 `parseSnapshot` → `migrateV1ToV2` 按资产标准 decimals 迁移。生产环境上线前必须对**真实/全量** v1 快照
  做预校验，避免未知资产被回退默认 8 位小数而错配精度。
- **相关提交**：`d40a6c2`（引入 migrateV1ToV2 + 复合 key 解析）、本轮新增 `ValidateV1SnapshotAssets` 预检与多资产迁移测试。

### 预发/生产校验步骤
1. **抽取真实 v1 快照**：从当前运行实例 `GET /debug/snapshot`（或对应持久化文件）导出 JSON（确保其中**无** `schema_version` 字段，即确为旧格式）。
2. **未知资产预检**：将快照喂入 `ledger.ValidateV1SnapshotAssets(v1)`，列出所有「未知资产」告警。
   - 若告警非空：先确认这些资产的真实 decimals；若与 `AssetDecimalsByName` 默认（8）不一致，须先补全该表或自定义迁移，否则金额会被放大/缩小。
   - 已知资产（BTC/ETH/USDT/USDC/TRX/TRON/TRC20）直接按标准 decimals 迁移，无需干预。
3. **单元级回归**：`go test ./internal/ledger/ -run 'TestMigrateV1ToV2|TestParseSnapshotVersionRouting|TestValidateV1SnapshotAssets'`
   已覆盖账户/流水/提现冻结/社会化分摊/复合 key 的多资产生成与「无 schema_version 走迁移、有 schema_version 走直读」路由。
4. **余额守恒核对**：迁移后用 `Ledger.Snapshot()` 导出 v2，按资产汇总 `Accounts[*].Available`，与 v1 人类单位金额逐资产比对，确认无凭空增减（仅最小单位内的 float 截断，属预期）。
5. **灰度上线**：先在预发环境用真实形状快照跑一遍 `Restore` + 对账，确认 `SysOtc`/各系统账户余额与迁移前一致，再上生产；生产回滚方案为保留 v1 快照副本，必要时以旧格式 `Restore`。

### 注意
- `migrateV1ToV2` 对未知资产**静默**采用默认 8 位——这是唯一可能错配精度的路径，故步骤 2 的预检是上线硬性前置。

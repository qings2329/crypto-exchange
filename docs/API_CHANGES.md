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

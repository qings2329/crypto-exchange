# 公告模块使用文档

> 面向运维 / 运营管理员，说明如何发布、查看与管理站内公告。
> 接口契约（字段、状态码、错误映射）以 [`API.md`](./API.md#ann) 为准；本文侧重「怎么用」，含可直接复制的 curl 示例。

---

## 1. 概述

公告模块用于平台向用户发布运营通知、活动公告、风险提示与系统维护预告。

- **后端包**：`internal/announcement`
- **挂载位置**：作为独立包挂载在**用户服务** `cmd/user`（默认 `:8081`），与用户模块共用同一数据库。
- **Base URL**：`/api/v1/announcement`
- **路由前缀**
  - 公开：`GET /api/v1/announcement/list`（无需登录，返回已发布公告）
  - 管理：`/api/v1/announcement/admin/*`（需管理员 Token）

### 响应信封

所有接口统一返回：

```json
{ "code": 0, "message": "ok", "data": <业务数据> }
```

失败时 `data` 为 `null`，`code` 为业务码，`message` 为可读描述。

---

## 2. 前置条件：获取管理员 Token

公告的增删改查（除公开列表外）需要**管理员角色** Token。管理员 Token 由管理后台服务（`cmd/admin`，默认 `:8090`）签发，与用户服务共用同一个 `cfg.Auth.Secret`，因此可直接用于用户服务上的公告管理接口。

```bash
# 向管理后台登录，拿到 access_token（默认演示账号见各环境配置）
curl -s -X POST http://localhost:8090/api/admin/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"***REDACTED***"}'
```

响应示例：

```json
{ "code": 0, "message": "ok", "data": { "access_token": "eyJ...", "expires_in": 86400 } }
```

后续示例中的 `<ADMIN_TOKEN>` 即为此 `access_token`，通过 `Authorization: Bearer <ADMIN_TOKEN>` 携带。

> 若用户服务与 admin 服务使用不同配置文件，请确认两者的 `auth.secret` 一致，否则 Token 校验会返回 `401`。

---

## 3. 公开查看公告（无需登录）

前端首页公告横幅、公告页均消费此接口。

```bash
curl -s http://localhost:8081/api/v1/announcement/list
```

响应：

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "announcements": [
      {
        "id": 1,
        "level": "maintenance",
        "title": "系统升级通知",
        "content": "今晚 22:00 起维护约 30 分钟，期间部分功能不可用。",
        "active": true,
        "published_at": "2026-08-17T22:00:00Z",
        "created_at": "2026-08-17T10:00:00Z",
        "updated_at": "2026-08-17T10:00:00Z"
      }
    ]
  }
}
```

- 仅返回 `active=true` 的公告，按 `published_at` 倒序。
- 无已发布公告时返回 `"announcements": []`。

---

## 4. 管理后台操作（需 admin Token）

通用请求头：`Authorization: Bearer <ADMIN_TOKEN>`、`Content-Type: application/json`。

### 4.1 全量列表（含草稿）

```bash
curl -s http://localhost:8081/api/v1/announcement/admin \
  -H "Authorization: Bearer <ADMIN_TOKEN>"
```

返回 `{ "announcements": [...] }`，包含 `active=false` 的草稿。

### 4.2 创建公告

```bash
curl -s -X POST http://localhost:8081/api/v1/announcement/admin \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{
    "level": "maintenance",
    "title": "系统升级通知",
    "content": "今晚 22:00 起维护约 30 分钟。",
    "active": true
  }'
```

- `title` **必填**；`level`、`content`、`active` 可选。
- 省略 `level` 时默认 `info`。
- `active=true` 且未给 `published_at` 时，服务端自动填充为当前时间（即「立即发布」）。
- 返回创建后的完整 `Announcement`（含 `id`）。

### 4.3 更新公告（部分更新）

仅修改提供的字段，未提供的字段保持不变。

```bash
# 把 id=1 的公告从草稿改为已发布
curl -s -X PUT http://localhost:8081/api/v1/announcement/admin/1 \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"active": true}'

# 修改标题与正文（等级不变）
curl -s -X PUT http://localhost:8081/api/v1/announcement/admin/1 \
  -H "Authorization: Bearer <ADMIN_TOKEN>" \
  -H "Content-Type: application/json" \
  -d '{"title":"维护时间调整","content":"改到明日 22:00。"}'
```

- id 不存在 → `404`（`announcement not found`）。

### 4.4 删除公告

```bash
curl -s -X DELETE http://localhost:8081/api/v1/announcement/admin/1 \
  -H "Authorization: Bearer <ADMIN_TOKEN>"
```

- 成功返回 `{ "ok": true }`；id 不存在 → `404`。

---

## 5. 字段与校验规则

| 字段 | 类型 | 必填 | 说明 / 校验 |
| --- | --- | --- | --- |
| `level` | string | 否 | `info` / `warning` / `maintenance`；非法值 → `400 invalid level`；缺省 `info` |
| `title` | string | **创建必填** | ≤ 128 字符；为空 → `400 title required`；超长 → `400 title too long` |
| `content` | string | 否 | ≤ 4096 字符；超长 → `400 content too long` |
| `active` | bool | 否 | 是否对外发布；`true` 后出现在公开列表 |
| `published_at` | time | 否 | 发布时间；草稿为 0 值；切到 `active=true` 且无此值时自动填充 |

错误速查（管理接口）：

| HTTP | message | 场景 |
| --- | --- | --- |
| 401 | 未认证 | 缺/非法 Token |
| 403 | insufficient role | 非 admin 调用管理接口 |
| 400 | title required / title too long / content too long / invalid level | 校验失败 |
| 404 | announcement not found | 更新/删除的 id 不存在 |
| 500 | — | 服务端内部异常 |

---

## 6. 等级与前端样式

`level` 同时决定前端 badge 样式：

| level | 文案 | 颜色 |
| --- | --- | --- |
| `info` | 公告 | 绿 |
| `warning` | 提醒 | 黄 |
| `maintenance` | 维护 | 蓝 |

---

## 7. 前端页面入口

- **首页（默认路由 `/home`）**：展示欢迎语、平台公告横幅（消费公开列表）、模块快捷入口、账户概览。
- **公告管理（`/announcements`）**：可视化增删改查表单；非 admin 账号访问时列表接口返回 `403`，页面以错误提示呈现（即「无权限」）。

---

## 8. 部署与迁移

- 公告模块随**用户服务** `cmd/user` 启动；`cfg.MySQL.DSN` 非空时自动建表。
- 迁移版本 **9401** `create_ce_announcements`：新建 `ce_announcements` 表（id 主键、level、title、content、active、published_at、created_at、updated_at），索引 `idx_active_published(active, published_at)`。
- 与用户模块（9101–9106）共用同一数据库与 `ce_schema_migrations`，版本号互不重叠，迁移幂等、可重入。
- DSN 缺失时降级为内存存储（重启即失），仅用于本地开发/演示。

```bash
# 用户服务（含公告模块），接入 MySQL 后启动自动迁移
go build -o bin/user ./cmd/user
./bin/user -config configs/config.yaml
```

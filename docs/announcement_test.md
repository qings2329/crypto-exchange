# 公告模块测试说明

> 说明公告模块的单元测试如何运行、覆盖哪些场景，以及测试设计要点。
> 接口契约与字段校验见 [`API.md`](./API.md#ann)；运维操作示例见 [`announcement_guide.md`](./announcement_guide.md)。

---

## 1. 概述

- **测试文件**：`internal/announcement/service_test.go`（包 `announcement`，白盒测试）
- **测试框架**：Go 标准库 `testing`，无第三方依赖、无需数据库或网络。
- **测试对象**：`Service` 层（`Create` / `Update` / `Delete` / `Get` / `ListActive` / `ListAll`），即公告的全部业务规则与校验。
- **存储后端**：使用 `NewMemStore()`（内存实现），与 MySQL 实现共享 `Store` 接口语义，因此服务层逻辑对两种实现一致。

### 覆盖边界（重要）

- **服务层 + HTTP 路由层**：已被单测覆盖（无 DB 即可运行），语句覆盖率约 **59.9%**。
- **`store_mysql.go`（建表/SQL）**：需要真实 MySQL，仅在设置 `MYSQL_TEST_DSN` 时由 `integration_test.go` 覆盖；未设置时该文件不执行，因此普通 `go test`（无 DSN）覆盖率不含它。

> 即：本地/CI 无 MySQL 时，`go test` 覆盖服务层与 handler 层；接入 MySQL（设 `MYSQL_TEST_DSN`）后，集成测试进一步覆盖存储层的 SQL 往返与迁移幂等。

---

## 2. 如何运行

```bash
# 运行公告模块全部测试
go test ./internal/announcement/...

# 显示逐个用例
go test ./internal/announcement/... -v

# 强制重跑（忽略缓存）
go test ./internal/announcement/... -count=1

# 查看覆盖率
go test ./internal/announcement/... -cover
```

> 退出码 0、输出 `ok` 即全部通过。当前共 **12** 个用例，全部通过（见下方结果）。

最近一次运行结果（`-v`，无 DSN，含 handler 测试）：

```
--- PASS: TestHandlerPublicListNoAuth (0.00s)
--- PASS: TestHandlerAdminRequiresAuth (0.00s)
--- PASS: TestHandlerAdminRequiresRole (0.00s)
--- PASS: TestHandlerAdminCreate (0.00s)
--- PASS: TestHandlerAdminCreateInvalidLevel (0.00s)
--- PASS: TestHandlerAdminUpdateNotFound (0.00s)
--- PASS: TestHandlerAdminDeleteNotFound (0.00s)
--- PASS: TestHandlerAdminLifecycle (0.00s)
--- SKIP: TestMySQLRoundTrip (0.00s)            # 无 MYSQL_TEST_DSN
--- SKIP: TestMySQLMigrationIdempotent (0.00s)  # 无 MYSQL_TEST_DSN
--- PASS: TestCreate (0.00s)
...（服务层 12 例）...
PASS
ok  github.com/coldlar/crypto-exchange/internal/announcement  0.013s
```

---

## 3. 测试辅助

`service_test.go` 顶部定义了可复用的小工具，保持用例简洁、确定性：

```go
func newTestService() *Service { return NewService(NewMemStore()) } // 每次新建干净的内存存储
func ptr(s string) *string     { return &s }                        // 构造 *string（部分更新入参）
func pbool(b bool) *bool       { return &b }                        // 构造 *bool
```

- `newTestService()` 每次用例新建独立 `memStore`，用例之间无状态污染。
- `ptr` / `pbool` 对应 `AnnouncementInput` 的指针字段（nil=不修改），便于模拟「部分更新」语义。

---

## 4. 用例清单

| 用例 | 场景 | 校验点 |
| --- | --- | --- |
| `TestCreate` | 用全字段（level/title/content/active=true）创建 | ID 非空；Level、Title 正确；Active=true；**切发布态自动填充 `PublishedAt`** |
| `TestCreateDefaultsLevel` | 只传 title 创建 | 未给 level 时默认 `info` |
| `TestCreateInvalidLevel` | `level="critical"` | 返回 `ErrInvalidLevel` |
| `TestCreateTitleRequired` | 不传 title | 返回 `ErrTitleRequired` |
| `TestCreateTitleTooLong` | title 长度 `maxTitleLen+1`（129） | 返回 `ErrTitleTooLong` |
| `TestListActive` | 写入 1 草稿 + 2 已发布 | 仅返回 2 条；**按发布时间倒序**（后发布的排在前） |
| `TestListAll` | 写入 1 已发布 + 1 草稿 | 返回共 2 条（含草稿） |
| `TestUpdate` | 草稿改为新标题 + active=true | Title 更新；Active=true；**激活后自动填充 `PublishedAt`**；原 Level 保留 |
| `TestUpdateInvalidLevel` | 更新 `level="bad"` | 返回 `ErrInvalidLevel` |
| `TestUpdateNotFound` | 更新不存在的 `id=999` | 返回 `ErrNotFound` |
| `TestDelete` | 创建 → 删除 → 再 `Get` | 删除成功；删除后 `Get` 返回 `ErrNotFound` |
| `TestDeleteNotFound` | 删除不存在的 `id=42` | 返回 `ErrNotFound` |

---

## 5. 覆盖矩阵

| 业务行为 | 覆盖用例 |
| --- | --- |
| 创建成功 + 字段回写 | `TestCreate` |
| 默认等级 | `TestCreateDefaultsLevel` |
| 创建校验：非法等级 | `TestCreateInvalidLevel` |
| 创建校验：标题必填 | `TestCreateTitleRequired` |
| 创建校验：标题超长 | `TestCreateTitleTooLong` |
| 公开列表过滤（仅 active） | `TestListActive` |
| 列表排序（发布时间倒序） | `TestListActive` |
| 全量列表（含草稿） | `TestListAll` |
| 部分更新 + 激活自动发布时间 | `TestUpdate` |
| 更新校验：非法等级 | `TestUpdateInvalidLevel` |
| 更新/删除：不存在 → 404 | `TestUpdateNotFound`、`TestDeleteNotFound` |
| 删除成功 + 删除后不可见 | `TestDelete` |

---

## 6. 集成测试覆盖建议

服务层单测（第 4 节）已覆盖全部业务规则；要补齐**存储层 SQL 往返**与 **HTTP 路由/鉴权语义**，建议补充以下两层集成测试。本模块已落地其中两类，可直接运行或扩展。

### 6.1 HTTP 路由层（已落地：`handler_test.go`）

使用 `gin.New()` + `net/http/httptest` 装配真实路由（`Handler.Register(r, verifier)`），无需数据库、随 `go test` 常驻运行。覆盖：

| 用例 | 场景 | 期望 |
| --- | --- | --- |
| `TestHandlerPublicListNoAuth` | `GET /list` 不带头 | 200，body 含 `announcements` |
| `TestHandlerAdminRequiresAuth` | `POST /admin` 无 Token | 401 |
| `TestHandlerAdminRequiresRole` | `POST /admin` 用普通用户 Token | 403 |
| `TestHandlerAdminCreate` | admin Token 创建 | 200，data 含 id/level/active |
| `TestHandlerAdminCreateInvalidLevel` | admin Token + 非法 level | 400 |
| `TestHandlerAdminUpdateNotFound` | 更新不存在 id | 404 |
| `TestHandlerAdminDeleteNotFound` | 删除不存在 id | 404 |
| `TestHandlerAdminLifecycle` | 创建→列表→更新→删除→再查 | 全流程 200/404 符合预期 |

要点：token 由同一 `verifier` 签发（`IssueAdmin` 得 admin、`Issue` 得普通用户），与 `middleware.Auth`/`AdminGuard` 同密钥校验，断言 HTTP 状态码与信封 `code/message`。

### 6.2 存储层集成测试（已落地：`integration_test.go`，需 MySQL）

约定与 `internal/ledger` 一致：**仅当设置 `MYSQL_TEST_DSN` 时运行**，否则 `t.Skip`。覆盖存储层真实 SQL 与迁移：

- `TestMySQLRoundTrip`：草稿的 `published_at` 为 0 值、已发布的自动填充；`ListActive` 仅含已发布；部分更新（草稿→发布）后 `published_at` 自动填充；删除后 `Get` 返回 `ErrNotFound`。
- `TestMySQLMigrationIdempotent`：`NewMySQLStore` 连建两次均成功，验证迁移 9401 幂等，且与用户模块共用 `ce_schema_migrations` 不冲突。

运行方式：

```bash
# 指向一个测试库（parseTime=true 让 DATETIME 正确解析）
export MYSQL_TEST_DSN="user:pass@tcp(127.0.0.1:3306)/ce_test?parseTime=true"

# 仅跑集成测试
go test ./internal/announcement/... -run 'TestMySQL' -v

# 全量（含 handler + 服务层）
go test ./internal/announcement/... -v
```

> 建议使用独立测试库（如 `ce_test`），避免与开发/生产数据混用；用例内用 `defer svc.Delete(id)` 清理，减少跨运行残留。

### 6.3 部署/同库集成验证（建议补充）

- **同库部署冒烟**：在 `cmd/user` 实际启动（带 `cfg.MySQL.DSN`）后，调用公开 `/list` 与管理 `/admin`，确认公告表与用户表共存于同一库、迁移版本不冲突。
- **迁移版本唯一性**：CI 中加一条断言——扫描 `ce_schema_migrations` 已应用版本无重复（9401 与 9101–9106 等区间互不重叠）。
- **并发迁移安全**：多个集成进程同时 `Up()` 同一远程库时 `INSERT IGNORE` 应保证不报错（项目 `migrate` 已做幂等）。

### 6.4 设计要点

- **为何用内存存储做服务层单测**：业务规则与存储解耦，内存实现语义与 MySQL 一致，可在无 DB 环境快速、确定地验证全部校验与 CRUD（毫秒级、可并行）。
- **确定性**：内存存储排序为稳定插入序 + 发布时间比较，用例不依赖时钟（仅断言 `PublishedAt` 非零）。
- **长度校验用 rune 计数**：`TestCreateTitleTooLong` 用 `[]rune` 构造精确超长字符串，覆盖「按字符数而非字节数校验」的逻辑（与 `service.go` 的 `len([]rune(...))` 一致）。
- **集成测试与单测分层**：单测（内存）保证快与确定性；集成测试（DSN）保证 SQL/迁移/同库真实行为；两者互补，CI 中分别纳入「无依赖单测」与「需服务的集成测试」两个阶段。

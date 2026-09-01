# 期货交易管理页面 — 集成说明（待应用到 web-admin）

后端 `adminapi` 期货交易管理模块已合并（`2105818`），本目录提供：

- `0003-futures-management-page.patch` — 新增页面 `src/pages/FuturesManagement.tsx`（可直接 `git apply`）。
- 本文件 — 把该页面接入 `web-admin` 所需的三处现有文件改动（权限字典 / 路由 / 菜单）。

> 由于本机无 web-admin 源码，以下改动以「搜索锚点 + 插入片段」给出，需在 web-admin 内手动套用。
> 套用后请本地 `npm run dev` / `tsc` 自测。

---

## 1. 权限字典 `src/lib/permissions.tsx`

在 `PERMISSIONS` 数组中，与 `risk:view` 相邻处加入 `futures:view` / `futures:manage`（与后端 `internal/adminapi/rbac.go` 对齐）：

**搜索锚点**（应已存在）：
```ts
{ key: "risk:view",   name: "风控大盘查看" },
```

**在其后追加**：
```ts
{ key: "futures:view",   name: "期货持仓/资金费查看" },
{ key: "futures:manage", name: "期货交易管理（充值/代客直提/应急冻结/风控开关/坏账分摊）" },
```

> 两个 key 必须和后端 `rbac.go` 完全一致，否则 `RequirePerm` 会 403。读操作由路由守卫 `futures:view` 控制，写操作（增/冻/提/分摊）由按钮调用接口时后端 `futures:manage` 再次拦截，双保险。

---

## 2. 路由注册 `src/router.tsx`（或你们的路由文件）

引入并注册页面（路由路径建议 `/futures`，菜单/面包屑可显示「期货交易管理」）：

**搜索锚点**：你们其它后台页面的 `import` 与 `routes` 注册段（例如 `import RiskManagement from "../pages/RiskManagement"`）。

**import 追加**：
```ts
import FuturesManagement from "../pages/FuturesManagement";
```

**routes 追加**（放在后台受保护路由组内）：
```tsx
{
  path: "/futures",
  element: <FuturesManagement />,
  meta: { title: "期货交易管理", permission: "futures:view" },
},
```

> `meta.permission: "futures:view"` 用于路由守卫：无该权限直接跳 403/登录页；写操作（手工充值/代客直提/应急冻结/风控开关/坏账分摊）由按钮调用接口时后端 `futures:manage` 再次拦截。

---

## 3. 侧边栏菜单 `src/layout/...` 或 `src/components/Sidebar.tsx`

在「期货」分组下新增菜单项（仅在拥有 `futures:view` 时显示）：

**搜索锚点**：「期货」分组里已有的菜单项（如没有期货分组，可新建一个，与后端 `rbac.go` 的 Group: "期货" 对应）。

**追加**：
```tsx
{
  path: "/futures",
  name: "期货交易管理",
  icon: "line-chart",      // 按你们图标库调整
  permission: "futures:view",
},
```

---

## 4. 页面内 `api` 客户端对齐

`FuturesManagement.tsx` 末尾有一行占位声明，编译前请**删除该行**并改为你们真实的 api 引入：
```ts
// 删除：declare const api: {...}
// 改为（路径按你们项目）：
import { api } from "../lib/api";
```
确认 `api.get/post` 已自动附带 admin token，并返回信封 `{ code, data, message }`（页面用 `.data` 取值）。

> 页面内所有写操作（手工充值 / 代客直提 / 应急冻结 / 应急解冻 / 风控开关 / 坏账分摊）均为高危，已加 `window.confirm` 二次确认；仅 `futures:view` 账号看不到这些按钮，但后端 `futures:manage` 才是真正的强制闸门。
> 写操作返回的业务数据字段不固定（提案/分摊含按用户分摊明细），页面统一以 JSON 原文展示，便于临时核查。

---

## 应用顺序

```bash
cd web-admin
git apply patches/web-admin/0003-futures-management-page.patch   # 新增页面
# 再按本文件第 1~4 点手动编辑现有文件
```

完成后建议自测：用仅含 `futures:view` 的运营账号登录 → 能看到持仓/资金费但「执行充值/代客直提/应急冻结/风控开关/坏账分摊」按钮调用返回 403；用含 `futures:manage` 的管理员 → 写操作生效。

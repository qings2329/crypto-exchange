# 风控管理页面 — 集成说明（待应用到 web-admin）

后端 `adminapi` 风控管理模块已合并（`fe1b2be`），本目录提供：

- `0001-risk-management-page.patch` — 新增页面 `src/pages/RiskManagement.tsx`（可直接 `git apply`）。
- 本文件 — 把该页面接入 `web-admin` 所需的三处现有文件改动（权限字典 / 路由 / 菜单）。

> 由于本机无 web-admin 源码，以下改动以「搜索锚点 + 插入片段」给出，需在 web-admin 内手动套用。
> 套用后请本地 `npm run dev` / `tsc` 自测。

---

## 1. 权限字典 `src/lib/permissions.tsx`

在 `PERMISSIONS` 数组中，与 `risk:view` 相邻处加入 `risk:manage`（与后端 `internal/adminapi/rbac.go` 对齐）：

**搜索锚点**（应已存在）：
```ts
{ key: "risk:view",   name: "风控大盘查看" },
```

**在其后追加**：
```ts
{ key: "risk:manage", name: "风控规则/黑名单管理（增删）" },
```

> 若 `PERMISSIONS` 里还没有 `risk:view`，则两条都加。两个 key 必须和后端 `rbac.go` 完全一致，否则 `RequirePerm` 会 403。

---

## 2. 路由注册 `src/router.tsx`（或你们的路由文件）

引入并注册页面（路由路径建议 `/risk`，菜单/面包屑可显示「风控管理」）：

**搜索锚点**：你们其它后台页面的 `import` 与 `routes` 注册段（例如 `import Dashboard from "../pages/Dashboard"`）。

**import 追加**：
```ts
import RiskManagement from "../pages/RiskManagement";
```

**routes 追加**（放在后台受保护路由组内）：
```tsx
{
  path: "/risk",
  element: <RiskManagement />,
  meta: { title: "风控管理", permission: "risk:view" },
},
```

> `meta.permission: "risk:view"` 用于路由守卫：无该权限直接跳 403/登录页；写操作（增删）由按钮调用接口时后端 `risk:manage` 再次拦截，双保险。

---

## 3. 侧边栏菜单 `src/layout/...` 或 `src/components/Sidebar.tsx`

在「看板」分组下新增菜单项（仅在拥有 `risk:view` 时显示）：

**搜索锚点**：「看板」分组里已有的菜单项（如风控大盘 `risk:view` 那一项）。

**在其后追加**：
```tsx
{
  path: "/risk",
  name: "风控管理",
  icon: "shield",            // 按你们图标库调整
  permission: "risk:view",
},
```

---

## 4. 页面内 `api` 客户端对齐

`RiskManagement.tsx` 末尾有一行占位声明，编译前请**删除该行**并改为你们真实的 api 引入：
```ts
// 删除：declare const api: {...}
// 改为（路径按你们项目）：
import { api } from "../lib/api";
```
确认 `api.get/post/delete` 已自动附带 admin token，并返回信封 `{ code, data, message }`（页面用 `.data` 取值）。

---

## 应用顺序

```bash
cd web-admin
git apply patches/web-admin/0001-risk-management-page.patch   # 新增页面
# 再按本文件第 1~4 点手动编辑现有文件
```

完成后建议自测：用仅含 `risk:view` 的运营账号登录 → 能看到列表但「提交/移除」返回 403；用含 `risk:manage` 的管理员 → 增删生效。

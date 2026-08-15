package adminapi

import "sort"

// 本文件定义管理后台 RBAC 的权限字典（全部可被授予的细粒度权限 key），
// 以及角色/权限的展示元数据。角色与权限的实体存储与分配见 store_adminaccount.go。

// 默认角色名（首次启动 SeedBootstrap 写入；之后可由管理员自定义新角色）。
const (
	RoleSuperAdmin = "super_admin" // 超级管理员（全部权限）
	RoleAdmin      = "admin"       // 运营管理员
	RoleOperator   = "operator"    // 只读操作员
)

// 权限 key 常量（建议按"资源:动作"命名）。
const (
	// 看板与风控
	PermDashboardView = "dashboard:view" // 查看风控与运营看板

	// 用户与账户
	PermUserRead  = "user:read"
	PermUserWrite = "user:write"

	// 交易对配置
	PermSymbolRead  = "symbol:read"
	PermSymbolWrite = "symbol:write"

	// 公链管理
	PermChainRead  = "chain:read"
	PermChainWrite = "chain:write"

	// 币种管理
	PermCoinRead  = "coin:read"
	PermCoinWrite = "coin:write"

	// 充值提币
	PermDepositRead     = "deposit:read"
	PermWithdrawApproval = "withdraw:approval"

	// 订单管理（跨用户查询/撤销，运营风控用）
	PermTradeRead  = "trade:read"  // 查看全部用户订单与成交
	PermTradeManage = "trade:manage" // 撤销任意用户订单（高危）

	// 运营通知
	PermNotificationManage = "notification:manage"

	// 账本与服务健康
	PermLedgerRead  = "ledger:read"
	PermServiceRead = "service:read"

	// 管理员与权限管理（高危）
	PermAdminManage = "admin:manage" // 新增/激活/禁用/重置管理员
	PermRoleManage  = "role:manage"  // 角色与权限分配
)

// PermissionDef 是权限字典中的一条展示元数据。
type PermissionDef struct {
	Key   string `json:"key"`
	Name  string `json:"name"`  // 中文展示名
	Group string `json:"group"` // 分组（前端按组展示）
}

// allPermissionDefs 是全部可被授予的权限（用于权限分配 UI 的字典）。
var allPermissionDefs = []PermissionDef{
	{Key: PermDashboardView, Name: "查看风控与看板", Group: "看板"},

	{Key: PermUserRead, Name: "查看用户与账户", Group: "用户"},
	{Key: PermUserWrite, Name: "编辑/冻结用户", Group: "用户"},

	{Key: PermSymbolRead, Name: "查看交易对", Group: "交易对"},
	{Key: PermSymbolWrite, Name: "配置交易对", Group: "交易对"},

	{Key: PermChainRead, Name: "查看公链", Group: "公链"},
	{Key: PermChainWrite, Name: "管理公链", Group: "公链"},

	{Key: PermCoinRead, Name: "查看币种", Group: "币种"},
	{Key: PermCoinWrite, Name: "管理币种", Group: "币种"},

	{Key: PermDepositRead, Name: "查看充值", Group: "充值提币"},
	{Key: PermWithdrawApproval, Name: "审批提币", Group: "充值提币"},

	{Key: PermTradeRead, Name: "查看用户订单", Group: "订单"},
	{Key: PermTradeManage, Name: "撤销用户订单", Group: "订单"},

	{Key: PermNotificationManage, Name: "管理运营通知", Group: "运营"},

	{Key: PermLedgerRead, Name: "查看账本对账", Group: "运营"},
	{Key: PermServiceRead, Name: "查看服务健康", Group: "运营"},

	{Key: PermAdminManage, Name: "管理员管理", Group: "系统"},
	{Key: PermRoleManage, Name: "角色与权限管理", Group: "系统"},
}

// AllPermissions 返回全部权限字典（按 key 排序，便于前端稳定展示）。
func AllPermissions() []PermissionDef {
	out := make([]PermissionDef, len(allPermissionDefs))
	copy(out, allPermissionDefs)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// IsValidPermission 报告 key 是否在权限字典中。
func IsValidPermission(key string) bool {
	for _, p := range allPermissionDefs {
		if p.Key == key {
			return true
		}
	}
	return false
}

// ValidatePermissions 过滤出合法的权限 key（去除未知项）；返回的合法集合用于入库与签发。
func ValidatePermissions(keys []string) []string {
	out := make([]string, 0, len(keys))
	seen := map[string]bool{}
	for _, k := range keys {
		if IsValidPermission(k) && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

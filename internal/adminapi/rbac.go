package adminapi

import "sort"

// 本文件定义管理后台 RBAC 的权限字典（全部可被授予的细粒度权限 key），
// 以及角色/权限的展示元数据。角色与权限的实体存储与分配见 store_adminaccount.go。
//
// 权限 key 采用与前端一致的规范命名（资源:动作，动作取 view/manage/approve/write），
// 前端 src/lib/permissions.tsx 中的 PERMISSIONS 为唯一权威来源，本字典与之对齐。

// 默认角色名（首次启动 SeedBootstrap 写入；之后可由管理员自定义新角色）。
const (
	RoleSuperAdmin = "super_admin" // 超级管理员（全部权限）
	RoleAdmin      = "admin"       // 运营管理员
	RoleOperator   = "operator"    // 只读操作员
)

// 权限 key 常量（与前端 PERMISSIONS 对齐）。
const (
	// 看板与风控
	PermOpsView  = "ops:view"  // 运营看板（账本/服务健康）
	PermRiskView = "risk:view" // 风控大盘查看
	PermRiskManage = "risk:manage" // 风控规则/黑名单管理（增删）

	// 用户与账户
	PermUserView  = "user:view"  // 用户列表查看
	PermUserWrite = "user:write" // 用户写入（创建/冻结/解冻）

	// 订单与交易
	PermTradeView   = "trade:view"   // 交易数据查看（订单/成交流水）
	PermTradeManage = "trade:manage" // 交易管理（撤销订单）

	// 审计日志
	PermAuditView = "audit:view" // 审计日志查看

	// API Key 管理
	PermApiKeyView   = "apikey:view"   // API Key 查看
	PermApiKeyManage = "apikey:manage" // API Key 管理（签发/吊销）

	// 资金/财务
	PermFinanceView    = "finance:view"    // 资金/财务查看（充值提币）
	PermFinanceApprove = "finance:approve" // 资金审批（提币审核）

	// C2C 交易
	PermC2CView   = "c2c:view"   // C2C 交易查看
	PermC2CManage = "c2c:manage" // C2C 交易管理（冻结订单）

	// 系统管理
	PermAdminManage  = "admin:manage"  // 管理员管理
	PermRoleManage   = "role:manage"   // 角色与权限管理
	PermSystemConfig = "system:config" // 系统配置（交易对/币种/公链）
	PermSysSettings  = "sys:settings"  // 安全设置

	// 运营
	PermNotificationWrite = "notification:write" // 发布运营通知
	PermAnnouncementWrite = "announcement:write" // 发布公告
)

// PermissionDef 是权限字典中的一条展示元数据。
type PermissionDef struct {
	Key   string `json:"key"`
	Name  string `json:"name"`  // 中文展示名
	Group string `json:"group"` // 分组（前端按组展示）
}

// allPermissionDefs 是全部可被授予的权限（用于权限分配 UI 的字典）。
// 与前端 src/lib/permissions.tsx 的 PERMISSIONS 保持一致。
var allPermissionDefs = []PermissionDef{
	{Key: PermOpsView, Name: "运营看板（账本/服务健康）", Group: "看板"},
	{Key: PermRiskView, Name: "风控大盘查看", Group: "看板"},
	{Key: PermRiskManage, Name: "风控规则/黑名单管理（增删）", Group: "看板"},

	{Key: PermUserView, Name: "用户列表查看", Group: "用户"},
	{Key: PermUserWrite, Name: "用户写入（创建/冻结/解冻）", Group: "用户"},

	{Key: PermTradeView, Name: "交易数据查看（订单/成交流水）", Group: "交易"},
	{Key: PermTradeManage, Name: "交易管理（撤销订单）", Group: "交易"},

	{Key: PermAuditView, Name: "审计日志查看", Group: "审计"},

	{Key: PermApiKeyView, Name: "API Key 查看", Group: "系统"},
	{Key: PermApiKeyManage, Name: "API Key 管理（签发/吊销）", Group: "系统"},

	{Key: PermFinanceView, Name: "资金/财务查看（充值提币）", Group: "资金"},
	{Key: PermFinanceApprove, Name: "资金审批（提币审核）", Group: "资金"},

	{Key: PermC2CView, Name: "C2C 交易查看", Group: "C2C"},
	{Key: PermC2CManage, Name: "C2C 交易管理（冻结订单）", Group: "C2C"},

	{Key: PermAdminManage, Name: "管理员管理", Group: "系统"},
	{Key: PermRoleManage, Name: "角色与权限管理", Group: "系统"},
	{Key: PermSystemConfig, Name: "系统配置（交易对/币种/公链）", Group: "系统"},
	{Key: PermSysSettings, Name: "安全设置", Group: "系统"},

	{Key: PermNotificationWrite, Name: "发布运营通知", Group: "运营"},
	{Key: PermAnnouncementWrite, Name: "发布公告", Group: "运营"},
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

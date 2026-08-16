package adminapi

import (
	"log"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/coldlar/crypto-exchange/internal/matching/client"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

// Server 是管理后台后端，聚合 7 个运营模块（风控/用户/交易对/账本/通知/充值提币/公链/币种），
// 并提供管理员账户/角色/权限（RBAC）与 MFA 自管理。
// 所有管理路由均套 Auth + AdminGuard，并按细粒度权限（RequirePerm）守卫。
// 读类模块（风控/账本/服务健康/通知）通过 UpstreamClient 实时聚合上游微服务，
// 上游不可达时优雅降级（返回已获取部分 + notes）。
type Server struct {
	cfg         *config.Config
	verifier    *middleware.TokenVerifier
	store       *Store
	up          *UpstreamClient
	matchClient *client.Client // 直连撮合引擎（cmd/matching），用于跨用户订单管理与撤销
	adminStore  AdminStore     // 管理员账户/角色/权限持久化（MySQL 优先，失败回退内存）
	catalog     CatalogStore   // 交易对/公链/币种/本地通知等管理员自有配置持久化（MySQL 优先，失败回退内存）
}

// NewServer 装配管理后台服务。verifier 使用全局 auth 共享密钥（与用户 token 同一密钥，
// 但 claims 带 role=admin 以示区分）；并自签一个长期 admin token 供聚合上游使用
// （上游 middleware.Auth 仅校验签名+过期，不强制 role）。
func NewServer(cfg *config.Config) *Server {
	verifier := middleware.NewTokenVerifier(cfg.Auth.Secret)
	selfToken := verifier.IssueRole(0, "admin", 365*24*time.Hour)

	// 管理员账户存储：优先 MySQL，连接/迁移失败则回退内存（本地无 MySQL 时可运行）。
	adminStore, isMem, storeErr := NewAdminStore(cfg.MySQL.DSN)
	if storeErr != nil {
		log.Printf("[admin] admin store: falling back to in-memory (mysql unavailable: %v)", storeErr)
	}
	if isMem {
		log.Printf("[admin] WARNING: admin accounts persisted in memory only; restart resets them. Configure mysql.dsn for real persistence.")
	}

	// 引导种子：config 的 admin 凭据 -> 超级管理员账户 + 默认三角色。
	bootstrapHash := cfg.Admin.PasswordHash
	if bootstrapHash == "" && cfg.Admin.Password != "" {
		if h, err := bcrypt.GenerateFromPassword([]byte(cfg.Admin.Password), bcrypt.DefaultCost); err == nil {
			bootstrapHash = string(h)
		}
	}
	if err := SeedBootstrap(adminStore, cfg.Admin.Username, bootstrapHash); err != nil {
		log.Printf("[admin] seed bootstrap admin failed: %v", err)
	}

	// 管理员自有配置存储：优先 MySQL，连接/迁移失败则回退内存（本地无 MySQL 时可运行）。
	catalog, isMemCat, catErr := NewCatalogStore(cfg.MySQL.DSN)
	if catErr != nil {
		log.Printf("[admin] catalog store: falling back to in-memory (mysql unavailable: %v)", catErr)
	}
	if isMemCat {
		log.Printf("[admin] WARNING: trading pairs/chains/coins/notifications persisted in memory only; restart resets them. Configure mysql.dsn for real persistence.")
	}
	if err := SeedCatalog(catalog); err != nil {
		log.Printf("[admin] seed catalog failed: %v", err)
	}

	return &Server{
		cfg:         cfg,
		verifier:    verifier,
		store:       NewStore(),
		up:          NewUpstreamClient(selfToken),
		matchClient: client.New(cfg.Matching.URL),
		adminStore:  adminStore,
		catalog:     catalog,
	}
}

// serviceURL 返回某上游服务的基址（来自 config.Services 映射）。
func (s *Server) serviceURL(name string) string {
	return s.cfg.Services[name]
}

// RegisterRoutes 把管理后台路由挂到 gin 引擎。
// 路由统一挂在 /api/admin 前缀下，与 web-admin 前端（Vite 代理 /api/admin -> :8095）契约一致。
// 登录与健康检查不需要 admin 角色；其余全部受 Auth + AdminGuard 保护。
func (s *Server) RegisterRoutes(r *gin.Engine) {
	r.POST("/api/admin/login", s.handleLogin)
	r.GET("/api/admin/health", s.handleHealth)

	admin := r.Group("/api/admin", middleware.Auth(s.verifier), middleware.AdminGuard())
	{
		// 风控与强平监控（实时聚合 futures）
		admin.GET("/risk", s.handleRisk)

		// 用户与账户管理（代理 user 服务 /admin/* 真实持久化）
		admin.GET("/users", s.listUsers)
		admin.POST("/users", s.createUser)
		admin.PUT("/users/:id", s.updateUser)
		admin.POST("/users/:id/freeze", s.freezeUser)
		admin.POST("/users/:id/unfreeze", s.unfreezeUser)

		// 交易对/参数配置（admin 自有持久化）
		admin.GET("/symbols", s.listSymbols)
		admin.POST("/symbols", s.upsertSymbol)
		admin.PUT("/symbols/:symbol", s.upsertSymbol)

		// 公链管理（admin 自有持久化）
		admin.GET("/chains", s.listChains)
		admin.POST("/chains", s.createChain)
		admin.PUT("/chains/:id", s.updateChain)

		// 币种管理（admin 自有持久化）
		admin.GET("/coins", s.listCoins)
		admin.POST("/coins", s.createCoin)
		admin.PUT("/coins/:id", s.updateCoin)

		// 充值提币记录（实时聚合 futures 链上事件）
		admin.GET("/deposits", s.listDeposits)
		admin.GET("/withdrawals", s.listWithdrawals)
		admin.POST("/withdrawals/:id/approve", middleware.RequirePerm(PermWithdrawApproval), s.approveWithdrawal)
		admin.POST("/withdrawals/:id/reject", middleware.RequirePerm(PermWithdrawApproval), s.rejectWithdrawal)

		// 运营通知管理（list 实时聚合 notification 服务）
		admin.GET("/notifications", s.listNotifications)
		admin.POST("/notifications", s.createNotification)
		admin.DELETE("/notifications/:id", s.deleteNotification)

		// 运营看板：账本对账（实时聚合 futures）+ 服务健康（探活各服务）
		admin.GET("/ledger", s.handleLedger)
		admin.GET("/services", s.handleServices)

		// 订单管理（跨用户查询/撤销，运营风控用）。读需 trade:read，撤销需 trade:manage（高危）。
		orders := admin.Group("/orders", middleware.RequirePerm(PermTradeRead))
		{
			orders.GET("", s.handleAdminOrders)
			orders.GET("/:id", s.handleAdminOrderDetail)
		}
		admin.GET("/trades", middleware.RequirePerm(PermTradeRead), s.handleAdminTrades)
		admin.POST("/orders/:id/cancel", middleware.RequirePerm(PermTradeManage), s.handleAdminCancelOrder)

		// 当前管理员自身信息（含角色/权限）；任何已登录管理员可访问。
		admin.GET("/me", s.adminMe)

		// 当前管理员自管理：修改密码、绑定/启用/关闭 Google 验证器（本人操作）。
		admin.POST("/password", s.changePassword)
		admin.POST("/mfa/setup", s.setupMFA)
		admin.POST("/mfa/enable", s.enableMFA)
		admin.POST("/mfa/disable", s.disableMFA)

		// 管理员账户管理（需 admin:manage 权限）
		admins := admin.Group("/admins", middleware.RequirePerm(PermAdminManage))
		{
			admins.GET("", s.listAdmins)
			admins.POST("", s.createAdmin)
			admins.PUT("/:id", s.updateAdmin)
			admins.POST("/:id/activate", s.activateAdmin)
			admins.POST("/:id/disable", s.disableAdmin)
			admins.POST("/:id/reset-password", s.resetAdminPassword)
		}

		// 角色与权限管理（需 role:manage 权限）
		roles := admin.Group("/roles", middleware.RequirePerm(PermRoleManage))
		{
			roles.GET("", s.listRoles)
			roles.POST("", s.createRole)
			roles.PUT("/:id/permissions", s.setRolePermissions)
			roles.DELETE("/:id", s.deleteRole)
		}
		admin.GET("/permissions", middleware.RequirePerm(PermRoleManage), s.listPermissionDict)
	}
}

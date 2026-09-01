package adminapi

import (
	"log"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/coldlar/crypto-exchange/internal/announcement"
	"github.com/coldlar/crypto-exchange/internal/apikeys"
	"github.com/coldlar/crypto-exchange/internal/matching/client"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/referral"
)

// Server 是管理后台后端，聚合 7 个运营模块（风控/用户/交易对/账本/通知/充值提币/公链/币种），
// 并提供管理员账户/角色/权限（RBAC）与 MFA 自管理。
// 所有管理路由均套 Auth + AdminGuard，并按细粒度权限（RequirePerm）守卫。
// 读类模块（风控/账本/服务健康/通知）通过 UpstreamClient 实时聚合上游微服务，
// 上游不可达时优雅降级（返回已获取部分 + notes）。
type Server struct {
	cfg           *config.Config
	verifier      *middleware.TokenVerifier
	store         *Store
	up            *UpstreamClient
	matchClient   *client.Client        // 直连撮合引擎（cmd/matching），用于跨用户订单管理与撤销
	adminStore    AdminStore            // 管理员账户/角色/权限持久化（MySQL 优先，失败回退内存）
	catalog       CatalogStore          // 交易对/公链/币种/本地通知等管理员自有配置持久化（MySQL 优先，失败回退内存）
	annH          *announcement.Handler // 公告管理（与用户服务共用同一份 ce_announcements 表与迁移版本 9401）
	annSvc        *announcement.Service // 公告服务（演示种子注入用）
	auditStore    AuditStore            // 管理员操作审计日志（MySQL 优先，失败回退内存）
	apiKeyStore   apikeys.Store         // API Key 管理（管理员为任意用户签发/吊销）
	referralStore referral.Store        // 邀请佣金查询（替代每请求 sql.Open）
	loginLimiter  *loginIPLimiter       // 基于 IP 的登录限流（防单 IP 爆破 + 缓解账户锁定 DoS）
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

	// 公告管理：与用户服务共用同一数据库（同一份 ce_schema_migrations，版本号 9401 已错开）。
	// 优先 MySQL；DSN 缺失则降级为内存实现（重启即丢，仅开发用）。
	var annStore announcement.Store
	if cfg.MySQL.DSN != "" {
		st, annErr := announcement.NewMySQLStore(cfg.MySQL.DSN)
		if annErr != nil {
			log.Printf("[admin] announcement store: falling back to in-memory (mysql unavailable: %v)", annErr)
		} else {
			annStore = st
		}
	}
	if annStore == nil {
		annStore = announcement.NewMemStore()
	}
	annSvc := announcement.NewService(annStore)
	annH := announcement.NewHandler(annSvc)

	// 审计日志存储：优先 MySQL；DSN 缺失或连接/迁移失败则降级为内存实现。
	auditStore, isMemAudit, auditErr := NewAuditStore(cfg.MySQL.DSN)
	if auditErr != nil {
		log.Printf("[admin] audit store: falling back to in-memory (mysql unavailable: %v)", auditErr)
	}
	if isMemAudit {
		log.Printf("[admin] WARNING: audit logs persisted in memory only; restart resets them. Configure mysql.dsn for real persistence.")
	}

	// API Key 管理存储：优先 MySQL；DSN 缺失或连接/迁移失败则降级为内存实现。
	apiKeyStore := apikeys.NewStore(cfg.MySQL.DSN)

	// 邀请佣金存储：优先 MySQL；DSN 缺失或连接/迁移失败则降级为内存实现。
	var referralStore referral.Store
	if cfg.MySQL.DSN != "" {
		rs, rsErr := referral.NewMySQLStore(cfg.MySQL.DSN)
		if rsErr != nil {
			log.Printf("[admin] referral store: falling back to in-memory (mysql unavailable: %v)", rsErr)
		} else {
			referralStore = rs
		}
	}
	if referralStore == nil {
		referralStore = referral.NewMemStore()
	}

	return &Server{
		cfg:           cfg,
		verifier:      verifier,
		store:         NewStore(),
		up:            NewUpstreamClient(selfToken),
		matchClient:   client.New(cfg.Matching.URL),
		adminStore:    adminStore,
		catalog:       catalog,
		annH:          annH,
		annSvc:        annSvc,
		auditStore:    auditStore,
		apiKeyStore:   apiKeyStore,
		referralStore: referralStore,
		loginLimiter: newLoginIPLimiter(
			cfg.Admin.LoginRateLimitPerIP,
			time.Duration(cfg.Admin.LoginRateWindowSec)*time.Second,
		),
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
	admin.Use(s.auditMiddleware()) // 记录所有变更类操作（GET 不记）
	{
		// 风控与强平监控（实时聚合 futures）
		admin.GET("/risk", s.handleRisk)

		// 风控管理（代理 risk 服务规则/黑名单 CRUD）。读需 risk:view，增删需 risk:manage。
		risk := admin.Group("/risk", middleware.RequirePerm(PermRiskView))
		{
			risk.GET("/rules", s.handleRiskRulesList)
			risk.POST("/rules", middleware.RequirePerm(PermRiskManage), s.handleRiskRuleCreate)
			risk.GET("/blacklist", s.handleRiskBlacklistList)
			risk.POST("/blacklist", middleware.RequirePerm(PermRiskManage), s.handleRiskBlacklistCreate)
			risk.DELETE("/blacklist", middleware.RequirePerm(PermRiskManage), s.handleRiskBlacklistDelete)
			risk.GET("/blacklist/check", s.handleRiskBlacklistCheck)
			risk.POST("/check/withdraw", s.handleRiskCheckWithdraw)
		}

		// 用户与账户管理（代理 user 服务 /admin/* 真实持久化）
		admin.GET("/users", s.listUsers)
		admin.POST("/users", s.createUser)
		admin.PUT("/users/:id", s.updateUser)
		admin.POST("/users/:id/freeze", s.freezeUser)
		admin.POST("/users/:id/unfreeze", s.unfreezeUser)
		admin.POST("/users/:id/tfa/reset", s.resetUserTFA)
		admin.GET("/users/:id/balances", s.getUserBalances)

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
		admin.GET("/pending-withdrawals", middleware.RequirePerm(PermFinanceApprove), s.listPendingWithdrawals)
		admin.GET("/withdrawals/:id/detail", middleware.RequirePerm(PermFinanceApprove), s.getWithdrawalDetail)

		// 用户充值地址（按 userID 在各充值链确定性派生，无持久化）
		admin.GET("/deposit-addresses", s.listUserDepositAddresses)
		admin.POST("/withdrawals/:id/approve", middleware.RequirePerm(PermFinanceApprove), s.approveWithdrawal)
		admin.POST("/withdrawals/:id/reject", middleware.RequirePerm(PermFinanceApprove), s.rejectWithdrawal)

		// 运营通知管理（list 实时聚合 notification 服务）
		admin.GET("/notifications", s.listNotifications)
		admin.POST("/notifications", s.createNotification)
		admin.DELETE("/notifications/:id", s.deleteNotification)

		// KYC 审核
		kyc := admin.Group("/kyc-reviews", middleware.RequirePerm(PermUserView))
		{
			kyc.GET("", s.listKycReviews)
			kyc.GET("/:id", s.getKycReviewDetail)
			kyc.POST("/:id/approve", middleware.RequirePerm(PermFinanceApprove), s.approveKyc)
			kyc.POST("/:id/reject", middleware.RequirePerm(PermFinanceApprove), s.rejectKyc)
		}

		// 公告管理（与用户服务共用同一份 ce_announcements 数据；管理组已套 Auth+AdminGuard）。
		s.annH.RegisterAdminRoutes(admin)

		// 理财资管（代理 wealth 服务）
		wealth := admin.Group("/wealth", middleware.RequirePerm(PermSystemConfig))
		{
			wealth.GET("/products", s.listWealthProducts)
			wealth.POST("/products", s.createWealthProduct)
			wealth.GET("/holdings", s.listWealthHoldings)
			wealth.POST("/holdings/accrue", s.accrueWealth)
		}

		// 运营看板：账本对账（实时聚合 futures）+ 服务健康（探活各服务）
		admin.GET("/ledger", s.handleLedger)
		admin.GET("/services", s.handleServices)

		// 订单管理（跨用户查询/撤销，运营风控用）。读需 trade:read，撤销需 trade:manage（高危）。
		orders := admin.Group("/orders", middleware.RequirePerm(PermTradeView))
		{
			orders.GET("", s.handleAdminOrders)
			orders.GET("/:id", s.handleAdminOrderDetail)
		}
		admin.GET("/trades", middleware.RequirePerm(PermTradeView), s.handleAdminTrades)
		admin.POST("/orders/:id/cancel", middleware.RequirePerm(PermTradeManage), s.handleAdminCancelOrder)

		// 当前管理员自身信息（含角色/权限）；任何已登录管理员可访问。
		admin.GET("/me", s.adminMe)

		// 当前管理员自身偏好（语言/主题/时区）；任何已登录管理员可访问。
		admin.GET("/preferences", s.getAdminPreferences)
		admin.PUT("/preferences", s.updateAdminPreferences)

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
			roles.PUT("/:id", s.updateRole)
			roles.PUT("/:id/permissions", s.setRolePermissions)
			roles.DELETE("/:id", s.deleteRole)
		}
		admin.GET("/permissions", middleware.RequirePerm(PermRoleManage), s.listPermissionDict)

		// 审计日志（需 audit:read 权限）
		admin.GET("/audit-logs", middleware.RequirePerm(PermAuditView), s.handleAuditLogs)

		// API Key 管理（管理员为任意用户签发/吊销）。读需 apikey:read，签发/吊销需 apikey:manage（高危）。
		apikeys := admin.Group("/apikeys", middleware.RequirePerm(PermApiKeyView))
		{
			apikeys.GET("", s.listApiKeys)
			apikeys.GET("/:id", s.getApiKey)
			apikeys.POST("", middleware.RequirePerm(PermApiKeyManage), s.createApiKey)
			apikeys.DELETE("/:id", middleware.RequirePerm(PermApiKeyManage), s.revokeApiKey)
		}

		// 借贷管理（代理 lending 服务 /admin/* 端点）
		admin.GET("/lending/pools", s.handleAdminLendingPools)
		admin.POST("/lending/pools", s.handleAdminLendingPoolsCreate)
		admin.GET("/lending/lends", s.handleAdminLendingLends)
		admin.GET("/lending/borrows", s.handleAdminLendingBorrows)

		// 交易机器人管理（代理 bot 服务 /admin/* 端点）
		admin.GET("/bot/strategies", s.handleAdminBotStrategies)
		admin.POST("/bot/strategies/:id/tick", s.handleAdminBotTick)

		// 邀请佣金管理（直查 ce_referral_commissions 表）
		admin.GET("/referral/commissions", s.handleAdminReferralCommissions)
	}
}

// SeedDemoAnnouncements 演示公告种子：无论内存还是连库模式都幂等注入若干公告，
// 供管理后台「公告管理」页展示测试。由 cmd/admin 在启动时显式调用（不在 NewServer 内，
// 以免污染依赖 NewServer 的单元测试所需初始态）。连库模式按标题判重，重启不重复插入。
func (s *Server) SeedDemoAnnouncements() {
	if s.annSvc == nil {
		return
	}
	seedDemoAnnouncements(s.annSvc)
}

// seedDemoAnnouncements 演示公告种子：按标题判重幂等注入若干公告，避免「公告管理」页为空，
// 同时避免连库模式下每次重启重复插入同一批演示公告。
func seedDemoAnnouncements(annSvc *announcement.Service) {
	str := func(s string) *string { return &s }
	active := true
	demo := []announcement.AnnouncementInput{
		{Level: str(announcement.LevelMaintenance), Title: str("系统升级通知"), Content: str("为提升服务稳定性，平台将于本周六 02:00-04:00 进行系统升级，期间部分功能可能短暂不可用，敬请谅解。"), Active: &active},
		{Level: str(announcement.LevelWarning), Title: str("安全提醒"), Content: str("请勿向任何人泄露您的账户密码或验证码，谨防钓鱼网站与冒充客服的诈骗行为。"), Active: &active},
		{Level: str(announcement.LevelInfo), Title: str("新币上线"), Content: str("平台将上线 ETH/USDT 合约交易对，敬请期待。"), Active: &active},
		{Level: str(announcement.LevelInfo), Title: str("费率调整说明"), Content: str("自下月起基础费率维持不变，VIP 等级费率权益详见帮助中心。"), Active: &active},
	}
	existing := map[string]bool{}
	if all, err := annSvc.ListAll(); err == nil {
		for _, a := range all {
			existing[a.Title] = true
		}
	}
	created := 0
	for _, in := range demo {
		if in.Title != nil && existing[*in.Title] {
			continue // 已有同标题公告，跳过（幂等）
		}
		if _, err := annSvc.Create(in); err != nil {
			log.Printf("[admin] seed demo announcement failed: %v", err)
			continue
		}
		created++
	}
	log.Printf("[admin] seeded demo announcements (%d created, existing skipped)", created)
}

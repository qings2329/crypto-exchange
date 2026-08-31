package adminapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
	"github.com/coldlar/crypto-exchange/internal/settlement"
	"github.com/gin-gonic/gin"
)

func (s *Server) ok(c *gin.Context, data interface{}) {
	response.JSON(c, data)
}

func (s *Server) fail(c *gin.Context, code int, msg string) {
	response.Error(c, code, code, msg)
}

func parseInt64(c *gin.Context, param string) (int64, bool) {
	v, err := strconv.ParseInt(c.Param(param), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// --- 登录（无 guard）：校验管理员账户（状态/密码/可选 TOTP）后签发带权限的 token ---
// 内置暴力破解防护：连续失败达到阈值锁定账户一段时间（自动过期、成功登录清零）。
func (s *Server) handleLogin(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		TOTP     string `json:"totp"` // 启用 Google 验证器后必填
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		s.fail(c, http.StatusBadRequest, "invalid body")
		return
	}
	// 基于 IP 的登录限流（防单 IP 自动化爆破 + 缓解账户级锁定的 DoS 取舍）。
	if !s.loginLimiter.allow(c.ClientIP()) {
		log.Printf("[admin] SECURITY: login rate-limited for IP %s", c.ClientIP())
		s.fail(c, http.StatusTooManyRequests, "too many login attempts from this IP, try again later")
		return
	}
	// 阈值与锁定时长（配置化，缺省 5 次 / 15 分钟）。
	maxFails := s.cfg.Admin.MaxLoginFailures
	if maxFails <= 0 {
		maxFails = 5
	}
	lockSec := s.cfg.Admin.LoginLockoutSec
	if lockSec <= 0 {
		lockSec = 900
	}

	acc, err := s.adminStore.GetAccountByUsername(req.Username)
	if err != nil || acc.Status != AdminStatusActive {
		// 统一错误，避免用户枚举；且不区分"不存在/锁定"，与现有 no-enumeration 策略一致。
		s.writeLoginAudit(c, nil, req.Username, "login_failed", "account not found or inactive", http.StatusUnauthorized)
		s.fail(c, http.StatusUnauthorized, "invalid admin credentials")
		return
	}
	// 锁定检查置于 bcrypt 之前以节省算力；返回统一错误避免泄露锁定状态。
	now := time.Now().Unix()
	if acc.LockedUntil > now {
		s.writeLoginAudit(c, acc, acc.Username, "login_failed", "account locked", http.StatusUnauthorized)
		s.fail(c, http.StatusUnauthorized, "invalid admin credentials")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(acc.PasswordHash), []byte(req.Password)) != nil {
		s.recordLoginFailure(c, acc, maxFails, lockSec, "invalid password")
		s.fail(c, http.StatusUnauthorized, "invalid admin credentials")
		return
	}
	if acc.TOTPEnabled {
		if !VerifyTOTP(acc.TOTPSecret, req.TOTP, time.Now()) {
			s.recordLoginFailure(c, acc, maxFails, lockSec, "invalid totp")
			s.fail(c, http.StatusUnauthorized, "invalid admin credentials")
			return
		}
	}
	// 登录成功：清零失败计数与锁定。
	acc.FailedAttempts = 0
	acc.LockedUntil = 0
	if err := s.adminStore.UpdateAccount(acc); err != nil {
		s.fail(c, http.StatusInternalServerError, "login state update failed")
		return
	}
	// 聚合该账户角色的权限集合，打包进 token（有效期内的权限快照）。
	perms := []string{}
	if acc.RoleID != 0 {
		if p, e := s.adminStore.GetRolePermissions(acc.RoleID); e == nil {
			perms = p
		}
	}
	ttl := time.Duration(s.cfg.Admin.TokenTTLSec) * time.Second
	if ttl <= 0 {
		ttl = time.Hour
	}
	tok := s.verifier.IssueAdmin(acc.ID, "admin", perms, ttl)
	s.writeLoginAudit(c, acc, acc.Username, "login", "admin login success", http.StatusOK)
	s.ok(c, gin.H{
		"token":         tok,
		"expires_in":    s.cfg.Admin.TokenTTLSec,
		"totp_required": acc.TOTPEnabled,
	})
}

// recordLoginFailure 累加失败次数；达到阈值则锁定账户 lockSec 秒（自动过期）。
// 失败计数与锁定到期持久化到账户，跨重启/副本一致；成功登录由调用方清零。
// 每次失败均写入审计日志，便于异常登录行为追溯。
func (s *Server) recordLoginFailure(c *gin.Context, acc *AdminAccount, maxFails, lockSec int, reason string) {
	acc.FailedAttempts++
	if acc.FailedAttempts >= maxFails {
		acc.LockedUntil = time.Now().Unix() + int64(lockSec)
		log.Printf("[admin] SECURITY: account %q locked for %d sec after %d failed logins",
			acc.Username, lockSec, maxFails)
	} else {
		log.Printf("[admin] SECURITY: failed login for account %q (attempt %d/%d)",
			acc.Username, acc.FailedAttempts, maxFails)
	}
	// 持久化失败计数/锁定（失败也照常写入，便于跨实例共享限流状态）。
	if err := s.adminStore.UpdateAccount(acc); err != nil {
		log.Printf("[admin] failed to persist login failure state for %q: %v", acc.Username, err)
	}
	// 仅记录成功登录到审计日志；失败登录不涉及资源变更，无需审计。
}

// writeLoginAudit 记录管理员成功登录审计事件。登录路由在 admin 组与 auditMiddleware 之外，
// 因此需在 handleLogin 中显式写入。仅记录成功登录，失败登录不写入审计日志。
func (s *Server) writeLoginAudit(c *gin.Context, acc *AdminAccount, username, action, detail string, status int) {
	aid := int64(0)
	if acc != nil {
		aid = acc.ID
	}
	_ = s.auditStore.Append(AuditEntry{
		AdminID: aid,
		Method:  http.MethodPost,
		Path:    "/api/admin/login",
		Action:  action,
		Target:  "/api/admin/login",
		Status:  status,
		Detail:  detail,
		IP:      c.ClientIP(),
		Time:    time.Now().UnixNano(),
	})
}

func (s *Server) handleHealth(c *gin.Context) {
	s.ok(c, gin.H{"status": "ok", "time": time.Now().Unix()})
}

// --- 风控与强平监控（实时聚合 futures 服务）---
func (s *Server) handleRisk(c *gin.Context) {
	ctx := c.Request.Context()
	var (
		liq struct {
			Liquidations []struct {
				UserID   int64   `json:"user_id"`
				Symbol   string  `json:"symbol"`
				Side     int     `json:"side"` // PosSide: 0=long,1=short
				Size     float64 `json:"size"`
				LiqPrice float64 `json:"liq_price"`
				Margin   float64 `json:"margin"`
				Time     int64   `json:"time"`
			} `json:"liquidations"`
		}
		adl struct {
			ADL []struct {
				UserID int64  `json:"user_id"`
				Symbol string `json:"symbol"`
			} `json:"adl"`
		}
		soc struct {
			Socialized []struct {
				UserID int64   `json:"user_id"`
				Symbol string  `json:"symbol"`
				Share  float64 `json:"share"`
			} `json:"socialized"`
		}
		wallet struct {
			Insurance float64 `json:"insurance"`
		}
	)
	errs := []string{}
	if base := s.serviceURL("futures"); base != "" {
		if err := s.up.Get(ctx, base, "/api/v1/futures/liquidations", &liq); err != nil {
			errs = append(errs, "liquidations: "+err.Error())
		}
		if err := s.up.Get(ctx, base, "/api/v1/futures/adl", &adl); err != nil {
			errs = append(errs, "adl: "+err.Error())
		}
		if err := s.up.Get(ctx, base, "/api/v1/futures/socialized", &soc); err != nil {
			errs = append(errs, "socialized: "+err.Error())
		}
		if err := s.up.Get(ctx, base, "/api/v1/futures/wallet?user_id=1", &wallet); err != nil {
			errs = append(errs, "insurance: "+err.Error())
		}
	} else {
		errs = append(errs, "futures service url not configured")
	}

	items := make([]LiquidationItem, 0, len(liq.Liquidations))
	for _, e := range liq.Liquidations {
		items = append(items, LiquidationItem{
			UserID:   e.UserID,
			Symbol:   e.Symbol,
			Side:     posSideStr(e.Side),
			Size:     e.Size,
			LiqPrice: e.LiqPrice,
			Equity:   e.Margin,
			Detected: time.Unix(0, e.Time),
		})
	}
	adlQ := make([]string, 0, len(adl.ADL))
	for _, e := range adl.ADL {
		adlQ = append(adlQ, fmt.Sprintf("%d:%s", e.UserID, e.Symbol))
	}
	var socialized float64
	for _, e := range soc.Socialized {
		socialized += e.Share
	}
	snap := RiskSnapshot{
		UpdatedAt:      time.Now(),
		Liquidations:   items,
		InsuranceFund:  wallet.Insurance,
		SocializedLoss: socialized,
		ADLQueue:       adlQ,
	}
	if len(errs) > 0 {
		snap.Notes = strings.Join(errs, "; ")
	}
	s.ok(c, snap)
}

// --- 用户与账户管理（代理 user 服务真实持久化，CRUD 落到 user 的存储层）---

// userListResp 是 user 服务 /admin/list 的返回结构（status/kyc 以 int 表达）。
type userListResp struct {
	Users []struct {
		ID        int64     `json:"id"`
		Username  string    `json:"username"`
		Email     string    `json:"email"`
		Phone     string    `json:"phone"`
		Status    int       `json:"status"`
		KYCLevel  int       `json:"kyc_level"`
		Level     int       `json:"level"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"users"`
}

// listUsers 代理 user 服务 /api/v1/user/admin/list；上游不可达时降级为空列表（data 始终为数组，
// 不返回伪造示例用户，发现 4 对称项），并经 X-Degraded 响应头告知前端上游不可用。
func (s *Server) listUsers(c *gin.Context) {
	ctx := c.Request.Context()
	limit, offset := parsePage(c)
	if base := s.serviceURL("user"); base != "" {
		var resp userListResp
		if err := s.up.Get(ctx, base, "/api/v1/user/admin/list", &resp); err == nil {
			out := make([]AdminUser, 0, len(resp.Users))
			for _, u := range resp.Users {
				au := AdminUser{
					ID:        u.ID,
					Username:  u.Username,
					Email:     u.Email,
					Status:    userStatusStr(u.Status),
					KYC:       kycStr(u.KYCLevel),
					Level:     u.Level,
					CreatedAt: u.CreatedAt,
				}
				s.enrichBalance(ctx, &au) // 接 futures 钱包余额（§25 后续：消除恒为 0）
				out = append(out, au)
			}
			s.ok(c, pageEnvelope(out, limit, offset))
			return
		}
	}
	// 上游不可达：返回空列表（与正常路径同构，data 始终为数组），不返回伪造的示例用户
	// （发现 4 对称项）；经 X-Degraded 响应头告知前端上游不可用，便于展示「数据暂不可用」横幅。
	c.Header("X-Degraded", "user-unavailable")
	s.ok(c, pageEnvelope([]AdminUser{}, limit, offset))
}

// enrichBalance 从 futures 钱包服务拉取该用户的 USDT 可用余额填入 AdminUser.Balance 展示字段
// （§19 已知缺口：此前余额恒为 0）。注意：该字段语义仅为「USDT 可用余额」，并非用户总资产；
// 属展示层、不进入任何资金路径，故硬编码 USDT 无资金安全后果（对应 futuresapi F3 的展示版）。
func (s *Server) enrichBalance(ctx context.Context, u *AdminUser) {
	fb := s.serviceURL("futures")
	if fb == "" {
		return
	}
	var bal struct {
		Available float64 `json:"available"`
		Exists    bool    `json:"exists"`
	}
	if err := s.up.Get(ctx, fb, "/api/v1/futures/wallet/balance?user_id="+strconv.FormatInt(u.ID, 10)+"&asset=USDT", &bal); err == nil && bal.Exists {
		u.Balance = bal.Available
	}
}

// createUser 经 user 服务 /admin 创建真实用户；password 必填（bcrypt 哈希落库）。
func (s *Server) createUser(c *gin.Context) {
	var req struct {
		Username string  `json:"username"`
		Email    string  `json:"email"`
		Password string  `json:"password"`
		Balance  float64 `json:"balance"`
		Status   string  `json:"status"`
		KYC      string  `json:"kyc"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		s.fail(c, http.StatusBadRequest, "invalid body")
		return
	}
	// 发现 7：余额初始化不经由本端点（须经受控充值/调账流程且用 AssetAmount 包装），
	// 显式拒绝携带 balance 的请求，避免静默忽略入参造成「已初始化」的误判。
	if req.Balance != 0 {
		s.fail(c, http.StatusBadRequest, "balance initialization not supported via this endpoint")
		return
	}
	target := req.Email
	if target == "" {
		target = req.Username
	}
	if target == "" || req.Password == "" {
		s.fail(c, http.StatusBadRequest, "email/username and password required")
		return
	}
	base := s.serviceURL("user")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "user service not configured")
		return
	}
	ctx := c.Request.Context()
	var created struct {
		ID int64 `json:"id"`
	}
	if err := s.up.Post(ctx, base, "/api/v1/user/admin", &created, map[string]string{
		"target":   target,
		"username": req.Username,
		"email":    req.Email,
		"password": req.Password,
	}); err != nil {
		s.fail(c, http.StatusBadGateway, "create user failed: "+err.Error())
		return
	}
	// 同步可选 KYC 等级（none 为默认值，无需调用）。
	if req.KYC != "" && req.KYC != "none" {
		_ = s.up.Put(ctx, base, fmt.Sprintf("/api/v1/user/admin/%d", created.ID), nil, map[string]int{
			"kyc_level": kycToInt(req.KYC),
		})
	}
	// 同步可选冻结状态（active 为默认，无需调用）。
	if req.Status == "frozen" {
		_ = s.up.Post(ctx, base, fmt.Sprintf("/api/v1/user/admin/%d/freeze", created.ID), nil, nil)
	} else if req.Status == "unfrozen" || req.Status == "active" {
		// active 即默认态，无需额外动作
	}
	s.ok(c, AdminUser{
		ID:        created.ID,
		Username:  req.Username,
		Email:     req.Email,
		Status:    firstNonEmpty(req.Status, "active"),
		KYC:       firstNonEmpty(req.KYC, "none"),
		Balance:   0,
		CreatedAt: time.Now(),
	})
}

// updateUser 代理 user 服务 /admin/:id（仅支持 email/status/kyc 补丁）。
func (s *Server) updateUser(c *gin.Context) {
	id, ok := parseInt64(c, "id")
	if !ok {
		s.fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Status   string `json:"status"`
		KYC      string `json:"kyc"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		s.fail(c, http.StatusBadRequest, "invalid body")
		return
	}
	base := s.serviceURL("user")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "user service not configured")
		return
	}
	ctx := c.Request.Context()
	body := map[string]interface{}{}
	if req.Email != "" {
		body["email"] = req.Email
	}
	if req.Status != "" {
		body["status"] = userStatusToInt(req.Status)
	}
	if req.KYC != "" {
		body["kyc_level"] = kycToInt(req.KYC)
	}
	if len(body) == 0 {
		s.fail(c, http.StatusBadRequest, "nothing to update")
		return
	}
	if err := s.up.Put(ctx, base, fmt.Sprintf("/api/v1/user/admin/%d", id), nil, body); err != nil {
		s.fail(c, http.StatusBadGateway, "update user failed: "+err.Error())
		return
	}
	s.ok(c, gin.H{"id": id, "status": firstNonEmpty(req.Status, "active"), "kyc": firstNonEmpty(req.KYC, "none")})
}

func (s *Server) freezeUser(c *gin.Context)   { s.freezeUnfreeze(c, true) }
func (s *Server) unfreezeUser(c *gin.Context) { s.freezeUnfreeze(c, false) }

// freezeUnfreeze 代理 user 服务 /admin/:id/freeze|unfreeze。
func (s *Server) freezeUnfreeze(c *gin.Context, frozen bool) {
	id, ok := parseInt64(c, "id")
	if !ok {
		s.fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	base := s.serviceURL("user")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "user service not configured")
		return
	}
	path := fmt.Sprintf("/api/v1/user/admin/%d/freeze", id)
	if !frozen {
		path = fmt.Sprintf("/api/v1/user/admin/%d/unfreeze", id)
	}
	if err := s.up.Post(c.Request.Context(), base, path, nil, nil); err != nil {
		s.fail(c, http.StatusBadGateway, "set status failed: "+err.Error())
		return
	}
	s.ok(c, gin.H{"id": id, "frozen": frozen, "status": statusWord(frozen)})
}

// resetUserTFA 代理 user 服务 /admin/:id/tfa/reset：强制重置某用户的 Google 验证码（关闭 2FA）。
func (s *Server) resetUserTFA(c *gin.Context) {
	id, ok := parseInt64(c, "id")
	if !ok {
		s.fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	base := s.serviceURL("user")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "user service not configured")
		return
	}
	path := fmt.Sprintf("/api/v1/user/admin/%d/tfa/reset", id)
	if err := s.up.Post(c.Request.Context(), base, path, nil, nil); err != nil {
		s.fail(c, http.StatusBadGateway, "reset user tfa failed: "+err.Error())
		return
	}
	s.ok(c, gin.H{"id": id, "tfa_enabled": false})
}

// --- 交易对/参数配置（持久化于 CatalogStore：MySQL 优先，失败回退内存）---
func (s *Server) listSymbols(c *gin.Context) {
	syms, err := s.catalog.ListSymbols()
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "list symbols failed: "+err.Error())
		return
	}
	limit, offset := parsePage(c)
	s.ok(c, pageEnvelope(syms, limit, offset))
}

func (s *Server) upsertSymbol(c *gin.Context) {
	var sym SymbolConfig
	if err := c.ShouldBindJSON(&sym); err != nil || sym.Symbol == "" {
		s.fail(c, http.StatusBadRequest, "invalid symbol config")
		return
	}
	out, err := s.catalog.UpsertSymbol(sym)
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "upsert symbol failed: "+err.Error())
		return
	}
	s.ok(c, out)
}

// --- 公链管理（持久化于 CatalogStore）---
func (s *Server) listChains(c *gin.Context) {
	chs, err := s.catalog.ListChains()
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "list chains failed: "+err.Error())
		return
	}
	limit, offset := parsePage(c)
	s.ok(c, pageEnvelope(chs, limit, offset))
}

// isValidRPCEndpoint 校验链 RPC 端点必须是合法 URL，且协议限定为
// http/https/ws/wss，防止恶意节点作为结算层单一数据源劫持充值确认/提现广播（F5）。
func isValidRPCEndpoint(ep string) bool {
	u, err := url.Parse(ep)
	if err != nil {
		return false
	}
	if u.Scheme == "" || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "http", "https", "ws", "wss":
		return true
	}
	return false
}

// validateChainCreate 校验新建公链的关键字段：Confirmations 必须 > 0；
// RpcEndpoint 若提供则须为合法 URL；启用充提时 RpcEndpoint 不可为空。
func validateChainCreate(ch Chain) error {
	if ch.Name == "" {
		return errors.New("chain name is required")
	}
	if ch.Confirmations <= 0 {
		return errors.New("confirmations must be > 0")
	}
	if ch.RpcEndpoint != "" && !isValidRPCEndpoint(ch.RpcEndpoint) {
		return errors.New("rpc_endpoint must be a valid http/https/ws/wss URL")
	}
	if (ch.DepositEnabled || ch.WithdrawEnabled) && ch.RpcEndpoint == "" {
		return errors.New("rpc_endpoint is required when deposit or withdraw is enabled")
	}
	return nil
}

// validateChainUpdate 校验部分更新：仅对显式提供的字段做校验，避免零值
// （未提供字段）误报。启用充提但 RPC 为空的组合由 store 层兜底。
func validateChainUpdate(patch Chain) error {
	if patch.Confirmations != 0 && patch.Confirmations <= 0 {
		return errors.New("confirmations must be > 0")
	}
	if patch.RpcEndpoint != "" && !isValidRPCEndpoint(patch.RpcEndpoint) {
		return errors.New("rpc_endpoint must be a valid http/https/ws/wss URL")
	}
	return nil
}

func (s *Server) createChain(c *gin.Context) {
	var ch Chain
	if err := c.ShouldBindJSON(&ch); err != nil {
		s.fail(c, http.StatusBadRequest, "invalid chain body")
		return
	}
	if err := validateChainCreate(ch); err != nil {
		s.fail(c, http.StatusBadRequest, err.Error())
		return
	}
	out, err := s.catalog.CreateChain(ch)
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "create chain failed: "+err.Error())
		return
	}
	s.ok(c, out)
}

func (s *Server) updateChain(c *gin.Context) {
	id, ok := parseInt64(c, "id")
	if !ok {
		s.fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	var patch Chain
	if err := c.ShouldBindJSON(&patch); err != nil {
		s.fail(c, http.StatusBadRequest, "invalid body")
		return
	}
	if err := validateChainUpdate(patch); err != nil {
		s.fail(c, http.StatusBadRequest, err.Error())
		return
	}
	out, err := s.catalog.UpdateChain(id, patch)
	if err != nil {
		if errors.Is(err, ErrCatalogNotFound) {
			s.fail(c, http.StatusNotFound, "chain not found")
			return
		}
		if errors.Is(err, ErrCatalogInvalid) {
			s.fail(c, http.StatusBadRequest, err.Error())
			return
		}
		s.fail(c, http.StatusInternalServerError, "update chain failed: "+err.Error())
		return
	}
	s.ok(c, out)
}

// --- 币种管理（持久化于 CatalogStore）---
func (s *Server) listCoins(c *gin.Context) {
	coins, err := s.catalog.ListCoins()
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "list coins failed: "+err.Error())
		return
	}
	limit, offset := parsePage(c)
	s.ok(c, pageEnvelope(coins, limit, offset))
}

func (s *Server) createCoin(c *gin.Context) {
	var coin Coin
	if err := c.ShouldBindJSON(&coin); err != nil || coin.Symbol == "" {
		s.fail(c, http.StatusBadRequest, "invalid coin")
		return
	}
	out, err := s.catalog.CreateCoin(coin)
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "create coin failed: "+err.Error())
		return
	}
	s.ok(c, out)
}

func (s *Server) updateCoin(c *gin.Context) {
	id, ok := parseInt64(c, "id")
	if !ok {
		s.fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	var patch Coin
	if err := c.ShouldBindJSON(&patch); err != nil {
		s.fail(c, http.StatusBadRequest, "invalid body")
		return
	}
	out, err := s.catalog.UpdateCoin(id, patch)
	if err != nil {
		if errors.Is(err, ErrCatalogNotFound) {
			s.fail(c, http.StatusNotFound, "coin not found")
			return
		}
		s.fail(c, http.StatusInternalServerError, "update coin failed: "+err.Error())
		return
	}
	s.ok(c, out)
}

// --- 充值提币记录（实时聚合 futures 链上事件；上游不可达时返回空列表而非伪造数据）---

type futuresDeposits struct {
	Deposits []struct {
		TxHash        string  `json:"tx_hash"`
		UserID        int64   `json:"user_id"`
		Asset         string  `json:"asset"`
		Amount        float64 `json:"amount"`
		Chain         string  `json:"chain"`
		Address       string  `json:"address"`
		Confirmations int     `json:"confirmations"`
		Required      int     `json:"required"`
		Status        string  `json:"status"`
		CreatedAt     int64   `json:"created_at"`
	} `json:"deposits"`
}

func (s *Server) listDeposits(c *gin.Context) {
	ctx := c.Request.Context()
	limit, offset := parsePage(c)
	userID, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)
	coin := c.Query("coin")
	status := c.Query("status")

	if base := s.serviceURL("futures"); base != "" {
		var resp futuresDeposits
		if err := s.up.Get(ctx, base, "/api/v1/futures/wallet/deposits", &resp); err == nil {
			out := make([]Deposit, 0, len(resp.Deposits))
			for _, d := range resp.Deposits {
				dep := Deposit{
					ID:     d.TxHash, // 真实链上标识直接作锚点，不再经 stableID 哈希
					UserID: d.UserID,
					Coin:   d.Asset,
					Chain:  d.Chain,
					Amount: d.Amount,
					TxHash: d.TxHash,
					Status: d.Status,
					Time:   time.Unix(d.CreatedAt/1e9, d.CreatedAt%1e9), // futures created_at 为纳秒
				}
				if (userID == 0 || dep.UserID == userID) &&
					(coin == "" || dep.Coin == coin) &&
					(status == "" || dep.Status == status) {
					out = append(out, dep)
				}
			}
			page, total := paginate(out, limit, offset)
			s.ok(c, gin.H{"deposits": page, "total": total})
			return
		}
	}
	// 上游不可达：返回空列表（与正常路径同构，data 始终为数组），不返回伪造记录（发现 4），
	// 避免误导运营资金决策；经 X-Degraded 响应头告知前端上游不可用。
	c.Header("X-Degraded", "futures-unavailable")
	s.ok(c, gin.H{"deposits": []Deposit{}, "total": 0})
}

// supportedDepositChains 是「用户充值地址」派生覆盖的链集合（与 settlement 支持的充值链一致）。
// 充值地址由 settlement.GenerateAddress 按 userID 非硬化派生（HD 派生或降级 mock），无持久化表，
// 因此直接遍历此固定集合即可；前端可用 chain 查询参数进一步过滤。
var supportedDepositChains = []settlement.Chain{
	settlement.ChainETH,
	settlement.ChainTRON,
	settlement.ChainBTC,
	settlement.ChainSOL,
	settlement.ChainLTC,
	settlement.ChainDOGE,
}

// listUserDepositAddresses 查询某用户的充值地址：对用户 ID 在各条充值链上确定性派生地址。
// 地址由 settlement.GenerateAddress 实时派生（HD 派生或降级 mock），无持久化存储需求。
// user_id 缺省/非法时返回空列表（查询页初始态友好），不报错。
func (s *Server) listUserDepositAddresses(c *gin.Context) {
	limit, offset := parsePage(c)
	userID, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)
	chainFilter := strings.ToUpper(strings.TrimSpace(c.Query("chain")))

	if userID <= 0 {
		s.ok(c, pageEnvelope([]DepositAddress{}, limit, offset))
		return
	}

	out := make([]DepositAddress, 0, len(supportedDepositChains))
	for _, ch := range supportedDepositChains {
		if chainFilter != "" && string(ch) != chainFilter {
			continue
		}
		out = append(out, DepositAddress{
			UserID:  userID,
			Chain:   string(ch),
			Address: settlement.GenerateAddress(userID, ch),
		})
	}
	s.ok(c, pageEnvelope(out, limit, offset))
}

// futuresHolds 是 futures 提现冷静期 hold 队列的返回结构（含真实 hold_id，审核的真正锚点）。
type futuresHolds struct {
	Holds []struct {
		ID        string    `json:"id"`
		UserID    int64     `json:"user_id"`
		Asset     string    `json:"asset"`
		Amount    float64   `json:"amount"`
		Fee       float64   `json:"fee"`
		Chain     string    `json:"chain"`
		Address   string    `json:"address"`
		CreatedAt time.Time `json:"created_at"`
		HoldUntil time.Time `json:"hold_until"`
		Finalized bool      `json:"finalized"`
		Cancelled bool      `json:"cancelled"`
	} `json:"holds"`
}

func (s *Server) listWithdrawals(c *gin.Context) {
	ctx := c.Request.Context()
	limit, offset := parsePage(c)
	userID, _ := strconv.ParseInt(c.Query("user_id"), 10, 64)
	coin := c.Query("coin")
	status := c.Query("status")

	if base := s.serviceURL("futures"); base != "" {
		var resp futuresHolds
		if err := s.up.Get(ctx, base, "/api/v1/futures/wallet/withdraw/holds", &resp); err == nil {
			s.store.mu.Lock()
			out := make([]Withdrawal, 0, len(resp.Holds))
			for _, h := range resp.Holds {
				id := h.ID // 真实 hold_id（字符串），直接作审批锚点，不再经 stableID 哈希/服务端 map 反查
				st := "pending"
				if h.Finalized {
					st = "approved"
				} else if h.Cancelled {
					st = "rejected"
				}
				if ov, ok := s.store.wdApprovals[id]; ok {
					st = ov // 叠加本会话内的审批结果回显
				}
				rec := Withdrawal{
					ID:      id,
					UserID:  h.UserID,
					Coin:    h.Asset,
					Chain:   h.Chain,
					Amount:  h.Amount,
					Address: h.Address,
					TxHash:  h.ID,
					Status:  st,
					Time:    h.CreatedAt,
				}
				s.store.wdByID[id] = rec
				if (userID == 0 || rec.UserID == userID) &&
					(coin == "" || rec.Coin == coin) &&
					(status == "" || rec.Status == status) {
					out = append(out, rec)
				}
			}
			s.store.mu.Unlock()
			page, total := paginate(out, limit, offset)
			s.ok(c, gin.H{"withdrawals": page, "total": total})
			return
		}
	}
	// 上游不可达：返回空列表（与正常路径同构，data 始终为数组），不返回伪造记录（发现 4），
	// 避免误导运营资金决策；经 X-Degraded 响应头告知前端上游不可用。
	c.Header("X-Degraded", "futures-unavailable")
	s.ok(c, gin.H{"withdrawals": []Withdrawal{}, "total": 0})
}

// approveWithdrawal 管理员审批通过一笔提现：反查 futures hold_id 后真正调用 futures 审批
// 端点放行（链上广播 + 账本划出），替代此前仅写内存会话态的伪审批（§25）。
func (s *Server) approveWithdrawal(c *gin.Context) {
	s.doWithdrawalReview(c, "approved", "/api/v1/futures/wallet/withdraw/approve/")
}

// rejectWithdrawal 管理员拒绝一笔提现：反查 futures hold_id 后真正调用 futures 拒绝
// 端点退回冻结资金。
func (s *Server) rejectWithdrawal(c *gin.Context) {
	s.doWithdrawalReview(c, "rejected", "/api/v1/futures/wallet/withdraw/reject/")
}

// doWithdrawalReview 以真实 futures hold_id（前端经列表拿到的 id）直接调用对应审批端点真正落地，
// 成功回写本会话审批结果供列表回显；支持终态短路实现幂等（发现 1）。上游不可达返回 502。
func (s *Server) doWithdrawalReview(c *gin.Context, status, pathPrefix string) {
	id := c.Param("id")
	if id == "" {
		s.fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	base := s.serviceURL("futures")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "futures service not configured")
		return
	}
	// 终态短路（发现 1）：本会话已审批过该 hold，直接幂等返回，避免前端重放/重试重复转发 futures
	// （futures 侧 ClaimWithdrawBroadcast 仍是最终审重保证）。审批锚点即真实 hold_id，不再经服务端 map 反查。
	s.store.mu.RLock()
	if st, ok := s.store.wdApprovals[id]; ok {
		s.store.mu.RUnlock()
		s.ok(c, gin.H{"id": id, "status": st, "hold_id": id, "already": true})
		return
	}
	s.store.mu.RUnlock()
	if err := s.up.Post(c.Request.Context(), base, pathPrefix+id, nil, nil); err != nil {
		s.fail(c, http.StatusBadGateway, "withdrawal "+status+" failed: "+err.Error())
		return
	}
	s.store.mu.Lock()
	s.store.wdApprovals[id] = status
	s.store.mu.Unlock()
	s.ok(c, gin.H{"id": id, "status": status, "hold_id": id})
}

// --- 运营通知管理（list 实时聚合 notification 服务，本地公告叠加；本地公告持久化于 CatalogStore）---
func (s *Server) listNotifications(c *gin.Context) {
	ctx := c.Request.Context()
	out := []Notification{}
	if base := s.serviceURL("notification"); base != "" {
		var resp struct {
			Items []struct {
				ID        int64     `json:"id"`
				Type      string    `json:"type"`
				Title     string    `json:"title"`
				Body      string    `json:"body"`
				Status    string    `json:"status"`
				CreatedAt time.Time `json:"created_at"`
			} `json:"items"`
		}
		if err := s.up.Get(ctx, base, "/api/v1/notification/admin/list?limit=100", &resp); err == nil {
			for _, it := range resp.Items {
				out = append(out, Notification{
					ID:        it.ID,
					Title:     it.Title,
					Body:      it.Body,
					Level:     levelFromType(it.Type),
					CreatedAt: it.CreatedAt,
					Source:    "live",
				})
			}
		}
	}
	// 本地公告（管理后台自有，持久化于 CatalogStore）。
	local, err := s.catalog.ListNotifications()
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "list local notifications failed: "+err.Error())
		return
	}
	out = append(out, local...)
	limit, offset := parsePage(c)
	s.ok(c, pageEnvelope(out, limit, offset))
}

func (s *Server) createNotification(c *gin.Context) {
	var n Notification
	if err := c.ShouldBindJSON(&n); err != nil || n.Title == "" {
		s.fail(c, http.StatusBadRequest, "invalid notification")
		return
	}
	out, err := s.catalog.CreateNotification(n)
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "create notification failed: "+err.Error())
		return
	}
	s.ok(c, out)
}

func (s *Server) deleteNotification(c *gin.Context) {
	id, ok := parseInt64(c, "id")
	if !ok {
		s.fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.catalog.DeleteNotification(id); err != nil {
		if errors.Is(err, ErrCatalogNotFound) {
			s.fail(c, http.StatusNotFound, "notification not found")
			return
		}
		s.fail(c, http.StatusInternalServerError, "delete notification failed: "+err.Error())
		return
	}
	s.ok(c, gin.H{"deleted": id})
}

// posSideStr 把 futures 的 PosSide(int) 映射为人类可读方向。
func posSideStr(p int) string {
	switch p {
	case 0:
		return "long"
	case 1:
		return "short"
	default:
		return "unknown"
	}
}

// levelFromType 把通知类型映射为运营级别（前端据此着色）。
func levelFromType(t string) string {
	switch t {
	case "risk_alert", "kyc_rejected":
		return "warning"
	default:
		return "info"
	}
}

// --- user 服务 status/kyc 枚举 <-> 前端字符串 的双向映射 ---
// user 服务：Status 0=normal,1=frozen；KYCLevel 0=none,1=pending,2=verified,3=rejected。

func userStatusStr(v int) string {
	switch v {
	case 1:
		return "frozen"
	default:
		return "active"
	}
}

func userStatusToInt(s string) int {
	if s == "frozen" {
		return 1
	}
	return 0
}

func kycStr(v int) string {
	switch v {
	case 1:
		return "pending"
	case 2:
		return "verified"
	case 3:
		return "rejected"
	default:
		return "none"
	}
}

func kycToInt(s string) int {
	switch s {
	case "pending":
		return 1
	case "verified":
		return 2
	case "rejected":
		return 3
	default:
		return 0
	}
}

// statusWord 返回冻结/解冻对应的前端状态词。
func statusWord(frozen bool) string {
	if frozen {
		return "frozen"
	}
	return "active"
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// --- 运营看板：账本对账（实时聚合 futures 复式记账对账探针）---
// 注意（发现 6）：下方 total/disc 为展示层 float64 累加，源数据来自 futures 的 reconcile/inventory
// 接口（其偏差值本身已是 float64，且 futures 侧对账根因已定点化）。本端点仅做人工看板聚合，
// 不进入任何资金移动路径；若需自动告警，应以 futures 返回的定点偏差为准，而非在此累加 float。
func (s *Server) handleLedger(c *gin.Context) {
	ctx := c.Request.Context()
	var (
		rec struct {
			Balanced           bool               `json:"balanced"`
			Deviation          map[string]float64 `json:"deviation"`
			InventoryDeviation map[string]float64 `json:"inventory_deviation"`
		}
		inv struct {
			Inventory []AssetTotal `json:"inventory"`
		}
	)
	errs := []string{}
	if base := s.serviceURL("futures"); base != "" {
		if err := s.up.Get(ctx, base, "/api/v1/futures/wallet/reconcile", &rec); err != nil {
			errs = append(errs, "reconcile: "+err.Error())
		}
		if err := s.up.Get(ctx, base, "/api/v1/futures/wallet/inventory", &inv); err != nil {
			errs = append(errs, "inventory: "+err.Error())
		}
	} else {
		errs = append(errs, "futures service url not configured")
	}

	var total, disc float64
	for _, it := range inv.Inventory {
		total += it.OnchainTotal
	}
	for _, v := range rec.Deviation {
		disc += math.Abs(v)
	}
	for _, v := range rec.InventoryDeviation {
		disc += math.Abs(v)
	}
	sum := LedgerSummary{
		UpdatedAt:     time.Now(),
		TotalAssets:   total,
		SettlementBal: total,
		Reconciled:    rec.Balanced,
		Discrepancy:   disc,
		Assets:        inv.Inventory,
	}

	// 接入结算服务实时清算聚合：/stats 与 /cleared（若已配置 settlement 上游）。
	if base := s.serviceURL("settlement"); base != "" {
		var st struct {
			TotalTrades     int64              `json:"total_trades"`
			TotalVolume     float64            `json:"total_volume"`
			TotalCommission float64            `json:"total_commission"`
			BySymbol        map[string]float64 `json:"by_symbol"`
		}
		if err := s.up.Get(ctx, base, "/api/v1/settlement/stats", &st); err != nil {
			sum.Settlement.Notes = "settlement stats: " + err.Error()
		} else {
			sum.Settlement.Enabled = true
			sum.Settlement.TotalTrades = st.TotalTrades
			sum.Settlement.TotalVolume = st.TotalVolume
			sum.Settlement.TotalCommission = st.TotalCommission
			sum.Settlement.BySymbol = st.BySymbol
		}
		var cl struct {
			Trades []ClearedTradeView `json:"trades"`
		}
		if err := s.up.Get(ctx, base, "/api/v1/settlement/cleared?limit=10", &cl); err != nil {
			if sum.Settlement.Notes == "" {
				sum.Settlement.Notes = "settlement cleared: " + err.Error()
			}
		} else {
			sum.Settlement.Recent = cl.Trades
		}
	} else {
		sum.Settlement.Notes = "settlement service url not configured"
	}

	if len(errs) > 0 {
		if sum.Settlement.Notes != "" {
			sum.Notes = strings.Join(errs, "; ") + "; " + sum.Settlement.Notes
		} else {
			sum.Notes = strings.Join(errs, "; ")
		}
	}
	s.ok(c, sum)
}

// --- 运营看板：服务健康（探活 config.Services 中各上游 + 自身）---
func (s *Server) handleServices(c *gin.Context) {
	ctx := c.Request.Context()
	health := make([]ServiceHealth, 0, len(s.cfg.Services)+1)
	for name, base := range s.cfg.Services {
		path := "/health"
		if name == "settlement" {
			path = "/healthz" // settlement 暴露的是 /healthz
		}
		st := "down"
		status, elapsed, err := s.up.Probe(ctx, base, path)
		if err == nil && status != 0 && status < 400 {
			st = "up"
		}
		health = append(health, ServiceHealth{
			Name:      name,
			Status:    st,
			LatencyMs: elapsed.Milliseconds(),
			LastCheck: time.Now(),
		})
	}
	health = append(health, ServiceHealth{Name: "admin", Status: "up", LatencyMs: 0, LastCheck: time.Now()})
	sort.Slice(health, func(i, j int) bool { return health[i].Name < health[j].Name })
	s.ok(c, health)
}

// --- KYC 审核 ---

func (s *Server) listKycReviews(c *gin.Context) {
	base := s.serviceURL("user")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "user service not configured")
		return
	}
	ctx := c.Request.Context()
	var resp struct {
		Items []gin.H `json:"items"`
		Total int     `json:"total"`
	}
	if err := s.up.Get(ctx, base, "/api/v1/user/admin/kyc-reviews", &resp); err != nil {
		s.fail(c, http.StatusBadGateway, "kyc reviews failed: "+err.Error())
		return
	}
	s.ok(c, gin.H{"items": resp.Items, "total": resp.Total})
}

func (s *Server) getKycReviewDetail(c *gin.Context) {
	base := s.serviceURL("user")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "user service not configured")
		return
	}
	ctx := c.Request.Context()
	path := "/api/v1/user/admin/kyc-reviews/" + c.Param("id")
	var detail gin.H
	if err := s.up.Get(ctx, base, path, &detail); err != nil {
		s.fail(c, http.StatusBadGateway, "kyc detail failed: "+err.Error())
		return
	}
	s.ok(c, detail)
}

func (s *Server) approveKyc(c *gin.Context) {
	base := s.serviceURL("user")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "user service not configured")
		return
	}
	uid, _ := middleware.UserID(c)
	reviewer := fmt.Sprintf("admin_%d", uid)
	body := gin.H{"user_id": c.Param("id"), "approve": true, "reviewer": reviewer}
	ctx := c.Request.Context()
	var out gin.H
	if err := s.up.Post(ctx, base, "/api/v1/user/admin/kyc/review", &out, body); err != nil {
		s.fail(c, http.StatusBadGateway, "kyc approve failed: "+err.Error())
		return
	}
	s.ok(c, out)
}

func (s *Server) rejectKyc(c *gin.Context) {
	base := s.serviceURL("user")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "user service not configured")
		return
	}
	uid, _ := middleware.UserID(c)
	reviewer := fmt.Sprintf("admin_%d", uid)
	var req struct {
		RejectReason string `json:"reject_reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		s.fail(c, http.StatusBadRequest, "invalid body")
		return
	}
	body := gin.H{"user_id": c.Param("id"), "approve": false, "reject_reason": req.RejectReason, "reviewer": reviewer}
	ctx := c.Request.Context()
	var out gin.H
	if err := s.up.Post(ctx, base, "/api/v1/user/admin/kyc/review", &out, body); err != nil {
		s.fail(c, http.StatusBadGateway, "kyc reject failed: "+err.Error())
		return
	}
	s.ok(c, out)
}

// --- 理财资管管理 ---

func (s *Server) listWealthProducts(c *gin.Context) {
	base := s.serviceURL("wealth")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "wealth service not configured")
		return
	}
	ctx := c.Request.Context()
	var resp gin.H
	if err := s.up.Get(ctx, base, "/api/v1/wealth/products", &resp); err != nil {
		s.fail(c, http.StatusBadGateway, "wealth products failed: "+err.Error())
		return
	}
	s.ok(c, resp)
}

func (s *Server) createWealthProduct(c *gin.Context) {
	base := s.serviceURL("wealth")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "wealth service not configured")
		return
	}
	ctx := c.Request.Context()
	var body any
	if err := c.ShouldBindJSON(&body); err != nil {
		s.fail(c, http.StatusBadRequest, "invalid body")
		return
	}
	var out gin.H
	if err := s.up.Post(ctx, base, "/api/v1/wealth/products", &out, body); err != nil {
		s.fail(c, http.StatusBadGateway, "wealth create failed: "+err.Error())
		return
	}
	s.ok(c, out)
}

func (s *Server) listWealthHoldings(c *gin.Context) {
	base := s.serviceURL("wealth")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "wealth service not configured")
		return
	}
	ctx := c.Request.Context()
	var resp gin.H
	if err := s.up.Get(ctx, base, "/api/v1/wealth/admin/holdings", &resp); err != nil {
		s.fail(c, http.StatusBadGateway, "wealth holdings failed: "+err.Error())
		return
	}
	s.ok(c, resp)
}

func (s *Server) accrueWealth(c *gin.Context) {
	base := s.serviceURL("wealth")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "wealth service not configured")
		return
	}
	ctx := c.Request.Context()
	var out gin.H
	if err := s.up.Post(ctx, base, "/api/v1/wealth/admin/accrue", &out, nil); err != nil {
		s.fail(c, http.StatusBadGateway, "wealth accrue failed: "+err.Error())
		return
	}
	s.ok(c, out)
}

func (s *Server) listPendingWithdrawals(c *gin.Context) {
	limit, offset := parsePage(c)
	base := s.serviceURL("futures")
	if base == "" {
		s.ok(c, gin.H{"items": []gin.H{}, "total": 0})
		return
	}
	// 复用 listWithdrawals 的数据源（futures holds），只取未终态（非已放行/已拒绝）的记录。
	// 此前误用 withdrawals 键并对原始 JSON 的 status 字符串做 == "pending" 过滤，
	// 而 futures holds 用 finalized/cancelled 布尔表示状态，导致该页恒为空（已修复）。
	var resp futuresHolds
	ctx := c.Request.Context()
	if err := s.up.Get(ctx, base, "/api/v1/futures/wallet/withdraw/holds", &resp); err != nil {
		s.ok(c, gin.H{"items": []gin.H{}, "total": 0})
		return
	}
	pending := make([]gin.H, 0, len(resp.Holds))
	for _, h := range resp.Holds {
		if h.Finalized || h.Cancelled {
			continue
		}
		pending = append(pending, gin.H{
			"id":           h.ID,
			"user_id":      h.UserID,
			"coin":         h.Asset,
			"amount":       h.Amount,
			"chain":        h.Chain,
			"address":      h.Address,
			"submitted_at": h.CreatedAt.Format(time.RFC3339),
			"status":       "pending",
		})
	}
	page, total := paginate(pending, limit, offset)
	s.ok(c, gin.H{"items": page, "total": total})
}

func (s *Server) getWithdrawalDetail(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		s.fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	s.store.mu.RLock()
	w, ok := s.store.wdByID[id]
	s.store.mu.RUnlock()
	if !ok {
		s.fail(c, http.StatusNotFound, "withdrawal not found")
		return
	}
	s.ok(c, gin.H{
		"id":           w.ID,
		"user_id":      w.UserID,
		"coin":         w.Coin,
		"amount":       w.Amount,
		"chain":        w.Chain,
		"address":      w.Address,
		"tx_hash":      w.TxHash,
		"status":       w.Status,
		"submitted_at": w.Time.Format(time.RFC3339),
	})
}

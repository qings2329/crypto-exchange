package adminapi

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/coldlar/crypto-exchange/internal/pkg/response"
	"github.com/gin-gonic/gin"
)

func (s *Server) ok(c *gin.Context, data interface{}) {
	response.JSON(c, data)
}

func (s *Server) fail(c *gin.Context, code int, msg string) {
	c.JSON(code, gin.H{"code": code, "message": msg})
}

func parseInt64(c *gin.Context, param string) (int64, bool) {
	v, err := strconv.ParseInt(c.Param(param), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// --- 登录（无 guard）：校验管理员账户（状态/密码/可选 TOTP）后签发带权限的 token ---
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
	acc, err := s.adminStore.GetAccountByUsername(req.Username)
	if err != nil || acc.Status != AdminStatusActive {
		s.fail(c, http.StatusUnauthorized, "invalid admin credentials")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(acc.PasswordHash), []byte(req.Password)) != nil {
		s.fail(c, http.StatusUnauthorized, "invalid admin credentials")
		return
	}
	if acc.TOTPEnabled {
		if !VerifyTOTP(acc.TOTPSecret, req.TOTP, time.Now()) {
			s.fail(c, http.StatusUnauthorized, "invalid totp code")
			return
		}
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
	s.ok(c, gin.H{
		"token":         tok,
		"expires_in":    s.cfg.Admin.TokenTTLSec,
		"totp_required": acc.TOTPEnabled,
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
		CreatedAt time.Time `json:"created_at"`
	} `json:"users"`
}

// listUsers 代理 user 服务 /api/v1/user/admin/list；上游不可达时降级为内存示例数据。
func (s *Server) listUsers(c *gin.Context) {
	ctx := c.Request.Context()
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
					CreatedAt: u.CreatedAt,
				}
				s.enrichBalance(ctx, &au) // 接 futures 钱包余额（§25 后续：消除恒为 0）
				out = append(out, au)
			}
			s.ok(c, out)
			return
		}
	}
	// 上游不可达：降级为内存示例数据，保证 UI 仍可渲染。
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	s.ok(c, s.store.users)
}

// enrichBalance 从 futures 钱包服务拉取该用户的 USDT 可用余额填入 AdminUser.Balance，
// 消除用户列表中余额恒为 0 的问题（§19 已知缺口）。上游不可达/用户无钱包时静默留 0（fail-degraded）。
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

// --- 交易对/参数配置（持久化于 CatalogStore：MySQL 优先，失败回退内存）---
func (s *Server) listSymbols(c *gin.Context) {
	syms, err := s.catalog.ListSymbols()
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "list symbols failed: "+err.Error())
		return
	}
	s.ok(c, syms)
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
	s.ok(c, chs)
}

func (s *Server) createChain(c *gin.Context) {
	var ch Chain
	if err := c.ShouldBindJSON(&ch); err != nil || ch.Name == "" {
		s.fail(c, http.StatusBadRequest, "invalid chain")
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
	out, err := s.catalog.UpdateChain(id, patch)
	if err != nil {
		if errors.Is(err, ErrCatalogNotFound) {
			s.fail(c, http.StatusNotFound, "chain not found")
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
	s.ok(c, coins)
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

// --- 充值提币记录（实时聚合 futures 链上事件；上游不可达时降级为内存示例）---

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
	if base := s.serviceURL("futures"); base != "" {
		var resp futuresDeposits
		if err := s.up.Get(ctx, base, "/api/v1/futures/wallet/deposits", &resp); err == nil {
			out := make([]Deposit, 0, len(resp.Deposits))
			for _, d := range resp.Deposits {
				out = append(out, Deposit{
					ID:     stableID(d.TxHash, strconv.FormatInt(d.UserID, 10), d.Asset, d.Chain),
					UserID: d.UserID,
					Coin:   d.Asset,
					Chain:  d.Chain,
					Amount: d.Amount,
					TxHash: d.TxHash,
					Status: d.Status,
					Time:   time.Unix(d.CreatedAt, 0),
				})
			}
			s.ok(c, out)
			return
		}
	}
	// 上游不可达：降级为内存示例数据。
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	s.ok(c, s.store.deposits)
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
	if base := s.serviceURL("futures"); base != "" {
		var resp futuresHolds
		if err := s.up.Get(ctx, base, "/api/v1/futures/wallet/withdraw/holds", &resp); err == nil {
			s.store.mu.Lock()
			s.store.wdByHoldID = make(map[int64]string, len(resp.Holds))
			out := make([]Withdrawal, 0, len(resp.Holds))
			for _, h := range resp.Holds {
				id := stableID(h.ID, strconv.FormatInt(h.UserID, 10), h.Asset, h.Address)
				status := "pending"
				if h.Finalized {
					status = "approved"
				} else if h.Cancelled {
					status = "rejected"
				}
				if ov, ok := s.store.wdApprovals[id]; ok {
					status = ov // 叠加本会话内的审批结果回显
				}
				rec := Withdrawal{
					ID:      id,
					UserID:  h.UserID,
					Coin:    h.Asset,
					Chain:   h.Chain,
					Amount:  h.Amount,
					Address: h.Address,
					TxHash:  h.ID,
					Status:  status,
					Time:    h.CreatedAt,
				}
				s.store.wdByID[id] = rec
				s.store.wdByHoldID[id] = h.ID
				out = append(out, rec)
			}
			s.store.mu.Unlock()
			s.ok(c, out)
			return
		}
	}
	// 上游不可达：降级为内存示例数据。
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	s.ok(c, s.store.withdrawals)
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

// doWithdrawalReview 反查前端 stableID 对应的 futures hold_id，调用对应审批端点真正落地，
// 成功回写本会话审批结果供列表回显；上游不可达或 hold 未找到返回 502/404。
func (s *Server) doWithdrawalReview(c *gin.Context, status, pathPrefix string) {
	id, ok := parseInt64(c, "id")
	if !ok {
		s.fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	base := s.serviceURL("futures")
	if base == "" {
		s.fail(c, http.StatusBadGateway, "futures service not configured")
		return
	}
	s.store.mu.RLock()
	holdID, found := s.store.wdByHoldID[id]
	s.store.mu.RUnlock()
	if !found {
		s.fail(c, http.StatusNotFound, "withdrawal not found (list withdrawals first)")
		return
	}
	if err := s.up.Post(c.Request.Context(), base, pathPrefix+holdID, nil, nil); err != nil {
		s.fail(c, http.StatusBadGateway, "withdrawal "+status+" failed: "+err.Error())
		return
	}
	s.store.mu.Lock()
	s.store.wdApprovals[id] = status
	s.store.mu.Unlock()
	s.ok(c, gin.H{"id": id, "status": status, "hold_id": holdID})
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
	s.ok(c, out)
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

// stableID 由若干字符串拼接做 FNV-64a 哈希，得到稳定且非负 int64 的数字 id，
// 供前端以 row.id 做审批（链上事件仅有 TxHash，无 int 型 id）。
func stableID(parts ...string) int64 {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return int64(h.Sum64())
}

// --- 运营看板：账本对账（实时聚合 futures 复式记账对账探针）---
func (s *Server) handleLedger(c *gin.Context) {
	ctx := c.Request.Context()
	var (
		rec struct {
			Balanced           bool               `json:"balanced"`
			Deviation          map[string]float64 `json:"deviation"`
			InventoryDeviation map[string]float64 `json:"inventory_deviation"`
		}
		inv struct {
			Inventory []struct {
				Asset        string  `json:"asset"`
				OnchainTotal float64 `json:"onchain_total"`
			} `json:"inventory"`
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
	}
	if len(errs) > 0 {
		sum.Notes = strings.Join(errs, "; ")
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

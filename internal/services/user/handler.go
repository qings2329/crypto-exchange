package user

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// Handler 暴露用户 HTTP 接口。
type Handler struct {
	svc      *Service
	verifier *middleware.TokenVerifier
}

// NewHandler 构造 handler。
func NewHandler(svc *Service, verifier *middleware.TokenVerifier) *Handler {
	return &Handler{svc: svc, verifier: verifier}
}

// Register 路由。
func (h *Handler) Register(r *gin.Engine) {
	// 健康检查（免鉴权，供管理后台 / 网关 / 探针探活）。
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "time": time.Now().Unix()})
	})
	g := r.Group("/api/v1/user")
	// 公开路由
	g.POST("/register", h.register)
	g.POST("/send-code", h.sendCode)
	g.POST("/verify", h.verify)
	g.POST("/login", h.login)
	g.POST("/refresh", h.refresh)
	g.POST("/logout", h.logout)
	g.POST("/forgot", h.forgot)
	g.POST("/reset", h.reset)

	// 鉴权路由（复用 T-01 的 HMAC 中间件）
	auth := g.Group("")
	auth.Use(middleware.Auth(h.verifier))
	auth.GET("/me", h.me)
	auth.PUT("/me", h.updateProfile)
	auth.POST("/password", h.changePassword)
	auth.GET("/preferences", h.getPreferences)
	auth.PUT("/preferences", h.updatePreferences)
	auth.POST("/tfa/setup", h.tfaSetup)
	auth.POST("/tfa/enable", h.tfaEnable)
	auth.POST("/tfa/disable", h.tfaDisable)
	auth.POST("/kyc/submit", h.kycSubmit)
	auth.GET("/kyc", h.kycGet)
	auth.GET("/referrals", h.getReferrals)
	auth.GET("/referral-code", h.getReferralCode)

	// 安全中心（API Key / 登录历史 / 会话 / 防钓鱼码）。
	h.registerSecurityRoutes(auth)

	// 管理后台聚合接口（仅管理员；cmd/admin 以 admin token 代理调用）。
	// 此处再叠加 AdminGuard 作为纵深防御：即便上游网关漏配，普通用户也无法越权操作他人账户 / 审核 KYC。
	// adminapi 转发的是 role=admin 的 selfToken，故不影响既有管理后台流程。
	adminG := g.Group("")
	adminG.Use(middleware.Auth(h.verifier), middleware.AdminGuard())
	adminG.POST("/kyc/review", h.kycReview) // F4：审核他人 KYC 必须管理员
	adminG.GET("/admin/kyc-reviews", h.listPendingKycReviews)
	adminG.GET("/admin/kyc-reviews/:id", h.getKycReviewDetail)
	adminG.GET("/admin/list", h.adminList)
	adminG.POST("/admin", h.adminCreate)
	adminG.GET("/admin/:id", h.adminGet) // 风控/提现网关按 user_id 取 KYC 等级（仅管理员）
	adminG.PUT("/admin/:id", h.adminUpdate)
	adminG.POST("/admin/:id/freeze", h.adminFreeze)
	adminG.POST("/admin/:id/unfreeze", h.adminUnfreeze)
	adminG.POST("/admin/:id/tfa/reset", h.adminResetTFA)
}

func (h *Handler) register(c *gin.Context) {
	var req struct {
		Target       string `json:"target"`
		Password     string `json:"password"`
		Code         string `json:"code"`
		ReferralCode string `json:"referral_code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Target == "" || req.Password == "" || req.Code == "" {
		response.Error(c, http.StatusBadRequest, 400, "target, password and code required")
		return
	}
	id, err := h.svc.Register(req.Target, req.Password, req.Code, req.ReferralCode)
	if err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"user_id": id, "message": "registered"})
}

func (h *Handler) sendCode(c *gin.Context) {
	var req struct {
		Target  string `json:"target"`
		Purpose string `json:"purpose"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Target == "" || req.Purpose == "" {
		response.Error(c, http.StatusBadRequest, 400, "target and purpose required")
		return
	}
	if err := h.svc.SendCode(req.Target, req.Purpose); err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"message": "code sent"})
}

func (h *Handler) verify(c *gin.Context) {
	var req struct {
		Target  string `json:"target"`
		Code    string `json:"code"`
		Purpose string `json:"purpose"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Target == "" || req.Code == "" {
		response.Error(c, http.StatusBadRequest, 400, "target and code required")
		return
	}
	if req.Purpose == "" {
		req.Purpose = PurposeVerify
	}
	if err := h.svc.VerifyAccount(req.Target, req.Code, req.Purpose); err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"message": "verified"})
}

func (h *Handler) login(c *gin.Context) {
	var req struct {
		Target  string `json:"target"`
		Password string `json:"password"`
		TFACode string `json:"tfa_code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Target == "" || req.Password == "" {
		response.Error(c, http.StatusBadRequest, 400, "target and password required")
		return
	}
	res, err := h.svc.Login(req.Target, req.Password, req.TFACode)
	// 登录历史/会话登记：能归属到具体用户（含密码错误的既有账号）即记录。
	if res != nil {
		h.svc.RecordLoginWithMeta(req.Target, err, res.User.ID, c.ClientIP(), c.Request.UserAgent())
	} else if uerr := err; uerr != nil {
		// 失败场景下重新定位用户以归属历史（定位不到则跳过）。
		if uid, ok := h.svc.LookupUserID(req.Target); ok {
			h.svc.RecordLoginWithMeta(req.Target, uerr, uid, c.ClientIP(), c.Request.UserAgent())
		}
	}
	if err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{
		"access_token":  res.AccessToken,
		"refresh_token": res.RefreshToken,
		"expires_in":    res.ExpiresIn,
		"user_id":       res.User.ID,
		"kyc_level":     res.User.KYCLevel,
		"tfa_enabled":   res.User.TFAEnabled,
	})
}

func (h *Handler) refresh(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.RefreshToken == "" {
		response.Error(c, http.StatusBadRequest, 400, "refresh_token required")
		return
	}
	token, err := h.svc.Refresh(req.RefreshToken)
	if err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"access_token": token})
}

func (h *Handler) logout(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.RefreshToken == "" {
		response.Error(c, http.StatusBadRequest, 400, "refresh_token required")
		return
	}
	if err := h.svc.Logout(req.RefreshToken); err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"message": "logged out"})
}

func (h *Handler) forgot(c *gin.Context) {
	var req struct {
		Target string `json:"target"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Target == "" {
		response.Error(c, http.StatusBadRequest, 400, "target required")
		return
	}
	if err := h.svc.ForgotPassword(req.Target); err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"message": "if account exists, code sent"})
}

func (h *Handler) reset(c *gin.Context) {
	var req struct {
		Target      string `json:"target"`
		Code        string `json:"code"`
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Target == "" || req.Code == "" || req.NewPassword == "" {
		response.Error(c, http.StatusBadRequest, 400, "target, code and new_password required")
		return
	}
	if err := h.svc.ResetPassword(req.Target, req.Code, req.NewPassword); err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"message": "password reset"})
}

func (h *Handler) me(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, http.StatusUnauthorized, 401, "unauthorized")
		return
	}
	u, kyc, err := h.svc.GetProfile(uid)
	if err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{
		"user_id":        u.ID,
		"email":          u.Email,
		"phone":          u.Phone,
		"nickname":       u.Nickname,
		"avatar":         u.Avatar,
		"status":         u.Status,
		"kyc_level":      u.KYCLevel,
		"tfa_enabled":    u.TFAEnabled,
		"email_verified": u.EmailVerified,
		"phone_verified": u.PhoneVerified,
		"kyc":            kyc,
	})
}

func (h *Handler) tfaSetup(c *gin.Context) {
	uid, _ := middleware.UserID(c)
	secret, uri, err := h.svc.SetupTFA(uid)
	if err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"secret": secret, "otpauth_uri": uri, "message": "scan qr then enable with code"})
}

func (h *Handler) updateProfile(c *gin.Context) {
	uid, _ := middleware.UserID(c)
	var req struct {
		Nickname *string `json:"nickname"`
		Avatar   *string `json:"avatar"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, 400, "invalid body")
		return
	}
	if err := h.svc.UpdateProfile(uid, req.Nickname, req.Avatar); err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

func (h *Handler) changePassword(c *gin.Context) {
	uid, _ := middleware.UserID(c)
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.OldPassword == "" || req.NewPassword == "" {
		response.Error(c, http.StatusBadRequest, 400, "old_password and new_password required")
		return
	}
	if err := h.svc.ChangePassword(uid, req.OldPassword, req.NewPassword); err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"ok": true, "message": "password changed, please re-login"})
}

func (h *Handler) getPreferences(c *gin.Context) {
	uid, _ := middleware.UserID(c)
	p, err := h.svc.GetPreferences(uid)
	if err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, p)
}

func (h *Handler) updatePreferences(c *gin.Context) {
	uid, _ := middleware.UserID(c)
	var req UserPreferences
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, 400, "invalid body")
		return
	}
	if err := h.svc.UpdatePreferences(uid, &req); err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

func (h *Handler) tfaEnable(c *gin.Context) {
	uid, _ := middleware.UserID(c)
	var req struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Code == "" {
		response.Error(c, http.StatusBadRequest, 400, "code required")
		return
	}
	if err := h.svc.EnableTFA(uid, req.Code); err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"tfa_enabled": true})
}

func (h *Handler) tfaDisable(c *gin.Context) {
	uid, _ := middleware.UserID(c)
	var req struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Code == "" {
		response.Error(c, http.StatusBadRequest, 400, "code required")
		return
	}
	if err := h.svc.DisableTFA(uid, req.Code); err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"tfa_enabled": false})
}

func (h *Handler) kycSubmit(c *gin.Context) {
	uid, _ := middleware.UserID(c)
	var req KYCRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, 400, "bad request")
		return
	}
	if err := h.svc.SubmitKYC(uid, req); err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"kyc_level": int(KYCPending), "message": "kyc submitted"})
}

func (h *Handler) kycGet(c *gin.Context) {
	uid, _ := middleware.UserID(c)
	_, kyc, err := h.svc.GetProfile(uid)
	if err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"kyc": kyc})
}

func (h *Handler) kycReview(c *gin.Context) {
	var req struct {
		UserID       int64  `json:"user_id"`
		Approve      bool   `json:"approve"`
		RejectReason string `json:"reject_reason"`
		Reviewer     string `json:"reviewer"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID == 0 {
		response.Error(c, http.StatusBadRequest, 400, "user_id required")
		return
	}
	if err := h.svc.ReviewKYC(req.UserID, req.Approve, req.RejectReason, req.Reviewer); err != nil {
		fail(c, err)
		return
	}
	level := int(KYCVerified)
	if !req.Approve {
		level = int(KYCRejected)
	}
	response.JSON(c, gin.H{"kyc_level": level, "message": "review done"})
}

func (h *Handler) listPendingKycReviews(c *gin.Context) {
	kycs, err := h.svc.ListPendingKYC()
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]gin.H, 0, len(kycs))
	for _, k := range kycs {
		out = append(out, gin.H{
			"id":           k.UserID,
			"user_id":      k.UserID,
			"full_name":    k.RealName,
			"id_number":    k.IDNumber,
			"country":      "",
			"submitted_at": k.SubmittedAt.Format(time.RFC3339),
			"status":       int(k.Status),
		})
	}
	response.JSON(c, gin.H{"items": out, "total": len(out)})
}

func (h *Handler) getKycReviewDetail(c *gin.Context) {
	userIDStr := c.Param("id")
	userID, err := strconv.ParseInt(userIDStr, 10, 64)
	if err != nil || userID == 0 {
		fail(c, ErrNotFound)
		return
	}
	kyc, err := h.svc.GetKYCForAdmin(userID)
	if err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{
		"id":           kyc.UserID,
		"user_id":      kyc.UserID,
		"full_name":    kyc.RealName,
		"id_number":    kyc.IDNumber,
		"country":      "",
		"submitted_at": kyc.SubmittedAt.Format(time.RFC3339),
		"doc_front":    kyc.DocFront,
		"doc_back":     kyc.DocBack,
		"status":       int(kyc.Status),
		"reject_reason": kyc.RejectReason,
	})
}

// ---- 管理后台聚合接口 ----

func (h *Handler) adminList(c *gin.Context) {
	us, err := h.svc.ListAll()
	if err != nil {
		fail(c, err)
		return
	}
	out := make([]gin.H, 0, len(us))
	for _, u := range us {
		username := u.Email
		if username == "" {
			username = u.Phone
		}
		out = append(out, gin.H{
			"id":         u.ID,
			"username":   username,
			"email":      u.Email,
			"phone":      u.Phone,
			"status":     int(u.Status),
			"kyc_level":  int(u.KYCLevel),
			"level":      int(u.Level),
			"created_at": u.CreatedAt,
		})
	}
	response.JSON(c, gin.H{"users": out})
}

func (h *Handler) adminCreate(c *gin.Context) {
	var req struct {
		Target  string `json:"target"`
		Username string `json:"username"`
		Email   string `json:"email"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, 400, "invalid body")
		return
	}
	target := req.Target
	if target == "" {
		target = req.Email
	}
	if target == "" {
		target = req.Username
	}
	if target == "" || req.Password == "" {
		response.Error(c, http.StatusBadRequest, 400, "target and password required")
		return
	}
	id, err := h.svc.AdminCreate(target, req.Password)
	if err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"id": id, "message": "user created"})
}

func (h *Handler) adminUpdate(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, http.StatusBadRequest, 400, "invalid id")
		return
	}
	var req struct {
		Email     *string `json:"email"`
		Status    *int    `json:"status"`
		KYCLevel  *int    `json:"kyc_level"`
		Level     *int    `json:"level"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, 400, "invalid body")
		return
	}
	in := AdminUpdateInput{}
	if req.Email != nil {
		in.Email = req.Email
	}
	if req.Status != nil {
		st := Status(*req.Status)
		in.Status = &st
	}
	if req.KYCLevel != nil {
		kl := KYCLevel(*req.KYCLevel)
		in.KYCLevel = &kl
	}
	if req.Level != nil {
		lvl := int8(*req.Level)
		in.Level = &lvl
	}
	if err := h.svc.AdminUpdate(id, in); err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"status": "ok"})
}

// adminGet 按 user_id 返回用户档案（含 kyc_level），供风控/提现网关在不持有 user 服务的内部
// 上下文下按目标用户取 KYC 等级。仅管理员可调用（adminG 已叠加 AdminGuard）。
func (h *Handler) adminGet(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, http.StatusBadRequest, 400, "invalid id")
		return
	}
	u, _, err := h.svc.GetProfile(id)
	if err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{
		"id":         u.ID,
		"email":      u.Email,
		"phone":      u.Phone,
		"status":     int(u.Status),
		"kyc_level":  int(u.KYCLevel),
		"level":      int(u.Level),
		"created_at": u.CreatedAt,
	})
}

// adminResetTFA 强制重置某用户的 Google 验证码（关闭 2FA 并清空密钥），让用户重新绑定。
func (h *Handler) adminResetTFA(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, http.StatusBadRequest, 400, "invalid id")
		return
	}
	if err := h.svc.AdminResetTFA(id); err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"id": id, "tfa_enabled": false})
}

func (h *Handler) adminFreeze(c *gin.Context) {
	h.adminSetStatus(c, true)
}

func (h *Handler) adminUnfreeze(c *gin.Context) {
	h.adminSetStatus(c, false)
}

func (h *Handler) adminSetStatus(c *gin.Context, frozen bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, http.StatusBadRequest, 400, "invalid id")
		return
	}
	st := StatusNormal
	if frozen {
		st = StatusFrozen
	}
	if err := h.svc.SetStatus(id, st); err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"status": "ok", "frozen": frozen})
}

// fail 把业务错误映射为 HTTP 响应。
func fail(c *gin.Context, err error) {
	switch err {
	case ErrNotFound, ErrRefreshInvalid:
		response.Error(c, http.StatusUnauthorized, 401, err.Error())
	case ErrWrongPassword, ErrTFAFailed, ErrTFARequired:
		response.Error(c, http.StatusUnauthorized, 401, err.Error())
	case ErrUserExists, ErrInvalidCode, ErrCodeExpired, ErrCodeConsumed,
		ErrTFANotEnabled, ErrKYCPending, ErrKYCNotPending, ErrInvalidAccount, ErrFrozen,
		ErrSamePassword, ErrInvalidPref, ErrNicknameTooLong, ErrAvatarTooLong, ErrPasswordTooShort:
		response.Error(c, http.StatusBadRequest, 400, err.Error())
	default:
		response.Error(c, http.StatusInternalServerError, 500, err.Error())
	}
}

// ---- 邀请 ----

func (h *Handler) getReferralCode(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, http.StatusUnauthorized, 401, "unauthorized")
		return
	}
	user, _, err := h.svc.GetProfile(uid)
	if err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"referral_code": user.ReferralCode})
}

func (h *Handler) getReferrals(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, http.StatusUnauthorized, 401, "unauthorized")
		return
	}
	users, err := h.svc.GetReferrals(uid)
	if err != nil {
		fail(c, err)
		return
	}
	type refInfo struct {
		UserID    int64  `json:"user_id"`
		Nickname  string `json:"nickname"`
		Email     string `json:"email"`
		CreatedAt string `json:"created_at"`
	}
	out := make([]refInfo, 0, len(users))
	for _, u := range users {
		out = append(out, refInfo{
			UserID:    u.ID,
			Nickname:  u.Nickname,
			Email:     u.Email,
			CreatedAt: u.CreatedAt.Format(time.RFC3339),
		})
	}
	response.JSON(c, gin.H{"referrals": out, "total": len(out)})
}

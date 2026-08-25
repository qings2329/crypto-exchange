package user

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// 本文件是安全中心四组端点的 HTTP 层（API Key / 登录历史 / 会话 / 防钓鱼码），
// 响应形状对齐 crypto-exchange-web/src/api/client.ts 的 SecurityCenter 契约。

func (h *Handler) uid(c *gin.Context) (int64, bool) {
	return middleware.UserID(c)
}

// registerSecurityRoutes 在已鉴权分组下挂载安全中心路由。
func (h *Handler) registerSecurityRoutes(auth *gin.RouterGroup) {
	auth.GET("/api-keys", h.apiKeysList)
	auth.POST("/api-keys", h.apiKeysCreate)
	auth.PUT("/api-keys/:id", h.apiKeysUpdate)
	auth.DELETE("/api-keys/:id", h.apiKeysDelete)

	auth.GET("/login-history", h.loginHistory)

	auth.GET("/sessions", h.sessionsList)
	auth.DELETE("/sessions/:id", h.sessionRevoke)
	auth.DELETE("/sessions", h.sessionRevokeAll)

	auth.GET("/anti-phishing", h.antiPhishingGet)
	auth.POST("/anti-phishing", h.antiPhishingSet)
}

// apiKeyView 对外视图：剔除 SecretHash；时间序列化为 RFC3339。
type apiKeyView struct {
	ID          int64      `json:"id"`
	UserID      int64      `json:"user_id"`
	Label       string     `json:"label"`
	Key         string     `json:"key"`
	Permissions []string   `json:"permissions"`
	IPWhitelist []string   `json:"ip_whitelist"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
}

func toApiKeyView(k *ApiKey) apiKeyView {
	perms := k.Permissions
	if perms == nil {
		perms = []string{}
	}
	ips := k.IPWhitelist
	if ips == nil {
		ips = []string{}
	}
	return apiKeyView{
		ID:          k.ID,
		UserID:      k.UserID,
		Label:       k.Label,
		Key:         k.KeyPublic,
		Permissions: perms,
		IPWhitelist: ips,
		Status:      k.Status,
		CreatedAt:   k.CreatedAt,
		LastUsedAt:  k.LastUsedAt,
	}
}

// apiKeysList GET /api/v1/user/api-keys?limit=&offset=&status= → {api_keys:[...], total:n}
// limit/offset/status 兼容前端可选分页筛选；不传即全量。
func (h *Handler) apiKeysList(c *gin.Context) {
	uid, ok := h.uid(c)
	if !ok {
		response.Error(c, http.StatusUnauthorized, 401, "unauthorized")
		return
	}
	list, err := h.svc.ListApiKeys(uid)
	if err != nil {
		fail(c, err)
		return
	}
	statusFilter := c.Query("status")
	limit, _ := strconv.Atoi(c.Query("limit"))
	offset, _ := strconv.Atoi(c.Query("offset"))
	total := len(list)
	filtered := make([]apiKeyView, 0, total)
	for _, k := range list {
		if statusFilter != "" && k.Status != statusFilter {
			continue
		}
		filtered = append(filtered, toApiKeyView(k))
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(filtered) {
		filtered = filtered[:0]
	} else {
		filtered = filtered[offset:]
	}
	if limit > 0 && limit < len(filtered) {
		filtered = filtered[:limit]
	}
	response.JSON(c, gin.H{"api_keys": filtered, "total": total})
}

// apiKeysCreate POST /api/v1/user/api-keys {label, permissions, ip_whitelist}
// → 201 {api_key:{...}, secret:"..."}（secret 仅本次返回）。
func (h *Handler) apiKeysCreate(c *gin.Context) {
	uid, ok := h.uid(c)
	if !ok {
		response.Error(c, http.StatusUnauthorized, 401, "unauthorized")
		return
	}
	var req struct {
		Label       string   `json:"label"`
		Permissions []string `json:"permissions"`
		IPWhitelist []string `json:"ip_whitelist"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, 400, "invalid body")
		return
	}
	k, secret, err := h.svc.CreateApiKey(uid, req.Label, req.Permissions, req.IPWhitelist)
	if err != nil {
		switch err {
		case ErrKeyLabelRequired, ErrKeyPermRequired, ErrKeyPermInvalid:
			response.Error(c, http.StatusBadRequest, 400, err.Error())
		default:
			response.Error(c, http.StatusInternalServerError, 500, err.Error())
		}
		return
	}
	response.JSON(c, gin.H{"api_key": toApiKeyView(k), "secret": secret})
}

// apiKeysUpdate PUT /api/v1/user/api-keys/:id {status:"active"|"disabled"} → {ok:true}
func (h *Handler) apiKeysUpdate(c *gin.Context) {
	uid, ok := h.uid(c)
	if !ok {
		response.Error(c, http.StatusUnauthorized, 401, "unauthorized")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, http.StatusBadRequest, 400, "invalid id")
		return
	}
	var req struct {
		Status string `json:"status"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, 400, "invalid body")
		return
	}
	if err := h.svc.SetApiKeyStatus(uid, id, req.Status); err != nil {
		if err == ErrNotFound {
			response.Error(c, http.StatusNotFound, 404, "api key not found")
			return
		}
		response.Error(c, http.StatusBadRequest, 400, err.Error())
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

// apiKeysDelete DELETE /api/v1/user/api-keys/:id → {ok:true}
func (h *Handler) apiKeysDelete(c *gin.Context) {
	uid, ok := h.uid(c)
	if !ok {
		response.Error(c, http.StatusUnauthorized, 401, "unauthorized")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, http.StatusBadRequest, 400, "invalid id")
		return
	}
	if err := h.svc.DeleteApiKey(uid, id); err != nil {
		if err == ErrNotFound {
			response.Error(c, http.StatusNotFound, 404, "api key not found")
			return
		}
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

// loginEntryView 视图：id 以字符串形式返回（前端 LoginHistoryEntry.id: string）。
type loginEntryView struct {
	ID        string    `json:"id"`
	IP        string    `json:"ip"`
	UA        string    `json:"ua"`
	Location  string    `json:"location"`
	Success   bool      `json:"success"`
	CreatedAt time.Time `json:"created_at"`
}

// loginHistory GET /api/v1/user/login-history?limit= → {entries:[...]}
func (h *Handler) loginHistory(c *gin.Context) {
	uid, ok := h.uid(c)
	if !ok {
		response.Error(c, http.StatusUnauthorized, 401, "unauthorized")
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	list, err := h.svc.ListLoginHistory(uid, limit)
	if err != nil {
		fail(c, err)
		return
	}
	views := make([]loginEntryView, 0, len(list))
	for _, e := range list {
		views = append(views, loginEntryView{
			ID:        strconv.FormatInt(e.ID, 10),
			IP:        e.IP,
			UA:        e.UA,
			Location:  e.Location,
			Success:   e.Success,
			CreatedAt: e.CreatedAt,
		})
	}
	response.JSON(c, gin.H{"entries": views})
}

// sessionView 视图：current 由服务层推导。
type sessionView struct {
	ID           string    `json:"id"`
	IP           string    `json:"ip"`
	UA           string    `json:"ua"`
	Location     string    `json:"location"`
	Current      bool      `json:"current"`
	CreatedAt    time.Time `json:"created_at"`
	LastActiveAt time.Time `json:"last_active_at"`
}

// sessionsList GET /api/v1/user/sessions → {sessions:[...]}
func (h *Handler) sessionsList(c *gin.Context) {
	uid, ok := h.uid(c)
	if !ok {
		response.Error(c, http.StatusUnauthorized, 401, "unauthorized")
		return
	}
	list, currentID, err := h.svc.ListSessions(uid)
	if err != nil {
		fail(c, err)
		return
	}
	views := make([]sessionView, 0, len(list))
	for _, sess := range list {
		views = append(views, sessionView{
			ID:           sess.ID,
			IP:           sess.IP,
			UA:           sess.UA,
			Location:     sess.Location,
			Current:      sess.ID == currentID,
			CreatedAt:    sess.CreatedAt,
			LastActiveAt: sess.LastActiveAt,
		})
	}
	response.JSON(c, gin.H{"sessions": views})
}

// sessionRevoke DELETE /api/v1/user/sessions/:id → {ok:true}；当前会话 400，未知 404。
func (h *Handler) sessionRevoke(c *gin.Context) {
	uid, ok := h.uid(c)
	if !ok {
		response.Error(c, http.StatusUnauthorized, 401, "unauthorized")
		return
	}
	_, currentID, err := h.svc.ListSessions(uid)
	if err != nil {
		fail(c, err)
		return
	}
	if err := h.svc.RevokeSession(uid, c.Param("id"), currentID); err != nil {
		switch err {
		case ErrSessionCurrent:
			response.Error(c, http.StatusBadRequest, 400, err.Error())
		case ErrNotFound:
			response.Error(c, http.StatusNotFound, 404, "session not found")
		default:
			fail(c, err)
		}
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

// sessionRevokeAll DELETE /api/v1/user/sessions → {ok:true, revoked:n}（保留当前会话）。
func (h *Handler) sessionRevokeAll(c *gin.Context) {
	uid, ok := h.uid(c)
	if !ok {
		response.Error(c, http.StatusUnauthorized, 401, "unauthorized")
		return
	}
	_, currentID, err := h.svc.ListSessions(uid)
	if err != nil {
		fail(c, err)
		return
	}
	n, err := h.svc.RevokeOtherSessions(uid, currentID)
	if err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"ok": true, "revoked": n})
}

// antiPhishingGet GET /api/v1/user/anti-phishing → {code:""}
func (h *Handler) antiPhishingGet(c *gin.Context) {
	uid, ok := h.uid(c)
	if !ok {
		response.Error(c, http.StatusUnauthorized, 401, "unauthorized")
		return
	}
	code, err := h.svc.GetAntiPhishing(uid)
	if err != nil {
		fail(c, err)
		return
	}
	response.JSON(c, gin.H{"code": code})
}

// antiPhishingSet POST /api/v1/user/anti-phishing {code} → {ok:true,message}；空串清除。
func (h *Handler) antiPhishingSet(c *gin.Context) {
	uid, ok := h.uid(c)
	if !ok {
		response.Error(c, http.StatusUnauthorized, 401, "unauthorized")
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, 400, "invalid body")
		return
	}
	if err := h.svc.SetAntiPhishing(uid, req.Code); err != nil {
		response.Error(c, http.StatusBadRequest, 400, err.Error())
		return
	}
	msg := "防钓鱼码已设置"
	if req.Code == "" {
		msg = "防钓鱼码已清除"
	}
	response.JSON(c, gin.H{"ok": true, "message": msg})
}

package notification

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// Handler 是通知 HTTP 层，仅做参数绑定、鉴权上下文提取、调 Service、返回。
type Handler struct {
	svc *Service
}

// NewHandler 创建通知 handler。
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterRoutes 注册路由。受保护端点要求 Bearer Token（middleware.Auth 已挂）。
func (h *Handler) RegisterRoutes(r *gin.Engine) {
	// 健康检查（免鉴权，供管理后台 / 网关 / 探针探活）。
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "time": time.Now().Unix()})
	})
	g := r.Group("/api/v1/notification")
	{
		g.GET("/list", h.list)            // 我的通知列表
		g.GET("/unread-count", h.count)   // 未读数
		g.POST("/read", h.read)           // 标记单条已读
		g.POST("/read-all", h.readAll)    // 全部已读
		g.POST("/publish", h.publish)     // 发布（演示/内部调用）
	}
	// 运营/排查：全部通知（全局 Auth 已挂载；此处追加 AdminGuard，仅管理员可见全量通知，
	// 避免任意登录用户越权读取他人通知内容）。
	r.GET("/api/v1/notification/admin/list", middleware.AdminGuard(), h.listAll)

	// 用户侧别名路由：对齐 crypto-exchange-web/src/api/client.ts 的站内信契约
	// （/api/v1/user/notifications*，响应字段 notifications/read/count）。
	u := r.Group("/api/v1/user/notifications")
	{
		u.GET("", h.userList)
		u.GET("/unread-count", h.userCount)
		u.POST("/read-all", h.readAllAlias)
		u.POST("/:id/read", h.userRead)
		u.DELETE("/:id", h.userDelete)
	}
}

func uid(c *gin.Context) int64 {
	v, _ := middleware.UserID(c)
	return v
}

func (h *Handler) list(c *gin.Context) {
	userID := uid(c)
	onlyUnread := c.Query("only_unread") == "true" || c.Query("unread") == "true"
	limit, _ := strconv.Atoi(c.Query("limit"))
	ns, err := h.svc.List(userID, onlyUnread, limit)
	if err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, gin.H{"user_id": userID, "items": ns, "count": len(ns)})
}

func (h *Handler) count(c *gin.Context) {
	userID := uid(c)
	n, err := h.svc.UnreadCount(userID)
	if err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, gin.H{"user_id": userID, "unread": n})
}

func (h *Handler) read(c *gin.Context) {
	userID := uid(c)
	var req struct {
		ID int64 `json:"id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.ID <= 0 {
		response.Error(c, 400, 400, "id required")
		return
	}
	if err := h.svc.MarkRead(userID, req.ID); err != nil {
		response.Error(c, 404, 404, err.Error())
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

func (h *Handler) readAll(c *gin.Context) {
	userID := uid(c)
	n, err := h.svc.MarkAllRead(userID)
	if err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, gin.H{"ok": true, "marked": n})
}

func (h *Handler) publish(c *gin.Context) {
	userID := uid(c)
	var in PublishInput
	if err := c.ShouldBindJSON(&in); err != nil {
		response.Error(c, 400, 400, "invalid body")
		return
	}
	// 演示：发布的通知接收者即当前 token 用户（真实集成由后端服务直写 store）。
	in.UserID = userID
	n, err := h.svc.Publish(in)
	if err != nil {
		response.Error(c, 400, 400, err.Error())
		return
	}
	response.JSON(c, n)
}

func (h *Handler) listAll(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	ns, err := h.svc.ListAll(limit)
	if err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, gin.H{"items": ns, "count": len(ns)})
}

// userNotificationView 是前端契约形状的投影：
// {id, level(info|warning|critical), title, content, read(bool), created_at}。
type userNotificationView struct {
	ID        int64     `json:"id"`
	Level     string    `json:"level"`
	Title     string    `json:"title"`
	Content   string    `json:"content"`
	Read      bool      `json:"read"`
	CreatedAt time.Time `json:"created_at"`
}

func toUserView(ns []*Notification) []userNotificationView {
	out := make([]userNotificationView, 0, len(ns))
	for _, n := range ns {
		out = append(out, userNotificationView{
			ID:        n.ID,
			Level:     LevelOf(n.Type),
			Title:     n.Title,
			Content:   n.Body,
			Read:      n.Status == StatusRead,
			CreatedAt: n.CreatedAt,
		})
	}
	return out
}

// userList GET /api/v1/user/notifications?only_unread=&limit= → {notifications:[...], unread:n}
func (h *Handler) userList(c *gin.Context) {
	userID := uid(c)
	onlyUnread := c.Query("only_unread") == "true" || c.Query("unread") == "true"
	limit, _ := strconv.Atoi(c.Query("limit"))
	ns, err := h.svc.List(userID, onlyUnread, limit)
	if err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	unread, err := h.svc.UnreadCount(userID)
	if err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, gin.H{"notifications": toUserView(ns), "unread": unread})
}

// userCount GET /api/v1/user/notifications/unread-count → {count:n}
func (h *Handler) userCount(c *gin.Context) {
	n, err := h.svc.UnreadCount(uid(c))
	if err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, gin.H{"count": n})
}

// userRead POST /api/v1/user/notifications/:id/read → {ok:true}
func (h *Handler) userRead(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, 400, 400, "invalid id")
		return
	}
	if err := h.svc.MarkRead(uid(c), id); err != nil {
		response.Error(c, 404, 404, err.Error())
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

// readAllAlias POST /api/v1/user/notifications/read-all → {ok:true}
func (h *Handler) readAllAlias(c *gin.Context) {
	if _, err := h.svc.MarkAllRead(uid(c)); err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

// userDelete DELETE /api/v1/user/notifications/:id → {ok:true}
func (h *Handler) userDelete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, 400, 400, "invalid id")
		return
	}
	if err := h.svc.Delete(uid(c), id); err != nil {
		response.Error(c, 404, 404, err.Error())
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

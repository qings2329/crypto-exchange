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
	// 运营/排查：全部通知（同样需鉴权，真实部署应加 RBAC）。
	r.GET("/api/v1/notification/admin/list", h.listAll)
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

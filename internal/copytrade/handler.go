package copytrade

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// RegisterRoutes 在 gin 引擎上注册跟单路由。用户接口受 middleware.Auth 保护，
// 复制下单以「粉丝授权 token + 下游 Bearer 校验」双重约束（F4）；管理全量查询受 AdminGuard。
func (s *Service) RegisterRoutes(r *gin.Engine, verifier *middleware.TokenVerifier) {
	api := r.Group("/api/v1/copytrade")
	api.Use(middleware.Auth(verifier))
	{
		// 带单高手
		api.POST("/leads", s.handleCreateLead)
		api.GET("/leads", s.handleListLeads)
		api.POST("/leads/:id/close", s.handleCloseLead)
		// 跟单关系
		api.POST("/follows", s.handleFollow)
		api.GET("/follows", s.handleMyFollows)
		api.POST("/follows/:id/stop", s.handleStopFollow)
		// 管理（AdminGuard）
		api.GET("/admin/leads", middleware.AdminGuard(), s.handleAdminLeads)
		api.GET("/admin/follows", middleware.AdminGuard(), s.handleAdminFollows)
		api.GET("/admin/copies", middleware.AdminGuard(), s.handleAdminCopies)
		// 管理：在没有 Kafka 的环境下手动注入一笔撮合成交流以驱动复制（等价于发布到 exchange.trades）。
		api.POST("/admin/simulate-trade", middleware.AdminGuard(), s.handleSimulateTrade)
	}
}

// handleSimulateTrade 供运维/调试在没有 Kafka 时手动注入一笔成交流，驱动跟单复制。
// 仅 AdminGuard 可调用；语义等价于把该事件发布到 exchange.trades 后由订阅器转发给 OnTrade。
func (s *Service) handleSimulateTrade(c *gin.Context) {
	var ev mq.TradeEvent
	if err := c.ShouldBindJSON(&ev); err != nil {
		response.Error(c, 400, 4000, "invalid trade event")
		return
	}
	if ev.Symbol == "" || ev.Price <= 0 || ev.Qty <= 0 {
		response.Error(c, 400, 4001, "symbol/price/qty required")
		return
	}
	s.OnTrade(c.Request.Context(), ev)
	response.JSON(c, gin.H{"status": "replicated", "symbol": ev.Symbol})
}

type createLeadReq struct {
	Name string `json:"name"`
	Bio  string `json:"bio"`
}

func (s *Service) handleCreateLead(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	var req createLeadReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	l, err := s.CreateLead(uid, req.Name, req.Bio)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, l)
}

func (s *Service) handleListLeads(c *gin.Context) {
	list, err := s.ListActiveLeads()
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"leads": list})
}

func (s *Service) handleCloseLead(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, 400, 4001, "invalid id")
		return
	}
	if err := s.CloseLead(uid, id); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"id": id, "status": string(LeadClosed)})
}

type followReq struct {
	LeadID         int64   `json:"lead_id"`
	CopyRatio      float64 `json:"copy_ratio"`
	AllocatedAmount float64 `json:"allocated_amount"`
	// FollowerToken 是粉丝授权 copytrade 代其向 spot/futures 下单的 Bearer 凭证（F4）。
	FollowerToken string `json:"follower_token"`
}

func (s *Service) handleFollow(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	var req followReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	if req.LeadID <= 0 || req.CopyRatio <= 0 {
		response.Error(c, 400, 4001, "lead_id/copy_ratio required")
		return
	}
	if req.FollowerToken == "" {
		response.Error(c, 400, 4001, "follower_token required (authorize copytrade to trade on your behalf)")
		return
	}
	f, err := s.RegisterFollow(uid, req.LeadID, req.CopyRatio, req.AllocatedAmount, req.FollowerToken)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, f)
}

func (s *Service) handleMyFollows(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	list, err := s.MyFollows(uid)
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"follows": list})
}

func (s *Service) handleStopFollow(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, 400, 4001, "invalid id")
		return
	}
	if err := s.StopFollow(uid, id); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"id": id, "status": string(FollowStopped)})
}

func (s *Service) handleAdminLeads(c *gin.Context) {
	list, err := s.AdminListLeads()
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"leads": list})
}

func (s *Service) handleAdminFollows(c *gin.Context) {
	list, err := s.AdminListFollows()
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"follows": list})
}

func (s *Service) handleAdminCopies(c *gin.Context) {
	list, err := s.AdminListCopies()
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"copies": list})
}

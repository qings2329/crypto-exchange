package bot

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// RegisterRoutes 在 gin 引擎上注册交易机器人路由。用户接口受 middleware.Auth 保护，
// 全部以「策略归属用户本人 + 下游 Bearer token」双重约束执行下单（F4 鉴权守卫）。
// 管理全量查询受 middleware.AdminGuard 保护。
func (s *Service) RegisterRoutes(r *gin.Engine, verifier *middleware.TokenVerifier) {
	api := r.Group("/api/v1/bot")
	api.Use(middleware.Auth(verifier))
	{
		api.POST("/strategies", s.handleCreateStrategy)
		api.GET("/strategies", s.handleMyStrategies)
		api.POST("/strategies/:id/start", s.handleStart)
		api.POST("/strategies/:id/stop", s.handleStop)
		api.GET("/strategies/:id/orders", s.handleListOrders)
		// 管理：全量策略查看（运维/风控用途）。
		api.GET("/admin/strategies", middleware.AdminGuard(), s.handleAdminStrategies)
	}
}

type createStrategyReq struct {
	Name   string     `json:"name"`
	Market Market     `json:"market"`
	Symbol string     `json:"symbol"`
	Side   string     `json:"side"`
	Type   StrategyType `json:"type"`
	// UserToken 是用户授权 bot 代其下单的凭证（HMAC Bearer token）。bot 不持有私钥，
	// 仅持此 token 调用 spot/futures 的 /order，下游以该 token 校验 userID，杜绝越权（F4）。
	UserToken string `json:"user_token"`
	Params    BotParams `json:"params"`
}

func (s *Service) handleCreateStrategy(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	var req createStrategyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	if req.UserToken == "" {
		response.Error(c, 400, 4001, "user_token required (authorize bot to trade on your behalf)")
		return
	}
	st := &BotStrategy{
		UserID:    uid,
		Name:      req.Name,
		Market:    req.Market,
		Symbol:    req.Symbol,
		Side:      req.Side,
		Type:      req.Type,
		UserToken: req.UserToken,
		Params:    req.Params,
	}
	if err := s.CreateStrategy(st); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, st)
}

func (s *Service) handleMyStrategies(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	list, err := s.store.ListStrategiesByUser(uid)
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"strategies": list})
}

func (s *Service) handleStart(c *gin.Context) {
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
	if err := s.StartStrategy(uid, id); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"id": id, "status": string(StrategyActive)})
}

func (s *Service) handleStop(c *gin.Context) {
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
	if err := s.StopStrategy(uid, id); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"id": id, "status": string(StrategyStopped)})
}

func (s *Service) handleListOrders(c *gin.Context) {
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
	// F4：仅本人可见自己的策略订单。
	st, err := s.store.GetStrategy(id)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	if st.UserID != uid {
		response.Error(c, 403, 4030, "not owner")
		return
	}
	list, err := s.store.ListOrdersByStrategy(id)
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"orders": list})
}

func (s *Service) handleAdminStrategies(c *gin.Context) {
	// AdminGuard 已确保调用者为管理员；此处返回全量策略（含 stopped）供运维/风控查看。
	list, err := s.store.ListAllStrategies()
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"strategies": list})
}

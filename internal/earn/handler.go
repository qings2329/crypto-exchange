package earn

import (
	"fmt"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// RegisterRoutes 注册理财中心（/api/v1/earn）与新币挖矿（/api/v1/launchpad）路由。
// 用户接口受 middleware.Auth 保护；产品/项目/充值/对账等管理接口受 AdminGuard 保护（F4）。
func (s *Service) RegisterRoutes(r *gin.Engine, verifier *middleware.TokenVerifier) {
	earnAPI := r.Group("/api/v1/earn")
	earnAPI.Use(middleware.Auth(verifier))
	{
		earnAPI.GET("/products", s.handleListProducts)
		earnAPI.POST("/products", middleware.AdminGuard(), s.handleCreateProduct)
		earnAPI.POST("/subscribe", s.handleSubscribe)
		earnAPI.GET("/subscriptions", s.handleMySubscriptions)
		earnAPI.POST("/subscriptions/:id/redeem", s.handleRedeem)
		earnAPI.POST("/admin/accrue", middleware.AdminGuard(), s.handleAdminAccrue)
	}

	lp := r.Group("/api/v1/launchpad")
	lp.Use(middleware.Auth(verifier))
	{
		lp.GET("/projects", s.handleListProjects)
		lp.POST("/admin/projects", middleware.AdminGuard(), s.handleCreateProject)
		lp.POST("/admin/fund", middleware.AdminGuard(), s.handleFundProject)
		lp.POST("/stake", s.handleStake)
		lp.POST("/unstake", s.handleUnstake)
		lp.POST("/harvest", s.handleHarvest)
		lp.GET("/positions", s.handleMyPositions)
		lp.GET("/admin/reconcile", middleware.AdminGuard(), s.handleReconcile)
	}
}

func userIDOf(c *gin.Context) (int64, bool) {
	return middleware.UserID(c)
}

// --- 理财中心 ---

// projectView 项目读模型（对齐前端 LaunchProject 契约：total_supply 为字符串原样展示）。
type projectView struct {
	ID          int64        `json:"id"`
	Name        string       `json:"name"`
	Token       string       `json:"token"`
	TotalSupply string       `json:"total_supply"`
	StartsAt    time.Time    `json:"starts_at"`
	EndsAt      time.Time    `json:"ends_at"`
	Status      string       `json:"status"`
	Pools       []LaunchPool `json:"pools"`
}

func (s *Service) handleListProducts(c *gin.Context) {
	ps, err := s.ListProducts(c.Query("term"))
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	if ps == nil {
		ps = []*EarnProduct{}
	}
	response.JSON(c, gin.H{"products": ps})
}

type createProductReq struct {
	Name      string  `json:"name"`
	Asset     string  `json:"asset"`
	TermDays  int     `json:"term_days"`
	APY       float64 `json:"apy"`
	MinAmount float64 `json:"min_amount"`
	MaxAmount float64 `json:"max_amount"`
}

func (s *Service) handleCreateProduct(c *gin.Context) {
	var req createProductReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	p := &EarnProduct{
		Name: req.Name, Asset: req.Asset, TermDays: req.TermDays,
		APY: req.APY, MinAmount: req.MinAmount, MaxAmount: req.MaxAmount,
		Status: ProductOpen,
	}
	if err := s.CreateProduct(p); err != nil {
		response.Error(c, 400, 4001, err.Error())
		return
	}
	response.JSON(c, p)
}

type subscribeReq struct {
	ProductID int64   `json:"product_id"`
	Amount    float64 `json:"amount"`
	Agreed    bool    `json:"agreed"` // 风险揭示勾选，必须为 true（F5）
}

func (s *Service) handleSubscribe(c *gin.Context) {
	uid, ok := userIDOf(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	var req subscribeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	sub, err := s.Subscribe(uid, req.ProductID, req.Amount, req.Agreed)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, s.subscriptionPayload(sub))
}

// subscriptionPayload 组装对齐前端 EarnSubscription 契约的响应体。
func (s *Service) subscriptionPayload(sub *EarnSubscription) gin.H {
	p, _ := s.store.GetProduct(sub.ProductID)
	apy, termDays := 0.0, 0
	if p != nil {
		apy, termDays = p.APY, p.TermDays
	}
	now := s.now()
	accrued := sub.Accrued
	if sub.Status == SubActive {
		// 实时投影：已入账 + 自上次入账点按公式推算的未入账部分（纯计算，不动账本）。
		accrued = accrued.Add(sub.YieldToAmount(now, apy, sub.Accrued.Decimals))
	}
	payload := gin.H{
		"id":         sub.ID,
		"product_id": sub.ProductID,
		"asset":      sub.Asset,
		"amount":     sub.Principal.HumanFloat(),
		"apy":        apy,
		"term_days":  termDays,
		"start_at":   sub.CreatedAt.Format(time.RFC3339),
		"status":     string(sub.Status),
		"accrued":    accrued.HumanFloat(),
	}
	if sub.Status == SubRedeemed {
		payload["redeemed_amount"] = sub.RedeemedAmount.HumanFloat()
	}
	return payload
}

func (s *Service) handleMySubscriptions(c *gin.Context) {
	uid, ok := userIDOf(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	subs, err := s.store.ListSubscriptions(uid)
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	out := make([]gin.H, 0, len(subs))
	for _, sub := range subs {
		out = append(out, s.subscriptionPayload(sub))
	}
	response.JSON(c, gin.H{"subscriptions": out})
}

func (s *Service) handleRedeem(c *gin.Context) {
	uid, ok := userIDOf(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, 400, 4000, "bad id")
		return
	}
	sub, err := s.Redeem(uid, id)
	if err != nil {
		response.Error(c, 400, 4003, err.Error())
		return
	}
	response.JSON(c, s.subscriptionPayload(sub))
}

func (s *Service) handleAdminAccrue(c *gin.Context) {
	total, err := s.AccrueAll(s.now())
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"total_accrued": total.HumanFloat()})
}

// --- 新币挖矿 ---

func (s *Service) handleListProjects(c *gin.Context) {
	ps, err := s.ListProjects()
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	now := s.now()
	out := make([]projectView, 0, len(ps))
	for _, p := range ps {
		if p.Pools == nil {
			p.Pools = []LaunchPool{}
		}
		out = append(out, projectView{
			ID: p.ID, Name: p.Name, Token: p.Token, TotalSupply: p.TotalSupply,
			StartsAt: p.StartsAt, EndsAt: p.EndsAt, Status: p.Status(now), Pools: p.Pools,
		})
	}
	response.JSON(c, gin.H{"projects": out})
}

type createProjectReq struct {
	Name        string       `json:"name"`
	Token       string       `json:"token"`
	TotalSupply string       `json:"total_supply"`
	StartsAt    time.Time    `json:"starts_at"`
	EndsAt      time.Time    `json:"ends_at"`
	Pools       []LaunchPool `json:"pools"`
}

func (s *Service) handleCreateProject(c *gin.Context) {
	var req createProjectReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	p := &LaunchProject{
		Name: req.Name, Token: req.Token, TotalSupply: req.TotalSupply,
		StartsAt: req.StartsAt, EndsAt: req.EndsAt, Pools: req.Pools,
	}
	if err := s.CreateProject(p); err != nil {
		response.Error(c, 400, 4001, err.Error())
		return
	}
	response.JSON(c, gin.H{"id": p.ID})
}

type fundReq struct {
	ProjectID int64   `json:"project_id"`
	Amount    float64 `json:"amount"`
	Ref       string  `json:"ref"` // 幂等键（F1）：同一 ref 重复提交只扣款一次
}

func (s *Service) handleFundProject(c *gin.Context) {
	uid, ok := userIDOf(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	var req fundReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	amt, err := s.FundProject(uid, req.ProjectID, req.Amount, req.Ref)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"funded": amt.HumanFloat()})
}

func (s *Service) handleStake(c *gin.Context) {
	uid, ok := userIDOf(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	var req struct {
		ProjectID int64   `json:"project_id"`
		PoolID    string  `json:"pool_id"`
		Amount    float64 `json:"amount"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.PoolID == "" {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	pos, err := s.Stake(uid, req.ProjectID, req.PoolID, req.Amount)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, positionPayload(pos))
}

func (s *Service) handleUnstake(c *gin.Context) {
	uid, ok := userIDOf(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	var req struct {
		PositionID int64   `json:"position_id"`
		Amount     float64 `json:"amount"` // <=0 视为全额解押
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	pos, err := s.Unstake(uid, req.PositionID, req.Amount)
	if err != nil {
		response.Error(c, 400, 4003, err.Error())
		return
	}
	response.JSON(c, positionPayload(pos))
}

// positionPayload 组装对齐前端 LaunchPosition 契约的响应体。
func positionPayload(pos *LaunchPosition) gin.H {
	return gin.H{
		"id":         pos.ID,
		"project_id": pos.ProjectID,
		"pool_id":    pos.PoolID,
		"staked":     pos.Staked.HumanFloat(),
		"rewards":    pos.RewardsPending.HumanFloat(),
	}
}

func (s *Service) handleHarvest(c *gin.Context) {
	uid, ok := userIDOf(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	var req struct {
		PositionID int64 `json:"position_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	pos, err0 := s.store.GetPosition(req.PositionID)
	if err0 != nil {
		response.Error(c, 404, 4004, ErrPositionNotFound.Error())
		return
	}
	claimed, err := s.Harvest(uid, req.PositionID)
	if err != nil {
		response.Error(c, 400, 4005, err.Error())
		return
	}
	resp := positionPayload(pos)
	resp["claimed"] = claimed.HumanFloat()
	resp["rewards"] = 0.0
	response.JSON(c, resp)
}

func (s *Service) handleMyPositions(c *gin.Context) {
	uid, ok := userIDOf(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	views, err := s.MyPositions(uid)
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"positions": views})
}

func (s *Service) handleReconcile(c *gin.Context) {
	dev := s.Reconcile()
	out := map[string]string{}
	for k, v := range dev {
		out[k] = fmt.Sprintf("%s", v.HumanString())
	}
	if out == nil {
		out = map[string]string{}
	}
	response.JSON(c, gin.H{"deviations": out})
}

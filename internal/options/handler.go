package options

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// RegisterRoutes 在 gin 引擎上注册期权业务路由。
// 业务路由受 middleware.Auth(verifier) 保护（需合法 HMAC Bearer Token）。
func (s *Service) RegisterRoutes(r *gin.Engine, verifier *middleware.TokenVerifier) {
	api := r.Group("/api/v1/options")
	api.Use(middleware.Auth(verifier))
	{
			api.POST("/contracts", middleware.AdminGuard(), s.handleCreateContract)
		api.GET("/contracts", s.handleListContracts)
		api.GET("/quote", s.handleQuote)
		api.POST("/positions", s.handleOpenPosition)
		api.GET("/positions", s.handleListPositions)
		api.GET("/admin/positions", middleware.AdminGuard(), s.handleAdminPositions)
		api.POST("/exercise", s.handleExercise)
		api.POST("/settle", middleware.AdminGuard(), s.handleSettle)
	}
}

type createContractReq struct {
	Underlying   string    `json:"underlying"`
	QuoteAsset   string    `json:"quote_asset"`
	Strike       float64   `json:"strike"`
	Expiry       time.Time `json:"expiry"`
	Type         string    `json:"type"`
	Style        string    `json:"style"`
	ContractSize float64   `json:"contract_size"`
	Premium      float64   `json:"premium"`
}

func (s *Service) handleCreateContract(c *gin.Context) {
	var req createContractReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	if req.Underlying == "" || req.Strike <= 0 || req.Expiry.IsZero() ||
		(req.Type != "call" && req.Type != "put") ||
		(req.Style != "european" && req.Style != "american") {
		response.Error(c, 400, 4001, "underlying/strike/expiry/type/style required and valid")
		return
	}
	if !settlement.KnownAsset(req.Underlying) ||
		(req.QuoteAsset != "" && !settlement.KnownAsset(req.QuoteAsset)) {
		response.Error(c, 400, 4001, "unsupported underlying/quote_asset")
		return
	}
	// 计价资产默认值与定点小数位解析（HTTP 输入为 float，落库统一为定点 AssetAmount）。
	quote := req.QuoteAsset
	if quote == "" {
		quote = "USDT"
	}
	dec := settlement.AssetDecimalsByName(quote)
	contract := &OptionContract{
		Underlying:   req.Underlying,
		QuoteAsset:   quote,
		Strike:       settlement.AssetAmountFromFloat(req.Strike, dec),
		Expiry:       req.Expiry,
		Type:         OptionType(req.Type),
		Style:        ExerciseStyle(req.Style),
		ContractSize: settlement.AssetAmountFromFloat(req.ContractSize, dec),
		Premium:      settlement.AssetAmountFromFloat(req.Premium, dec),
	}
	if err := s.CreateContract(contract); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, contract)
}

func (s *Service) handleListContracts(c *gin.Context) {
	list, err := s.ListContracts()
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"contracts": list})
}

func (s *Service) handleQuote(c *gin.Context) {
	id, err := strconv.ParseInt(c.Query("contract_id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, 400, 4000, "contract_id required")
		return
	}
	premium, delta, err := s.Quote(id)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"contract_id": id, "premium": premium, "delta": delta})
}

type openPositionReq struct {
	ContractID int64   `json:"contract_id"`
	Side       string  `json:"side"`
	Quantity   float64 `json:"quantity"`
}

func (s *Service) handleOpenPosition(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	var req openPositionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 4000, "invalid body")
		return
	}
	if req.ContractID <= 0 || (req.Side != "long" && req.Side != "short") || req.Quantity <= 0 {
		response.Error(c, 400, 4001, "contract_id/side/quantity required")
		return
	}
	p, err := s.OpenPosition(uid, req.ContractID, PositionSide(req.Side), req.Quantity)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, p)
}

func (s *Service) handleListPositions(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	list, err := s.ListPositions(uid)
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"positions": list})
}

func (s *Service) handleAdminPositions(c *gin.Context) {
	list, err := s.AdminListPositions()
	if err != nil {
		response.Error(c, 500, 5000, err.Error())
		return
	}
	response.JSON(c, gin.H{"positions": list})
}

type positionIDReq struct {
	PositionID int64 `json:"position_id"`
}

func (s *Service) handleExercise(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 4010, "unauthorized")
		return
	}
	var req positionIDReq
	if err := c.ShouldBindJSON(&req); err != nil || req.PositionID <= 0 {
		response.Error(c, 400, 4000, "position_id required")
		return
	}
	if err := s.Exercise(uid, req.PositionID); err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"ok": true})
}

func (s *Service) handleSettle(c *gin.Context) {
	var req positionIDReq
	if err := c.ShouldBindJSON(&req); err != nil || req.PositionID <= 0 {
		response.Error(c, 400, 4000, "position_id required")
		return
	}
	settled, err := s.SettlePosition(req.PositionID)
	if err != nil {
		response.Error(c, 400, 4002, err.Error())
		return
	}
	response.JSON(c, gin.H{"settled": settled})
}

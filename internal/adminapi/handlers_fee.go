package adminapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
)

// handleTradingFeeGet 返回当前交易手续费模型配置快照（全局费率 + VIP 折扣 + 交易对覆盖）。
func (s *Server) handleTradingFeeGet(c *gin.Context) {
	snapshot := s.feeModel.GetSnapshot()
	response.JSON(c, snapshot)
}

// handleTradingFeeSet 更新交易手续费配置：
//
//	请求体允许字段：
//	  global_taker_rate  float64 — 全局 taker 基础费率，<=0 表示不变
//	  global_maker_rate  float64 — 全局 maker 基础费率，负数表示返佣
//	  vip_discounts      []{level, taker_discount, maker_discount}
//	  symbol_overrides   {symbol: {taker_rate, maker_rate}}
//
// 未传入的字段保留原值。
func (s *Server) handleTradingFeeSet(c *gin.Context) {
	var body struct {
		GlobalTakerRate  float64 `json:"global_taker_rate"`
		GlobalMakerRate  float64 `json:"global_maker_rate"`
		VIPDiscounts     []struct {
			Level           int8    `json:"level"`
			TakerDiscount   float64 `json:"taker_discount"`
			MakerDiscount   float64 `json:"maker_discount"`
		} `json:"vip_discounts"`
		SymbolOverrides map[string]struct {
			TakerRate float64 `json:"taker_rate"`
			MakerRate float64 `json:"maker_rate"`
		} `json:"symbol_overrides"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		response.Error(c, http.StatusBadRequest, 400, "invalid body: "+err.Error())
		return
	}

	// 全局费率
	if body.GlobalTakerRate > 0 || body.GlobalMakerRate >= 0 {
		s.feeModel.SetGlobalRates(body.GlobalTakerRate, body.GlobalMakerRate)
	}

	// VIP 折扣
	if len(body.VIPDiscounts) > 0 {
		vips := make([]matching.VIPDiscount, 0, len(body.VIPDiscounts))
		for _, d := range body.VIPDiscounts {
			vips = append(vips, matching.VIPDiscount{
				Level:         d.Level,
				TakerDiscount: d.TakerDiscount,
				MakerDiscount: d.MakerDiscount,
			})
		}
		s.feeModel.SetVIPDiscounts(vips)
	}

	// 交易对覆盖
	if len(body.SymbolOverrides) > 0 {
		overrides := make(map[string]matching.SymbolFeeConfig, len(body.SymbolOverrides))
		for sym, cfg := range body.SymbolOverrides {
			overrides[sym] = matching.SymbolFeeConfig{
				TakerRate: cfg.TakerRate,
				MakerRate: cfg.MakerRate,
			}
		}
		s.feeModel.SetSymbolOverrides(overrides)
	}

	response.JSON(c, s.feeModel.GetSnapshot())
}

package adminapi

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// userBalanceAssetList 是「查看用户账户余额」页默认轮询的资产清单。
// futures 钱包账本为多资产（USDT/BTC/ETH），但没有「单用户全资产」接口，
// 故逐资产调用 /api/v1/futures/wallet/balance 聚合。需要更多资产时在此追加。
var userBalanceAssetList = []string{"USDT", "BTC", "ETH"}

// UserAssetBalance 是单个资产的账户余额视图（futures 钱包）。
type UserAssetBalance struct {
	Asset          string  `json:"asset"`
	Available      float64 `json:"available"`
	Frozen         float64 `json:"frozen"`
	WithdrawFrozen float64 `json:"withdraw_frozen"`
	Exists         bool    `json:"exists"`
}

// UserBalances 是某用户账户余额聚合视图。
type UserBalances struct {
	UserID int64              `json:"user_id"`
	Assets []UserAssetBalance `json:"assets"`
	// AssetTotals 按资产分别汇总（available+frozen+withdraw_frozen），避免不同币种
	// 直接相加成无意义的单一数字（跨币种加总在财务上不成立）。
	AssetTotals map[string]float64 `json:"asset_totals"`
}

// getUserBalances 代理 futures 钱包，聚合展示某用户的多资产余额（USDT/BTC/ETH 等）。
// 数据来自 futures 上游的 /api/v1/futures/wallet/balance（逐资产）；上游全部不可达时
// 返回空资产列表并置 X-Degraded 头（与 listUsers 的降级语义一致）。
func (s *Server) getUserBalances(c *gin.Context) {
	uid, ok := parseInt64(c, "id")
	if !ok || uid <= 0 {
		s.fail(c, http.StatusBadRequest, "invalid user id")
		return
	}

	ctx := c.Request.Context()
	fb := s.serviceURL("futures")

	assets := make([]UserAssetBalance, 0, len(userBalanceAssetList))
	anyErr := false
	for _, asset := range userBalanceAssetList {
		var bal struct {
			Asset          string  `json:"asset"`
			Available      float64 `json:"available"`
			Frozen         float64 `json:"frozen"`
			WithdrawFrozen float64 `json:"withdraw_frozen"`
			Exists         bool    `json:"exists"`
		}
		path := "/api/v1/futures/wallet/balance?user_id=" + strconv.FormatInt(uid, 10) + "&asset=" + asset
		if fb == "" || s.up.Get(ctx, fb, path, &bal) != nil {
			anyErr = true
			continue
		}
		assets = append(assets, UserAssetBalance{
			Asset:          bal.Asset,
			Available:      bal.Available,
			Frozen:         bal.Frozen,
			WithdrawFrozen: bal.WithdrawFrozen,
			Exists:         bal.Exists,
		})
	}

	assetTotals := make(map[string]float64, len(assets))
	for _, a := range assets {
		assetTotals[a.Asset] = a.Available + a.Frozen + a.WithdrawFrozen
	}

	if anyErr && len(assets) == 0 {
		c.Header("X-Degraded", "futures-unavailable")
	}

	s.ok(c, UserBalances{
		UserID:      uid,
		Assets:      assets,
		AssetTotals: assetTotals,
	})
}

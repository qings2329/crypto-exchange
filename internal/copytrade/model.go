package copytrade

import (
	"strings"

	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// LeadStatus 带单高手状态。
type LeadStatus string

const (
	LeadActive LeadStatus = "active"
	LeadClosed LeadStatus = "closed"
)

// FollowStatus 跟单关系状态。
type FollowStatus string

const (
	FollowActive  FollowStatus = "active"
	FollowStopped FollowStatus = "stopped"
)

// CopyStatus 复制成交记录状态。
type CopyStatus string

const (
	CopyDone   CopyStatus = "done"
	CopyFailed CopyStatus = "failed"
)

// LeadTrader 是「带单高手」：任何用户都可注册为被跟单对象（其成交会被粉丝复制）。
// ID 即该用户的 user_id（一笔成交事件中作为 TakerID/MakerID 出现时，触发粉丝复制）。
type LeadTrader struct {
	ID        int64      `json:"id"`         // 用户的 user_id
	Name      string     `json:"name"`
	Bio       string     `json:"bio"`
	Status    LeadStatus `json:"status"`
	CreatedAt int64      `json:"created_at"`
}

// Follow 是「粉丝」对某个 lead 的跟单关系。
// CopyRatio 为复制比例（按 lead 成交名义额的占比，可 >1 表示放大）；
// AllocatedAmount 为粉丝用于跟单的最大本金（计价币本位，如 USDT），复制额超过则封顶；
// FollowerToken 是粉丝授权 copytrade 代其向 spot/futures 下单的 Bearer 凭证（不序列化外出）。
type Follow struct {
	ID             int64        `json:"id"`
	LeadID         int64        `json:"lead_id"`
	FollowerID     int64        `json:"follower_id"`
	CopyRatio      float64      `json:"copy_ratio"`
	AllocatedAmount float64     `json:"allocated_amount"`
	FollowerToken  string       `json:"-"`
	Status         FollowStatus `json:"status"`
	CreatedAt      int64        `json:"created_at"`
	StoppedAt      int64        `json:"stopped_at"`
}

// CopyRecord 是一条「复制成交」记录：某次 lead 成交触发、为某粉丝产生的一笔跟单。
// EventID 与 FollowID 组合构成幂等键（F1），防止同一笔成交被重复复制。
type CopyRecord struct {
	ID             int64      `json:"id"`
	EventID        string     `json:"event_id"`    // 源成交事件指纹（用于全局去重）
	LeadID         int64      `json:"lead_id"`
	FollowID       int64      `json:"follow_id"`
	FollowerID     int64      `json:"follower_id"`
	Symbol         string     `json:"symbol"`
	Side           string     `json:"side"`
	Price          float64    `json:"price"`
	Qty            float64    `json:"qty"`
	Notional       float64    `json:"notional"`        // 粉丝复制名义额（计价币，仅用于下单尺寸/展示）
	FeeAmount      settlement.AssetAmount `json:"fee_amount"` // 平台复制费（定点，结算入 SysCopyTradeFee）
	ExchangeOrderID string    `json:"exchange_order_id"`
	Status         CopyStatus `json:"status"`
	CreatedAt      int64      `json:"created_at"`
}

// quoteAsset 从现货交易对（BASE_QUOTE）解析计价资产；非标准格式返回空串（由 F5 拒绝）。
func quoteAsset(symbol string) string {
	parts := strings.SplitN(symbol, "_", 2)
	if len(parts) != 2 || parts[1] == "" {
		return ""
	}
	return parts[1]
}

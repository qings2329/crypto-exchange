// Package otc 实现场外交易（OTC / P2P）业务线。
//
// 业务模型（中央托管 escrow 简化模型）：
//   - 广告（Advertisement）：maker 发布买入/卖出加密资产的广告（含固定法币单价、数量区间、支付方式）。
//   - 订单（Order）：taker 吃单后生成订单，crypto 卖方的资产先冻结进中央托管账户（SysOtc）；
//     法币由交易双方线下 P2P 支付，不在账本内；待卖方确认收款后，crypto 从托管释放给买方。
//   - 对手方信用（Counterparty）：每对用户维护成交次数与评分，支撑"对手方信用"评估。
//   - 对账（Reconcile）：所有订单终态后，SysOtc 余额应恒为 0。
//
// 分层：本包为「Service + Store」层，HTTP 路由在 handler.go，装配在 cmd/otc/main.go。
// 所有持久化表名以 ce_ 开头（见 docs/CONVENTIONS.md）。
package otc

import (
	"errors"
	"time"
)

// 领域错误。
var (
	ErrAdNotFound            = errors.New("advertisement not found")
	ErrOrderNotFound         = errors.New("order not found")
	ErrCounterpartyNotFound  = errors.New("counterparty not found")
	ErrInvalidSide           = errors.New("side must be buy or sell")
	ErrInvalidAmount         = errors.New("amount must be positive")
	ErrInvalidTransition     = errors.New("invalid order status transition")
	ErrNotAdvertiser         = errors.New("not the advertiser")
	ErrNotParty              = errors.New("not a party of this order")
	ErrInsufficientBalance   = errors.New("insufficient available crypto balance")
	ErrAdNotOpen             = errors.New("advertisement not open")
	ErrAmountOutOfRange      = errors.New("amount out of advertisement range")
	ErrNotTaker              = errors.New("only taker can mark paid")
	ErrNotMaker              = errors.New("only maker can confirm complete")
	ErrEscrowReleased        = errors.New("escrow already released")
)

// AdSide 是广告方向：buy = 买方广告（maker 买 crypto，taker 卖）；sell = 卖方广告（maker 卖）。
type AdSide string

const (
	SideBuy  AdSide = "buy"
	SideSell AdSide = "sell"
)

// AdStatus 是广告状态。
type AdStatus string

const (
	AdOpen      AdStatus = "open"
	AdFilled    AdStatus = "filled"
	AdCancelled AdStatus = "cancelled"
)

// OrderStatus 是订单状态机。
type OrderStatus string

const (
	OrderPending   OrderStatus = "pending"   // 已吃单，crypto 在托管，待买方法币支付
	OrderPaid      OrderStatus = "paid"      // 买方标记已付款，待卖方确认
	OrderCompleted OrderStatus = "completed" // 卖方确认，crypto 已释放给买方（终态）
	OrderCancelled OrderStatus = "cancelled" // 取消，crypto 退回卖方（终态）
	OrderDisputed  OrderStatus = "disputed"  // 争议中，待管理员裁决
)

// OtcAdvertisement 是场外交易广告（maker 挂单）。
type OtcAdvertisement struct {
	ID             int64     `json:"id"`
	UserID         int64     `json:"user_id"`       // 广告发布方（maker）
	Side           AdSide    `json:"side"`          // buy / sell
	Asset          string    `json:"asset"`         // 加密资产，如 BTC
	FiatCurrency   string    `json:"fiat_currency"` // 法币，如 CNY
	Price          float64   `json:"price"`         // 每单位 crypto 的法币价格
	MinAmount      float64   `json:"min_amount"`    // 单笔最小 crypto 数量
	MaxAmount      float64   `json:"max_amount"`    // 单笔最大 crypto 数量
	PaymentMethods string    `json:"payment_methods"`
	Status         AdStatus  `json:"status"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// CryptoAmountFor 返回一笔法币金额对应的 crypto 数量。
func (a *OtcAdvertisement) CryptoAmountFor(fiat float64) float64 {
	if a.Price <= 0 {
		return 0
	}
	return fiat / a.Price
}

// OtcOrder 是一笔场外成交订单。
type OtcOrder struct {
	ID             int64       `json:"id"`
	AdID           int64       `json:"ad_id"`
	MakerID        int64       `json:"maker_id"`
	TakerID        int64       `json:"taker_id"`
	Side           AdSide      `json:"side"`          // 继承广告方向
	Asset          string      `json:"asset"`
	FiatCurrency   string      `json:"fiat_currency"`
	CryptoAmount   float64     `json:"crypto_amount"`
	Price          float64     `json:"price"`
	FiatAmount     float64     `json:"fiat_amount"`
	PaymentMethod  string      `json:"payment_method"`
	Status         OrderStatus `json:"status"`
	Rating         int         `json:"rating"` // 0 表示未评分
	CreatedAt      time.Time   `json:"created_at"`
	PaidAt         time.Time   `json:"paid_at"`
	CompletedAt    time.Time   `json:"completed_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

// SellerID 返回 crypto 卖方：sell 广告 -> maker；buy 广告 -> taker。
func (o *OtcOrder) SellerID() int64 {
	if o.Side == SideSell {
		return o.MakerID
	}
	return o.TakerID
}

// BuyerID 返回 crypto 买方：sell 广告 -> taker；buy 广告 -> maker。
func (o *OtcOrder) BuyerID() int64 {
	if o.Side == SideSell {
		return o.TakerID
	}
	return o.MakerID
}

// IsFinal 是否已到终态（completed / cancelled）。
func (o *OtcOrder) IsFinal() bool {
	return o.Status == OrderCompleted || o.Status == OrderCancelled
}

// OtcCounterparty 是一对用户的对手方信用/声誉记录。
type OtcCounterparty struct {
	ID              int64     `json:"id"`
	UserID          int64     `json:"user_id"`        // 本地用户
	CounterpartyID  int64     `json:"counterparty_id"` // 对手方
	TradesTotal     int       `json:"trades_total"`
	TradesCompleted int       `json:"trades_completed"`
	RatingSum       int       `json:"rating_sum"`
	RatingCount     int       `json:"rating_count"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

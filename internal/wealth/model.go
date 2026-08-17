// Package wealth 实现理财资管业务线（活期 / 定期理财产品）。
//
// 业务模型（中央托管模型）：
//   - 产品（WealthProduct）：由平台发行的理财产品，含底层资产（如 USDT）、产品类型（活期/定期）、
//     年化收益率、锁定期限（定期产品 duration_days>0，活期为 0 可随时赎回）、起购金额、上下架状态。
//   - 持仓（WealthHolding）：用户申购后生成的持仓，记录本金、已计收益、状态（持有中/已赎回）。
//   - 中央托管：用户申购时本金转入 SysWealth；赎回时本金+应计收益从 SysWealth 支出给用户。
//     收益按"本金 × 年化 × 持有小时 / 8760"连续计息，定期产品到期前不可赎回。
//
// 分层：本包为「Service + Store」层，HTTP 路由在 handler.go，装配在 cmd/wealth/main.go。
// 所有持久化表名以 ce_ 开头（见 docs/CONVENTIONS.md）。
package wealth

import (
	"errors"
	"time"

	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// 领域错误。
var (
	ErrProductNotFound  = errors.New("product not found")
	ErrHoldingNotFound  = errors.New("holding not found")
	ErrInvalidType      = errors.New("product type must be current or fixed")
	ErrInvalidRate      = errors.New("annual rate must be non-negative")
	ErrInvalidDuration  = errors.New("duration days must be >= 0")
	ErrInvalidAmount    = errors.New("amount must be positive")
	ErrBelowMinAmount   = errors.New("amount below product minimum")
	ErrProductNotOpen   = errors.New("product not open")
	ErrInsufficientBal  = errors.New("insufficient available balance")
	ErrNotOwner         = errors.New("not the holding owner")
	ErrAlreadyRedeemed  = errors.New("holding already redeemed")
	ErrLocked           = errors.New("fixed product is still locked before maturity")
)

// ProductType 是理财产品类型。
type ProductType string

const (
	TypeCurrent ProductType = "current" // 活期：可随时赎回
	TypeFixed   ProductType = "fixed"   // 定期：到期前锁定
)

// ProductStatus 是产品上下架状态。
type ProductStatus string

const (
	ProductOpen   ProductStatus = "open"
	ProductClosed ProductStatus = "closed"
)

// HoldingStatus 是持仓状态。
type HoldingStatus string

const (
	HoldingActive   HoldingStatus = "active"   // 持有中（已出资，可赎回/计息）
	HoldingFunding  HoldingStatus = "funding"  // 瞬态：持仓已落库、本金转入进行中（不可赎回/计息）
	HoldingRedeemed HoldingStatus = "redeemed" // 已赎回（终态）
)

// WealthProduct 是一个理财产品。
type WealthProduct struct {
	ID           int64         `json:"id"`
	Name         string        `json:"name"`
	Asset        string        `json:"asset"`         // 底层资产，如 USDT
	Type         ProductType   `json:"type"`          // current / fixed
	AnnualRate   float64       `json:"annual_rate"`   // 年化收益率，如 0.05 表示 5%
	DurationDays int           `json:"duration_days"` // 锁定期限（天）；活期为 0
	MinAmount    float64       `json:"min_amount"`    // 起购金额
	Status       ProductStatus `json:"status"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

// Maturity 返回定期产品到期时间（活期返回零值）。
func (p *WealthProduct) Maturity(h *WealthHolding) time.Time {
	if p.Type != TypeFixed || p.DurationDays <= 0 {
		return time.Time{}
	}
	return h.CreatedAt.AddDate(0, 0, p.DurationDays)
}

// WealthHolding 是用户的一笔理财持仓。
type WealthHolding struct {
	ID            int64         `json:"id"`
	UserID        int64         `json:"user_id"`
	ProductID     int64         `json:"product_id"`
	Asset         string       `json:"asset"`          // 底层资产（principal/accrued_yield 的计价单位，扫描时推导小数位）
	Principal     settlement.AssetAmount `json:"principal"`      // 本金（申购金额，定点）
	AccruedYield  settlement.AssetAmount `json:"accrued_yield"`  // 已计收益（累计，定点）
	Status        HoldingStatus `json:"status"`
	CreatedAt     time.Time     `json:"created_at"`
	LastAccrualAt time.Time     `json:"last_accrual_at"` // 上次计息基准时间
	RedeemedAt    time.Time     `json:"redeemed_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
}

// YieldTo 计算从 last_accrual_at（未计则取 created_at）到 t 的应计收益（连续计息，按小时）。
// 纯函数，便于单测。年化按 8760 小时计。
func (h *WealthHolding) YieldTo(t time.Time, annualRate float64) float64 {
	from := h.LastAccrualAt
	if from.IsZero() {
		from = h.CreatedAt
	}
	if t.Before(from) {
		return 0
	}
	hours := t.Sub(from).Hours()
	if hours <= 0 {
		return 0
	}
	return h.Principal.HumanFloat() * annualRate * hours / 8760.0
}

// TotalValue 返回本金 + 已计收益（定点）。
func (h *WealthHolding) TotalValue() settlement.AssetAmount {
	return h.Principal.Add(h.AccruedYield)
}

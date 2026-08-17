// Package margin 实现现货杠杆（跨仓保证金）业务线。
//
// 业务模型：用户以稳定币（默认 USDT）作为抵押，借入某交易资产（如 BTC）获得杠杆，
// 借出资产直接记入 ledger 可用余额，用户可在现货市场使用；利息按小时利率在债务上累计，
// 当抵押价值不足维持率时触发强平（收回借出资产、罚没部分抵押入保险基金）。
//
// 分层：本包为「Service + Store」层，HTTP 路由在 handler.go，装配在 cmd/margin/main.go。
// 所有持久化表名以 ce_ 开头（见 docs/CONVENTIONS.md）。
package margin

import (
	"errors"
	"time"

	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// 领域错误。
var (
	ErrNotFound             = errors.New("margin account not found")
	ErrNoActiveAccount      = errors.New("no active margin account")
	ErrInsufficientBalance  = errors.New("insufficient available balance")
	ErrInsufficientCollateral = errors.New("insufficient collateral balance")
	ErrOverMaxLeverage      = errors.New("leverage exceeds max")
	ErrInvalidLeverage      = errors.New("leverage must be >= 1")
	ErrNothingOwed          = errors.New("nothing owed")
	ErrAccountLiquidated    = errors.New("account already liquidated/closed")
	ErrAmountMustBePositive = errors.New("amount must be positive")
	ErrAlreadyBorrowed      = errors.New("active margin account already exists for this asset")
	ErrUnsupportedAsset     = errors.New("unsupported asset")
)

// AccountStatus 是杠杆账户状态。
type AccountStatus string

const (
	StatusActive     AccountStatus = "active"
	StatusLiquidated AccountStatus = "liquidated"
	StatusClosed     AccountStatus = "closed"
)

// MarginAccount 是一条杠杆账户记录（单用户 + 单借入资产）。
type MarginAccount struct {
	UserID           int64                      `json:"user_id"`
	Asset            string                     `json:"asset"`             // 借入资产，如 BTC
	CollateralAsset  string                     `json:"collateral_asset"`  // 抵押资产，如 USDT
	CollateralAmount settlement.AssetAmount     `json:"collateral_amount"` // 已冻结抵押数量
	Debt             settlement.AssetAmount     `json:"debt"`              // 借入本金
	InterestAccrued  settlement.AssetAmount     `json:"interest_accrued"`  // 累计利息
	Leverage         int                        `json:"leverage"`
	Status           AccountStatus              `json:"status"`
	LastAccrual      time.Time                  `json:"last_accrual"`
	CreatedAt        time.Time                  `json:"created_at"`
	UpdatedAt        time.Time                  `json:"updated_at"`
}

// TotalOwed 返回当前应还总额（本金 + 利息，定点）。
func (a *MarginAccount) TotalOwed() settlement.AssetAmount {
	return a.Debt.Add(a.InterestAccrued)
}

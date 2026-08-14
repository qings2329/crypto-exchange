// Package options 实现期权（options）业务线。
//
// 业务模型（中央对手方简化模型）：交易所作为所有期权持仓的中央对手方。
//   - 买方（long）：开仓支付权利金给系统账户（SysOptions），到期/行权时按内在价值获得收益。
//   - 卖方（short）：开仓收取权利金，并按名义价值冻结保证金；到期结算时承担义务。
//
// 分层：本包为「Service + Store」层，HTTP 路由在 handler.go，装配在 cmd/options/main.go。
// 所有持久化表名以 ce_ 开头（见 docs/CONVENTIONS.md）。
package options

import (
	"errors"
	"time"
)

// 领域错误。
var (
	ErrContractNotFound  = errors.New("option contract not found")
	ErrPositionNotFound  = errors.New("option position not found")
	ErrInvalidSide       = errors.New("side must be long or short")
	ErrInvalidQuantity   = errors.New("quantity must be positive")
	ErrInsufficientBalance = errors.New("insufficient available balance")
	ErrInvalidExpiry     = errors.New("expiry must be in the future")
	ErrInvalidType       = errors.New("type must be call or put")
	ErrInvalidStyle      = errors.New("style must be european or american")
	ErrNotExercisable    = errors.New("contract not exercisable at this time")
	ErrAlreadySettled    = errors.New("position already settled")
	ErrNoPriceFeed       = errors.New("price feed unavailable for underlying")
	ErrPremiumRequired   = errors.New("premium required (or provide a live price feed)")
)

// OptionType 是期权类型。
type OptionType string

const (
	TypeCall OptionType = "call"
	TypePut  OptionType = "put"
)

// ExerciseStyle 是行权方式。
type ExerciseStyle string

const (
	StyleEuropean ExerciseStyle = "european" // 仅到期可行权
	StyleAmerican ExerciseStyle = "american" // 随时可行权
)

// PositionSide 是持仓方向：long 买方 / short 卖方。
type PositionSide string

const (
	SideLong  PositionSide = "long"
	SideShort PositionSide = "short"
)

// PositionStatus 是持仓状态。
type PositionStatus string

const (
	StatusOpen      PositionStatus = "open"
	StatusExercised PositionStatus = "exercised"
	StatusExpired   PositionStatus = "expired"
)

// OptionContract 是期权合约模板（由管理员创建）。
type OptionContract struct {
	ID           int64         `json:"id"`
	Underlying   string        `json:"underlying"`     // 标的资产，如 BTC
	QuoteAsset   string        `json:"quote_asset"`    // 计价资产，如 USDT
	Strike       float64       `json:"strike"`         // 行权价（以 quote 计）
	Expiry       time.Time     `json:"expiry"`         // 到期时间
	Type         OptionType    `json:"type"`           // call / put
	Style        ExerciseStyle `json:"style"`          // european / american
	ContractSize float64       `json:"contract_size"`  // 每张合约对应标的数量，默认 1
	Premium      float64       `json:"premium"`        // 开仓权利金单价（每张，quote 计）
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

// size 返回合约乘数，默认 1。
func (c *OptionContract) size() float64 {
	if c.ContractSize <= 0 {
		return 1
	}
	return c.ContractSize
}

// IntrinsicValue 按现货价 spot 计算「每张」内在价值（quote 计）。
func (c *OptionContract) IntrinsicValue(spot float64) float64 {
	switch c.Type {
	case TypeCall:
		if spot > c.Strike {
			return (spot - c.Strike) * c.size()
		}
	case TypePut:
		if c.Strike > spot {
			return (c.Strike - spot) * c.size()
		}
	}
	return 0
}

// OptionPosition 是用户的一条期权持仓。
type OptionPosition struct {
	ID         int64          `json:"id"`
	UserID     int64          `json:"user_id"`
	ContractID int64          `json:"contract_id"`
	Side       PositionSide   `json:"side"`       // long(买方) / short(卖方)
	Quantity   float64        `json:"quantity"`   // 持有张数
	Premium    float64        `json:"premium"`    // 开仓权利金单价（每张）
	Margin     float64        `json:"margin"`     // 卖方冻结的保证金（quote 计）
	Status     PositionStatus `json:"status"`
	OpenedAt   time.Time      `json:"opened_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

// PremiumTotal 返回该持仓支付/收取的权利金总额（quote 计）。
func (p *OptionPosition) PremiumTotal() float64 {
	return p.Premium * p.Quantity
}

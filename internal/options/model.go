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

	"github.com/coldlar/crypto-exchange/internal/settlement"
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
	ErrUnsupportedAsset  = errors.New("unsupported asset")
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
	Underlying   string                 `json:"underlying"`     // 标的资产，如 BTC
	QuoteAsset   string                 `json:"quote_asset"`    // 计价资产，如 USDT
	Strike       settlement.AssetAmount `json:"strike"`         // 行权价（以 quote 计，定点）
	Expiry       time.Time              `json:"expiry"`         // 到期时间
	Type         OptionType             `json:"type"`           // call / put
	Style        ExerciseStyle          `json:"style"`          // european / american
	ContractSize settlement.AssetAmount `json:"contract_size"`  // 每张合约对应标的数量，默认 1（定点）
	Premium      settlement.AssetAmount `json:"premium"`        // 开仓权利金单价（每张，quote 计，定点）
	CreatedAt    time.Time              `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
}

// quoteDec 返回合约计价资产的小数位（定点化用）。
func (c *OptionContract) quoteDec() int {
	return settlement.AssetDecimalsByName(c.QuoteAsset)
}

// size 返回合约乘数（定点，默认 1）。
func (c *OptionContract) size() settlement.AssetAmount {
	if c.ContractSize.Sign() <= 0 {
		return settlement.AssetAmount{Decimals: c.quoteDec()}
	}
	return c.ContractSize
}

// IntrinsicValue 按现货价 spot（quote 计，外部行情为 float）计算「每张」内在价值（quote 计，定点）。
// 定点减法消除行权价 float 精度损失；乘数/数量通常为整数，经 float 桥接无精度损失（沿用 PremiumTotal 模式）。
func (c *OptionContract) IntrinsicValue(spot float64) settlement.AssetAmount {
	dec := c.quoteDec()
	spotAmt := settlement.AssetAmountFromFloat(spot, dec)
	switch c.Type {
	case TypeCall:
		if spot > c.Strike.HumanFloat() {
			iv := spotAmt.Sub(c.Strike)
			return settlement.AssetAmountFromFloat(iv.HumanFloat()*c.size().HumanFloat(), dec)
		}
	case TypePut:
		if c.Strike.HumanFloat() > spot {
			iv := c.Strike.Sub(spotAmt)
			return settlement.AssetAmountFromFloat(iv.HumanFloat()*c.size().HumanFloat(), dec)
		}
	}
	return settlement.AssetAmount{Decimals: dec}
}

// OptionPosition 是用户的一条期权持仓。
type OptionPosition struct {
	ID         int64          `json:"id"`
	UserID     int64          `json:"user_id"`
	ContractID int64          `json:"contract_id"`
	Side       PositionSide   `json:"side"`       // long(买方) / short(卖方)
	Quantity   float64        `json:"quantity"`   // 持有张数
	QuoteAsset string         `json:"quote_asset"` // 计价资产（premium/margin 的计价单位，扫描时推导小数位）
	Premium    settlement.AssetAmount `json:"premium"` // 开仓权利金单价（每张，quote 计）
	Margin     settlement.AssetAmount `json:"margin"`  // 卖方冻结的保证金（quote 计）
	Status     PositionStatus `json:"status"`
	OpenedAt   time.Time      `json:"opened_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

// PremiumTotal 返回该持仓支付/收取的权利金总额（quote 计，定点）。
func (p *OptionPosition) PremiumTotal() settlement.AssetAmount {
	dec := settlement.AssetDecimalsByName(p.QuoteAsset)
	// 保留裸 FromFloat：p.Premium 与 p.Quantity 均为账本内部定点值，不接受用户输入，NaN/Inf 不可达；
	// 此处仅做权利金总额量化（持仓已 F2 定点化）。
	return settlement.AssetAmountFromFloat(p.Premium.HumanFloat()*p.Quantity, dec)
}

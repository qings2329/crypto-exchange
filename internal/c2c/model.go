package c2c

import (
	"time"
)

// OrderStatus 是 C2C 订单（挂单广告）状态。
type OrderStatus string

const (
	StatusOpen      OrderStatus = "open"      // 挂单（可被冻结/完成）
	StatusLocked    OrderStatus = "locked"    // 已冻结（运营介入）
	StatusCompleted OrderStatus = "completed" // 已完成
	StatusCancelled OrderStatus = "cancelled" // 已取消
	StatusDisputed  OrderStatus = "disputed"  // 纠纷中
)

func ValidStatus(s OrderStatus) bool {
	switch s {
	case StatusOpen, StatusLocked, StatusCompleted, StatusCancelled, StatusDisputed:
		return true
	}
	return false
}

// Side 表示买卖方向。
type Side string

const (
	SideBuy  Side = "buy"  // 买入
	SideSell Side = "sell" // 卖出
)

func ValidSide(s Side) bool {
	return s == SideBuy || s == SideSell
}

// Order 是 C2C 订单（用户发布的法币/币币买卖挂单广告）。
type Order struct {
	ID        int64       `json:"id"`
	Side      Side        `json:"side"`
	Coin      string      `json:"coin"`    // 交易标的（USDT/BTC/ETH）
	Amount    float64     `json:"amount"`  // 数量
	Price     float64     `json:"price"`   // 单价（法定货币，如 CNY）
	Total     float64     `json:"total"`   // 总价 = amount * price
	UserID    int64       `json:"user_id"` // 挂单用户
	Status    OrderStatus `json:"status"`
	Note      string      `json:"note,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// 校验非法输入的错误。
var (
	ErrInvalidSide   = &OrderError{"invalid side, must be buy or sell"}
	ErrInvalidCoin   = &OrderError{"invalid coin"}
	ErrInvalidAmount = &OrderError{"invalid amount: must be > 0"}
	ErrInvalidPrice  = &OrderError{"invalid price: must be > 0"}
	ErrNotFound      = &OrderError{"order not found"}
	ErrBadTransition = &OrderError{"invalid status transition"}
)

// OrderError 是 C2C 领域错误。
type OrderError struct{ msg string }

func (e *OrderError) Error() string { return e.msg }

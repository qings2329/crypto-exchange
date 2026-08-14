package risk

import (
	"errors"
	"time"
)

// 风控规则类型。
const (
	KindWithdrawLimit = "withdraw_limit" // 单日提现限额（按资产）
	KindOrderLimit    = "order_limit"    // 单笔/单日下单数额限制
	KindPositionLimit = "position_limit" // 持仓限额
	KindFreqLimit     = "freq_limit"     // 频率（单日次数）限制
)

// 规则作用域。
const (
	ScopeGlobal = "global" // 对所有用户生效
	ScopeUser   = "user"   // 针对具体用户（结合 user_id）
)

// 黑名单类型。
const (
	BlacklistUser    = "user"
	BlacklistAddress = "address"
)

// ErrNotFound 表示记录不存在。
var ErrNotFound = errors.New("risk record not found")

// ErrRejected 表示风控拒绝（附带原因）。
var ErrRejected = errors.New("risk check rejected")

// RiskRule 是一条风控规则。
type RiskRule struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	Kind           string    `json:"kind"`
	Scope          string    `json:"scope"`
	UserID         int64     `json:"user_id"`       // Scope=user 时有效
	Asset          string    `json:"asset"`         // 空表示任意资产
	MaxAmountPerDay float64  `json:"max_amount_per_day"`
	MaxCountPerDay  int      `json:"max_count_per_day"`
	MinKYCLevel    int       `json:"min_kyc_level"`
	Enabled        bool      `json:"enabled"`
	CreatedAt      time.Time `json:"created_at"`
}

// BlacklistEntry 是黑名单条目。
type BlacklistEntry struct {
	ID        int64     `json:"id"`
	Target    string    `json:"target"` // user_id 字符串或链上地址
	Kind      string    `json:"kind"`   // user / address
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// RiskEvent 是一条风控触发记录（审计/排查）。
type RiskEvent struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	Kind      string    `json:"kind"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

// CheckResult 是风控评估结果。
type CheckResult struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

package lending

import "github.com/coldlar/crypto-exchange/internal/settlement"

// PoolStatus 借贷池状态。
type PoolStatus string

const (
	PoolActive  PoolStatus = "active"
	PoolPaused  PoolStatus = "paused"
	PoolClosed  PoolStatus = "closed"
)

// LendingPool 借贷资金池（每种资产一个池）。
type LendingPool struct {
	ID            int64                  `json:"id"`
	Asset         string                 `json:"asset"`          // 资产类型（USDT/ETH/BTC）
	TotalSupply   settlement.AssetAmount `json:"total_supply"`   // 总存款额
	TotalBorrow   settlement.AssetAmount `json:"total_borrow"`   // 总借款额
	Available     settlement.AssetAmount `json:"available"`      // 可借额度 = TotalSupply - TotalBorrow
	Utilization   float64                `json:"utilization"`    // 使用率 = TotalBorrow / TotalSupply
	InterestRate  float64                `json:"interest_rate"`  // 当前年化利率（按使用率动态调整）
	CollateralReq float64                `json:"collateral_req"` // 最低抵押率（如1.5 = 150%）
	Status        PoolStatus             `json:"status"`
	CreatedAt     int64                  `json:"created_at"`
}

// LendOrder 存款（出借）订单。
type LendOrder struct {
	ID        int64                  `json:"id"`
	UserID    int64                  `json:"user_id"`
	PoolID    int64                  `json:"pool_id"`
	Amount    settlement.AssetAmount `json:"amount"`     // 存款金额
	Rate      float64                `json:"rate"`       // 锁定时的利率快照
	Status    string                 `json:"status"`     // active / withdrawn
	CreatedAt int64                  `json:"created_at"`
}

// BorrowOrder 借款订单。
type BorrowOrder struct {
	ID          int64                  `json:"id"`
	UserID      int64                  `json:"user_id"`
	PoolID      int64                  `json:"pool_id"`
	Amount      settlement.AssetAmount `json:"amount"`       // 借款金额
	Collateral  settlement.AssetAmount `json:"collateral"`   // 抵押品金额（计价资产）
	Rate        float64                `json:"rate"`         // 锁定时的利率快照
	InterestAcc settlement.AssetAmount `json:"interest_acc"` // 已计利息
	Status      string                 `json:"status"`       // active / repaid / liquidated
	CreatedAt   int64                  `json:"created_at"`
	RepaidAt    int64                  `json:"repaid_at"`
}

// InterestRecord 利息归集记录。
type InterestRecord struct {
	ID        int64                  `json:"id"`
	PoolID    int64                  `json:"pool_id"`
	UserID    int64                  `json:"user_id"`
	Type      string                 `json:"type"`      // lend_earned / borrow_interest
	Amount    settlement.AssetAmount `json:"amount"`
	RecordedAt int64                 `json:"recorded_at"`
}

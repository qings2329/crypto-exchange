package staking

import (
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// 产品状态。
type ProductStatus string

const (
	ProductActive ProductStatus = "active"
	ProductClosed ProductStatus = "closed"
)

// 委托（质押单）状态。
type DelegationStatus string

const (
	// DelegationActive 质押中（本金锁定在链上，累积奖励）。
	DelegationActive DelegationStatus = "active"
	// DelegationUnbonding 已发起解质押，等待链上确认期（解锁排队）。
	DelegationUnbonding DelegationStatus = "unbonding"
	// DelegationUnbonded 解质押已确认，本金与奖励已释放回用户账本。
	DelegationUnbonded DelegationStatus = "unbonded"
)

// StakingProduct 是链上质押理财产品（对应一个验证人/节点池/质押合约）。
type StakingProduct struct {
	ID           int64                  `json:"id"`
	Name         string                `json:"name"`
	Chain        string                `json:"chain"`         // eth / tron
	Validator    string                `json:"validator"`     // 验证人/节点地址
	ContractAddr string                `json:"contract_addr"` // 质押合约地址
	Asset        string                `json:"asset"`         // 质押资产（如 ETH/TRX）
	AnnualRate   float64               `json:"annual_rate"`   // 预估年化（仅展示，真实奖励来自链上）
	DurationDays int                   `json:"duration_days"` // 锁仓天数（0=灵活）
	MinAmount    settlement.AssetAmount `json:"min_amount"`   // 起质押额（定点）
	Status       ProductStatus         `json:"status"`
	CreatedAt    int64                 `json:"created_at"`
}

// StakingDelegation 是用户的一笔质押委托。
type StakingDelegation struct {
	ID          int64                `json:"id"`
	UserID      int64                `json:"user_id"`
	ProductID   int64                `json:"product_id"`
	Principal   settlement.AssetAmount `json:"principal"`  // 质押本金（定点）
	Status      DelegationStatus     `json:"status"`
	TxHash      string              `json:"tx_hash"`   // 链上质押交易哈希
	CreatedAt   int64               `json:"created_at"`
	UnbondAt    int64               `json:"unbond_at"`   // 发起解质押时间（unix 秒，0=未解押）
	UnbondedAt  int64               `json:"unbonded_at"` // 链上确认解质押时间（unix 秒，0=未确认）
}

// StakingReward 是一笔已归集到用户账本的链上质押奖励。
type StakingReward struct {
	ID           int64                `json:"id"`
	DelegationID int64                `json:"delegation_id"`
	Amount       settlement.AssetAmount `json:"amount"` // 奖励额（定点）
	AccruedAt    int64               `json:"accrued_at"`
}

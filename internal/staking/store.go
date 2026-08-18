package staking

// Store 是质押业务的数据持久化接口（MySQL 优先，失败回退内存）。
type Store interface {
	// 产品
	CreateProduct(p *StakingProduct) error
	GetProduct(id int64) (*StakingProduct, error)
	ListProducts(status ProductStatus) ([]*StakingProduct, error)
	CloseProduct(id int64) error

	// 委托
	CreateDelegation(d *StakingDelegation) error
	GetDelegation(id int64) (*StakingDelegation, error)
	ListDelegationsByUser(uid int64) ([]*StakingDelegation, error)
	ListAllDelegations() ([]*StakingDelegation, error)
	UpdateDelegation(d *StakingDelegation) error
	DeleteDelegation(id int64) error

	// 奖励
	CreateReward(r *StakingReward) error
	ListRewardsByDelegation(did int64) ([]*StakingReward, error)
}

package lending

// Store 是借贷业务的数据持久化接口（MySQL 优先，失败回退内存）。
type Store interface {
	// 资金池
	CreatePool(p *LendingPool) error
	GetPool(id int64) (*LendingPool, error)
	GetPoolByAsset(asset string) (*LendingPool, error)
	ListPools(status PoolStatus) ([]*LendingPool, error)
	UpdatePool(p *LendingPool) error

	// 存款订单
	CreateLendOrder(o *LendOrder) error
	GetLendOrder(id int64) (*LendOrder, error)
	ListLendOrdersByUser(uid int64) ([]*LendOrder, error)
	ListLendOrdersByPool(pid int64) ([]*LendOrder, error)
	ListAllLendOrders() ([]*LendOrder, error)
	UpdateLendOrder(o *LendOrder) error

	// 借款订单
	CreateBorrowOrder(o *BorrowOrder) error
	GetBorrowOrder(id int64) (*BorrowOrder, error)
	ListBorrowOrdersByUser(uid int64) ([]*BorrowOrder, error)
	ListBorrowOrdersByPool(pid int64) ([]*BorrowOrder, error)
	ListAllBorrowOrders() ([]*BorrowOrder, error)
	ListActiveBorrowOrders() ([]*BorrowOrder, error)
	UpdateBorrowOrder(o *BorrowOrder) error

	// 利息记录
	CreateInterestRecord(r *InterestRecord) error
	ListInterestRecordsByUser(uid int64) ([]*InterestRecord, error)
}

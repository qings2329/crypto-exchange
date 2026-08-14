package options

// Store 是期权业务（合约 + 持仓）的持久化抽象。Service 只依赖该接口，不关心底层是内存还是 MySQL。
type Store interface {
	// --- 合约 ---
	// CreateContract 写入一条合约（store 内部负责分配自增 ID）。
	CreateContract(c *OptionContract) error
	GetContract(id int64) (*OptionContract, error)
	ListContracts() ([]*OptionContract, error)

	// --- 持仓 ---
	// UpsertPosition 写入或更新一条持仓（按 id 唯一）。
	UpsertPosition(p *OptionPosition) error
	GetPosition(id int64) (*OptionPosition, error)
	ListPositions(userID int64) ([]*OptionPosition, error)
	ListAllPositions() ([]*OptionPosition, error)
	ListAllOpen() ([]*OptionPosition, error)
	DeletePosition(id int64) error
}

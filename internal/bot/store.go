package bot

// Store 是机器人策略与订单的持久化接口（MySQL 优先，失败回退内存）。
type Store interface {
	CreateStrategy(s *BotStrategy) error
	GetStrategy(id int64) (*BotStrategy, error)
	ListStrategiesByUser(uid int64) ([]*BotStrategy, error)
	ListActiveStrategies() ([]*BotStrategy, error)
	ListAllStrategies() ([]*BotStrategy, error)
	UpdateStrategy(s *BotStrategy) error

	CreateOrder(o *BotOrder) error
	ListOrdersByStrategy(sid int64) ([]*BotOrder, error)
	CountOrdersByStrategy(sid int64) (int64, error)
}

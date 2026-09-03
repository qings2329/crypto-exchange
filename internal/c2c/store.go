package c2c

// Store 是 C2C 订单存储接口。实现：内存（开发/测试降级）与 MySQL。
type Store interface {
	// Create 落库一条订单，并回填 ID / CreatedAt / UpdatedAt。
	Create(o *Order) error
	// List 按过滤条件分页查询（user_id/side/coin/status 为空表示不过滤）。
	// 返回当前页与总条数。
	List(filter OrderFilter, limit, offset int) ([]*Order, int, error)
	// Get 按 ID 查订单。
	Get(id int64) (*Order, error)
	// UpdateStatus 原子地更新订单状态（乐观锁：仅当当前状态为 from 时更新到 to）。
	// 返回是否成功（不成功表示状态已被并发修改，由调用方重读）。
	UpdateStatus(id int64, from, to OrderStatus) (bool, error)
	// Update 用 provided 字段更新订单（用于完成时回写）。
	Update(o *Order) error
}

// OrderFilter 是后台/用户查询过滤条件。
type OrderFilter struct {
	UserID int64
	Side   Side
	Coin   string
	Status OrderStatus
}

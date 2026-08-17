package wealth

// Store 是理财资管业务（产品 + 持仓）的持久化抽象。Service 只依赖该接口，不关心底层是内存还是 MySQL。
type Store interface {
	// --- 产品 ---
	CreateProduct(p *WealthProduct) error
	GetProduct(id int64) (*WealthProduct, error)
	ListProducts(status ProductStatus) ([]*WealthProduct, error)
	UpdateProduct(p *WealthProduct) error

	// --- 持仓 ---
	CreateHolding(h *WealthHolding) error
	GetHolding(id int64) (*WealthHolding, error)
	UpdateHolding(h *WealthHolding) error
	DeleteHolding(id int64) error
	ListHoldings(userID int64) ([]*WealthHolding, error)
	ListAllHoldings() ([]*WealthHolding, error)
}

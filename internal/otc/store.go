package otc

// Store 是 OTC 业务（广告 + 订单 + 对手方信用）的持久化抽象。Service 只依赖该接口，不关心底层是内存还是 MySQL。
type Store interface {
	// --- 广告 ---
	CreateAd(a *OtcAdvertisement) error
	GetAd(id int64) (*OtcAdvertisement, error)
	ListAds(side AdSide, asset string) ([]*OtcAdvertisement, error)
	UpdateAd(a *OtcAdvertisement) error

	// --- 订单 ---
	CreateOrder(o *OtcOrder) error
	GetOrder(id int64) (*OtcOrder, error)
	UpdateOrder(o *OtcOrder) error
	ListOrders(userID int64) ([]*OtcOrder, error)
	ListAllOrders() ([]*OtcOrder, error)

	// --- 对手方信用 ---
	UpsertCounterparty(cp *OtcCounterparty) error
	GetCounterparty(userID, counterpartyID int64) (*OtcCounterparty, error)
	ListCounterparties(userID int64) ([]*OtcCounterparty, error)
}

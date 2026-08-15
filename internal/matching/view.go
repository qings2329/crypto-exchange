package matching

// 本文件定义订单管理模块对外暴露的只读视图（OrderView / TradeView）与状态枚举。
// 这些 DTO 不持有引擎内部结构（*Order / 锁），便于 HTTP 序列化与跨服务传输。

// OrderStatus 是订单生命周期状态（字符串形态，供 JSON / 配置使用）。
type OrderStatus string

const (
	OrderOpen     OrderStatus = "open"     // 挂单中（可能部分成交）
	OrderPartial  OrderStatus = "partial"  // 部分成交、剩余仍挂单
	OrderFilled   OrderStatus = "filled"   // 完全成交
	OrderCanceled OrderStatus = "canceled" // 已撤销（用户撤单或系统移除）
	OrderRejected OrderStatus = "rejected" // 未成交被拒（IOC/FOK 流动性不足整笔丢弃）
)

// OrderView 一笔订单的只读视图。
type OrderView struct {
	ID          int64       `json:"id"`
	UserID      int64       `json:"user_id"`
	Symbol      string      `json:"symbol"`
	Market      string      `json:"market"` // spot | futures
	Side        string      `json:"side"`   // buy | sell
	Price       float64     `json:"price"`  // 0 表示市价单
	Qty         float64     `json:"qty"`
	Filled      float64     `json:"filled"`
	Status      OrderStatus `json:"status"`
	TimeInForce string      `json:"time_in_force,omitempty"`
	CreatedAt   int64       `json:"created_at"`
	UpdatedAt   int64       `json:"updated_at"`
}

// TradeView 一笔成交的只读视图（买卖双边用户均可见，对应其各自历史）。
type TradeView struct {
	ID        int64   `json:"id"`         // 全局成交序号
	Symbol    string  `json:"symbol"`
	Market    string  `json:"market"`     // spot | futures
	Price     float64 `json:"price"`
	Qty       float64 `json:"qty"`
	TakerID   int64   `json:"taker_id"`
	MakerID   int64   `json:"maker_id"`
	TakerSide string  `json:"taker_side"` // buy | sell
	TakerOID  int64   `json:"taker_oid"`
	MakerOID  int64   `json:"maker_oid"`
	Time      int64   `json:"time"`
}

// sideString 把 Side 转为可读字符串。
func sideString(s Side) string {
	if s == Buy {
		return "buy"
	}
	return "sell"
}

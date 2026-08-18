package bot

// Market 交易场所。
type Market string

const (
	MarketSpot    Market = "spot"
	MarketFutures Market = "futures"
)

// StrategyType 策略类型。
type StrategyType string

const (
	StrategyGrid StrategyType = "grid" // 网格
	StrategyDCA  StrategyType = "dca"  // 定投
	StrategyMA   StrategyType = "ma"   // 均线
)

// StrategyStatus 策略状态。
type StrategyStatus string

const (
	StrategyActive  StrategyStatus = "active"
	StrategyStopped StrategyStatus = "stopped"
)

// BotParams 是策略参数（按类型选用不同字段），以 JSON 存入 store。
type BotParams struct {
	// 网格
	GridLower float64 `json:"grid_lower"` // 区间下沿
	GridUpper float64 `json:"grid_upper"` // 区间上沿
	GridNum   int     `json:"grid_num"`   // 网格数
	GridStep  float64 `json:"grid_step"`  // 触发步长（市价偏离）
	// 定投
	DCAIntervalSec int     `json:"dca_interval_sec"` // 定投间隔（秒）
	DCAAmount      float64 `json:"dca_amount"`       // 每期投入额
	// 均线
	MAShort int `json:"ma_short"` // 短周期
	MALong  int `json:"ma_long"`  // 长周期
	// 通用
	MaxPosition float64 `json:"max_position"` // 最大持仓额（资产本位，防超仓）
	OrderAmount float64 `json:"order_amount"` // 单笔下单额
}

// BotStrategy 是用户的一条交易机器人策略。
type BotStrategy struct {
	ID     int64  `json:"id"`
	UserID int64  `json:"user_id"`
	Name   string `json:"name"`
	Market Market `json:"market"`
	Symbol string `json:"symbol"`
	Side   string `json:"side"` // buy / sell
	Type   StrategyType `json:"type"`
	Params BotParams    `json:"params"`
	Status StrategyStatus `json:"status"`
	// UserToken 是用户授权 bot 代其下单的凭证（不序列化到外部响应）。
	UserToken string `json:"-"`
	CreatedAt int64  `json:"created_at"`
}

// BotOrder 是 bot 代用户下达的一笔订单记录。
type BotOrder struct {
	ID             int64  `json:"id"`
	StrategyID     int64  `json:"strategy_id"`
	UserID         int64  `json:"user_id"`
	Market         Market `json:"market"`
	Symbol         string `json:"symbol"`
	Side           string `json:"side"`
	Price          float64 `json:"price"`
	Qty            float64 `json:"qty"`
	ClientOID      string `json:"client_oid"`       // 幂等键（spot/futures 后端 F1 去重）
	ExchangeOrderID string `json:"exchange_order_id"` // 交易所/撮合返回的订单号
	Status         string `json:"status"`
	CreatedAt      int64  `json:"created_at"`
}

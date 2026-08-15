package matching

// Matcher 是撮合能力的抽象。它同时被「进程内撮合引擎 *Engine」与「远程撮合服务的
// HTTP/WS 客户端 *client.Client」满足，使上游（spot/futuresapi）既能直接持有引擎（单测），
// 也能改为调用独立的 cmd/matching 服务（多实例单写者部署），无需改动业务逻辑。
//
// 设计动机：原 spot/futures 各自 new 一份 matching.Engine，部署多实例时订单簿分裂、
// order_id 冲突。把匹配收敛为单一 cmd/matching 服务后，spot/futures 退化为无状态客户端，
// 仅依赖 Matcher 接口。见 DEVELOPMENT_TASKS §17/§18。
type Matcher interface {
	// Submit 提交一笔订单（异步入队）；返回 false 表示交易对未注册。
	Submit(symbol string, o *Order) bool
	// Depth 返回某交易对深度快照；ok=false 表示交易对未注册。
	Depth(symbol string) (bids, asks []Level, ok bool)
	// MatchNow 同步撮合一笔订单并返回成交列表与是否完全成交（用于强平）。
	MatchNow(symbol string, o *Order, rest bool) ([]Trade, bool)
	// Cancel 撤销一笔订单；返回是否成功撤销（false 表示订单不存在或已成交/已撤）。
	Cancel(symbol string, orderID int64) bool
	// ListOrders 返回指定用户的订单（按 user_id 过滤；symbol/status 为空表示不过滤；
	// limit<=0 表示不限制）。远程 *client.Client 经 cmd/matching /orders 实现。
	ListOrders(userID int64, symbol, status string, limit int) []OrderView
	// GetOrder 按订单 ID 返回详情；不存在返回 (OrderView{}, false)。
	GetOrder(orderID int64) (OrderView, bool)
	// ListTrades 返回指定用户的成交流水（按 user_id；symbol 为空表示不过滤；
	// limit<=0 表示不限制）。远程 *client.Client 经 cmd/matching /trades 实现。
	ListTrades(userID int64, symbol string, limit int) []TradeView
}

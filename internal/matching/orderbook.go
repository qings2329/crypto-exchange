package matching

import (
	"sort"
	"sync"
)

// Side 买卖方向。
type Side int

const (
	Buy Side = iota
	Sell
)

// Order 是撮合引擎中的一笔委托。
type Order struct {
	ID     int64
	UserID int64
	Side   Side
	Price  float64 // 市价单 Price=0
	Qty    float64
	Filled float64
	Time   int64 // 时间戳，用于时间优先
}

// IsFilled 是否完全成交。
func (o *Order) IsFilled() bool { return o.Filled >= o.Qty }

// Remaining 剩余未成交量。
func (o *Order) Remaining() float64 { return o.Qty - o.Filled }

// Level 某一价格档的订单队列。
type Level struct {
	Price  float64
	Orders []*Order
}

// Trade 成交回报。
type Trade struct {
	Price     float64
	Qty       float64
	TakerID   int64 // 吃单用户 ID（资金记账溯源）
	MakerID   int64 // 挂单用户 ID
	TakerSide Side
	// TakerOID/MakerOID 为对应订单 ID，便于上游按订单维度释放预冻结资金；
	// 与 TakerID/MakerID（用户维度）互补，均用于审计与对账。
	TakerOID int64 `json:"taker_oid"`
	MakerOID int64 `json:"maker_oid"`
}

// BookState 是单个交易对订单簿的可序列化快照，用于持久化与恢复。
type BookState struct {
	Symbol string  `json:"symbol"`
	Bids   []Level `json:"bids"`
	Asks   []Level `json:"asks"`
}

// OrderBook 单个交易对的内存订单簿。
// 注意：骨架用 map + 每次排序实现价格优先，生产环境应替换为红黑树/跳表以保证 O(log n)。
type OrderBook struct {
	symbol string
	mu     sync.RWMutex
	bids   map[float64]*Level // 买盘
	asks   map[float64]*Level // 卖盘
}

// NewOrderBook 创建订单簿。
func NewOrderBook(symbol string) *OrderBook {
	return &OrderBook{
		symbol: symbol,
		bids:   make(map[float64]*Level),
		asks:   make(map[float64]*Level),
	}
}

// sortedBidPrices 买盘价格降序。
func (ob *OrderBook) sortedBidPrices() []float64 {
	prices := make([]float64, 0, len(ob.bids))
	for p := range ob.bids {
		prices = append(prices, p)
	}
	sort.Slice(prices, func(i, j int) bool { return prices[i] > prices[j] })
	return prices
}

// sortedAskPrices 卖盘价格升序。
func (ob *OrderBook) sortedAskPrices() []float64 {
	prices := make([]float64, 0, len(ob.asks))
	for p := range ob.asks {
		prices = append(prices, p)
	}
	sort.Slice(prices, func(i, j int) bool { return prices[i] < prices[j] })
	return prices
}

// Match 撮合一笔新订单，返回成交列表；未成交部分保留在订单簿。
// 规则：价格优先 + 时间优先。等价于 MatchRest(in, true)。
func (ob *OrderBook) Match(in *Order) []Trade {
	return ob.MatchRest(in, true)
}

// MatchRest 撮合一笔新订单，返回成交列表。
// rest=true（默认 Match 行为）：未成交部分挂单到订单簿（限价单正常挂单）。
// rest=false：市价单（Price=0）未成交部分直接丢弃、不挂单——用于强平场景，
// 避免空流动性时残留 price=0 挂单污染订单簿（剩余由保险基金兜底成交）。
func (ob *OrderBook) MatchRest(in *Order, rest bool) []Trade {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	var trades []Trade
	opposite := ob.asks
	own := ob.bids
	bestPrices := ob.sortedAskPrices()
	if in.Side == Sell {
		opposite = ob.bids
		own = ob.asks
		bestPrices = ob.sortedBidPrices()
	}

	for _, p := range bestPrices {
		if in.IsFilled() {
			break
		}
		// 价格不满足则停止（限价单）
		if in.Price > 0 {
			if in.Side == Buy && p > in.Price {
				break
			}
			if in.Side == Sell && p < in.Price {
				break
			}
		}
		level := opposite[p]
		for len(level.Orders) > 0 && !in.IsFilled() {
			maker := level.Orders[0]
			qty := in.Remaining()
			if maker.Remaining() < qty {
				qty = maker.Remaining()
			}
			maker.Filled += qty
			in.Filled += qty
		trades = append(trades, Trade{
			Price:     p,
			Qty:      qty,
			TakerID:   in.UserID,
			MakerID:   maker.UserID,
			TakerSide: in.Side,
			TakerOID:  in.ID,
			MakerOID:  maker.ID,
		})
			if maker.IsFilled() {
				level.Orders = level.Orders[1:]
			}
		}
		if len(level.Orders) == 0 {
			delete(opposite, p)
		}
	}

	// 未完全成交：挂单（rest=true 时；市价单在 rest=false 下不挂单）。
	if !in.IsFilled() && rest && in.Price > 0 {
		book := own
		level, ok := book[in.Price]
		if !ok {
			level = &Level{Price: in.Price}
			book[in.Price] = level
		}
		level.Orders = append(level.Orders, in)
	}
	return trades
}

// Depth 返回买卖盘深度快照（用于行情推送）。
func (ob *OrderBook) Depth() (bids, asks []Level) {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	for _, p := range ob.sortedBidPrices() {
		bids = append(bids, *ob.bids[p])
	}
	for _, p := range ob.sortedAskPrices() {
		asks = append(asks, *ob.asks[p])
	}
	return
}

// Snapshot 返回订单簿的可序列化快照（深拷贝，避免外部修改影响内部状态）。
func (ob *OrderBook) Snapshot() BookState {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	clone := func(m map[float64]*Level) []Level {
		out := make([]Level, 0, len(m))
		for _, lvl := range m {
			c := Level{Price: lvl.Price, Orders: make([]*Order, len(lvl.Orders))}
			copy(c.Orders, lvl.Orders)
			out = append(out, c)
		}
		return out
	}
	return BookState{Symbol: ob.symbol, Bids: clone(ob.bids), Asks: clone(ob.asks)}
}

// Restore 用快照整体覆盖当前订单簿（恢复路径使用）。
func (ob *OrderBook) Restore(s BookState) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	ob.symbol = s.Symbol
	ob.bids = make(map[float64]*Level, len(s.Bids))
	ob.asks = make(map[float64]*Level, len(s.Asks))
	for _, lvl := range s.Bids {
		ords := make([]*Order, len(lvl.Orders))
		copy(ords, lvl.Orders)
		ob.bids[lvl.Price] = &Level{Price: lvl.Price, Orders: ords}
	}
	for _, lvl := range s.Asks {
		ords := make([]*Order, len(lvl.Orders))
		copy(ords, lvl.Orders)
		ob.asks[lvl.Price] = &Level{Price: lvl.Price, Orders: ords}
	}
}

// Cancel 从订单簿中撤销指定 ID 的订单；找到并移除返回 true，否则 false。
func (ob *OrderBook) Cancel(orderID int64) bool {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	sides := []map[float64]*Level{ob.bids, ob.asks}
	for _, book := range sides {
		for p, lvl := range book {
			for i, o := range lvl.Orders {
				if o.ID == orderID {
					lvl.Orders = append(lvl.Orders[:i], lvl.Orders[i+1:]...)
					if len(lvl.Orders) == 0 {
						delete(book, p)
					}
					return true
				}
			}
		}
	}
	return false
}

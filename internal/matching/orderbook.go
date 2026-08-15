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

// 时间约束力（Time-In-Force）。
const (
	TIFGTC = "GTC" // 默认：未成交部分挂单，直到成交或撤单
	TIFIOC = "IOC" // 立即成交否则撤销：尽量撮合，剩余部分直接撤销、不挂单
	TIFFOK = "FOK" // 全部成交否则撤销：对手盘流动性不足以完全成交时整笔撤销
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

	// 高级订单属性（原型支持，默认零值表示普通限价/GTC）。
	TimeInForce string  // GTC/IOC/FOK，空按 GTC 处理
	StopPrice   float64 // >0 表示止盈止损单：成交价穿越该触发价后才激活为普通单
	StopLimit   float64 // 仅 stop-limit 用：激活后作为限价单的挂单价；0 表示激活为市价单

	// Market 标记订单来源市场（"spot" | "futures"），由上游在下单时写入，
	// 用于订单管理模块按交易类型区分同一撮合引擎内的订单（现货/合约共用同一登记簿）。
	Market string
	// IsMargin 标记该单是否为杠杆单（现货杠杆单与合约单均为 true，普通现货单为 false），
	// 用于订单管理模块按"是否杠杆"过滤。Leverage 为杠杆倍数（无杠杆时 0）。
	IsMargin bool
	Leverage float64
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
	stops  []*Order           // 未触发的止盈止损单：最近成交价穿越触发价后激活为普通单
	last   float64            // 最近成交价，止盈止损触发判定的参考价
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
//
// 时间约束力（仅对 taker 生效）：
//   - IOC：忽略 rest，尽量撮合、剩余直接撤销。
//   - FOK：先校验对手盘可用流动性是否足以完全成交；不足则整笔撤销、不撮合。
func (ob *OrderBook) MatchRest(in *Order, rest bool) []Trade {
	ob.mu.Lock()
	defer ob.mu.Unlock()

	// 止盈止损单：成交价穿越触发价前不入簿，挂起到 stops 等待激活。
	if in.StopPrice > 0 {
		if !ob.stopTriggered(in) {
			ob.stops = append(ob.stops, in)
			return nil
		}
		in = ob.activateStop(in)
	}

	trades := ob.matchCore(in, rest)

	// 更新最近成交价，并激活穿越触发价的止盈止损单；循环直到无新触发，避免连锁遗漏。
	if len(trades) > 0 {
		ob.last = trades[len(trades)-1].Price
	}
	for {
		triggered := ob.drainStops()
		if len(triggered) == 0 {
			break
		}
		trades = append(trades, triggered...)
		if len(triggered) > 0 {
			ob.last = triggered[len(triggered)-1].Price
		}
	}
	return trades
}

// canFullyFill 调用方必须已持 ob.mu。估算对手盘可用流动性是否足以完全成交 in。
// 市价单（Price=0）统计全部对手盘；限价单仅统计可成交档位（满足价格优先）。
func (ob *OrderBook) canFullyFill(in *Order) bool {
	var available float64
	opposite := ob.asks
	bestPrices := ob.sortedAskPrices()
	if in.Side == Sell {
		opposite = ob.bids
		bestPrices = ob.sortedBidPrices()
	}
	for _, p := range bestPrices {
		if in.Price > 0 {
			// 限价单：仅统计可成交档位。
			if in.Side == Buy && p > in.Price {
				break
			}
			if in.Side == Sell && p < in.Price {
				break
			}
		}
		level := opposite[p]
		for _, o := range level.Orders {
			available += o.Remaining()
		}
	}
	return available >= in.Qty
}

// stopTriggered 调用方必须已持 ob.mu。依据最近成交价判定止盈止损单是否触发。
//   - 买止损（StopPrice>0，无成交价前不触发）：last >= StopPrice 时触发（价格上涨触及）。
//   - 卖止损：last <= StopPrice 时触发（价格下跌触及）。
func (ob *OrderBook) stopTriggered(in *Order) bool {
	if ob.last == 0 {
		return false // 尚无成交价可参考
	}
	if in.Side == Buy {
		return ob.last >= in.StopPrice
	}
	return ob.last <= in.StopPrice
}

// activateStop 返回激活形态的订单副本（不修改入参，保留原 ID 以便记账对账）：
// StopLimit>0 激活为限价单（挂单价=StopLimit），否则激活为市价单（Price=0）。
func (ob *OrderBook) activateStop(in *Order) *Order {
	act := *in
	act.StopPrice = 0
	if in.StopLimit > 0 {
		act.Price = in.StopLimit
	} else {
		act.Price = 0
	}
	return &act
}

// drainStops 调用方必须已持 ob.mu。激活所有已触发止盈止损单并撮合，返回其成交流。
func (ob *OrderBook) drainStops() []Trade {
	var out []Trade
	kept := ob.stops[:0]
	for _, s := range ob.stops {
		if ob.stopTriggered(s) {
			out = append(out, ob.matchCore(ob.activateStop(s), true)...)
		} else {
			kept = append(kept, s)
		}
	}
	ob.stops = kept
	return out
}

// matchCore 不含止盈止损分支的核心撮合；调用方必须已持 ob.mu。
func (ob *OrderBook) matchCore(in *Order, rest bool) []Trade {
	switch in.TimeInForce {
	case TIFIOC:
		rest = false
	case TIFFOK:
		if !ob.canFullyFill(in) {
			return nil
		}
		rest = false
	}
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

// SetLast 更新最近成交价（如行情/标记价喂价），并激活穿越触发价的止盈止损单。
// 返回被激活订单的成交流；引擎可据此回调 onTrade 对外发布。
func (ob *OrderBook) SetLast(price float64) []Trade {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	if price > 0 {
		ob.last = price
	}
	var trades []Trade
	for {
		triggered := ob.drainStops()
		if len(triggered) == 0 {
			break
		}
		trades = append(trades, triggered...)
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

// Contains 回报订单簿中是否挂有指定订单 ID（用于判定 taker 是否留挂单）。
func (ob *OrderBook) Contains(orderID int64) bool {
	ob.mu.RLock()
	defer ob.mu.RUnlock()
	for _, lvl := range ob.bids {
		for _, o := range lvl.Orders {
			if o.ID == orderID {
				return true
			}
		}
	}
	for _, lvl := range ob.asks {
		for _, o := range lvl.Orders {
			if o.ID == orderID {
				return true
			}
		}
	}
	return false
}

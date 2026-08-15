package matching

import "testing"

// 以下 helper 仅构造订单，不触碰 TimeInForce / StopPrice / StopLimit。
func buy(o *Order, qty, price float64, uid int64) *Order {
	o.Side = Buy
	o.Qty = qty
	o.Price = price
	o.UserID = uid
	return o
}

func sell(o *Order, qty, price float64, uid int64) *Order {
	o.Side = Sell
	o.Qty = qty
	o.Price = price
	o.UserID = uid
	return o
}

// TestMatchRestIOC：IOC 尽量撮合，剩余直接撤销、不入簿。
func TestMatchRestIOC(t *testing.T) {
	ob := NewOrderBook("TEST")
	ob.Match(sell(&Order{ID: 1, UserID: 1}, 5, 100, 1)) // 挂卖 @100 x5

	tr := ob.Match(buy(&Order{ID: 2, UserID: 2, TimeInForce: TIFIOC}, 10, 0, 2))
	var filled float64
	for _, x := range tr {
		filled += x.Qty
	}
	if filled != 5 {
		t.Fatalf("IOC 应仅成交对手盘 5，got %v", filled)
	}
	if _, asks := ob.Depth(); len(asks) != 0 {
		t.Fatalf("IOC 剩余部分应撤销、不入簿，asks=%v", asks)
	}
}

// TestMatchRestFOK：FOK 不足整笔撤销、无成交、不入簿；充足则全成。
func TestMatchRestFOK(t *testing.T) {
	ob := NewOrderBook("TEST")
	ob.Match(sell(&Order{ID: 1, UserID: 1}, 5, 100, 1)) // 挂卖 @100 x5

	// 流动性不足：整笔撤销
	tr := ob.Match(buy(&Order{ID: 2, UserID: 2, TimeInForce: TIFFOK}, 10, 100, 2))
	if len(tr) != 0 {
		t.Fatalf("FOK 不足应无成交，got %v", tr)
	}
	if _, asks := ob.Depth(); len(asks) != 1 {
		t.Fatalf("FOK 不足不应入簿，asks=%v", asks)
	}

	// 流动性充足：全成
	tr = ob.Match(buy(&Order{ID: 3, UserID: 3, TimeInForce: TIFFOK}, 5, 100, 3))
	var filled float64
	for _, x := range tr {
		filled += x.Qty
	}
	if filled != 5 {
		t.Fatalf("FOK 充足应全成，got %v", filled)
	}
}

// TestStopOrderBuyTriggers：买止损单在成交价穿越触发价后激活为市价单成交。
func TestStopOrderBuyTriggers(t *testing.T) {
	ob := NewOrderBook("TEST")
	ob.Match(sell(&Order{ID: 1, UserID: 1}, 10, 110, 1)) // 挂卖 @110 x10

	stop := &Order{ID: 999, UserID: 2, Side: Buy, Qty: 3, StopPrice: 105}
	tr := ob.Match(stop) // 尚无成交价，应挂起不成交
	if len(tr) != 0 {
		t.Fatalf("止损未触发前不应成交，got %v", tr)
	}
	if len(ob.stops) != 1 {
		t.Fatalf("止损应挂起等待，stops=%d", len(ob.stops))
	}

	// 市价买 @110 x3 产生成交，last=110 → 激活止损（市价买 x3）
	tr = ob.Match(buy(&Order{ID: 2, UserID: 2}, 3, 110, 2))
	sawStop := false
	for _, x := range tr {
		if x.TakerOID == 999 {
			sawStop = true
		}
	}
	if !sawStop {
		t.Fatalf("止损激活后应产生 TakerOID=999 的成交，trades=%v", tr)
	}
	if len(ob.stops) != 0 {
		t.Fatalf("止损激活后应移除，stops=%d", len(ob.stops))
	}
}

// TestStopLimitActivatesAsLimit：stop-limit 激活为限价单，无法穿越则挂买盘。
func TestStopLimitActivatesAsLimit(t *testing.T) {
	ob := NewOrderBook("TEST")
	ob.Match(sell(&Order{ID: 1, UserID: 1}, 10, 110, 1)) // 挂卖 @110 x10

	stop := &Order{ID: 7, UserID: 2, Side: Buy, Qty: 3, StopPrice: 105, StopLimit: 108}
	ob.Match(stop) // 挂起
	// 市价买 @110 x2 触发 last=110，止损激活为限价买 @108（<110 无法成交 → 挂买盘）
	ob.Match(buy(&Order{ID: 2, UserID: 2}, 2, 110, 2))

	bids, _ := ob.Depth()
	if len(bids) != 1 || bids[0].Price != 108 {
		t.Fatalf("止损激活后限价单应挂买盘@108，bids=%v", bids)
	}
	if len(ob.stops) != 0 {
		t.Fatalf("止损激活后应移除，stops=%d", len(ob.stops))
	}
}

// TestCanFullyFill：市价单统计全部对手盘，限价单仅统计可成交档位。
func TestCanFullyFill(t *testing.T) {
	ob := NewOrderBook("TEST")
	ob.Match(sell(&Order{ID: 1}, 5, 100, 1))
	ob.Match(sell(&Order{ID: 2}, 5, 101, 1))

	if !ob.canFullyFill(&Order{Side: Buy, Qty: 10, Price: 0}) {
		t.Fatal("市价买 x10 应可全成")
	}
	if ob.canFullyFill(&Order{Side: Buy, Qty: 10, Price: 100}) {
		t.Fatal("限价买@100 x10 仅 5 可成")
	}
	if ob.canFullyFill(&Order{Side: Buy, Qty: 6, Price: 100}) {
		t.Fatal("限价买@100 x6 仅 5 可成")
	}
	if !ob.canFullyFill(&Order{Side: Buy, Qty: 5, Price: 100}) {
		t.Fatal("限价买@100 x5 应可全成")
	}
}

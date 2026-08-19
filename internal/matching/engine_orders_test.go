package matching

import (
	"testing"
)

// TestEngineOrderRegistry 验证撮合引擎订单/成交登记簿：注册、按用户/状态/symbol 过滤、
// market 标记、详情查询与撤销后状态。
func TestEngineOrderRegistry(t *testing.T) {
	e := NewEngine(nil, nil)
	e.Register("BTC_USDT")

	// 用户1的现货限价买（挂单，未成交）。
	if _, _ = e.MatchNow("BTC_USDT", &Order{
		ID: 1, UserID: 1, Side: Buy, Price: fxPrice(100), Qty: fxQty(1), Time: 1, Market: "spot",
	}, true); false {
	}
	// 用户2的合约市价卖，吃掉上方挂单，双方完全成交（合约=杠杆单）。
	if _, _ = e.MatchNow("BTC_USDT", &Order{
		ID: 2, UserID: 2, Side: Sell, Price: Fixed{}, Qty: fxQty(1), Time: 2, Market: "futures", IsMargin: true, Leverage: 10,
	}, false); false {
	}
	// 用户3的现货杠杆买（借币后下的现货杠杆单）。
	if _, _ = e.MatchNow("BTC_USDT", &Order{
		ID: 3, UserID: 3, Side: Buy, Price: fxPrice(50), Qty: fxQty(1), Time: 3, Market: "spot", IsMargin: true, Leverage: 3,
	}, true); false {
	}

	// 按用户过滤 + market 标记。
	os1 := e.ListOrders(1, "", "", 0)
	if len(os1) != 1 || os1[0].Status != OrderFilled || os1[0].Market != "spot" {
		t.Fatalf("user1 orders wrong: %+v", os1)
	}
	os2 := e.ListOrders(2, "", "", 0)
	if len(os2) != 1 || os2[0].Market != "futures" {
		t.Fatalf("user2 orders wrong: %+v", os2)
	}
	// 管理员查全部。
	if all := e.ListOrders(0, "", "", 0); len(all) != 3 {
		t.Fatalf("all orders should be 3, got %d", len(all))
	}
	// 状态过滤。
	if filled := e.ListOrders(0, "", string(OrderFilled), 0); len(filled) != 2 {
		t.Fatalf("filled orders should be 2, got %d", len(filled))
	}
	if open := e.ListOrders(0, "", string(OrderOpen), 0); len(open) != 1 {
		t.Fatalf("open orders should be 1, got %d", len(open))
	}
	// 杠杆维度：现货杠杆单(订单3)与合约单(订单2)为杠杆，普通现货(订单1)非杠杆。
	var marginOrders, plainOrders []OrderView
	for _, v := range e.ListOrders(0, "", "", 0) {
		if v.IsMargin {
			marginOrders = append(marginOrders, v)
		} else {
			plainOrders = append(plainOrders, v)
		}
	}
	if len(marginOrders) != 2 {
		t.Fatalf("margin orders should be 2, got %d", len(marginOrders))
	}
	if len(plainOrders) != 1 {
		t.Fatalf("plain orders should be 1, got %d", len(plainOrders))
	}
	if !plainOrders[0].MarginMatches("") || !plainOrders[0].MarginMatches("all") {
		t.Fatal("MarginMatches(\"\")/(\"all\") should pass")
	}
	if plainOrders[0].MarginMatches("1") || plainOrders[0].MarginMatches("margin") {
		t.Fatal("plain order should not match margin filter")
	}
	if marginOrders[0].MarginMatches("0") {
		t.Fatal("margin order should not match margin=0")
	}
	// symbol 过滤。
	if other := e.ListOrders(0, "ETH_USDT", "", 0); len(other) != 0 {
		t.Fatalf("ETH_USDT orders should be 0, got %d", len(other))
	}
	// limit 截断（全部2条，limit=1）。
	if limited := e.ListOrders(0, "", "", 1); len(limited) != 1 {
		t.Fatalf("limit=1 should return 1, got %d", len(limited))
	}

	// 详情与 market 标记。
	if v, ok := e.GetOrder(1); !ok || v.UserID != 1 || v.Market != "spot" {
		t.Fatalf("GetOrder(1) wrong: ok=%v v=%+v", ok, v)
	}
	if _, ok := e.GetOrder(999); ok {
		t.Fatal("GetOrder(999) should be missing")
	}

	// 成交登记：双边可见，market=futures，且从吃单（合约杠杆单）继承 is_margin/leverage。
	ts1 := e.ListTrades(1, "", 0)
	if len(ts1) != 1 || ts1[0].Market != "futures" {
		t.Fatalf("user1 trades wrong: %+v", ts1)
	}
	if !ts1[0].IsMargin || ts1[0].Leverage != 10 {
		t.Fatalf("trade should inherit taker leverage: %+v", ts1[0])
	}
	ts2 := e.ListTrades(2, "", 0)
	if len(ts2) != 1 || ts2[0].Market != "futures" {
		t.Fatalf("user2 trades wrong: %+v", ts2)
	}
	// 成交按杠杆过滤：本场景唯一成交是杠杆，margin=0 应为空。
	var levTrades, plainTrades []TradeView
	for _, v := range e.ListTrades(0, "", 0) {
		if v.IsMargin {
			levTrades = append(levTrades, v)
		} else {
			plainTrades = append(plainTrades, v)
		}
	}
	if len(levTrades) != 1 || len(plainTrades) != 0 {
		t.Fatalf("trade leverage split wrong: lev=%d plain=%d", len(levTrades), len(plainTrades))
	}
	if !levTrades[0].MarginMatches("1") || levTrades[0].MarginMatches("0") {
		t.Fatal("TradeView.MarginMatches wrong")
	}

	// 撤销：新挂一笔后撤单，状态应变为 canceled。
	if _, _ = e.MatchNow("BTC_USDT", &Order{
		ID: 4, UserID: 4, Side: Buy, Price: fxPrice(50), Qty: fxQty(1), Time: 4, Market: "spot",
	}, true); false {
	}
	if !e.Cancel("BTC_USDT", 4) {
		t.Fatal("cancel should succeed for live order")
	}
	if v, ok := e.GetOrder(4); !ok || v.Status != OrderCanceled {
		t.Fatalf("canceled order status wrong: ok=%v v=%+v", ok, v)
	}
}

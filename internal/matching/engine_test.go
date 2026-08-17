package matching

import (
	"math"
	"testing"
)

// 演示：MatchNow 同步撮合——市价卖单与挂单的限价买单成交，返回成交且完全成交。
func TestMatchNowFillsAgainstBook(t *testing.T) {
	e := NewEngine(nil, nil)
	e.Register("BTC_USDT")

	// 先同步挂一笔限价买单（rest=true 使其挂入订单簿；Submit 为异步入队，此处用同步接口播种）。
	rest := &Order{ID: 1, UserID: 1, Side: Buy, Price: 100, Qty: 1, Time: 1}
	if _, _ = e.MatchNow("BTC_USDT", rest, true); rest.IsFilled() {
		t.Fatalf("限价买单应挂单未成交")
	}

	// 同步提交市价卖单（Price=0）吃流动性。
	o := &Order{ID: 2, UserID: 2, Side: Sell, Price: 0, Qty: 1, Time: 2}
	trades, fully := e.MatchNow("BTC_USDT", o, false)
	if !fully {
		t.Fatalf("市价卖单应完全成交，实际 filled=%.4f", o.Filled)
	}
	if len(trades) != 1 || !approx(trades[0].Price, 100, 1e-9) || !approx(trades[0].Qty, 1, 1e-9) {
		t.Fatalf("成交错误: %+v", trades)
	}
	if trades[0].TakerID != 2 || trades[0].MakerID != 1 {
		t.Fatalf("taker/maker 标识错误: %+v", trades[0])
	}
}

// 演示：MatchNow 在订单簿无流动性且 rest=false 时，市价单不挂单、不污染订单簿。
func TestMatchNowNoLiquidityNoRest(t *testing.T) {
	e := NewEngine(nil, nil)
	e.Register("BTC_USDT")

	o := &Order{ID: 3, UserID: 2, Side: Sell, Price: 0, Qty: 1, Time: 1}
	trades, fully := e.MatchNow("BTC_USDT", o, false)
	if fully {
		t.Fatalf("无流动性时不应完全成交")
	}
	if len(trades) != 0 {
		t.Fatalf("无流动性时应无成交，实际 %+v", trades)
	}
	// 订单簿不应出现 price=0 的挂单。
	bids, asks, ok := e.Depth("BTC_USDT")
	if !ok {
		t.Fatalf("depth 查询失败")
	}
	for _, lvl := range append(bids, asks...) {
		if lvl.Price <= 0 {
			t.Fatalf("订单簿被 price<=0 挂单污染: %+v", lvl)
		}
	}
}

func approx(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

// F5：非法订单（nil/非正数量/负价/NaN/Inf）在引擎层被拒绝，不得进入撮合、不得污染订单簿。
func TestMatchNowRejectsInvalidOrders(t *testing.T) {
	e := NewEngine(nil, nil)
	e.Register("BTC_USDT")

	invalid := []*Order{
		nil,
		{Side: Buy, Price: 50000, Qty: 0},
		{Side: Buy, Price: 50000, Qty: -1},
		{Side: Buy, Price: math.NaN(), Qty: 1},
		{Side: Buy, Price: math.Inf(1), Qty: 1},
		{Side: Buy, Price: -1, Qty: 1},
		{Side: Buy, Price: 50000, Qty: math.NaN()},
		{Side: Buy, Price: 50000, StopPrice: math.Inf(1), Qty: 1},
	}
	for i, o := range invalid {
		if tr, _ := e.MatchNow("BTC_USDT", o, false); tr != nil {
			t.Fatalf("case %d: invalid order must be rejected (nil trades), got %d trades", i, len(tr))
		}
	}

	// 合法市价单（price=0）不被拒：先同步挂一笔限价买单，再市价卖应成交。
	rest := &Order{ID: 1, UserID: 1, Side: Buy, Price: 100, Qty: 1, Time: 1}
	e.MatchNow("BTC_USDT", rest, true)
	market := &Order{ID: 2, UserID: 2, Side: Sell, Price: 0, Qty: 1, Time: 2}
	tr, fully := e.MatchNow("BTC_USDT", market, false)
	if tr == nil || len(tr) == 0 || !fully {
		t.Fatalf("valid market order should fill, got trades=%d fully=%v", len(tr), fully)
	}
}

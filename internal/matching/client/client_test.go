package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/matching/client"
)

// 编译期断言：*client.Client 满足 matching.Matcher 接口（spot/futures 收敛的关键）。
var _ matching.Matcher = (*client.Client)(nil)

// okJSON 以与 cmd/matching（response.JSON）一致的信封结构返回成功响应，保证
// client 解包逻辑与真实服务契约一致。
func okJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": 0, "message": "ok", "data": data})
}

// newFakeREST 用真实 matching.Engine 模拟 cmd/matching 的 REST 端点，验证客户端 JSON 契约。
func newFakeREST(t *testing.T) (*httptest.Server, *matching.Engine) {
	t.Helper()
	e := matching.NewEngine(nil, nil)
	e.Register("BTC_USDT")

	var seq int64 = 1000 // 模拟 Store.NextOrderID：测试 fake 不接 Store，自行分配唯一 ID。

	mux := http.NewServeMux()
	mux.HandleFunc("/order", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Symbol string  `json:"symbol"`
			Side   string  `json:"side"`
			Price  float64 `json:"price"`
			Qty    float64 `json:"qty"`
			UserID int64   `json:"user_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		side := matching.Buy
		if req.Side == "sell" {
			side = matching.Sell
		}
		o := &matching.Order{UserID: req.UserID, Side: side, Price: req.Price, Qty: req.Qty, Time: time.Now().UnixNano()}
		seq++
		o.ID = seq
		if !e.Submit(req.Symbol, o) {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "message": "unknown symbol", "data": nil})
			return
		}
		okJSON(w, map[string]interface{}{"order_id": o.ID, "status": "accepted"})
	})
	mux.HandleFunc("/depth", func(w http.ResponseWriter, r *http.Request) {
		sym := r.URL.Query().Get("symbol")
		bids, asks, ok := e.Depth(sym)
		if !ok {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": 400, "message": "unknown symbol", "data": nil})
			return
		}
		okJSON(w, map[string]interface{}{"symbol": sym, "bids": bids, "asks": asks})
	})
	mux.HandleFunc("/match-now", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Symbol string  `json:"symbol"`
			Side   string  `json:"side"`
			Price  float64 `json:"price"`
			Qty    float64 `json:"qty"`
			UserID int64   `json:"user_id"`
			Rest   bool    `json:"rest"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		side := matching.Buy
		if req.Side == "sell" {
			side = matching.Sell
		}
		o := &matching.Order{UserID: req.UserID, Side: side, Price: req.Price, Qty: req.Qty, Time: time.Now().UnixNano()}
		trades, fully := e.MatchNow(req.Symbol, o, req.Rest)
		okJSON(w, map[string]interface{}{
			"symbol": req.Symbol, "trades": trades, "filled": o.Filled, "fully_filled": fully,
		})
	})
	mux.HandleFunc("/cancel", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Symbol  string `json:"symbol"`
			OrderID int64  `json:"order_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		c := e.Cancel(req.Symbol, req.OrderID)
		okJSON(w, map[string]interface{}{"canceled": c})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, e
}

func TestClientSubmitAndDepth(t *testing.T) {
	srv, _ := newFakeREST(t)
	c := client.New(srv.URL)

	o := &matching.Order{Side: matching.Buy, Price: 100, Qty: 1, Time: time.Now().UnixNano()}
	if !c.Submit("BTC_USDT", o) {
		t.Fatal("submit should succeed")
	}
	if o.ID <= 0 {
		t.Fatalf("submit should assign a positive order_id, got %d", o.ID)
	}

	bids, asks, ok := c.Depth("BTC_USDT")
	if !ok {
		t.Fatal("depth should succeed")
	}
	if len(asks) != 0 {
		t.Fatalf("expected no asks, got %d", len(asks))
	}
	if len(bids) == 0 {
		t.Fatal("expected at least one bid level after submit")
	}
	if bids[0].Price != 100 {
		t.Fatalf("bid price should be 100, got %.2f", bids[0].Price)
	}
}

func TestClientMatchNow(t *testing.T) {
	srv, e := newFakeREST(t)
	c := client.New(srv.URL)

	// 播种一笔限价买@90 作为对手流动性。
	if _, _ = e.MatchNow("BTC_USDT", &matching.Order{
		ID: 1, Side: matching.Buy, Price: 90, Qty: 1, Time: 1,
	}, true); false {
	}

	o := &matching.Order{Side: matching.Sell, Price: 0, Qty: 1, Time: 2}
	trades, fully := c.MatchNow("BTC_USDT", o, false)
	if !fully {
		t.Fatal("market sell should fully fill against resting buy")
	}
	if len(trades) == 0 {
		t.Fatal("expected at least one trade")
	}
	if trades[0].Price != 90 {
		t.Fatalf("trade price should be 90 (resting liquidity), got %.2f", trades[0].Price)
	}
}

func TestClientCancel(t *testing.T) {
	srv, _ := newFakeREST(t)
	c := client.New(srv.URL)

	o := &matching.Order{Side: matching.Buy, Price: 100, Qty: 1, Time: time.Now().UnixNano()}
	if !c.Submit("BTC_USDT", o) {
		t.Fatal("submit failed")
	}
	canceled, err := c.CancelOrder("BTC_USDT", o.ID)
	if err != nil {
		t.Fatalf("cancel error: %v", err)
	}
	if !canceled {
		t.Fatal("cancel should report true for a live order")
	}
}

// TestClientWatch 验证 WebSocket 行情订阅：连接后服务端推送成交与深度，客户端应能解析回调。
func TestClientWatch(t *testing.T) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]interface{}{
			"type": "trade", "symbol": "BTC_USDT",
			"data": matching.Trade{Price: 100, Qty: 1, TakerSide: matching.Buy},
		})
		_ = conn.WriteJSON(map[string]interface{}{
			"type": "depth", "symbol": "BTC_USDT",
			"data": map[string]interface{}{
				"bids": []matching.Level{{Price: 100}},
				"asks": []matching.Level{},
			},
		})
		time.Sleep(200 * time.Millisecond)
		_ = conn.Close()
	}))
	defer srv.Close()

	c := client.New(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	tradeCh := make(chan client.TradeEvent, 4)
	depthCh := make(chan client.DepthEvent, 4)
	go func() {
		_ = c.Watch(ctx, []string{"BTC_USDT"},
			func(e client.TradeEvent) { tradeCh <- e },
			func(e client.DepthEvent) { depthCh <- e })
	}()

	select {
	case ev := <-tradeCh:
		if ev.Symbol != "BTC_USDT" || ev.Trade.Price != 100 {
			t.Fatalf("unexpected trade event: %+v", ev)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("did not receive trade event via WS")
	}

	select {
	case ev := <-depthCh:
		if ev.Symbol != "BTC_USDT" || len(ev.Bids) != 1 || ev.Bids[0].Price != 100 {
			t.Fatalf("unexpected depth event: %+v", ev)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("did not receive depth event via WS")
	}
}

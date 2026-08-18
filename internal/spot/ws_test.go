package spot

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/coldlar/crypto-exchange/internal/ws"
)

// wsDial 升级到 spot 行情 WebSocket（公开端点，豁免鉴权）。
func wsDial(t *testing.T, r *gin.Engine, symbol string) *websocket.Conn {
	t.Helper()
	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/spot/ws"
	if symbol != "" {
		url += "?symbol=" + symbol
	}
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

// wsExpectReceive 广播 payload 并读取客户端首条消息；注册竞态下以短超时循环重广播直至收到。
func wsExpectReceive(t *testing.T, conn *websocket.Conn, hub *ws.Hub, symbol string, payload interface{}) map[string]interface{} {
	t.Helper()
	overall := time.Now().Add(3 * time.Second)
	for {
		hub.Broadcast(symbol, payload)
		_ = conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		var got map[string]interface{}
		if err := conn.ReadJSON(&got); err == nil {
			return got
		}
		if time.Now().After(overall) {
			t.Fatal("timeout waiting for ws message (registration race or no delivery)")
		}
	}
}

// 用例15（WS 行情推送）：订阅 BTC_USDT 后，该 symbol 的广播被推送到客户端。
func TestWSPushSubscribedSymbol(t *testing.T) {
	s := newTestServer()
	s.hub = ws.NewHub()
	r := setupRouter(s)
	conn := wsDial(t, r, "BTC_USDT")

	got := wsExpectReceive(t, conn, s.hub, "BTC_USDT", gin.H{"type": "trade", "data": gin.H{"price": 100}})
	if got["type"] != "trade" {
		t.Fatalf("expect type=trade, got %v", got["type"])
	}
}

// 用例15b（WS 符号过滤）：订阅 BTC_USDT 时，ETH_USDT 的广播不会被推送。
func TestWSFilteredSymbolNotDelivered(t *testing.T) {
	s := newTestServer()
	s.hub = ws.NewHub()
	r := setupRouter(s)
	conn := wsDial(t, r, "BTC_USDT")

	s.hub.Broadcast("ETH_USDT", gin.H{"type": "depth"})
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("should not receive message for unsubscribed symbol")
	}
}

// 用例15c（WS 全部订阅）：未指定 symbol 的客户端接收任意 symbol 的广播。
func TestWSAllSubscriberReceivesAll(t *testing.T) {
	s := newTestServer()
	s.hub = ws.NewHub()
	r := setupRouter(s)
	conn := wsDial(t, r, "") // 空订阅 = 接收全部

	got := wsExpectReceive(t, conn, s.hub, "BTC_USDT", gin.H{"type": "trade"})
	if got["type"] != "trade" {
		t.Fatalf("expect type=trade, got %v", got["type"])
	}
	got = wsExpectReceive(t, conn, s.hub, "ETH_USDT", gin.H{"type": "depth"})
	if got["type"] != "depth" {
		t.Fatalf("expect type=depth, got %v", got["type"])
	}
}

// 用例15e（WS 空 symbol 广播集成）：以空 symbol 广播，推送至所有连接，
// 无论其订阅为具体 symbol（BTC_USDT / ETH_USDT）还是空订阅（全部）。
// 三类异构订阅客户端均能收到，验证"symbol 为空表示推送给所有连接"的语义。
func TestWSBroadcastEmptySymbolToAllConnections(t *testing.T) {
	s := newTestServer()
	s.hub = ws.NewHub()
	r := setupRouter(s)
	cBTC := wsDial(t, r, "BTC_USDT")  // 具体订阅
	cETH := wsDial(t, r, "ETH_USDT")  // 具体订阅（不同对）
	cAll := wsDial(t, r, "")          // 空订阅 = 全部

	// wsExpectReceive 内部以短超时循环重广播，规避注册竞态，无需 sleep。
	for i, conn := range []*websocket.Conn{cBTC, cETH, cAll} {
		got := wsExpectReceive(t, conn, s.hub, "", gin.H{"type": "global"})
		if got["type"] != "global" {
			t.Fatalf("client %d expect type=global, got %v", i, got["type"])
		}
	}
}

// 用例15d（WS 多 symbol 订阅）：订阅 BTC_USDT,ETH_USDT 后两者均推送，无关 symbol 不推送。
func TestWSMultipleSymbols(t *testing.T) {
	s := newTestServer()
	s.hub = ws.NewHub()
	r := setupRouter(s)
	conn := wsDial(t, r, "BTC_USDT,ETH_USDT")

	got := wsExpectReceive(t, conn, s.hub, "ETH_USDT", gin.H{"type": "depth"})
	if got["type"] != "depth" {
		t.Fatalf("expect depth, got %v", got["type"])
	}
	got = wsExpectReceive(t, conn, s.hub, "BTC_USDT", gin.H{"type": "trade"})
	if got["type"] != "trade" {
		t.Fatalf("expect trade, got %v", got["type"])
	}

	// 无关 symbol（SOL_USDT）不应推送。
	s.hub.Broadcast("SOL_USDT", gin.H{"type": "trade"})
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("should not receive message for symbol outside subscription")
	}
}

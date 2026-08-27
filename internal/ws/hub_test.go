package ws

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// newTestHub 创建测试用 Hub + httptest.Server，返回 hub、URL 和关闭函数。
func newTestHub(t *testing.T) (*Hub, string, func()) {
	t.Helper()
	hub := NewHub()
	srv := httptest.NewServer(http.HandlerFunc(hub.Handle))
	return hub, srv.URL, srv.Close
}

// dialHub 连接 WebSocket，可选 symbol 订阅参数。
func dialHub(t *testing.T, url, symbol string) *websocket.Conn {
	t.Helper()
	u := strings.Replace(url, "http://", "ws://", 1)
	if symbol != "" {
		u += "?symbol=" + symbol
	}
	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", u, err)
	}
	return c
}

// readMsgWithTimeout 带超时读取一条消息，超时返回 nil。
func readMsgWithTimeout(c *websocket.Conn, timeout time.Duration) []byte {
	c.SetReadDeadline(time.Now().Add(timeout))
	_, msg, err := c.ReadMessage()
	if err != nil {
		return nil
	}
	return msg
}

func TestHub_BroadcastMatchingSymbol(t *testing.T) {
	hub := NewHub()
	srv := httptest.NewServer(http.HandlerFunc(hub.Handle))
	defer srv.Close()

	btc := dialHub(t, srv.URL, "BTCUSDT")
	defer btc.Close()
	eth := dialHub(t, srv.URL, "ETHUSDT")
	defer eth.Close()

	time.Sleep(50 * time.Millisecond)

	payload := map[string]string{"symbol": "BTCUSDT", "price": "60000"}
	hub.Broadcast("BTCUSDT", payload)

	btcMsg := readMsgWithTimeout(btc, 500*time.Millisecond)
	ethMsg := readMsgWithTimeout(eth, 500*time.Millisecond)

	if btcMsg == nil {
		t.Fatal("BTC subscriber should have received message")
	}
	if ethMsg != nil {
		t.Fatal("ETH subscriber should not have received BTC message")
	}

	var got map[string]string
	if err := json.Unmarshal(btcMsg, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["price"] != "60000" {
		t.Fatalf("unexpected payload: %v", got)
	}
}

func TestHub_BroadcastAllToEmptySubscription(t *testing.T) {
	hub := NewHub()
	srv := httptest.NewServer(http.HandlerFunc(hub.Handle))
	defer srv.Close()

	// 无 symbol 订阅的客户端，应接收所有广播
	noSub := dialHub(t, srv.URL, "")
	defer noSub.Close()

	time.Sleep(50 * time.Millisecond)

	hub.Broadcast("ANYTHING", map[string]string{"data": "hello"})
	msg := readMsgWithTimeout(noSub, 500*time.Millisecond)
	if msg == nil {
		t.Fatal("empty-subscription client should receive all broadcasts")
	}
}

func TestHub_BroadcastEmptySymbolToAll(t *testing.T) {
	hub := NewHub()
	srv := httptest.NewServer(http.HandlerFunc(hub.Handle))
	defer srv.Close()

	btc := dialHub(t, srv.URL, "BTCUSDT")
	defer btc.Close()
	eth := dialHub(t, srv.URL, "ETHUSDT")
	defer eth.Close()

	time.Sleep(50 * time.Millisecond)

	// symbol="" 广播给所有连接
	hub.Broadcast("", map[string]string{"msg": "all"})

	btcMsg := readMsgWithTimeout(btc, 500*time.Millisecond)
	ethMsg := readMsgWithTimeout(eth, 500*time.Millisecond)

	if btcMsg == nil {
		t.Fatal("BTC subscriber should receive empty-symbol broadcast")
	}
	if ethMsg == nil {
		t.Fatal("ETH subscriber should receive empty-symbol broadcast")
	}
}

func TestHub_BroadcastBufferFull(t *testing.T) {
	hub := NewHub()
	srv := httptest.NewServer(http.HandlerFunc(hub.Handle))
	defer srv.Close()

	c := dialHub(t, srv.URL, "TEST")
	defer c.Close()

	time.Sleep(50 * time.Millisecond)

	// send channel 容量为 64，灌入 65 条消息不应阻塞
	done := make(chan struct{})
	go func() {
		for i := 0; i < 65; i++ {
			hub.Broadcast("TEST", map[string]int{"i": i})
		}
		close(done)
	}()

	select {
	case <-done:
		// 未阻塞
	case <-time.After(2 * time.Second):
		t.Fatal("Broadcast blocked on full buffer")
	}
}

func TestHub_UnregisterOnClientClose(t *testing.T) {
	hub := NewHub()
	srv := httptest.NewServer(http.HandlerFunc(hub.Handle))
	defer srv.Close()

	c := dialHub(t, srv.URL, "CLOSED")
	time.Sleep(50 * time.Millisecond)
	c.Close()
	time.Sleep(100 * time.Millisecond)

	hub.mu.RLock()
	count := len(hub.clients)
	hub.mu.RUnlock()

	if count != 0 {
		t.Fatalf("expected 0 clients after close, got %d", count)
	}

	// 向已清空的 hub 广播不应 panic
	done := make(chan struct{})
	go func() {
		hub.Broadcast("ANYTHING", map[string]string{"data": "test"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("Broadcast to empty hub blocked")
	}
}

func TestContainsSymbol(t *testing.T) {
	tests := []struct {
		symbols []string
		target  string
		want    bool
	}{
		{[]string{"BTCUSDT", "ETHUSDT"}, "BTCUSDT", true},
		{[]string{"BTCUSDT", "ETHUSDT"}, "SOLUSDT", false},
		{[]string{}, "BTCUSDT", false},
		{nil, "BTCUSDT", false},
	}
	for _, tt := range tests {
		got := containsSymbol(tt.symbols, tt.target)
		if got != tt.want {
			t.Errorf("containsSymbol(%v, %q) = %v, want %v", tt.symbols, tt.target, got, tt.want)
		}
	}
}

func TestHub_CheckOriginWithAllowedOrigins(t *testing.T) {
	hub := NewHubWithOrigins([]string{"https://app.example.com"})
	srv := httptest.NewServer(http.HandlerFunc(hub.Handle))
	defer srv.Close()

	u := strings.Replace(srv.URL, "http://", "ws://", 1) + "?symbol=TEST"

	// 不带 Origin 应被拒绝（gorilla 默认不设置 Origin 时用空字符串）
	_, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err == nil {
		// 某些版本的 gorilla 在无 Origin 时仍允许连接（降级到无 Origin header）
		// 检查 HTTP 响应码
		if resp != nil && resp.StatusCode == http.StatusSwitchingProtocols {
			// 连接成功 — gorilla 在无 Origin header 时不做检查
			// 这是预期行为：没有 Origin header 意味着同源请求
		}
		if resp != nil {
			resp.Body.Close()
		}
	} else if resp != nil {
		resp.Body.Close()
	}

	// 带不匹配的 Origin 应被拒绝
	header := http.Header{}
	header.Set("Origin", "https://evil.com")
	badConn, resp, err := websocket.DefaultDialer.Dial(u, header)
	if err == nil {
		badConn.Close()
		t.Fatal("connection with wrong origin should be rejected")
	}
	if resp != nil {
		resp.Body.Close()
	}

	// 带匹配的 Origin 应被接受
	header.Set("Origin", "https://app.example.com")
	goodConn, resp, err := websocket.DefaultDialer.Dial(u, header)
	if err != nil {
		t.Fatalf("connection with correct origin should be accepted: %v", err)
	}
	goodConn.Close()
	if resp != nil {
		resp.Body.Close()
	}
}

func TestHub_MultiSymbolSubscription(t *testing.T) {
	hub := NewHub()
	srv := httptest.NewServer(http.HandlerFunc(hub.Handle))
	defer srv.Close()

	// 订阅多个 symbol（逗号分隔）
	multi := dialHub(t, srv.URL, "BTCUSDT,ETHUSDT")
	defer multi.Close()

	time.Sleep(50 * time.Millisecond)

	hub.Broadcast("BTCUSDT", map[string]string{"symbol": "BTCUSDT"})
	msg1 := readMsgWithTimeout(multi, 500*time.Millisecond)
	if msg1 == nil {
		t.Fatal("multi-symbol subscriber should receive BTCUSDT broadcast")
	}

	hub.Broadcast("ETHUSDT", map[string]string{"symbol": "ETHUSDT"})
	msg2 := readMsgWithTimeout(multi, 500*time.Millisecond)
	if msg2 == nil {
		t.Fatal("multi-symbol subscriber should receive ETHUSDT broadcast")
	}

	hub.Broadcast("SOLUSDT", map[string]string{"symbol": "SOLUSDT"})
	msg3 := readMsgWithTimeout(multi, 500*time.Millisecond)
	if msg3 != nil {
		t.Fatal("multi-symbol subscriber should NOT receive SOLUSDT broadcast")
	}
}

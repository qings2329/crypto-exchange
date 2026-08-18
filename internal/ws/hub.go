package ws

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

// Client 表示一个 WebSocket 连接，按 symbol 订阅（支持逗号分隔的多个 symbol）。
type Client struct {
	conn    *websocket.Conn
	symbols []string
	send    chan []byte
}

// Hub 管理所有 WebSocket 客户端，按 symbol 广播消息。
type Hub struct {
	mu      sync.RWMutex
	clients map[*Client]struct{}
	up      websocket.Upgrader
}

// NewHub 创建广播中心。
func NewHub() *Hub {
	return &Hub{
		clients: make(map[*Client]struct{}),
		up: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true }, // 开发期放开，生产按域名校验
		},
	}
}

// Handle 升级 HTTP 为 WebSocket，并按 ?symbol= 订阅。
func (h *Hub) Handle(w http.ResponseWriter, r *http.Request) {
	conn, err := h.up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	// 订阅的 symbol 支持逗号分隔的多个交易对；为空表示订阅全部。
	var symbols []string
	if s := r.URL.Query().Get("symbol"); s != "" {
		for _, p := range strings.Split(s, ",") {
			if p = strings.TrimSpace(p); p != "" {
				symbols = append(symbols, p)
			}
		}
	}
	c := &Client{
		conn:    conn,
		symbols: symbols,
		send:    make(chan []byte, 64),
	}
	h.register(c)
	go c.writePump()
	go c.readPump(h)
}

func (h *Hub) register(c *Client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *Hub) unregister(c *Client) {
	h.mu.Lock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		close(c.send)
	}
	h.mu.Unlock()
}

func (c *Client) writePump() {
	defer c.conn.Close()
	for msg := range c.send {
		if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
	}
}

// readPump 读取客户端消息（此处仅用于检测断开）。
func (c *Client) readPump(h *Hub) {
	defer h.unregister(c)
	defer c.conn.Close()
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
	}
}

// Broadcast 向订阅了指定 symbol 的客户端推送消息。
// symbol 为空表示推送给所有连接；客户端订阅了多个 symbol 时，任一匹配即推送；
// 客户端空订阅（未指定 symbol）时同样接收所有广播。
func (h *Hub) Broadcast(symbol string, payload interface{}) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		// 空 symbol 广播给所有连接；否则仅推送给订阅了该 symbol（或空订阅/全部）的客户端。
		if symbol == "" || len(c.symbols) == 0 || containsSymbol(c.symbols, symbol) {
			select {
			case c.send <- b:
			default: // 发送缓冲满则丢弃，避免阻塞撮合
			}
		}
	}
}

// containsSymbol 判断订阅列表是否包含目标 symbol。
func containsSymbol(symbols []string, symbol string) bool {
	for _, s := range symbols {
		if s == symbol {
			return true
		}
	}
	return false
}

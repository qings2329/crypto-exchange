package ws

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// Client 表示一个 WebSocket 连接，按 symbol 订阅。
type Client struct {
	conn   *websocket.Conn
	symbol string
	send   chan []byte
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
	c := &Client{
		conn:   conn,
		symbol: r.URL.Query().Get("symbol"),
		send:   make(chan []byte, 64),
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

// Broadcast 向订阅了指定 symbol 的所有客户端推送消息。
// symbol 为空表示推送给所有连接。
func (h *Hub) Broadcast(symbol string, payload interface{}) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for c := range h.clients {
		if c.symbol == "" || c.symbol == symbol {
			select {
			case c.send <- b:
			default: // 发送缓冲满则丢弃，避免阻塞撮合
			}
		}
	}
}

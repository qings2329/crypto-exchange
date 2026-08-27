// Package client 是撮合引擎（cmd/matching）的 HTTP + WebSocket 客户端。
//
// 它实现 matching.Matcher 接口，使 spot/futures 等服务可把「匹配」收敛为调用单一
// cmd/matching 服务，而非各自持有进程内引擎。这样多实例部署时只有一个匹配权威
// （单写者），订单簿不再分裂、order_id 不再冲突（见 DEVELOPMENT_TASKS §17/§18）。
//
//   - SubmitOrder/CancelOrder/Depth：走 HTTP，对应 cmd/matching 的 REST 端点；
//   - MatchNow：走 HTTP /match-now，同步返回真实成交（供强平器使用）；
//   - Watch：走 WebSocket /ws，实时接收成交与深度，带断线重连，驱动上游业务逻辑。
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/coldlar/crypto-exchange/internal/matching"
)

// Client 是 cmd/matching 服务的客户端，满足 matching.Matcher 接口。
type Client struct {
	baseURL string
	http    *http.Client
}

// New 创建客户端。baseURL 形如 http://127.0.0.1:8085（会自动升级为 ws 用于 Watch）。
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// ---- matching.Matcher 接口实现 ----

// Submit 提交一笔订单（对应 POST /order）。返回服务端分配的全局唯一 order_id。
func (c *Client) Submit(symbol string, o *matching.Order) bool {
	side := "buy"
	if o.Side == matching.Sell {
		side = "sell"
	}
	var resp struct {
		OrderID int64  `json:"order_id"`
		Status  string `json:"status"`
		Error   string `json:"error"`
	}
	if err := c.postJSON("/order", map[string]interface{}{
		"symbol":  symbol,
		"side":    side,
		"price":   o.Price,
		"qty":     o.Qty,
		"user_id": o.UserID,
		"market":  o.Market,
	}, &resp); err != nil {
		return false
	}
	if resp.OrderID != 0 {
		o.ID = resp.OrderID
	}
	return resp.Error == "" && resp.OrderID != 0
}

// Depth 返回某交易对深度（对应 GET /depth）。
func (c *Client) Depth(symbol string) (bids, asks []matching.Level, ok bool) {
	var resp struct {
		Symbol string           `json:"symbol"`
		Bids   []matching.Level `json:"bids"`
		Asks   []matching.Level `json:"asks"`
		Error  string           `json:"error"`
	}
	if err := c.getJSON("/depth?symbol="+url.QueryEscape(symbol), &resp); err != nil {
		return nil, nil, false
	}
	if resp.Error != "" {
		return nil, nil, false
	}
	return resp.Bids, resp.Asks, true
}

// MatchNow 同步撮合一笔订单并返回真实成交（对应 POST /match-now），用于强平。
func (c *Client) MatchNow(symbol string, o *matching.Order, rest bool) ([]matching.Trade, bool) {
	side := "buy"
	if o.Side == matching.Sell {
		side = "sell"
	}
	var resp struct {
		Symbol      string           `json:"symbol"`
		Trades      []matching.Trade `json:"trades"`
		Filled      matching.Fixed   `json:"filled"`
		FullyFilled bool             `json:"fully_filled"`
		Error       string           `json:"error"`
	}
	if err := c.postJSON("/match-now", map[string]interface{}{
		"symbol":  symbol,
		"side":    side,
		"price":   o.Price,
		"qty":     o.Qty,
		"user_id": o.UserID,
		"rest":    rest,
	}, &resp); err != nil {
		return nil, false
	}
	if resp.Error != "" {
		return nil, false
	}
	o.Filled = resp.Filled
	return resp.Trades, resp.FullyFilled
}

// ---- 额外便捷方法（不满足 Matcher，供调用方显式撤销）----

// CancelOrder 撤销一笔订单（对应 POST /cancel）。
func (c *Client) CancelOrder(symbol string, orderID int64) (canceled bool, err error) {
	var resp struct {
		Canceled bool   `json:"canceled"`
		Error    string `json:"error"`
	}
	if err := c.postJSON("/cancel", map[string]interface{}{
		"symbol":   symbol,
		"order_id": orderID,
	}, &resp); err != nil {
		return false, err
	}
	if resp.Error != "" {
		return false, fmt.Errorf("%s", resp.Error)
	}
	return resp.Canceled, nil
}

// Cancel 实现 matching.Matcher：经 cmd/matching /cancel 撤销订单；网络/服务端错误返回 false。
func (c *Client) Cancel(symbol string, orderID int64) bool {
	canceled, err := c.CancelOrder(symbol, orderID)
	if err != nil {
		return false
	}
	return canceled
}

// ---- WebSocket 行情订阅 ----

// TradeEvent 是单笔成交事件（来自 WS）。
type TradeEvent struct {
	Symbol string
	Trade  matching.Trade
}

// DepthEvent 是订单簿深度事件（来自 WS）。
type DepthEvent struct {
	Symbol string
	Bids   []matching.Level
	Asks   []matching.Level
}

// Watch 订阅 cmd/matching 的行情 WebSocket，实时回调 onTrade/onDepth，带断线重连。
// symbols 为空表示订阅全部交易对；否则仅订阅给定交易对。阻塞直到 ctx 取消。
func (c *Client) Watch(ctx context.Context, symbols []string,
	onTrade func(TradeEvent), onDepth func(DepthEvent)) error {

	wsURL, err := c.wsURL(symbols)
	if err != nil {
		return err
	}

	// 指数退避重连，直到 ctx 取消。
	backoff := time.Second
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		conn, _, derr := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
		if derr != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			if backoff < 8*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second

		readErr := c.readLoop(ctx, conn, onTrade, onDepth)
		_ = conn.Close()
		if readErr != nil {
			// 连接断开，进入重连；若 ctx 已取消则退出。
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
	}
}

func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn,
	onTrade func(TradeEvent), onDepth func(DepthEvent)) error {

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var envelope struct {
			Type   string          `json:"type"`
			Symbol string          `json:"symbol"`
			Data   json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			continue
		}
		switch envelope.Type {
		case "trade":
			var t matching.Trade
			if err := json.Unmarshal(envelope.Data, &t); err == nil && onTrade != nil {
				onTrade(TradeEvent{Symbol: envelope.Symbol, Trade: t})
			}
		case "depth":
			var d struct {
				Bids []matching.Level `json:"bids"`
				Asks []matching.Level `json:"asks"`
			}
			if err := json.Unmarshal(envelope.Data, &d); err == nil && onDepth != nil {
				onDepth(DepthEvent{Symbol: envelope.Symbol, Bids: d.Bids, Asks: d.Asks})
			}
		}
	}
}

func (c *Client) wsURL(symbols []string) (string, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
		// 已正确
	default:
		return "", fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}
	q := url.Values{}
	if len(symbols) > 0 {
		q.Set("symbol", strings.Join(symbols, ","))
	}
	u.Path = "/ws"
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ---- HTTP 辅助 ----

// envelope 是 cmd/matching 经 response.JSON 返回的统一种子结构：{code,message,data}。
// 真实业务数据嵌套在 data 字段内，client 需解包后再反序列化到 out。
type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// unwrap 读取 HTTP 响应：先按信封解包，据状态码/code 判定错误，再把 data 子负载Unmarshal 到 out。
func unwrap(resp *http.Response, out interface{}) error {
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("matching http %d: %s", resp.StatusCode, string(data))
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("matching bad response: %w (raw=%s)", err, string(data))
	}
	if env.Code != 0 {
		return fmt.Errorf("matching error %d: %s", env.Code, env.Message)
	}
	if out != nil && len(env.Data) > 0 {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

func (c *Client) postJSON(path string, body interface{}, out interface{}) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := c.http.Post(c.baseURL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return unwrap(resp, out)
}

func (c *Client) getJSON(path string, out interface{}) error {
	resp, err := c.http.Get(c.baseURL + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return unwrap(resp, out)
}

// ListOrders 经 cmd/matching /orders 查询指定用户订单。
func (c *Client) ListOrders(userID int64, symbol, status string, limit int) []matching.OrderView {
	var out []matching.OrderView
	q := fmt.Sprintf("user_id=%d", userID)
	if symbol != "" {
		q += "&symbol=" + symbol
	}
	if status != "" {
		q += "&status=" + status
	}
	if limit > 0 {
		q += fmt.Sprintf("&limit=%d", limit)
	}
	if err := c.getJSON("/orders?"+q, &out); err != nil {
		return nil
	}
	return out
}

// GetOrder 经 cmd/matching /orders/:id 查询订单详情。
func (c *Client) GetOrder(orderID int64) (matching.OrderView, bool) {
	var out matching.OrderView
	if err := c.getJSON(fmt.Sprintf("/orders/%d", orderID), &out); err != nil {
		return matching.OrderView{}, false
	}
	return out, true
}

// OrderState 三态查询订单：known=true 时 v 为订单视图；known=false 且 err=nil 表示撮合
// 明确无此订单（HTTP 404，已从登记簿淘汰）；err!=nil 表示撮合不可达或响应异常——调用方
// （如 spot 重启恢复）必须保守处理，不得把「查不到」当作「订单不存在」而误释放冻结。
func (c *Client) OrderState(orderID int64) (matching.OrderView, bool, error) {
	resp, err := c.http.Get(fmt.Sprintf("%s/orders/%d", c.baseURL, orderID))
	if err != nil {
		return matching.OrderView{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return matching.OrderView{}, false, nil
	}
	var out matching.OrderView
	if err := unwrap(resp, &out); err != nil {
		return matching.OrderView{}, false, err
	}
	return out, true, nil
}

// ListTrades 经 cmd/matching /trades 查询指定用户成交流水。
func (c *Client) ListTrades(userID int64, symbol string, limit int) []matching.TradeView {
	var out []matching.TradeView
	q := fmt.Sprintf("user_id=%d", userID)
	if symbol != "" {
		q += "&symbol=" + symbol
	}
	if limit > 0 {
		q += "&limit=" + strconv.Itoa(limit)
	}
	if err := c.getJSON("/trades?"+q, &out); err != nil {
		return nil
	}
	return out
}

// RecentTrades 查询交易对最近的全市场公开成交（user_id=0），用于 spot 服务重启后
// 补结算停机窗口内漏掉的成交（配合账本幂等指纹去重，重放安全）。
func (c *Client) RecentTrades(symbol string, limit int) ([]matching.TradeView, error) {
	var out []matching.TradeView
	q := "symbol=" + url.QueryEscape(symbol)
	if limit > 0 {
		q += "&limit=" + strconv.Itoa(limit)
	}
	if err := c.getJSON("/trades?"+q, &out); err != nil {
		return nil, err
	}
	return out, nil
}

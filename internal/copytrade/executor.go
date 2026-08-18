package copytrade

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// OrderExecutor 代下单执行器（可插拔）。默认 HTTPExecutor 调 spot/futures 的 /order 接口，
// 复用其资金预冻/结算安全层与 F1(client_oid 幂等)/F4(token 鉴权) 守卫——粉丝资金安全由
// spot/futures 持有，copytrade 仅持粉丝授权 token 触发下单，绝不触碰粉丝私钥或余额。
type OrderExecutor interface {
	Execute(ctx context.Context, followerToken, market, symbol, side string, price, qty float64, clientOID string) (exchangeOrderID string, err error)
}

// HTTPExecutor 经 HTTP 调 spot/futures 的 /order（持有粉丝授权 token）。
type HTTPExecutor struct {
	spotURL    string
	futuresURL string
	client     *http.Client
}

// NewHTTPExecutor 构造 HTTP 执行器。
func NewHTTPExecutor(spotURL, futuresURL string) *HTTPExecutor {
	return &HTTPExecutor{spotURL: spotURL, futuresURL: futuresURL, client: &http.Client{Timeout: 10 * time.Second}}
}

// Execute 代粉丝下单；body 携带 client_oid（下游 F1 幂等）与 Bearer token（下游 F4）。
func (e *HTTPExecutor) Execute(ctx context.Context, followerToken, market, symbol, side string, price, qty float64, clientOID string) (string, error) {
	base := e.spotURL
	if market == "futures" {
		base = e.futuresURL
	}
	if base == "" {
		return "", fmt.Errorf("copytrade: no %s endpoint configured", market)
	}
	body := map[string]interface{}{
		"symbol": symbol, "side": side, "price": price, "qty": qty, "client_oid": clientOID,
	}
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/"+market+"/order", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+followerToken)
	resp, err := e.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("copytrade: order failed %d: %s", resp.StatusCode, string(data))
	}
	var out struct {
		OrderID string `json:"order_id"`
		Data    struct {
			OrderID string `json:"order_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(data, &out)
	id := out.OrderID
	if id == "" {
		id = out.Data.OrderID
	}
	return id, nil
}

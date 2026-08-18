// Package e2e 提供 bot / copytrade 两条新业务线与下游 spot/futures 订单服务的
// 跨服务端到端验证：在进程内以独立 httptest 服务拉起「下游 spot 订单服务」与 bot/copytrade
// 路由，二者共享同一 TokenVerifier，走真实 HTTP 验证跨服务资金安全不变量：
//
//   - F4 授权：bot/copytrade 代用户下单时携带的用户 token，经下游 spot 真实校验后解析出的
//     user_id 必须等于被代用户（杜绝越权）；
//   - F1 幂等：bot 的 client_oid=bot:<id>:<round>、copytrade 的 client_oid=copytrade:<fid>:<eventID>
//     随请求落到下游，由下游去重；
//   - 复制费：copytrade 触发复制后，平台复制费以定点结算入 SysCopyTradeFee。
//
// 下游 spot 订单端点复刻了 spot/futures `/api/v1/<market>/order` 的契约（Bearer 鉴权 +
// client_oid + 返回 order_id），不依赖 MySQL/Kafka，可在任意环境 `go test ./e2e/...` 运行。
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/bot"
	"github.com/coldlar/crypto-exchange/internal/copytrade"
	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// recordedOrder 是下游 spot 订单服务记录到的一笔成交委托（含解析出的 user_id，用于断言 F4）。
type recordedOrder struct {
	userID    int64
	market    string
	symbol    string
	side      string
	price     float64
	qty       float64
	clientOID string
}

// downstreamSpot 复刻 spot/futures 订单服务契约的下游桩：真实校验 Bearer token（F4），
// 记录每笔委托并返回 order_id。bot/copytrade 的 HTTPExecutor 直接打这个端点。
type downstreamSpot struct {
	verifier *middleware.TokenVerifier
	mu       sync.Mutex
	orders   []recordedOrder
}

func newDownstreamSpot(v *middleware.TokenVerifier) *downstreamSpot {
	return &downstreamSpot{verifier: v}
}

func (d *downstreamSpot) routes() *gin.Engine {
	r := gin.New()
	handler := func(market string) gin.HandlerFunc {
		return func(c *gin.Context) {
			token := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
			uid, ok := d.verifier.Verify(token)
			if !ok {
				c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
				return
			}
			var body struct {
				Symbol    string  `json:"symbol"`
				Side      string  `json:"side"`
				Price     float64 `json:"price"`
				Qty       float64 `json:"qty"`
				ClientOID string  `json:"client_oid"`
			}
			_ = c.ShouldBindJSON(&body)
			d.mu.Lock()
			d.orders = append(d.orders, recordedOrder{userID: uid, market: market, symbol: body.Symbol, side: body.Side, price: body.Price, qty: body.Qty, clientOID: body.ClientOID})
			idx := len(d.orders)
			d.mu.Unlock()
			c.JSON(http.StatusOK, gin.H{"order_id": fmt.Sprintf("spot-%d", idx)})
		}
	}
	r.POST("/api/v1/spot/order", handler("spot"))
	r.POST("/api/v1/futures/order", handler("futures"))
	return r
}

func (d *downstreamSpot) lastOrders() []recordedOrder {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]recordedOrder, len(d.orders))
	copy(out, d.orders)
	return out
}

// httpDo 发起一次带 Bearer 鉴权的 HTTP 请求，返回状态码与响应体。
func httpDo(t *testing.T, method, url, token string, body interface{}) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

// TestBotCrossServiceE2E 验证 bot → spot 跨服务下单：F4 token 透传解析为正确 user_id、
// F1 client_oid 落到下游、订单参数正确。
func TestBotCrossServiceE2E(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "e2e-bot-secret"
	v := middleware.NewTokenVerifier(secret)

	spot := newDownstreamSpot(v)
	spotSrv := httptest.NewServer(spot.routes())
	defer spotSrv.Close()

	store := bot.NewMemStore()
	exec := bot.NewHTTPExecutor(spotSrv.URL, spotSrv.URL)
	svc := bot.NewService(store, nil, exec, bot.Config{}, nil)
	botRtr := gin.New()
	svc.RegisterRoutes(botRtr, v)
	botSrv := httptest.NewServer(botRtr)
	defer botSrv.Close()

	// 用户 42 的 token；bot 创建策略时把这个 token 作为 user_token 授权 bot 代其下单。
	userToken := v.Issue(42, time.Hour)

	// 创建 DCA 策略（F4 鉴权 + 携带 user_token）。
	createBody := map[string]interface{}{
		"name":        "e2e-dca",
		"market":      "spot",
		"symbol":      "BTC_USDT",
		"side":        "buy",
		"type":        "dca",
		"user_token":  userToken,
		"params":      map[string]interface{}{"order_amount": 100, "dca_interval_sec": 60, "dca_amount": 100},
	}
	code, data := httpDo(t, http.MethodPost, botSrv.URL+"/api/v1/bot/strategies", userToken, createBody)
	if code != 200 {
		t.Fatalf("create strategy: status=%d body=%s", code, data)
	}
	var created struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &created); err != nil || created.Data.ID == 0 {
		t.Fatalf("parse strategy id: %v body=%s", err, data)
	}
	strategyID := created.Data.ID

	// 启动策略（F4 归属校验）。
	code, data = httpDo(t, http.MethodPost, fmt.Sprintf("%s/api/v1/bot/strategies/%d/start", botSrv.URL, strategyID), userToken, nil)
	if code != 200 {
		t.Fatalf("start strategy: status=%d body=%s", code, data)
	}

	// 他人（用户 99）尝试启动应被拒（F4 越权）。
	otherToken := v.Issue(99, time.Hour)
	code, _ = httpDo(t, http.MethodPost, fmt.Sprintf("%s/api/v1/bot/strategies/%d/start", botSrv.URL, strategyID), otherToken, nil)
	if code != 400 {
		t.Fatalf("F4: other user start should be rejected, got %d", code)
	}

	// 强制驱动一轮 tick → bot 代用户 42 下单到下游 spot。
	if err := svc.Tick(context.Background(), strategyID); err != nil {
		t.Fatalf("tick: %v", err)
	}

	orders := spot.lastOrders()
	if len(orders) != 1 {
		t.Fatalf("F1/F4: want 1 downstream order, got %d", len(orders))
	}
	o := orders[0]
	// F4：下游解析出的 user_id 必须等于被代用户 42（token 透传正确，无越权）。
	if o.userID != 42 {
		t.Errorf("F4: downstream resolved user_id=%d, want 42", o.userID)
	}
	// F1：client_oid 形如 bot:<id>:0，供下游去重。
	if !strings.HasPrefix(o.clientOID, fmt.Sprintf("bot:%d:", strategyID)) {
		t.Errorf("F1: downstream client_oid=%q, want prefix bot:%d:", o.clientOID, strategyID)
	}
	if o.symbol != "BTC_USDT" || o.side != "buy" {
		t.Errorf("order params mismatch: %+v", o)
	}
}

// TestCopytradeCrossServiceE2E 验证 copytrade → spot 跨服务复制：lead 成交触发粉丝复制、
// F4 粉丝 token 透传、F1 client_oid 落地、平台复制费结算入 SysCopyTradeFee。
func TestCopytradeCrossServiceE2E(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "e2e-ct-secret"
	v := middleware.NewTokenVerifier(secret)

	spot := newDownstreamSpot(v)
	spotSrv := httptest.NewServer(spot.routes())
	defer spotSrv.Close()

	// copytrade 自身账本：种子充值粉丝(用户 7)的 USDT，供平台复制费结算。
	lg := ledger.New()
	_ = lg.ReceiveOnChain(7, "USDT", settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("USDT")), "seed")

	store := copytrade.NewMemStore()
	exec := copytrade.NewHTTPExecutor(spotSrv.URL, spotSrv.URL)
	svc := copytrade.NewService(store, lg, exec, copytrade.Config{MinNotional: 1, CopyFeeRate: 0.001}, nil)
	ctRtr := gin.New()
	svc.RegisterRoutes(ctRtr, v)
	ctSrv := httptest.NewServer(ctRtr)
	defer ctSrv.Close()

	leadToken := v.Issue(10, time.Hour)    // 带单高手 = 用户 10
	followerToken := v.Issue(7, time.Hour)  // 粉丝 = 用户 7

	// 用户 10 注册为带单高手（F4）。
	code, data := httpDo(t, http.MethodPost, ctSrv.URL+"/api/v1/copytrade/leads", leadToken, map[string]string{"name": "alice"})
	if code != 200 {
		t.Fatalf("create lead: status=%d body=%s", code, data)
	}

	// 用户 7 关注用户 10，授权 copytrade 以其 token 代下单（F4）。
	followBody := map[string]interface{}{"lead_id": 10, "copy_ratio": 0.5, "allocated_amount": 0, "follower_token": followerToken}
	code, data = httpDo(t, http.MethodPost, ctSrv.URL+"/api/v1/copytrade/follows", followerToken, followBody)
	if code != 200 {
		t.Fatalf("follow: status=%d body=%s", code, data)
	}

	// 模拟撮合引擎发布一笔用户 10 作为 taker 的成交（即订阅者回调入口）。
	ev := mq.TradeEvent{Symbol: "BTC_USDT", Price: 10000, Qty: 2, TakerID: 10, MakerID: 99, TakerSide: "buy", Ts: 1700000000000}
	svc.OnTrade(context.Background(), ev)

	orders := spot.lastOrders()
	if len(orders) != 1 {
		t.Fatalf("F1/F4: want 1 follower order on spot, got %d", len(orders))
	}
	o := orders[0]
	// F4：下游解析出的 user_id 必须是粉丝 7（不是 lead 10），证明复制下单以粉丝身份执行。
	if o.userID != 7 {
		t.Errorf("F4: follower order resolved user_id=%d, want 7", o.userID)
	}
	if o.side != "buy" {
		t.Errorf("F4: copied side=%s, want buy (same as lead taker)", o.side)
	}
	if !strings.HasPrefix(o.clientOID, "copytrade:") {
		t.Errorf("F1: follower order client_oid=%q, want copytrade: prefix", o.clientOID)
	}

	// 平台复制费 = lead 名义额(20000) * ratio(0.5) * feeRate(0.001) = 10 USDT 结算入 SysCopyTradeFee。
	feeAvail, _, ok := lg.Balance(ledger.SysCopyTradeFee, "USDT")
	if !ok || feeAvail.Value == nil {
		t.Fatalf("SysCopyTradeFee not credited; ok=%v", ok)
	}
	if feeAvail.Value.Int64() != 10*int64(1e6) {
		t.Errorf("SysCopyTradeFee balance: want 10 USDT, got %v", feeAvail)
	}

	// F1 幂等：同一成交事件再次到达，下游不应再出现新订单。
	svc.OnTrade(context.Background(), ev)
	if len(spot.lastOrders()) != 1 {
		t.Errorf("F1: duplicate event should not re-replicate, downstream orders=%d", len(spot.lastOrders()))
	}
}

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

	"github.com/coldlar/crypto-exchange/internal/adminapi"
	"github.com/coldlar/crypto-exchange/internal/bot"
	"github.com/coldlar/crypto-exchange/internal/copytrade"
	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/lending"
	"github.com/coldlar/crypto-exchange/internal/notification"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
	"github.com/coldlar/crypto-exchange/internal/services/user"
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

// TestLendingCrossServiceE2E 验证借贷服务完整流程：创建池 → 存款 → 借款 → 还款 → 取回，
// 全程经 ledger 复式记账，资金安全由 ledger 不变量保证。
func TestLendingCrossServiceE2E(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "e2e-lending-secret"
	v := middleware.NewTokenVerifier(secret)

	lg := ledger.New()
	// 种子充值：用户 1 有 10000 USDT 可供出借，用户 2 有 10000 USDT 可作抵押。
	_ = lg.ReceiveOnChain(1, "USDT", settlement.AssetAmountFromFloat(10000, settlement.AssetDecimalsByName("USDT")), "seed:user1")
	_ = lg.ReceiveOnChain(2, "USDT", settlement.AssetAmountFromFloat(10000, settlement.AssetDecimalsByName("USDT")), "seed:user2")

	store := lending.NewMemStore()
	svc := lending.NewService(store, lg, lending.Config{
		BaseInterestRate: 0.05,
		MaxInterestRate:  1.0,
	}, nil)

	rtr := gin.New()
	svc.RegisterRoutes(rtr, v)
	srv := httptest.NewServer(rtr)
	defer srv.Close()

	user1 := v.Issue(1, time.Hour)
	user2 := v.Issue(2, time.Hour)

	// 1) 创建借贷池（用户1也做管理员用途，这里直接用 store 操作绕过 admin guard）。
	p := &lending.LendingPool{
		Asset:         "USDT",
		InterestRate:  0.05,
		CollateralReq: 1.5,
		Status:        lending.PoolActive,
		CreatedAt:     time.Now().Unix(),
	}
	if err := store.CreatePool(p); err != nil {
		t.Fatal(err)
	}

	// 2) 用户1 存入 5000 USDT。
	code, data := httpDo(t, http.MethodPost, srv.URL+"/api/v1/lending/lend", user1, map[string]interface{}{
		"pool_id": p.ID,
		"amount":  "5000",
	})
	if code != 200 {
		t.Fatalf("lend: status=%d body=%s", code, data)
	}

	// 3) 用户2 借入 1000 USDT（抵押 1500 USDT = 150%）。
	code, data = httpDo(t, http.MethodPost, srv.URL+"/api/v1/lending/borrow", user2, map[string]interface{}{
		"pool_id":       p.ID,
		"borrow_amount": "1000",
		"collateral":    "1500",
	})
	if code != 200 {
		t.Fatalf("borrow: status=%d body=%s", code, data)
	}
	var borrowResp struct {
		Data struct {
			ID int64 `json:"id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(data, &borrowResp)
	borrowID := borrowResp.Data.ID

	// 4) 验证池状态更新。
	code, data = httpDo(t, http.MethodGet, srv.URL+fmt.Sprintf("/api/v1/lending/pools/%d", p.ID), user1, nil)
	if code != 200 {
		t.Fatalf("pool info: status=%d body=%s", code, data)
	}

	// 5) 用户2 还款。
	code, data = httpDo(t, http.MethodPost, srv.URL+fmt.Sprintf("/api/v1/lending/repay/%d", borrowID), user2, nil)
	if code != 200 {
		t.Fatalf("repay: status=%d body=%s", code, data)
	}

	// 6) 用户1 取回存款（查看自己的存款列表）。
	code, data = httpDo(t, http.MethodGet, srv.URL+"/api/v1/lending/my/lends", user1, nil)
	if code != 200 {
		t.Fatalf("my lends: status=%d body=%s", code, data)
	}
	var myLends struct {
		Data struct {
			Lends []struct {
				ID     int64  `json:"id"`
				Status string `json:"status"`
			} `json:"lends"`
		} `json:"data"`
	}
	_ = json.Unmarshal(data, &myLends)
	if len(myLends.Data.Lends) != 1 {
		t.Fatalf("expected 1 lend order, got %d", len(myLends.Data.Lends))
	}
	lendID := myLends.Data.Lends[0].ID

	// 7) 用户1 取回资金。
	code, data = httpDo(t, http.MethodPost, srv.URL+fmt.Sprintf("/api/v1/lending/withdraw/%d", lendID), user1, nil)
	if code != 200 {
		t.Fatalf("withdraw: status=%d body=%s", code, data)
	}

	// 8) 验证池恢复为空。
	code, data = httpDo(t, http.MethodGet, srv.URL+fmt.Sprintf("/api/v1/lending/pools/%d", p.ID), user1, nil)
	if code != 200 {
		t.Fatalf("pool info after: status=%d body=%s", code, data)
	}

	// 9) 验证用户2 的借款已还清。
	code, data = httpDo(t, http.MethodGet, srv.URL+"/api/v1/lending/my/borrows", user2, nil)
	if code != 200 {
		t.Fatalf("my borrows: status=%d body=%s", code, data)
	}
	var myBorrows struct {
		Data struct {
			Borrows []struct {
				ID     int64  `json:"id"`
				Status string `json:"status"`
			} `json:"borrows"`
		} `json:"data"`
	}
	_ = json.Unmarshal(data, &myBorrows)
	if len(myBorrows.Data.Borrows) != 1 || myBorrows.Data.Borrows[0].Status != "repaid" {
		t.Fatalf("expected 1 repaid borrow, got %+v", myBorrows.Data.Borrows)
	}
}

// TestAdminLendingBotProxyE2E 验证管理后台 → lending/bot 服务的完整代理链路：
// 真实启动 lending 与 bot 服务，adminapi 将 /api/admin/lending/* 和 /api/admin/bot/*
// 代理到上游，端到端验证数据透传正确。
func TestAdminLendingBotProxyE2E(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "e2e-admin-proxy-secret"
	v := middleware.NewTokenVerifier(secret)

	// ---- 启动真实 lending 服务 ----
	lg := ledger.New()
	_ = lg.ReceiveOnChain(1, "USDT", settlement.AssetAmountFromFloat(100000, settlement.AssetDecimalsByName("USDT")), "seed:admin-proxy")
	lendStore := lending.NewMemStore()
	lendSvc := lending.NewService(lendStore, lg, lending.Config{
		BaseInterestRate: 0.05,
		MaxInterestRate:  1.0,
	}, nil)
	lendRouter := gin.New()
	lendSvc.RegisterRoutes(lendRouter, v)
	lendSrv := httptest.NewServer(lendRouter)
	defer lendSrv.Close()

	// ---- 启动 bot 服务 ----
	botStore := bot.NewMemStore()
	botSvc := bot.NewService(botStore, nil, nil, bot.Config{}, nil)
	botRouter := gin.New()
	botSvc.RegisterRoutes(botRouter, v)
	botSrv := httptest.NewServer(botRouter)
	defer botSrv.Close()

	// ---- 配置 adminapi 指向真实上游 ----
	adminCfg := &config.Config{}
	adminCfg.Auth.Secret = secret
	adminCfg.Admin.Username = "admin"
	adminCfg.Admin.Password = "***REDACTED***"
	adminCfg.Admin.TokenTTLSec = 3600
	adminCfg.Services = map[string]string{
		"lending": lendSrv.URL,
		"bot":     botSrv.URL,
	}
	adminRouter := gin.New()
	adminapi.NewServer(adminCfg).RegisterRoutes(adminRouter)

	// 登录拿 admin token
	adminTok := e2eLoginAdmin(t, adminRouter)
	if adminTok == "" {
		t.Fatal("failed to get admin token")
	}

	// ---- 测试 lending 代理链路 ----
	// 1) POST /api/admin/lending/pools → 创建资金池
	code, body := e2eAdminDo(t, adminRouter, http.MethodPost, "/api/admin/lending/pools", adminTok, map[string]interface{}{
		"asset":          "USDT",
		"collateral_req": 1.5,
	})
	if code != http.StatusOK {
		t.Fatalf("create pool via proxy: status=%d body=%s", code, body)
	}
	var createResp struct {
		Data struct {
			Pool struct {
				ID    int64  `json:"id"`
				Asset string `json:"asset"`
			} `json:"pool"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &createResp)
	poolID := createResp.Data.Pool.ID
	if poolID == 0 {
		t.Fatalf("expected non-zero pool id, got body=%s", body)
	}

	// 2) GET /api/admin/lending/pools → 代理查询资金池
	code, body = e2eAdminDo(t, adminRouter, http.MethodGet, "/api/admin/lending/pools", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("list pools via proxy: status=%d body=%s", code, body)
	}
	var poolsResp struct {
		Data struct {
			Pools []struct {
				ID    int64  `json:"id"`
				Asset string `json:"asset"`
			} `json:"pools"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &poolsResp)
	if len(poolsResp.Data.Pools) != 1 || poolsResp.Data.Pools[0].Asset != "USDT" {
		t.Fatalf("expected 1 USDT pool via proxy, got %+v", poolsResp.Data.Pools)
	}

	// 3) GET /api/admin/lending/lends → 代理查询存款订单（应为空）
	code, body = e2eAdminDo(t, adminRouter, http.MethodGet, "/api/admin/lending/lends", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("list lends via proxy: status=%d body=%s", code, body)
	}

	// 4) GET /api/admin/lending/borrows → 代理查询借款订单（应为空）
	code, body = e2eAdminDo(t, adminRouter, http.MethodGet, "/api/admin/lending/borrows", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("list borrows via proxy: status=%d body=%s", code, body)
	}

	// ---- 测试 bot 代理链路 ----
	// 5) GET /api/admin/bot/strategies → 代理查询策略列表（应为空）
	code, body = e2eAdminDo(t, adminRouter, http.MethodGet, "/api/admin/bot/strategies", adminTok, nil)
	if code != http.StatusOK {
		t.Fatalf("list strategies via proxy: status=%d body=%s", code, body)
	}
	var stratsResp struct {
		Data struct {
			Strategies []interface{} `json:"strategies"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &stratsResp)
	if len(stratsResp.Data.Strategies) != 0 {
		t.Fatalf("expected 0 strategies, got %d", len(stratsResp.Data.Strategies))
	}
}

// e2eLoginAdmin 登录管理后台并返回 admin token。
func e2eLoginAdmin(t *testing.T, r *gin.Engine) string {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"username": "admin", "password": "***REDACTED***"})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/login", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("admin login failed: %d %s", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return env.Data.Token
}

// e2eAdminDo 发送带 admin token 的请求并返回状态码与原始响应体。
func e2eAdminDo(t *testing.T, r *gin.Engine, method, path, token string, body interface{}) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

// TestSecurityCenterE2E 验证用户服务「安全中心」四组端点在进程内的完整闭环
// （API Key 创建+列出、登录历史、会话列表、防钓鱼码读写）。与线上一致需 Bearer 鉴权。
func TestSecurityCenterE2E(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "e2e-security-secret"
	v := middleware.NewTokenVerifier(secret)

	store := user.NewMemStore()
	// notifSvc 共享实例：便于安全中心与通知中心在同一进程内闭环（KYC 审核结果会写入此处）。
	notifSvc := notification.New(notification.NewMemStore())
	svc := user.NewService(store, v, user.NewLogNotifier(), notifSvc, user.Config{})
	h := user.NewHandler(svc, v)
	r := gin.New()
	h.Register(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	token := v.Issue(1, time.Hour)

	// 1) 创建 API Key
	code, body := httpDo(t, http.MethodPost, srv.URL+"/api/v1/user/api-keys", token,
		map[string]interface{}{"label": "e2e-key", "permissions": []string{"read", "trade"}})
	if code != http.StatusOK {
		t.Fatalf("create api-key: %d %s", code, body)
	}
	var created struct {
		Data struct {
			ApiKey struct {
				ID int64 `json:"id"`
			} `json:"api_key"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("parse api-key: %v", err)
	}
	if created.Data.ApiKey.ID == 0 {
		t.Fatalf("api-key id missing: %s", body)
	}

	// 2) 列出 API Key（应含刚创建的）
	code, body = httpDo(t, http.MethodGet, srv.URL+"/api/v1/user/api-keys", token, nil)
	if code != http.StatusOK {
		t.Fatalf("list api-keys: %d %s", code, body)
	}
	var list struct {
		Data struct {
			ApiKeys []struct {
				ID    int64    `json:"id"`
				Label string    `json:"label"`
				Key   string    `json:"key"`
				Perms []string  `json:"permissions"`
			} `json:"api_keys"`
			Total int `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("parse api-key list: %v", err)
	}
	if list.Data.Total != 1 || list.Data.ApiKeys[0].Label != "e2e-key" {
		t.Fatalf("api-key list unexpected: %s", body)
	}

	// 3) 登录历史（mem store 初始为空，验证端点接通）
	code, body = httpDo(t, http.MethodGet, srv.URL+"/api/v1/user/login-history", token, nil)
	if code != http.StatusOK {
		t.Fatalf("login-history: %d %s", code, body)
	}

	// 4) 会话列表（mem store 初始为空）
	code, body = httpDo(t, http.MethodGet, srv.URL+"/api/v1/user/sessions", token, nil)
	if code != http.StatusOK {
		t.Fatalf("sessions: %d %s", code, body)
	}

	// 5) 防钓鱼码：初始应为空
	code, body = httpDo(t, http.MethodGet, srv.URL+"/api/v1/user/anti-phishing", token, nil)
	if code != http.StatusOK {
		t.Fatalf("anti-phishing get: %d %s", code, body)
	}
	var ap struct {
		Data struct {
			Code string `json:"code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &ap); err != nil {
		t.Fatalf("parse anti-phishing: %v", err)
	}
	if ap.Data.Code != "" {
		t.Fatalf("expected empty anti-phishing initially, got %q", ap.Data.Code)
	}

	// 6) 设置防钓鱼码后应回读一致
	code, body = httpDo(t, http.MethodPost, srv.URL+"/api/v1/user/anti-phishing", token,
		map[string]interface{}{"code": "CE-E2E"})
	if code != http.StatusOK {
		t.Fatalf("anti-phishing set: %d %s", code, body)
	}
	code, body = httpDo(t, http.MethodGet, srv.URL+"/api/v1/user/anti-phishing", token, nil)
	if code != http.StatusOK {
		t.Fatalf("anti-phishing get2: %d %s", code, body)
	}
	if err := json.Unmarshal(body, &ap); err != nil {
		t.Fatalf("parse anti-phishing2: %v", err)
	}
	if ap.Data.Code != "CE-E2E" {
		t.Fatalf("expected anti-phishing CE-E2E, got %q", ap.Data.Code)
	}
}

// TestNotificationCenterE2E 验证通知中心用户侧读链路：生产者（以同一 notifSvc 直接 Publish
// 模拟强平/充值等业务事件）→ 经 /api/v1/user/notifications* 端点读取、未读数、全部已读 全闭环。
// 与 integration.sh 不同，本测试共享同一 notification.Service 实例，可做真正的跨「生产→消费」往返。
func TestNotificationCenterE2E(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "e2e-notif-secret"
	v := middleware.NewTokenVerifier(secret)

	notifSvc := notification.New(notification.NewMemStore())
	nh := notification.NewHandler(notifSvc)
	r := gin.New()
	r.Use(middleware.Auth(v)) // 与 cmd/notification/main.go 一致：引擎级挂载鉴权
	nh.RegisterRoutes(r)
	srv := httptest.NewServer(r)
	defer srv.Close()

	token := v.Issue(1, time.Hour)

	// 生产者侧：模拟业务事件写入（强平通知）
	if _, err := notifSvc.Publish(notification.PublishInput{
		UserID: 1, Type: notification.TypeLiquidation,
		Title: "合约仓位被强平", Body: "您的 BTC_USDT_PERP 多头已被强平",
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// 1) 列出通知：应含 1 条
	code, body := httpDo(t, http.MethodGet, srv.URL+"/api/v1/user/notifications", token, nil)
	if code != http.StatusOK {
		t.Fatalf("list: %d %s", code, body)
	}
	var list struct {
		Data struct {
			Notifications []struct {
				ID      int64  `json:"id"`
				Title   string `json:"title"`
				Content string `json:"content"`
			} `json:"notifications"`
			Unread int64 `json:"unread"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("parse list: %v", err)
	}
	if len(list.Data.Notifications) != 1 || list.Data.Notifications[0].Title != "合约仓位被强平" {
		t.Fatalf("notification list unexpected: %s", body)
	}
	if list.Data.Unread != 1 {
		t.Fatalf("expected unread 1, got %d", list.Data.Unread)
	}

	// 2) 未读数
	code, body = httpDo(t, http.MethodGet, srv.URL+"/api/v1/user/notifications/unread-count", token, nil)
	if code != http.StatusOK {
		t.Fatalf("unread-count: %d %s", code, body)
	}
	var uc struct {
		Data struct {
			Count int64 `json:"count"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &uc); err != nil {
		t.Fatalf("parse unread-count: %v", err)
	}
	if uc.Data.Count != 1 {
		t.Fatalf("expected count 1, got %d", uc.Data.Count)
	}

	// 3) 全部已读
	code, body = httpDo(t, http.MethodPost, srv.URL+"/api/v1/user/notifications/read-all", token, nil)
	if code != http.StatusOK {
		t.Fatalf("read-all: %d %s", code, body)
	}
	code, body = httpDo(t, http.MethodGet, srv.URL+"/api/v1/user/notifications/unread-count", token, nil)
	if code != http.StatusOK {
		t.Fatalf("unread-count2: %d %s", code, body)
	}
	if err := json.Unmarshal(body, &uc); err != nil {
		t.Fatalf("parse unread-count2: %v", err)
	}
	if uc.Data.Count != 0 {
		t.Fatalf("expected count 0 after read-all, got %d", uc.Data.Count)
	}
}

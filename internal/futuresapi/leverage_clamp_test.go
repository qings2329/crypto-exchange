package futuresapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/futures"
	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/ws"
)

// stubMatcher 是最小撮合桩：Submit 恒成功，其余接口未用。
type stubMatcher struct{ submitted []*matching.Order }

func (m *stubMatcher) Submit(symbol string, o *matching.Order) bool { m.submitted = append(m.submitted, o); return true }
func (m *stubMatcher) Depth(symbol string) (bids, asks []matching.Level, ok bool) {
	return nil, nil, false
}
func (m *stubMatcher) MatchNow(symbol string, o *matching.Order, rest bool) ([]matching.Trade, bool) {
	return nil, true
}
func (m *stubMatcher) Cancel(symbol string, orderID int64) bool { return true }
func (m *stubMatcher) ListOrders(userID int64, symbol, status string, limit int) []matching.OrderView {
	return nil
}
func (m *stubMatcher) GetOrder(orderID int64) (matching.OrderView, bool) {
	return matching.OrderView{}, false
}
func (m *stubMatcher) ListTrades(userID int64, symbol string, limit int) []matching.TradeView {
	return nil
}

// clampServer 构造一个仅含杠杆钳制所需依赖的最小 Server + 路由。
func clampServer(maxLev int) (*Server, *gin.Engine, *middleware.TokenVerifier) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Futures.MaxLeverage = maxLev
	s := &Server{
		cfg:        cfg,
		matcher:    &stubMatcher{},
		ledgerSvc:  ledger.New(),
		liquidator: futures.NewLiquidator(func(ev futures.LiquidationEvent) {}),
		hub:        ws.NewHub(),
	}
	verifier := middleware.NewTokenVerifier("test-secret")
	r := gin.New()
	r.Use(middleware.AuthWithSkips(verifier, "/metrics"))
	r.POST("/api/v1/futures/order", s.handleOrder)
	return s, r, verifier
}

// TestOpenRejectsOverMaxLeverage 验证服务端杠杆钳制：超过 MaxLeverage 的开仓单必须被 400 拒绝，
// 严防绕过前端滑条上限（1-125x）直接调 API 抬升高杠杆敞口。price=0 时链路只走到杠钳制+Submit，
// 无需 ledger/liquidator.Book 依赖，便于孤立验证钳制本身。
func TestOpenRejectsOverMaxLeverage(t *testing.T) {
	cases := []struct {
		name string
		lev  string
		max  int
		code int
	}{
		{"at_max_ok", "125", 125, http.StatusOK},
		{"below_max_ok", "20", 125, http.StatusOK},
		{"zero_defaults_ok", "0", 125, http.StatusOK},
		{"negative_defaults_ok", "-5", 125, http.StatusOK},
		{"over_max_rejected", "126", 125, http.StatusBadRequest},
		{"over_max_threshold", "200", 125, http.StatusBadRequest},
		{"custom_max_respected", "5", 10, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, r, verifier := clampServer(tc.max)
			tok := verifier.Issue(1, time.Hour)
			body := `{"symbol":"BTC_USDT_PERP","action":"open","pos_side":"long","margin_mode":"isolated","leverage":` + tc.lev + `,"price":0,"qty":1}`
			w := httptest.NewRecorder()
			req, _ := http.NewRequest(http.MethodPost, "/api/v1/futures/order", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+tok)
			r.ServeHTTP(w, req)
			if w.Code != tc.code {
				t.Fatalf("leverage=%s max=%d: expected %d, got %d: %s", tc.lev, tc.max, tc.code, w.Code, w.Body.String())
			}
		})
	}
}

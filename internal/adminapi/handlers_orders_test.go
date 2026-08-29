package adminapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/adminapi"
	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
)

// newFakeMatching 启动一个模拟 cmd/matching 的 REST 服务（仅订单管理相关端点），
// 返回服务与其内部引擎，便于在测试中预置订单/成交后验证管理后台代理契约。
func newFakeMatching(t *testing.T) (*httptest.Server, *matching.Engine) {
	t.Helper()
	e := matching.NewEngine(nil, nil)
	e.Register("BTC_USDT")

	mux := http.NewServeMux()
	mux.HandleFunc("/orders/", func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/orders/"), 10, 64)
		if v, ok := e.GetOrder(id); ok {
			writeEnv(w, v)
		} else {
			w.WriteHeader(404)
			writeEnv(w, nil)
		}
	})
	mux.HandleFunc("/orders", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		uid, _ := strconv.ParseInt(q.Get("user_id"), 10, 64)
		writeEnv(w, e.ListOrders(uid, q.Get("symbol"), q.Get("status"), atoi(q.Get("limit"))))
	})
	mux.HandleFunc("/trades", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		uid, _ := strconv.ParseInt(q.Get("user_id"), 10, 64)
		writeEnv(w, e.ListTrades(uid, q.Get("symbol"), atoi(q.Get("limit"))))
	})
	mux.HandleFunc("/cancel", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Symbol  string `json:"symbol"`
			OrderID int64  `json:"order_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		writeEnv(w, map[string]bool{"canceled": e.Cancel(req.Symbol, req.OrderID)})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, e
}

func writeEnv(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"code": 0, "message": "ok", "data": data})
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// loginAdmin 用种子超级管理员凭据登录，返回 admin token（含全部权限，含 trade:read/trade:manage）。
func loginAdmin(t *testing.T, r *gin.Engine) string {
	t.Helper()
	code, data := postJSON(t, r, "/api/admin/login", "", map[string]string{
		"username": "admin", "password": "***REDACTED***",
	})
	if code != http.StatusOK {
		t.Fatalf("admin login failed: code=%d", code)
	}
	tok, _ := data.(map[string]interface{})["token"].(string)
	if tok == "" {
		t.Fatal("empty admin token")
	}
	return tok
}

// TestAdminOrderManagement 验证管理后台跨用户订单管理代理：读需 trade:read、撤销需 trade:manage。
func TestAdminOrderManagement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	match, eng := newFakeMatching(t)

	// 预置：用户1现货挂单（普通，成交），用户2合约吃单（杠杆单，成交）。
	if _, _ = eng.MatchNow("BTC_USDT", &matching.Order{
		ID: 1, UserID: 1, Side: matching.Buy, Price: matching.FixedFromFloat(100, 2), Qty: matching.FixedFromFloat(1, 8), Time: 1, Market: "spot",
	}, true); false {
	}
	if _, _ = eng.MatchNow("BTC_USDT", &matching.Order{
		ID: 2, UserID: 2, Side: matching.Sell, Price: matching.Fixed{}, Qty: matching.FixedFromFloat(1, 8), Time: 2, Market: "futures", IsMargin: true, Leverage: 10,
	}, false); false {
	}

	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "***REDACTED***"
	cfg.Admin.TokenTTLSec = 3600
	cfg.Matching.URL = match.URL
	r := gin.New()
	adminapi.NewServer(cfg).RegisterRoutes(r)

	tok := loginAdmin(t, r)

	// 列表（全部用户）：应含2条。
	code, data := getJSON(t, r, "/api/admin/orders", tok)
	if code != http.StatusOK {
		t.Fatalf("GET /orders expected 200, got %d", code)
	}
	orders := data.(map[string]interface{})["orders"].([]interface{})
	if len(orders) != 2 {
		t.Fatalf("expected 2 orders via admin, got %d", len(orders))
	}

	// 杠杆过滤：?margin=1 仅合约杠杆单（1条），?margin=0 仅普通现货单（1条）。
	code, data = getJSON(t, r, "/api/admin/orders?margin=1", tok)
	if code != http.StatusOK {
		t.Fatalf("GET /orders?margin=1 expected 200, got %d", code)
	}
	if lev := data.(map[string]interface{})["orders"].([]interface{}); len(lev) != 1 {
		t.Fatalf("expected 1 margin order, got %d", len(lev))
	}
	code, data = getJSON(t, r, "/api/admin/orders?margin=0", tok)
	if code != http.StatusOK {
		t.Fatalf("GET /orders?margin=0 expected 200, got %d", code)
	}
	if plain := data.(map[string]interface{})["orders"].([]interface{}); len(plain) != 1 {
		t.Fatalf("expected 1 plain order, got %d", len(plain))
	}

	// 详情。
	code, data = getJSON(t, r, "/api/admin/orders/1", tok)
	if code != http.StatusOK {
		t.Fatalf("GET /orders/1 expected 200, got %d", code)
	}
	od := data.(map[string]interface{})["order"].(map[string]interface{})
	if od["market"] != "spot" {
		t.Fatalf("expected market=spot, got %v", od["market"])
	}

	// 成交列表。
	code, data = getJSON(t, r, "/api/admin/trades?market=futures", tok)
	if code != http.StatusOK {
		t.Fatalf("GET /trades expected 200, got %d", code)
	}
	trades := data.(map[string]interface{})["trades"].([]interface{})
	if len(trades) != 1 {
		t.Fatalf("expected 1 futures trade, got %d", len(trades))
	}
	// 成交按杠杆过滤：唯一成交来自合约杠杆单，?margin=1 返回1条，?margin=0 返回0条。
	code, data = getJSON(t, r, "/api/admin/trades?margin=1", tok)
	if code != http.StatusOK {
		t.Fatalf("GET /trades?margin=1 expected 200, got %d", code)
	}
	if lev := data.(map[string]interface{})["trades"].([]interface{}); len(lev) != 1 {
		t.Fatalf("expected 1 margin trade, got %d", len(lev))
	}
	code, data = getJSON(t, r, "/api/admin/trades?margin=0", tok)
	if code != http.StatusOK {
		t.Fatalf("GET /trades?margin=0 expected 200, got %d", code)
	}
	if plain := data.(map[string]interface{})["trades"].([]interface{}); len(plain) != 0 {
		t.Fatalf("expected 0 plain trades, got %d", len(plain))
	}

	// 无 token → 401。
	code, _ = getJSON(t, r, "/api/admin/orders", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", code)
	}
}

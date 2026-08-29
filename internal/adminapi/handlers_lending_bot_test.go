package adminapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/coldlar/crypto-exchange/internal/adminapi"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
)

// newFakeUpstream 启动一个模拟上游服务，返回标准 {code:0, data:...} 信封。
func newFakeUpstream(t *testing.T, path string, data interface{}) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 0, "message": "ok", "data": data,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newFakeUpstreamPost 处理 POST 请求的上游桩。
func newFakeUpstreamPost(t *testing.T, path string, respData interface{}) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"code": 0, "message": "ok", "data": respData,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestAdminLendingPoolsProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fakeLending := newFakeUpstream(t, "/api/v1/lending/admin/pools", map[string]interface{}{
		"pools": []map[string]interface{}{
			{"id": 1, "asset": "USDT", "status": "active"},
		},
	})

	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "***REDACTED***"
	cfg.Admin.TokenTTLSec = 3600
	cfg.Services = map[string]string{"lending": fakeLending.URL}

	r := gin.New()
	adminapi.NewServer(cfg).RegisterRoutes(r)

	tok := loginAdmin(t, r)
	code, data := getJSON(t, r, "/api/admin/lending/pools", tok)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	d := data.(map[string]interface{})
	pools := d["pools"].([]interface{})
	if len(pools) != 1 {
		t.Fatalf("expected 1 pool, got %d", len(pools))
	}
}

func TestAdminLendingLendsProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fakeLending := newFakeUpstream(t, "/api/v1/lending/admin/lends", map[string]interface{}{
		"lends": []map[string]interface{}{
			{"id": 1, "user_id": 1, "amount": "1000"},
			{"id": 2, "user_id": 2, "amount": "2000"},
		},
	})

	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "***REDACTED***"
	cfg.Admin.TokenTTLSec = 3600
	cfg.Services = map[string]string{"lending": fakeLending.URL}

	r := gin.New()
	adminapi.NewServer(cfg).RegisterRoutes(r)

	tok := loginAdmin(t, r)
	code, data := getJSON(t, r, "/api/admin/lending/lends", tok)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	d := data.(map[string]interface{})
	lends := d["lends"].([]interface{})
	if len(lends) != 2 {
		t.Fatalf("expected 2 lends, got %d", len(lends))
	}
}

func TestAdminLendingBorrowsProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fakeLending := newFakeUpstream(t, "/api/v1/lending/admin/borrows", map[string]interface{}{
		"borrows": []map[string]interface{}{
			{"id": 1, "user_id": 1, "status": "active"},
		},
	})

	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "***REDACTED***"
	cfg.Admin.TokenTTLSec = 3600
	cfg.Services = map[string]string{"lending": fakeLending.URL}

	r := gin.New()
	adminapi.NewServer(cfg).RegisterRoutes(r)

	tok := loginAdmin(t, r)
	code, data := getJSON(t, r, "/api/admin/lending/borrows", tok)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	d := data.(map[string]interface{})
	borrows := d["borrows"].([]interface{})
	if len(borrows) != 1 {
		t.Fatalf("expected 1 borrow, got %d", len(borrows))
	}
}

func TestAdminBotStrategiesProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fakeBot := newFakeUpstream(t, "/api/v1/bot/admin/strategies", map[string]interface{}{
		"strategies": []map[string]interface{}{
			{"id": 1, "name": "grid-btc", "symbol": "BTC_USDT", "status": "active"},
			{"id": 2, "name": "grid-eth", "symbol": "ETH_USDT", "status": "stopped"},
		},
	})

	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "***REDACTED***"
	cfg.Admin.TokenTTLSec = 3600
	cfg.Services = map[string]string{"bot": fakeBot.URL}

	r := gin.New()
	adminapi.NewServer(cfg).RegisterRoutes(r)

	tok := loginAdmin(t, r)
	code, data := getJSON(t, r, "/api/admin/bot/strategies", tok)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	d := data.(map[string]interface{})
	strats := d["strategies"].([]interface{})
	if len(strats) != 2 {
		t.Fatalf("expected 2 strategies, got %d", len(strats))
	}
}

func TestAdminBotTickProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fakeBot := newFakeUpstreamPost(t, "/api/v1/bot/admin/strategies/5/tick", map[string]interface{}{
		"id": 5, "status": "tick submitted",
	})

	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "***REDACTED***"
	cfg.Admin.TokenTTLSec = 3600
	cfg.Services = map[string]string{"bot": fakeBot.URL}

	r := gin.New()
	adminapi.NewServer(cfg).RegisterRoutes(r)

	tok := loginAdmin(t, r)
	code, data := postJSON(t, r, "/api/admin/bot/strategies/5/tick", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	d := data.(map[string]interface{})
	if d["status"] != "tick submitted" {
		t.Fatalf("expected tick submitted, got %v", d["status"])
	}
}

func TestAdminLendingUpstreamUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "***REDACTED***"
	cfg.Admin.TokenTTLSec = 3600
	cfg.Services = map[string]string{} // no lending configured

	r := gin.New()
	adminapi.NewServer(cfg).RegisterRoutes(r)

	tok := loginAdmin(t, r)
	// Should return 502 when upstream not configured
	req := httptest.NewRequest(http.MethodGet, "/api/admin/lending/pools", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for missing upstream, got %d", w.Code)
	}
}

func TestAdminBotUpstreamUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "***REDACTED***"
	cfg.Admin.TokenTTLSec = 3600
	cfg.Services = map[string]string{} // no bot configured

	r := gin.New()
	adminapi.NewServer(cfg).RegisterRoutes(r)

	tok := loginAdmin(t, r)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/bot/strategies", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for missing upstream, got %d", w.Code)
	}
}

func TestAdminLendingNoToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fakeLending := newFakeUpstream(t, "/api/v1/lending/admin/pools", map[string]interface{}{"pools": []interface{}{}})

	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "***REDACTED***"
	cfg.Admin.TokenTTLSec = 3600
	cfg.Services = map[string]string{"lending": fakeLending.URL}

	r := gin.New()
	adminapi.NewServer(cfg).RegisterRoutes(r)

	// No token → 401
	req := httptest.NewRequest(http.MethodGet, "/api/admin/lending/pools", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestAdminLendingPoolsCreateProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fakeLending := newFakeUpstreamPost(t, "/api/v1/lending/admin/pools", map[string]interface{}{
		"pool": map[string]interface{}{
			"id": 1, "asset": "BTC", "status": "active",
			"interest_rate": 0.05, "collateral_req": 1.5,
		},
	})

	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "***REDACTED***"
	cfg.Admin.TokenTTLSec = 3600
	cfg.Services = map[string]string{"lending": fakeLending.URL}

	r := gin.New()
	adminapi.NewServer(cfg).RegisterRoutes(r)

	tok := loginAdmin(t, r)
	code, data := postJSON(t, r, "/api/admin/lending/pools", tok, map[string]interface{}{
		"asset": "BTC", "collateral_req": 1.5,
	})
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	d := data.(map[string]interface{})
	pool := d["pool"].(map[string]interface{})
	if pool["asset"] != "BTC" {
		t.Fatalf("expected asset BTC, got %v", pool["asset"])
	}
}

func TestAdminBotTickMissingID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	fakeBot := newFakeUpstreamPost(t, "/api/v1/bot/admin/strategies//tick", nil)

	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "***REDACTED***"
	cfg.Admin.TokenTTLSec = 3600
	cfg.Services = map[string]string{"bot": fakeBot.URL}

	r := gin.New()
	adminapi.NewServer(cfg).RegisterRoutes(r)

	tok := loginAdmin(t, r)
	// Empty ID should be caught by the handler's validation
	req := httptest.NewRequest(http.MethodPost, "/api/admin/bot/strategies//tick", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing id, got %d", w.Code)
	}
}

// Ensure unused import doesn't break
var _ = strings.TrimSpace

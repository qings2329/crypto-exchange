package lending

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

func setupAdminHandler(t *testing.T) (*Service, *MemStore, *gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store := NewMemStore()
	svc := NewService(store, nil, Config{}, nil)

	verifier := middleware.NewTokenVerifier("test-secret")
	// Issue a real admin token (AdminGuard checks role=admin in claims).
	adminTok := verifier.IssueRole(0, "admin", 1*time.Hour)

	r := gin.New()
	svc.RegisterRoutes(r, verifier)
	return svc, store, r, adminTok
}

func adminGetJSON(t *testing.T, r *gin.Engine, path, tok string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var env map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return w.Code, env
}

func TestAdminListPools(t *testing.T) {
	_, store, r, tok := setupAdminHandler(t)

	store.CreatePool(&LendingPool{Asset: "USDT", Status: PoolActive, InterestRate: 0.05, CollateralReq: 1.5})
	store.CreatePool(&LendingPool{Asset: "ETH", Status: PoolClosed, InterestRate: 0.03, CollateralReq: 2.0})

	code, body := adminGetJSON(t, r, "/api/v1/lending/admin/pools", tok)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	data, _ := body["data"].(map[string]interface{})
	pools, _ := data["pools"].([]interface{})
	if len(pools) != 2 {
		t.Fatalf("expected 2 pools, got %d", len(pools))
	}
}

func TestAdminListLends(t *testing.T) {
	_, store, r, tok := setupAdminHandler(t)

	store.CreateLendOrder(&LendOrder{UserID: 1, PoolID: 1, Status: "active"})
	store.CreateLendOrder(&LendOrder{UserID: 2, PoolID: 1, Status: "withdrawn"})

	code, body := adminGetJSON(t, r, "/api/v1/lending/admin/lends", tok)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	data, _ := body["data"].(map[string]interface{})
	lends, _ := data["lends"].([]interface{})
	if len(lends) != 2 {
		t.Fatalf("expected 2 lends, got %d", len(lends))
	}
}

func TestAdminListBorrows(t *testing.T) {
	_, store, r, tok := setupAdminHandler(t)

	store.CreateBorrowOrder(&BorrowOrder{UserID: 1, PoolID: 1, Status: "active"})
	store.CreateBorrowOrder(&BorrowOrder{UserID: 2, PoolID: 1, Status: "repaid"})
	store.CreateBorrowOrder(&BorrowOrder{UserID: 3, PoolID: 1, Status: "liquidated"})

	code, body := adminGetJSON(t, r, "/api/v1/lending/admin/borrows", tok)
	if code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	data, _ := body["data"].(map[string]interface{})
	borrows, _ := data["borrows"].([]interface{})
	if len(borrows) != 3 {
		t.Fatalf("expected 3 borrows, got %d", len(borrows))
	}
}

func TestAdminRoutesRequireAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewMemStore()
	svc := NewService(store, nil, Config{}, nil)
	verifier := middleware.NewTokenVerifier("test-secret")
	r := gin.New()
	svc.RegisterRoutes(r, verifier)

	for _, path := range []string{
		"/api/v1/lending/admin/pools",
		"/api/v1/lending/admin/lends",
		"/api/v1/lending/admin/borrows",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401, got %d", path, w.Code)
		}
	}
}

func TestAdminRoutesRejectNonAdminToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := NewMemStore()
	svc := NewService(store, nil, Config{}, nil)
	verifier := middleware.NewTokenVerifier("test-secret")
	r := gin.New()
	svc.RegisterRoutes(r, verifier)

	// Issue a user-role token (not admin) → should be rejected by AdminGuard
	userTok := verifier.IssueRole(1, "user", 1*time.Hour)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/lending/admin/pools", nil)
	req.Header.Set("Authorization", "Bearer "+userTok)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for user token on admin route, got %d", w.Code)
	}
}

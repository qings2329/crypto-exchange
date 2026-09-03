package adminapi_test

import (
	"strconv"
	"testing"

	"github.com/coldlar/crypto-exchange/internal/adminapi"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/gin-gonic/gin"
)

// newC2CTestServer 构造管理后台测试服务器并预置 C2C 演示订单（内存存储）。
// 返回 gin.Engine 与 super_admin token（拥有 c2c:view / c2c:manage）。
func newC2CTestServer(t *testing.T) (*gin.Engine, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Admin.Username = "admin"
	cfg.Admin.Password = "***REDACTED***"
	cfg.Admin.TokenTTLSec = 3600

	r := gin.New()
	srv := adminapi.NewServer(cfg)
	srv.SeedDemoC2COrders()
	srv.RegisterRoutes(r)

	_, data := postJSON(t, r, "/api/admin/login", "", map[string]string{"username": "admin", "password": "***REDACTED***"})
	tok, _ := data.(map[string]interface{})["token"].(string)
	if tok == "" {
		t.Fatal("expected non-empty admin token")
	}
	return r, tok
}

func TestC2CListAndActions(t *testing.T) {
	r, tok := newC2CTestServer(t)

	code, data := getJSON(t, r, "/api/admin/c2c/orders", tok)
	if code != 200 {
		t.Fatalf("list: got %d, want 200 (data=%v)", code, data)
	}
	obj := data.(map[string]interface{})
	orders, _ := obj["orders"].([]interface{})
	if len(orders) == 0 {
		t.Fatal("expected seeded c2c orders")
	}
	if int(obj["total"].(float64)) == 0 {
		t.Fatal("expected total > 0")
	}

	// 挑一个订单做冻结/完成。种子含 open/locked/completed，视情况先解冻。
	var oid int64
	var status string
	for _, it := range orders {
		m := it.(map[string]interface{})
		status = m["status"].(string)
		oid = int64(m["id"].(float64))
		if status == "open" || status == "locked" {
			break
		}
	}
	if status == "locked" {
		code, _ = postJSON(t, r, "/api/admin/c2c/orders/"+strconv.FormatInt(oid, 10)+"/release", tok, struct{}{})
		if code != 200 {
			t.Fatalf("release: got %d, want 200", code)
		}
	}

	code, data = postJSON(t, r, "/api/admin/c2c/orders/"+strconv.FormatInt(oid, 10)+"/freeze", tok, struct{}{})
	if code != 200 {
		t.Fatalf("freeze: got %d, want 200 (data=%v)", code, data)
	}
	if got := data.(map[string]interface{})["order"].(map[string]interface{})["status"]; got != "locked" {
		t.Fatalf("freeze status = %v, want locked", got)
	}

	code, data = postJSON(t, r, "/api/admin/c2c/orders/"+strconv.FormatInt(oid, 10)+"/complete", tok, struct{}{})
	if code != 200 {
		t.Fatalf("complete: got %d, want 200 (data=%v)", code, data)
	}
	if got := data.(map[string]interface{})["order"].(map[string]interface{})["status"]; got != "completed" {
		t.Fatalf("complete status = %v, want completed", got)
	}
}

func TestC2CListRequiresAuth(t *testing.T) {
	r, _ := newC2CTestServer(t)
	code, _ := getJSON(t, r, "/api/admin/c2c/orders", "")
	if code != 401 {
		t.Fatalf("unauthenticated list: got %d, want 401", code)
	}
}

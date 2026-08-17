package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/coldlar/crypto-exchange/internal/pkg/config"
)

// newTestRouter 构造一个带测试端点的引擎，便于复用各中间件测试。
func newTestRouter(mws ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(mws...)
	r.GET("/ok", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	r.GET("/api/v1/spot/depth", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	r.POST("/api/v1/spot/order", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	r.GET("/metrics", func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) })
	return r
}

func TestSecurityHeaders(t *testing.T) {
	r := newTestRouter(SecurityHeaders())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/ok", nil))

	if w.Code != 200 {
		t.Fatalf("want 200 got %d", w.Code)
	}
	h := w.Header()
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing X-Content-Type-Options: nosniff")
	}
	if h.Get("X-Frame-Options") != "DENY" {
		t.Error("missing X-Frame-Options: DENY")
	}
	if h.Get("Strict-Transport-Security") == "" {
		t.Error("missing Strict-Transport-Security")
	}
	if h.Get("Content-Security-Policy") == "" {
		t.Error("missing Content-Security-Policy")
	}
	if h.Get("Server") != "" {
		t.Error("Server header should be stripped")
	}
}

func TestCORS(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.AllowedOrigins = []string{"https://ex.com"}
	r := newTestRouter(CORS(cfg.Server.AllowedOrigins))

	// 白名单命中：应设置 ACAO。
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ok", nil)
	req.Header.Set("Origin", "https://ex.com")
	r.ServeHTTP(w, req)
	if w.Header().Get("Access-Control-Allow-Origin") != "https://ex.com" {
		t.Error("CORS should allow whitelisted origin")
	}

	// 不在白名单：不应设置 ACAO。
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/ok", nil)
	req2.Header.Set("Origin", "https://evil.com")
	r.ServeHTTP(w2, req2)
	if w2.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("CORS should deny non-whitelisted origin")
	}
}

func TestMaxBodySize(t *testing.T) {
	r := newTestRouter(MaxBodySize(10))
	req := httptest.NewRequest("POST", "/api/v1/spot/order", strings.NewReader("01234567890123456789"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("want 413 got %d", w.Code)
	}

	// 小于上限的请求正常通过。
	req2 := httptest.NewRequest("POST", "/api/v1/spot/order", strings.NewReader("short"))
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Errorf("small body should pass, got %d", w2.Code)
	}
}

func TestAudit(t *testing.T) {
	level := zap.NewAtomicLevelAt(zapcore.DebugLevel)
	core, logs := observer.New(level)
	log := zap.New(core)

	v := NewTokenVerifier("secret")
	tok := v.Issue(7, time.Hour)
	// Audit 注册在 Auth 之前（与真实入口顺序一致），即使被 Auth 拒绝也记录访问。
	r := newTestRouter(Audit(log), Auth(v))

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/ok", nil))
	if logs.FilterMessage("access").Len() == 0 {
		t.Error("audit log entry missing")
	}

	// 带 token 的请求应记录 user_id。
	req := httptest.NewRequest("GET", "/ok", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	r.ServeHTTP(httptest.NewRecorder(), req)
	found := false
	for _, e := range logs.All() {
		if e.Message == "access" {
			for _, f := range e.Context {
				if f.Key == "user_id" && f.Integer == 7 {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("audit log should capture user_id from token")
	}
}

func TestAuthWithSkips(t *testing.T) {
	v := NewTokenVerifier("secret")
	tok := v.Issue(1, time.Hour)
	r := newTestRouter(AuthWithSkips(v, "/api/v1/spot/depth", "/metrics"))

	// 豁免路径（行情深度）无 token 应通过。
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/spot/depth", nil))
	if w.Code != 200 {
		t.Errorf("skipped path should pass without token, got %d", w.Code)
	}

	// 豁免路径（metrics）无 token 应通过。
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if w.Code != 200 {
		t.Errorf("metrics should pass without token, got %d", w.Code)
	}

	// 受保护路径（下单）无 token 应 401。
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/v1/spot/order", nil))
	if w.Code != 401 {
		t.Errorf("protected path should 401 without token, got %d", w.Code)
	}

	// 受保护路径带 token 应通过。
	w = httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/spot/order", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Errorf("protected path should pass with token, got %d", w.Code)
	}

	// 前缀扩展的兄弟路径（如 /api/v1/spot/depthXxx）不得被免鉴权（防前缀混淆绕过）。
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/spot/depthXxx", nil))
	if w.Code != 401 {
		t.Errorf("prefix-extension sibling should NOT be skipped (expect 401), got %d", w.Code)
	}
}

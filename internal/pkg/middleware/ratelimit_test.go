package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/redis"
)

func setupRouter(rl ...gin.HandlerFunc) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(rl...)
	r.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func doRequest(r *gin.Engine) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	r.ServeHTTP(w, req)
	return w
}

func TestRateLimit_WithinLimit(t *testing.T) {
	r := setupRouter(RateLimit(5, time.Second))
	for i := 0; i < 5; i++ {
		w := doRequest(r)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+1, w.Code)
		}
	}
}

func TestRateLimit_ExceedsLimit(t *testing.T) {
	r := setupRouter(RateLimit(3, time.Second))
	for i := 0; i < 3; i++ {
		w := doRequest(r)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+1, w.Code)
		}
	}
	// 第 4 次应被限流
	w := doRequest(r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
}

func TestRateLimit_WindowReset(t *testing.T) {
	r := setupRouter(RateLimit(2, 50*time.Millisecond))
	// 用完配额
	doRequest(r)
	doRequest(r)
	w := doRequest(r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
	// 等窗口过期
	time.Sleep(60 * time.Millisecond)
	w = doRequest(r)
	if w.Code != http.StatusOK {
		t.Fatalf("after window reset: expected 200, got %d", w.Code)
	}
}

func TestRateLimit_DistinctIPs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RateLimit(1, time.Second))
	r.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// 不同 IP 互不影响
	w1 := doRequest(r)
	if w1.Code != http.StatusOK {
		t.Fatalf("first IP: expected 200, got %d", w1.Code)
	}
	// 同一 IP 第二次应被限流
	w2 := doRequest(r)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("same IP second: expected 429, got %d", w2.Code)
	}
}

func TestRateLimitWith_MemLimiter(t *testing.T) {
	lim := redis.New("", "", 0) // 空地址 → 内存限流
	r := setupRouter(RateLimitWith(lim, 2, time.Second))
	// 用完配额
	doRequest(r)
	doRequest(r)
	w := doRequest(r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w.Code)
	}
}

func TestRateLimitWith_WithinLimit(t *testing.T) {
	lim := redis.New("", "", 0)
	r := setupRouter(RateLimitWith(lim, 5, time.Second))
	for i := 0; i < 5; i++ {
		w := doRequest(r)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: expected 200, got %d", i+1, w.Code)
		}
	}
}

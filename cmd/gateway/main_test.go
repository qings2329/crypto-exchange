package main

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

// fakeBackend 记录收到的请求路径，便于断言网关把请求转发到了哪个后端。
type fakeBackend struct {
	mu     sync.Mutex
	hits   []string
	server *httptest.Server
}

func newFakeBackend() *fakeBackend {
	b := &fakeBackend{}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.hits = append(b.hits, r.URL.Path)
		b.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok:" + r.URL.Path))
	}))
	return b
}

func (b *fakeBackend) close() { b.server.Close() }

func (b *fakeBackend) reset() {
	b.mu.Lock()
	b.hits = nil
	b.mu.Unlock()
}

func (b *fakeBackend) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.hits)
}

func newGateway(t *testing.T, spotURL, matchingURL string) (*gin.Engine, *middleware.TokenVerifier) {
	t.Helper()
	cfg := &config.Config{}
	cfg.Auth.Secret = "test-secret"
	cfg.Services = map[string]string{
		"spot":     spotURL,
		"matching": matchingURL,
	}
	log := zap.NewNop()
	r := buildRouter(cfg, log)
	return r, middleware.NewTokenVerifier(cfg.Auth.Secret)
}

func doReq(t *testing.T, r *gin.Engine, method, path, token string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

// TestGatewayMatchingWriteEndpointsNotProxied 锁定 §18.1 的资金安全不变量：
// 网关不得把 matching 的写端点（/order、/cancel、/match-now）暴露给外部，否则订单
// 提交会绕过 spot/futures 的资金预冻结与账本结算。
func TestGatewayMatchingWriteEndpointsNotProxied(t *testing.T) {
	spot := newFakeBackend()
	defer spot.close()
	matching := newFakeBackend()
	defer matching.close()

	r, v := newGateway(t, spot.server.URL, matching.server.URL)
	tok := v.Issue(1, 0) // 任意合法 user token

	for _, ep := range []string{
		"/api/v1/matching/order",
		"/api/v1/matching/cancel",
		"/api/v1/matching/match-now",
	} {
		matching.reset()
		code := doReq(t, r, http.MethodPost, ep, tok)
		if code != http.StatusNotFound {
			t.Fatalf("matching write endpoint %s should be 404 at gateway, got %d", ep, code)
		}
		if matching.count() != 0 {
			t.Fatalf("matching write endpoint %s must NOT reach matching backend, got %d hits", ep, matching.count())
		}
	}
}

// TestGatewayMatchingReadEndpointsProxied 验证 matching 的只读/行情端点经网关收敛
// 到 cmd/matching，且业务服务（spot）的通用反代不受影响。
func TestGatewayMatchingReadEndpointsProxied(t *testing.T) {
	spot := newFakeBackend()
	defer spot.close()
	matching := newFakeBackend()
	defer matching.close()

	r, v := newGateway(t, spot.server.URL, matching.server.URL)
	tok := v.Issue(1, 0)

	readEndpoints := []string{
		"/api/v1/matching/depth",
		"/api/v1/matching/orders",
		"/api/v1/matching/orders/123",
		"/api/v1/matching/trades",
		"/api/v1/matching/health",
	}
	for _, ep := range readEndpoints {
		matching.reset()
		code := doReq(t, r, http.MethodGet, ep, tok)
		if code != http.StatusOK {
			t.Fatalf("matching read endpoint %s should reach matching (200), got %d", ep, code)
		}
		if matching.count() == 0 {
			t.Fatalf("matching read endpoint %s did not reach matching backend", ep)
		}
	}

	// spot 通用反代仍生效：请求应落到 spot 后端（路径前缀 /api/v1/spot/*）。
	spot.reset()
	code := doReq(t, r, http.MethodGet, "/api/v1/spot/depth", tok)
	if code != http.StatusOK || spot.count() == 0 {
		t.Fatalf("spot generic proxy broken: code=%d spotHits=%d", code, spot.count())
	}
}

// TestGatewayUnauthenticatedReadRejected 验证非公开端点（含 matching 只读端点）
// 在缺 token 时被边缘鉴权拒绝（默认拒绝，避免行情数据在未授权下外泄）。
func TestGatewayUnauthenticatedReadRejected(t *testing.T) {
	spot := newFakeBackend()
	defer spot.close()
	matching := newFakeBackend()
	defer matching.close()

	r, _ := newGateway(t, spot.server.URL, matching.server.URL)

	code := doReq(t, r, http.MethodGet, "/api/v1/matching/depth", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated matching depth should be 401, got %d", code)
	}
	if matching.count() != 0 {
		t.Fatalf("unauthenticated request must not reach matching backend")
	}
}

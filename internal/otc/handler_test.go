package otc

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/settlement"
	"go.uber.org/zap"
)

// newTestServer 起一个真实 gin 引擎并注册 otc 路由，返回引擎与签发 token 的 verifier。
func newTestServer() (*gin.Engine, *middleware.TokenVerifier, *Service) {
	gin.SetMode(gin.TestMode)
	store := NewMemStore()
	l := ledger.New()
	for _, uid := range []int64{1, 2} {
		_ = l.Deposit(uid, "BTC", settlement.AssetAmountFromFloat(10, settlement.AssetDecimalsByName("BTC")), "seed")
	}
	svc := NewService(store, l, Config{}, zap.NewNop(), func(string) (float64, bool) { return 0, false })
	verifier := middleware.NewTokenVerifier("test-secret")
	r := gin.New()
	svc.RegisterRoutes(r, verifier)
	return r, verifier, svc
}

// TestResolveRequiresAdmin F4：争议裁决端点必须要求管理员角色，否则任意登录用户可移动托管资金。
func TestResolveRequiresAdmin(t *testing.T) {
	r, verifier, svc := newTestServer()
	// 准备一笔已争议订单。
	ad := mustSellAd(svc)
	o, err := svc.TakeOrder(ad.ID, 2, 60000, "bank")
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if err := svc.MarkPaid(o.ID, 2); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	if err := svc.OpenDispute(o.ID, 1); err != nil {
		t.Fatalf("dispute: %v", err)
	}

	adminTok := verifier.IssueRole(99, middleware.RoleAdmin, time.Hour)
	userTok := verifier.IssueRole(7, "user", time.Hour)

	doResolve := func(tok string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/otc/orders/"+itoa(o.ID)+"/resolve", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	// 非管理员：必须 403（关闭盗窃路径）。
	if w := doResolve(userTok); w.Code != http.StatusForbidden {
		t.Fatalf("non-admin resolve should be 403, got %d %s", w.Code, w.Body.String())
	}
	// 管理员：应成功裁决。
	if w := doResolve(adminTok); w.Code != http.StatusOK {
		t.Fatalf("admin resolve should succeed, got %d %s", w.Code, w.Body.String())
	}
}

// TestMessageAndProofEndpoints 验证订单沟通与付款凭证的 HTTP 接口：发送/列出消息、上传/列出/下载凭证，
// 以及非订单参与方必须被拒绝（403）。
func TestMessageAndProofEndpoints(t *testing.T) {
	r, verifier, svc := newTestServer()
	ad := mustSellAd(svc)
	o, err := svc.TakeOrder(ad.ID, 2, 60000, "bank")
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	tok1 := verifier.IssueRole(1, "user", time.Hour)
	tok2 := verifier.IssueRole(2, "user", time.Hour)
	tokOther := verifier.IssueRole(99, "user", time.Hour)

	// 发送消息。
	sendMsg := func(tok, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/otc/orders/"+itoa(o.ID)+"/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	if w := sendMsg(tok1, `{"content":"hi from seller"}`); w.Code != http.StatusOK {
		t.Fatalf("send msg: %d %s", w.Code, w.Body.String())
	}
	if w := sendMsg(tokOther, `{"content":"x"}`); w.Code != http.StatusForbidden {
		t.Fatalf("non-party send msg should 403, got %d", w.Code)
	}

	// 列出消息。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/otc/orders/"+itoa(o.ID)+"/messages", nil)
	req.Header.Set("Authorization", "Bearer "+tok2)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list msg: %d %s", w.Code, w.Body.String())
	}

	// 上传凭证（multipart）。
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "pay.png")
	fw.Write([]byte("fake-bytes"))
	_ = mw.Close()
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/otc/orders/"+itoa(o.ID)+"/proofs", &buf)
	req2.Header.Set("Content-Type", mw.FormDataContentType())
	req2.Header.Set("Authorization", "Bearer "+tok2)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("upload proof: %d %s", w2.Code, w2.Body.String())
	}

	// 非参与方上传凭证必须 403。
	var buf3 bytes.Buffer
	mw3 := multipart.NewWriter(&buf3)
	fw3, _ := mw3.CreateFormFile("file", "x.png")
	fw3.Write([]byte("x"))
	_ = mw3.Close()
	req3 := httptest.NewRequest(http.MethodPost, "/api/v1/otc/orders/"+itoa(o.ID)+"/proofs", &buf3)
	req3.Header.Set("Content-Type", mw3.FormDataContentType())
	req3.Header.Set("Authorization", "Bearer "+tokOther)
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusForbidden {
		t.Fatalf("non-party upload proof should 403, got %d", w3.Code)
	}

	// 列出凭证。
	req4 := httptest.NewRequest(http.MethodGet, "/api/v1/otc/orders/"+itoa(o.ID)+"/proofs", nil)
	req4.Header.Set("Authorization", "Bearer "+tok1)
	w4 := httptest.NewRecorder()
	r.ServeHTTP(w4, req4)
	if w4.Code != http.StatusOK {
		t.Fatalf("list proof: %d %s", w4.Code, w4.Body.String())
	}
}

// TestAdminEndpointsRequireAdmin F4：/admin/orders 与 /admin/reconcile 必须要求管理员角色。
func TestAdminEndpointsRequireAdmin(t *testing.T) {
	r, verifier, _ := newTestServer()
	adminTok := verifier.IssueRole(99, middleware.RoleAdmin, time.Hour)
	userTok := verifier.IssueRole(7, "user", time.Hour)

	for _, path := range []string{"/api/v1/otc/admin/orders", "/api/v1/otc/admin/reconcile"} {
		// 非管理员：403。
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+userTok)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s: non-admin should be 403, got %d", path, w.Code)
		}
		// 管理员：200。
		req2 := httptest.NewRequest(http.MethodGet, path, nil)
		req2.Header.Set("Authorization", "Bearer "+adminTok)
		w2 := httptest.NewRecorder()
		r.ServeHTTP(w2, req2)
		if w2.Code != http.StatusOK {
			t.Fatalf("%s: admin should be 200, got %d %s", path, w2.Code, w2.Body.String())
		}
	}
}

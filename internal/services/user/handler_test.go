package user_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/services/user"
)

var testVerifier = middleware.NewTokenVerifier("test-secret-user")

func newTestHandler(t *testing.T) (*gin.Engine, *user.Service) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	store := user.NewMemStore()
	svc := user.NewService(store, testVerifier, user.NewLogNotifier(), user.Config{})
	h := user.NewHandler(svc, testVerifier)
	r := gin.New()
	h.Register(r)
	return r, svc
}

func userAuth(uid int64) string  { return "Bearer " + testVerifier.Issue(uid, time.Hour) }
func adminAuth(uid int64) string { return "Bearer " + testVerifier.IssueAdmin(uid, "admin", nil, time.Hour) }

// 用 store 直接造一个待审核 KYC 的用户，避免走验证码注册流程。
func seedPendingKYC(t *testing.T, svc *user.Service, uid int64) {
	t.Helper()
	if _, err := svc.AdminCreate("user1@example.com", "password123"); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := svc.SubmitKYC(uid, user.KYCRequest{RealName: "Alice", IDNumber: "X123"}); err != nil {
		t.Fatalf("seed kyc: %v", err)
	}
}

// F4：普通用户调用 KYC 审核接口必须被 AdminGuard 拒绝（403），不得越权审批他人 KYC。
func TestKycReviewForbiddenForUser(t *testing.T) {
	r, svc := newTestHandler(t)
	seedPendingKYC(t, svc, 1)

	body := `{"user_id":1,"approve":true,"reviewer":"attacker"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/user/kyc/review", strings.NewReader(body))
	req.Header.Set("Authorization", userAuth(99)) // 普通用户 token
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expect 403 for normal user, got %d body=%s", w.Code, w.Body.String())
	}
	// 确认未被审核：KYC 仍为待审核。
	_, kyc, err := svc.GetProfile(1)
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if kyc == nil || kyc.Status != user.KYCPending {
		t.Fatalf("kyc must remain pending, got %v", kyc)
	}
}

// F4：管理员可正常审核他人 KYC（200），且状态机推进到 Verified。
func TestKycReviewAllowedForAdmin(t *testing.T) {
	r, svc := newTestHandler(t)
	seedPendingKYC(t, svc, 1)

	body := `{"user_id":1,"approve":true,"reviewer":"admin"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/user/kyc/review", strings.NewReader(body))
	req.Header.Set("Authorization", adminAuth(7)) // 管理员 token
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expect 200 for admin, got %d body=%s", w.Code, w.Body.String())
	}
	_, kyc, err := svc.GetProfile(1)
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if kyc == nil || kyc.Status != user.KYCVerified {
		t.Fatalf("kyc should be verified, got %v", kyc)
	}
}

// F4：普通用户调用管理后台冻结接口必须 403（不得越权冻结他人账户）。
func TestAdminFreezeForbiddenForUser(t *testing.T) {
	r, svc := newTestHandler(t)
	seedPendingKYC(t, svc, 1)

	body := `{}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/user/admin/1/freeze", strings.NewReader(body))
	req.Header.Set("Authorization", userAuth(99))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expect 403 for normal user, got %d body=%s", w.Code, w.Body.String())
	}
	u, _, err := svc.GetProfile(1)
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if u.Status != user.StatusNormal {
		t.Fatalf("user must remain normal, got %v", u.Status)
	}
}

// F4：管理员可冻结他人账户（200），状态推进到 Frozen。
func TestAdminFreezeAllowedForAdmin(t *testing.T) {
	r, svc := newTestHandler(t)
	seedPendingKYC(t, svc, 1)

	body := `{}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/v1/user/admin/1/freeze", strings.NewReader(body))
	req.Header.Set("Authorization", adminAuth(7))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expect 200 for admin, got %d body=%s", w.Code, w.Body.String())
	}
	u, _, err := svc.GetProfile(1)
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if u.Status != user.StatusFrozen {
		t.Fatalf("user should be frozen, got %v", u.Status)
	}
}

// 普通用户自身接口不受 AdminGuard 影响：本人查档案仍为 200（证明用户路由未被误加管理员门槛）。
func TestSelfEndpointsStillOpenToUser(t *testing.T) {
	r, svc := newTestHandler(t)
	id, err := svc.AdminCreate("me@example.com", "password123")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/v1/user/me", nil)
	req.Header.Set("Authorization", userAuth(id))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expect 200 for self /me, got %d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	data, ok := resp["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("no data envelope: %s", w.Body.String())
	}
	if int64(data["user_id"].(float64)) != id {
		t.Fatalf("user_id mismatch: got %v want %d", data["user_id"], id)
	}
}

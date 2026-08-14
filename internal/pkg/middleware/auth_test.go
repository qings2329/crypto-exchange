package middleware

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestTokenVerifyRoundtrip(t *testing.T) {
	v := NewTokenVerifier("secret-key")
	tok := v.Issue(77777, time.Hour)
	uid, ok := v.Verify(tok)
	if !ok || uid != 77777 {
		t.Fatalf("roundtrip failed: uid=%d ok=%v", uid, ok)
	}
}

func TestTokenVerifyRejectsTamper(t *testing.T) {
	v := NewTokenVerifier("secret-key")
	tok := v.Issue(1, time.Hour)
	// 篡改载荷
	tampered := "eyJ1aWQiOjk5OTk5fQ" + "." + tok[strings.Index(tok, ".")+1:]
	if _, ok := v.Verify(tampered); ok {
		t.Fatal("tampered token should be rejected")
	}
}

func TestTokenVerifyExpired(t *testing.T) {
	v := NewTokenVerifier("secret-key")
	tok := v.Issue(1, -time.Second) // 已过期
	if _, ok := v.Verify(tok); ok {
		t.Fatal("expired token should be rejected")
	}
}

func TestTokenVerifyEmptySecretFailClosed(t *testing.T) {
	v := NewTokenVerifier("")
	if _, ok := v.Verify("anything"); ok {
		t.Fatal("empty secret must reject all tokens")
	}
}

func TestAuthMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	v := NewTokenVerifier("secret-key")

	r := gin.New()
	r.Use(Auth(v))
	r.GET("/ping", func(c *gin.Context) {
		uid, _ := UserID(c)
		c.JSON(http.StatusOK, gin.H{"uid": uid})
	})

	// 无 Authorization → 401
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without auth, got %d", w.Code)
	}

	// 合法 token → 200 且上下文带回 user_id
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req2.Header.Set("Authorization", "Bearer "+v.Issue(42, time.Hour))
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid token, got %d", w2.Code)
	}
	if w2.Body.String() == "" {
		t.Fatal("empty body")
	}

	// 伪造 token → 401
	w3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req3.Header.Set("Authorization", "Bearer "+base64.RawURLEncoding.EncodeToString([]byte(`{"uid":1,"exp":0}`))+".bad")
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with forged token, got %d", w3.Code)
	}
}

func TestIssueRoleAndVerifyFull(t *testing.T) {
	v := NewTokenVerifier("secret-key")
	// 默认 Issue 应为 user 角色。
	uTok := v.Issue(10, time.Hour)
	if uid, role, ok := v.VerifyFull(uTok); !ok || uid != 10 || role != RoleUser {
		t.Fatalf("user token mismatch: uid=%d role=%q ok=%v", uid, role, ok)
	}
	// 显式 admin 角色。
	aTok := v.IssueRole(99, RoleAdmin, time.Hour)
	if uid, role, ok := v.VerifyFull(aTok); !ok || uid != 99 || role != RoleAdmin {
		t.Fatalf("admin token mismatch: uid=%d role=%q ok=%v", uid, role, ok)
	}
	// Issue 与 IssueRole(user) 互为等价。
	if uid, role, ok := v.VerifyFull(v.IssueRole(5, RoleUser, time.Hour)); !ok || uid != 5 || role != RoleUser {
		t.Fatalf("explicit user role mismatch")
	}
}

func TestRoleGuardMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	v := NewTokenVerifier("secret-key")

	r := gin.New()
	r.Use(Auth(v), AdminGuard())
	r.GET("/admin", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// 普通用户 token → 403（角色不足）。
	w1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req1.Header.Set("Authorization", "Bearer "+v.Issue(1, time.Hour))
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for user role, got %d", w1.Code)
	}

	// 管理员 token → 200。
	w2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/admin", nil)
	req2.Header.Set("Authorization", "Bearer "+v.IssueRole(1, RoleAdmin, time.Hour))
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("expected 200 for admin role, got %d", w2.Code)
	}
}

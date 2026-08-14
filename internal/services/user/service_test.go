package user

import (
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

func newTestService() *Service {
	store := NewMemStore()
	verifier := middleware.NewTokenVerifier("test-secret")
	svc := NewService(store, verifier, NewLogNotifier(), Config{
		AccessTTL:  15 * time.Minute,
		RefreshTTL: time.Hour,
		CodeTTL:    time.Minute,
		CodeLen:    6,
	})
	return svc
}

func TestRegisterLoginEmail(t *testing.T) {
	svc := newTestService()
	email := "alice@example.com"

	// 未发码直接注册应失败
	if _, err := svc.Register(email, "secret123", "000000"); err == nil {
		t.Fatal("register without code should fail")
	}

	// 发码并注册
	if err := svc.SendCode(email, PurposeRegister); err != nil {
		t.Fatalf("send code: %v", err)
	}
	code, err := latestCode(svc, email, PurposeRegister)
	if err != nil {
		t.Fatalf("get code: %v", err)
	}
	id, err := svc.Register(email, "secret123", code)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if id == 0 {
		t.Fatal("expected user id")
	}

	// 重复注册冲突
	if _, err := svc.Register(email, "secret123", code); err != ErrUserExists {
		t.Fatalf("expected ErrUserExists, got %v", err)
	}

	// 登录成功并拿到真正可用的 token
	res, err := svc.Login(email, "secret123", "")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.AccessToken == "" || res.RefreshToken == "" {
		t.Fatal("expected tokens")
	}
	if _, ok := verifierVerify(svc, res.AccessToken); !ok {
		t.Fatal("issued access token should verify")
	}

	// refresh 换 token
	newAccess, err := svc.Refresh(res.RefreshToken)
	if err != nil || newAccess == "" {
		t.Fatalf("refresh failed: %v", err)
	}

	// 密码错误
	if _, err := svc.Login(email, "wrong", ""); err != ErrWrongPassword {
		t.Fatalf("expected ErrWrongPassword, got %v", err)
	}

	// logout 后 refresh 失效
	if err := svc.Logout(res.RefreshToken); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := svc.Refresh(res.RefreshToken); err != ErrRefreshInvalid {
		t.Fatalf("expected ErrRefreshInvalid after logout, got %v", err)
	}
}

func TestRegisterPhone(t *testing.T) {
	svc := newTestService()
	phone := "13800138000"
	if err := svc.SendCode(phone, PurposeRegister); err != nil {
		t.Fatalf("send code: %v", err)
	}
	code, _ := latestCode(svc, phone, PurposeRegister)
	if _, err := svc.Register(phone, "secret123", code); err != nil {
		t.Fatalf("register phone: %v", err)
	}
	if _, err := svc.Login(phone, "secret123", ""); err != nil {
		t.Fatalf("login phone: %v", err)
	}
}

func TestResetPassword(t *testing.T) {
	svc := newTestService()
	email := "bob@example.com"
	_ = svc.SendCode(email, PurposeRegister)
	code, _ := latestCode(svc, email, PurposeRegister)
	if _, err := svc.Register(email, "oldpass1", code); err != nil {
		t.Fatalf("register: %v", err)
	}
	// 找回：发重置码
	if err := svc.ForgotPassword(email); err != nil {
		t.Fatalf("forgot: %v", err)
	}
	rCode, _ := latestCode(svc, email, PurposeReset)
	if err := svc.ResetPassword(email, rCode, "newpass1"); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := svc.Login(email, "newpass1", ""); err != nil {
		t.Fatalf("login with new password: %v", err)
	}
	if _, err := svc.Login(email, "oldpass1", ""); err != ErrWrongPassword {
		t.Fatalf("old password should fail, got %v", err)
	}
}

func TestTFAFlow(t *testing.T) {
	svc := newTestService()
	email := "carol@example.com"
	_ = svc.SendCode(email, PurposeRegister)
	code, _ := latestCode(svc, email, PurposeRegister)
	id, err := svc.Register(email, "secret123", code)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// setup
	secret, uri, err := svc.SetupTFA(id)
	if err != nil || secret == "" || uri == "" {
		t.Fatalf("tfa setup: %v secret=%q", err, secret)
	}
	// 登录尚未启用 2FA，不应要求 code
	if _, err := svc.Login(email, "secret123", ""); err != nil {
		t.Fatalf("login before enable: %v", err)
	}
	// 用当前 TOTP 启用
	totp, err := tfaNow(secret)
	if err != nil {
		t.Fatalf("tfa now: %v", err)
	}
	if err := svc.EnableTFA(id, totp); err != nil {
		t.Fatalf("enable tfa: %v", err)
	}
	// 启用后登录必须带正确 code
	if _, err := svc.Login(email, "secret123", ""); err != ErrTFARequired {
		t.Fatalf("expected ErrTFARequired, got %v", err)
	}
	totp2, _ := tfaNow(secret)
	res, err := svc.Login(email, "secret123", totp2)
	if err != nil {
		t.Fatalf("login with tfa: %v", err)
	}
	_ = res
	// 关闭
	totp3, _ := tfaNow(secret)
	if err := svc.DisableTFA(id, totp3); err != nil {
		t.Fatalf("disable tfa: %v", err)
	}
	if _, err := svc.Login(email, "secret123", ""); err != nil {
		t.Fatalf("login after disable: %v", err)
	}
}

func TestKYCFlow(t *testing.T) {
	svc := newTestService()
	email := "dave@example.com"
	_ = svc.SendCode(email, PurposeRegister)
	code, _ := latestCode(svc, email, PurposeRegister)
	id, err := svc.Register(email, "secret123", code)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := svc.SubmitKYC(id, KYCRequest{RealName: "Dave", IDType: "id_card", IDNumber: "12345"}); err != nil {
		t.Fatalf("submit kyc: %v", err)
	}
	// 重复提交应驳回
	if err := svc.SubmitKYC(id, KYCRequest{RealName: "Dave", IDType: "id_card", IDNumber: "12345"}); err != ErrKYCPending {
		t.Fatalf("expected ErrKYCPending, got %v", err)
	}
	// 审核通过
	if err := svc.ReviewKYC(id, true, "", "admin"); err != nil {
		t.Fatalf("review: %v", err)
	}
	u, _, _ := svc.GetProfile(id)
	if u.KYCLevel != KYCVerified {
		t.Fatalf("expected KYCVerified, got %v", u.KYCLevel)
	}
}

// ---- 测试辅助 ----

func latestCode(svc *Service, target, purpose string) (string, error) {
	c, err := svc.store.GetLatestCode(target, purpose)
	if err != nil {
		return "", err
	}
	return c.Code, nil
}

func verifierVerify(svc *Service, token string) (int64, bool) {
	return svc.verifier.Verify(token)
}

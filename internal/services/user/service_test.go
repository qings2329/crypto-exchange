package user

import (
	"errors"
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/notification"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

func newTestService() *Service {
	store := NewMemStore()
	verifier := middleware.NewTokenVerifier("test-secret")
	svc := NewService(store, verifier, NewLogNotifier(), nil, Config{
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
	if _, err := svc.Register(email, "secret123", "000000", ""); err == nil {
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
	id, err := svc.Register(email, "secret123", code, "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if id == 0 {
		t.Fatal("expected user id")
	}

	// 重复注册冲突
	if _, err := svc.Register(email, "secret123", code, ""); err != ErrUserExists {
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
	if _, err := svc.Register(phone, "secret123", code, ""); err != nil {
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
	if _, err := svc.Register(email, "oldpass1", code, ""); err != nil {
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
	id, err := svc.Register(email, "secret123", code, "")
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
	id, err := svc.Register(email, "secret123", code, "")
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

// registerTestUser 注册并返回用户 ID（用于设置类测试前置）。
func registerTestUser(t *testing.T, svc *Service, email, password string) int64 {
	t.Helper()
	if err := svc.SendCode(email, PurposeRegister); err != nil {
		t.Fatalf("send code: %v", err)
	}
	code, err := latestCode(svc, email, PurposeRegister)
	if err != nil {
		t.Fatalf("get code: %v", err)
	}
	if _, err := svc.Register(email, password, code, ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	u, err := svc.store.GetByEmail(email)
	if err != nil {
		t.Fatalf("get by email: %v", err)
	}
	return u.ID
}

func TestUpdateProfile(t *testing.T) {
	svc := newTestService()
	id := registerTestUser(t, svc, "bob@example.com", "secret123")
	str := func(s string) *string { return &s }

	if err := svc.UpdateProfile(id, str("Bobby"), str("https://x/y.png")); err != nil {
		t.Fatalf("update profile: %v", err)
	}
	u, _, _ := svc.GetProfile(id)
	if u.Nickname != "Bobby" || u.Avatar != "https://x/y.png" {
		t.Fatalf("profile not persisted: %+v", u)
	}

	// 仅清空昵称（头像不变）
	if err := svc.UpdateProfile(id, str(""), nil); err != nil {
		t.Fatalf("clear nickname: %v", err)
	}
	u, _, _ = svc.GetProfile(id)
	if u.Nickname != "" || u.Avatar != "https://x/y.png" {
		t.Fatalf("nickname not cleared / avatar changed: %+v", u)
	}

	// 昵称超长应失败
	long := string(make([]rune, 33))
	if err := svc.UpdateProfile(id, str(long), nil); err == nil {
		t.Fatal("expected nickname-too-long error")
	}
}

func TestChangePassword(t *testing.T) {
	svc := newTestService()
	id := registerTestUser(t, svc, "carol@example.com", "secret123")

	// 旧密码错误
	if err := svc.ChangePassword(id, "wrong", "newpass1"); err != ErrWrongPassword {
		t.Fatalf("expected ErrWrongPassword, got %v", err)
	}

	// 与旧密码相同应被拒
	if err := svc.ChangePassword(id, "secret123", "secret123"); err != ErrSamePassword {
		t.Fatalf("expected ErrSamePassword, got %v", err)
	}

	// 成功改密，且旧密码不可用、新密码可用
	if err := svc.ChangePassword(id, "secret123", "newpass1"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if _, err := svc.Login("carol@example.com", "secret123", ""); err != ErrWrongPassword {
		t.Fatalf("old password should fail, got %v", err)
	}
	if _, err := svc.Login("carol@example.com", "newpass1", ""); err != nil {
		t.Fatalf("new password should work: %v", err)
	}
}

func TestPreferences(t *testing.T) {
	svc := newTestService()
	id := registerTestUser(t, svc, "dave@example.com", "secret123")

	// 未设置时返回默认偏好
	def, err := svc.GetPreferences(id)
	if err != nil {
		t.Fatalf("get preferences: %v", err)
	}
	if def.Language != "zh-CN" || !def.NotifyOrder || def.NotifyMarketing {
		t.Fatalf("unexpected defaults: %+v", def)
	}

	// 更新偏好
	in := &UserPreferences{
		Language:       "en",
		Theme:          "dark",
		NotifyOrder:    false,
		NotifySecurity: true,
		NotifyMarketing: true,
	}
	if err := svc.UpdatePreferences(id, in); err != nil {
		t.Fatalf("update preferences: %v", err)
	}
	got, err := svc.GetPreferences(id)
	if err != nil {
		t.Fatalf("get preferences after update: %v", err)
	}
	if got.Language != "en" || got.Theme != "dark" || got.NotifyOrder || !got.NotifyMarketing {
		t.Fatalf("preferences not persisted: %+v", got)
	}
}

// TestPreferencesTimezone 验证偏好中的 timezone 字段可正确写入并读回，
// 包括空串（跟随系统）语义，避免字段被静默丢弃。
func TestPreferencesTimezone(t *testing.T) {
	svc := newTestService()
	id := registerTestUser(t, svc, "tz@example.com", "secret123")

	// 未设置时 timezone 默认为空串（跟随系统）
	def, err := svc.GetPreferences(id)
	if err != nil {
		t.Fatalf("get preferences: %v", err)
	}
	if def.Timezone != "" {
		t.Fatalf("expected default timezone \"\", got %q", def.Timezone)
	}

	// 写入具体时区
	in := &UserPreferences{
		Language: "ja",
		Theme:    "midnight",
		Timezone: "Asia/Tokyo",
	}
	if err := svc.UpdatePreferences(id, in); err != nil {
		t.Fatalf("update preferences: %v", err)
	}
	got, err := svc.GetPreferences(id)
	if err != nil {
		t.Fatalf("get preferences after update: %v", err)
	}
	if got.Timezone != "Asia/Tokyo" {
		t.Fatalf("timezone not persisted: got %q", got.Timezone)
	}
	// 同一次写入的语言/主题也应保持
	if got.Language != "ja" || got.Theme != "midnight" {
		t.Fatalf("language/theme drift: %+v", got)
	}

	// 改回空串（跟随系统）应被保留，而非回退成默认
	in2 := &UserPreferences{Timezone: ""}
	if err := svc.UpdatePreferences(id, in2); err != nil {
		t.Fatalf("update timezone to empty: %v", err)
	}
	got2, err := svc.GetPreferences(id)
	if err != nil {
		t.Fatalf("get preferences after clear: %v", err)
	}
	if got2.Timezone != "" {
		t.Fatalf("expected empty timezone after clear, got %q", got2.Timezone)
	}
}

// TestReviewKYCPublishesNotification 验证 ReviewKYC 通过/驳回均写入通知中心。
func TestReviewKYCPublishesNotification(t *testing.T) {
	notifSvc := notification.New(notification.NewMemStore())
	store := NewMemStore()
	verifier := middleware.NewTokenVerifier("test-secret")
	svc := NewService(store, verifier, NewLogNotifier(), notifSvc, Config{})

	uid := registerTestUser(t, svc, "kyc@example.com", "secret123")
	if err := svc.SubmitKYC(uid, KYCRequest{RealName: "张三", IDNumber: "110"}); err != nil {
		t.Fatalf("submit kyc: %v", err)
	}

	// 通过
	if err := svc.ReviewKYC(uid, true, "", "reviewer-a"); err != nil {
		t.Fatalf("review kyc approve: %v", err)
	}
	list, err := notifSvc.List(uid, false, 10)
	if err != nil {
		t.Fatalf("list notifications: %v", err)
	}
	if len(list) != 1 || list[0].Type != notification.TypeKYCAproved {
		t.Fatalf("expected 1 approved notification, got %+v", list)
	}

	// 再次提交并驳回
	if err := svc.SubmitKYC(uid, KYCRequest{RealName: "张三", IDNumber: "110"}); err != nil {
		t.Fatalf("submit kyc again: %v", err)
	}
	if err := svc.ReviewKYC(uid, false, "材料不清晰", "reviewer-b"); err != nil {
		t.Fatalf("review kyc reject: %v", err)
	}
	list, err = notifSvc.List(uid, false, 10)
	if err != nil {
		t.Fatalf("list notifications again: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 notifications, got %d", len(list))
	}
	var rejected *notification.Notification
	for _, n := range list {
		if n.Type == notification.TypeKYCRejected {
			rejected = n
		}
	}
	if rejected == nil {
		t.Fatalf("rejected notification missing: %+v", list)
	}
	if rejected.Body != "材料不清晰" {
		t.Fatalf("expected reject reason in body, got %q", rejected.Body)
	}
}

// TestRiskAlertOnFailedLogin 验证既有账号登录失败时推送风险预警通知。
func TestRiskAlertOnFailedLogin(t *testing.T) {
	notifSvc := notification.New(notification.NewMemStore())
	svc := NewService(NewMemStore(), middleware.NewTokenVerifier("test-secret"), NewLogNotifier(), notifSvc, Config{})
	uid := registerTestUser(t, svc, "risk@example.com", "secret123")

	// 对既有账号发起一次失败登录（authErr != nil，userID 已知）。
	svc.RecordLoginWithMeta("risk@example.com", errors.New("bad password"), uid, "203.0.113.9", "curl/8.0")
	list, err := notifSvc.List(uid, false, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Type != notification.TypeRiskAlert {
		t.Fatalf("expected 1 risk_alert notification, got %+v", list)
	}
	if list[0].Title != "账号登录异常提醒" {
		t.Fatalf("unexpected title: %s", list[0].Title)
	}

	// 成功登录不应再产生风险预警（仅历史/会话）。
	svc.RecordLoginWithMeta("risk@example.com", nil, uid, "203.0.113.9", "curl/8.0")
	list2, _ := notifSvc.List(uid, false, 10)
	if len(list2) != 1 {
		t.Fatalf("successful login must not add risk_alert, got %d", len(list2))
	}
}

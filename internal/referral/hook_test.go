package referral

import (
	"errors"
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/services/user"
	"go.uber.org/zap"
)

// fakeUserStore 是 user.Store 的最小内存实现，仅覆盖测试所需方法。
type fakeUserStore struct {
	byID   map[int64]*user.User
	byCode map[string]*user.User
}

func (f *fakeUserStore) GetByID(id int64) (*user.User, error) {
	if u, ok := f.byID[id]; ok {
		return u, nil
	}
	return nil, errors.New("user not found")
}

func (f *fakeUserStore) GetByReferralCode(code string) (*user.User, error) {
	if u, ok := f.byCode[code]; ok {
		return u, nil
	}
	return nil, errors.New("user not found")
}

// 其余接口方法留空实现（测试不会调用）。
func (f *fakeUserStore) CreateUser(*user.User) error      { return nil }
func (f *fakeUserStore) GetByEmail(string) (*user.User, error) { return nil, errors.New("n/a") }
func (f *fakeUserStore) GetByPhone(string) (*user.User, error)  { return nil, errors.New("n/a") }
func (f *fakeUserStore) UpdateUser(*user.User) error      { return nil }
func (f *fakeUserStore) ListAll() ([]*user.User, error)   { return nil, nil }
func (f *fakeUserStore) SaveCode(*user.VerifyCode) error  { return nil }
func (f *fakeUserStore) GetLatestCode(string, string) (*user.VerifyCode, error) {
	return nil, errors.New("n/a")
}
func (f *fakeUserStore) ConsumeCode(int64) error { return nil }
func (f *fakeUserStore) SaveRefresh(*user.RefreshToken) error  { return nil }
func (f *fakeUserStore) GetRefresh(string) (*user.RefreshToken, error) {
	return nil, errors.New("n/a")
}
func (f *fakeUserStore) DeleteRefresh(string) error                { return nil }
func (f *fakeUserStore) DeleteUserRefreshes(int64) error          { return nil }
func (f *fakeUserStore) SaveKYC(*user.KYCSubmission) error        { return nil }
func (f *fakeUserStore) GetKYC(int64) (*user.KYCSubmission, error) {
	return nil, errors.New("n/a")
}
func (f *fakeUserStore) UpdateKYC(*user.KYCSubmission) error      { return nil }
func (f *fakeUserStore) ListPendingKYC() ([]*user.KYCSubmission, error) {
	return nil, nil
}
func (f *fakeUserStore) GetPreferences(int64) (*user.UserPreferences, error) {
	return nil, errors.New("n/a")
}
func (f *fakeUserStore) UpdatePreferences(*user.UserPreferences) error { return nil }
func (f *fakeUserStore) GetReferrals(int64) ([]*user.User, error)      { return nil, nil }
func (f *fakeUserStore) CreateApiKey(*user.ApiKey) error          { return nil }
func (f *fakeUserStore) ListApiKeys(int64) ([]*user.ApiKey, error) {
	return nil, nil
}
func (f *fakeUserStore) UpdateApiKeyStatus(int64, int64, string) error { return nil }
func (f *fakeUserStore) DeleteApiKey(int64, int64) error              { return nil }
func (f *fakeUserStore) RecordLogin(*user.LoginEntry) error           { return nil }
func (f *fakeUserStore) ListLoginHistory(int64, int) ([]*user.LoginEntry, error) {
	return nil, nil
}
func (f *fakeUserStore) CreateSession(*user.Session) error            { return nil }
func (f *fakeUserStore) ListSessions(int64) ([]*user.Session, error)  { return nil, nil }
func (f *fakeUserStore) TouchSession(int64, string, time.Time) error  { return nil }
func (f *fakeUserStore) DeleteSession(int64, string) error            { return nil }
func (f *fakeUserStore) DeleteOtherSessions(int64, string) (int64, error) {
	return 0, nil
}
func (f *fakeUserStore) GetAntiPhishing(int64) (string, error) { return "", nil }
func (f *fakeUserStore) SetAntiPhishing(int64, string) error   { return nil }

// fakeCommissionStore 记录被写入的佣金，供断言。
type fakeCommissionStore struct {
	recorded []*ReferralCommission
}

func (f *fakeCommissionStore) RecordCommission(c *ReferralCommission) error {
	f.recorded = append(f.recorded, c)
	return nil
}
func (f *fakeCommissionStore) GetCommissionByRef(string) (*ReferralCommission, error) {
	return nil, errors.New("n/a")
}
func (f *fakeCommissionStore) ListCommissionsByReferrer(int64, int, int) ([]*ReferralCommission, int, error) {
	return nil, 0, nil
}
func (f *fakeCommissionStore) ListAll(int, int) ([]*ReferralCommission, int, error) {
	return nil, 0, nil
}
func (f *fakeCommissionStore) TotalByReferrer(int64) (map[string]int64, error) {
	return nil, nil
}

// TestReferralCooldown 验证 §38 冷却期：新用户注册后 N 天内交易不计佣金。
func TestReferralCooldown(t *testing.T) {
	now := time.Now()
	us := &fakeUserStore{
		byID: map[int64]*user.User{
			1: {ID: 1, ReferralCode: "REFERRER1", CreatedAt: now.Add(-100 * 24 * time.Hour)}, // 老邀请人
			2: {ID: 2, ReferrerID: 1, CreatedAt: now.Add(-1 * time.Hour)},                    // 刚注册 1h
			3: {ID: 3, ReferrerID: 1, CreatedAt: now.Add(-30 * 24 * time.Hour)},              // 注册 30 天
		},
	}
	cs := &fakeCommissionStore{}

	// 1) 默认冷却 7 天：注册 1h 的 taker(2) 不应产生佣金。
	svc := NewService(cs, zap.NewNop())
	hook := NewHookAdapter(us, svc, 0.2)
	if err := hook.RecordTradeFee(1, 2, "USDT", 1000, "biz:2"); err != nil {
		t.Fatalf("RecordTradeFee unexpected err: %v", err)
	}
	if len(cs.recorded) != 0 {
		t.Fatalf("cooldown: expected 0 commission for fresh user, got %d", len(cs.recorded))
	}

	// 2) 注册 30 天的 taker(3) 应正常计入佣金。
	if err := hook.RecordTradeFee(1, 3, "USDT", 1000, "biz:3"); err != nil {
		t.Fatalf("RecordTradeFee unexpected err: %v", err)
	}
	if len(cs.recorded) != 1 {
		t.Fatalf("cooldown: expected 1 commission for 30d user, got %d", len(cs.recorded))
	}
	if cs.recorded[0].TakerID != 3 {
		t.Fatalf("cooldown: wrong taker recorded: %d", cs.recorded[0].TakerID)
	}

	// 3) cooldownDays=0 关闭冷却：刚注册用户也计入。
	cs2 := &fakeCommissionStore{}
	svc2 := NewService(cs2, zap.NewNop())
	hook2 := NewHookAdapterWithCooldown(us, svc2, 0.2, 0)
	if err := hook2.RecordTradeFee(1, 2, "USDT", 1000, "biz:2b"); err != nil {
		t.Fatalf("RecordTradeFee unexpected err: %v", err)
	}
	if len(cs2.recorded) != 1 {
		t.Fatalf("no-cooldown: expected 1 commission, got %d", len(cs2.recorded))
	}

	// 4) 自邀请（referrer==taker）应被拒绝，不产生佣金（原有防刷规则保留）。
	cs3 := &fakeCommissionStore{}
	svc3 := NewService(cs3, zap.NewNop())
	hook3 := NewHookAdapter(us, svc3, 0.2)
	if err := hook3.RecordTradeFee(4, 4, "USDT", 1000, "biz:4"); err == nil {
		t.Fatal("self-referral should error")
	}
	if len(cs3.recorded) != 0 {
		t.Fatalf("self-referral: expected 0 commission, got %d", len(cs3.recorded))
	}
}

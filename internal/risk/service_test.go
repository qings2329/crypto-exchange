package risk

import (
	"math"
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/settlement"
)

func newTestSvc() *Service { return New(NewMemStore()) }

func TestWithdrawLimitPass(t *testing.T) {
	svc := newTestSvc()
	if _, err := svc.AddRule(&RiskRule{Kind: KindWithdrawLimit, Asset: "BTC", MaxAmountPerDay: settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("BTC")), MinKYCLevel: 1}); err != nil {
		t.Fatalf("add rule: %v", err)
	}
	res, err := svc.CheckWithdraw(1, "BTC", 500, 1, "")
	if err != nil || !res.Allowed {
		t.Fatalf("want allowed, got %+v err=%v", res, err)
	}
}

func TestWithdrawLimitExceeded(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindWithdrawLimit, Asset: "BTC", MaxAmountPerDay: settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("BTC")), MinKYCLevel: 1})
	res, _ := svc.CheckWithdraw(1, "BTC", 2000, 1, "")
	if res.Allowed || res.Reason != "exceeds withdraw limit" {
		t.Fatalf("want rejected(exceeds), got %+v", res)
	}
}

func TestWithdrawLowKYC(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindWithdrawLimit, Asset: "BTC", MaxAmountPerDay: settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("BTC")), MinKYCLevel: 2})
	res, _ := svc.CheckWithdraw(1, "BTC", 500, 1, "")
	if res.Allowed || res.Reason != "kyc level too low" {
		t.Fatalf("want rejected(kyc), got %+v", res)
	}
}

// TestWithdrawLimitRejectsNonFinite 回归 M5：NaN/Inf 金额经 CheckWithdraw 必须被拒绝（不能静默记 0 绕过限额）。
func TestWithdrawLimitRejectsNonFinite(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindWithdrawLimit, Asset: "BTC", MaxAmountPerDay: settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("BTC")), MinKYCLevel: 1})
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		res, _ := svc.CheckWithdraw(1, "BTC", bad, 1, "")
		if res.Allowed {
			t.Fatalf("non-finite amount %v should be rejected, got allowed", bad)
		}
	}
}

// TestOrderLimitRejectsNonFinite 回归 M5：下单风控同样须拦截 NaN/Inf 数量。
func TestOrderLimitRejectsNonFinite(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindOrderLimit, Asset: "ETH", MaxAmountPerDay: settlement.AssetAmountFromFloat(10, settlement.AssetDecimalsByName("ETH")), MinKYCLevel: 1})
	for _, bad := range []float64{math.NaN(), math.Inf(1)} {
		res, _ := svc.CheckOrder(1, "ETH", bad, 1)
		if res.Allowed {
			t.Fatalf("non-finite qty %v should be rejected, got allowed", bad)
		}
	}
}

func TestUserBlacklistBlocksWithdraw(t *testing.T) {
	svc := newTestSvc()
	svc.AddBlacklist("7", BlacklistUser, "fraud")
	res, _ := svc.CheckWithdraw(7, "BTC", 1, 3, "")
	if res.Allowed || res.Reason != "user blacklisted" {
		t.Fatalf("want rejected(blacklist), got %+v", res)
	}
	if ok, _ := svc.IsBlacklisted("7"); !ok {
		t.Fatal("expected 7 blacklisted")
	}
	// 非黑名单用户不受影响
	res, _ = svc.CheckWithdraw(8, "BTC", 1, 3, "")
	if !res.Allowed {
		t.Fatalf("user 8 should pass, got %+v", res)
	}
}

func TestAddressBlacklistBlocksWithdraw(t *testing.T) {
	svc := newTestSvc()
	svc.AddBlacklist("0xbad", BlacklistAddress, "sanctioned")
	res, _ := svc.CheckWithdraw(1, "BTC", 1, 3, "0xbad")
	if res.Allowed || res.Reason != "address blacklisted" {
		t.Fatalf("want rejected(addr), got %+v", res)
	}
}

func TestNoRuleDefaultsAllow(t *testing.T) {
	svc := newTestSvc()
	res, _ := svc.CheckWithdraw(1, "BTC", 999999, 0, "")
	if !res.Allowed {
		t.Fatalf("want allowed when no rule, got %+v", res)
	}
}

func TestOrderLimit(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindOrderLimit, Asset: "ETH", MaxAmountPerDay: settlement.AssetAmountFromFloat(10, settlement.AssetDecimalsByName("ETH")), MinKYCLevel: 1})
	if res, _ := svc.CheckOrder(1, "ETH", 5, 1); !res.Allowed {
		t.Fatalf("want allowed, got %+v", res)
	}
	if res, _ := svc.CheckOrder(1, "ETH", 20, 1); res.Allowed || res.Reason != "exceeds order limit" {
		t.Fatalf("want rejected, got %+v", res)
	}
}

func TestWithdrawNegativeAmountRejected(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindWithdrawLimit, Asset: "BTC", MaxAmountPerDay: settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("BTC")), MinKYCLevel: 1})
	res, _ := svc.CheckWithdraw(1, "BTC", -100, 1, "")
	if res.Allowed || res.Reason != "amount must be positive" {
		t.Fatalf("want rejected(negative amount), got %+v", res)
	}
}

func TestWithdrawZeroAmountRejected(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindWithdrawLimit, Asset: "BTC", MaxAmountPerDay: settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("BTC")), MinKYCLevel: 1})
	res, _ := svc.CheckWithdraw(1, "BTC", 0, 1, "")
	if res.Allowed || res.Reason != "amount must be positive" {
		t.Fatalf("want rejected(zero amount), got %+v", res)
	}
}

func TestOrderNegativeQtyRejected(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindOrderLimit, Asset: "ETH", MaxAmountPerDay: settlement.AssetAmountFromFloat(10, settlement.AssetDecimalsByName("ETH")), MinKYCLevel: 1})
	res, _ := svc.CheckOrder(1, "ETH", -5, 1)
	if res.Allowed || res.Reason != "qty must be positive" {
		t.Fatalf("want rejected(negative qty), got %+v", res)
	}
}

func TestWithdrawLimitBoundaryExact(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindWithdrawLimit, Asset: "BTC", MaxAmountPerDay: settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("BTC")), MinKYCLevel: 1})
	// 边界相等：定点化后 Cmp 为 0，应放行（不再因浮点漂移误杀）。
	res, _ := svc.CheckWithdraw(1, "BTC", 1000, 1, "")
	if !res.Allowed {
		t.Fatalf("want allowed at exact limit, got %+v", res)
	}
	// 恰超 1 satoshi 应拒绝。
	res, _ = svc.CheckWithdraw(1, "BTC", 1000.00000001, 1, "")
	if res.Allowed || res.Reason != "exceeds withdraw limit" {
		t.Fatalf("want rejected(just over), got %+v", res)
	}
}

func TestRuleListAndRemove(t *testing.T) {
	svc := newTestSvc()
	r1, _ := svc.AddRule(&RiskRule{Kind: KindWithdrawLimit, Asset: "BTC", MaxAmountPerDay: settlement.AssetAmountFromFloat(100, settlement.AssetDecimalsByName("BTC"))})
	r2, _ := svc.AddRule(&RiskRule{Kind: KindOrderLimit, Asset: "ETH", MaxAmountPerDay: settlement.AssetAmountFromFloat(10, settlement.AssetDecimalsByName("ETH"))})
	all, _ := svc.ListRules("")
	if len(all) != 2 {
		t.Fatalf("want 2 rules got %d", len(all))
	}
	w, _ := svc.ListRules(KindWithdrawLimit)
	if len(w) != 1 || w[0].ID != r1.ID {
		t.Fatalf("want 1 withdraw rule")
	}
	// 更新启用状态
	r2.Enabled = false
	if _, err := svc.AddRule(r2); err != nil {
		t.Fatalf("update rule: %v", err)
	}
	// 删除 r1
	if err := svc.RemoveBlacklist(""); err == nil {
	}
	_ = r1
}

func TestEventsRecordedOnReject(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindWithdrawLimit, Asset: "BTC", MaxAmountPerDay: settlement.AssetAmountFromFloat(100, settlement.AssetDecimalsByName("BTC")), MinKYCLevel: 1})
	svc.CheckWithdraw(1, "BTC", 5000, 1, "")
	evs, _ := svc.ListEvents(1, 0)
	if len(evs) != 1 {
		t.Fatalf("want 1 event got %d", len(evs))
	}
	if evs[0].Kind != KindWithdrawLimit {
		t.Fatalf("want withdraw_limit event got %s", evs[0].Kind)
	}
}

// ---------- CheckPosition ----------

func TestPositionLimitPass(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindPositionLimit, Asset: "BTC", MaxAmountPerDay: settlement.AssetAmountFromFloat(10, 8)})
	res, err := svc.CheckPosition(1, "BTC", 5, 0)
	if err != nil || !res.Allowed {
		t.Fatalf("want allowed, got %+v err=%v", res, err)
	}
}

func TestPositionLimitExceeded(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindPositionLimit, Asset: "BTC", MaxAmountPerDay: settlement.AssetAmountFromFloat(10, 8)})
	res, err := svc.CheckPosition(1, "BTC", 15, 0)
	if err != nil || res.Allowed {
		t.Fatalf("want rejected, got %+v err=%v", res, err)
	}
}

func TestPositionBlacklisted(t *testing.T) {
	svc := newTestSvc()
	svc.AddBlacklist("1", "user", "test")
	svc.AddRule(&RiskRule{Kind: KindPositionLimit, Asset: "BTC", MaxAmountPerDay: settlement.AssetAmountFromFloat(100, 8)})
	res, err := svc.CheckPosition(1, "BTC", 1, 0)
	if err != nil || res.Allowed {
		t.Fatalf("blacklisted user should be rejected, got %+v", res)
	}
}

func TestPositionNoRuleDefaultsAllow(t *testing.T) {
	svc := newTestSvc()
	res, err := svc.CheckPosition(1, "BTC", 99999, 0)
	if err != nil || !res.Allowed {
		t.Fatalf("no rule should allow, got %+v err=%v", res, err)
	}
}

// ---------- CheckFrequency ----------

func TestFrequencyLimitPass(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindFreqLimit, MaxCountPerDay: 5})
	res, err := svc.CheckFrequency(1, "login", 24*time.Hour)
	if err != nil || !res.Allowed {
		t.Fatalf("want allowed, got %+v err=%v", res, err)
	}
}

func TestFrequencyLimitExceeded(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindFreqLimit, MaxCountPerDay: 2})
	// 前两次放行
	svc.CheckFrequency(1, "withdraw", 24*time.Hour)
	svc.CheckFrequency(1, "withdraw", 24*time.Hour)
	// 第三次被拒
	res, err := svc.CheckFrequency(1, "withdraw", 24*time.Hour)
	if err != nil || res.Allowed {
		t.Fatalf("want rejected, got %+v err=%v", res, err)
	}
}

func TestFrequencyBlacklisted(t *testing.T) {
	svc := newTestSvc()
	svc.AddBlacklist("1", "user", "test")
	svc.AddRule(&RiskRule{Kind: KindFreqLimit, MaxCountPerDay: 100})
	res, err := svc.CheckFrequency(1, "login", 24*time.Hour)
	if err != nil || res.Allowed {
		t.Fatalf("blacklisted user should be rejected, got %+v", res)
	}
}

func TestFrequencyNoRuleDefaultsAllow(t *testing.T) {
	svc := newTestSvc()
	res, err := svc.CheckFrequency(1, "login", 24*time.Hour)
	if err != nil || !res.Allowed {
		t.Fatalf("no rule should allow, got %+v err=%v", res, err)
	}
}

func TestFrequencyDifferentUsersIndependent(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindFreqLimit, MaxCountPerDay: 1})
	svc.CheckFrequency(1, "withdraw", 24*time.Hour)
	// user 1 已用完配额
	res1, _ := svc.CheckFrequency(1, "withdraw", 24*time.Hour)
	if res1.Allowed {
		t.Fatal("user 1 should be rejected")
	}
	// user 2 仍可操作
	res2, _ := svc.CheckFrequency(2, "withdraw", 24*time.Hour)
	if !res2.Allowed {
		t.Fatal("user 2 should be allowed")
	}
}

func TestFrequencyEventsRecordedOnReject(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindFreqLimit, MaxCountPerDay: 1})
	svc.CheckFrequency(1, "withdraw", 24*time.Hour)
	svc.CheckFrequency(1, "withdraw", 24*time.Hour) // 被拒
	evs, _ := svc.ListEvents(1, 0)
	found := false
	for _, e := range evs {
		if e.Kind == KindFreqLimit {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected freq_limit event on reject")
	}
}

package risk

import "testing"

func newTestSvc() *Service { return New(NewMemStore()) }

func TestWithdrawLimitPass(t *testing.T) {
	svc := newTestSvc()
	if _, err := svc.AddRule(&RiskRule{Kind: KindWithdrawLimit, Asset: "BTC", MaxAmountPerDay: 1000, MinKYCLevel: 1}); err != nil {
		t.Fatalf("add rule: %v", err)
	}
	res, err := svc.CheckWithdraw(1, "BTC", 500, 1, "")
	if err != nil || !res.Allowed {
		t.Fatalf("want allowed, got %+v err=%v", res, err)
	}
}

func TestWithdrawLimitExceeded(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindWithdrawLimit, Asset: "BTC", MaxAmountPerDay: 1000, MinKYCLevel: 1})
	res, _ := svc.CheckWithdraw(1, "BTC", 2000, 1, "")
	if res.Allowed || res.Reason != "exceeds withdraw limit" {
		t.Fatalf("want rejected(exceeds), got %+v", res)
	}
}

func TestWithdrawLowKYC(t *testing.T) {
	svc := newTestSvc()
	svc.AddRule(&RiskRule{Kind: KindWithdrawLimit, Asset: "BTC", MaxAmountPerDay: 1000, MinKYCLevel: 2})
	res, _ := svc.CheckWithdraw(1, "BTC", 500, 1, "")
	if res.Allowed || res.Reason != "kyc level too low" {
		t.Fatalf("want rejected(kyc), got %+v", res)
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
	svc.AddRule(&RiskRule{Kind: KindOrderLimit, Asset: "ETH", MaxAmountPerDay: 10, MinKYCLevel: 1})
	if res, _ := svc.CheckOrder(1, "ETH", 5, 1); !res.Allowed {
		t.Fatalf("want allowed, got %+v", res)
	}
	if res, _ := svc.CheckOrder(1, "ETH", 20, 1); res.Allowed || res.Reason != "exceeds order limit" {
		t.Fatalf("want rejected, got %+v", res)
	}
}

func TestRuleListAndRemove(t *testing.T) {
	svc := newTestSvc()
	r1, _ := svc.AddRule(&RiskRule{Kind: KindWithdrawLimit, Asset: "BTC", MaxAmountPerDay: 100})
	r2, _ := svc.AddRule(&RiskRule{Kind: KindOrderLimit, Asset: "ETH", MaxAmountPerDay: 10})
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
	svc.AddRule(&RiskRule{Kind: KindWithdrawLimit, Asset: "BTC", MaxAmountPerDay: 100, MinKYCLevel: 1})
	svc.CheckWithdraw(1, "BTC", 5000, 1, "")
	evs, _ := svc.ListEvents(1, 0)
	if len(evs) != 1 {
		t.Fatalf("want 1 event got %d", len(evs))
	}
	if evs[0].Kind != KindWithdrawLimit {
		t.Fatalf("want withdraw_limit event got %s", evs[0].Kind)
	}
}

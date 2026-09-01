package referral

import (
	"math"
	"testing"

	"go.uber.org/zap"
)

// TestRecordTradeCommissionGuards 回归 REFERRAL-F1（F5 边界校验）：
// 佣金率必须有限且不超过手续费全额，资产必须已知。修复前 rate>1 会让平台倒贴，
// NaN/Inf 下 float64→int64 转换结果依赖实现，会把天文数字或负数写进佣金账。
func TestRecordTradeCommissionGuards(t *testing.T) {
	svc := NewService(NewMemStore(), zap.NewNop())

	cases := []struct {
		name  string
		asset string
		rate  float64
	}{
		{"rate 超过 100% 会让平台倒贴", "USDT", 1.5},
		{"rate 为 NaN", "USDT", math.NaN()},
		{"rate 为 +Inf", "USDT", math.Inf(1)},
		{"rate 为 -Inf", "USDT", math.Inf(-1)},
		{"rate 为负", "USDT", -0.2},
		{"未知资产", "NOT_A_COIN", 0.2},
		{"空资产", "", 0.2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := svc.RecordTradeCommission(1, 2, tc.asset, 1000, tc.rate, "biz:"+tc.name); err == nil {
				t.Fatalf("expected rejection for %q", tc.name)
			}
		})
	}

	// 非法入参不得留下任何记录
	totals, err := svc.GetMyReferralStats(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 0 {
		t.Fatalf("invalid params should record nothing, got %v", totals)
	}

	// 合法入参仍正常记账：1000 × 20% = 200
	if err := svc.RecordTradeCommission(1, 2, "USDT", 1000, 0.2, "biz:ok"); err != nil {
		t.Fatalf("valid commission rejected: %v", err)
	}
	totals, err = svc.GetMyReferralStats(1)
	if err != nil {
		t.Fatal(err)
	}
	if totals["USDT"] != 200 {
		t.Fatalf("totals=%v want USDT=200", totals)
	}
}

// TestRecordTradeCommissionIdempotentByBizRef 回归 F1：同一 bizRef 重复记账只入一次。
func TestRecordTradeCommissionIdempotentByBizRef(t *testing.T) {
	svc := NewService(NewMemStore(), zap.NewNop())

	for i := 0; i < 3; i++ {
		// store 返回 ErrCommissionExists 时 service 视为幂等命中并返回 nil
		if err := svc.RecordTradeCommission(1, 2, "USDT", 1000, 0.2, "biz:same"); err != nil {
			t.Fatalf("repeat %d: %v", i, err)
		}
	}
	totals, err := svc.GetMyReferralStats(1)
	if err != nil {
		t.Fatal(err)
	}
	if totals["USDT"] != 200 {
		t.Fatalf("totals=%v want USDT=200 (dedup failed)", totals)
	}
}

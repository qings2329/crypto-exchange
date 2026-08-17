package futures

import (
	"math"
	"testing"
	"time"
)

// 演示：同一结算周期内重复调用 Settle 必须幂等，不得二次扣收资金费（F1）。
func TestFundingIdempotent(t *testing.T) {
	sym := "BTC_USDT_PERP"
	fm := NewFundingManager(time.Hour) // 大周期，两次调用落在同一周期
	fm.Register(sym)
	fm.UpdateIndexPrice(sym, 50000)

	long := Position{UserID: 1, Symbol: sym, Side: Long, Size: 1, EntryPrice: 50000, Margin: 5000, Leverage: 10}
	short := Position{UserID: 2, Symbol: sym, Side: Short, Size: 1, EntryPrice: 50000, Margin: 5000, Leverage: 10}
	positions := []Position{long, short}

	ev1 := fm.Settle(sym, 50000, 0.001, positions)
	if len(ev1.Payments) == 0 {
		t.Fatal("首次结算应产生资金费支付")
	}
	ev2 := fm.Settle(sym, 50000, 0.001, positions)
	if len(ev2.Payments) != 0 {
		t.Fatalf("同周期二次结算必须幂等（不重复扣费），实际产生 %d 笔", len(ev2.Payments))
	}
}

// 演示：异常行情（NaN/Inf）不得产生资金费支付，避免费率污染（F5）。
func TestFundingRejectsInvalidMark(t *testing.T) {
	sym := "BTC_USDT_PERP"
	fm := NewFundingManager(time.Hour)
	fm.Register(sym)
	fm.UpdateIndexPrice(sym, 50000)

	long := Position{UserID: 1, Symbol: sym, Side: Long, Size: 1, EntryPrice: 50000, Margin: 5000, Leverage: 10}
	positions := []Position{long}

	if ev := fm.Settle(sym, math.NaN(), 0.001, positions); len(ev.Payments) != 0 {
		t.Fatalf("NaN 标记价不应产生支付，实际 %d 笔", len(ev.Payments))
	}
	if ev := fm.Settle(sym, math.Inf(1), 0.001, positions); len(ev.Payments) != 0 {
		t.Fatalf("Inf 标记价不应产生支付，实际 %d 笔", len(ev.Payments))
	}
	// 正常价仍应结算
	if ev := fm.Settle(sym, 50000, 0.001, positions); len(ev.Payments) == 0 {
		t.Fatal("正常标记价应产生支付")
	}
}

func TestPremiumIndex(t *testing.T) {
	// 标记价高于指数价 -> 正溢价
	if got := PremiumIndex(50500, 50000); !approx(got, 0.01, 1e-9) {
		t.Fatalf("PremiumIndex = %v, want 0.01", got)
	}
	// 折价
	if got := PremiumIndex(49500, 50000); !approx(got, -0.01, 1e-9) {
		t.Fatalf("PremiumIndex = %v, want -0.01", got)
	}
	// 指数价非法 -> 0
	if got := PremiumIndex(50000, 0); got != 0 {
		t.Fatalf("PremiumIndex(index=0) = %v, want 0", got)
	}
}

func TestFundingRateClamp(t *testing.T) {
	// 高溢价被限幅到 +0.05%
	rate := FundingRate(InterestRatePerInterval, 0.01)
	want := InterestRatePerInterval + PremiumClamp // 0.0001 + 0.0005
	if !approx(rate, want, 1e-12) {
		t.Fatalf("FundingRate = %v, want %v", rate, want)
	}
	// 深度折价被限幅到 -0.05%
	rate = FundingRate(InterestRatePerInterval, -0.02)
	want = InterestRatePerInterval - PremiumClamp // 0.0001 - 0.0005 = -0.0004
	if !approx(rate, want, 1e-12) {
		t.Fatalf("FundingRate = %v, want %v", rate, want)
	}
	t.Logf("限幅费率: 正溢价=%.6f 负溢价=%.6f", InterestRatePerInterval+PremiumClamp, InterestRatePerInterval-PremiumClamp)
}

func TestMarkPriceCalculator(t *testing.T) {
	mc := NewMarkPriceCalculator(0)
	mc.SetIndex(50000)
	// 连续喂入溢价 0.004 的合约价 (50200)，EMA 应收敛到 0.004
	for i := 0; i < 20; i++ {
		mc.UpdateContractPrice(50200)
	}
	if !approx(mc.PremiumEMA(), 0.004, 1e-9) {
		t.Fatalf("PremiumEMA 应收敛到 0.004, 实际 %v", mc.PremiumEMA())
	}
	// 标记价 = 指数 × (1 + 溢价EMA) = 50000 × 1.004 = 50200
	if !approx(mc.MarkPrice(), 50200, 1e-6) {
		t.Fatalf("MarkPrice = %v, want 50200", mc.MarkPrice())
	}
	t.Logf("收敛: 溢价EMA=%.6f 标记价=%.2f", mc.PremiumEMA(), mc.MarkPrice())
}

func TestMarkPriceAntiWick(t *testing.T) {
	// 抗插针：正常溢价后，单笔极端成交价对标记价影响极小。
	mc := NewMarkPriceCalculator(0)
	mc.SetIndex(50000)
	for i := 0; i < 10; i++ {
		mc.UpdateContractPrice(51000) // 稳定溢价 2%
	}
	stable := mc.MarkPrice() // 约 51000
	// 一笔插针单砸到 40000（瞬时 -20%），标记价应几乎不动
	mc.UpdateContractPrice(40000)
	afterWick := mc.MarkPrice()
	drop := (stable - afterWick) / stable
	if drop > 0.03 {
		t.Fatalf("插针单导致标记价下跌 %.4f, 应 < 0.03 (抗插针失效)", drop)
	}
	t.Logf("插针前标记价=%.2f 插针后=%.2f 跌幅=%.4f (应很小)", stable, afterWick, drop)
}

func TestFundingPaymentDirection(t *testing.T) {
	sym := "BTC_USDT_PERP"
	fm := NewFundingManager(time.Hour)
	fm.Register(sym)
	fm.UpdateIndexPrice(sym, 50000)

	// 用标记价格计算器构造正溢价 EMA
	mc := NewMarkPriceCalculator(0)
	mc.SetIndex(50000)
	for i := 0; i < 10; i++ {
		mc.UpdateContractPrice(51000) // 溢价 2%
	}
	if mc.PremiumEMA() <= 0 {
		t.Fatalf("期望正溢价, 实际 %v", mc.PremiumEMA())
	}

	long := Position{UserID: 1, Symbol: sym, Side: Long, Size: 1, EntryPrice: 50000}
	short := Position{UserID: 2, Symbol: sym, Side: Short, Size: 1, EntryPrice: 50000}

	ev := fm.Settle(sym, mc.MarkPrice(), mc.PremiumEMA(), []Position{long, short})
	find := func(u int64) FundingPayment {
		for _, p := range ev.Payments {
			if p.UserID == u {
				return p
			}
		}
		t.Fatalf("未找到用户 %d 的支付", u)
		return FundingPayment{}
	}
	lp := find(1)
	sp := find(2)
	// 多头支付（负），空头收取（正），绝对值相等
	if lp.Payment >= 0 {
		t.Fatalf("多头应付出(负), 实际 %v", lp.Payment)
	}
	if sp.Payment <= 0 {
		t.Fatalf("空头应收取(正), 实际 %v", sp.Payment)
	}
	if !approx(lp.Payment+sp.Payment, 0, 1e-9) {
		t.Fatalf("多空支付应相互抵消, got %v + %v", lp.Payment, sp.Payment)
	}
	t.Logf("费率=%.6f 多头付=%.4f 空头收=%.4f", ev.FundingRate, lp.Payment, sp.Payment)
}

func TestFundingSettleEvent(t *testing.T) {
	sym := "BTC_USDT_PERP"
	fm := NewFundingManager(time.Hour)
	fm.Register(sym)
	fm.UpdateIndexPrice(sym, 50000)

	mc := NewMarkPriceCalculator(0)
	mc.SetIndex(50000)
	for i := 0; i < 10; i++ {
		mc.UpdateContractPrice(50500) // 溢价 1%
	}
	pos := []Position{
		{UserID: 1, Symbol: sym, Side: Long, Size: 2, EntryPrice: 50000},
		{UserID: 2, Symbol: sym, Side: Short, Size: 2, EntryPrice: 50000},
	}
	ev := fm.Settle(sym, mc.MarkPrice(), mc.PremiumEMA(), pos)
	if len(ev.Payments) != 2 {
		t.Fatalf("应产生 2 笔支付, 实际 %d", len(ev.Payments))
	}
	if ev.FundingRate == 0 {
		t.Fatalf("结算事件费率不应为 0")
	}
	t.Logf("结算事件: 费率=%.6f 笔数=%d 标记价=%.2f", ev.FundingRate, len(ev.Payments), ev.MarkPrice)
}

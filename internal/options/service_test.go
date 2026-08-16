package options

import (
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// eqAmt 将账本返回的 AssetAmount 与人类单位字面量按资产小数位精确比较（无 epsilon）。
func eqAmt(a settlement.AssetAmount, human float64, asset string) bool {
	return a.Cmp(settlement.AssetAmountFromFloat(human, settlement.AssetDecimalsByName(asset))) == 0
}

func newTestService() (*Service, *ledger.Ledger) {
	store := NewMemStore()
	l := ledger.New()
	for _, uid := range []int64{1, 2, 3, 4} {
		_ = l.Deposit(uid, "USDT", settlement.AssetAmountFromFloat(100000, settlement.AssetDecimalsByName("USDT")), "seed")
	}
	priceFn := func(asset string) (float64, bool) {
		if asset == "BTC" {
			return 50000, true
		}
		return 0, false
	}
	cfg := Config{
		QuoteAsset:    "USDT",
		RiskFreeRate:  0.03,
		Volatility:    0.6,
		MarginRatio:   0.3,
		SettleInterval: time.Second,
	}
	return NewService(store, l, cfg, nil, priceFn), l
}

func mustContract(s *Service, premium float64, expiry time.Time) *OptionContract {
	c := &OptionContract{
		Underlying: "BTC", QuoteAsset: "USDT", Strike: 40000,
		Expiry: expiry, Type: TypeCall, Style: StyleAmerican,
		ContractSize: 1, Premium: premium,
	}
	if err := s.CreateContract(c); err != nil {
		panic(err)
	}
	return c
}

func TestOpenLongPaysPremium(t *testing.T) {
	svc, l := newTestService()
	c := mustContract(svc, 1000, time.Now().Add(time.Hour))
	const uid = int64(1)

	p, err := svc.OpenPosition(uid, c.ID, SideLong, 2)
	if err != nil {
		t.Fatalf("open long: %v", err)
	}
	if p.Side != SideLong || p.Quantity != 2 || p.Premium != 1000 {
		t.Fatalf("unexpected position: %+v", p)
	}
	// 用户可用扣 2*1000=2000；系统对手方收 2000。
	if avail, _, _ := l.Balance(uid, "USDT"); !eqAmt(avail, 98000, "USDT") {
		t.Fatalf("long premium not deducted: avail=%v want ~98000", avail)
	}
	if ins, _, _ := l.Balance(ledger.SysOptions, "USDT"); ins.Cmp(settlement.AssetAmountFromFloat(1999, settlement.AssetDecimalsByName("USDT"))) < 0 {
		t.Fatalf("system did not receive premium: sys=%v", ins)
	}
}

func TestOpenShortFrozenAndReceivesPremium(t *testing.T) {
	svc, l := newTestService()
	c := mustContract(svc, 1000, time.Now().Add(time.Hour))
	const uid = int64(2)

	p, err := svc.OpenPosition(uid, c.ID, SideShort, 2)
	if err != nil {
		t.Fatalf("open short: %v", err)
	}
	// 保证金 = strike*size*qty*ratio = 40000*1*2*0.3 = 24000。
	if p.Margin < 23999 || p.Margin > 24001 {
		t.Fatalf("unexpected margin: %.4f", p.Margin)
	}
	avail, frozen, _ := l.Balance(uid, "USDT")
	// 可用 = 100000 - 24000(冻结) + 2000(权利金) = 78000。
	if avail.Cmp(settlement.AssetAmountFromFloat(77999, settlement.AssetDecimalsByName("USDT"))) < 0 ||
		avail.Cmp(settlement.AssetAmountFromFloat(78001, settlement.AssetDecimalsByName("USDT"))) > 0 {
		t.Fatalf("short avail wrong: %v", avail)
	}
	if frozen.Cmp(settlement.AssetAmountFromFloat(23999, settlement.AssetDecimalsByName("USDT"))) < 0 ||
		frozen.Cmp(settlement.AssetAmountFromFloat(24001, settlement.AssetDecimalsByName("USDT"))) > 0 {
		t.Fatalf("short margin not frozen: %v", frozen)
	}
}

func TestExerciseLongPayoff(t *testing.T) {
	svc, l := newTestService()
	c := mustContract(svc, 1000, time.Now().Add(time.Hour)) // american, 可随时行权
	const uid = int64(1)

	p, err := svc.OpenPosition(uid, c.ID, SideLong, 1)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// spot=50000, ITV=(50000-40000)*1=10000; payoff=10000-1000=9000。
	if err := svc.Exercise(uid, p.ID); err != nil {
		t.Fatalf("exercise: %v", err)
	}
	avail, _, _ := l.Balance(uid, "USDT")
	if avail.Cmp(settlement.AssetAmountFromFloat(107999, settlement.AssetDecimalsByName("USDT"))) < 0 ||
		avail.Cmp(settlement.AssetAmountFromFloat(108001, settlement.AssetDecimalsByName("USDT"))) > 0 {
		t.Fatalf("exercise payoff wrong: avail=%v want ~108000", avail)
	}
	got, _ := svc.GetPosition(p.ID)
	if got.Status != StatusExercised {
		t.Fatalf("expected exercised, got %s", got.Status)
	}
}

func TestSettleExpiryLong(t *testing.T) {
	svc, l := newTestService()
	c := mustContract(svc, 1000, time.Now().Add(-time.Hour)) // 已到期
	const uid = int64(1)
	p, _ := svc.OpenPosition(uid, c.ID, SideLong, 1)

	settled, err := svc.SettlePosition(p.ID)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if !settled {
		t.Fatal("expected settled=true")
	}
	avail, _, _ := l.Balance(uid, "USDT")
	if avail.Cmp(settlement.AssetAmountFromFloat(107999, settlement.AssetDecimalsByName("USDT"))) < 0 ||
		avail.Cmp(settlement.AssetAmountFromFloat(108001, settlement.AssetDecimalsByName("USDT"))) > 0 {
		t.Fatalf("long settle payoff wrong: avail=%v want ~108000", avail)
	}
	got, _ := svc.GetPosition(p.ID)
	if got.Status != StatusExpired {
		t.Fatalf("expected expired, got %s", got.Status)
	}
}

func TestSettleExpiryShort(t *testing.T) {
	svc, l := newTestService()
	c := mustContract(svc, 1000, time.Now().Add(-time.Hour)) // 已到期
	const uid = int64(2)
	p, _ := svc.OpenPosition(uid, c.ID, SideShort, 1) // margin=12000

	settled, err := svc.SettlePosition(p.ID)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if !settled {
		t.Fatal("expected settled=true")
	}
	// 解冻 12000，支付损失 min(ITV=10000, margin=12000)=10000。
	// 可用 = 100000 - 12000 + 1000(权利金) + 12000(解冻) - 10000(损失) = 91000。
	avail, frozen, _ := l.Balance(uid, "USDT")
	if avail.Cmp(settlement.AssetAmountFromFloat(90999, settlement.AssetDecimalsByName("USDT"))) < 0 ||
		avail.Cmp(settlement.AssetAmountFromFloat(91001, settlement.AssetDecimalsByName("USDT"))) > 0 {
		t.Fatalf("short settle avail wrong: %v want ~91000", avail)
	}
	if frozen.Sign() > 0 {
		t.Fatalf("short margin not released: %v", frozen)
	}
	got, _ := svc.GetPosition(p.ID)
	if got.Status != StatusExpired {
		t.Fatalf("expected expired, got %s", got.Status)
	}
}

func TestSettleNotExpiredSkips(t *testing.T) {
	svc, _ := newTestService()
	c := mustContract(svc, 1000, time.Now().Add(time.Hour)) // 未到期
	const uid = int64(1)
	p, _ := svc.OpenPosition(uid, c.ID, SideLong, 1)

	settled, err := svc.SettlePosition(p.ID)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if settled {
		t.Fatal("expected settled=false for unexpired")
	}
	got, _ := svc.GetPosition(p.ID)
	if got.Status != StatusOpen {
		t.Fatalf("expected still open, got %s", got.Status)
	}
}

func TestQuoteRejectsMissingPrice(t *testing.T) {
	svc, _ := newTestService()
	// ETH 不在 priceFn 中（无行情），premium 显式给定以通过创建。
	c := &OptionContract{
		Underlying: "ETH", QuoteAsset: "USDT", Strike: 100,
		Expiry: time.Now().Add(time.Hour), Type: TypeCall,
		Style: StyleAmerican, Premium: 100,
	}
	if err := svc.CreateContract(c); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := svc.Quote(c.ID); err != ErrNoPriceFeed {
		t.Fatalf("expected ErrNoPriceFeed, got %v", err)
	}
}

func TestBlackScholes(t *testing.T) {
	// ATM call 价格 > 0 且较小。
	price, delta := BlackScholes(TypeCall, 100, 100, 1, 0.03, 0.2)
	if price <= 0 || price > 20 {
		t.Fatalf("ATM call price out of range: %.4f", price)
	}
	if delta <= 0.4 || delta >= 0.6 {
		t.Fatalf("ATM call delta should be ~0.5, got %.4f", delta)
	}
	// 深度 ITM call：价格 ≈ spot - strike，delta ≈ 1。
	price, delta = BlackScholes(TypeCall, 200, 100, 1, 0.03, 0.2)
	if price < 95 || price > 105 {
		t.Fatalf("deep ITM call price ~ spot-strike, got %.4f", price)
	}
	if delta < 0.99 {
		t.Fatalf("deep ITM call delta ~1, got %.4f", delta)
	}
	// 退化 t=0：call 返回内在价值。
	price, _ = BlackScholes(TypeCall, 150, 100, 0, 0.03, 0.2)
	if price < 49.9 || price > 50.1 {
		t.Fatalf("expired call price = intrinsic, got %.4f", price)
	}
	// put OTM（spot>strike）内在价值 0。
	price, _ = BlackScholes(TypePut, 150, 100, 0, 0.03, 0.2)
	if price > 1e-6 {
		t.Fatalf("expired OTM put price should be 0, got %.4f", price)
	}
}

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

// aa 按资产小数位把人类单位字面量构造为定点 AssetAmount（测试构造合约字段用）。
func aa(asset string, human float64) settlement.AssetAmount {
	return settlement.AssetAmountFromFloat(human, settlement.AssetDecimalsByName(asset))
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
		Underlying: "BTC", QuoteAsset: "USDT", Strike: aa("USDT", 40000),
		Expiry: expiry, Type: TypeCall, Style: StyleAmerican,
		ContractSize: aa("USDT", 1), Premium: aa("USDT", premium),
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
	if p.Side != SideLong || p.Quantity != 2 || p.Premium.HumanFloat() != 1000 {
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
	if p.Margin.HumanFloat() < 23999 || p.Margin.HumanFloat() > 24001 {
		t.Fatalf("unexpected margin: %.4f", p.Margin.HumanFloat())
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
	// spot=50000, ITV=(50000-40000)*1=10000; 中性 CCP payoff=10000（不再扣权利金）。
	if err := svc.Exercise(uid, p.ID); err != nil {
		t.Fatalf("exercise: %v", err)
	}
	avail, _, _ := l.Balance(uid, "USDT")
	if avail.Cmp(settlement.AssetAmountFromFloat(108999, settlement.AssetDecimalsByName("USDT"))) < 0 ||
		avail.Cmp(settlement.AssetAmountFromFloat(109001, settlement.AssetDecimalsByName("USDT"))) > 0 {
		t.Fatalf("exercise payoff wrong: avail=%v want ~109000", avail)
	}
	// 净收益对账（F3-1 + F3-4 偿付能力护栏）：long 净收益 = 内在价值 - 已付权利金 = 10000 - 1000 = 9000，
	// 用户足额收到。CCP（SysOptions）仅收过 1000 权利金、需付 10000，缺口 9000 在护栏下
	// 不再击穿 CCP 为负，而是付尽 CCP（→0）后由穿仓损失账户（SysLiquidationLoss）垫付（本测试未注入保险基金）。
	if ins, _, _ := l.Balance(ledger.SysOptions, "USDT"); !eqAmt(ins, 0, "USDT") {
		t.Fatalf("SysOptions CCP should be drained to 0 (not negative): %v", ins)
	}
	if loss, _, _ := l.Balance(ledger.SysLiquidationLoss, "USDT"); !eqAmt(loss, -9000, "USDT") {
		t.Fatalf("CCP deficit should be absorbed by liquidation-loss account: %v want -9000", loss)
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
	if avail.Cmp(settlement.AssetAmountFromFloat(108999, settlement.AssetDecimalsByName("USDT"))) < 0 ||
		avail.Cmp(settlement.AssetAmountFromFloat(109001, settlement.AssetDecimalsByName("USDT"))) > 0 {
		t.Fatalf("long settle payoff wrong: avail=%v want ~109000", avail)
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

func TestExerciseIdempotent(t *testing.T) {
	svc, l := newTestService()
	c := mustContract(svc, 1000, time.Now().Add(time.Hour)) // american, 可随时行权
	const uid = int64(1)
	p, _ := svc.OpenPosition(uid, c.ID, SideLong, 1)

	if err := svc.Exercise(uid, p.ID); err != nil {
		t.Fatalf("exercise#1: %v", err)
	}
	// 第二次行权应被终态短路，不重复吐钱。
	if err := svc.Exercise(uid, p.ID); err != ErrAlreadySettled {
		t.Fatalf("exercise#2: expected ErrAlreadySettled, got %v", err)
	}
	// 收益只结算一次：可用 = 100000 - 1000(权利金) + 10000(payoff) = 109000。
	avail, _, _ := l.Balance(uid, "USDT")
	if !eqAmt(avail, 109000, "USDT") {
		t.Fatalf("exercise not idempotent: avail=%v want 109000", avail)
	}
	got, _ := svc.GetPosition(p.ID)
	if got.Status != StatusExercised {
		t.Fatalf("expected exercised, got %s", got.Status)
	}
}

func TestSettleLongIdempotent(t *testing.T) {
	svc, l := newTestService()
	c := mustContract(svc, 1000, time.Now().Add(-time.Hour)) // 已到期
	const uid = int64(1)
	p, _ := svc.OpenPosition(uid, c.ID, SideLong, 1)

	settled, err := svc.SettlePosition(p.ID)
	if err != nil || !settled {
		t.Fatalf("settle#1: settled=%v err=%v", settled, err)
	}
	// 第二次结算应终态短路。
	settled, err = svc.SettlePosition(p.ID)
	if err != nil || settled {
		t.Fatalf("settle#2: expected (false,nil), got settled=%v err=%v", settled, err)
	}
	// 收益只结算一次：可用 = 109000。
	avail, _, _ := l.Balance(uid, "USDT")
	if !eqAmt(avail, 109000, "USDT") {
		t.Fatalf("settle not idempotent: avail=%v want 109000", avail)
	}
}

func TestSettleShortIdempotent(t *testing.T) {
	svc, l := newTestService()
	c := mustContract(svc, 1000, time.Now().Add(-time.Hour)) // 已到期
	const uid = int64(2)
	p, _ := svc.OpenPosition(uid, c.ID, SideShort, 1) // margin=12000

	settled, err := svc.SettlePosition(p.ID)
	if err != nil || !settled {
		t.Fatalf("settle#1: settled=%v err=%v", settled, err)
	}
	settled, err = svc.SettlePosition(p.ID)
	if err != nil || settled {
		t.Fatalf("settle#2: expected (false,nil), got settled=%v err=%v", settled, err)
	}
	// 只结算一次：可用 = 100000 - 12000(冻结) + 1000(权利金) + 12000(解冻) - 10000(损失) = 91000。
	avail, frozen, _ := l.Balance(uid, "USDT")
	if !eqAmt(avail, 91000, "USDT") {
		t.Fatalf("short settle not idempotent: avail=%v want 91000", avail)
	}
	if frozen.Sign() > 0 {
		t.Fatalf("short margin not released: %v", frozen)
	}
}

func TestOpenLongTransferFailureRollsBack(t *testing.T) {
	svc, l := newTestService()
	c := mustContract(svc, 1000, time.Now().Add(time.Hour))
	const uid = int64(1)
	// 把余额压到不足，触发资金不足路径，验证不残留持仓。
	if avail, _, _ := l.Balance(uid, "USDT"); avail.Sign() > 0 {
		_ = l.Transfer(uid, ledger.SysBadDebt, "USDT", avail, "drain", "test-drain")
	}
	if _, err := svc.OpenPosition(uid, c.ID, SideLong, 2); err == nil {
		t.Fatal("expected insufficient balance error")
	}
	// 余额不足时不应创建任何持仓。
	positions, _ := svc.ListPositions(uid)
	if len(positions) != 0 {
		t.Fatalf("rollback left %d stale positions: %+v", len(positions), positions)
	}
}

func TestQuoteRejectsMissingPrice(t *testing.T) {
	svc, _ := newTestService()
	// ETH 不在 priceFn 中（无行情），premium 显式给定以通过创建。
	c := &OptionContract{
		Underlying: "ETH", QuoteAsset: "USDT", Strike: aa("USDT", 100),
		Expiry: time.Now().Add(time.Hour), Type: TypeCall,
		Style: StyleAmerican, ContractSize: aa("USDT", 1), Premium: aa("USDT", 100),
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

// TestCreateContractRejectsUnsupportedAsset F5-1：未知计价资产不得建约（防按默认 8 位缩放错配/铸造）。
func TestCreateContractRejectsUnsupportedAsset(t *testing.T) {
	svc, _ := newTestService()
	c := &OptionContract{
		Underlying: "BTC", QuoteAsset: "XYZ", Strike: aa("USDT", 40000),
		Expiry: time.Now().Add(time.Hour), Type: TypeCall, Style: StyleAmerican,
		ContractSize: aa("USDT", 1), Premium: aa("USDT", 100),
	}
	if err := svc.CreateContract(c); err != ErrUnsupportedAsset {
		t.Fatalf("expected ErrUnsupportedAsset, got %v", err)
	}
}

// TestCreateContractRejectsBadContractSize F5-5：非正合约乘数直接拒绝（不再静默置 1）。
func TestCreateContractRejectsBadContractSize(t *testing.T) {
	svc, _ := newTestService()
	c := &OptionContract{
		Underlying: "BTC", QuoteAsset: "USDT", Strike: aa("USDT", 40000),
		Expiry: time.Now().Add(time.Hour), Type: TypeCall, Style: StyleAmerican,
		ContractSize: aa("USDT", -1), Premium: aa("USDT", 100),
	}
	if err := svc.CreateContract(c); err == nil {
		t.Fatal("expected error for negative contract_size")
	}
}

// TestExerciseRejectsBadSpot F5-4：负价行情不得行权（防看跌赔付被放大）。
func TestExerciseRejectsBadSpot(t *testing.T) {
	svc, _ := newTestService()
	svc.priceFn = func(asset string) (float64, bool) { return -1, true }
	c := mustContract(svc, 1000, time.Now().Add(time.Hour))
	const uid = int64(1)
	p, err := svc.OpenPosition(uid, c.ID, SideLong, 1)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := svc.Exercise(uid, p.ID); err != ErrNoPriceFeed {
		t.Fatalf("expected ErrNoPriceFeed for negative spot, got %v", err)
	}
}

// TestOpenPositionRejectsUnsupportedAsset F5-2：持仓计价资产未知时不得开仓（防御性校验，
// 即使合约经 store 直写绕过 CreateContract 白名单也应被拒）。
func TestOpenPositionRejectsUnsupportedAsset(t *testing.T) {
	svc, _ := newTestService()
	c := &OptionContract{
		Underlying: "BTC", QuoteAsset: "XYZ", Strike: aa("USDT", 40000),
		Expiry: time.Now().Add(time.Hour), Type: TypeCall, Style: StyleAmerican,
		ContractSize: aa("USDT", 1), Premium: aa("USDT", 100),
	}
	if err := svc.store.CreateContract(c); err != nil {
		t.Fatalf("seed contract: %v", err)
	}
	if _, err := svc.OpenPosition(1, c.ID, SideLong, 1); err != ErrUnsupportedAsset {
		t.Fatalf("expected ErrUnsupportedAsset, got %v", err)
	}
}

// TestExerciseCCPSolvencyGuard F3-4：行权收益超过 CCP（SysOptions）现有余额时，
// CCP 被付尽（不为负），缺口由保险基金（SysInsurance）垫付，用户仍足额收到。
func TestExerciseCCPSolvencyGuard(t *testing.T) {
	svc, l := newTestService()
	// 保险基金注入 20000，足以覆盖缺口。
	ins := settlement.AssetAmountFromFloat(20000, settlement.AssetDecimalsByName("USDT"))
	if err := l.CreditAvailable(ledger.SysInsurance, "USDT", ins, "seed_ins", ""); err != nil {
		t.Fatalf("seed insurance: %v", err)
	}
	const uid = int64(1)
	c := mustContract(svc, 1000, time.Now().Add(time.Hour))
	p, err := svc.OpenPosition(uid, c.ID, SideLong, 2)
	if err != nil {
		t.Fatalf("open long: %v", err)
	}
	// 开仓付权利金 2*1000=2000 入 CCP；CCP 余额=2000。
	if ccp, _, _ := l.Balance(ledger.SysOptions, "USDT"); !eqAmt(ccp, 2000, "USDT") {
		t.Fatalf("CCP before exercise %v want 2000", ccp)
	}
	// 美式期权随时可行权；BTC=50000、行权价 40000、数量 2 → 内在价值 payoff=20000。
	if err := svc.Exercise(uid, p.ID); err != nil {
		t.Fatalf("exercise: %v", err)
	}
	// 用户收到全额 20000（账户 = 100000 - 2000 权利金 + 20000 收益 = 118000）。
	if avail, _, _ := l.Balance(uid, "USDT"); !eqAmt(avail, 118000, "USDT") {
		t.Fatalf("user after exercise %v want 118000 (full payoff)", avail)
	}
	// CCP 被付尽，不为负（下限护栏 = 0）。
	if ccp, _, _ := l.Balance(ledger.SysOptions, "USDT"); ccp.Sign() != 0 {
		t.Fatalf("CCP after exercise %v want 0 (drained, not negative)", ccp)
	}
	// 缺口 20000-2000=18000 由保险基金承担：保险剩 20000-18000=2000。
	if insAfter, _, _ := l.Balance(ledger.SysInsurance, "USDT"); !eqAmt(insAfter, 2000, "USDT") {
		t.Fatalf("insurance after exercise %v want 2000 (backstop 18000)", insAfter)
	}
}

package spot

import (
	"fmt"
	"testing"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/settlement"
	"go.uber.org/zap"
)

// ---- 重启恢复回归测试（openOrders 对账重建 + 补账重放）----
//
// 背景：撮合引擎有独立快照/WAL，重启后旧挂单仍在簿；若 spot 侧冻结登记丢失，
// 僵尸单成交会走「无冻结记录→纯转账」分支（资金安全缺口）。本组测试验证
// RestoreOrders 的三态对账与 CatchUpSettlement 的幂等补账。

// newRestoreServer 构造带 fake 撮合的 Server（newTestServer 不含 client 字段）。
func newRestoreServer(fm *fakeMatcher) *Server {
	cfg := &config.Config{}
	cfg.Matching.DefaultPriceScale = 2
	cfg.Matching.DefaultQtyScale = 8
	return &Server{
		log:          zap.NewNop(),
		client:       fm,
		cfg:          cfg,
		ledgerSvc:    ledger.New(),
		openOrders:   make(map[int64]*freezeRec),
		clientOIDMap: make(map[string]int64),
		settledRefs:  make(map[string]bool),
	}
}

const usdtDec = 8 // settlement.AssetDecimalsByName("USDT")

// 场景1：订单仍挂簿（open）→ openOrders/clientOIDMap 完整重建，冻结原样保留。
func TestRestoreOpenOrderRebuildsRegistry(t *testing.T) {
	fm := &fakeMatcher{orders: map[int64]matching.OrderView{}}
	s := newRestoreServer(fm)
	seed(s, 1, 1000, 0)

	rec, err := s.reserveOnOpen(1, matching.Buy, fxPrice(100), fxQty(1), "BTC_USDT")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	const oid = int64(501)
	rec.clientOID = "cli-1"
	fm.orders[oid] = matching.OrderView{ID: oid, UserID: 1, Status: matching.OrderOpen}

	recs := []OrderRecord{freezeRecToRecord(oid, rec, "cli-1")}
	s.RestoreOrders(recs)

	got, ok := s.openOrders[oid]
	if !ok || got.user != 1 || got.frozenQuote.Cmp(settlement.AssetAmountFromFloat(100, usdtDec)) != 0 {
		t.Fatalf("freeze registry wrong: %+v", got)
	}
	if s.clientOIDMap["1:cli-1"] != oid {
		t.Fatal("clientOIDMap not rebuilt")
	}
	_, frozen, _ := s.ledgerSvc.Balance(1, "USDT")
	if !eqAmt(frozen, 100, "USDT") {
		t.Fatalf("frozen changed after restore: %v", frozen)
	}
}

// 场景2：订单在停机期间已撤销 → 残留冻结释放，僵尸记录不重建。
func TestRestoreTerminalOrderReleasesFreeze(t *testing.T) {
	fm := &fakeMatcher{orders: map[int64]matching.OrderView{}}
	s := newRestoreServer(fm)
	seed(s, 1, 1000, 0)
	if _, err := s.reserveOnOpen(1, matching.Buy, fxPrice(100), fxQty(1), "BTC_USDT"); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	const oid = int64(502)
	fm.orders[oid] = matching.OrderView{ID: oid, UserID: 1, Status: matching.OrderCanceled}

	recs := []OrderRecord{{
		OrderID: oid, User: 1, Side: int(matching.Buy), Symbol: "BTC_USDT",
		Base: "BTC", Quote: "USDT",
		FrozenQuote: settlement.AssetAmountFromFloat(100, usdtDec),
	}}
	s.RestoreOrders(recs)

	if len(s.openOrders) != 0 {
		t.Fatal("terminal order should not be in openOrders")
	}
	avail, frozen, _ := s.ledgerSvc.Balance(1, "USDT")
	if !eqAmt(avail, 1000, "USDT") || !eqAmt(frozen, 0, "USDT") {
		t.Fatalf("freeze not released: avail=%v frozen=%v", avail, frozen)
	}
	if !s.ledgerSvc.IsBalanced() {
		t.Fatal("ledger unbalanced after release")
	}
}

// 场景3：撮合不可达 → 保守保留冻结登记（不可误释放）。
func TestRestoreUnreachableKeepsFreeze(t *testing.T) {
	fm := &fakeMatcher{unreachable: true}
	s := newRestoreServer(fm)
	btcAmt := settlement.AssetAmountFromFloat(1, settlement.AssetDecimalsByName("BTC"))
	if err := s.ledgerSvc.ReceiveOnChain(1, "BTC", btcAmt, fmt.Sprintf("seed:%d:BTC", 1)); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.ledgerSvc.Freeze(1, "BTC", btcAmt); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	recs := []OrderRecord{{
		OrderID: 503, User: 1, Side: int(matching.Sell), Symbol: "BTC_USDT",
		Base: "BTC", Quote: "USDT", FrozenBase: btcAmt,
	}}
	s.RestoreOrders(recs)

	if len(s.openOrders) != 1 {
		t.Fatalf("unreachable: expect conservative keep, got %d recs", len(s.openOrders))
	}
	_, frozen, _ := s.ledgerSvc.Balance(1, "BTC")
	if !eqAmt(frozen, 1, "BTC") {
		t.Fatalf("frozen must be untouched: %v", frozen)
	}
}

// 场景4：补账——停机窗口内漏结算的成交被重放划转；重复重放不双付。
func TestCatchUpSettlesMissedFillOnce(t *testing.T) {
	fm := &fakeMatcher{}
	s := newRestoreServer(fm)
	seed(s, 1, 1000, 0) // 买方
	seed(s, 2, 0, 2)    // 卖方

	tv := matching.TradeView{
		ID: 1, Symbol: "BTC_USDT", Market: "spot",
		Price: fxPrice(100), Qty: fxQty(1),
		TakerID: 1, MakerID: 2, TakerSide: "buy",
		TakerOID: 601, MakerOID: 602, Time: 123,
	}
	fm.trades = []matching.TradeView{tv}

	// 第一次补账：买方付 100 USDT、卖方付 1 BTC
	s.CatchUpSettlement()
	a1, _, _ := s.ledgerSvc.Balance(1, "USDT")
	b1, _, _ := s.ledgerSvc.Balance(1, "BTC")
	a2, _, _ := s.ledgerSvc.Balance(2, "BTC")
	if !eqAmt(a1, 900, "USDT") || !eqAmt(b1, 1, "BTC") || !eqAmt(a2, 1, "BTC") {
		t.Fatalf("catchup settle wrong: buyer USDT=%v BTC=%v seller BTC=%v", a1, b1, a2)
	}
	if !s.settledRefs[fmt.Sprintf("spot:%s:t%d:m%d:p%v:q%v:b%d:s%d:%v",
		"BTC_USDT", tv.TakerOID, tv.MakerOID, tv.Price, tv.Qty, tv.TakerID, tv.MakerID, matching.Buy)] {
		t.Fatal("settledRefs not marked")
	}

	// 重放（模拟再次重启后同一批 /trades）：settledRefs 去重 + 账本指纹兜底，余额不变。
	s.CatchUpSettlement()
	a1b, _, _ := s.ledgerSvc.Balance(1, "USDT")
	b1b, _, _ := s.ledgerSvc.Balance(1, "BTC")
	a2b, _, _ := s.ledgerSvc.Balance(2, "BTC")
	if !eqAmt(a1b, 900, "USDT") || !eqAmt(b1b, 1, "BTC") || !eqAmt(a2b, 1, "BTC") {
		t.Fatalf("double-settle detected: buyer USDT=%v BTC=%v seller BTC=%v", a1b, b1b, a2b)
	}
	if !s.ledgerSvc.IsBalanced() {
		t.Fatal("ledger unbalanced after catchup")
	}
}

// 场景5：合约成交（market=futures）不参与 spot 补账。
func TestCatchUpSkipsFuturesTrades(t *testing.T) {
	fm := &fakeMatcher{}
	s := newRestoreServer(fm)
	seed(s, 1, 1000, 0)
	fm.trades = []matching.TradeView{{
		ID: 9, Symbol: "BTC_USDT", Market: "futures",
		Price: fxPrice(100), Qty: fxQty(1),
		TakerID: 1, MakerID: 2, TakerSide: "buy",
		TakerOID: 701, MakerOID: 702, Time: 456,
	}}
	s.CatchUpSettlement()
	a1, _, _ := s.ledgerSvc.Balance(1, "USDT")
	if !eqAmt(a1, 1000, "USDT") {
		t.Fatalf("futures trade must be skipped, buyer USDT=%v", a1)
	}
}

// 场景6：恢复后的 open 单成交 → 冻结正确递减（登记与账本联动闭环）。
func TestRestoredOrderSettleDecrementsFreeze(t *testing.T) {
	fm := &fakeMatcher{orders: map[int64]matching.OrderView{}}
	s := newRestoreServer(fm)
	seed(s, 1, 1000, 0)
	seed(s, 2, 0, 2)

	rec, err := s.reserveOnOpen(1, matching.Buy, fxPrice(100), fxQty(1), "BTC_USDT")
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	const oid = int64(801)
	rec.clientOID = "cli-9"
	fm.orders[oid] = matching.OrderView{ID: oid, UserID: 1, Status: matching.OrderPartial}
	recs := []OrderRecord{freezeRecToRecord(oid, rec, "cli-9")}
	s.RestoreOrders(recs)

	// 半数成交：50 USDT 结算、冻结递减至 50
	trade := matching.Trade{
		Price: fxPrice(100), Qty: fxQty(0.5),
		TakerID: 2, MakerID: 1, TakerSide: matching.Sell,
		TakerOID: 802, MakerOID: oid,
	}
	if err := s.settleFill("BTC_USDT", trade); err != nil {
		t.Fatalf("settleFill: %v", err)
	}
	_, frozen, _ := s.ledgerSvc.Balance(1, "USDT")
	if !eqAmt(frozen, 50, "USDT") {
		t.Fatalf("frozen after fill: %v", frozen)
	}
	if got := s.openOrders[oid].frozenQuote; got.Cmp(settlement.AssetAmountFromFloat(50, usdtDec)) != 0 {
		t.Fatalf("rec decrement wrong: %v", got)
	}
	if !s.ledgerSvc.IsBalanced() {
		t.Fatal("ledger unbalanced")
	}
}

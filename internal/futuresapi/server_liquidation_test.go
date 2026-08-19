package futuresapi

import (
	"testing"

	"github.com/coldlar/crypto-exchange/internal/futures"
	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/settlement"
	"go.uber.org/zap"
)

// testCfg 返回带默认精度配置的 Config（与 configs/config.yaml 默认一致），供测试构造 Server。
func testCfg() *config.Config {
	cfg := &config.Config{}
	cfg.Matching.DefaultPriceScale = 2
	cfg.Matching.DefaultQtyScale = 8
	return cfg
}

// fxPrice/fxQty 构造测试用的定点价格/数量（按默认 scale 对齐，与生产边界一致）。
func fxPrice(f float64) matching.Fixed { return matching.FixedFromFloat(f, 2) }
func fxQty(f float64) matching.Fixed   { return matching.FixedFromFloat(f, 8) }

// 演示：生产路径的 liquidationCloser——强平单作为市价单送入撮合引擎，
// 订单簿无流动性时由保险基金（SysLiquidationLoss）兜底成交，保证强平必定完成。
func TestLiquidationCloserBackstop(t *testing.T) {
	e := matching.NewEngine(nil, nil)
	e.Register("BTC_USDT_PERP")
	s := &Server{matcher: e, cfg: testCfg()}

	// 场景1：订单簿无流动性 -> 兜底在标记价 45000 全额成交。
	fill := s.liquidationCloser("BTC_USDT_PERP", 1001, futures.Long, 1, 45000)
	if !approx(fill.Filled, 1, 1e-9) {
		t.Fatalf("兜底应全额成交，实际 filled=%.6f", fill.Filled)
	}
	if !approx(fill.AvgPrice, 45000, 1e-9) {
		t.Fatalf("兜底成交均价应为标记价 45000，实际 %.2f", fill.AvgPrice)
	}

	// 场景2：订单簿有真实流动性 -> 在订单簿价成交（不被兜底价覆盖）。
	// 平多=卖，故播种一笔限价买@44000 作为对手流动性。
	if _, _ = e.MatchNow("BTC_USDT_PERP", &matching.Order{
		ID: 2, UserID: 2, Side: matching.Buy, Price: fxPrice(44000), Qty: fxQty(1), Time: 1,
	}, true); true {
	}
	fill2 := s.liquidationCloser("BTC_USDT_PERP", 1001, futures.Long, 1, 45000)
	if !approx(fill2.Filled, 1, 1e-9) {
		t.Fatalf("应全额成交，实际 filled=%.6f", fill2.Filled)
	}
	if !approx(fill2.AvgPrice, 44000, 1e-9) {
		t.Fatalf("应在订单簿价 44000 成交，实际 %.2f（被错误兜底价覆盖？）", fill2.AvgPrice)
	}

	// 场景3：部分流动性 -> 部分按订单簿价、剩余按兜底标记价，加权均价介于二者之间。
	// 再播种一笔限价买@43000（仅 0.5 张），平多卖单先吃 43000 的 0.5，剩余 0.5 兜底@45000。
	if _, _ = e.MatchNow("BTC_USDT_PERP", &matching.Order{
		ID: 3, UserID: 3, Side: matching.Buy, Price: fxPrice(43000), Qty: fxQty(0.5), Time: 2,
	}, true); true {
	}
	fill3 := s.liquidationCloser("BTC_USDT_PERP", 1001, futures.Long, 1, 45000)
	if !approx(fill3.Filled, 1, 1e-9) {
		t.Fatalf("部分流动性应仍全额成交（兜底补剩余），实际 filled=%.6f", fill3.Filled)
	}
	// 加权均价 = (43000*0.5 + 45000*0.5)/1 = 44000
	if !approx(fill3.AvgPrice, 44000, 1e-6) {
		t.Fatalf("加权均价应=44000，实际 %.2f", fill3.AvgPrice)
	}
	// 兜底成交的 maker 应为保险基金账户（不污染真实用户）。
	_ = ledger.SysLiquidationLoss // 编译期确认常量存在
}

func approx(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

// eqAmt 将账本 AssetAmount 与人类单位字面量按资产小数位精确比较（无 epsilon）。
func eqAmt(a settlement.AssetAmount, human float64, asset string) bool {
	return a.Cmp(settlement.AssetAmountFromFloat(human, settlement.AssetDecimalsByName(asset))) == 0
}

// TestOnLiquidationFullBatchAtomic 验证整仓强平的资金动作经 ledger.Batch 原子执行：
// 释放冻结保证金 + 没收入保险基金后，账本保持全局平衡，且保证金完整转入 SysInsurance。
// 回归：确认 F3 多腿原子化改造未改变净额语义（此前若 Debit 成功、Credit 失败会失衡）。
func TestOnLiquidationFullBatchAtomic(t *testing.T) {
	l := ledger.New()
	// 链上充值保持创世平衡；冻结 1000 USDT 作为被强平用户的保证金。
	_ = l.ReceiveOnChain(1, "USDT", settlement.AssetAmountFromFloat(100000, settlement.AssetDecimalsByName("USDT")), "seed:1:USDT")
	if err := l.Freeze(1, "USDT", settlement.AssetAmountFromFloat(1000, settlement.AssetDecimalsByName("USDT"))); err != nil {
		t.Fatalf("freeze failed: %v", err)
	}
	s := &Server{ledgerSvc: l, log: zap.NewNop()}

	ev := futures.LiquidationEvent{
		UserID: 1, Symbol: "BTC_USDT", Mode: futures.Isolated,
		Margin: 1000, Realized: 0, Partial: false,
	}
	s.onLiquidation(ev)

	// 用户：冻结应清零，可用应为 99000（100000 - 1000 没收）。
	if _, f, _ := l.Balance(1, "USDT"); !eqAmt(f, 0, "USDT") {
		t.Fatalf("user frozen should be 0, got %v", f)
	}
	if a, _, _ := l.Balance(1, "USDT"); !eqAmt(a, 99000, "USDT") {
		t.Fatalf("user available should be 99000, got %v", a)
	}
	// 保险基金应收到 1000 没收保证金。
	if ins, _, _ := l.Balance(ledger.SysInsurance, "USDT"); !eqAmt(ins, 1000, "USDT") {
		t.Fatalf("SysInsurance should be 1000, got %v", ins)
	}
	if !l.IsBalanced() {
		t.Fatalf("ledger unbalanced after full liquidation")
	}
}

package futuresapi

import (
	"testing"

	"github.com/coldlar/crypto-exchange/internal/futures"
	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/matching"
)

// 演示：生产路径的 liquidationCloser——强平单作为市价单送入撮合引擎，
// 订单簿无流动性时由保险基金（SysLiquidationLoss）兜底成交，保证强平必定完成。
func TestLiquidationCloserBackstop(t *testing.T) {
	e := matching.NewEngine(nil, nil)
	e.Register("BTC_USDT_PERP")
	s := &Server{matcher: e}

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
		ID: 2, UserID: 2, Side: matching.Buy, Price: 44000, Qty: 1, Time: 1,
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
		ID: 3, UserID: 3, Side: matching.Buy, Price: 43000, Qty: 0.5, Time: 2,
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

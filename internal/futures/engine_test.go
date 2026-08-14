package futures

import (
	"testing"
)

// 演示：开 10x 多仓，价格下跌触发强平。
func TestLiquidationLong(t *testing.T) {
	sym := "BTC_USDT_PERP"
	liq := NewLiquidator(nil)
	liq.Register(sym)
	book, _ := liq.Book(sym)

	// 用户 1001 以 50000 开多 1 BTC，10x，锁定保证金 = 50000/10 = 5000
	entry := 50000.0
	lev := 10.0
	margin := entry / lev
	book.Open(1001, sym, Long, 1, entry, margin, lev, 0)

	p, _ := book.pos[1001]
	// 强平价应约为 45226（50000*0.9/0.995）；1 BTC@50000 名义价值 50000 属最低档 0.5%
	wantLiq := entry * (1 - 1/lev) / (1 - MaintenanceMarginRate(entry))
	if approx(p.LiqPrice(), wantLiq, 1e-6) {
		t.Logf("强平价 = %.2f（预期 %.2f）", p.LiqPrice(), wantLiq)
	} else {
		t.Fatalf("强平价计算错误: got %.2f want %.2f", p.LiqPrice(), wantLiq)
	}

	// 价格高于强平价：不触发
	evs := liq.UpdateMarkPrice(sym, 46000)
	if len(evs) != 0 {
		t.Fatalf("46000 不应强平，却触发了 %d 次", len(evs))
	}

	// 价格跌到 45200（< 强平价）：触发强平
	evs = liq.UpdateMarkPrice(sym, 45200)
	if len(evs) != 1 {
		t.Fatalf("45200 应触发 1 次强平，实际 %d", len(evs))
	}
	ev := evs[0]
	if ev.UserID != 1001 || ev.Side != Long || ev.Size != 1 {
		t.Fatalf("强平事件字段错误: %+v", ev)
	}
	t.Logf("强平事件: 用户=%d 方向=多 数量=%.4f 强平价=%.2f 手续费=%.2f",
		ev.UserID, ev.Size, ev.LiqPrice, ev.Fee)

	// 仓位应已清空
	if _, ok := book.pos[1001]; ok {
		t.Fatalf("强平后仓位未清空")
	}
}

// 演示：开 20x 空仓，价格上涨触发强平。
func TestLiquidationShort(t *testing.T) {
	sym := "BTC_USDT_PERP"
	liq := NewLiquidator(nil)
	liq.Register(sym)
	book, _ := liq.Book(sym)

	entry := 50000.0
	lev := 20.0
	margin := entry / lev
	book.Open(2002, sym, Short, 1, entry, margin, lev, 0)

	p, _ := book.pos[2002]
	// 空仓强平价 = 50000*(1+0.05)/(1+0.005) ≈ 52487；最低档 0.5%
	wantLiq := entry * (1 + 1/lev) / (1 + MaintenanceMarginRate(entry))
	if !approx(p.LiqPrice(), wantLiq, 1e-6) {
		t.Fatalf("空仓强平价错误: got %.2f want %.2f", p.LiqPrice(), wantLiq)
	}

	// 涨到 52500（> 强平价）：触发
	evs := liq.UpdateMarkPrice(sym, 52500)
	if len(evs) != 1 {
		t.Fatalf("52500 应触发 1 次强平，实际 %d", len(evs))
	}
	t.Logf("空仓强平成功: 用户=%d 强平价=%.2f", evs[0].UserID, evs[0].LiqPrice)
}

// 演示：多个持仓同时扫描，仅爆仓者被强平。
func TestMultiPositionScan(t *testing.T) {
	sym := "BTC_USDT_PERP"
	liq := NewLiquidator(nil)
	liq.Register(sym)
	book, _ := liq.Book(sym)

	// 用户A：10x 多，价格跌穿强平价 -> 爆
	book.Open(1, sym, Long, 1, 50000, 5000, 10, 0)
	// 用户B：5x 多（强平价约 40201），跌到 45200 不爆
	book.Open(2, sym, Long, 1, 50000, 10000, 5, 0)

	evs := liq.UpdateMarkPrice(sym, 45200)
	if len(evs) != 1 || evs[0].UserID != 1 {
		t.Fatalf("应只强平用户1，实际: %+v", evs)
	}
	if _, ok := book.pos[2]; !ok {
		t.Fatalf("用户2 不应被强平")
	}
	t.Logf("选择性强平正确：仅用户1被强平")
}

func approx(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

// 演示：独立强平扫描（与撮合成交解耦）。
// liqScanLoop 通过周期性调用 UpdateMarkPrice 触发强平，本测试等价验证：
// 即便没有任何成交流，只要标记价击穿强平价即可触发，且重复扫描幂等（不产生重复事件）。
func TestLiquidationScanIdempotent(t *testing.T) {
	sym := "BTC_USDT_PERP"
	liq := NewLiquidator(nil)
	liq.Register(sym)
	book, _ := liq.Book(sym)

	// 用户 3003 以 50000 开多 1 BTC，10x，保证金 5000，强平价约 45226
	entry := 50000.0
	lev := 10.0
	margin := entry / lev
	book.Open(3003, sym, Long, 1, entry, margin, lev, 0)

	// 模拟独立扫描首次：标记价 = 指数价 45000（< 强平价）-> 触发强平
	evs := liq.UpdateMarkPrice(sym, 45000)
	if len(evs) != 1 {
		t.Fatalf("首次扫描应触发 1 次强平，实际 %d", len(evs))
	}
	if evs[0].UserID != 3003 {
		t.Fatalf("强平用户错误: %+v", evs[0])
	}
	// 仓位已被清空（整仓强平）
	if _, ok := book.pos[3003]; ok {
		t.Fatalf("强平后仓位未清空")
	}

	// 模拟独立扫描第二次：同一标记价重复扫描，不应再产生事件（幂等）
	evs2 := liq.UpdateMarkPrice(sym, 45000)
	if len(evs2) != 0 {
		t.Fatalf("二次扫描不应再触发强平，实际 %d", len(evs2))
	}

	// 价格回升也无意触发（仓位已不存在）
	evs3 := liq.UpdateMarkPrice(sym, 60000)
	if len(evs3) != 0 {
		t.Fatalf("空账户扫描不应触发，实际 %d", len(evs3))
	}
	t.Logf("独立扫描触发与幂等验证通过")
}

// 演示：全仓多仓，共享保证金随价格下跌被击穿 -> 整户强平。
func TestCrossLiquidationLong(t *testing.T) {
	sym := "BTC_USDT_PERP"
	liq := NewLiquidator(nil)
	liq.Register(sym)

	// 用户 3001 以 50000 开多 1 BTC，划入共享保证金 5000（等效 10x）。
	liq.OpenCross(sym, 3001, Long, 1, 50000, 5000, 10, 0)
	if liq.ModeOf(sym, 3001) != Cross {
		t.Fatalf("应识别为全仓账户")
	}
	// 全仓强平价应约等于逐仓同参数值：50000*(1-0.1)/(1-0.005) ≈ 45226
	acc := liq.AllPositions(sym)
	if len(acc) != 1 {
		t.Fatalf("应有 1 个全仓腿，实际 %d", len(acc))
	}

	// 46000：不触发
	if evs := liq.UpdateMarkPrice(sym, 46000); len(evs) != 0 {
		t.Fatalf("46000 不应强平，实际 %d", len(evs))
	}
	// 45200：击穿，整户强平（共享保证金没收）
	evs := liq.UpdateMarkPrice(sym, 45200)
	if len(evs) != 1 || evs[0].UserID != 3001 || evs[0].Side != Long {
		t.Fatalf("全仓强平事件错误: %+v", evs)
	}
	if evs[0].Margin != 5000 {
		t.Fatalf("全仓应没收共享保证金 5000，实际 %.2f", evs[0].Margin)
	}
	// 强平后账户应被清除
	if liq.ModeOf(sym, 3001) != Isolated {
		t.Fatalf("强平后全仓账户应已移除")
	}
}

// 演示：全仓多空双边对冲，价格剧烈波动不会触发强平（盈亏内部抵消）。
// 对比：同等保证金的逐仓多仓在 40000 会爆，全仓对冲户不会。
func TestCrossHedgedResilience(t *testing.T) {
	sym := "BTC_USDT_PERP"
	liq := NewLiquidator(nil)
	liq.Register(sym)

	// 划入 6000 共享保证金，开多 1@50000，再加开空 1@50000（不再追加保证金）。
	liq.OpenCross(sym, 4004, Long, 1, 50000, 6000, 10, 0)
	liq.OpenCross(sym, 4004, Short, 1, 50000, 0, 10, 0)

	// 暴跌到 40000：多 -10000 / 空 +10000，净值不变，账户不应强平。
	evs := liq.UpdateMarkPrice(sym, 40000)
	if len(evs) != 0 {
		t.Fatalf("对冲全仓在 40000 不应强平，实际 %d", len(evs))
	}
	if liq.ModeOf(sym, 4004) != Cross {
		t.Fatalf("对冲账户应仍存在")
	}
}

// 演示：同交易对下逐仓与全仓账户并存，强平互不影响。
func TestCrossAndIsolatedCoexist(t *testing.T) {
	sym := "BTC_USDT_PERP"
	liq := NewLiquidator(nil)
	liq.Register(sym)
	book, _ := liq.Book(sym)

	// 逐仓用户 5001：10x 多，强平价约 45226
	book.Open(5001, sym, Long, 1, 50000, 5000, 10, 0)
	// 全仓用户 5002：划入 5000 开多 1@50000
	liq.OpenCross(sym, 5002, Long, 1, 50000, 5000, 10, 0)

	// 跌到 45200：两个都爆，但各自按自身模式处理。
	evs := liq.UpdateMarkPrice(sym, 45200)
	if len(evs) != 2 {
		t.Fatalf("应强平 2 个账户（逐仓+全仓），实际 %d: %+v", len(evs), evs)
	}
	// 逐仓账户已清，全仓账户已清
	if _, ok := book.pos[5001]; ok {
		t.Fatalf("逐仓用户 5001 应已强平")
	}
	if liq.ModeOf(sym, 5002) != Isolated {
		t.Fatalf("全仓用户 5002 应已强平")
	}
}

// 演示：阶梯维持保证金率使部分强平能"恢复"，而非整仓强平。
// 大盘高挡位仓位（名义价值 200万 → 2.0% 维持率）在略低于阈值时被强平，
// 平掉一半使名义价值降档（<100万 → 1.0% 维持率），维持需求跳降，
// 保证金率随即恢复到安全线，保留剩余仓位继续监控。
func TestPartialLiquidation(t *testing.T) {
	sym := "BTC_USDT_PERP"
	liq := NewLiquidator(nil)
	liq.SetPartialRatio(0.5)
	liq.Register(sym)
	book, _ := liq.Book(sym)

	// 40 BTC @ 50000，名义价值 200 万（高挡位 2%），保证金 60000。
	// 在 49475 时权益 39000 < 维持 39580，略低于强平价阈值，触发部分强平。
	book.Open(1001, sym, Long, 40, 50000, 60000, 10, 0)
	evs := liq.UpdateMarkPrice(sym, 49475)
	if len(evs) != 1 || !evs[0].Partial {
		t.Fatalf("应触发部分强平，实际: %+v", evs)
	}
	p, ok := book.pos[1001]
	if !ok {
		t.Fatalf("部分强平后仓位应保留")
	}
	if !approx(p.Size, 20, 1e-9) {
		t.Fatalf("平掉一半后剩余应为 20，实际 %.4f", p.Size)
	}
	if !approx(evs[0].RemainingSize, 20, 1e-9) {
		t.Fatalf("事件剩余应为 20，实际 %.4f", evs[0].RemainingSize)
	}
	if !approx(evs[0].Margin, 30000, 1e-6) {
		t.Fatalf("释放保证金应为 30000（一半），实际 %.2f", evs[0].Margin)
	}
	// 恢复后保证金率应 >= 安全线
	if p.MarginRatio(49475) < SafeMarginRatio {
		t.Fatalf("部分强平后保证金率应 >= %.2f，实际 %.4f", SafeMarginRatio, p.MarginRatio(49475))
	}
	t.Logf("部分强平恢复：平掉 %.4f，剩余 %.4f，恢复后保证金率 %.4f",
		evs[0].Size, evs[0].RemainingSize, p.MarginRatio(49475))
}

// 演示：阶梯维持保证金率随名义价值分档。
func TestTieredMaintenanceRate(t *testing.T) {
	cases := []struct {
		notional float64
		want     float64
	}{
		{50_000, 0.005},
		{100_000, 0.005},
		{500_000, 0.010},
		{1_000_000, 0.010},
		{3_000_000, 0.020},
		{5_000_000, 0.020},
		{10_000_000, 0.025},
		{100_000_000, 0.025},
	}
	for _, c := range cases {
		if got := MaintenanceMarginRate(c.notional); !approx(got, c.want, 1e-9) {
			t.Fatalf("名义价值 %.0f 维持率错误: got %.4f want %.4f", c.notional, got, c.want)
		}
	}
}

// 演示：穿仓且保险基金不足时触发 ADL，由盈利的对手空头按缺口部分减仓吸收穿仓亏损。
func TestADLOnBankruptcy(t *testing.T) {
	sym := "BTC_USDT_PERP"
	liq := NewLiquidator(nil)
	liq.Register(sym)
	book, _ := liq.Book(sym)
	// 保险基金为 0 -> 任何穿仓都触发 ADL
	liq.SetInsuranceProvider(func() float64 { return 0 })

	// 被强平方：10x 多 1@50000（mark 跌穿破产价 45000 即穿仓）
	book.Open(7001, sym, Long, 1, 50000, 5000, 10, 0)
	// 对手盈利方：10x 空 1@50000（mark 下跌则盈利 10000）
	book.Open(7002, sym, Short, 1, 50000, 5000, 10, 0)

	// 深跌到 40000：多单穿仓亏损 = (45000-40000)*1 = 5000
	evs := liq.UpdateMarkPrice(sym, 40000)
	if len(evs) != 1 {
		t.Fatalf("应强平 1 个（多头），实际 %d: %+v", len(evs), evs)
	}
	ev := evs[0]
	if !approx(ev.Deficit, 5000, 1e-6) {
		t.Fatalf("穿仓亏损应为 5000，实际 %.2f", ev.Deficit)
	}
	if !approx(ev.InsuranceCovered, 0, 1e-6) || !approx(ev.ADLCovered, 5000, 1e-6) ||
		!approx(ev.Socialized, 0, 1e-6) || !approx(ev.Residual, 0, 1e-6) {
		t.Fatalf("吸收分层错误: %+v", ev)
	}
	// 多头整仓强平并清空
	if _, ok := book.pos[7001]; ok {
		t.Fatalf("多头 7001 应已强平")
	}
	// ADL 应减掉盈利空头 7002 刚好覆盖缺口的一半（盈利 10000，仅取 5000）
	adls := liq.RecentADL()
	if len(adls) != 1 {
		t.Fatalf("应触发 1 次 ADL，实际 %d: %+v", len(adls), adls)
	}
	if adls[0].UserID != 7002 {
		t.Fatalf("ADL 应减掉空头 7002，实际 %+v", adls[0])
	}
	if !approx(adls[0].ReducedSize, 0.5, 1e-9) {
		t.Fatalf("ADL 应部分减仓 0.5（取 5000/10000 盈利），实际 %.4f", adls[0].ReducedSize)
	}
	if !approx(adls[0].ProfitCovered, 5000, 1e-6) {
		t.Fatalf("ADL 吸收盈利应为 5000，实际 %.2f", adls[0].ProfitCovered)
	}
	p, ok := book.pos[7002]
	if !ok || !approx(p.Size, 0.5, 1e-9) {
		t.Fatalf("空头 7002 应保留 0.5，实际 %+v", p)
	}
	t.Logf("ADL：空头 7002 减仓 %.4f，吸收盈利 %.2f（剩余 0.5 保留）", adls[0].ReducedSize, adls[0].ProfitCovered)
}

// 演示：保险基金余额充足时，穿仓亏损由保险基金全额吸收，不触发 ADL/社会化。
func TestDeficitWaterfallInsuranceCovered(t *testing.T) {
	sym := "BTC_USDT_PERP"
	liq := NewLiquidator(nil)
	liq.Register(sym)
	book, _ := liq.Book(sym)
	liq.SetInsuranceProvider(func() float64 { return 100000 }) // 保险基金充足

	book.Open(7001, sym, Long, 1, 50000, 5000, 10, 0)
	book.Open(7002, sym, Short, 1, 50000, 5000, 10, 0) // 盈利对手，但保险基金足够故不应被减

	evs := liq.UpdateMarkPrice(sym, 40000)
	if len(evs) != 1 {
		t.Fatalf("应强平 1 个，实际 %d", len(evs))
	}
	ev := evs[0]
	if !approx(ev.Deficit, 5000, 1e-6) {
		t.Fatalf("穿仓亏损应为 5000，实际 %.2f", ev.Deficit)
	}
	if !approx(ev.InsuranceCovered, 5000, 1e-6) {
		t.Fatalf("保险基金应吸收 5000，实际 %.2f", ev.InsuranceCovered)
	}
	if !approx(ev.ADLCovered, 0, 1e-6) || !approx(ev.Socialized, 0, 1e-6) || !approx(ev.Residual, 0, 1e-6) {
		t.Fatalf("不应触发 ADL/社会化，分层: %+v", ev)
	}
	if len(liq.RecentADL()) != 0 {
		t.Fatalf("保险基金充足时不应触发 ADL")
	}
	if _, ok := book.pos[7002]; !ok || !approx(book.pos[7002].Size, 1, 1e-9) {
		t.Fatalf("盈利空头 7002 不应被减仓")
	}
	t.Logf("保险基金全额吸收穿仓：InsuranceCovered=%.2f", ev.InsuranceCovered)
}

// 演示：穿仓亏损超过单一 ADL 对手盈利时，剩余由社会化分摊（全体盈利方按比例）吸收。
func TestSocializedLoss(t *testing.T) {
	sym := "BTC_USDT_PERP"
	liq := NewLiquidator(nil)
	liq.Register(sym)
	book, _ := liq.Book(sym)
	liq.SetInsuranceProvider(func() float64 { return 0 }) // 保险基金为 0

	// 被强平方：10x 多 1@50000，穿仓亏损 5000（mark 40000 < 破产价 45000）
	book.Open(7001, sym, Long, 1, 50000, 5000, 10, 0)
	// ADL 对手（空头）：@42000，mark 40000 盈利 2000（仅能覆盖部分缺口）
	book.Open(7002, sym, Short, 1, 42000, 5000, 10, 0)
	// 另一盈利方（多头）：@30000，mark 40000 盈利 10000（用于社会化分摊）
	book.Open(7003, sym, Long, 1, 30000, 5000, 10, 0)

	evs := liq.UpdateMarkPrice(sym, 40000)
	if len(evs) != 1 {
		t.Fatalf("应强平 1 个，实际 %d", len(evs))
	}
	ev := evs[0]
	if !approx(ev.Deficit, 5000, 1e-6) {
		t.Fatalf("穿仓亏损应为 5000，实际 %.2f", ev.Deficit)
	}
	// 分层：保险 0 + ADL 2000 + 社会化 3000 + 残差 0
	if !approx(ev.InsuranceCovered, 0, 1e-6) || !approx(ev.ADLCovered, 2000, 1e-6) ||
		!approx(ev.Socialized, 3000, 1e-6) || !approx(ev.Residual, 0, 1e-6) {
		t.Fatalf("吸收分层错误: %+v", ev)
	}
	// ADL 减掉空头 7002 整腿（盈利恰好 2000）
	adls := liq.RecentADL()
	if len(adls) != 1 || adls[0].UserID != 7002 || !approx(adls[0].ProfitCovered, 2000, 1e-6) {
		t.Fatalf("ADL 应减掉 7002 并吸收 2000，实际 %+v", adls)
	}
	if _, ok := book.pos[7002]; ok {
		t.Fatalf("空头 7002 应已被 ADL 整腿减仓")
	}
	// 社会化分摊：多头 7003 按份额承担 3000（盈利 10000 的 30%），保留 0.7
	socs := liq.RecentSocialized()
	if len(socs) != 1 || socs[0].UserID != 7003 || !approx(socs[0].Share, 3000, 1e-6) {
		t.Fatalf("社会化应减 7003 并承担 3000，实际 %+v", socs)
	}
	p, ok := book.pos[7003]
	if !ok || !approx(p.Size, 0.7, 1e-9) {
		t.Fatalf("社会化后 7003 应保留 0.7，实际 %+v", p)
	}
	t.Logf("穿仓吸收瀑布：保险 0 + ADL 2000 + 社会化 3000 = 5000，残差 0")
}

// 演示：强平单真正送入撮合引擎（经由注入的 LiquidationCloser）。
// 验证三点：
//  1. 强平触发时调用了 closer，且传入正确的 symbol/userID/side/计划平仓张数；
//  2. 强平引擎用 closer 返回的「真实成交均价」回填持仓，而非固定标记价——
//     因此实现盈亏与默认（按标记价）路径不同，证明走的是撮合引擎成交价；
//  3. 仓位被清空（整仓强平）。
func TestLiquidationThroughMatchingEngine(t *testing.T) {
	sym := "BTC_USDT_PERP"
	liq := NewLiquidator(nil)
	liq.Register(sym)
	book, _ := liq.Book(sym)

	// 用户 9001 以 50000 开多 1 BTC，10x，保证金 5000，强平价约 45226。
	entry := 50000.0
	lev := 10.0
	margin := entry / lev
	book.Open(9001, sym, Long, 1, entry, margin, lev, 0)

	// 注入一个模拟撮合引擎的 closer：返回成交均价 44000（劣于标记价 45000，
	// 模拟强平单在订单簿中以更低价格成交），成交张数 = 计划平仓张数。
	var gotSym string
	var gotUID int64
	var gotSide PosSide
	var gotQty float64
	liq.SetLiquidationCloser(func(s string, uid int64, side PosSide, qty, mark float64) LiquidationFill {
		gotSym, gotUID, gotSide, gotQty = s, uid, side, qty
		return LiquidationFill{Filled: qty, AvgPrice: 44000, Trades: 1}
	})

	// 标记价 45000（< 强平价）→ 触发强平，closer 返回成交均价 44000。
	evs := liq.UpdateMarkPrice(sym, 45000)
	if len(evs) != 1 {
		t.Fatalf("应触发 1 次强平，实际 %d", len(evs))
	}
	ev := evs[0]

	// 1) closer 被调用且参数正确。
	if gotSym != sym || gotUID != 9001 || gotSide != Long {
		t.Fatalf("closer 参数错误: sym=%s uid=%d side=%v qty=%.4f", gotSym, gotUID, gotSide, gotQty)
	}
	if !approx(gotQty, 1, 1e-9) {
		t.Fatalf("closer 计划平仓张数应为 1，实际 %.4f", gotQty)
	}

	// 2) 实现盈亏按成交均价 44000 计算：多仓 realized = (44000-50000)*1 = -6000。
	//    若仍按默认标记价 45000，realized 应为 -5000；此处必须为 -6000，证明走撮合引擎价。
	if !approx(ev.Realized, -6000, 1e-6) {
		t.Fatalf("强平实现盈亏应按撮合引擎成交均价 44000 计（-6000），实际 %.2f", ev.Realized)
	}
	// 穿仓亏损 = (破产价 45000 - 成交价 44000)*1 = 1000（成交劣于破产价）。
	if !approx(ev.Deficit, 1000, 1e-6) {
		t.Fatalf("穿仓亏损应为 1000，实际 %.2f", ev.Deficit)
	}

	// 3) 整仓强平，仓位清空。
	if _, ok := book.pos[9001]; ok {
		t.Fatalf("强平后仓位未清空")
	}
	t.Logf("强平经撮合引擎成交：成交均价=44000 实现盈亏=%.2f 穿仓=%.2f", ev.Realized, ev.Deficit)
}

// 演示：撮合引擎每轮只提供部分流动性（closer 返回 Filled < qty）时，强平引擎据每次
// 部分成交回填，并持续清算剩余持仓直至整仓清空（不卡死、不残留）。上层 liquidationCloser
// 由保险基金兜底保证「一轮即全平」，本测试验证引擎侧对「部分成交」的健壮闭合。
func TestLiquidationPartialFillFromEngine(t *testing.T) {
	sym := "BTC_USDT_PERP"
	liq := NewLiquidator(nil)
	liq.Register(sym)
	book, _ := liq.Book(sym)
	book.Open(9101, sym, Long, 2, 50000, 10000, 10, 0)

	// closer 每轮只成交一半（@44500），模拟市价单分批吃流动性。
	liq.SetLiquidationCloser(func(s string, uid int64, side PosSide, qty, mark float64) LiquidationFill {
		return LiquidationFill{Filled: qty / 2, AvgPrice: 44500, Trades: 1}
	})

	evs := liq.UpdateMarkPrice(sym, 45000)
	if len(evs) != 1 {
		t.Fatalf("应触发 1 次强平，实际 %d", len(evs))
	}
	// 强平引擎持续清算直至整仓清空：2 BTC 全部 @44500 平仓，realized ≈ (44500-50000)*2 = -11000。
	// （分批成交的几何尾差约 1e-5，故用 1e-3 容差。）
	if !approx(evs[0].Realized, -11000, 1e-3) {
		t.Fatalf("整仓清算实现盈亏应约为 -11000，实际 %.2f", evs[0].Realized)
	}
	// 整仓强平，仓位清空（未被部分成交逻辑卡住残留）。
	if _, ok := book.pos[9101]; ok {
		t.Fatalf("部分成交场景下强平后仓位未清空（可能卡死/残留）")
	}
	t.Logf("强平经撮合引擎分批成交并完整清算：实现盈亏 %.2f，仓位已清空", evs[0].Realized)
}

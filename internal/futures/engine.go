package futures

import (
	"sort"
	"sync"
	"time"
)

// LiquidationFill 是撮合引擎对强平单的成交回报（强平单送入撮合引擎后的真实成交）。
type LiquidationFill struct {
	Filled   float64 // 实际成交张数
	AvgPrice float64 // 成交均价
	Trades   int     // 成交笔数
}

// LiquidationCloser 强平平仓执行器：把强平单作为市价单送入撮合引擎成交，返回真实成交
// （含成交均价）。未注入时默认按标记价直接减仓（兼容单测与无撮合引擎的降级场景）。
// 参数：symbol 交易对、userID 被强平用户、side 被强平持仓方向（用于决定下单买卖方向）、
// qty 计划平仓张数、mark 当前标记价（订单簿无流动性时由保险基金兜底成交的参考价）。
type LiquidationCloser func(symbol string, userID int64, side PosSide, qty, mark float64) LiquidationFill

// LiquidationEvent 强平事件，用于审计、前端展示与复盘。
type LiquidationEvent struct {
	UserID        int64
	Symbol        string
	Side          PosSide
	Mode          MarginMode // 逐仓 / 全仓（上层据此决定钱包处理）
	Size          float64
	EntryPrice    float64
	LiqPrice      float64 // 触发强平时的标记价格
	Fee           float64 // 强平手续费
	Margin        float64 // 本次强平涉及的保证金：全平=全部没收；部分平=释放比例
	Realized      float64 // 本次平仓实现盈亏（部分平用于结算，可正可负）
	Partial       bool    // 是否为部分强平（false=整仓/整户强平）
	RemainingSize float64 // 部分平后剩余持仓（整仓强平时为 0）
	Deficit       float64 // 本次（含多次部分平）累计穿仓亏损
	// 穿仓吸收瀑布的分层结果（Deficit = InsuranceCovered + ADLCovered + Socialized + Residual）：
	InsuranceCovered float64 // 保险基金直接吸收的穿仓亏损
	ADLCovered       float64 // ADL（自动减仓对手盈利方）吸收的穿仓亏损
	Socialized       float64 // 社会化分摊（全体盈利方按比例）吸收的穿仓亏损
	Residual         float64 // 仍未被覆盖的残差（保险基金负余额承载，即系统净损失）
	Time             int64
}

// ADLEvent 自动减仓（Auto-Deleveraging）事件：穿仓且保险基金不足时，
// 按"盈利百分比高、杠杆高"优先级挑选对手方向仓位，以标记价（不滑点）减仓，
// 其盈利用于吸收穿仓亏损。被减仓者不承受滑点，按标记价实现盈利。
type ADLEvent struct {
	UserID        int64
	Symbol        string
	Side          PosSide
	ReducedSize   float64 // 被减仓张数
	Price         float64 // 减仓成交价（标记价，不滑点）
	ProfitCovered float64 // 本次减仓为穿仓吸收的盈利额
	Time          int64
}

// SocializedLossEvent 社会化分摊事件：ADL 后仍不足以覆盖穿仓亏损时，
// 由同交易对所有盈利持仓按未实现盈利比例分摊剩余亏损——被减仓者以标记价部分减仓，
// 其实现盈利转为穿仓吸收（Share 即该户本次吸收的亏损额）。这是穿仓吸收瀑布的最后一道防线。
type SocializedLossEvent struct {
	UserID      int64
	Symbol      string
	Side        PosSide
	ReducedSize float64 // 被减仓张数（按份额比例部分减仓）
	Price       float64 // 减仓成交价（标记价）
	Share       float64 // 该户本次分摊吸收的亏损额（= 其被减仓实现的盈利额）
	Time        int64
}

// Liquidator 强平引擎：管理各交易对的持仓账本，并在标记价格变化时检测并触发强平。
//
// 同时支持两种保证金模式：
//   - 逐仓（isolated）：每用户每交易对一本 PositionBook，单仓独立保证金，爆仓不影响其他仓位。
//   - 全仓（cross）：每用户每交易对一本 crossBook，多/空共享保证金池，整户强平。
//
// 生产级强平分两步（骨架简化为一步，但保留接口）：
//  1. 部分强平（维持保证金率恢复）：下市价单吃流动性，直到保证金率回到安全线。
//  2. 破产价穿仓：若清算价劣于破产价，差额由保险基金吸收；基金不足则触发 ADL（自动减仓）。
type Liquidator struct {
	mu    sync.RWMutex
	books map[string]*PositionBook
	cross map[string]*crossBook
	onLiq func(ev LiquidationEvent)
	onADL func(ev ADLEvent)

	// closer 强平平仓执行器：把强平单送入撮合引擎成交，返回真实成交均价。
	// 未注入时使用默认实现（按标记价直接减仓），保证无撮合引擎时行为不变。
	closer LiquidationCloser

	// 穿仓吸收瀑布的回调（由上层注入钱包/账本操作）：
	//   - onDeficitPay：保险基金支付穿仓亏损（Debit SysInsurance，Credit SysLiquidationLoss）
	//   - onSocialize：社会化分摊，各盈利持仓按份额 Debit 可用、Credit 保险基金
	onDeficitPay func(deficit float64)
	onSocialize  func(shares []SocializedLossEvent)

	partialRatio float64        // 部分强平比例（1.0=整仓/整户）
	insurance    func() float64 // 保险基金余额查询（由上层注入钱包余额）

	events []LiquidationEvent // 最近强平记录（演示用，生产应落库/发 Kafka）
	adls   []ADLEvent         // 最近 ADL 记录
	socs   []SocializedLossEvent // 最近社会化分摊记录
}

// crossBook 单交易对的全仓账户集合（线程安全）。
type crossBook struct {
	mu   sync.RWMutex
	accs map[int64]*CrossAccount
}

// NewLiquidator 创建强平引擎。onLiq 在每次强平时回调（用于下单平仓、资金结算）。
func NewLiquidator(onLiq func(ev LiquidationEvent)) *Liquidator {
	return &Liquidator{
		books:        make(map[string]*PositionBook),
		cross:        make(map[string]*crossBook),
		onLiq:        onLiq,
		partialRatio: DefaultPartialLiqRatio,
		insurance:    func() float64 { return 0 },
		onDeficitPay: func(float64) {},
		onSocialize:  func([]SocializedLossEvent) {},
		// 默认 closer：无撮合引擎时按标记价直接减仓（与历史行为一致，兼容单测/降级）。
		closer: func(symbol string, userID int64, side PosSide, qty, mark float64) LiquidationFill {
			return LiquidationFill{Filled: qty, AvgPrice: mark, Trades: 1}
		},
	}
}

// SetLiquidationCloser 注入强平平仓执行器（把强平单送入撮合引擎成交）。
// 上层（futuresapi）注入后，强平将经撮合引擎真实成交并据成交均价回填持仓；
// 未注入则使用默认实现（按标记价直接减仓）。
func (l *Liquidator) SetLiquidationCloser(fn LiquidationCloser) {
	if fn != nil {
		l.closer = fn
	}
}

// SetPartialRatio 设置部分强平比例（0<r<=1）。r=1 为整仓/整户强平（默认）。
func (l *Liquidator) SetPartialRatio(r float64) {
	if r > 0 && r <= 1 {
		l.partialRatio = r
	}
}

// SetADLCallback 设置 ADL 事件回调（用于资金闭环：被减仓者盈利转入保险基金）。
func (l *Liquidator) SetADLCallback(cb func(ev ADLEvent)) {
	l.onADL = cb
}

// SetInsuranceProvider 注入保险基金余额查询（用于判断穿仓时是否需要 ADL）。
func (l *Liquidator) SetInsuranceProvider(fn func() float64) {
	if fn != nil {
		l.insurance = fn
	}
}

// SetDeficitPayer 注入穿仓亏损支付回调（保险基金支付，记穿仓损失归集账户）。
func (l *Liquidator) SetDeficitPayer(fn func(deficit float64)) {
	if fn != nil {
		l.onDeficitPay = fn
	}
}

// SetSocializeCallback 注入社会化分摊回调（盈利持仓按份额承担穿仓亏损）。
func (l *Liquidator) SetSocializeCallback(fn func(shares []SocializedLossEvent)) {
	if fn != nil {
		l.onSocialize = fn
	}
}

// Register 注册交易对的逐仓账本与全仓账本。
func (l *Liquidator) Register(symbol string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.books[symbol]; !ok {
		l.books[symbol] = NewPositionBook(DefaultMMR)
	}
	if _, ok := l.cross[symbol]; !ok {
		l.cross[symbol] = &crossBook{accs: make(map[int64]*CrossAccount)}
	}
}

// Book 返回指定交易对的持仓账本。
func (l *Liquidator) Book(symbol string) (*PositionBook, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	b, ok := l.books[symbol]
	return b, ok
}

// crossBookOf 返回指定交易对的全仓账本（内部，不加锁）。
func (l *Liquidator) crossBookOf(symbol string) (*crossBook, bool) {
	cb, ok := l.cross[symbol]
	return cb, ok
}

// OpenCross 全仓开仓/加仓：若账户不存在则创建并划入 balance 共享保证金；
// 若已存在则把 balance 追加进共享池，并在指定方向开仓。
// 返回更新后的全仓账户。
func (l *Liquidator) OpenCross(symbol string, userID int64, side PosSide, size, price, balance, leverage float64, now int64) *CrossAccount {
	cb, ok := l.crossBookOf(symbol)
	if !ok {
		return nil
	}
	cb.mu.Lock()
	a, ok := cb.accs[userID]
	if !ok {
		a = NewCrossAccount(userID, symbol, balance)
		cb.accs[userID] = a
	} else {
		a.Balance += balance
	}
	a.Open(side, size, price, leverage, now)
	cb.mu.Unlock()
	return a
}

// ModeOf 返回指定用户在该交易对下的保证金模式（默认逐仓）。
func (l *Liquidator) ModeOf(symbol string, userID int64) MarginMode {
	if cb, ok := l.crossBookOf(symbol); ok {
		cb.mu.RLock()
		_, x := cb.accs[userID]
		cb.mu.RUnlock()
		if x {
			return Cross
		}
	}
	return Isolated
}

// AdjustCrossBalance 调整某全仓账户的共享保证金（资金费结算时调用，delta 可正可负）。
// 非全仓用户调用无效。
func (l *Liquidator) AdjustCrossBalance(symbol string, userID int64, delta float64) {
	if cb, ok := l.crossBookOf(symbol); ok {
		cb.mu.Lock()
		if a, ok := cb.accs[userID]; ok {
			a.Balance += delta
		}
		cb.mu.Unlock()
	}
}

// CrossBalance 返回某全仓账户的共享保证金（无账户返回 0）。
func (l *Liquidator) CrossBalance(symbol string, userID int64) float64 {
	if cb, ok := l.crossBookOf(symbol); ok {
		cb.mu.RLock()
		a, ok := cb.accs[userID]
		cb.mu.RUnlock()
		if ok {
			return a.Balance
		}
	}
	return 0
}

// AllPositions 返回某交易对下全部持仓快照（逐仓 + 全仓），供资金结算与查询使用。
// 全仓账户的多/空腿会填充 Mode=Cross 与展示用 LiqPriceVal。
func (l *Liquidator) AllPositions(symbol string) []Position {
	var out []Position
	if b, ok := l.Book(symbol); ok {
		out = append(out, b.Positions()...)
	}
	if cb, ok := l.crossBookOf(symbol); ok {
		cb.mu.RLock()
		for _, a := range cb.accs {
			for _, p := range a.Sides() {
				snap := *p
				snap.LiqPriceVal = a.LiqPrice()
				out = append(out, snap)
			}
		}
		cb.mu.RUnlock()
	}
	return out
}

// UpdateMarkPrice 更新标记价格并对该交易对所有持仓做强平扫描。
// 返回本次触发的强平事件。
func (l *Liquidator) UpdateMarkPrice(symbol string, mark float64) []LiquidationEvent {
	l.mu.RLock()
	book, ok := l.books[symbol]
	l.mu.RUnlock()
	if !ok {
		return nil
	}
	book.SetMarkPrice(mark)
	setLastMark(symbol, mark)

	book.mu.RLock()
	positions := make([]*Position, 0, len(book.pos))
	for _, p := range book.pos {
		positions = append(positions, p)
	}
	book.mu.RUnlock()

	var triggered []LiquidationEvent
	for _, p := range positions {
		if !p.IsLiquidatable(mark) {
			continue
		}
		ev := l.liquidate(book, p, mark)
		if ev != nil {
			triggered = append(triggered, *ev)
		}
	}

	// 全仓扫描：账户级强平（整户）。
	if cb, ok := l.crossBookOf(symbol); ok {
		cb.mu.RLock()
		accs := make([]*CrossAccount, 0, len(cb.accs))
		for _, a := range cb.accs {
			accs = append(accs, a)
		}
		cb.mu.RUnlock()
		for _, a := range accs {
			if !a.IsLiquidatable(mark) {
				continue
			}
			ev := l.liquidateCross(cb, a, mark)
			if ev != nil {
				triggered = append(triggered, *ev)
			}
		}
	}
	return triggered
}

// liquidate 执行单个持仓的强平：阶梯式部分平仓，直到保证金率恢复到安全线
// （SafeMarginRatio）或整仓平完。记录事件，并在穿仓且保险基金不足时触发 ADL。
//
// 阶梯维持保证金率使"平掉一部分 → 名义价值降档 → 维持需求跳降"成为可能，
// 因此大盘高挡位仓位可在不全部平仓的情况下恢复保证金率（而非整仓强平）。
func (l *Liquidator) liquidate(book *PositionBook, p *Position, mark float64) *LiquidationEvent {
	book.mu.Lock()
	// 双重检查，避免并发重复强平
	cur := book.pos[p.UserID]
	if cur == nil || !cur.IsLiquidatable(mark) {
		book.mu.Unlock()
		return nil
	}
	origMargin := cur.Margin
	origSize := cur.Size
	var totalRealized, totalClosed, totalDeficit, sumNotional float64
	var fullyClosed bool
	for {
		if cur.MarginRatio(mark) >= SafeMarginRatio {
			break // 保证金率已恢复，停止部分强平
		}
		if cur.Size <= 1e-9 {
			fullyClosed = true
			break
		}
		// 把强平单送入撮合引擎成交（closer 注入后走真实撮合），据真实成交均价回填持仓。
		target := cur.Size * l.partialRatio
		fill := l.closer(p.Symbol, p.UserID, p.Side, target, mark)
		if fill.Filled <= 1e-9 {
			// 撮合引擎无法成交（兜底异常）：保守视为已清仓，避免死循环。
			fullyClosed = true
			break
		}
		r, d, fc := cur.closeBy(fill.AvgPrice, fill.Filled)
		totalRealized += r
		totalClosed += fill.Filled
		sumNotional += fill.AvgPrice * fill.Filled
		totalDeficit += d
		if fc {
			fullyClosed = true
			break
		}
	}
	fee := sumNotional * LiquidationFeeRate
	closedFrac := 1.0
	if origSize > 1e-9 {
		closedFrac = (origSize - cur.Size) / origSize
	}
	ev := LiquidationEvent{
		UserID:     p.UserID,
		Symbol:     p.Symbol,
		Side:       p.Side,
		Mode:       Isolated,
		Size:       totalClosed,
		EntryPrice: p.EntryPrice,
		LiqPrice:   mark,
		Fee:        fee,
		Realized:   totalRealized,
		Time:       time.Now().UnixNano(),
	}
	if fullyClosed {
		ev.Margin = origMargin // 整仓：全部保证金没收归宿保险基金
		delete(book.pos, p.UserID)
	} else {
		ev.Partial = true
		ev.RemainingSize = cur.Size
		ev.Margin = origMargin * closedFrac // 部分平：释放对应比例的保证金
	}
	ev.Deficit = totalDeficit
	book.mu.Unlock()

	l.mu.Lock()
	l.events = append(l.events, ev)
	if len(l.events) > 1000 {
		l.events = l.events[len(l.events)-1000:]
	}
	l.mu.Unlock()

	if l.onLiq != nil {
		l.onLiq(ev)
	}
	// 穿仓（成交价劣于破产价）→ 三层瀑布吸收：保险基金 → ADL → 社会化分摊 → 残差
	if totalDeficit > 0 {
		l.absorbDeficit(p.Symbol, opposite(p.Side), totalDeficit, &ev)
	}
	return &ev
}

// liquidateCross 执行全仓账户的强平：阶梯式部分平仓（同 liquidate），
// 直到账户保证金率恢复到安全线或整户平完。整户强平没收全部共享保证金
// （ev.Margin = 账户余额总额），清空多/空双腿。
func (l *Liquidator) liquidateCross(cb *crossBook, a *CrossAccount, mark float64) *LiquidationEvent {
	cb.mu.Lock()
	cur := cb.accs[a.UserID]
	if cur == nil || !cur.IsLiquidatable(mark) {
		cb.mu.Unlock()
		return nil
	}
	// 账户级代表方向：取净值更大的腿（多空双边时）。
	side := Long
	var entry float64
	if cur.Long != nil {
		side, entry = Long, cur.Long.EntryPrice
	}
	if cur.Short != nil {
		if cur.Long == nil {
			side, entry = Short, cur.Short.EntryPrice
		} else if cur.Short.Size >= cur.Long.Size {
			side, entry = Short, cur.Short.EntryPrice
		}
	}
	origBalance := cur.Balance
	var totalRealized, totalClosed, totalDeficit, sumNotional float64
	var fullyClosed bool
	for {
		if cur.MarginRatio(mark) >= SafeMarginRatio {
			break
		}
		if (cur.Long == nil || cur.Long.Size <= 1e-9) && (cur.Short == nil || cur.Short.Size <= 1e-9) {
			fullyClosed = true
			break
		}
		// 对每条非空腿把强平单送入撮合引擎成交，据真实成交均价回填该腿持仓。
		if cur.Long != nil && cur.Long.Size > 1e-9 {
			target := cur.Long.Size * l.partialRatio
			fill := l.closer(a.Symbol, a.UserID, Long, target, mark)
			if fill.Filled > 1e-9 {
				r, d, fc := cur.Long.closeBy(fill.AvgPrice, fill.Filled)
				totalRealized += r
				totalClosed += fill.Filled
				sumNotional += fill.AvgPrice * fill.Filled
				totalDeficit += d
				if fc {
					cur.Long = nil
				}
			}
		}
		if cur.Short != nil && cur.Short.Size > 1e-9 {
			target := cur.Short.Size * l.partialRatio
			fill := l.closer(a.Symbol, a.UserID, Short, target, mark)
			if fill.Filled > 1e-9 {
				r, d, fc := cur.Short.closeBy(fill.AvgPrice, fill.Filled)
				totalRealized += r
				totalClosed += fill.Filled
				sumNotional += fill.AvgPrice * fill.Filled
				totalDeficit += d
				if fc {
					cur.Short = nil
				}
			}
		}
		if cur.Long == nil && cur.Short == nil {
			fullyClosed = true
			break
		}
	}
	fee := sumNotional * LiquidationFeeRate
	ev := LiquidationEvent{
		UserID:     a.UserID,
		Symbol:     a.Symbol,
		Side:       side,
		Mode:       Cross,
		Size:       totalClosed,
		EntryPrice: entry,
		LiqPrice:   mark,
		Fee:        fee,
		Realized:   totalRealized,
		Time:       time.Now().UnixNano(),
	}
	if fullyClosed {
		ev.Margin = origBalance // 整户：共享保证金没收归宿保险基金
		delete(cb.accs, a.UserID)
	} else {
		ev.Partial = true
		ev.RemainingSize = cur.Long.Size + cur.Short.Size
		// 全仓部分平：单腿 Margin=0，无独立释放；盈亏调整体现在共享余额（a.Balance），
		// 上层据 ev.Realized 调整冻结余额，此处 Margin 记为 0（不单独释放到可用）。
		ev.Margin = 0
	}
	ev.Deficit = totalDeficit
	cb.mu.Unlock()

	l.mu.Lock()
	l.events = append(l.events, ev)
	if len(l.events) > 1000 {
		l.events = l.events[len(l.events)-1000:]
	}
	l.mu.Unlock()

	if l.onLiq != nil {
		l.onLiq(ev)
	}
	// 穿仓 → 三层瀑布吸收：保险基金 → ADL → 社会化分摊 → 残差
	if totalDeficit > 0 {
		against := opposite(side)
		if cur.Short != nil && cur.Long != nil {
			// 双腿账户：以净值较大腿的反向为对手
			if cur.Long.Size >= cur.Short.Size {
				against = Short
			} else {
				against = Long
			}
		}
		l.absorbDeficit(a.Symbol, against, totalDeficit, &ev)
	}
	return &ev
}

// opposite 返回反向。
func opposite(s PosSide) PosSide {
	if s == Long {
		return Short
	}
	return Long
}

// adlTarget ADL 候选减仓目标（对手方向盈利仓位）。
type adlTarget struct {
	userID int64
	symbol string
	mode   MarginMode
	pos    *Position
	upnl   float64 // 当前未实现盈亏（标记价）
	lev    float64
}

// adlCandidates 收集某交易对、指定方向的对手持仓（逐仓 + 全仓），按
// 「未实现盈利降序、杠杆降序」排序（盈利越高、杠杆越高越优先被减）。
func (l *Liquidator) adlCandidates(symbol string, against PosSide, mark float64) []adlTarget {
	var out []adlTarget
	if b, ok := l.books[symbol]; ok {
		b.mu.RLock()
		for uid, p := range b.pos {
			if p.Side == against {
				out = append(out, adlTarget{userID: uid, symbol: symbol, mode: Isolated, pos: p, upnl: p.UPNL(mark), lev: p.Leverage})
			}
		}
		b.mu.RUnlock()
	}
	if cb, ok := l.cross[symbol]; ok {
		cb.mu.RLock()
		for uid, a := range cb.accs {
			for _, leg := range a.Sides() {
				if leg.Side == against {
					out = append(out, adlTarget{userID: uid, symbol: symbol, mode: Cross, pos: leg, upnl: leg.UPNL(mark), lev: leg.Leverage})
				}
			}
		}
		cb.mu.RUnlock()
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].upnl != out[j].upnl {
			return out[i].upnl > out[j].upnl
		}
		return out[i].lev > out[j].lev
	})
	return out
}

// runADL 执行自动减仓：从对手方向盈利最高的仓位开始，以标记价（不滑点）部分减仓，
// 把减仓实现的盈利转入保险基金以吸收穿仓亏损，直到覆盖 deficit 或候选盈利耗尽。
// 返回实际吸收的亏损额（<= deficit）。被减仓者按缺口需要部分减仓，而非整腿。
func (l *Liquidator) runADL(symbol string, against PosSide, deficit float64) (covered float64) {
	cands := l.adlCandidates(symbol, against, getLastMark(symbol))
	remaining := deficit
	mark := getLastMark(symbol)
	for _, c := range cands {
		if remaining <= 1e-9 {
			break
		}
		if c.upnl <= 0 {
			continue // 仅盈利仓位可吸收穿仓
		}
		// 仅减仓到刚好覆盖剩余缺口所需的盈利比例（不超过整腿）。
		take := remaining
		if c.upnl < take {
			take = c.upnl
		}
		ratio := take / c.upnl
		reduceSize := c.pos.Size * ratio
		if c.mode == Isolated {
			if b, ok := l.books[symbol]; ok {
				b.mu.Lock()
				if p := b.pos[c.userID]; p != nil {
					_, _, fc, _ := p.PartialClose(mark, ratio)
					if fc {
						delete(b.pos, c.userID)
					}
				}
				b.mu.Unlock()
			}
		} else {
			if cb, ok := l.cross[symbol]; ok {
				cb.mu.Lock()
				if a := cb.accs[c.userID]; a != nil {
					if c.pos.Side == Long {
						if _, _, fc, _ := a.Long.PartialClose(mark, ratio); fc {
							a.Long = nil
						}
					} else {
						if _, _, fc, _ := a.Short.PartialClose(mark, ratio); fc {
							a.Short = nil
						}
					}
					if a.Long == nil && a.Short == nil {
						delete(cb.accs, c.userID)
					}
				}
				cb.mu.Unlock()
			}
		}
		ev := ADLEvent{
			UserID:        c.userID,
			Symbol:        symbol,
			Side:          c.pos.Side,
			ReducedSize:   reduceSize,
			Price:         mark,
			ProfitCovered: take,
			Time:          time.Now().UnixNano(),
		}
		l.mu.Lock()
		l.adls = append(l.adls, ev)
		if len(l.adls) > 1000 {
			l.adls = l.adls[len(l.adls)-1000:]
		}
		l.mu.Unlock()
		if l.onADL != nil {
			l.onADL(ev)
		}
		covered += take
		remaining -= take
	}
	return covered
}

// absorbDeficit 穿仓吸收瀑布：保险基金 → ADL → 社会化分摊 → 残差。
// 把各层吸收额写入 ev 的分层字段，并通过回调驱动账本资金流。
//
// 资金流（保证全局借贷恒等）：
//  1. 保险基金支付全部穿仓亏损：Debit(SysInsurance) + Credit(SysLiquidationLoss)。
//  2. ADL：被减仓盈利方 Debit 可用 + Credit(SysInsurance)，回填保险基金。
//  3. 社会化分摊：各盈利方按份额 Debit 可用 + Credit(SysInsurance)，回填保险基金。
//
// 保险基金净变化 = -deficit + adlCovered + socialized；残差 = deficit - 保险可付 - 已回填，
// 即保险基金最终承载的未覆盖系统损失（可为负）。
func (l *Liquidator) absorbDeficit(symbol string, against PosSide, deficit float64, ev *LiquidationEvent) {
	insAvail := l.insurance()
	insCovered := deficit
	if insAvail < insCovered {
		insCovered = insAvail
	}
	if insCovered < 0 {
		insCovered = 0
	}
	remaining := deficit - insCovered

	adlCovered := 0.0
	if remaining > 1e-9 {
		adlCovered = l.runADL(symbol, against, remaining)
		remaining -= adlCovered
	}

	socialized := 0.0
	var shares []SocializedLossEvent
	if remaining > 1e-9 {
		socialized, shares = l.runSocializedLoss(symbol, remaining)
		remaining -= socialized
	}

	residual := remaining
	if residual < 0 {
		residual = 0
	}

	// 保险基金支付全部穿仓亏损（不足部分由 ADL/社会化回填，净残差即系统损失）。
	l.onDeficitPay(deficit)
	if len(shares) > 0 {
		l.onSocialize(shares)
	}

	ev.InsuranceCovered = insCovered
	ev.ADLCovered = adlCovered
	ev.Socialized = socialized
	ev.Residual = residual
}

// socializeCandidates 收集某交易对所有盈利持仓（多空均可，任何方向的盈利都能分摊穿仓）。
func (l *Liquidator) socializeCandidates(symbol string, mark float64) []adlTarget {
	var out []adlTarget
	if b, ok := l.books[symbol]; ok {
		b.mu.RLock()
		for uid, p := range b.pos {
			if p.UPNL(mark) > 0 {
				out = append(out, adlTarget{userID: uid, symbol: symbol, mode: Isolated, pos: p, upnl: p.UPNL(mark), lev: p.Leverage})
			}
		}
		b.mu.RUnlock()
	}
	if cb, ok := l.cross[symbol]; ok {
		cb.mu.RLock()
		for uid, a := range cb.accs {
			for _, leg := range a.Sides() {
				if leg.UPNL(mark) > 0 {
					out = append(out, adlTarget{userID: uid, symbol: symbol, mode: Cross, pos: leg, upnl: leg.UPNL(mark), lev: leg.Leverage})
				}
			}
		}
		cb.mu.RUnlock()
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].upnl != out[j].upnl {
			return out[i].upnl > out[j].upnl
		}
		return out[i].lev > out[j].lev
	})
	return out
}

// runSocializedLoss 社会化分摊：ADL 后仍不足时，由同交易对所有盈利持仓按未实现盈利
// 比例分摊剩余亏损。每个被分摊者按份额部分减仓，实现盈利转为穿仓吸收。
// 返回实际吸收额与分摊事件列表。
func (l *Liquidator) runSocializedLoss(symbol string, deficit float64) (float64, []SocializedLossEvent) {
	mark := getLastMark(symbol)
	cands := l.socializeCandidates(symbol, mark)
	var totalProfit float64
	for _, c := range cands {
		totalProfit += c.upnl
	}
	if totalProfit <= 1e-9 {
		return 0, nil
	}
	remaining := deficit
	var shares []SocializedLossEvent
	for _, c := range cands {
		if remaining <= 1e-9 {
			break
		}
		// 该户按盈利占比分摊（不超过自身盈利、不超过剩余缺口）
		share := deficit * (c.upnl / totalProfit)
		if share > c.upnl {
			share = c.upnl
		}
		if share > remaining {
			share = remaining
		}
		ratio := share / c.upnl
		reduceSize := c.pos.Size * ratio
		if c.mode == Isolated {
			if b, ok := l.books[symbol]; ok {
				b.mu.Lock()
				if p := b.pos[c.userID]; p != nil {
					_, _, fc, _ := p.PartialClose(mark, ratio)
					if fc {
						delete(b.pos, c.userID)
					}
				}
				b.mu.Unlock()
			}
		} else {
			if cb, ok := l.cross[symbol]; ok {
				cb.mu.Lock()
				if a := cb.accs[c.userID]; a != nil {
					if c.pos.Side == Long {
						if _, _, fc, _ := a.Long.PartialClose(mark, ratio); fc {
							a.Long = nil
						}
					} else {
						if _, _, fc, _ := a.Short.PartialClose(mark, ratio); fc {
							a.Short = nil
						}
					}
					if a.Long == nil && a.Short == nil {
						delete(cb.accs, c.userID)
					}
				}
				cb.mu.Unlock()
			}
		}
		shares = append(shares, SocializedLossEvent{
			UserID:      c.userID,
			Symbol:      symbol,
			Side:        c.pos.Side,
			ReducedSize: reduceSize,
			Price:       mark,
			Share:       share,
			Time:        time.Now().UnixNano(),
		})
		remaining -= share
	}
	covered := deficit - remaining
	if covered < 0 {
		covered = 0
	}
	if len(shares) > 0 {
		l.mu.Lock()
		l.socs = append(l.socs, shares...)
		if len(l.socs) > 1000 {
			l.socs = l.socs[len(l.socs)-1000:]
		}
		l.mu.Unlock()
	}
	return covered, shares
}

// lastMark 各交易对最近一次标记价格（供 ADL 以标记价减仓）。
var (
	lastMarkMu sync.RWMutex
	lastMark   = make(map[string]float64)
)

func setLastMark(symbol string, mark float64) {
	lastMarkMu.Lock()
	lastMark[symbol] = mark
	lastMarkMu.Unlock()
}

func getLastMark(symbol string) float64 {
	lastMarkMu.RLock()
	m := lastMark[symbol]
	lastMarkMu.RUnlock()
	return m
}

// RecentADL 返回最近 ADL 记录（按时间倒序）。
func (l *Liquidator) RecentADL() []ADLEvent {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]ADLEvent, len(l.adls))
	copy(out, l.adls)
	sort.Slice(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return out
}

// RecentSocialized 返回最近社会化分摊记录（按时间倒序）。
func (l *Liquidator) RecentSocialized() []SocializedLossEvent {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]SocializedLossEvent, len(l.socs))
	copy(out, l.socs)
	sort.Slice(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return out
}

// RecentLiquidations 返回最近强平记录（按时间倒序）。
func (l *Liquidator) RecentLiquidations() []LiquidationEvent {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]LiquidationEvent, len(l.events))
	copy(out, l.events)
	sort.Slice(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return out
}

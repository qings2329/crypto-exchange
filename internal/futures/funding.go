package futures

import (
	"sort"
	"sync"
	"time"
)

// 资金费率相关常量（演示值，贴近主流交易所）。
const (
	// InterestRatePerInterval 每个资金结算周期的名义利率（无风险利率差），演示取 0.01%/周期。
	// 主流交易所通常 0.01%~0.03% 每 8 小时。
	InterestRatePerInterval = 0.0001
	// PremiumClamp 溢价成分限幅（±0.05%），防止极端行情下费率失控。
	PremiumClamp = 0.0005
	// DefaultFundingInterval 默认资金结算周期。生产为 8h；演示用短周期便于观察。
	DefaultFundingInterval = 8 * time.Hour
)

// PremiumIndex 溢价指数 = (标记价 - 指数价) / 指数价。
// 反映合约相对现货的溢价/折价程度。
func PremiumIndex(mark, index float64) float64 {
	if index <= 0 || !validFinite(mark) || !validFinite(index) {
		return 0 // F5：非法行情直接归零，避免 NaN/Inf 污染费率
	}
	return (mark - index) / index
}

// FundingRate 资金费率 = 名义利率 + 限幅后的溢价成分(EMA)。
// 费率 > 0：多头向空头支付；费率 < 0：空头向多头支付。
func FundingRate(interest, emaPremium float64) float64 {
	if !validFinite(interest) || !validFinite(emaPremium) {
		return 0 // F5：异常输入归零，避免费率变为 NaN 后落账为 0
	}
	c := emaPremium
	if c > PremiumClamp {
		c = PremiumClamp
	}
	if c < -PremiumClamp {
		c = -PremiumClamp
	}
	return interest + c
}

// FundingPayment 单个持仓在一轮资金结算中应支付/收取的金额（已带符号）。
// Payment > 0 表示收到（资金流入），< 0 表示付出（资金流出）。
// 规则：多头支付 = -名义价值 × 费率；空头收取 = +名义价值 × 费率。
type FundingPayment struct {
	UserID   int64
	Side     PosSide
	Notional float64
	Payment  float64
}

// FundingEvent 一轮资金结算的聚合事件（审计/前端/复盘用）。
type FundingEvent struct {
	Symbol      string
	Time        int64
	IndexPrice  float64
	MarkPrice   float64
	FundingRate float64
	Payments    []FundingPayment
}

// fundingState 单交易对的资金费率状态。
// 溢价 EMA 与标记价格统一由 MarkPriceCalculator 维护，这里只保存结算所需的
// 指数价与最近一次结算的费率快照。
type fundingState struct {
	mu          sync.RWMutex
	indexPrice  float64
	lastRate    float64
	lastPremium float64
}

// FundingManager 资金费率管理器：维护各交易对的指数价、溢价指数 EMA、费率，
// 并在结算周期触发对所有持仓的资金费用结算。
type FundingManager struct {
	mu          sync.RWMutex
	states      map[string]*fundingState
	interval    time.Duration
	history     []FundingEvent
	lastSettled map[string]int64 // 每交易对最近一次成功结算所在周期起点（纳秒），用于同周期幂等防重（F1）
}

// NewFundingManager 创建资金费率管理器；interval 传 0 时使用默认 8h。
func NewFundingManager(interval time.Duration) *FundingManager {
	if interval <= 0 {
		interval = DefaultFundingInterval
	}
	return &FundingManager{
		states:      make(map[string]*fundingState),
		interval:    interval,
		lastSettled: make(map[string]int64),
	}
}

// Register 注册交易对的资金费率状态。
func (f *FundingManager) Register(symbol string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.states[symbol]; ok {
		return
	}
	f.states[symbol] = &fundingState{}
}

// UpdateIndexPrice 更新指数价（由外部指数价源/预言机驱动）。
func (f *FundingManager) UpdateIndexPrice(symbol string, index float64) {
	f.mu.RLock()
	s, ok := f.states[symbol]
	f.mu.RUnlock()
	if !ok {
		return
	}
	s.mu.Lock()
	s.indexPrice = index
	s.mu.Unlock()
}

// State 返回指定交易对最近一次结算的资金费率快照（供展示）。
// 实时溢价与标记价由 MarkPriceCalculator 提供，结算事件中的费率为结算时刻值。
func (f *FundingManager) State(symbol string) (index, lastPremium, rate float64, ok bool) {
	f.mu.RLock()
	s, ok := f.states[symbol]
	f.mu.RUnlock()
	if !ok {
		return 0, 0, 0, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.indexPrice, s.lastPremium, s.lastRate, true
}

// Settle 对指定交易对执行一轮资金结算。
// mark 为结算时刻的标记价格（来自 MarkPriceCalculator），premium 为溢价指数 EMA
// （同样来自 MarkPriceCalculator），positions 为当前持仓。
// 返回结算事件；钱包扣减由调用方（cmd/futures）根据 Payments 接入 Ledger 完成。
//
// 同周期幂等（F1）：仅在进入新的结算周期（intervalStart）时才真正结算，重复触发
// （行情循环重入、服务重启）不会导致资金费被二次扣收；ev.Time 设为周期起点，
// 供调用方生成稳定幂等 Ref 并对接账本 OpTransfer 去重。
func (f *FundingManager) Settle(symbol string, mark, premium float64, positions []Position) FundingEvent {
	f.mu.RLock()
	s, ok := f.states[symbol]
	f.mu.RUnlock()
	ev := FundingEvent{Symbol: symbol, Time: time.Now().UnixNano()}
	if !ok {
		return ev
	}
	// F5：非法标记价直接跳过本轮结算，避免据 NaN/Inf 算出异常资金费。
	if !validMark(mark) {
		return ev
	}
	// F1：同周期仅结算一次。
	now := time.Now().UnixNano()
	intervalStart := now - (now % int64(f.interval))
	f.mu.Lock()
	if f.lastSettled[symbol] == intervalStart {
		f.mu.Unlock()
		return ev // 本周期已结算，跳过（不重复扣费）
	}
	f.lastSettled[symbol] = intervalStart
	f.mu.Unlock()
	ev.Time = intervalStart // 稳定周期标识，供调用方生成幂等 Ref

	s.mu.Lock()
	index := s.indexPrice
	rate := FundingRate(InterestRatePerInterval, premium)
	s.lastRate = rate
	s.lastPremium = premium
	s.mu.Unlock()

	ev.IndexPrice = index
	ev.MarkPrice = mark
	ev.FundingRate = rate

	payments := make([]FundingPayment, 0, len(positions))
	for _, p := range positions {
		notional := p.Notional(mark)
		if !validFinite(notional) {
			continue // F5：异常名义价值跳过该持仓
		}
		var pay float64
		if p.Side == Long {
			pay = -notional * rate // 多头付出
		} else {
			pay = notional * rate // 空头收取
		}
		if !validFinite(pay) {
			continue // F5：异常资金费跳过该持仓
		}
		// F2：吸附到 USDT 最小精度，使账本转账额精确、无亚聪偏差。
		pay = roundSatoshi(pay)
		payments = append(payments, FundingPayment{
			UserID:   p.UserID,
			Side:     p.Side,
			Notional: notional,
			Payment:  pay,
		})
	}
	ev.Payments = payments

	f.mu.Lock()
	f.history = append(f.history, ev)
	if len(f.history) > 1000 {
		f.history = f.history[len(f.history)-1000:]
	}
	f.mu.Unlock()
	return ev
}

// RecentFunding 返回最近的资金结算历史（按时间倒序）。
func (f *FundingManager) RecentFunding() []FundingEvent {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]FundingEvent, len(f.history))
	copy(out, f.history)
	sort.Slice(out, func(i, j int) bool { return out[i].Time > out[j].Time })
	return out
}

// Interval 返回资金结算周期。
func (f *FundingManager) Interval() time.Duration {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.interval
}

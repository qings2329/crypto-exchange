package futures

import (
	"math"
	"sync"
)

// 合约方向：多头 / 空头。注意这是持仓方向，与撮合引擎的买/卖方向不同。
type PosSide int

const (
	Long PosSide = iota
	Short
)

// DefaultMMR 默认（最低档）维持保证金率，用于无名义价值上下文时的兜底。
const DefaultMMR = 0.005

// MaintenanceTier 阶梯维持保证金档位：名义价值越大，维持率越高（仓位越大风险越高）。
// 仿主流交易所设计；生产环境应按 VIP/风险档位、单边/双边持仓、币种分别细分。
type MaintenanceTier struct {
	UpToNotional float64 // 该档名义价值上限（含）；math.Inf(1) 表示无上限
	Rate         float64 // 该档维持保证金率
}

// DefaultMaintenanceTiers 演示用阶梯费率（以 USDT 计名义价值）：
//   - ≤ 100k：0.50%（最低档）
//   - ≤ 1M：1.00%
//   - ≤ 5M：2.00%
//   - > 5M：2.50%（最高档）
//
// 关键意义：大盘仓位处于高挡位（高维持率），部分强平时平掉一部分使名义价值降档，
// 维持保证金需求随之跳降，从而有机会在不全部平仓的情况下恢复保证金率。
var DefaultMaintenanceTiers = []MaintenanceTier{
	{UpToNotional: 100_000, Rate: 0.005},
	{UpToNotional: 1_000_000, Rate: 0.010},
	{UpToNotional: 5_000_000, Rate: 0.020},
	{UpToNotional: math.Inf(1), Rate: 0.025},
}

// MaintenanceMarginRate 按名义价值返回对应档位的维持保证金率。
func MaintenanceMarginRate(notional float64) float64 {
	for _, t := range DefaultMaintenanceTiers {
		if notional <= t.UpToNotional {
			return t.Rate
		}
	}
	return DefaultMaintenanceTiers[len(DefaultMaintenanceTiers)-1].Rate
}

// SafeMarginRatio 部分强平后目标保证金率：恢复到该值即停止继续平仓。
// 略高于 1.0（强平价阈值），为账户留出缓冲，避免反复在阈值边缘抖动。
const SafeMarginRatio = 1.1

// LiquidationFeeRate 强平手续费（演示值 0.5%），归保险基金/清算引擎。
const LiquidationFeeRate = 0.005

// MarginMode 保证金模式。
type MarginMode int

const (
	// Isolated 逐仓：每仓独立保证金，爆仓不影响账户其他仓位。
	Isolated MarginMode = iota
	// Cross 全仓：同一用户同一交易对下所有持仓共享一个保证金池。
	Cross
)

// Position 持仓（逐仓或全仓下的单腿）。
// 逐仓（isolated）时 Margin 为该仓位独立锁定的保证金，爆仓仅损失本仓保证金。
// 全仓（cross）时 Margin 记为 0，真实保证金在 CrossAccount.Balance 共享池中。
type Position struct {
	UserID     int64
	Symbol     string
	Side       PosSide
	Size       float64 // 持仓张数，单位为基础资产（如 1 = 1 BTC）
	EntryPrice float64 // 开仓均价
	Margin     float64 // 逐仓模式下的锁定保证金；全仓模式下为 0
	Leverage   float64
	Mode       MarginMode // 该持仓所属保证金模式
	OpenTime   int64
	LiqPriceVal float64 // 展示用强平价（由开仓/快照时计算填充）
}

// Notional 持仓名义价值（以报价货币计）。
func (p *Position) Notional(mark float64) float64 {
	return p.Size * mark
}

// UPNL 未实现盈亏。
func (p *Position) UPNL(mark float64) float64 {
	if p.Side == Long {
		return (mark - p.EntryPrice) * p.Size
	}
	return (p.EntryPrice - mark) * p.Size
}

// Equity 仓位权益 = 保证金 + 未实现盈亏。
func (p *Position) Equity(mark float64) float64 {
	return p.Margin + p.UPNL(mark)
}

// MaintenanceMargin 维持保证金 = 名义价值 × 维持保证金率（按名义价值分档）。
func (p *Position) MaintenanceMargin(mark float64) float64 {
	return p.Notional(mark) * MaintenanceMarginRate(p.Notional(mark))
}

// MarginRatio 保证金率 = 权益 / 维持保证金。
// 实际交易所用「Margin Ratio = (Equity) / (InitialMargin + ... )」，此处用更直观的
// 权益相对维持保证金的比例：<=1 即触发强平。
func (p *Position) MarginRatio(mark float64) float64 {
	mm := p.MaintenanceMargin(mark)
	if mm <= 0 {
		return math.Inf(1)
	}
	return p.Equity(mark) / mm
}

// IsLiquidatable 当权益不足以覆盖维持保证金时触发强平。
func (p *Position) IsLiquidatable(mark float64) bool {
	return p.Equity(mark) <= p.MaintenanceMargin(mark)
}

// LiqPrice 强平价（用于前端展示，由保证金率=1 反解）。
// 多头：M = Entry*(1 - 1/L) / (1 - MMR)
// 空头：M = Entry*(1 + 1/L) / (1 + MMR)
func (p *Position) LiqPrice() float64 {
	// 按开仓名义价值取档（展示用快照；价格变动会改变档位，强平价仅作参考）。
	mmr := MaintenanceMarginRate(p.Size * p.EntryPrice)
	denom := 1 - mmr
	if p.Side == Short {
		denom = 1 + mmr
	}
	numer := 1 - 1/p.Leverage
	if p.Side == Short {
		numer = 1 + 1/p.Leverage
	}
	return p.EntryPrice * numer / denom
}

// BankruptcyPrice 破产价（权益=0 时）。
// 多头：M = Entry*(1 - 1/L)；空头：M = Entry*(1 + 1/L)
func (p *Position) BankruptcyPrice() float64 {
	if p.Side == Long {
		return p.EntryPrice * (1 - 1/p.Leverage)
	}
	return p.EntryPrice * (1 + 1/p.Leverage)
}

// PositionBook 单交易对的逐仓持仓账本（线程安全）。
type PositionBook struct {
	mu    sync.RWMutex
	pos   map[int64]*Position // userID -> position
	mmr   float64
	liqMu float64 // 标记价格
}

// NewPositionBook 创建持仓账本。
func NewPositionBook(mmr float64) *PositionBook {
	if mmr <= 0 {
		mmr = DefaultMMR
	}
	return &PositionBook{pos: make(map[int64]*Position), mmr: mmr}
}

// MarkPrice 返回当前标记价格。
func (pb *PositionBook) MarkPrice() float64 {
	pb.mu.RLock()
	defer pb.mu.RUnlock()
	return pb.liqMu
}

// Positions 返回当前所有持仓快照。
func (pb *PositionBook) Positions() []Position {
	pb.mu.RLock()
	defer pb.mu.RUnlock()
	out := make([]Position, 0, len(pb.pos))
	for _, p := range pb.pos {
		out = append(out, *p)
	}
	return out
}

// SetMarkPrice 更新标记价格（由行情/指数价回调驱动）。
func (pb *PositionBook) SetMarkPrice(p float64) {
	pb.mu.Lock()
	pb.liqMu = p
	pb.mu.Unlock()
}

// Open 开仓/加仓：返回更新后的持仓。
// margin 为该笔新开仓锁定的保证金；size 为该笔张数；price 为成交均价。
func (pb *PositionBook) Open(userID int64, symbol string, side PosSide, size, price, margin, leverage float64, now int64) *Position {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	p, ok := pb.pos[userID]
	if !ok {
		p = &Position{UserID: userID, Symbol: symbol, Side: side, Mode: Isolated, Leverage: leverage, OpenTime: now}
		pb.pos[userID] = p
	}
	// 加仓：加权计算开仓均价与累计保证金
	totalSize := p.Size + size
	if totalSize > 0 {
		p.EntryPrice = (p.EntryPrice*p.Size + price*size) / totalSize
	}
	p.Size = totalSize
	p.Margin += margin
	// 杠杆取本次开仓值（逐仓简化：以末次杠杆为准）
	p.Leverage = leverage
	p.LiqPriceVal = p.LiqPrice()
	return p
}

// CrossAccount 全仓（cross）保证金账户：同一用户同一交易对下，多头与空头持仓
// 共享一个保证金池 Balance。任一方向亏损都消耗共享余额，任一方向盈利都增加账户
// 权益。当「账户权益 <= 全部持仓维持保证金之和」时，整户被强平。
//
// 与逐仓的区别：
//   - 逐仓：爆仓只损失单仓 Margin，不影响同用户其他仓位。
//   - 全仓：共享资金兜底，扛单能力更强，但一个仓位拉爆会连累同户同对的所有仓位。
type CrossAccount struct {
	UserID  int64
	Symbol  string
	Balance float64 // 已划入的共享保证金（来自钱包冻结）
	Long    *Position
	Short   *Position
}

// NewCrossAccount 创建全仓账户并划入初始共享保证金。
func NewCrossAccount(userID int64, symbol string, balance float64) *CrossAccount {
	return &CrossAccount{UserID: userID, Symbol: symbol, Balance: balance}
}

// Open 在指定方向开仓/加仓；price 为本次成交均价，size 为本笔张数。
// 全仓的保证金在账户层共享，单腿 Position.Margin 记为 0。
func (a *CrossAccount) Open(side PosSide, size, price float64, leverage float64, now int64) {
	var p *Position
	if side == Long {
		p = a.Long
	} else {
		p = a.Short
	}
	if p == nil {
		p = &Position{UserID: a.UserID, Symbol: a.Symbol, Side: side, Mode: Cross, OpenTime: now}
		if side == Long {
			a.Long = p
		} else {
			a.Short = p
		}
	}
	total := p.Size + size
	if total > 0 {
		p.EntryPrice = (p.EntryPrice*p.Size + price*size) / total
	}
	p.Size = total
	p.Margin = 0 // 全仓：保证金在账户层共享
	p.Leverage = leverage
}

// Notional 账户名义价值（多空名义价值之和）。
func (a *CrossAccount) Notional(mark float64) float64 {
	var n float64
	if a.Long != nil {
		n += a.Long.Notional(mark)
	}
	if a.Short != nil {
		n += a.Short.Notional(mark)
	}
	return n
}

// UPNL 账户未实现盈亏（多空盈亏之和）。
func (a *CrossAccount) UPNL(mark float64) float64 {
	var u float64
	if a.Long != nil {
		u += a.Long.UPNL(mark)
	}
	if a.Short != nil {
		u += a.Short.UPNL(mark)
	}
	return u
}

// Equity 账户权益 = 共享保证金 + 未实现盈亏。
func (a *CrossAccount) Equity(mark float64) float64 {
	return a.Balance + a.UPNL(mark)
}

// Maintenance 账户维持保证金 = 多空维持保证金之和。
func (a *CrossAccount) Maintenance(mark float64) float64 {
	var m float64
	if a.Long != nil {
		m += a.Long.MaintenanceMargin(mark)
	}
	if a.Short != nil {
		m += a.Short.MaintenanceMargin(mark)
	}
	return m
}

// MarginRatio 账户保证金率 = 权益 / 维持保证金；<=1 即触发强平。
func (a *CrossAccount) MarginRatio(mark float64) float64 {
	mm := a.Maintenance(mark)
	if mm <= 0 {
		return math.Inf(1)
	}
	return a.Equity(mark) / mm
}

// IsLiquidatable 账户权益不足以覆盖全部持仓维持保证金时，整户强平。
func (a *CrossAccount) IsLiquidatable(mark float64) bool {
	return a.Equity(mark) <= a.Maintenance(mark)
}

// LiqPrice 账户强平价（用于前端展示）。
// 仅单边持仓时有解析解；多空双边持仓相互抵消，返回 0（前端展示为 N/A）。
// 多头：M = (size*entry - Balance) / (size*(1 - MMR))
// 空头：M = (Balance + size*entry) / (size*(1 + MMR))
func (a *CrossAccount) LiqPrice() float64 {
	if a.Long != nil && a.Short == nil {
		p := a.Long
		mmr := MaintenanceMarginRate(p.Size * p.EntryPrice)
		denom := p.Size * (1 - mmr)
		if denom <= 0 {
			return 0
		}
		return (p.Size*p.EntryPrice - a.Balance) / denom
	}
	if a.Short != nil && a.Long == nil {
		p := a.Short
		mmr := MaintenanceMarginRate(p.Size * p.EntryPrice)
		denom := p.Size * (1 + mmr)
		if denom <= 0 {
			return 0
		}
		return (a.Balance + p.Size*p.EntryPrice) / denom
	}
	return 0
}

// Sides 返回当前非空持仓（快照指针）。
func (a *CrossAccount) Sides() []*Position {
	var out []*Position
	if a.Long != nil {
		out = append(out, a.Long)
	}
	if a.Short != nil {
		out = append(out, a.Short)
	}
	return out
}

// DefaultPartialLiqRatio 默认部分强平比例：1.0 = 整仓/整户强平（与历史行为一致）。
// 生产常用 0.5（先平一半），配合阶梯维持保证金率逐步恢复保证金率。
const DefaultPartialLiqRatio = 1.0

// closeBy 按指定成交价与张数平仓，原地修改持仓。
// 与 PartialClose 的区别：平仓价由撮合引擎真实成交均价决定（而非固定标记价），
// 用于强平把强平单送入撮合引擎成交后的持仓回填。
// 返回：实现盈亏 realized、穿仓亏损 deficit、是否清仓 fullyClosed。
func (p *Position) closeBy(price, qty float64) (realized, deficit float64, fullyClosed bool) {
	if qty > p.Size {
		qty = p.Size
	}
	if qty <= 1e-9 {
		return 0, 0, p.Size <= 1e-9
	}
	if p.Side == Long {
		realized = (price - p.EntryPrice) * qty
	} else {
		realized = (p.EntryPrice - price) * qty
	}
	bp := p.BankruptcyPrice()
	if p.Side == Long && price < bp {
		deficit = (bp - price) * qty
	} else if p.Side == Short && price > bp {
		deficit = (price - bp) * qty
	}
	// 按平仓比例释放保证金（实现盈亏由上层结算到可用余额，不计入冻结保证金）。
	release := p.Margin * (qty / p.Size)
	p.Margin -= release
	p.Size -= qty
	if p.Size <= 1e-9 {
		fullyClosed = true
	}
	return
}

// PartialClose 按 ratio(0,1] 比例对持仓做市价（@mark）部分平仓，原地修改持仓。
// 返回：
//   - realized：本次平仓实现的盈亏（多：(mark-entry)*q；空：(entry-mark)*q）
//   - closed：本次平仓张数
//   - fullyClosed：是否彻底清仓
//   - deficit：本次平仓的穿仓亏损（成交价 mark 劣于破产价时的差额，由保险基金/ADL 吸收）
//
// 注意：对深亏仓位，平仓实现亏损会使释放保证金 < 实现亏损，故"部分平"本身未必能恢复
// 保证金率——真实交易所依赖阶梯维持保证金率（平掉一部分使仓位降档、维持需求跳降）才能恢复。
// 此处按配置比例演示部分强平流程。ADL/社会化分摊强制减对手盈利仓时也复用本方法（按标记价减仓）。
func (p *Position) PartialClose(mark, ratio float64) (realized, closed float64, fullyClosed bool, deficit float64) {
	if ratio <= 0 {
		ratio = 1
	}
	if ratio > 1 {
		ratio = 1
	}
	closed = p.Size * ratio
	if closed < 1e-9 {
		return 0, 0, p.Size <= 1e-9, 0
	}
	r, d, fc := p.closeBy(mark, closed)
	return r, closed, fc, d
}

// closeBy 按指定成交价与张数平多/空双腿，原地修改，并据盈亏调整共享保证金。
// 每条非空腿按其当前张数比例减仓（与 PartialClose 的 ratio 语义一致），平仓价统一为 price
// （来自撮合引擎真实成交均价）。返回值语义同 Position.closeBy。
func (a *CrossAccount) closeBy(price, qty float64) (realized, closed float64, fullyClosed bool, deficit float64) {
	if qty <= 0 {
		return 0, 0, a.Long == nil && a.Short == nil, 0
	}
	// 按各腿当前张数占净值的比例分配本次平仓张数。
	var totalSize float64
	if a.Long != nil {
		totalSize += a.Long.Size
	}
	if a.Short != nil {
		totalSize += a.Short.Size
	}
	if totalSize <= 1e-9 {
		return 0, 0, true, 0
	}
	if a.Long != nil && a.Long.Size > 1e-9 {
		qL := qty * (a.Long.Size / totalSize)
		rL, dL, fcL := a.Long.closeBy(price, qL)
		realized += rL
		closed += qL
		deficit += dL
		if fcL {
			a.Long = nil
		}
	}
	if a.Short != nil && a.Short.Size > 1e-9 {
		qS := qty * (a.Short.Size / totalSize)
		rS, dS, fcS := a.Short.closeBy(price, qS)
		realized += rS
		closed += qS
		deficit += dS
		if fcS {
			a.Short = nil
		}
	}
	// 全仓单腿 Margin=0，释放保证金为 0；共享余额直接按盈亏调整。
	a.Balance += realized
	if a.Long == nil && a.Short == nil {
		fullyClosed = true
	}
	return
}

// PartialClose 对全仓账户按 ratio 比例平多/空双腿，原地修改，并据盈亏调整共享保证金。
// 返回值语义同 Position.PartialClose（deficit 为双腿穿仓亏损之和）。
func (a *CrossAccount) PartialClose(mark, ratio float64) (realized, closed float64, fullyClosed bool, deficit float64) {
	if ratio <= 0 {
		ratio = 1
	}
	if ratio > 1 {
		ratio = 1
	}
	var cL, cS float64
	if a.Long != nil {
		rL, clL, fcL, dL := a.Long.PartialClose(mark, ratio)
		realized += rL
		cL = clL
		deficit += dL
		if fcL {
			a.Long = nil
		}
	}
	if a.Short != nil {
		rS, clS, fcS, dS := a.Short.PartialClose(mark, ratio)
		realized += rS
		cS = clS
		deficit += dS
		if fcS {
			a.Short = nil
		}
	}
	closed = cL + cS
	// 全仓单腿 Margin=0，释放保证金为 0；共享余额直接按盈亏调整。
	a.Balance += realized
	if a.Long == nil && a.Short == nil {
		fullyClosed = true
	}
	return
}

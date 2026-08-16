// Package ledger 提供交易所钱包的复式记账核心。
//
// 设计要点：
//   - 每个用户每种资产一个 Account，含可用(Available)与冻结(Frozen)余额。
//   - 所有资金变动都落一条 Entry 流水（带符号 Delta），便于审计与对账。
//   - 跨用户资金转移通过 Transfer 实现，两边各记一条相反数流水，全局借贷恒等。
//   - 系统账户（资金费中转池、保险基金）也是普通 Account，使"交易所角色"也纳入复式记账。
package ledger

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"
)

// 系统账户 ID（与真实用户 ID 区分，使用特殊值）。
const (
	// SysFundingPool 资金费中转账户：多头付出的资金费先入此账户，再付出给空头。
	// 一轮结算净额恒为 0，余额应始终为 0。
	SysFundingPool int64 = 0
	// SysInsurance 保险基金：强平没收的剩余保证金归宿，亦用于吸收穿仓亏损。
	SysInsurance int64 = -1
	// SysLiquidationLoss 穿仓损失归集账户：累计被审计/吸收的穿仓亏损（被销毁的价值）。
	// 保险基金支付穿仓亏损时，Debit(SysInsurance) 与 Credit(SysLiquidationLoss) 配对，
	// 保证全局借贷恒等；ADL/社会化分摊把盈利转入保险基金，逐步回填 SysInsurance。
	SysLiquidationLoss int64 = -2
	// SysChainClearing 链上清结算负债账户：用户在链上充值经确认后入账时，
	// 从该账户 Debit（负债减少）并 Credit 用户可用余额，保持复式记账守恒。
	// 余额应为负的用户链上充值累计（交易所对用户的总负债）。
	SysChainClearing int64 = -3
	// SysBadDebt 坏账垫付账户：链上充值被孤块/重组丢弃时，若用户已动用该笔资金
	// （可用余额不足以全额回拨），交易所须先行垫付差额，记为本账户负债（余额为负）。
	// 与 ReverseOnChain 配合，保证"充值回滚"业务的借贷恒等。坏账后续由风控/审计
	// 通过追回或社会化分摊消化。
	SysBadDebt int64 = -4
	// SysOptions 期权中央对手方账户：用户买入期权支付的权利金归集于此，
	// 到期/行权时向用户支付的收益（内在价值）亦从此账户支出，保持复式记账守恒。
	SysOptions int64 = -5
	// SysOtc OTC 场外交易中央托管（escrow）账户：成交订单创建时卖方 crypto 冻结入此账户，
	// 买方法币线下支付、卖方确认后由本账户释放给买方；取消/争议退回则归还卖方。
	// 所有订单终态后余额应恒为 0，便于对账。
	SysOtc int64 = -6
	// SysWealth 理财资管中央托管账户：用户申购时本金转入此账户（余额为正），
	// 赎回时本金+应计收益从此账户支出；账户余额恒等于「在管本金 - 用户应计收益负债」，
	// 即 余额 = Σ本金 - Σ已计收益。系统账户允许透支，复式记账自动守恒。
	SysWealth int64 = -7
)

// Entry 一条资金流水（复式记账的单边）。
type Entry struct {
	ID      int64
	UserID  int64 // 账户主体（含系统账户）
	Asset   string
	Delta   float64 // + 入账 / - 出账（相对该账户）
	Balance float64 // 变动后的可用余额（仅记录可用部分变化）
	BizType string  // 业务类型：open/funding/liquidation/transfer/deposit
	Ref     string  // 关联单号（订单号 / 结算周期 / 强平用户）
	Time    int64
}

// Account 单一用户单一资产的钱包。
//
// 两类冻结明确区分，避免资金安全语义混淆：
//   - Frozen：持仓保证金冻结（开仓/强平时由 futures 引擎锁定，不可提现）；
//   - WithdrawFrozen：提现冻结（提交提现后由出入金路径锁定，待链上确认达标后划出）。
//
// 二者互斥：提现只动 WithdrawFrozen，开仓只动 Frozen，互不干扰，杜绝"提现结算误扣
// 持仓保证金"或"回滚提现冲掉开仓保证金"的资损路径。
type Account struct {
	UserID         int64
	Asset          string
	Available      float64
	Frozen         float64 // 持仓保证金冻结
	WithdrawFrozen float64 // 提现冻结
}

// ReconStats 对账巡检的最近一次结果快照（供监控/HTTP 端点展示）。
type ReconStats struct {
	LastDeviation  map[string]float64 // 各资产借贷偏差（接近 0 即平衡）
	LastBalanced   bool               // 最近一次是否全局平衡
	LastRun        time.Time          // 最近一次巡检时间
	ImbalanceCount int                // 累计探测到的不平账次数（告警计数）
}

// Ledger 钱包总账（线程安全）。
type Ledger struct {
	mu         sync.RWMutex
	accounts   map[string]*Account // key = "userID:asset"
	restricted map[string]bool     // 出金限制：key = "userID:asset" -> true（有未冲抵坏账，强制先补缴）
	seq        int64
	log        []Entry

	// 对账巡检（定时对账探针）：reconMu 独立保护巡检状态，避免长持有 l.mu。
	reconMu      sync.RWMutex
	reconStats   ReconStats
	alertHook    func(map[string]float64) // 不平账告警回调（生产可接监控/告警平台）
	stopRecon    chan struct{}
	reconRunning bool

	// 坏账归属：key = "userID:asset" -> 该用户造成的未冲抵坏账额。
	// 用于"用户级精确解限"——仅当某用户自身的坏账贡献被冲抵后才解除其出金限制，
	// 而非全局坏账结清才一刀切解除所有人（避免 A 已补缴却仍被 B 的欠款连坐限制）。
	badDebtByUser map[string]float64

	// 社会化分摊治理提案（按资产维度，每资产最多一个待审批提案）。
	socializeProposals map[string]*SocializeProposal

	// 链上钱包库存（交易所自有资产，独立于用户负债账本）：
	//   hotWallet[asset]  = 热钱包在链上的余额（持续暴露于热私钥泄露/被攻破风险）；
	//   coldWallet[asset] = 冷钱包在链上的余额（离线多签/空气隙保管，窃取需突破离线防线）；
	//   hotWalletCap[asset] = 热钱包风险敞口上限，超过则触发自动归集（sweep）到冷钱包。
	// 二者之和恒等于 -SysChainClearing[asset]（交易所对用户净负债 = 实际链上持仓），
	// 属资金安全不变量（偿付能力/储备证明）：InventoryMatchesLiability 校验之。
	hotWallet    map[string]float64
	coldWallet   map[string]float64
	hotWalletCap map[string]float64

	// 提现安全冷静期（延时清算）风控：用户提现请求先进入冷静期队列并冻结，期满后运维/定时调度
	// 才真正链上广播清算。即便账户/热钱包被攻破，冷静期给风控留出冻结止损窗口（与冷热钱包
	// 防线互补）。withdrawHoldPeriod 为冷静期时长；withdrawHolds 为活跃队列（按 holdID 索引）。
	withdrawHoldPeriod time.Duration
	withdrawHolds      map[string]*WithdrawHoldEntry
	withdrawHoldSeq    int64
	// 全局紧急冻结：运维发现异常（私钥疑似泄露/异常大额出金）时一键冻结所有出金受理。
	withdrawalFrozenGlobal bool
	// 每日提现限额（按资产维度），与每日累计共同约束单用户单日提现总额。
	dailyWithdrawLimit map[string]float64
	dailyWithdrawUsed  map[string]float64 // key = "uid:asset:YYYY-MM-DD" -> 当日累计提现额（含手续费）

	// 提现地址白名单（防钓鱼/未授权地址盗提）：出金地址须预登记并验证，未验证地址拒绝受理。
	// 新地址含"验证冷静期"——即便立即验证，首次可用于提现前仍需等待 addressVerifyPeriod，
	// 给风控/用户留出发现异常地址的窗口（与提现冷静期互补，形成"新地址+新提现"双重延时，
	// 即便账户被攻破，攻击者也无法在冷静期内把资金转去未授权地址）。
	addressVerifyPeriod time.Duration
	withdrawAddressBook map[int64]map[string]*WithdrawAddress // uid -> "asset|chain|address" -> 地址条目

	// 可疑行为风控引擎：把 #18 的"手动全局紧急冻结"与 #16 的白名单升级为自动风控，
	// 让提现冷静期/白名单/冻结真正形成闭环——检测到异常（提现速率骤增、短时间大量新增地址）
	// 即自动触发全局冻结并留痕，给风控/运维留出人工介入窗口。
	riskEnabled        bool          // 风控引擎总开关
	riskAutoFreeze     bool          // 触发高危规则时是否自动全局冻结
	riskWindow         time.Duration // 滑动窗口（行为计数/累计的时间范围）
	riskVelocityAmount float64       // 窗口内单用户提现累计额阈值（跨资产合计）
	riskVelocityCount  int           // 窗口内单用户提现请求次数阈值
	riskAddrBurstCount int           // 窗口内单用户新增地址数阈值
	riskEvents         []*RiskEvent  // 风控事件审计轨迹（环形增长，可持久化）
	riskEventSeq       int64
	autoFrozenByRisk   bool // 当前全局冻结是否由风控引擎自动触发（人工 resume 后清零）
	// 瞬态行为活动（不持久化，重启后自然冷启动）：
	riskWithdrawActivity map[int64][]riskAct   // uid -> 最近提现活动（at/amount）
	riskAddrActivity     map[int64][]time.Time // uid -> 最近新增地址时间
}

// RiskEvent 一条风控引擎产出的可疑行为事件（审计/告警溯源）。
type RiskEvent struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"` // withdraw_velocity | address_burst
	UserID      int64     `json:"user_id"`
	Severity    string    `json:"severity"` // high | medium | low
	Message     string    `json:"message"`
	Action      string    `json:"action"` // auto_global_freeze | logged
	TriggeredAt time.Time `json:"triggered_at"`
}

// riskAct 单条提现活动记录（瞬态，用于滑动窗口统计）。
type riskAct struct {
	at     time.Time
	amount float64
}

// WithdrawHoldEntry 一笔处于冷静期的提现请求。资金已在账本侧冻结（WithdrawFrozen），
// 但尚未链上广播；HoldUntil 之前不可清算，给风控留出拦截窗口。
type WithdrawHoldEntry struct {
	ID        string    `json:"id"`
	UserID    int64     `json:"user_id"`
	Asset     string    `json:"asset"`
	Amount    float64   `json:"amount"` // 净额（到用户外部地址）
	Fee       float64   `json:"fee"`
	Chain     string    `json:"chain"`
	Address   string    `json:"address"`
	CreatedAt time.Time `json:"created_at"`
	HoldUntil time.Time `json:"hold_until"`
	Finalized bool      `json:"finalized"`
	Cancelled bool      `json:"cancelled"`
	// Broadcasted 标记该 hold 是否已占广播槽（链上 SubmitWithdraw 已发起）。配合 TxHash 实现
	// finalizeHold 的幂等广播：重试/并发时复用既有 txHash、跳过重复 SubmitWithdraw，杜绝双提现（F1）。
	Broadcasted bool   `json:"broadcasted"`
	TxHash      string `json:"tx_hash"`
}

// WithdrawAddress 一条提现地址白名单条目（按 userID + asset + chain + address 维度）。
//
// 安全模型：出金地址须先 Add 预登记，再经 Confirm（模拟 2FA/邮件验证）标记为已验证。
// 即便立即验证，新地址仍须度过 addressVerifyPeriod 验证冷静期（VerifyUntil 之前）方可首次
// 用于提现——形成"新地址 + 新提现"双重延时，与提现冷静期互补：即便账户/热钱包被攻破，
// 攻击者也无法在冷静期内把资金转去未授权的新地址。Verified 为假或仍在验证冷静期内，
// RequestWithdrawHold 一律拒绝受理。
type WithdrawAddress struct {
	UserID      int64     `json:"user_id"`
	Asset       string    `json:"asset"`
	Chain       string    `json:"chain"`
	Address     string    `json:"address"`
	Label       string    `json:"label"`
	AddedAt     time.Time `json:"added_at"`
	Verified    bool      `json:"verified"`     // 是否已通过 2FA/邮件验证
	VerifyUntil time.Time `json:"verify_until"` // 新地址验证冷静期截止（首次可用于提现的前提）
}

// New 创建总账。
func New() *Ledger {
	return &Ledger{
		accounts:             make(map[string]*Account),
		restricted:           make(map[string]bool),
		badDebtByUser:        make(map[string]float64),
		socializeProposals:   make(map[string]*SocializeProposal),
		hotWallet:            make(map[string]float64),
		coldWallet:           make(map[string]float64),
		hotWalletCap:         make(map[string]float64),
		withdrawHolds:        make(map[string]*WithdrawHoldEntry),
		dailyWithdrawLimit:   make(map[string]float64),
		dailyWithdrawUsed:    make(map[string]float64),
		withdrawAddressBook:  make(map[int64]map[string]*WithdrawAddress),
		riskWithdrawActivity: make(map[int64][]riskAct),
		riskAddrActivity:     make(map[int64][]time.Time),
	}
}

// SetOutflowRestricted 设置/解除某用户某资产的出金限制（有未冲抵坏账时限制，强制先补缴）。
func (l *Ledger) SetOutflowRestricted(userID int64, asset string, restricted bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := l.key(userID, asset)
	if restricted {
		l.restricted[k] = true
	} else {
		delete(l.restricted, k)
	}
}

// IsOutflowRestricted 查询某用户某资产是否处于出金限制（线程安全）。
func (l *Ledger) IsOutflowRestricted(userID int64, asset string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.restricted[l.key(userID, asset)]
}

// RestrictedCount 返回当前处于出金限制的用户-资产条目数（线程安全），用于监控指标。
func (l *Ledger) RestrictedCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.restricted)
}

// reconcileRestrictionsLocked 按"用户级精确解限"规则刷新某资产的出金限制（调用方须已持写锁）。
//
// 规则：
//   - 若全局坏账已结清（SysBadDebt≈0，可能因他人补缴/社会化分摊兜底），解除该资产全部限制；
//   - 否则仅解除"自身坏账贡献已被冲抵（badDebtByUser≈0）"的用户，避免已补缴者被他人欠款连坐。
func (l *Ledger) reconcileRestrictionsLocked(asset string) {
	suffix := ":" + asset
	bd := l.getOrCreateLocked(SysBadDebt, asset)
	globalCleared := bd.Available >= -1e-9
	for k := range l.restricted {
		if !strings.HasSuffix(k, suffix) {
			continue
		}
		if globalCleared || l.badDebtByUser[k] <= 1e-9 {
			delete(l.restricted, k)
		}
	}
	if globalCleared {
		// 全局结清：清零该资产全部坏账归属，保持不变量（归属之和 == 全局坏账额）。
		for k := range l.badDebtByUser {
			if strings.HasSuffix(k, suffix) {
				delete(l.badDebtByUser, k)
			}
		}
	}
}

func (l *Ledger) key(userID int64, asset string) string {
	return fmt.Sprintf("%d:%s", userID, asset)
}

// GetOrCreate 获取或创建账户。
func (l *Ledger) GetOrCreate(userID int64, asset string) *Account {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := l.key(userID, asset)
	a, ok := l.accounts[k]
	if !ok {
		a = &Account{UserID: userID, Asset: asset}
		l.accounts[k] = a
	}
	return a
}

// Balance 返回指定账户的可用与冻结余额。
func (l *Ledger) Balance(userID int64, asset string) (available, frozen float64, ok bool) {
	l.mu.RLock()
	a, ok := l.accounts[l.key(userID, asset)]
	l.mu.RUnlock()
	if !ok {
		return 0, 0, false
	}
	return a.Available, a.Frozen, true
}

// Freeze 将可用余额冻结（开仓锁定保证金）。可用不足返回错误。
func (l *Ledger) Freeze(userID int64, asset string, amount float64) error {
	if amount < 0 {
		return fmt.Errorf("freeze amount must be >= 0")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.getOrCreateLocked(userID, asset)
	if a.Available < amount-1e-9 {
		return fmt.Errorf("insufficient available balance: have %.8f want %.8f", a.Available, amount)
	}
	a.Available -= amount
	a.Frozen += amount
	return nil
}

// Unfreeze 解冻到可用余额（平仓释放保证金）。冻结不足返回错误。
func (l *Ledger) Unfreeze(userID int64, asset string, amount float64) error {
	if amount < 0 {
		return fmt.Errorf("unfreeze amount must be >= 0")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.getOrCreateLocked(userID, asset)
	if a.Frozen < amount-1e-9 {
		return fmt.Errorf("insufficient frozen balance")
	}
	a.Frozen -= amount
	a.Available += amount
	return nil
}

// FreezeWithdraw 将可用余额冻结为提现冻结（提交提现受理时锁定，与持仓保证金冻结
// Frozen 互不干扰）。可用不足返回错误。
func (l *Ledger) FreezeWithdraw(userID int64, asset string, amount float64) error {
	if amount < 0 {
		return fmt.Errorf("freeze withdraw amount must be >= 0")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.getOrCreateLocked(userID, asset)
	if a.Available < amount-1e-9 {
		return fmt.Errorf("insufficient available balance: have %.8f want %.8f", a.Available, amount)
	}
	a.Available -= amount
	a.WithdrawFrozen += amount
	return nil
}

// UnfreezeWithdraw 解冻提现冻结到可用余额（提现失败回退或回滚后退回可用）。冻结不足返回错误。
func (l *Ledger) UnfreezeWithdraw(userID int64, asset string, amount float64) error {
	if amount < 0 {
		return fmt.Errorf("unfreeze withdraw amount must be >= 0")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.getOrCreateLocked(userID, asset)
	if a.WithdrawFrozen < amount-1e-9 {
		return fmt.Errorf("insufficient withdraw frozen balance")
	}
	a.WithdrawFrozen -= amount
	a.Available += amount
	return nil
}

// WithdrawFrozenBalance 返回指定账户的提现冻结余额（线程安全）。
func (l *Ledger) WithdrawFrozenBalance(userID int64, asset string) (withdrawFrozen float64, ok bool) {
	l.mu.RLock()
	a, ok := l.accounts[l.key(userID, asset)]
	l.mu.RUnlock()
	if !ok {
		return 0, false
	}
	return a.WithdrawFrozen, true
}

// CreditAvailable 增加可用余额（入账，如充值、收到资金费、平仓释放盈亏）。
func (l *Ledger) CreditAvailable(userID int64, asset string, amount float64, biz, ref string) error {
	if amount < 0 {
		return fmt.Errorf("credit amount must be >= 0")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.getOrCreateLocked(userID, asset)
	a.Available += amount
	l.appendLocked(a, +amount, biz, ref)
	return nil
}

// DebitAvailable 减少可用余额（出账，如支付资金费、提现）。
// 骨架允许余额转为负数以承载坏账（真实交易所需风控拦截）；生产应在此前做余额校验。
func (l *Ledger) DebitAvailable(userID int64, asset string, amount float64, biz, ref string) error {
	if amount < 0 {
		return fmt.Errorf("debit amount must be >= 0")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.getOrCreateLocked(userID, asset)
	a.Available -= amount
	l.appendLocked(a, -amount, biz, ref)
	return nil
}

// Transfer 从 from 可用余额转到 to 可用余额，两边各记一条相反数流水。
func (l *Ledger) Transfer(from, to int64, asset string, amount float64, biz, ref string) error {
	if amount < 0 {
		return fmt.Errorf("transfer amount must be >= 0")
	}
	if from == to {
		return fmt.Errorf("transfer to self")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fa := l.getOrCreateLocked(from, asset)
	ta := l.getOrCreateLocked(to, asset)
	// 允许 from 余额转负（坏账），保证资金费闭环的借贷恒等
	fa.Available -= amount
	ta.Available += amount
	l.appendSideLocked(fa, -amount, biz, ref, &l.seq)
	l.appendSideLocked(ta, +amount, biz, ref, &l.seq)
	return nil
}

// Deposit 充值（演示用；生产充值应来自链上确认/清结算系统）。
func (l *Ledger) Deposit(userID int64, asset string, amount float64, ref string) error {
	return l.CreditAvailable(userID, asset, amount, "deposit", ref)
}

// ReceiveOnChain 链上充值入账：经链上确认后，从链上清结算负债账户划转到用户可用余额。
// 采用复式记账（Debit 负债账户 + Credit 用户），全局借贷恒等。ref 建议为链上交易哈希。
//
// 坏账自动回收：若此前充值孤块回滚导致交易所垫付了坏账（SysBadDebt 为负），本笔充值会
// 优先冲抵该坏账——同等金额先"还给"坏账账户，剩余才入用户可用。这样充值回滚产生的
// 死账变为可循环回收的闭环，且全程借贷恒等。
func (l *Ledger) ReceiveOnChain(userID int64, asset string, amount float64, txHash string) error {
	if amount < 0 {
		return fmt.Errorf("deposit amount must be >= 0")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.getOrCreateLocked(userID, asset)
	sys := l.getOrCreateLocked(SysChainClearing, asset)
	bd := l.getOrCreateLocked(SysBadDebt, asset)
	ref := "chain:" + txHash
	// 标准充值：负债减少、用户可用增加
	sys.Available -= amount
	l.appendSideLocked(sys, -amount, "chain_deposit", ref, &l.seq)
	a.Available += amount
	l.appendSideLocked(a, +amount, "chain_deposit", ref, &l.seq)
	// 冷热钱包库存：充值入账 = 交易所热钱包收到链上资金（库存增加），并触发超限自动归集。
	l.hotWallet[asset] += amount
	l.autoSweepHotLocked(asset)
	// 坏账回收：用本笔充值优先冲抵交易所垫付坏账
	if bd.Available < 0 {
		recovered := math.Min(amount, -bd.Available)
		if recovered > 0 {
			a.Available -= recovered
			bd.Available += recovered
			l.appendSideLocked(a, -recovered, "bad_debt_recover", ref, &l.seq)
			l.appendSideLocked(bd, +recovered, "bad_debt_recover", ref, &l.seq)
			// 按各债务人坏账归属比例分摊本笔回收（保持"归属之和==全局坏账额"不变量）
			l.applyRecoveryToAttributionLocked(asset, recovered)
		}
		// 刷新出金限制（已持锁）：全局结清解限全部，否则仅解限自身贡献已冲抵者
		l.reconcileRestrictionsLocked(asset)
	}
	return nil
}

// applyRecoveryToAttributionLocked 将一笔坏账回收额按各债务人当前归属比例冲减，保持
// badDebtByUser 之和恒等于该资产全局坏账额（调用方须已持写锁）。
func (l *Ledger) applyRecoveryToAttributionLocked(asset string, recovered float64) {
	suffix := ":" + asset
	var total float64
	for k, v := range l.badDebtByUser {
		if strings.HasSuffix(k, suffix) && v > 1e-9 {
			total += v
		}
	}
	if total <= 1e-9 {
		return
	}
	for k, v := range l.badDebtByUser {
		if !strings.HasSuffix(k, suffix) || v <= 1e-9 {
			continue
		}
		share := recovered * (v / total)
		l.badDebtByUser[k] -= share
		if l.badDebtByUser[k] <= 1e-9 {
			delete(l.badDebtByUser, k)
		}
	}
}

// BadDebtTotal 返回某资产交易所未冲抵的坏账总额（SysBadDebt 余额为负值的绝对值，
// 为 0 表示无坏账）。用于风控/审计展示与回收决策。
func (l *Ledger) BadDebtTotal(asset string) float64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	bd := l.getOrCreateLocked(SysBadDebt, asset)
	if bd.Available < 0 {
		return -bd.Available
	}
	return 0
}

// RepayBadDebt 用户主动补缴坏账：从用户可用余额划出 amount 冲抵交易所坏账账户
// （Debit 用户可用 + Credit SysBadDebt），与充值自动回收同源，保持借贷恒等。
// 补缴额不得超过未冲抵坏账且不得超过用户可用余额。
func (l *Ledger) RepayBadDebt(userID int64, asset string, amount float64, ref string) error {
	if amount <= 0 {
		return fmt.Errorf("repay amount must be > 0")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.getOrCreateLocked(userID, asset)
	if a.Available < amount-1e-9 {
		return fmt.Errorf("insufficient available balance to repay bad debt")
	}
	bd := l.getOrCreateLocked(SysBadDebt, asset)
	if bd.Available >= 0 {
		return fmt.Errorf("no outstanding bad debt to repay")
	}
	// 补缴不超过剩余坏账
	if amount > -bd.Available {
		amount = -bd.Available
	}
	a.Available -= amount
	bd.Available += amount
	l.appendSideLocked(a, -amount, "bad_debt_repay", ref, &l.seq)
	l.appendSideLocked(bd, +amount, "bad_debt_repay", ref, &l.seq)
	// 坏账归属冲减：优先冲抵补缴者自身的坏账贡献，余下按比例分摊给其他债务人
	l.repayAttributionLocked(userID, asset, amount)
	// 刷新出金限制（已持锁）
	l.reconcileRestrictionsLocked(asset)
	return nil
}

// repayAttributionLocked 将一笔补缴额先在坏账归属账上冲减补缴者自身贡献，剩余按比例
// 分摊给其他债务人（调用方须已持写锁）。
func (l *Ledger) repayAttributionLocked(userID int64, asset string, amount float64) {
	suffix := ":" + asset
	k := l.key(userID, asset)
	owe := l.badDebtByUser[k]
	if owe > 1e-9 {
		pay := math.Min(amount, owe)
		l.badDebtByUser[k] -= pay
		amount -= pay
		if l.badDebtByUser[k] <= 1e-9 {
			delete(l.badDebtByUser, k)
		}
	}
	if amount <= 1e-9 {
		return
	}
	// 剩余补缴（如用户自愿多缴替他人兜底）按比例冲减其他债务人
	var total float64
	for kk, vv := range l.badDebtByUser {
		if strings.HasSuffix(kk, suffix) && vv > 1e-9 {
			total += vv
		}
	}
	if total <= 1e-9 {
		return
	}
	for kk, vv := range l.badDebtByUser {
		if !strings.HasSuffix(kk, suffix) || vv <= 1e-9 {
			continue
		}
		share := amount * (vv / total)
		l.badDebtByUser[kk] -= share
		if l.badDebtByUser[kk] <= 1e-9 {
			delete(l.badDebtByUser, kk)
		}
	}
}

// SocializeBadDebt 社会化分摊回收（直接执行版）：等价于先 Propose 再 Approve。保留此入口
// 便于单测与内部调用；对外治理流程建议走 ProposeSocialize + ApproveSocialize 两步审批。
func (l *Ledger) SocializeBadDebt(asset string) (detail map[int64]float64, recovered float64, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.socializeLocked(asset)
}

// socializeLocked 在已持有写锁时执行社会化分摊（实现见 SocializeBadDebt 说明）。
func (l *Ledger) socializeLocked(asset string) (detail map[int64]float64, recovered float64, err error) {
	bd := l.getOrCreateLocked(SysBadDebt, asset)
	if bd.Available >= 0 {
		return map[int64]float64{}, 0, nil // 无坏账可分摊
	}
	debt := -bd.Available
	detail = make(map[int64]float64)
	ref := fmt.Sprintf("socialize:%s:%d", asset, time.Now().UnixNano())

	// 1) 保险基金优先冲减
	ins := l.getOrCreateLocked(SysInsurance, asset)
	if ins.Available > 1e-9 {
		cov := math.Min(debt, ins.Available)
		ins.Available -= cov
		bd.Available += cov
		l.appendSideLocked(ins, -cov, "bad_debt_socialize", ref, &l.seq)
		l.appendSideLocked(bd, +cov, "bad_debt_socialize", ref, &l.seq)
		debt -= cov
		recovered += cov
	}

	// 2) 剩余社会化分摊：按非受限盈利用户可用余额占比
	if debt > 1e-9 {
		type contrib struct {
			uid int64
			av  float64
		}
		var pool []contrib
		var base float64
		for _, a := range l.accounts {
			if a.UserID <= 0 {
				continue // 跳过系统账户
			}
			if l.restricted[l.key(a.UserID, asset)] {
				continue // 坏账来源方不参与分摊（已是受损方）
			}
			if a.Available > 1e-9 {
				pool = append(pool, contrib{a.UserID, a.Available})
				base += a.Available
			}
		}
		// 待分摊总额固定，避免后续用户占比因前序扣减而失真
		toShare := debt
		if base > 1e-9 {
			for _, c := range pool {
				share := toShare * (c.av / base)
				actual := share
				if actual > c.av {
					actual = c.av // cap 在可用余额，不致变负
				}
				if actual <= 0 {
					continue
				}
				a := l.getOrCreateLocked(c.uid, asset)
				a.Available -= actual
				bd.Available += actual
				l.appendSideLocked(a, -actual, "bad_debt_socialize", ref, &l.seq)
				l.appendSideLocked(bd, +actual, "bad_debt_socialize", ref, &l.seq)
				detail[c.uid] += actual
				recovered += actual
				debt -= actual
			}
		}
		// 基数不足时 debt 仍有残留，保留于 SysBadDebt（余额仍为负），等待后续回收
	}

	// 坏账结清（含按比例分摊产生的浮点残差归零）则解除该资产全部出金限制
	if bd.Available >= -1e-9 {
		bd.Available = 0 // 消除分摊浮点残差，避免微小负值卡住结清判断
		l.reconcileRestrictionsLocked(asset)
	}
	return detail, recovered, nil
}

// SocializeProposal 社会化分摊治理提案（待审批）。
type SocializeProposal struct {
	ID        string            // 提案号（唯一）
	Asset     string            // 标的资产
	Recovered float64           // 预计回收总额（保险基金冲减 + 用户分摊）
	Detail    map[int64]float64 // 预计各用户分摊额
	CreatedAt int64             // 提案时间（纳秒）
	Status    string            // pending / approved / rejected
}

// previewSocializeLocked 在已持锁时只读模拟社会化分摊结果（不改动账本），供提案预览使用。
func (l *Ledger) previewSocializeLocked(asset string) (detail map[int64]float64, recovered float64) {
	bd := l.getOrCreateLocked(SysBadDebt, asset)
	if bd.Available >= 0 {
		return map[int64]float64{}, 0
	}
	debt := -bd.Available
	detail = make(map[int64]float64)
	ref := fmt.Sprintf("socialize:%s:%d", asset, time.Now().UnixNano())

	ins := l.getOrCreateLocked(SysInsurance, asset)
	if ins.Available > 1e-9 {
		cov := math.Min(debt, ins.Available)
		debt -= cov
		recovered += cov
	}
	_ = ref
	if debt > 1e-9 {
		type contrib struct {
			uid int64
			av  float64
		}
		var pool []contrib
		var base float64
		for _, a := range l.accounts {
			if a.UserID <= 0 || l.restricted[l.key(a.UserID, asset)] || a.Available <= 1e-9 {
				continue
			}
			pool = append(pool, contrib{a.UserID, a.Available})
			base += a.Available
		}
		toShare := debt
		if base > 1e-9 {
			for _, c := range pool {
				share := toShare * (c.av / base)
				actual := share
				if actual > c.av {
					actual = c.av
				}
				if actual <= 0 {
					continue
				}
				detail[c.uid] += actual
				recovered += actual
				debt -= actual
			}
		}
	}
	return detail, recovered
}

// ProposeSocialize 发起社会化分摊治理提案：仅计算预览（不动账本），生成待审批提案。
// 返回提案号与预览结果。若无未冲抵坏账则返回错误。
func (l *Ledger) ProposeSocialize(asset string) (proposalID string, preview SocializeProposal, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	bd := l.getOrCreateLocked(SysBadDebt, asset)
	if bd.Available >= 0 {
		return "", SocializeProposal{}, fmt.Errorf("no outstanding bad debt to socialize")
	}
	detail, recovered := l.previewSocializeLocked(asset)
	id := fmt.Sprintf("SOC-%s-%d", asset, time.Now().UnixNano())
	p := SocializeProposal{
		ID:        id,
		Asset:     asset,
		Recovered: recovered,
		Detail:    detail,
		CreatedAt: time.Now().UnixNano(),
		Status:    "pending",
	}
	l.socializeProposals[asset] = &p
	return id, p, nil
}

// ApproveSocialize 审批通过并执行社会化分摊：校验提案号匹配后执行 socializeLocked，
// 执行成功将提案置为 approved。提案号不匹配或提案不存在则返回错误。
func (l *Ledger) ApproveSocialize(asset, proposalID string) (detail map[int64]float64, recovered float64, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.socializeProposals[asset]
	if !ok || p.ID != proposalID || p.Status != "pending" {
		return nil, 0, fmt.Errorf("no pending socialize proposal matching asset=%s id=%s", asset, proposalID)
	}
	detail, recovered, err = l.socializeLocked(asset)
	if err != nil {
		return nil, 0, err
	}
	p.Status = "approved"
	return detail, recovered, nil
}

// ReverseOnChain 链上充值回滚（孤块/重组）：经确认入账后若区块被重组丢弃，将此前入账的
// 可用余额回拨到链上清结算负债账户（Debit 用户 + Credit 负债账户），与 ReceiveOnChain
// 互逆，复式记账守恒。ref 建议为链上交易哈希。
//
// 坏账风控：若用户已动用该笔资金（可用余额不足以全额回拨），最多扣减其可用余额至 0，
// 差额由交易所垫付，记入 SysBadDebt 坏账账户（余额转负，表示交易所损失），从而完整保持
// 借贷恒等。返回 badDebt（交易所垫付额，通常为 0）供上层风控/审计处置（追回或分摊）。
func (l *Ledger) ReverseOnChain(userID int64, asset string, amount float64, txHash string) (badDebt float64, err error) {
	if amount < 0 {
		return 0, fmt.Errorf("reverse amount must be >= 0")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.getOrCreateLocked(userID, asset)
	ref := "chain:" + txHash
	// 用户侧：回拨已动用部分（最多扣到 0）
	recovered := amount
	if a.Available < recovered {
		recovered = a.Available
	}
	if recovered > 0 {
		a.Available -= recovered
		l.appendSideLocked(a, -recovered, "chain_rollback", ref, &l.seq)
	}
	// 负债账户：完全抵消之前的充值负债（与 ReceiveOnChain 对称 +amount）
	sys := l.getOrCreateLocked(SysChainClearing, asset)
	sys.Available += amount
	l.appendSideLocked(sys, +amount, "chain_rollback", ref, &l.seq)
	// 充值被孤块丢弃：交易所实际从未收到该笔链上资金，热钱包库存须等额回拨（与 ReceiveOnChain 对称）。
	l.hotWallet[asset] -= amount
	// 坏账：交易所垫付差额（Debit 坏账账户，余额转负）
	badDebt = amount - recovered
	if badDebt > 0 {
		bd := l.getOrCreateLocked(SysBadDebt, asset)
		bd.Available -= badDebt
		l.appendSideLocked(bd, -badDebt, "chain_bad_debt", ref, &l.seq)
		// 坏账归属：记录该用户造成的坏账额（用于用户级精确解限）
		l.badDebtByUser[l.key(userID, asset)] += badDebt
		// 出金限制：产生坏账即限制该用户出金，强制先补缴（已持锁，直接操作 map）
		l.restricted[l.key(userID, asset)] = true
	}
	return badDebt, nil
}

// ReverseWithdraw 链上提现回滚（提现广播后被孤块/重组丢弃）：提现达到安全确认数后已
// 经 SettleWithdraw 把提现冻结余额划出系统、贷记链上清结算负债账户，此时若区块被重组丢弃，
// 需把该笔资金回拨——重新冻结到用户提现冻结（Debit 负债账户 + Credit 用户 WithdrawFrozen），
// 与 SettleWithdraw 互逆，复式记账守恒。ref 建议为链上交易哈希。
// 上层（服务/风控）可随后决定把提现冻结退回可用（重新可提取）或保留冻结待重发。
func (l *Ledger) ReverseWithdraw(userID int64, asset string, amount, fee float64, txHash string) error {
	total := amount + fee
	if total < 0 {
		return fmt.Errorf("withdraw amount must be >= 0")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.getOrCreateLocked(userID, asset)
	sys := l.getOrCreateLocked(SysChainClearing, asset)
	// 资金回拨：负债账户减少（回到"尚未划出"状态），用户提现冻结回升（不影响持仓保证金 Frozen）
	sys.Available -= total
	a.WithdrawFrozen += total
	ref := "chain:" + txHash
	l.appendSideLocked(a, +total, "chain_withdraw_revert", ref, &l.seq)
	l.appendSideLocked(sys, -total, "chain_withdraw_revert", ref, &l.seq)
	// 提现被孤块丢弃：资金从未真正离链（广播未最终确认），热钱包库存须回升（与 SettleWithdraw 对称）。
	l.hotWallet[asset] += total
	return nil
}

// SettleWithdraw 链上提现清算：提现达到安全确认数后，将用户此前提现冻结的余额（含手续费）
// 真正划出系统，贷记到链上清结算负债账户（SysChainClearing 余额回升，表示交易所对用户
// 负债减少）。该操作在持锁内原子完成"扣减提现冻结 -> 贷记负债账户"两笔流水，保证借贷恒等。
// 调用前须已通过 FreezeWithdraw 冻结 (amount+fee)；若提现失败应使用 UnfreezeWithdraw 回退。
func (l *Ledger) SettleWithdraw(userID int64, asset string, amount, fee float64, txHash string) error {
	total := amount + fee
	if total < 0 {
		return fmt.Errorf("withdraw amount must be >= 0")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.getOrCreateLocked(userID, asset)
	if a.WithdrawFrozen < total-1e-9 {
		return fmt.Errorf("insufficient withdraw frozen balance: have %.8f want %.8f", a.WithdrawFrozen, total)
	}
	// 提现冻结离开系统：提现冻结减少，同时贷记负债账户（交易所对用户负债减少），不影响持仓保证金 Frozen。
	a.WithdrawFrozen -= total
	sys := l.getOrCreateLocked(SysChainClearing, asset)
	sys.Available += total
	ref := "chain:" + txHash
	l.appendSideLocked(a, -total, "chain_withdraw", ref, &l.seq)
	l.appendSideLocked(sys, +total, "chain_withdraw", ref, &l.seq)
	// 链上库存：提现资金从热钱包划出到用户外部地址（库存减少）。
	l.hotWallet[asset] -= total
	return nil
}

// History 返回指定账户的流水（按时间正序）。
func (l *Ledger) History(userID int64, asset string) []Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	k := l.key(userID, asset)
	out := make([]Entry, 0)
	for _, e := range l.log {
		if l.key(e.UserID, e.Asset) == k {
			out = append(out, e)
		}
	}
	return out
}

// Log 返回全局流水（审计/对账用）。
func (l *Ledger) Log() []Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Entry, len(l.log))
	copy(out, l.log)
	return out
}

// --- 冷热钱包库存与风险敞口控制 ---

// SetHotWalletCap 设置某资产热钱包风险敞口上限（超过则自动归集冷钱包）。cap<=0 表示不限制。
func (l *Ledger) SetHotWalletCap(asset string, cap float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cap <= 0 {
		delete(l.hotWalletCap, asset)
		return
	}
	l.hotWalletCap[asset] = cap
	l.autoSweepHotLocked(asset) // 设置上限后立即收敛一次（若已超限）
}

// HotWalletBalance 返回某资产热钱包在链上的余额（交易所自有资产，持续暴露于热私钥泄露风险）。
func (l *Ledger) HotWalletBalance(asset string) float64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.hotWallet[asset]
}

// ColdWalletBalance 返回某资产冷钱包在链上的余额（离线多签/空气隙保管，窃取需突破离线防线）。
func (l *Ledger) ColdWalletBalance(asset string) float64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.coldWallet[asset]
}

// HotWalletCap 返回某资产热钱包敞口上限（0 表示未设限）。
func (l *Ledger) HotWalletCap(asset string) float64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.hotWalletCap[asset]
}

// HotWalletExcess 返回某资产热钱包超出上限的敞口（<=0 表示未超限）。用于监控告警。
func (l *Ledger) HotWalletExcess(asset string) float64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cap, ok := l.hotWalletCap[asset]
	if !ok || cap <= 0 {
		return 0
	}
	ex := l.hotWallet[asset] - cap
	if ex < 0 {
		return 0
	}
	return ex
}

// SweepToCold 手动/自动将热钱包资金归集到冷钱包（交易所内部转账，不改变对用户负债）。
// 归集额不超过热钱包实际余额。返回实际归集额。
func (l *Ledger) SweepToCold(asset string, amount float64) (float64, error) {
	if amount < 0 {
		return 0, fmt.Errorf("sweep amount must be >= 0")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	hot := l.hotWallet[asset]
	if amount > hot {
		amount = hot
	}
	if amount <= 0 {
		return 0, nil
	}
	l.hotWallet[asset] -= amount
	l.coldWallet[asset] += amount
	return amount, nil
}

// UnsweepFromCold 从冷钱包调拨资金回热钱包（大额提现前运维拉取，避免热钱包余额不足阻塞出金）。
// 调拨额不超过冷钱包实际余额。返回实际调拨额。
func (l *Ledger) UnsweepFromCold(asset string, amount float64) (float64, error) {
	if amount < 0 {
		return 0, fmt.Errorf("unsweep amount must be >= 0")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cold := l.coldWallet[asset]
	if amount > cold {
		amount = cold
	}
	if amount <= 0 {
		return 0, nil
	}
	l.coldWallet[asset] -= amount
	l.hotWallet[asset] += amount
	return amount, nil
}

// InventoryMatchesLiability 校验某资产"链上实际持仓(hot+cold)"是否等于"对用户净负债(-SysChainClearing)"。
// 二者恒等即证明交易所链上确实持有对用户负债的足额资产（偿付能力/储备证明不变量）。
// 返回偏差（接近 0 即一致）；非 0 意味着账本与链上持仓脱节（凭空铸币/链上丢币/记账错漏），属资金安全事故。
func (l *Ledger) InventoryMatchesLiability(asset string) float64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	sys := l.getOrCreateLocked(SysChainClearing, asset)
	inventory := l.hotWallet[asset] + l.coldWallet[asset]
	expected := -sys.Available
	return inventory - expected
}

// autoSweepHotLocked 在已持锁时按上限自动归集热钱包超额到冷钱包（调用方须已持写锁）。
func (l *Ledger) autoSweepHotLocked(asset string) {
	cap, ok := l.hotWalletCap[asset]
	if !ok || cap <= 0 {
		return
	}
	hot := l.hotWallet[asset]
	if hot > cap+1e-9 {
		excess := hot - cap
		l.hotWallet[asset] -= excess
		l.coldWallet[asset] += excess
	}
}

// --- 提现安全冷静期 / 全局紧急冻结 / 每日限额 ---

// SetWithdrawHoldPeriod 设置提现冷静期时长（冷静期内不可链上清算）。
func (l *Ledger) SetWithdrawHoldPeriod(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.withdrawHoldPeriod = d
}

// WithdrawHoldPeriod 返回当前冷静期时长（线程安全）。
func (l *Ledger) WithdrawHoldPeriod() time.Duration {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.withdrawHoldPeriod
}

// --- 提现地址白名单（新地址验证冷静期）---

// SetAddressVerifyPeriod 设置新地址验证冷静期时长（新登记地址即便已验证，首次可用于提现前
// 仍须等待该时长）。period<=0 表示不等待（演示/测试用）。
func (l *Ledger) SetAddressVerifyPeriod(period time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.addressVerifyPeriod = period
}

// AddressVerifyPeriod 返回当前新地址验证冷静期时长（线程安全）。
func (l *Ledger) AddressVerifyPeriod() time.Duration {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.addressVerifyPeriod
}

// addrBookKey 生成白名单内层索引 key（asset|chain|address）。
func addrBookKey(asset, chain, address string) string {
	return asset + "|" + chain + "|" + address
}

// AddWithdrawAddress 预登记一条提现地址：默认未验证，且须度过验证冷静期(VerifyUntil)后方可
// 用于提现。地址已存在则返回错误。返回登记后的条目（含 VerifyUntil）供上层展示。
func (l *Ledger) AddWithdrawAddress(userID int64, asset, chain, address, label string) (*WithdrawAddress, error) {
	if userID <= 0 || asset == "" || chain == "" || address == "" {
		return nil, fmt.Errorf("invalid withdraw address params")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.addWithdrawAddressLocked(userID, asset, chain, address, label)
}

// addWithdrawAddressLocked 是 AddWithdrawAddress 的无锁版本，调用方须已持 l.mu。
func (l *Ledger) addWithdrawAddressLocked(userID int64, asset, chain, address, label string) (*WithdrawAddress, error) {
	inner, ok := l.withdrawAddressBook[userID]
	if !ok {
		inner = make(map[string]*WithdrawAddress)
		l.withdrawAddressBook[userID] = inner
	}
	k := addrBookKey(asset, chain, address)
	if _, exists := inner[k]; exists {
		return nil, fmt.Errorf("withdraw address already registered")
	}
	now := time.Now()
	e := &WithdrawAddress{
		UserID:      userID,
		Asset:       asset,
		Chain:       chain,
		Address:     address,
		Label:       label,
		AddedAt:     now,
		Verified:    false,
		VerifyUntil: now.Add(l.addressVerifyPeriod),
	}
	inner[k] = e
	// 风控引擎：记录新增地址活动并判定地址突增，触发则自动全局冻结（副作用，
	// 不改变本方法"地址已登记"的语义；冻结状态可由 /metrics 与 /risk/events 观测）。
	l.evaluateAddressRiskLocked(userID)
	cp := *e
	return &cp, nil
}

// ConfirmWithdrawAddress 标记一条预登记地址为已验证（模拟 2FA/邮件验证通过）。验证后地址
// 仍须等待验证冷静期(VerifyUntil)方可首次提现。地址不存在返回错误。
func (l *Ledger) ConfirmWithdrawAddress(userID int64, asset, chain, address string) error {
	if userID <= 0 || asset == "" || chain == "" || address == "" {
		return fmt.Errorf("invalid withdraw address params")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	inner, ok := l.withdrawAddressBook[userID]
	if !ok {
		return fmt.Errorf("withdraw address not found")
	}
	e, ok := inner[addrBookKey(asset, chain, address)]
	if !ok {
		return fmt.Errorf("withdraw address not found")
	}
	e.Verified = true
	return nil
}

// RemoveWithdrawAddress 撤销一条已登记地址（用户主动移除/风控拉黑）。地址不存在返回错误。
func (l *Ledger) RemoveWithdrawAddress(userID int64, asset, chain, address string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	inner, ok := l.withdrawAddressBook[userID]
	if !ok {
		return fmt.Errorf("withdraw address not found")
	}
	k := addrBookKey(asset, chain, address)
	if _, ok := inner[k]; !ok {
		return fmt.Errorf("withdraw address not found")
	}
	delete(inner, k)
	if len(inner) == 0 {
		delete(l.withdrawAddressBook, userID)
	}
	return nil
}

// ListWithdrawAddresses 列出白名单地址（可按 userID 过滤，0 表示全部）。
func (l *Ledger) ListWithdrawAddresses(userID int64) []WithdrawAddress {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]WithdrawAddress, 0)
	for uid, inner := range l.withdrawAddressBook {
		if userID != 0 && uid != userID {
			continue
		}
		for _, e := range inner {
			out = append(out, *e)
		}
	}
	return out
}

// WithdrawAddressCount 返回白名单地址总数（线程安全），用于监控指标。
func (l *Ledger) WithdrawAddressCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	n := 0
	for _, inner := range l.withdrawAddressBook {
		n += len(inner)
	}
	return n
}

// isWithdrawAddressAllowedLocked 在已持锁时判断某地址是否已可用于提现（已登记+已验证+过
// 验证冷静期）。调用方须在 RequestWithdrawHold 等持锁上下文中复用，避免重复加锁死锁。
func (l *Ledger) isWithdrawAddressAllowedLocked(userID int64, asset, chain, address string) bool {
	inner, ok := l.withdrawAddressBook[userID]
	if !ok {
		return false
	}
	e, ok := inner[addrBookKey(asset, chain, address)]
	if !ok {
		return false
	}
	return e.Verified && !time.Now().Before(e.VerifyUntil)
}

// SetDailyWithdrawLimit 设置某资产单用户每日提现限额（含手续费），limit<=0 表示不限制。
func (l *Ledger) SetDailyWithdrawLimit(asset string, limit float64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if limit <= 0 {
		delete(l.dailyWithdrawLimit, asset)
		return
	}
	l.dailyWithdrawLimit[asset] = limit
}

// SetGlobalWithdrawalFreeze 设置/解除全局紧急冻结（运维异常时一键冻结所有出金受理）。
func (l *Ledger) SetGlobalWithdrawalFreeze(frozen bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.withdrawalFrozenGlobal = frozen
}

// IsGlobalWithdrawalFrozen 返回全局紧急冻结状态（线程安全）。
func (l *Ledger) IsGlobalWithdrawalFrozen() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.withdrawalFrozenGlobal
}

// --- 可疑行为风控引擎 -------------------------------------------------------
// 把"手动全局冻结 / 白名单"升级为自动风控：检测到提现速率骤增、短时间大量新增地址等
// 可疑行为即自动全局冻结并留痕，与提现冷静期/白名单形成闭环。

// EnableRiskEngine 开启/关闭风控引擎，并可配置触发高危规则时是否自动全局冻结。
func (l *Ledger) EnableRiskEngine(enabled, autoFreeze bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.riskEnabled = enabled
	l.riskAutoFreeze = autoFreeze
}

// IsRiskEngineEnabled 返回风控引擎开关状态。
func (l *Ledger) IsRiskEngineEnabled() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.riskEnabled
}

// SetRiskThresholds 配置滑动窗口与各阈值：window 为行为计数时间范围；velocityAmount 为
// 窗口内单用户提现累计额阈值（跨资产合计）；velocityCount 为窗口内提现请求次数阈值；
// addrBurstCount 为窗口内新增地址数阈值。任一阈值<=0 表示该项不触发（仅记录）。
func (l *Ledger) SetRiskThresholds(window time.Duration, velocityAmount float64, velocityCount, addrBurstCount int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.riskWindow = window
	l.riskVelocityAmount = velocityAmount
	l.riskVelocityCount = velocityCount
	l.riskAddrBurstCount = addrBurstCount
}

// ListRiskEvents 返回风控事件（按 userID 过滤，0 表示全部），最新在前。
func (l *Ledger) ListRiskEvents(userID int64) []RiskEvent {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]RiskEvent, 0, len(l.riskEvents))
	for i := len(l.riskEvents) - 1; i >= 0; i-- {
		e := l.riskEvents[i]
		if userID != 0 && e.UserID != userID {
			continue
		}
		out = append(out, *e)
	}
	return out
}

// RiskEventCount 返回累计风控事件数（线程安全），用于监控指标。
func (l *Ledger) RiskEventCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return len(l.riskEvents)
}

// AutoFrozenByRisk 返回当前全局冻结是否由风控引擎自动触发（人工 resume 后清零）。
func (l *Ledger) AutoFrozenByRisk() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.autoFrozenByRisk
}

// ClearRiskAutoFreeze 在人工解除全局冻结后调用，清零"自动冻结"标记（仅清标记，不改动冻结状态本身）。
func (l *Ledger) ClearRiskAutoFreeze() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.autoFrozenByRisk = false
}

// raiseRiskEventLocked 在已持锁时记录一条风控事件；若开启自动冻结且尚未自动冻结，
// 则触发全局紧急冻结并标注 action=auto_global_freeze。调用方须已持 l.mu。
func (l *Ledger) raiseRiskEventLocked(userID int64, typ, severity, msg string) {
	l.riskEventSeq++
	e := &RiskEvent{
		ID:          fmt.Sprintf("rk-%d", l.riskEventSeq),
		Type:        typ,
		UserID:      userID,
		Severity:    severity,
		Message:     msg,
		TriggeredAt: time.Now(),
	}
	if l.riskAutoFreeze && !l.autoFrozenByRisk {
		l.withdrawalFrozenGlobal = true
		l.autoFrozenByRisk = true
		e.Action = "auto_global_freeze"
	} else {
		e.Action = "logged"
	}
	l.riskEvents = append(l.riskEvents, e)
}

// evaluateWithdrawRiskLocked 在已持锁时记录一次提现活动并判断是否触发提现速率规则。
// 返回 true 表示触发高危规则（已自动冻结）。调用方须已持 l.mu。
func (l *Ledger) evaluateWithdrawRiskLocked(userID int64, amount float64) bool {
	if !l.riskEnabled {
		return false
	}
	now := time.Now()
	l.riskWithdrawActivity[userID] = append(l.riskWithdrawActivity[userID], riskAct{at: now, amount: amount})
	// 滑动窗口裁剪 + 统计。
	cutoff := now.Add(-l.riskWindow)
	acts := l.riskWithdrawActivity[userID]
	kept := acts[:0]
	sum, cnt := 0.0, 0
	for _, a := range acts {
		if a.at.After(cutoff) {
			kept = append(kept, a)
			sum += a.amount
			cnt++
		}
	}
	l.riskWithdrawActivity[userID] = kept
	if (l.riskVelocityAmount > 0 && sum >= l.riskVelocityAmount-1e-9) ||
		(l.riskVelocityCount > 0 && cnt >= l.riskVelocityCount) {
		l.raiseRiskEventLocked(userID, "withdraw_velocity", "high",
			fmt.Sprintf("withdraw velocity: %d requests / %.2f within %s", cnt, sum, l.riskWindow))
		return true
	}
	return false
}

// evaluateAddressRiskLocked 在已持锁时记录一次新增地址活动并判断是否触发地址突增规则。
// 返回 true 表示触发高危规则（已自动冻结）。调用方须已持 l.mu。
func (l *Ledger) evaluateAddressRiskLocked(userID int64) bool {
	if !l.riskEnabled {
		return false
	}
	now := time.Now()
	l.riskAddrActivity[userID] = append(l.riskAddrActivity[userID], now)
	cutoff := now.Add(-l.riskWindow)
	acts := l.riskAddrActivity[userID]
	kept := acts[:0]
	cnt := 0
	for _, t := range acts {
		if t.After(cutoff) {
			kept = append(kept, t)
			cnt++
		}
	}
	l.riskAddrActivity[userID] = kept
	if l.riskAddrBurstCount > 0 && cnt >= l.riskAddrBurstCount {
		l.raiseRiskEventLocked(userID, "address_burst", "high",
			fmt.Sprintf("new address burst: %d within %s", cnt, l.riskWindow))
		return true
	}
	return false
}

// dailyKey 生成当日累计限额的 key（按用户-资产-日期维度）。
func withdrawDailyKey(userID int64, asset, date string) string {
	return fmt.Sprintf("%d:%s:%s", userID, asset, date)
}

// RequestWithdrawHold 受理一笔提现请求进入冷静期：风控校验（全局冻结/坏账限制/余额/每日限额）
// 通过后冻结资金并入队，返回 holdID 与 HoldUntil。冷静期内不可链上清算。
func (l *Ledger) RequestWithdrawHold(userID int64, asset string, amount, fee float64, chain, address string) (id string, holdUntil time.Time, err error) {
	if amount < 0 || fee < 0 {
		return "", time.Time{}, fmt.Errorf("withdraw amount/fee must be >= 0")
	}
	total := amount + fee
	if total <= 0 {
		return "", time.Time{}, fmt.Errorf("withdraw total must be > 0")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// 全局紧急冻结：拦截所有出金受理。
	if l.withdrawalFrozenGlobal {
		return "", time.Time{}, fmt.Errorf("global withdrawal freeze active")
	}
	// 坏账风控：有未冲抵坏账禁止出金。
	if l.restricted[l.key(userID, asset)] {
		return "", time.Time{}, fmt.Errorf("outflow restricted: repay outstanding bad debt first")
	}
	a := l.getOrCreateLocked(userID, asset)
	if a.Available < total-1e-9 {
		return "", time.Time{}, fmt.Errorf("insufficient available balance")
	}
	// 提现地址白名单：出金地址须预登记、已验证且度过验证冷静期，否则拒绝受理
	// （防钓鱼/未授权地址盗提；与提现冷静期互补形成"新地址+新提现"双重延时）。
	if !l.isWithdrawAddressAllowedLocked(userID, asset, chain, address) {
		return "", time.Time{}, fmt.Errorf("withdrawal address not whitelisted/verified")
	}
	// 风控引擎：记录提现活动并判定提现速率，触发则自动全局冻结并拒绝本次受理
	// （与 #18 全局冻结互补——把手动阀门升级为自动风控闭环）。
	if l.evaluateWithdrawRiskLocked(userID, total) {
		return "", time.Time{}, fmt.Errorf("suspicious withdraw activity detected: global freeze engaged")
	}
	// 每日限额：预占当日额度，避免并发超额。
	if limit, ok := l.dailyWithdrawLimit[asset]; ok && limit > 0 {
		today := time.Now().UTC().Format("2006-01-02")
		k := withdrawDailyKey(userID, asset, today)
		if l.dailyWithdrawUsed[k]+total > limit+1e-9 {
			return "", time.Time{}, fmt.Errorf("daily withdrawal limit exceeded")
		}
		l.dailyWithdrawUsed[k] += total
	}
	// 冻结资金（离开可用，进入提现冻结），尚未链上划出。
	if err := l.freezeWithdrawLocked(userID, asset, total); err != nil {
		// 冻结失败需回退已预占的当日额度。
		if limit, ok := l.dailyWithdrawLimit[asset]; ok && limit > 0 {
			today := time.Now().UTC().Format("2006-01-02")
			k := withdrawDailyKey(userID, asset, today)
			l.dailyWithdrawUsed[k] -= total
		}
		return "", time.Time{}, err
	}
	l.withdrawHoldSeq++
	now := time.Now()
	e := &WithdrawHoldEntry{
		ID:        fmt.Sprintf("wh-%d", l.withdrawHoldSeq),
		UserID:    userID,
		Asset:     asset,
		Amount:    amount,
		Fee:       fee,
		Chain:     chain,
		Address:   address,
		CreatedAt: now,
		HoldUntil: now.Add(l.withdrawHoldPeriod),
	}
	l.withdrawHolds[e.ID] = e
	return e.ID, e.HoldUntil, nil
}

// FinalizeWithdrawHold 冷静期满后清算一笔提现：真正链上划出（SettleWithdraw），
// 返回对应 entry 供上层广播。冷静期内或已终态则返回错误。
func (l *Ledger) FinalizeWithdrawHold(id string) (*WithdrawHoldEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.withdrawHolds[id]
	if !ok {
		return nil, fmt.Errorf("withdraw hold not found")
	}
	if e.Finalized {
		return nil, fmt.Errorf("withdraw hold already finalized")
	}
	if e.Cancelled {
		return nil, fmt.Errorf("withdraw hold cancelled")
	}
	if time.Now().Before(e.HoldUntil) {
		return nil, fmt.Errorf("withdraw hold in cooling period until %s", e.HoldUntil.Format(time.RFC3339))
	}
	// 链上划出：提现冻结 -> 离开系统（借记负债账户）。
	if err := l.settleWithdrawLocked(e.UserID, e.Asset, e.Amount, e.Fee, e.ID); err != nil {
		return nil, err
	}
	e.Finalized = true
	return e, nil
}

// FinalizeWithdrawHoldForce 管理员审批放行专用：跳过冷静期直接清算，
// 用于管理后台显式授权提现（冷却期是防用户误操作的，不适用于审批场景）。
// 与 FinalizeWithdrawHold 的区别仅在于不做冷静期守卫。
func (l *Ledger) FinalizeWithdrawHoldForce(id string) (*WithdrawHoldEntry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.withdrawHolds[id]
	if !ok {
		return nil, fmt.Errorf("withdraw hold not found")
	}
	if e.Finalized {
		return nil, fmt.Errorf("withdraw hold already finalized")
	}
	if e.Cancelled {
		return nil, fmt.Errorf("withdraw hold cancelled")
	}
	if err := l.settleWithdrawLocked(e.UserID, e.Asset, e.Amount, e.Fee, e.ID); err != nil {
		return nil, err
	}
	e.Finalized = true
	return e, nil
}

// CancelWithdrawHold 撤销一笔未清算的提现：退回冻结资金到可用，并退还当日预占额度。
func (l *Ledger) CancelWithdrawHold(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.withdrawHolds[id]
	if !ok {
		return fmt.Errorf("withdraw hold not found")
	}
	if e.Finalized {
		return fmt.Errorf("withdraw hold already finalized")
	}
	if e.Cancelled {
		return fmt.Errorf("withdraw hold already cancelled")
	}
	total := e.Amount + e.Fee
	if err := l.unfreezeWithdrawLocked(e.UserID, e.Asset, total); err != nil {
		return err
	}
	// 退还当日预占额度。
	if limit, ok := l.dailyWithdrawLimit[e.Asset]; ok && limit > 0 {
		today := time.Now().UTC().Format("2006-01-02")
		k := withdrawDailyKey(e.UserID, e.Asset, today)
		l.dailyWithdrawUsed[k] -= total
		if l.dailyWithdrawUsed[k] < -1e-9 {
			l.dailyWithdrawUsed[k] = 0
		}
	}
	e.Cancelled = true
	return nil
}

// ClaimWithdrawBroadcast 在链上广播前原子地占有 hold 的广播槽，消除 finalizeHold「先广播后终态」
// 的 TOCTOU 与失败重试导致的重复链上广播（F1）。在 ledger 锁内完成，返回：
//   - 已终态/取消：error（与 Finalize 守卫一致）；
//   - 已广播过（重试/并发）：返回既有 TxHash 且 already=true，上层据此跳过 SubmitWithdraw 复用；
//   - 首次广播：标记 Broadcasted=true 并返回 already=false，上层随后 SubmitWithdraw 并回填 TxHash。
func (l *Ledger) ClaimWithdrawBroadcast(id string) (txHash string, already bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.withdrawHolds[id]
	if !ok {
		return "", false, fmt.Errorf("withdraw hold not found")
	}
	if e.Finalized {
		return "", false, fmt.Errorf("withdraw hold already finalized")
	}
	if e.Cancelled {
		return "", false, fmt.Errorf("withdraw hold cancelled")
	}
	if e.Broadcasted {
		return e.TxHash, true, nil
	}
	e.Broadcasted = true
	return "", false, nil
}

// SetWithdrawTxHash 广播成功后回填链上 txHash 并固化 Broadcasted（与 ClaimWithdrawBroadcast 配对）。
func (l *Ledger) SetWithdrawTxHash(id, txHash string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.withdrawHolds[id]
	if !ok {
		return fmt.Errorf("withdraw hold not found")
	}
	e.TxHash = txHash
	e.Broadcasted = true
	return nil
}

// ResetWithdrawBroadcast 广播失败（SubmitWithdraw 返回 error）时释放广播槽，
// 允许上层重试重新广播，避免「Broadcasted 已置位但无 txHash」卡死（F1）。
func (l *Ledger) ResetWithdrawBroadcast(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.withdrawHolds[id]
	if !ok {
		return fmt.Errorf("withdraw hold not found")
	}
	if e.Finalized || e.Cancelled {
		return nil
	}
	e.Broadcasted = false
	return nil
}

// WithdrawHold 返回指定 holdID 的提现请求（线程安全）。
func (l *Ledger) WithdrawHold(id string) (*WithdrawHoldEntry, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	e, ok := l.withdrawHolds[id]
	if !ok {
		return nil, false
	}
	cp := *e
	return &cp, true
}

// ListWithdrawHolds 列出提现请求队列（可按 userID 过滤，0 表示全部）。
func (l *Ledger) ListWithdrawHolds(userID int64) []WithdrawHoldEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]WithdrawHoldEntry, 0, len(l.withdrawHolds))
	for _, e := range l.withdrawHolds {
		if userID != 0 && e.UserID != userID {
			continue
		}
		out = append(out, *e)
	}
	return out
}

// PendingWithdrawHoldCount 返回尚未清算/撤销的提现请求数（线程安全），用于监控指标。
func (l *Ledger) PendingWithdrawHoldCount() int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	n := 0
	for _, e := range l.withdrawHolds {
		if !e.Finalized && !e.Cancelled {
			n++
		}
	}
	return n
}

// --- 内部辅助 ---

// freezeWithdrawLocked / unfreezeWithdrawLocked / settleWithdrawLocked 是 FreezeWithdraw /
// UnfreezeWithdraw / SettleWithdraw 的内部无锁版本，调用方须已持 l.mu。供提现冷静期等
// 已在持锁上下文中的逻辑复用，避免重复加锁死锁。
func (l *Ledger) freezeWithdrawLocked(userID int64, asset string, amount float64) error {
	if amount < 0 {
		return fmt.Errorf("freeze withdraw amount must be >= 0")
	}
	a := l.getOrCreateLocked(userID, asset)
	if a.Available < amount-1e-9 {
		return fmt.Errorf("insufficient available balance: have %.8f want %.8f", a.Available, amount)
	}
	a.Available -= amount
	a.WithdrawFrozen += amount
	return nil
}

func (l *Ledger) unfreezeWithdrawLocked(userID int64, asset string, amount float64) error {
	if amount < 0 {
		return fmt.Errorf("unfreeze withdraw amount must be >= 0")
	}
	a := l.getOrCreateLocked(userID, asset)
	if a.WithdrawFrozen < amount-1e-9 {
		return fmt.Errorf("insufficient withdraw frozen balance")
	}
	a.WithdrawFrozen -= amount
	a.Available += amount
	return nil
}

func (l *Ledger) settleWithdrawLocked(userID int64, asset string, amount, fee float64, txHash string) error {
	total := amount + fee
	if total < 0 {
		return fmt.Errorf("withdraw amount must be >= 0")
	}
	a := l.getOrCreateLocked(userID, asset)
	if a.WithdrawFrozen < total-1e-9 {
		return fmt.Errorf("insufficient withdraw frozen balance: have %.8f want %.8f", a.WithdrawFrozen, total)
	}
	a.WithdrawFrozen -= total
	sys := l.getOrCreateLocked(SysChainClearing, asset)
	sys.Available += total
	ref := "chain:" + txHash
	l.appendSideLocked(a, -total, "chain_withdraw", ref, &l.seq)
	l.appendSideLocked(sys, +total, "chain_withdraw", ref, &l.seq)
	l.hotWallet[asset] -= total
	return nil
}

func (l *Ledger) getOrCreateLocked(userID int64, asset string) *Account {
	k := l.key(userID, asset)
	a, ok := l.accounts[k]
	if !ok {
		a = &Account{UserID: userID, Asset: asset}
		l.accounts[k] = a
	}
	return a
}

// appendLocked 在已持有锁时追加流水（使用账户最新可用余额）。
func (l *Ledger) appendLocked(a *Account, delta float64, biz, ref string) {
	l.appendSideLocked(a, delta, biz, ref, &l.seq)
}

func (l *Ledger) appendSideLocked(a *Account, delta float64, biz, ref string, seq *int64) {
	*seq++
	l.log = append(l.log, Entry{
		ID:      *seq,
		UserID:  a.UserID,
		Asset:   a.Asset,
		Delta:   delta,
		Balance: a.Available,
		BizType: biz,
		Ref:     ref,
		Time:    time.Now().UnixNano(),
	})
}

// Reconcile 复式记账对账：返回每个资产下所有账户（含系统负债账户）权益总和的偏差。
//
// 复式记账恒等要求该偏差为 0——每一笔资金流动都同时记录借贷两侧，使"交易所对用户的总
// 资产"（用户 Available+Frozen+WithdrawFrozen 之和）恰好等于系统负债账户余额的相反数，
// 二者相加恒为 0。任何偏离 0 都意味着账本出现未配对记账（凭空铸币、丢失流水、借贷不平衡），
// 属资金安全事故。
//
// 生产资金流动（ReceiveOnChain / SettleWithdraw / ReverseOnChain / ReverseWithdraw /
// Transfer / SocializeBadDebt / Freeze / FreezeWithdraw 等）均严格复式配对，偏差恒为 0。
// 注意：演示用 Deposit/CreditAvailable 凭空铸币不配对系统负债，会引入非零偏差——生产
// 充值必须走 ReceiveOnChain，对账时应以生产路径为准。返回 map 的 value 为偏差（接近 0 即平衡）。
func (l *Ledger) Reconcile() map[string]float64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[string]float64)
	for _, a := range l.accounts {
		out[a.Asset] += a.Available + a.Frozen + a.WithdrawFrozen
	}
	for k, v := range out {
		if math.Abs(v) < 1e-9 {
			out[k] = 0 // 消除浮点残差，便于精确比较
		}
	}
	return out
}

// IsBalanced 返回账本是否全局借贷平衡（所有资产偏差均在容差 1e-6 内）。
// 可作为运行时对账探针（如定时任务、HTTP /wallet/reconcile）与测试断言使用。
func (l *Ledger) IsBalanced() bool {
	for _, v := range l.Reconcile() {
		if math.Abs(v) > 1e-6 {
			return false
		}
	}
	return true
}

// SetReconcileAlertHook 设置不平账告警回调：当定时巡检探测到借贷偏差超容差时异步调用，
// 入参为各资产偏差 map。生产可在此推送监控指标/告警平台；传 nil 关闭。
func (l *Ledger) SetReconcileAlertHook(fn func(map[string]float64)) {
	l.reconMu.Lock()
	defer l.reconMu.Unlock()
	l.alertHook = fn
}

// StartReconciler 启动后台对账巡检：每隔 interval 调用一次 Reconcile，更新最近巡检结果，
// 并在探测到不平账时触发告警回调。幂等：重复调用不会启动多个 goroutine。
func (l *Ledger) StartReconciler(interval time.Duration) {
	l.reconMu.Lock()
	if l.reconRunning {
		l.reconMu.Unlock()
		return
	}
	l.stopRecon = make(chan struct{})
	stop := l.stopRecon
	l.reconRunning = true
	l.reconMu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				l.RunReconcileOnce()
			}
		}
	}()
}

// StopReconciler 停止后台对账巡检（幂等）。
func (l *Ledger) StopReconciler() {
	l.reconMu.Lock()
	defer l.reconMu.Unlock()
	if l.reconRunning {
		close(l.stopRecon)
		l.reconRunning = false
	}
}

// RunReconcileOnce 执行一次对账巡检：计算偏差、更新最近结果、不平账时触发告警回调。
// 既供后台 goroutine 调用，也可由调用方主动触发（如运维手动巡检）。返回本次巡检快照。
func (l *Ledger) RunReconcileOnce() ReconStats {
	dev := l.Reconcile()
	balanced := true
	for _, v := range dev {
		if math.Abs(v) > 1e-6 {
			balanced = false
			break
		}
	}

	l.reconMu.Lock()
	l.reconStats.LastDeviation = dev
	l.reconStats.LastBalanced = balanced
	l.reconStats.LastRun = time.Now()
	triggered := false
	if !balanced {
		l.reconStats.ImbalanceCount++
		triggered = l.alertHook != nil
	}
	l.reconMu.Unlock()

	// 在锁外触发告警回调，避免回调内再次请求账本导致死锁。
	if triggered {
		l.alertHook(dev)
	}
	return l.LastReconcile()
}

// LastReconcile 返回最近一次对账巡检的快照副本（线程安全）。
func (l *Ledger) LastReconcile() ReconStats {
	l.reconMu.RLock()
	defer l.reconMu.RUnlock()
	cp := l.reconStats
	if cp.LastDeviation != nil {
		cp.LastDeviation = make(map[string]float64, len(l.reconStats.LastDeviation))
		for k, v := range l.reconStats.LastDeviation {
			cp.LastDeviation[k] = v
		}
	}
	return cp
}

// LedgerSnapshot 是可序列化的账本状态，用于持久化（进程重启后恢复资金安全状态）。
//
// 持久化动机：账本此前为纯内存态，进程崩溃/重启会丢失余额、坏账归属、出金限制与治理
// 提案——意味着坏账限制被"遗忘"、坏账可再次被挪用，资金安全闭环断裂。通过 Snapshot
// 把全部关键状态落盘，重启时用 Restore 重建，保证安全不变量跨进程生命周期连续。
//
// 注意：仅持久化资金安全强相关状态（账户余额、两类冻结、坏账归属、出金限制、治理提案、
// 流水与序列号）；对账巡检的运行时统计（reconStats/告警计数）属瞬时监控态，重启后重新
// 累积，无需恢复。链上充值/提现网关的 pending 事件同样不持久化——真实环境由区块链重新
// 确认入账，Mock 网关重启后窗口自然清空。
type LedgerSnapshot struct {
	Accounts               []*Account                   `json:"accounts"`                 // 全部账户（含系统账户）
	Restricted             []string                     `json:"restricted"`               // 处于出金限制的用户-资产 key 列表
	BadDebtByUser          map[string]float64           `json:"bad_debt_by_user"`         // 坏账归属：key=userID:asset -> 未冲抵坏账额
	SocializeProposals     map[string]SocializeProposal `json:"socialize_proposals"`      // 待审批的社会化分摊治理提案
	HotWallet              map[string]float64           `json:"hot_wallet"`               // 热钱包链上库存（每资产）
	ColdWallet             map[string]float64           `json:"cold_wallet"`              // 冷钱包链上库存（每资产）
	HotWalletCap           map[string]float64           `json:"hot_wallet_cap"`           // 热钱包风险敞口上限（每资产）
	WithdrawHolds          []*WithdrawHoldEntry         `json:"withdraw_holds"`           // 处于冷静期的提现请求队列
	WithdrawHoldPeriod     time.Duration                `json:"withdraw_hold_period"`     // 冷静期时长
	WithdrawHoldSeq        int64                        `json:"withdraw_hold_seq"`        // 提现请求序列号
	WithdrawalFrozenGlobal bool                         `json:"withdrawal_frozen_global"` // 全局紧急冻结开关
	DailyWithdrawLimit     map[string]float64           `json:"daily_withdraw_limit"`     // 每日提现限额（每资产）
	DailyWithdrawUsed      map[string]float64           `json:"daily_withdraw_used"`      // 当日已用提现额度（按 uid:asset:date）
	WithdrawAddresses      []*WithdrawAddress           `json:"withdraw_addresses"`       // 提现地址白名单（防钓鱼/未授权盗提）
	AddressVerifyPeriod    time.Duration                `json:"address_verify_period"`    // 新地址验证冷静期时长
	RiskEvents             []*RiskEvent                 `json:"risk_events"`              // 风控事件审计轨迹
	RiskEventSeq           int64                        `json:"risk_event_seq"`           // 风控事件序列号
	AutoFrozenByRisk       bool                         `json:"auto_frozen_by_risk"`      // 当前全局冻结是否由风控自动触发
	Log                    []Entry                      `json:"log"`                      // 资金流水（审计/对账溯源）
	Seq                    int64                        `json:"seq"`                      // 流水序列号（恢复后续写不冲突）
}

// Snapshot 生成账本当前状态的可序列化副本（线程安全，持读锁）。
func (l *Ledger) Snapshot() LedgerSnapshot {
	l.mu.RLock()
	defer l.mu.RUnlock()
	snap := LedgerSnapshot{
		BadDebtByUser:      make(map[string]float64, len(l.badDebtByUser)),
		SocializeProposals: make(map[string]SocializeProposal, len(l.socializeProposals)),
		HotWallet:          make(map[string]float64, len(l.hotWallet)),
		ColdWallet:         make(map[string]float64, len(l.coldWallet)),
		HotWalletCap:       make(map[string]float64, len(l.hotWalletCap)),
		DailyWithdrawLimit: make(map[string]float64, len(l.dailyWithdrawLimit)),
		DailyWithdrawUsed:  make(map[string]float64, len(l.dailyWithdrawUsed)),
		Log:                make([]Entry, len(l.log)),
		Seq:                l.seq,
	}
	for _, a := range l.accounts {
		snap.Accounts = append(snap.Accounts, &Account{
			UserID:         a.UserID,
			Asset:          a.Asset,
			Available:      a.Available,
			Frozen:         a.Frozen,
			WithdrawFrozen: a.WithdrawFrozen,
		})
	}
	for k := range l.restricted {
		snap.Restricted = append(snap.Restricted, k)
	}
	for k, v := range l.badDebtByUser {
		snap.BadDebtByUser[k] = v
	}
	for k, p := range l.socializeProposals {
		snap.SocializeProposals[k] = *p
	}
	for k, v := range l.hotWallet {
		snap.HotWallet[k] = v
	}
	for k, v := range l.coldWallet {
		snap.ColdWallet[k] = v
	}
	for k, v := range l.hotWalletCap {
		snap.HotWalletCap[k] = v
	}
	// 提现冷静期状态：深拷贝每条 entry（避免恢复后与原账本共享指针）。
	snap.WithdrawHolds = make([]*WithdrawHoldEntry, 0, len(l.withdrawHolds))
	for _, e := range l.withdrawHolds {
		cp := *e
		snap.WithdrawHolds = append(snap.WithdrawHolds, &cp)
	}
	snap.WithdrawHoldPeriod = l.withdrawHoldPeriod
	snap.WithdrawHoldSeq = l.withdrawHoldSeq
	snap.WithdrawalFrozenGlobal = l.withdrawalFrozenGlobal
	for k, v := range l.dailyWithdrawLimit {
		snap.DailyWithdrawLimit[k] = v
	}
	for k, v := range l.dailyWithdrawUsed {
		snap.DailyWithdrawUsed[k] = v
	}
	// 提现地址白名单：深拷贝每条条目（避免恢复后与原账本共享指针）。
	snap.WithdrawAddresses = make([]*WithdrawAddress, 0, l.WithdrawAddressCount())
	for _, inner := range l.withdrawAddressBook {
		for _, e := range inner {
			cp := *e
			snap.WithdrawAddresses = append(snap.WithdrawAddresses, &cp)
		}
	}
	snap.AddressVerifyPeriod = l.addressVerifyPeriod
	// 风控事件审计轨迹：深拷贝（避免恢复后共享指针）。
	snap.RiskEvents = make([]*RiskEvent, 0, len(l.riskEvents))
	for _, e := range l.riskEvents {
		cp := *e
		snap.RiskEvents = append(snap.RiskEvents, &cp)
	}
	snap.RiskEventSeq = l.riskEventSeq
	snap.AutoFrozenByRisk = l.autoFrozenByRisk
	copy(snap.Log, l.log)
	return snap
}

// Restore 用快照重建账本状态（线程安全，持写锁）。会整体替换账户、限制、坏账归属、提案与
// 流水，并恢复序列号，使恢复后的账本在对账、坏账回收与治理流程上与重启前行为一致。
func (l *Ledger) Restore(snap LedgerSnapshot) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.accounts = make(map[string]*Account, len(snap.Accounts))
	for _, a := range snap.Accounts {
		if a == nil {
			continue
		}
		l.accounts[l.key(a.UserID, a.Asset)] = &Account{
			UserID:         a.UserID,
			Asset:          a.Asset,
			Available:      a.Available,
			Frozen:         a.Frozen,
			WithdrawFrozen: a.WithdrawFrozen,
		}
	}
	l.restricted = make(map[string]bool, len(snap.Restricted))
	for _, k := range snap.Restricted {
		l.restricted[k] = true
	}
	l.badDebtByUser = make(map[string]float64, len(snap.BadDebtByUser))
	for k, v := range snap.BadDebtByUser {
		l.badDebtByUser[k] = v
	}
	l.socializeProposals = make(map[string]*SocializeProposal, len(snap.SocializeProposals))
	for k, p := range snap.SocializeProposals {
		clone := p
		l.socializeProposals[k] = &clone
	}
	// 链上钱包库存与上限：清空后从快照重建（避免残留旧 key）。
	l.hotWallet = make(map[string]float64, len(snap.HotWallet))
	for k, v := range snap.HotWallet {
		l.hotWallet[k] = v
	}
	l.coldWallet = make(map[string]float64, len(snap.ColdWallet))
	for k, v := range snap.ColdWallet {
		l.coldWallet[k] = v
	}
	l.hotWalletCap = make(map[string]float64, len(snap.HotWalletCap))
	for k, v := range snap.HotWalletCap {
		l.hotWalletCap[k] = v
	}
	// 提现冷静期状态：清空后从快照重建。
	l.withdrawHolds = make(map[string]*WithdrawHoldEntry, len(snap.WithdrawHolds))
	for _, e := range snap.WithdrawHolds {
		if e == nil {
			continue
		}
		cp := *e
		l.withdrawHolds[cp.ID] = &cp
	}
	l.withdrawHoldPeriod = snap.WithdrawHoldPeriod
	l.withdrawHoldSeq = snap.WithdrawHoldSeq
	l.withdrawalFrozenGlobal = snap.WithdrawalFrozenGlobal
	l.dailyWithdrawLimit = make(map[string]float64, len(snap.DailyWithdrawLimit))
	for k, v := range snap.DailyWithdrawLimit {
		l.dailyWithdrawLimit[k] = v
	}
	l.dailyWithdrawUsed = make(map[string]float64, len(snap.DailyWithdrawUsed))
	for k, v := range snap.DailyWithdrawUsed {
		l.dailyWithdrawUsed[k] = v
	}
	// 提现地址白名单：清空后从快照重建嵌套 map（避免残留旧 key）。
	l.withdrawAddressBook = make(map[int64]map[string]*WithdrawAddress, len(snap.WithdrawAddresses))
	for _, e := range snap.WithdrawAddresses {
		if e == nil {
			continue
		}
		inner, ok := l.withdrawAddressBook[e.UserID]
		if !ok {
			inner = make(map[string]*WithdrawAddress)
			l.withdrawAddressBook[e.UserID] = inner
		}
		cp := *e
		inner[addrBookKey(e.Asset, e.Chain, e.Address)] = &cp
	}
	l.addressVerifyPeriod = snap.AddressVerifyPeriod
	// 风控事件：清空后从快照重建（审计轨迹跨重启保留）。
	l.riskEvents = make([]*RiskEvent, 0, len(snap.RiskEvents))
	for _, e := range snap.RiskEvents {
		if e == nil {
			continue
		}
		cp := *e
		l.riskEvents = append(l.riskEvents, &cp)
	}
	l.riskEventSeq = snap.RiskEventSeq
	l.autoFrozenByRisk = snap.AutoFrozenByRisk
	l.log = make([]Entry, len(snap.Log))
	copy(l.log, snap.Log)
	l.seq = snap.Seq
}

// SaveToFile 把账本当前状态序列化并写入文件（atomically 通过临时文件+rename）。便于运维
// 主动触发快照、后台定时落盘或进程退出前保存。路径不可写时返回错误。
func (l *Ledger) SaveToFile(path string) error {
	snap := l.Snapshot()
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	// 先写临时文件再原子 rename，避免写一半进程崩溃留下半截文件。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write snapshot tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename snapshot: %w", err)
	}
	return nil
}

// LoadSnapshotFromFile 从文件读取并反序列化为账本快照（不修改当前账本，供调用方 Restore）。
func LoadSnapshotFromFile(path string) (LedgerSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return LedgerSnapshot{}, fmt.Errorf("read snapshot: %w", err)
	}
	var snap LedgerSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return LedgerSnapshot{}, fmt.Errorf("unmarshal snapshot: %w", err)
	}
	return snap, nil
}

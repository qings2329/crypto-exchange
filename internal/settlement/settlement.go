// Package settlement 链上清结算层：把用户在区块链上的充值（与未来提现）映射到
// 交易所内部账本（Ledger），形成"链上确认 -> 内部入账"的闭环。
//
// 生产环境需对接真实节点/RPC（ETH/BTC/Tron 等），监听充值交易、累计区块确认数、
// 达到安全确认数后入账。此处提供可插拔的 DepositGateway 接口与一个离线模拟实现
// MockChainGateway（按区块递增确认数），便于骨架演示与单测，与生产实现可无缝替换。
package settlement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// Chain 支持的链标识。
type Chain string

const (
	ChainETH  Chain = "ETH"
	ChainTRON Chain = "TRON"
	ChainBTC  Chain = "BTC"
	ChainSOL  Chain = "SOL"
)

// emitSendTimeout 是 emit 向订阅者投递事件时的最大阻塞时长。订阅者因背压长期不消费时，
// 阻塞到此上限即放弃本次投递并告警（而非静默丢弃），避免"链上已确认但用户未到账"且无迹可查。
// 已入账状态仍持久化在 g.pending，运维可经 Pending() 对账重放。
// 用 var 而非 const 以便单测调短，缩短背压路径验证耗时。
var emitSendTimeout = 5 * time.Second

// DepositStatus 充值状态机。
type DepositStatus int

const (
	// DepositPending 已广播/已上链，确认数不足。
	DepositPending DepositStatus = iota
	// DepositConfirmed 达到安全确认数（此处与 Credited 合并，确认即入账）。
	DepositConfirmed
	// DepositCredited 已清结算入账到内部账本。
	DepositCredited
	// DepositFailed 充值失败（演示预留）。
	DepositFailed
	// DepositOrphaned 已确认入账后被孤块/重组丢弃，需回滚入账。
	DepositOrphaned
)

func (s DepositStatus) String() string {
	switch s {
	case DepositPending:
		return "pending"
	case DepositConfirmed:
		return "confirmed"
	case DepositCredited:
		return "credited"
	case DepositOrphaned:
		return "orphaned"
	default:
		return "failed"
	}
}

// MarshalJSON 暴露充值事件的可读 JSON 契约（领域字段均为未导出，需显式映射）。
func (e DepositEvent) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"tx_hash":       e.TxHash,
		"user_id":       e.UserID,
		"asset":         e.Asset,
		"amount":        e.Amount,
		"chain":         string(e.Chain),
		"address":       e.Address,
		"confirmations": e.Confirmations,
		"required":      e.Required,
		"status":        e.Status.String(),
		"block_height":  e.BlockHeight,
		"created_at":    e.CreatedAt,
		"updated_at":    e.UpdatedAt,
	})
}

// DepositEvent 一笔链上充值事件（贯穿状态机生命周期）。
type DepositEvent struct {
	TxHash        string        // 链上交易哈希（幂等键）
	UserID        int64         // 目标交易所用户
	Asset         string        // 资产（如 USDT）
	Amount        AssetAmount   // 充值数量（最小单位整数，#6）
	Chain         Chain         // 来源链
	Address       string        // 充值地址（用户专属）
	Confirmations int           // 当前区块确认数
	Required      int           // 安全确认数阈值
	Status        DepositStatus // 当前状态
	BlockHeight   int           // 达 Credited 时的区块高度；-1 表示尚未最终确认
	CreatedAt     int64
	UpdatedAt     int64
}

// DepositGateway 链上充值网关（可插拔：模拟 / 真实 RPC 实现可互换）。
type DepositGateway interface {
	// SubmitDeposit 记录一笔用户充值意图，返回待确认事件（含模拟 TxHash 与地址）。
	SubmitDeposit(userID int64, asset string, chain Chain, amount AssetAmount, address string) (*DepositEvent, error)
	// SubmitDepositWithHash 与 SubmitDeposit 同义，但使用调用方提供的链上 TxHash（真实扫描入账时
	// 保留节点返回的真实哈希，保证链上幂等/对账一致；空则回退本地生成）。
	SubmitDepositWithHash(userID int64, asset string, chain Chain, amount AssetAmount, address, txHash string) (*DepositEvent, error)
	// Watch 订阅"已入账"事件流（确认达标后推送），用于驱动内部账本入账。
	Watch(ctx context.Context) (<-chan DepositEvent, error)
	// StartScan 启动链上充值监听（真实 RPC 扫描）；模拟网关为 no-op（充值仅经 SubmitDeposit 注入）。
	StartScan(ctx context.Context)
	// Start 启动后台确认循环（模拟网关）。
	Start()
	// Interval 返回区块确认间隔（演示预估到账时间用）。
	Interval() time.Duration
	// WatchRollback 订阅"孤块/重组回滚"事件流，用于驱动内部账本回拨。
	WatchRollback(ctx context.Context) (<-chan DepositEvent, error)
	// Reorg 模拟孤块/重组：根据交易当前状态采取不同动作（待确认安全回退 / 已入账推送回滚）。
	Reorg(txHash string) (*DepositEvent, error)
	// ReorgDepth 按给定深度触发批量重组回滚。
	ReorgDepth(depth int) []DepositEvent
	// Pending 返回当前所有充值事件（含待确认与已入账，供审计/查询）。
	Pending() []DepositEvent
	// Stop 停止后台确认循环。
	Stop()
}

// MockChainGateway 离线模拟链上充值网关。
// 每经过一个"区块"（interval）为所有待确认充值 +1 确认数；达 Required 后置为
// Credited 并推送到订阅者。演示与单测使用；生产替换为真实 RPC 实现即可。
type MockChainGateway struct {
	mu           sync.RWMutex
	required     int
	interval     time.Duration
	seq          int64
	pending      map[string]*DepositEvent
	subs         subscriberSet[DepositEvent]
	rollbackSubs subscriberSet[DepositEvent]
	height       int // 当前模拟区块高度，每 tick 推进；深度重组据此回退
	stop         chan struct{}
	started      bool
	// confirmSource 提供真实链上确认数（可选）。非 nil 时 tick 用节点确认数推进充值
	// 确认（替代模拟「每 tick +1」）；查询失败则回退 +1（fail-degraded）。nil=纯模拟。
	confirmSource ConfirmSource
}

// NewMockChainGateway 创建模拟网关；required<=0 默认 2 确认，interval<=0 默认 2s。
func NewMockChainGateway(required int, interval time.Duration) *MockChainGateway {
	if required <= 0 {
		required = 2
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &MockChainGateway{
		required: required,
		interval: interval,
		pending:  make(map[string]*DepositEvent),
		stop:     make(chan struct{}),
	}
}

// Start 启动后台确认循环（按区块递增确认数）。
func (g *MockChainGateway) Start() {
	g.mu.Lock()
	if g.started {
		g.mu.Unlock()
		return
	}
	g.started = true
	stop := make(chan struct{}) // 重建，避免复用 Stop 已关闭的 stop（否则再次 Start 失效、再次 Stop panic）
	g.stop = stop
	g.mu.Unlock()
	go func() {
		ticker := time.NewTicker(g.interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				g.tick()
			}
		}
	}()
}

// Stop 停止确认循环。
func (g *MockChainGateway) Stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.started {
		return
	}
	close(g.stop)
	g.started = false
}

// StartScan 模拟网关无外部扫描源，no-op（充值仅经 SubmitDeposit 注入，与改动前一致）。
func (g *MockChainGateway) StartScan(ctx context.Context) {}

// Interval 返回区块确认间隔（演示预估到账时间用）。
func (g *MockChainGateway) Interval() time.Duration {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.interval
}

// Tick 手动推进一个区块（单测/演示用，无需等待 interval）。
func (g *MockChainGateway) Tick() { g.tick() }

func (g *MockChainGateway) tick() {
	g.mu.Lock()
	now := time.Now().UnixNano()
	g.height++ // 模拟区块推进
	credited := make([]DepositEvent, 0)
	for _, ev := range g.pending {
		if ev.Status != DepositPending {
			continue
		}
		// 真实节点可达时用链上确认数推进；否则（未配置/节点宕机）回退模拟 +1。
		ev.Confirmations = nextConfirmations(g.confirmSource, context.Background(), ev.Chain, ev.TxHash, ev.Confirmations)
		ev.UpdatedAt = now
		if ev.Confirmations >= ev.Required {
			ev.Status = DepositCredited
			ev.BlockHeight = g.height // 记录最终确认所在区块高度
			g.seq++
			credited = append(credited, *ev)
		}
	}
	g.mu.Unlock()

	for _, ev := range credited {
		g.emit(ev)
	}
}

func (g *MockChainGateway) emit(ev DepositEvent) {
	g.subs.broadcast(ev, func(ev DepositEvent) {
		// 订阅者背压超时：非静默放弃，告警以便对账（状态已持久化于 g.pending）。
		log.Printf("[settlement] deposit emit DROPPED: tx=%s user=%d status=%s (subscriber backpressure)",
			ev.TxHash, ev.UserID, ev.Status)
	})
}

// SubmitDeposit 记录一笔充值意图（无真实链上哈希时回退本地生成哈希）。
// 委托 SubmitDepositWithHash(txHash="") 实现，避免与透传路径重复。
func (g *MockChainGateway) SubmitDeposit(userID int64, asset string, chain Chain, amount AssetAmount, address string) (*DepositEvent, error) {
	return g.SubmitDepositWithHash(userID, asset, chain, amount, address, "")
}

// SubmitDepositWithHash 与 SubmitDeposit 同义，但使用调用方提供的链上交易哈希（txHash）。
// 供 StartScan 在真实扫描后将节点返回的真实 txHash 透传入账（而非自生成模拟哈希），保证链上
// 幂等与对账一致；txHash 为空时回退本地生成。pending 以 txHash 为键，重复提交同一 txHash 幂等。
func (g *MockChainGateway) SubmitDepositWithHash(userID int64, asset string, chain Chain, amount AssetAmount, address, txHash string) (*DepositEvent, error) {
	if userID <= 0 || amount.Sign() <= 0 {
		return nil, fmt.Errorf("invalid deposit params")
	}
	if asset == "" {
		asset = "USDT"
	}
	if address == "" {
		address = GenerateAddress(userID, chain)
	}
	if txHash == "" {
		txHash = GenerateTxHash(userID, asset, chain, amount, time.Now().UnixNano())
	}
	g.mu.Lock()
	ev := &DepositEvent{
		TxHash:        txHash,
		UserID:        userID,
		Asset:         asset,
		Amount:        amount,
		Chain:         chain,
		Address:       address,
		Confirmations: 0,
		Required:      g.required,
		Status:        DepositPending,
		CreatedAt:     time.Now().UnixNano(),
		UpdatedAt:     time.Now().UnixNano(),
	}
	g.pending[txHash] = ev
	g.mu.Unlock()
	return ev, nil
}

// Watch 订阅已入账事件流。
func (g *MockChainGateway) Watch(ctx context.Context) (<-chan DepositEvent, error) {
	ch := make(chan DepositEvent, 64)
	g.subs.register(ch, ctx)
	return ch, nil
}

// WatchRollback 订阅"孤块/重组回滚"事件流（已入账充值被丢弃时推送），用于驱动内部账本回拨。
func (g *MockChainGateway) WatchRollback(ctx context.Context) (<-chan DepositEvent, error) {
	ch := make(chan DepositEvent, 64)
	g.rollbackSubs.register(ch, ctx)
	return ch, nil
}

// Reorg 模拟孤块/重组：根据交易当前状态采取不同动作，体现"重组窗口"语义。
//   - DepositPending（未达最终确认）：交易随短链被抛弃，安全回退到待确认（重置确认数），
//     不推送回滚、不触发内部账本回拨/坏账——因为该笔充值从未入账，用户资金不受影响。
//     这正是真实链"重组发生在安全确认窗口内"的正确处理，避免误触发坏账。
//   - DepositCredited（已达最终确认）：标记孤块丢弃并推送回滚事件，驱动内部账本 ReverseOnChain
//     （必要时垫付坏账）。
//   - DepositOrphaned：幂等，直接返回。
//
// 生产环境由真实节点的区块重组通知触发；此处供演示/单测注入孤块场景。
func (g *MockChainGateway) Reorg(txHash string) (*DepositEvent, error) {
	g.mu.Lock()
	ev, ok := g.pending[txHash]
	if !ok {
		g.mu.Unlock()
		return nil, fmt.Errorf("deposit not found: %s", txHash)
	}
	switch ev.Status {
	case DepositOrphaned:
		g.mu.Unlock()
		return ev, nil
	case DepositCredited:
		ev.Status = DepositOrphaned
		ev.UpdatedAt = time.Now().UnixNano()
		g.mu.Unlock()
		g.emitRollback(*ev)
		return ev, nil
	case DepositPending:
		// 重组窗口：未最终确认的交易安全回退，不触发回滚/坏账。
		ev.Confirmations = 0
		ev.UpdatedAt = time.Now().UnixNano()
		g.mu.Unlock()
		return ev, nil
	default:
		g.mu.Unlock()
		return nil, fmt.Errorf("deposit status %s cannot reorg", ev.Status)
	}
}

// ReorgDepth 模拟"深度重组"：回退最近 depth 个区块内所有已达最终确认（Credited）的充值，
// 标记孤块丢弃并推送回滚事件（驱动内部账本回拨/坏账）；同时把窗口内尚未最终确认的
// Pending 交易重置确认数（随短链被抛弃、重新等待确认）。height 相应回退 depth。
// 返回被回滚的交易列表。生产由真实节点的长链重组通知触发；此处供演示/单测注入场景。
func (g *MockChainGateway) ReorgDepth(depth int) []DepositEvent {
	if depth <= 0 {
		return nil
	}
	g.mu.Lock()
	now := time.Now().UnixNano()
	var rolled []DepositEvent
	cutoff := g.height - depth + 1 // 最近 depth 区块的起始高度（含）
	for _, ev := range g.pending {
		switch ev.Status {
		case DepositCredited:
			if ev.BlockHeight >= cutoff {
				ev.Status = DepositOrphaned
				ev.UpdatedAt = now
				rolled = append(rolled, *ev)
			}
		case DepositPending:
			// 窗口内未最终确认的交易随短链被抛弃，重置确认数重新等待
			ev.Confirmations = 0
			ev.UpdatedAt = now
		}
	}
	if g.height >= depth {
		g.height -= depth
	}
	g.mu.Unlock()
	for _, ev := range rolled {
		g.emitRollback(ev)
	}
	return rolled
}

func (g *MockChainGateway) emitRollback(ev DepositEvent) {
	g.rollbackSubs.broadcast(ev, func(ev DepositEvent) {
		log.Printf("[settlement] deposit rollback emit DROPPED: tx=%s user=%d status=%s (subscriber backpressure)",
			ev.TxHash, ev.UserID, ev.Status)
	})
}

// Pending 返回所有充值事件快照。
func (g *MockChainGateway) Pending() []DepositEvent {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]DepositEvent, 0, len(g.pending))
	for _, ev := range g.pending {
		out = append(out, *ev)
	}
	return out
}

// GenerateAddress 为用户生成充值地址。若已配置 HD 充值地址生成器（DepositAddressGenerator，
// 经 HSM 导出的账户级 xpub 非硬化派生），返回该用户真实的 ETH/BTC/TRON 地址；否则回退确定性的
// 模拟占位地址（fail-degraded，未配置 HD 派生时仍能运行）。派生失败（含非法 userID）同样回退 mock。
func GenerateAddress(userID int64, chain Chain) string {
	if g := depositAddrGen.Load(); g != nil {
		if addr, err := g.Address(userID, chain); err == nil && addr != "" {
			return addr
		}
		// 派生失败（未配置/非法 userID/xpub 不可用）→ 降级到 mock。
	}
	return mockDepositAddress(userID, chain)
}

// mockDepositAddress 生成确定性的模拟充值地址（未配置 HD 派生时的 fail-degraded 占位）。
func mockDepositAddress(userID int64, chain Chain) string {
	if chain == ChainSOL {
		// Solana 风格地址（ed25519 base58），即便未配置 HD 派生也给出真实形态地址。
		return deriveSolanaAddress(userID)
	}
	h := sha256.Sum256([]byte(fmt.Sprintf("%d-%s", userID, chain)))
	return fmt.Sprintf("%s_%s", chain, hex.EncodeToString(h[:])[:24])
}

// GenerateTxHash 生成确定性的模拟交易哈希。
func GenerateTxHash(userID int64, asset string, chain Chain, amount AssetAmount, nonce int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d-%s-%s-%s-%d", userID, asset, chain, amount.HumanString(), nonce)))
	return "0x" + hex.EncodeToString(h[:])
}

// ============================================================================
// 链上提现网关（与充值网关对称，补全"出入金"资金闭环的另一半）。
//
// 流程：用户提交提现 -> 交易所冻结其可用余额（复核/风控）-> 链上广播 -> 按区块
// 累计确认数 -> 达到安全确认数后"清结算"：冻结余额真正划出，交易所对用户负债
// （SysChainClearing）相应减少。若广播失败/孤块丢弃则回退冻结余额。
// 此处 MockWithdrawGateway 按区块推进状态机，生产应替换为真实 RPC 广播+确认实现。
// ============================================================================

// WithdrawStatus 提现状态机。
type WithdrawStatus int

const (
	// WithdrawPending 已受理，等待链上广播。
	WithdrawPending WithdrawStatus = iota
	// WithdrawBroadcasting 已上链，确认数累计中。
	WithdrawBroadcasting
	// WithdrawCredited 达到安全确认数，链上清结算完成（余额已划出）。
	WithdrawCredited
	// WithdrawFailed 提现失败（广播被拒），冻结余额回退。
	WithdrawFailed
	// WithdrawOrphaned 已清结算（Credited）后被孤块/重组丢弃，需回拨冻结余额。
	WithdrawOrphaned
)

func (s WithdrawStatus) String() string {
	switch s {
	case WithdrawPending:
		return "pending"
	case WithdrawBroadcasting:
		return "broadcasting"
	case WithdrawCredited:
		return "credited"
	case WithdrawOrphaned:
		return "orphaned"
	default:
		return "failed"
	}
}

// MarshalJSON 暴露提现事件的可读 JSON 契约（领域字段均为未导出，需显式映射）。
func (e WithdrawEvent) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"tx_hash":       e.TxHash,
		"user_id":       e.UserID,
		"asset":         e.Asset,
		"amount":        e.Amount,
		"fee":           e.Fee,
		"chain":         string(e.Chain),
		"address":       e.Address,
		"confirmations": e.Confirmations,
		"required":      e.Required,
		"status":        e.Status.String(),
		"block_height":  e.BlockHeight,
		"will_fail":     e.WillFail,
		"created_at":    e.CreatedAt,
		"updated_at":    e.UpdatedAt,
	})
}

// WithdrawEvent 一笔链上提现事件（贯穿状态机生命周期）。
type WithdrawEvent struct {
	TxHash        string  // 链上交易哈希（幂等键）
	UserID        int64   // 发起提现的交易所用户
	Asset         string     // 资产（如 USDT）
	Amount        AssetAmount // 提现数量（最小单位整数，不含手续费，#6）
	Fee           AssetAmount // 链上手续费（最小单位整数，#6）
	Chain         Chain       // 目标链
	Address       string  // 提现目标地址
	Confirmations int     // 当前区块确认数
	Required      int     // 安全确认数阈值
	Status        WithdrawStatus
	BlockHeight   int  // 达 Credited 时的区块高度；-1 表示尚未最终确认
	WillFail      bool // 模拟失败（演示/单测用；生产由链上实际结果决定）
	CreatedAt     int64
	UpdatedAt     int64
}

// WithdrawGateway 链上提现网关（可插拔：模拟 / 真实 RPC 实现可互换）。
// 完整契约：模拟网关（MockWithdrawGateway）与真实 RPC 网关（RPCWithdrawGateway）
// 均实现之，futures 服务按接口消费，便于生产无缝替换为真实节点广播。
type WithdrawGateway interface {
	// Start 启动后台确认循环（按区块推进提现状态机）。
	Start()
	// SubmitWithdraw 受理一笔用户提现，返回待广播事件（含模拟/真实 TxHash）。
	// willFail 仅用于演示/单测注入失败路径；生产应始终传 false，由链上结果决定成败。
	SubmitWithdraw(userID int64, asset string, chain Chain, amount, fee AssetAmount, address string, willFail bool) (*WithdrawEvent, error)
	// WatchWithdraw 订阅"清结算结果"事件流（成功/失败均推送），用于驱动内部账本。
	WatchWithdraw(ctx context.Context) (<-chan WithdrawEvent, error)
	// WatchWithdrawRollback 订阅"提现孤块/重组回滚"事件流（已清结算提现被丢弃时推送），
	// 用于驱动内部账本回拨冻结余额。
	WatchWithdrawRollback(ctx context.Context) (<-chan WithdrawEvent, error)
	// WithdrawHistory 返回全部提现事件（供审计/查询）。
	WithdrawHistory() []WithdrawEvent
	// WithdrawReorg 模拟提现孤块/重组（演示/单测注入场景）。
	WithdrawReorg(txHash string) (*WithdrawEvent, error)
	// WithdrawReorgDepth 按深度触发批量重组。
	WithdrawReorgDepth(depth int) []WithdrawEvent
	// Interval 返回确认轮询间隔（前端预估到账时间用）。
	Interval() time.Duration
	// Stop 停止后台确认循环。
	Stop()
}

// MockWithdrawGateway 离线模拟链上提现网关。
// 每经过一个"区块"（interval）：Pending->Broadcasting(确认=1)；Broadcasting 每区块
// +1 确认，达 Required 置 Credited 推送成功；WillFail 事件在首区块直接转 Failed。
type MockWithdrawGateway struct {
	mu           sync.RWMutex
	required     int
	interval     time.Duration
	seq          int64
	pending      map[string]*WithdrawEvent
	subs         subscriberSet[WithdrawEvent]
	rollbackSubs subscriberSet[WithdrawEvent]
	height       int // 当前模拟区块高度，每 tick 推进；深度重组据此回退
	stop         chan struct{}
	started      bool
	// confirmSource 提供真实链上确认数（可选）。非 nil 时 tick 在 Broadcasting 阶段用
	// 节点确认数推进（替代模拟「每 tick +1」）；查询失败回退 +1（fail-degraded）。nil=纯模拟。
	confirmSource ConfirmSource
}

// NewMockWithdrawGateway 创建模拟提现网关；required<=0 默认 2 确认，interval<=0 默认 2s。
func NewMockWithdrawGateway(required int, interval time.Duration) *MockWithdrawGateway {
	if required <= 0 {
		required = 2
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	return &MockWithdrawGateway{
		required: required,
		interval: interval,
		pending:  make(map[string]*WithdrawEvent),
		stop:     make(chan struct{}),
	}
}

// Start 启动后台确认循环（按区块推进提现状态机）。
func (g *MockWithdrawGateway) Start() {
	g.mu.Lock()
	if g.started {
		g.mu.Unlock()
		return
	}
	g.started = true
	stop := make(chan struct{}) // 重建，避免复用 Stop 已关闭的 stop（否则再次 Start 失效、再次 Stop panic）
	g.stop = stop
	g.mu.Unlock()
	go func() {
		ticker := time.NewTicker(g.interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				g.tick()
			}
		}
	}()
}

// Stop 停止确认循环。
func (g *MockWithdrawGateway) Stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.started {
		return
	}
	close(g.stop)
	g.started = false
}

// Interval 返回区块确认间隔。
func (g *MockWithdrawGateway) Interval() time.Duration {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.interval
}

// Tick 手动推进一个区块（单测/演示用，无需等待 interval）。
func (g *MockWithdrawGateway) Tick() { g.tick() }

func (g *MockWithdrawGateway) tick() {
	g.mu.Lock()
	now := time.Now().UnixNano()
	g.height++ // 模拟区块推进
	done := make([]WithdrawEvent, 0)
	started := make([]WithdrawEvent, 0)
	for _, ev := range g.pending {
		if ev.Status == WithdrawCredited || ev.Status == WithdrawFailed {
			continue
		}
		switch ev.Status {
		case WithdrawPending:
			if ev.WillFail {
				ev.Status = WithdrawFailed
				ev.UpdatedAt = now
				done = append(done, *ev)
			} else {
				ev.Status = WithdrawBroadcasting
				ev.Confirmations = 1
				ev.UpdatedAt = now
				started = append(started, *ev)
			}
		case WithdrawBroadcasting:
			// 真实节点可达时用链上确认数推进；否则（未配置/节点宕机）回退模拟 +1。
			ev.Confirmations = nextConfirmations(g.confirmSource, context.Background(), ev.Chain, ev.TxHash, ev.Confirmations)
			ev.UpdatedAt = now
			if ev.Confirmations >= ev.Required {
				ev.Status = WithdrawCredited
				ev.BlockHeight = g.height // 记录最终确认所在区块高度
				g.seq++
				done = append(done, *ev)
			}
		}
	}
	g.mu.Unlock()

	// 生命周期里程碑日志（锁外，避免持锁做 I/O；与 emit 一致）。
	for _, ev := range started {
		log.Printf("[settlement] withdraw broadcasting (entering confirmation tracking): tx=%s user=%d asset=%s chain=%s amount=%s",
			ev.TxHash, ev.UserID, ev.Asset, ev.Chain, ev.Amount.HumanString())
	}
	for _, ev := range done {
		if ev.Status == WithdrawCredited {
			log.Printf("[settlement] withdraw credited (final confirmation reached): tx=%s user=%d asset=%s chain=%s amount=%s confirmations=%d block=%d",
				ev.TxHash, ev.UserID, ev.Asset, ev.Chain, ev.Amount.HumanString(), ev.Confirmations, ev.BlockHeight)
		}
		g.emit(ev)
	}
}

func (g *MockWithdrawGateway) emit(ev WithdrawEvent) {
	g.subs.broadcast(ev, func(ev WithdrawEvent) {
		log.Printf("[settlement] withdraw emit DROPPED: tx=%s user=%d status=%s (subscriber backpressure)",
			ev.TxHash, ev.UserID, ev.Status)
	})
}

// SubmitWithdraw 受理一笔提现意图（使用本地生成的模拟 TxHash）。
func (g *MockWithdrawGateway) SubmitWithdraw(userID int64, asset string, chain Chain, amount, fee AssetAmount, address string, willFail bool) (*WithdrawEvent, error) {
	txHash := GenerateTxHash(userID, "W_"+asset, chain, amount, time.Now().UnixNano())
	return g.SubmitWithdrawWithHash(userID, asset, chain, amount, fee, address, willFail, txHash)
}

// SubmitWithdrawWithHash 与 SubmitWithdraw 同义，但使用调用方提供的交易哈希
// （链上 RPC 广播后由节点返回的真实 TxHash）。供 RPCWithdrawGateway 在真实广播
// 成功后注入真实哈希，使链上记录与内部事件一致。txHash 为空时回退本地生成。
func (g *MockWithdrawGateway) SubmitWithdrawWithHash(userID int64, asset string, chain Chain, amount, fee AssetAmount, address string, willFail bool, txHash string) (*WithdrawEvent, error) {
	if userID <= 0 || amount.Sign() <= 0 {
		return nil, fmt.Errorf("invalid withdraw params")
	}
	if asset == "" {
		asset = "USDT"
	}
	if address == "" {
		address = GenerateAddress(userID, chain)
	}
	if txHash == "" {
		txHash = GenerateTxHash(userID, "W_"+asset, chain, amount, time.Now().UnixNano())
	}
	g.mu.Lock()
	ev := &WithdrawEvent{
		TxHash:        txHash,
		UserID:        userID,
		Asset:         asset,
		Amount:        amount,
		Fee:           fee,
		Chain:         chain,
		Address:       address,
		Confirmations: 0,
		Required:      g.required,
		Status:        WithdrawPending,
		WillFail:      willFail,
		CreatedAt:     time.Now().UnixNano(),
		UpdatedAt:     time.Now().UnixNano(),
	}
	g.pending[txHash] = ev
	g.mu.Unlock()
	return ev, nil
}

// WatchWithdraw 订阅清结算结果事件流。
func (g *MockWithdrawGateway) WatchWithdraw(ctx context.Context) (<-chan WithdrawEvent, error) {
	ch := make(chan WithdrawEvent, 64)
	g.subs.register(ch, ctx)
	return ch, nil
}

// WatchWithdrawRollback 订阅"提现孤块/重组回滚"事件流（已清结算提现被丢弃时推送），
// 用于驱动内部账本回拨冻结余额。
func (g *MockWithdrawGateway) WatchWithdrawRollback(ctx context.Context) (<-chan WithdrawEvent, error) {
	ch := make(chan WithdrawEvent, 64)
	g.rollbackSubs.register(ch, ctx)
	return ch, nil
}

// WithdrawReorg 模拟提现孤块/重组：根据交易当前状态采取不同动作，体现"重组窗口"语义。
//   - WithdrawBroadcasting（已广播但尚未达安全确认）：交易随短链被抛弃，安全回退到
//     Pending（重置确认数、重新广播），不推送回滚、不触发内部账本 ReverseWithdraw——
//     因为该笔提现从未最终划出，用户资金不受影响（重组窗口内的正确处理）。
//   - WithdrawCredited（已达最终确认）：标记孤块丢弃并推送回滚事件，驱动内部账本
//     ReverseWithdraw 回拨冻结余额。
//   - WithdrawOrphaned：幂等，直接返回。
//   - WithdrawPending（尚未广播）：无重组对象，拒绝（返回 error）。
//
// 生产环境由真实节点的区块重组通知触发；此处供演示/单测注入场景。
func (g *MockWithdrawGateway) WithdrawReorg(txHash string) (*WithdrawEvent, error) {
	g.mu.Lock()
	ev, ok := g.pending[txHash]
	if !ok {
		g.mu.Unlock()
		return nil, fmt.Errorf("withdraw not found: %s", txHash)
	}
	switch ev.Status {
	case WithdrawOrphaned:
		g.mu.Unlock()
		return ev, nil
	case WithdrawCredited:
		ev.Status = WithdrawOrphaned
		ev.UpdatedAt = time.Now().UnixNano()
		g.mu.Unlock()
		// 高危资金事件：已最终确认的提现被链上重组丢弃，需驱动账本 ReverseWithdraw 回拨冻结余额。
		log.Printf("[settlement] WARN withdraw REORG rollback: tx=%s user=%d asset=%s chain=%s amount=%s (credited tx orphaned, ledger reversal required)",
			ev.TxHash, ev.UserID, ev.Asset, ev.Chain, ev.Amount.HumanString())
		g.emitWithdrawRollback(*ev)
		return ev, nil
	case WithdrawBroadcasting:
		// 重组窗口：已广播未达安全确认，安全回退到待确认重新广播，不触发回滚。
		ev.Confirmations = 0
		ev.Status = WithdrawPending
		ev.UpdatedAt = time.Now().UnixNano()
		g.mu.Unlock()
		log.Printf("[settlement] withdraw reorg window reset: tx=%s user=%d asset=%s chain=%s (broadcasting tx dropped, re-broadcast pending)",
			ev.TxHash, ev.UserID, ev.Asset, ev.Chain)
		return ev, nil
	default: // WithdrawPending 等：未广播，无重组对象
		g.mu.Unlock()
		return nil, fmt.Errorf("withdraw status %s cannot reorg", ev.Status)
	}
}

// WithdrawReorgDepth 模拟"深度重组"：回退最近 depth 个区块内所有已达最终确认（Credited）
// 的提现，标记孤块丢弃并推送回滚事件（驱动内部账本 ReverseWithdraw 回拨）；同时把窗口内
// 尚未最终确认的 Broadcasting/Pending 交易重置确认数（随短链被抛弃、重新等待确认）。
// height 相应回退 depth。返回被回滚的交易列表。生产由真实节点的长链重组通知触发。
func (g *MockWithdrawGateway) WithdrawReorgDepth(depth int) []WithdrawEvent {
	if depth <= 0 {
		return nil
	}
	g.mu.Lock()
	now := time.Now().UnixNano()
	var rolled []WithdrawEvent
	cutoff := g.height - depth + 1
	resetCount := 0
	for _, ev := range g.pending {
		switch ev.Status {
		case WithdrawCredited:
			if ev.BlockHeight >= cutoff {
				ev.Status = WithdrawOrphaned
				ev.UpdatedAt = now
				rolled = append(rolled, *ev)
			}
		case WithdrawBroadcasting, WithdrawPending:
			// 窗口内未最终确认的交易随短链被抛弃，重置确认数重新等待
			ev.Confirmations = 0
			ev.Status = WithdrawPending
			ev.UpdatedAt = now
			resetCount++
		}
	}
	if g.height >= depth {
		g.height -= depth
	}
	g.mu.Unlock()
	// 逐笔回滚日志（锁外）+ 汇总，便于审计「哪些已确认提现被深度重组回拨」。
	for _, ev := range rolled {
		log.Printf("[settlement] WARN withdraw REORG rollback (depth=%d): tx=%s user=%d asset=%s chain=%s amount=%s (credited tx orphaned, ledger reversal required)",
			depth, ev.TxHash, ev.UserID, ev.Asset, ev.Chain, ev.Amount.HumanString())
	}
	if len(rolled) > 0 || resetCount > 0 {
		log.Printf("[settlement] withdraw depth reorg applied: depth=%d rolled=%d reset=%d newHeight=%d",
			depth, len(rolled), resetCount, g.height)
	}
	for _, ev := range rolled {
		g.emitWithdrawRollback(ev)
	}
	return rolled
}

func (g *MockWithdrawGateway) emitWithdrawRollback(ev WithdrawEvent) {
	g.rollbackSubs.broadcast(ev, func(ev WithdrawEvent) {
		log.Printf("[settlement] withdraw rollback emit DROPPED: tx=%s user=%d status=%s (subscriber backpressure)",
			ev.TxHash, ev.UserID, ev.Status)
	})
}

// WithdrawHistory 返回所有提现事件快照。
func (g *MockWithdrawGateway) WithdrawHistory() []WithdrawEvent {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]WithdrawEvent, 0, len(g.pending))
	for _, ev := range g.pending {
		out = append(out, *ev)
	}
	return out
}

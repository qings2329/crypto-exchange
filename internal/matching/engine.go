package matching

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"
)

// Engine 撮合引擎：管理多个交易对的订单簿。
// 设计：每个交易对一个 goroutine 串行处理订单，避免锁竞争（actor 模型）。
//
// 持久化（可选）：调用 UseStore 接入 Store 后，引擎具备
//   - 全局唯一订单号（多实例不冲突）；
//   - WAL：订单在写入内存簿前先落盘，崩溃可重放；
//   - 周期快照 + leader 选举，支持多实例单写者部署与故障接管。
// 未接入 Store 时行为与原来完全一致（内存、本地自增 ID）。
type Engine struct {
	mu      sync.RWMutex
	books   map[string]*bookActor
	onTrade func(symbol string, t Trade)
	onBook  func(symbol string) // 每笔订单处理后回调，用于推送深度变化

	// 以下字段仅在 UseStore 后被使用。
	store           Store
	nodeID          string
	snapshotInterval time.Duration
	recovered       bool
}

type bookActor struct {
	book *OrderBook
	ch   chan *Order
}

// NewEngine 创建撮合引擎。
func NewEngine(onTrade func(symbol string, t Trade), onBook func(symbol string)) *Engine {
	return &Engine{
		books:   make(map[string]*bookActor),
		onTrade: onTrade,
		onBook:  onBook,
	}
}

// UseStore 接入持久化与协调后端。snapshotInterval<=0 时默认 5s。
// 应在 Register 之前调用；多实例部署时每个进程传入唯一 nodeID。
func (e *Engine) UseStore(store Store, nodeID string, snapshotInterval time.Duration) {
	if snapshotInterval <= 0 {
		snapshotInterval = 5 * time.Second
	}
	e.store = store
	e.nodeID = nodeID
	e.snapshotInterval = snapshotInterval
}

// Register 注册一个交易对并启动其处理 goroutine（幂等）。
func (e *Engine) Register(symbol string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.books[symbol]; ok {
		return
	}
	ba := &bookActor{
		book: NewOrderBook(symbol),
		ch:   make(chan *Order, 1024),
	}
	e.books[symbol] = ba
	go e.run(ba)
}

func (e *Engine) run(ba *bookActor) {
	// 单笔订单处理异常不应拖垮整个撮合 goroutine（生产应告警+重试）。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[matching] panic in symbol %s: %v", ba.book.symbol, r)
		}
	}()
	for o := range ba.ch {
		trades := ba.book.Match(o)
		for _, t := range trades {
			if e.onTrade != nil {
				e.onTrade(ba.book.symbol, t)
			}
			// TODO: 发布成交流到 Kafka，触发清算服务记账
		}
		if e.onBook != nil {
			e.onBook(ba.book.symbol)
		}
	}
}

// Submit 提交一笔订单到对应交易对（异步入队）。
// 接入 Store 时：若 o.ID==0 则由 Store 分配全局唯一 ID；并在入队前同步写入 WAL。
func (e *Engine) Submit(symbol string, o *Order) bool {
	e.mu.RLock()
	ba, ok := e.books[symbol]
	e.mu.RUnlock()
	if !ok {
		return false
	}
	if e.store != nil {
		if o.ID == 0 {
			if id, err := e.store.NextOrderID(context.Background()); err == nil {
				o.ID = id
			}
		}
		// best-effort：写 WAL 失败仅记录，不阻断撮合。
		if err := e.store.Append(context.Background(), OrderEvent{
			Symbol: symbol,
			Type:   EventSubmit,
			Order:  o,
			Ts:     time.Now().UnixNano(),
		}); err != nil {
			log.Printf("[matching] WAL append failed (submit %d): %v", o.ID, err)
		}
	}
	ba.ch <- o
	return true
}

// Cancel 撤销一笔订单（同步、线程安全）。接入 Store 时先写 WAL 再改内存簿。
func (e *Engine) Cancel(symbol string, orderID int64) bool {
	e.mu.RLock()
	ba, ok := e.books[symbol]
	e.mu.RUnlock()
	if !ok {
		return false
	}
	if e.store != nil {
		if err := e.store.Append(context.Background(), OrderEvent{
			Symbol:  symbol,
			Type:    EventCancel,
			OrderID: orderID,
			Ts:      time.Now().UnixNano(),
		}); err != nil {
			log.Printf("[matching] WAL append failed (cancel %d): %v", orderID, err)
		}
	}
	return ba.book.Cancel(orderID)
}

// MatchNow 同步撮合一笔订单（不入队、不触发 onTrade/onBook），返回成交列表与是否完全成交。
// 用于强平：把强平单直接送入撮合引擎成交，再据真实成交回填持仓/账本。
//   - rest=true：未成交部分挂单（与 Submit 路径一致）。
//   - rest=false：市价单（Price=0）未成交部分直接丢弃（由调用方以保险基金兜底成交，
//     避免空流动性时残留 price=0 挂单污染订单簿）。
//
// 注意：本方法同步返回，不会回调 onTrade，因此调用方可安全在强平扫描上下文中使用，
// 不会因 onTrade 再次触发 UpdateMarkPrice 造成重入。强平消耗订单簿流动性，
// 该变更目前不写入 WAL（强平属特殊路径），恢复时由强平扫描重新触发，见 DEVELOPMENT_TASKS。
func (e *Engine) MatchNow(symbol string, o *Order, rest bool) ([]Trade, bool) {
	e.mu.RLock()
	ba, ok := e.books[symbol]
	e.mu.RUnlock()
	if !ok {
		return nil, false
	}
	trades := ba.book.MatchRest(o, rest)
	return trades, o.IsFilled()
}

// Depth 获取某交易对深度快照。
func (e *Engine) Depth(symbol string) (bids, asks []Level, ok bool) {
	e.mu.RLock()
	ba, ok := e.books[symbol]
	e.mu.RUnlock()
	if !ok {
		return nil, nil, false
	}
	b, a := ba.book.Depth()
	return b, a, true
}

// Recover 从 Store 恢复订单簿：先加载快照，再重放快照之后的 WAL 增量。
// 必须在 Register（至少对已持久化交易对）之后、开始接受新订单之前调用。
// 接入 Store 时才生效；未接入直接返回 nil。幂等于 recovered 标记。
func (e *Engine) Recover(ctx context.Context) error {
	if e.store == nil {
		return nil
	}
	e.mu.Lock()
	if e.recovered {
		e.mu.Unlock()
		return nil
	}
	e.recovered = true
	e.mu.Unlock()

	version, state, err := e.store.LoadSnapshot(ctx)
	if err != nil {
		return err
	}
	if len(state) > 0 {
		var books []BookState
		if err := json.Unmarshal(state, &books); err != nil {
			return err
		}
		for _, bs := range books {
			e.registerLocked(bs.Symbol, bs)
		}
	}

	events, err := e.store.Replay(ctx, version)
	if err != nil {
		return err
	}
	var maxID int64
	for _, ev := range events {
		ba := e.ensureBookLocked(ev.Symbol)
		switch ev.Type {
		case EventSubmit:
			if ev.Order != nil {
				ba.book.MatchRest(ev.Order, true)
				if ev.Order.ID > maxID {
					maxID = ev.Order.ID
				}
			}
		case EventCancel:
			ba.book.Cancel(ev.OrderID)
		}
	}
	if maxID > 0 {
		if err := e.store.SetMinOrderID(ctx, maxID); err != nil {
			return err
		}
	}
	log.Printf("[matching] recovered from snapshot@%d + %d wal events", version, len(events))
	return nil
}

// SnapshotLoop 周期对全量订单簿做快照并剪枝已覆盖的 WAL。仅在 leader 上运行。
func (e *Engine) SnapshotLoop(ctx context.Context) {
	if e.store == nil {
		return
	}
	ticker := time.NewTicker(e.snapshotInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.snapshot(ctx)
		}
	}
}

func (e *Engine) snapshot(ctx context.Context) {
	e.mu.RLock()
	books := make([]BookState, 0, len(e.books))
	for _, ba := range e.books {
		books = append(books, ba.book.Snapshot())
	}
	e.mu.RUnlock()

	data, err := json.Marshal(books)
	if err != nil {
		log.Printf("[matching] snapshot marshal failed: %v", err)
		return
	}
	maxSeq, err := e.store.MaxSeq(ctx)
	if err != nil {
		log.Printf("[matching] snapshot maxseq failed: %v", err)
		return
	}
	if err := e.store.SaveSnapshot(ctx, maxSeq, data); err != nil {
		log.Printf("[matching] snapshot save failed: %v", err)
		return
	}
	if err := e.store.PruneWAL(ctx, maxSeq); err != nil {
		log.Printf("[matching] wal prune failed: %v", err)
		return
	}
}

// Reset 清空内存订单簿并结束所有交易对的 goroutine（用于失去 leadership 后重新接管时重同步）。
func (e *Engine) Reset() {
	e.mu.Lock()
	for _, ba := range e.books {
		close(ba.ch)
	}
	e.books = make(map[string]*bookActor)
	e.recovered = false
	e.mu.Unlock()
}

// registerLocked 调用方必须持有 e.mu。无快照时用空簿注册；有快照时恢复。
func (e *Engine) registerLocked(symbol string, snap BookState) {
	ba := &bookActor{
		book: NewOrderBook(symbol),
		ch:   make(chan *Order, 1024),
	}
	if len(snap.Bids)+len(snap.Asks) > 0 {
		ba.book.Restore(snap)
	}
	e.books[symbol] = ba
	go e.run(ba)
}

// ensureBookLocked 调用方必须持有 e.mu；返回 symbol 对应的 bookActor（不存在则创建）。
func (e *Engine) ensureBookLocked(symbol string) *bookActor {
	if ba, ok := e.books[symbol]; ok {
		return ba
	}
	ba := &bookActor{
		book: NewOrderBook(symbol),
		ch:   make(chan *Order, 1024),
	}
	e.books[symbol] = ba
	go e.run(ba)
	return ba
}

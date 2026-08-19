package matching

import (
	"context"
	"encoding/json"
	"log"
	"sort"
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
//
// 未接入 Store 时行为与原来完全一致（内存、本地自增 ID）。
type Engine struct {
	mu      sync.RWMutex
	books   map[string]*bookActor
	onTrade func(symbol string, t Trade)
	onBook  func(symbol string) // 每笔订单处理后回调，用于推送深度变化

	// 以下字段仅在 UseStore 后被使用。
	store            Store
	nodeID           string
	snapshotInterval time.Duration
	recovered        bool

	// 订单/成交登记（订单管理模块）：内存态。重启后 open 订单由 Recover 重建，
	// 历史 filled/canceled 与成交流水丢失（原型限制，见 DEVELOPMENT_TASKS）。
	ordersMu   sync.Mutex
	orders     map[int64]*orderMeta
	userOrders map[int64][]int64
	trades     []tradeRecord
	userTrades map[int64][]int64
	tradeSeq   int64
}

// orderMeta 是订单在登记表中的不可变快照 + 成交累加量。
// 关键：不持有 *Order 指针——止盈止损激活产生副本后避免指针错位；
// FilledQty 按 TakerOID/MakerOID（同 orderID）累加，状态始终正确。
type orderMeta struct {
	ID          int64
	UserID      int64
	Symbol      string
	Market      string
	IsMargin    bool
	Leverage    float64
	Side        Side
	Price       Fixed
	Qty         Fixed
	FilledQty   Fixed
	TimeInForce string
	Status      OrderStatus
	CreatedAt   int64
	UpdatedAt   int64
}

// tradeRecord 是一笔成交的登记表条目（买卖双边用户均索引）。
type tradeRecord struct {
	Seq       int64
	Symbol    string
	Market    string
	IsMargin  bool
	Leverage  float64
	Price     Fixed
	Qty       Fixed
	TakerID   int64
	MakerID   int64
	TakerSide Side
	TakerOID  int64
	MakerOID  int64
	Time      int64
}

type bookActor struct {
	book *OrderBook
	ch   chan *Order
}

// NewEngine 创建撮合引擎。
func NewEngine(onTrade func(symbol string, t Trade), onBook func(symbol string)) *Engine {
	return &Engine{
		books:      make(map[string]*bookActor),
		onTrade:    onTrade,
		onBook:     onBook,
		orders:     make(map[int64]*orderMeta),
		userOrders: make(map[int64][]int64),
		userTrades: make(map[int64][]int64),
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
		// 订单状态与成交流水登记（订单管理模块）。
		e.applyTrades(ba.book.symbol, o, trades)
		for _, t := range trades {
			if e.onTrade != nil {
				e.onTrade(ba.book.symbol, t)
			}
			// 成交流对外发布（Kafka / WS）由调用方在 onTrade 回调中实现：
			// 例如 cmd/matching 的 onTrade 会 pub.PublishTrade 到 Kafka 并广播 WS。
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
	// F5：非法订单拒绝接入，避免异常数值经 WAL/订单簿扩散。
	if !validOrder(o) {
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
	e.registerOrder(o, symbol, time.Now().UnixNano())
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
	if ba.book.Cancel(orderID) {
		e.ordersMu.Lock()
		if m, ok := e.orders[orderID]; ok {
			m.Status = OrderCanceled
			m.UpdatedAt = time.Now().UnixNano()
		}
		e.ordersMu.Unlock()
		return true
	}
	return false
}

// MatchNow 同步撮合一笔订单（不入队、不触发 onTrade/onBook），返回成交列表与是否完全成交。
// 用于强平：把强平单直接送入撮合引擎成交，再据真实成交回填持仓/账本。
//   - rest=true：未成交部分挂单（与 Submit 路径一致）。
//   - rest=false：市价单（Price=0）未成交部分直接丢弃（由调用方以保险基金兜底成交，
//     避免空流动性时残留 price=0 挂单污染订单簿）。
//
// 注意：本方法同步返回，不会回调 onTrade，因此调用方可安全在强平扫描上下文中使用，
// 不会因 onTrade 再次触发 UpdateMarkPrice 造成重入。
//
// 持久化：接入 Store 时，强平单与普通订单一致写入 WAL（EventSubmit），
// 因此崩溃恢复能重放该成交，补齐原 §17 已知的"强平流动性不写 WAL"缺口；
// 恢复经 Recover 重放 EventSubmit 时并不触发 onTrade，故不会重复结算。
// validOrder 校验订单数值合法性（F5 防御纵深）：数量必须为正且有限；价格、触发价、
// 限价必须为有限且非负（Price=0 表示市价单，允许）。非法订单拒绝进入撮合，
// 避免 NaN/负价污染订单簿或产生异常成交（HTTP 入口虽已拦，引擎层再兜底）。
func validOrder(o *Order) bool {
	if o == nil {
		return false
	}
	if !o.Qty.IsPositive() {
		return false
	}
	if !o.Price.IsZero() && !o.Price.IsPositive() {
		return false
	}
	if !o.StopPrice.IsZero() && !o.StopPrice.IsPositive() {
		return false
	}
	if !o.StopLimit.IsZero() && !o.StopLimit.IsPositive() {
		return false
	}
	return true
}

func (e *Engine) MatchNow(symbol string, o *Order, rest bool) ([]Trade, bool) {
	e.mu.RLock()
	ba, ok := e.books[symbol]
	e.mu.RUnlock()
	if !ok {
		return nil, false
	}
	// F5：非法订单直接拒绝，不进入撮合（避免异常数值破坏订单簿/成交）。
	if !validOrder(o) {
		return nil, false
	}
	if e.store != nil {
		if o.ID == 0 {
			if id, err := e.store.NextOrderID(context.Background()); err == nil {
				o.ID = id
			}
		}
		// 强平单同样写 WAL，保证恢复时可重放（与 Submit 路径一致）。
		if err := e.store.Append(context.Background(), OrderEvent{
			Symbol: symbol,
			Type:   EventSubmit,
			Order:  o,
			Ts:     time.Now().UnixNano(),
		}); err != nil {
			log.Printf("[matching] WAL append failed (matchnow %d): %v", o.ID, err)
		}
	}
	e.registerOrder(o, symbol, time.Now().UnixNano())
	trades := ba.book.MatchRest(o, rest)
	e.applyTrades(symbol, o, trades)
	return trades, o.IsFilled()
}

// SetMarkPrice 喂入某交易对最新标记/成交参考价，并激活穿越触发价的止盈止损单。
// 返回被激活订单的成交流，便于调用方经 onTrade 对外发布（无交易对返回 nil）。
// 注意：成交驱动的最后价由 MatchRest 内部自动更新；本方法用于"无成交但价格已穿越"的
// 触发场景（如指数/标记价触发的止损）。
func (e *Engine) SetMarkPrice(symbol string, price Fixed) []Trade {
	e.mu.RLock()
	ba, ok := e.books[symbol]
	e.mu.RUnlock()
	if !ok {
		return nil
	}
	// F5：拒绝非正标记价，避免 NaN 污染最近成交价并导致止盈止损单误触发。
	if !price.IsPositive() {
		return nil
	}
	return ba.book.SetLast(price)
}

// ---- 订单管理模块：登记与查询 ----

// registerOrder 登记一笔订单到登记表（幂等：已存在则跳过）。
func (e *Engine) registerOrder(o *Order, symbol string, now int64) {
	e.ordersMu.Lock()
	defer e.ordersMu.Unlock()
	if _, ok := e.orders[o.ID]; ok {
		return
	}
	e.orders[o.ID] = &orderMeta{
		ID:          o.ID,
		UserID:      o.UserID,
		Symbol:      symbol,
		Market:      o.Market,
		IsMargin:    o.IsMargin,
		Leverage:    o.Leverage,
		Side:        o.Side,
		Price:       o.Price,
		Qty:         o.Qty,
		FilledQty:   o.Filled,
		TimeInForce: o.TimeInForce,
		Status:      OrderOpen,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	e.userOrders[o.UserID] = append(e.userOrders[o.UserID], o.ID)
}

// applyTrades 在撮合完成后更新订单状态并登记成交流水。
// trades 为本笔 taker 订单产生的成交；maker 侧的 FilledQty 按 MakerOID 累加。
func (e *Engine) applyTrades(symbol string, taker *Order, trades []Trade) {
	now := time.Now().UnixNano()
	e.ordersMu.Lock()
	for _, t := range trades {
		e.tradeSeq++
		rec := tradeRecord{
			Seq:       e.tradeSeq,
			Symbol:    symbol,
			Market:    taker.Market,
			IsMargin:  taker.IsMargin,
			Leverage:  taker.Leverage,
			Price:     t.Price,
			Qty:       t.Qty,
			TakerID:   t.TakerID,
			MakerID:   t.MakerID,
			TakerSide: t.TakerSide,
			TakerOID:  t.TakerOID,
			MakerOID:  t.MakerOID,
			Time:      now,
		}
		e.trades = append(e.trades, rec)
		e.userTrades[t.TakerID] = append(e.userTrades[t.TakerID], rec.Seq)
		e.userTrades[t.MakerID] = append(e.userTrades[t.MakerID], rec.Seq)
		if m, ok := e.orders[t.MakerOID]; ok {
			m.FilledQty = m.FilledQty.Add(t.Qty)
			if m.FilledQty.Cmp(m.Qty) >= 0 {
				m.Status = OrderFilled
			}
			m.UpdatedAt = now
		}
	}
	if tm, ok := e.orders[taker.ID]; ok {
		tm.FilledQty = taker.Filled
		switch {
		case taker.IsFilled():
			tm.Status = OrderFilled
		case e.bookHasOrder(symbol, taker.ID):
			if tm.FilledQty.IsPositive() {
				tm.Status = OrderPartial
			} else {
				tm.Status = OrderOpen
			}
		default:
			tm.Status = OrderCanceled
		}
		tm.UpdatedAt = now
	}
	e.ordersMu.Unlock()
}

// bookHasOrder 回报指定交易对订单簿中是否仍挂有该订单 ID（判断 taker 是否留挂单）。
func (e *Engine) bookHasOrder(symbol string, id int64) bool {
	e.mu.RLock()
	ba, ok := e.books[symbol]
	e.mu.RUnlock()
	if !ok {
		return false
	}
	return ba.book.Contains(id)
}

// rebuildOrderIndex 扫描全部订单簿档位，重建 open 订单登记（重启后调用）。
// 历史 filled/canceled 与成交流水不重建（原型限制）。
func (e *Engine) rebuildOrderIndex() {
	e.mu.RLock()
	books := make([]*bookActor, 0, len(e.books))
	for _, ba := range e.books {
		books = append(books, ba)
	}
	e.mu.RUnlock()
	now := time.Now().UnixNano()
	for _, ba := range books {
		bids, asks := ba.book.Depth()
		for _, lvl := range bids {
			for _, o := range lvl.Orders {
				e.registerOrder(o, ba.book.symbol, now)
			}
		}
		for _, lvl := range asks {
			for _, o := range lvl.Orders {
				e.registerOrder(o, ba.book.symbol, now)
			}
		}
	}
}

// tradeBySeq 在单调递增的 trades 切片中二分查找成交记录。
func (e *Engine) tradeBySeq(seq int64) *tradeRecord {
	lo, hi := 0, len(e.trades)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		if e.trades[mid].Seq == seq {
			return &e.trades[mid]
		}
		if e.trades[mid].Seq < seq {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return nil
}

// ListOrders 返回指定用户的订单（按 user_id 过滤；symbol/status 为空不过滤；
// limit<=0 不限制）。user_id=0 时返回全部（调试用）。
func (e *Engine) ListOrders(userID int64, symbol, status string, limit int) []OrderView {
	e.ordersMu.Lock()
	defer e.ordersMu.Unlock()
	var ids []int64
	if userID != 0 {
		ids = e.userOrders[userID]
	} else {
		ids = make([]int64, 0, len(e.orders))
		for id := range e.orders {
			ids = append(ids, id)
		}
	}
	out := make([]OrderView, 0, len(ids))
	for _, id := range ids {
		m := e.orders[id]
		if m == nil {
			continue
		}
		if symbol != "" && m.Symbol != symbol {
			continue
		}
		if status != "" && string(m.Status) != status {
			continue
		}
		out = append(out, m.toView())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// GetOrder 按订单 ID 返回详情；不存在返回 (OrderView{}, false)。
func (e *Engine) GetOrder(orderID int64) (OrderView, bool) {
	e.ordersMu.Lock()
	defer e.ordersMu.Unlock()
	m, ok := e.orders[orderID]
	if !ok {
		return OrderView{}, false
	}
	return m.toView(), true
}

// ListTrades 返回指定用户的成交流水（按 user_id；symbol 为空不过滤；limit<=0 不限制）。
// 结果按时间倒序（最新在前）。
func (e *Engine) ListTrades(userID int64, symbol string, limit int) []TradeView {
	e.ordersMu.Lock()
	defer e.ordersMu.Unlock()
	var seqs []int64
	if userID != 0 {
		seqs = e.userTrades[userID]
	} else {
		seqs = make([]int64, 0, len(e.trades))
		for _, r := range e.trades {
			seqs = append(seqs, r.Seq)
		}
	}
	out := make([]TradeView, 0)
	for i := len(seqs) - 1; i >= 0; i-- {
		rec := e.tradeBySeq(seqs[i])
		if rec == nil {
			continue
		}
		if symbol != "" && rec.Symbol != symbol {
			continue
		}
		out = append(out, rec.toView())
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

func (m *orderMeta) toView() OrderView {
	return OrderView{
		ID:          m.ID,
		UserID:      m.UserID,
		Symbol:      m.Symbol,
		Market:      m.Market,
		IsMargin:    m.IsMargin,
		Leverage:    m.Leverage,
		Side:        sideString(m.Side),
		Price:       m.Price,
		Qty:         m.Qty,
		Filled:      m.FilledQty,
		Status:      m.Status,
		TimeInForce: m.TimeInForce,
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
	}
}

func (r *tradeRecord) toView() TradeView {
	return TradeView{
		ID:        r.Seq,
		Symbol:    r.Symbol,
		Market:    r.Market,
		IsMargin:  r.IsMargin,
		Leverage:  r.Leverage,
		Price:     r.Price,
		Qty:       r.Qty,
		TakerID:   r.TakerID,
		MakerID:   r.MakerID,
		TakerSide: sideString(r.TakerSide),
		TakerOID:  r.TakerOID,
		MakerOID:  r.MakerOID,
		Time:      r.Time,
	}
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
	// 重建 open 订单登记（历史 filled/canceled 与成交流水重启丢失，原型限制）。
	e.rebuildOrderIndex()
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

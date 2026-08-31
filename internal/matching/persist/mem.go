// Package persist 提供撮合引擎 Store 接口的实现：
//   - MemStore：纯内存实现，单实例开发与单测使用（不持久，进程退出即丢）；
//   - MySQLStore：以 MySQL 为共享后端，支持多实例部署与崩溃恢复。
//
// 两个实现都满足 internal/matching.Store 契约；cmd/matching 按配置选择。
package persist

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coldlar/crypto-exchange/internal/matching"
)

// MemStore 内存实现：无持久性，仅用于开发与测试。
// leader 选举以单 holder + 过期时间模拟；序号以原子计数器模拟。
// 成交流水 / 订单登记同样以内存切片保存（进程退出即丢，与整体 MemStore 语义一致）。
type MemStore struct {
	mu        sync.Mutex
	orderID   int64
	wal       []matching.OrderEvent
	seq       int64
	snapVer   int64
	snap      []byte
	trades    []matching.PersistedTrade
	orders    map[int64]matching.PersistedOrder
	leader    string
	leaderExp time.Time
}

// NewMemStore 构造内存 Store。
func NewMemStore() *MemStore { return &MemStore{} }

// NextOrderID 返回单调递增订单号。
func (m *MemStore) NextOrderID(ctx context.Context) (int64, error) {
	return atomic.AddInt64(&m.orderID, 1), nil
}

// SetMinOrderID 保证后续序号大于 id（恢复后对齐本地计数器）。
func (m *MemStore) SetMinOrderID(ctx context.Context, id int64) error {
	for {
		cur := atomic.LoadInt64(&m.orderID)
		if id <= cur {
			return nil
		}
		if atomic.CompareAndSwapInt64(&m.orderID, cur, id) {
			return nil
		}
	}
}

// Append 追加一条 WAL 事件（分配 seq）。
func (m *MemStore) Append(ctx context.Context, ev matching.OrderEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	ev.Seq = m.seq
	m.wal = append(m.wal, ev)
	return nil
}

// SaveSnapshot 保存快照（覆盖式）。
func (m *MemStore) SaveSnapshot(ctx context.Context, version int64, state []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapVer = version
	cp := make([]byte, len(state))
	copy(cp, state)
	m.snap = cp
	return nil
}

// LoadSnapshot 返回最近快照；无则 version<0。
func (m *MemStore) LoadSnapshot(ctx context.Context) (int64, []byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.snap == nil {
		return -1, nil, nil
	}
	cp := make([]byte, len(m.snap))
	copy(cp, m.snap)
	return m.snapVer, cp, nil
}

// Replay 返回 seq>afterVersion 的事件（升序）。
func (m *MemStore) Replay(ctx context.Context, afterVersion int64) ([]matching.OrderEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]matching.OrderEvent, 0, len(m.wal))
	for _, ev := range m.wal {
		if ev.Seq > afterVersion {
			out = append(out, ev)
		}
	}
	return out, nil
}

// MaxSeq 返回当前 WAL 最大 seq。
func (m *MemStore) MaxSeq(ctx context.Context) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.seq, nil
}

// PruneWAL 删除 seq<=seq 的事件。
func (m *MemStore) PruneWAL(ctx context.Context, seq int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.wal[:0:0]
	for _, ev := range m.wal {
		if ev.Seq > seq {
			kept = append(kept, ev)
		}
	}
	m.wal = kept
	return nil
}

// TryAcquireLeader 尝试成为 leader。
func (m *MemStore) TryAcquireLeader(ctx context.Context, node string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if m.leader == "" || m.leader == node || now.After(m.leaderExp) {
		m.leader = node
		m.leaderExp = now.Add(ttl)
		return true, nil
	}
	return false, nil
}

// RenewLeader 续约（仅当前 holder 成功）。
func (m *MemStore) RenewLeader(ctx context.Context, node string, ttl time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.leader == node && time.Now().Before(m.leaderExp) {
		m.leaderExp = time.Now().Add(ttl)
		return true, nil
	}
	return false, nil
}

// ReleaseLeader 放弃 leadership。
func (m *MemStore) ReleaseLeader(ctx context.Context, node string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.leader == node {
		m.leader = ""
		m.leaderExp = time.Time{}
	}
	return nil
}

// IsLeader 报告 node 是否为有效 leader。
func (m *MemStore) IsLeader(ctx context.Context, node string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.leader == node && time.Now().Before(m.leaderExp), nil
}

// AppendTrade 追加一笔成交流水（内存）。
func (m *MemStore) AppendTrade(ctx context.Context, t matching.PersistedTrade) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	t.Seq = m.seq
	m.trades = append(m.trades, t)
	return nil
}

// LoadTrades 返回内存中所有成交流水（升序，按追加顺序即 seq 序）。
func (m *MemStore) LoadTrades(ctx context.Context) ([]matching.PersistedTrade, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]matching.PersistedTrade, len(m.trades))
	copy(out, m.trades)
	return out, nil
}

// UpsertOrder 覆盖写入一笔订单登记（同 ID 覆盖）。
func (m *MemStore) UpsertOrder(ctx context.Context, o matching.PersistedOrder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.orders == nil {
		m.orders = make(map[int64]matching.PersistedOrder)
	}
	m.orders[o.ID] = o
	return nil
}

// LoadOrders 返回内存中所有订单登记。
func (m *MemStore) LoadOrders(ctx context.Context) ([]matching.PersistedOrder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]matching.PersistedOrder, 0, len(m.orders))
	for _, o := range m.orders {
		out = append(out, o)
	}
	return out, nil
}

// Close 内存实现无需释放。
func (m *MemStore) Close() error { return nil }

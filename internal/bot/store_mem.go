package bot

import (
	"sort"
	"sync"
)

// MemStore 是机器人业务的纯内存实现。
type MemStore struct {
	mu       sync.RWMutex
	strategies map[int64]*BotStrategy
	orders     map[int64]*BotOrder
	stratSeq  int64
	ordSeq    int64
}

// NewMemStore 构造内存存储。
func NewMemStore() *MemStore {
	return &MemStore{
		strategies: map[int64]*BotStrategy{},
		orders:     map[int64]*BotOrder{},
	}
}

func (m *MemStore) CreateStrategy(s *BotStrategy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stratSeq++
	s.ID = m.stratSeq
	cp := *s
	m.strategies[s.ID] = &cp
	return nil
}

func (m *MemStore) GetStrategy(id int64) (*BotStrategy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.strategies[id]
	if !ok {
		return nil, ErrStrategyNotFound
	}
	cp := *s
	return &cp, nil
}

func (m *MemStore) ListStrategiesByUser(uid int64) ([]*BotStrategy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*BotStrategy, 0, len(m.strategies))
	for _, s := range m.strategies {
		if s.UserID == uid {
			cp := *s
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) ListActiveStrategies() ([]*BotStrategy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*BotStrategy, 0, len(m.strategies))
	for _, s := range m.strategies {
		if s.Status == StrategyActive {
			cp := *s
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) ListAllStrategies() ([]*BotStrategy, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*BotStrategy, 0, len(m.strategies))
	for _, s := range m.strategies {
		cp := *s
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) UpdateStrategy(s *BotStrategy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.strategies[s.ID]; !ok {
		return ErrStrategyNotFound
	}
	cp := *s
	m.strategies[s.ID] = &cp
	return nil
}

func (m *MemStore) CreateOrder(o *BotOrder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ordSeq++
	o.ID = m.ordSeq
	cp := *o
	m.orders[o.ID] = &cp
	return nil
}

func (m *MemStore) ListOrdersByStrategy(sid int64) ([]*BotOrder, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*BotOrder, 0, len(m.orders))
	for _, o := range m.orders {
		if o.StrategyID == sid {
			cp := *o
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) CountOrdersByStrategy(sid int64) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var n int64
	for _, o := range m.orders {
		if o.StrategyID == sid {
			n++
		}
	}
	return n, nil
}

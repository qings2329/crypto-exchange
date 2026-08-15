// 内存清算存储：无 MySQL 时回退，单测与本地开发使用。幂等由 id 索引保证。
package settlement

import "sync"

type memClearingStore struct {
	mu     sync.RWMutex
	byID   map[int64]ClearedTrade
	order  []int64 // 入账顺序（用于 Recent 倒序）
	cap    int
}

// NewMemClearingStore 构造内存清算存储；cap<=0 默认保留最近 10000 条。
func NewMemClearingStore(capacity int) ClearingStore {
	if capacity <= 0 {
		capacity = 10000
	}
	return &memClearingStore{
		byID: map[int64]ClearedTrade{},
		cap:  capacity,
	}
}

func (s *memClearingStore) Record(t ClearedTrade) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[t.ID]; ok {
		return false, nil // 重复，幂等跳过
	}
	s.byID[t.ID] = t
	s.order = append(s.order, t.ID)
	// 超容量则丢弃最旧，避免无限增长（仅影响 Recent 展示，不影响统计准确性）。
	if len(s.order) > s.cap {
		old := s.order[0]
		s.order = s.order[1:]
		delete(s.byID, old)
	}
	return true, nil
}

func (s *memClearingStore) Recent(limit int) ([]ClearedTrade, error) {
	if limit <= 0 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ClearedTrade, 0, limit)
	// order 为正序，倒序取最近 limit 条。
	for i := len(s.order) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.byID[s.order[i]])
	}
	return out, nil
}

func (s *memClearingStore) Close() error { return nil }

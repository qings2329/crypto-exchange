package referral

import (
	"fmt"
	"sync"
)

// memStore 是 Store 的内存实现（开发/测试降级用）。
type memStore struct {
	mu   sync.RWMutex
	rows []*ReferralCommission
	next int64
}

func NewMemStore() Store {
	return &memStore{}
}

func (m *memStore) RecordCommission(c *ReferralCommission) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.BizRef == c.BizRef {
			return ErrCommissionExists
		}
	}
	m.next++
	c.ID = m.next
	m.rows = append(m.rows, c)
	return nil
}

func (m *memStore) GetCommissionByRef(bizRef string) (*ReferralCommission, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.rows {
		if r.BizRef == bizRef {
			return r, nil
		}
	}
	return nil, fmt.Errorf("commission not found")
}

func (m *memStore) ListCommissionsByReferrer(referrerID int64, limit, offset int) ([]*ReferralCommission, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var filtered []*ReferralCommission
	for _, r := range m.rows {
		if r.ReferrerID == referrerID {
			filtered = append(filtered, r)
		}
	}
	total := len(filtered)
	if offset >= total {
		return nil, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return filtered[offset:end], total, nil
}

func (m *memStore) ListAll(limit, offset int) ([]*ReferralCommission, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	total := len(m.rows)
	if offset >= total {
		return nil, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return m.rows[offset:end], total, nil
}

func (m *memStore) TotalByReferrer(referrerID int64) (map[string]int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]int64)
	for _, r := range m.rows {
		if r.ReferrerID == referrerID && r.Status == 1 {
			out[r.Asset] += r.Amount
		}
	}
	return out, nil
}

package c2c

import (
	"sync"
	"time"
)

// memStore 是 Store 的内存实现（开发/测试降级用）。
type memStore struct {
	mu   sync.RWMutex
	rows []*Order
	next int64
}

// NewMemStore 构造内存存储。
func NewMemStore() Store {
	return &memStore{}
}

func (m *memStore) Create(o *Order) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	now := time.Now()
	o.ID = m.next
	o.CreatedAt = now
	o.UpdatedAt = now
	m.rows = append(m.rows, o)
	return nil
}

func match(o *Order, f OrderFilter) bool {
	if f.UserID != 0 && o.UserID != f.UserID {
		return false
	}
	if f.Side != "" && o.Side != f.Side {
		return false
	}
	if f.Coin != "" && o.Coin != f.Coin {
		return false
	}
	if f.Status != "" && o.Status != f.Status {
		return false
	}
	return true
}

func (m *memStore) List(filter OrderFilter, limit, offset int) ([]*Order, int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Order
	for _, r := range m.rows {
		if match(r, filter) {
			out = append(out, r)
		}
	}
	total := len(out)
	if offset >= total {
		return []*Order{}, total, nil
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return out[offset:end], total, nil
}

func (m *memStore) Get(id int64) (*Order, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.rows {
		if r.ID == id {
			return r, nil
		}
	}
	return nil, ErrNotFound
}

func (m *memStore) UpdateStatus(id int64, from, to OrderStatus) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.ID == id {
			if r.Status != from {
				return false, nil
			}
			r.Status = to
			r.UpdatedAt = time.Now()
			return true, nil
		}
	}
	return false, ErrNotFound
}

func (m *memStore) Update(o *Order) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.ID == o.ID {
			r.Side = o.Side
			r.Coin = o.Coin
			r.Amount = o.Amount
			r.Price = o.Price
			r.Total = o.Total
			r.Note = o.Note
			r.UpdatedAt = time.Now()
			return nil
		}
	}
	return ErrNotFound
}

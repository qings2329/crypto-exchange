package wealth

import (
	"sync"
	"time"
)

// memStore 是内存版 Store，供单测与无 MySQL 的演示环境使用。
type memStore struct {
	mu              sync.RWMutex
	products        map[int64]*WealthProduct
	holdings        map[int64]*WealthHolding
	nextProductID   int64
	nextHoldingID   int64
}

// NewMemStore 构造内存 Store。
func NewMemStore() Store {
	return &memStore{
		products:      make(map[int64]*WealthProduct),
		holdings:      make(map[int64]*WealthHolding),
		nextProductID: 1,
		nextHoldingID: 1,
	}
}

// --- 产品 ---

func (s *memStore) CreateProduct(p *WealthProduct) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ID == 0 {
		p.ID = s.nextProductID
		s.nextProductID++
	}
	now := time.Now()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	cp := *p
	s.products[cp.ID] = &cp
	return nil
}

func (s *memStore) GetProduct(id int64) (*WealthProduct, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.products[id]
	if !ok {
		return nil, ErrProductNotFound
	}
	cp := *p
	return &cp, nil
}

func (s *memStore) ListProducts(status ProductStatus) ([]*WealthProduct, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*WealthProduct, 0)
	for _, p := range s.products {
		if status != "" && p.Status != status {
			continue
		}
		cp := *p
		out = append(out, &cp)
	}
	return out, nil
}

func (s *memStore) UpdateProduct(p *WealthProduct) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.UpdatedAt = time.Now()
	cp := *p
	s.products[cp.ID] = &cp
	return nil
}

// --- 持仓 ---

func (s *memStore) CreateHolding(h *WealthHolding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h.ID == 0 {
		h.ID = s.nextHoldingID
		s.nextHoldingID++
	}
	now := time.Now()
	if h.CreatedAt.IsZero() {
		h.CreatedAt = now
	}
	if h.LastAccrualAt.IsZero() {
		h.LastAccrualAt = now
	}
	h.UpdatedAt = now
	cp := *h
	s.holdings[cp.ID] = &cp
	return nil
}

func (s *memStore) GetHolding(id int64) (*WealthHolding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	h, ok := s.holdings[id]
	if !ok {
		return nil, ErrHoldingNotFound
	}
	cp := *h
	return &cp, nil
}

func (s *memStore) UpdateHolding(h *WealthHolding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h.UpdatedAt = time.Now()
	cp := *h
	s.holdings[cp.ID] = &cp
	return nil
}

func (s *memStore) ListHoldings(userID int64) ([]*WealthHolding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*WealthHolding, 0)
	for _, h := range s.holdings {
		if h.UserID == userID {
			cp := *h
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *memStore) ListAllHoldings() ([]*WealthHolding, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*WealthHolding, 0, len(s.holdings))
	for _, h := range s.holdings {
		cp := *h
		out = append(out, &cp)
	}
	return out, nil
}

package lending

import (
	"sort"
	"sync"
)

// MemStore 是借贷业务的纯内存实现。
type MemStore struct {
	mu             sync.RWMutex
	pools          map[int64]*LendingPool
	lendOrders     map[int64]*LendOrder
	borrowOrders   map[int64]*BorrowOrder
	interestRecs   map[int64]*InterestRecord
	poolSeq        int64
	lendSeq        int64
	borrowSeq      int64
	interestSeq    int64
}

func NewMemStore() *MemStore {
	return &MemStore{
		pools:        map[int64]*LendingPool{},
		lendOrders:   map[int64]*LendOrder{},
		borrowOrders: map[int64]*BorrowOrder{},
		interestRecs: map[int64]*InterestRecord{},
	}
}

func (m *MemStore) CreatePool(p *LendingPool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.poolSeq++
	p.ID = m.poolSeq
	cp := *p
	m.pools[p.ID] = &cp
	return nil
}

func (m *MemStore) GetPool(id int64) (*LendingPool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.pools[id]
	if !ok {
		return nil, ErrPoolNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *MemStore) GetPoolByAsset(asset string) (*LendingPool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, p := range m.pools {
		if p.Asset == asset {
			cp := *p
			return &cp, nil
		}
	}
	return nil, ErrPoolNotFound
}

func (m *MemStore) ListPools(status PoolStatus) ([]*LendingPool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*LendingPool
	for _, p := range m.pools {
		if status == "" || p.Status == status {
			cp := *p
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) UpdatePool(p *LendingPool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.pools[p.ID]; !ok {
		return ErrPoolNotFound
	}
	cp := *p
	m.pools[p.ID] = &cp
	return nil
}

func (m *MemStore) CreateLendOrder(o *LendOrder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lendSeq++
	o.ID = m.lendSeq
	cp := *o
	m.lendOrders[o.ID] = &cp
	return nil
}

func (m *MemStore) GetLendOrder(id int64) (*LendOrder, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.lendOrders[id]
	if !ok {
		return nil, ErrOrderNotFound
	}
	cp := *o
	return &cp, nil
}

func (m *MemStore) ListLendOrdersByUser(uid int64) ([]*LendOrder, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*LendOrder
	for _, o := range m.lendOrders {
		if o.UserID == uid {
			cp := *o
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) ListLendOrdersByPool(pid int64) ([]*LendOrder, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*LendOrder
	for _, o := range m.lendOrders {
		if o.PoolID == pid && o.Status == "active" {
			cp := *o
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) ListAllLendOrders() ([]*LendOrder, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*LendOrder
	for _, o := range m.lendOrders {
		cp := *o
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) UpdateLendOrder(o *LendOrder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.lendOrders[o.ID]; !ok {
		return ErrOrderNotFound
	}
	cp := *o
	m.lendOrders[o.ID] = &cp
	return nil
}

func (m *MemStore) CreateBorrowOrder(o *BorrowOrder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.borrowSeq++
	o.ID = m.borrowSeq
	cp := *o
	m.borrowOrders[o.ID] = &cp
	return nil
}

func (m *MemStore) GetBorrowOrder(id int64) (*BorrowOrder, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.borrowOrders[id]
	if !ok {
		return nil, ErrOrderNotFound
	}
	cp := *o
	return &cp, nil
}

func (m *MemStore) ListBorrowOrdersByUser(uid int64) ([]*BorrowOrder, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*BorrowOrder
	for _, o := range m.borrowOrders {
		if o.UserID == uid {
			cp := *o
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) ListBorrowOrdersByPool(pid int64) ([]*BorrowOrder, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*BorrowOrder
	for _, o := range m.borrowOrders {
		if o.PoolID == pid {
			cp := *o
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) ListAllBorrowOrders() ([]*BorrowOrder, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*BorrowOrder
	for _, o := range m.borrowOrders {
		cp := *o
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) ListActiveBorrowOrders() ([]*BorrowOrder, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*BorrowOrder
	for _, o := range m.borrowOrders {
		if o.Status == "active" {
			cp := *o
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) UpdateBorrowOrder(o *BorrowOrder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.borrowOrders[o.ID]; !ok {
		return ErrOrderNotFound
	}
	cp := *o
	m.borrowOrders[o.ID] = &cp
	return nil
}

func (m *MemStore) CreateInterestRecord(r *InterestRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.interestSeq++
	r.ID = m.interestSeq
	cp := *r
	m.interestRecs[r.ID] = &cp
	return nil
}

func (m *MemStore) ListInterestRecordsByUser(uid int64) ([]*InterestRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*InterestRecord
	for _, r := range m.interestRecs {
		if r.UserID == uid {
			cp := *r
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

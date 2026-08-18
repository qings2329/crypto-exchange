package staking

import (
	"sort"
	"sync"
)

// MemStore 是质押业务的纯内存实现（演示/测试/MySQL 不可用时的降级）。
type MemStore struct {
	mu          sync.RWMutex
	products    map[int64]*StakingProduct
	delegations map[int64]*StakingDelegation
	rewards     map[int64]*StakingReward
	prodSeq     int64
	delSeq      int64
	rewSeq      int64
}

// NewMemStore 构造内存存储。
func NewMemStore() *MemStore {
	return &MemStore{
		products:    map[int64]*StakingProduct{},
		delegations: map[int64]*StakingDelegation{},
		rewards:     map[int64]*StakingReward{},
	}
}

// ---- 产品 ----

func (m *MemStore) CreateProduct(p *StakingProduct) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prodSeq++
	p.ID = m.prodSeq
	cp := *p
	m.products[p.ID] = &cp
	return nil
}

func (m *MemStore) GetProduct(id int64) (*StakingProduct, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.products[id]
	if !ok {
		return nil, ErrProductNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *MemStore) ListProducts(status ProductStatus) ([]*StakingProduct, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*StakingProduct, 0, len(m.products))
	for _, p := range m.products {
		if status == "" || p.Status == status {
			cp := *p
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) CloseProduct(id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.products[id]
	if !ok {
		return ErrProductNotFound
	}
	p.Status = ProductClosed
	return nil
}

// ---- 委托 ----

func (m *MemStore) CreateDelegation(d *StakingDelegation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delSeq++
	d.ID = m.delSeq
	cp := *d
	m.delegations[d.ID] = &cp
	return nil
}

func (m *MemStore) GetDelegation(id int64) (*StakingDelegation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.delegations[id]
	if !ok {
		return nil, ErrDelegationNotFound
	}
	cp := *d
	return &cp, nil
}

func (m *MemStore) ListDelegationsByUser(uid int64) ([]*StakingDelegation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*StakingDelegation, 0, len(m.delegations))
	for _, d := range m.delegations {
		if d.UserID == uid {
			cp := *d
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) ListAllDelegations() ([]*StakingDelegation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*StakingDelegation, 0, len(m.delegations))
	for _, d := range m.delegations {
		cp := *d
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) UpdateDelegation(d *StakingDelegation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.delegations[d.ID]; !ok {
		return ErrDelegationNotFound
	}
	cp := *d
	m.delegations[d.ID] = &cp
	return nil
}

// ---- 奖励 ----

func (m *MemStore) CreateReward(r *StakingReward) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rewSeq++
	r.ID = m.rewSeq
	cp := *r
	m.rewards[r.ID] = &cp
	return nil
}

func (m *MemStore) ListRewardsByDelegation(did int64) ([]*StakingReward, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*StakingReward, 0, len(m.rewards))
	for _, r := range m.rewards {
		if r.DelegationID == did {
			cp := *r
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

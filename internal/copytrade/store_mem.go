package copytrade

import (
	"sort"
	"sync"
)

// MemStore 是跟单业务的纯内存实现。
type MemStore struct {
	mu        sync.RWMutex
	leads     map[int64]*LeadTrader
	follows   map[int64]*Follow
	copies    map[int64]*CopyRecord
	leadSeq   int64
	followSeq int64
	copySeq   int64
}

// NewMemStore 构造内存存储。
func NewMemStore() *MemStore {
	return &MemStore{
		leads:   map[int64]*LeadTrader{},
		follows: map[int64]*Follow{},
		copies:  map[int64]*CopyRecord{},
	}
}

func (m *MemStore) CreateLead(l *LeadTrader) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *l
	m.leads[cp.ID] = &cp
	return nil
}

func (m *MemStore) GetLead(id int64) (*LeadTrader, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	l, ok := m.leads[id]
	if !ok {
		return nil, ErrLeadNotFound
	}
	cp := *l
	return &cp, nil
}

func (m *MemStore) ListActiveLeads() ([]*LeadTrader, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*LeadTrader, 0, len(m.leads))
	for _, l := range m.leads {
		if l.Status == LeadActive {
			cp := *l
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) CloseLead(id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.leads[id]
	if !ok {
		return ErrLeadNotFound
	}
	l.Status = LeadClosed
	return nil
}

func (m *MemStore) ListAllLeads() ([]*LeadTrader, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*LeadTrader, 0, len(m.leads))
	for _, l := range m.leads {
		cp := *l
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) CreateFollow(f *Follow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ex := range m.follows {
		if ex.LeadID == f.LeadID && ex.FollowerID == f.FollowerID && ex.Status == FollowActive {
			return ErrAlreadyFollowing
		}
	}
	m.followSeq++
	f.ID = m.followSeq
	cp := *f
	m.follows[f.ID] = &cp
	return nil
}

func (m *MemStore) GetFollow(id int64) (*Follow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.follows[id]
	if !ok {
		return nil, ErrFollowNotFound
	}
	cp := *f
	return &cp, nil
}

func (m *MemStore) ListFollowsByLead(leadID int64) ([]*Follow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Follow, 0, len(m.follows))
	for _, f := range m.follows {
		if f.LeadID == leadID && f.Status == FollowActive {
			cp := *f
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) ListFollowsByFollower(uid int64) ([]*Follow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Follow, 0, len(m.follows))
	for _, f := range m.follows {
		if f.FollowerID == uid {
			cp := *f
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) UpdateFollow(f *Follow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.follows[f.ID]; !ok {
		return ErrFollowNotFound
	}
	cp := *f
	m.follows[f.ID] = &cp
	return nil
}

func (m *MemStore) ListAllFollows() ([]*Follow, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Follow, 0, len(m.follows))
	for _, f := range m.follows {
		cp := *f
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) CreateCopy(c *CopyRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.copySeq++
	c.ID = m.copySeq
	cp := *c
	m.copies[c.ID] = &cp
	return nil
}

func (m *MemStore) GetCopy(eventID string, followID int64) (*CopyRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, c := range m.copies {
		if c.EventID == eventID && c.FollowID == followID {
			cp := *c
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *MemStore) ListCopiesByFollower(uid int64) ([]*CopyRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*CopyRecord, 0, len(m.copies))
	for _, c := range m.copies {
		if c.FollowerID == uid {
			cp := *c
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) ListAllCopies() ([]*CopyRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*CopyRecord, 0, len(m.copies))
	for _, c := range m.copies {
		cp := *c
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

package options

import (
	"sync"
	"time"
)

// memStore 是内存版 Store，供单测与无 MySQL 的演示环境使用。
type memStore struct {
	mu              sync.RWMutex
	contracts       map[int64]*OptionContract
	positions       map[int64]*OptionPosition
	nextContractID  int64
	nextPositionID  int64
}

// NewMemStore 构造内存 Store。
func NewMemStore() Store {
	return &memStore{
		contracts:      make(map[int64]*OptionContract),
		positions:      make(map[int64]*OptionPosition),
		nextContractID: 1,
		nextPositionID: 1,
	}
}

func (s *memStore) CreateContract(c *OptionContract) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.ID == 0 {
		c.ID = s.nextContractID
		s.nextContractID++
	}
	now := time.Now()
	if c.CreatedAt.IsZero() {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	cp := *c
	s.contracts[cp.ID] = &cp
	return nil
}

func (s *memStore) GetContract(id int64) (*OptionContract, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.contracts[id]
	if !ok {
		return nil, ErrContractNotFound
	}
	cp := *c
	return &cp, nil
}

func (s *memStore) ListContracts() ([]*OptionContract, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*OptionContract, 0, len(s.contracts))
	for _, c := range s.contracts {
		cp := *c
		out = append(out, &cp)
	}
	return out, nil
}

func (s *memStore) UpsertPosition(p *OptionPosition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ID == 0 {
		p.ID = s.nextPositionID
		s.nextPositionID++
	}
	now := time.Now()
	if p.OpenedAt.IsZero() {
		p.OpenedAt = now
	}
	p.UpdatedAt = now
	cp := *p
	s.positions[cp.ID] = &cp
	return nil
}

func (s *memStore) GetPosition(id int64) (*OptionPosition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.positions[id]
	if !ok {
		return nil, ErrPositionNotFound
	}
	cp := *p
	return &cp, nil
}

func (s *memStore) ListPositions(userID int64) ([]*OptionPosition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*OptionPosition, 0)
	for _, p := range s.positions {
		if p.UserID == userID {
			cp := *p
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *memStore) ListAllPositions() ([]*OptionPosition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*OptionPosition, 0, len(s.positions))
	for _, p := range s.positions {
		cp := *p
		out = append(out, &cp)
	}
	return out, nil
}

func (s *memStore) ListAllOpen() ([]*OptionPosition, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*OptionPosition, 0)
	for _, p := range s.positions {
		if p.Status == StatusOpen {
			cp := *p
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *memStore) DeletePosition(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.positions, id)
	return nil
}

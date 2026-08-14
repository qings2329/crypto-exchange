package risk

import (
	"sync"
	"time"
)

// memStore 是内存实现，用于单测与无 MySQL 的 dev 降级。
type memStore struct {
	mu      sync.RWMutex
	seqR    int64
	seqB    int64
	seqE    int64
	rules   map[int64]*RiskRule
	blk     map[string]*BlacklistEntry // key: kind + ":" + target
	events  map[int64]*RiskEvent
}

// NewMemStore 返回内存实现的 Store。
func NewMemStore() Store {
	return &memStore{
		rules:  make(map[int64]*RiskRule),
		blk:    make(map[string]*BlacklistEntry),
		events: make(map[int64]*RiskEvent),
	}
}

func (s *memStore) UpsertRule(r *RiskRule) (*RiskRule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *r
	if cp.ID == 0 {
		s.seqR++
		cp.ID = s.seqR
		cp.CreatedAt = time.Now().UTC()
	} else {
		old, ok := s.rules[cp.ID]
		if !ok {
			return nil, ErrNotFound
		}
		cp.CreatedAt = old.CreatedAt
	}
	s.rules[cp.ID] = &cp
	return &cp, nil
}

func (s *memStore) GetRule(id int64) (*RiskRule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.rules[id]
	if !ok {
		return nil, ErrNotFound
	}
	return r, nil
}

func (s *memStore) ListRules(kind string) ([]*RiskRule, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*RiskRule, 0)
	for _, r := range s.rules {
		if kind != "" && r.Kind != kind {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *memStore) AddBlacklist(b *BlacklistEntry) (*BlacklistEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *b
	s.seqB++
	cp.ID = s.seqB
	cp.CreatedAt = time.Now().UTC()
	s.blk[b.Kind+":"+b.Target] = &cp
	return &cp, nil
}

func (s *memStore) RemoveBlacklist(target string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	deleted := false
	for k, v := range s.blk {
		if v.Target == target {
			delete(s.blk, k)
			deleted = true
		}
	}
	if !deleted {
		return ErrNotFound
	}
	return nil
}

func (s *memStore) IsBlacklisted(target string) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, v := range s.blk {
		if v.Target == target {
			return true, nil
		}
	}
	return false, nil
}

func (s *memStore) ListBlacklist(kind string) ([]*BlacklistEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*BlacklistEntry, 0)
	for _, v := range s.blk {
		if kind != "" && v.Kind != kind {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *memStore) RecordEvent(e *RiskEvent) (*RiskEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *e
	s.seqE++
	cp.ID = s.seqE
	cp.CreatedAt = time.Now().UTC()
	s.events[cp.ID] = &cp
	return &cp, nil
}

func (s *memStore) ListEvents(userID int64, limit int) ([]*RiskEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*RiskEvent, 0)
	for _, e := range s.events {
		if userID != 0 && e.UserID != userID {
			continue
		}
		out = append(out, e)
	}
	// 倒序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

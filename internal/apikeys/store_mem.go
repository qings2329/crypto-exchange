package apikeys

import (
	"sort"
	"sync"
	"time"
)

// 内存实现（无 MySQL 时回退 / 单元测试用）。非线程安全由 mu 保护。
type memStore struct {
	mu     sync.RWMutex
	seq    int64
	byID   map[int64]*APIKey
	byHash map[string]int64
}

// NewMemStore 构造内存实现。
func NewMemStore() Store {
	return &memStore{
		byID:   map[int64]*APIKey{},
		byHash: map[string]int64{},
	}
}

func (s *memStore) Create(k *APIKey) error {
	if k.UserID == 0 || k.Label == "" || k.Prefix == "" || k.KeyHash == "" {
		return ErrInvalidInput
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	now := time.Now()
	// 回填调用方传入的记录（与 MySQL 实现一致，便于 handler 直接取 View）。
	k.ID = s.seq
	k.Status = StatusActive
	k.CreatedAt = now
	k.RevokedAt = nil
	k.LastUsedAt = nil
	cp := *k
	s.byID[cp.ID] = &cp
	s.byHash[cp.KeyHash] = cp.ID
	return nil
}

func (s *memStore) GetByID(id int64) (*APIKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.byID[id]
	if !ok {
		return nil, ErrKeyNotFound
	}
	cp := *k
	return &cp, nil
}

func (s *memStore) List(f ListFilter) ([]*APIKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*APIKey, 0, len(s.byID))
	for _, k := range s.byID {
		if f.UserID != 0 && k.UserID != f.UserID {
			continue
		}
		cp := *k
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (s *memStore) ListByUser(userID int64) ([]*APIKey, error) {
	return s.List(ListFilter{UserID: userID})
}

func (s *memStore) GetByKeyHash(keyHash string) (*APIKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byHash[keyHash]
	if !ok {
		return nil, ErrKeyNotFound
	}
	cp := *s.byID[id]
	return &cp, nil
}

func (s *memStore) Revoke(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byID[id]
	if !ok {
		return ErrKeyNotFound
	}
	if k.Status == StatusRevoked {
		return ErrKeyRevoked
	}
	now := time.Now()
	k.Status = StatusRevoked
	k.RevokedAt = &now
	return nil
}

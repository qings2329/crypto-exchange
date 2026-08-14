package margin

import (
	"sync"
	"time"
)

// memStore 是内存版 Store，供单测与无 MySQL 的演示环境使用。
type memStore struct {
	mu      sync.RWMutex
	accounts map[string]*MarginAccount // key: userID\x00asset
}

// NewMemStore 构造内存 Store。
func NewMemStore() Store {
	return &memStore{accounts: make(map[string]*MarginAccount)}
}

func memKey(userID int64, asset string) string {
	return itoa(userID) + "\x00" + asset
}

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	buf := make([]byte, 0, 20)
	for i > 0 {
		buf = append([]byte{byte('0' + i%10)}, buf...)
		i /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

func (s *memStore) UpsertAccount(a *MarginAccount) error {
	now := time.Now()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	cp := *a
	s.mu.Lock()
	s.accounts[memKey(a.UserID, a.Asset)] = &cp
	s.mu.Unlock()
	return nil
}

func (s *memStore) GetAccount(userID int64, asset string) (*MarginAccount, error) {
	s.mu.RLock()
	a, ok := s.accounts[memKey(userID, asset)]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	cp := *a
	return &cp, nil
}

func (s *memStore) ListAccounts(userID int64) ([]*MarginAccount, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*MarginAccount, 0, len(s.accounts))
	prefix := itoa(userID) + "\x00"
	for k, a := range s.accounts {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			cp := *a
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *memStore) ListAllActive() ([]*MarginAccount, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*MarginAccount, 0)
	for _, a := range s.accounts {
		if a.Status == StatusActive {
			cp := *a
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *memStore) DeleteAccount(userID int64, asset string) error {
	s.mu.Lock()
	delete(s.accounts, memKey(userID, asset))
	s.mu.Unlock()
	return nil
}

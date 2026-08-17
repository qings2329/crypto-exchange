package user

import (
	"sync"
	"time"
)

// memStore 是 Store 的内存实现（单测 / 无 DB 开发用）。非线程安全的业务逻辑在
// Service 层加锁；此处用 RWMutex 保证并发安全以匹配 MySQL 实现语义。
type memStore struct {
	mu        sync.RWMutex
	users     map[int64]*User
	byEmail   map[string]int64
	byPhone   map[string]int64
	seq       int64
	codes     []*VerifyCode
	codeSeq   int64
	refreshes map[string]*RefreshToken
	kycs      map[int64]*KYCSubmission
	prefs     map[int64]*UserPreferences
}

// NewMemStore 构造内存存储。
func NewMemStore() Store {
	return &memStore{
		users:     make(map[int64]*User),
		byEmail:   make(map[string]int64),
		byPhone:   make(map[string]int64),
		refreshes: make(map[string]*RefreshToken),
		kycs:      make(map[int64]*KYCSubmission),
		prefs:     make(map[int64]*UserPreferences),
	}
}

func (s *memStore) CreateUser(u *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if u.Email != "" {
		if _, ok := s.byEmail[u.Email]; ok {
			return ErrUserExists
		}
	}
	if u.Phone != "" {
		if _, ok := s.byPhone[u.Phone]; ok {
			return ErrUserExists
		}
	}
	s.seq++
	u.ID = s.seq
	u.CreatedAt = now
	u.UpdatedAt = now
	cp := *u
	s.users[u.ID] = &cp
	if u.Email != "" {
		s.byEmail[u.Email] = u.ID
	}
	if u.Phone != "" {
		s.byPhone[u.Phone] = u.ID
	}
	return nil
}

func (s *memStore) GetByEmail(email string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byEmail[email]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneUser(s.users[id]), nil
}

func (s *memStore) GetByPhone(phone string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byPhone[phone]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneUser(s.users[id]), nil
}

func (s *memStore) GetByID(id int64) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[id]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneUser(u), nil
}

func (s *memStore) UpdateUser(u *User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.users[u.ID]
	if !ok {
		return ErrNotFound
	}
	// 邮箱/手机变更时同步反向索引
	if existing.Email != u.Email {
		delete(s.byEmail, existing.Email)
		if u.Email != "" {
			s.byEmail[u.Email] = u.ID
		}
	}
	if existing.Phone != u.Phone {
		delete(s.byPhone, existing.Phone)
		if u.Phone != "" {
			s.byPhone[u.Phone] = u.ID
		}
	}
	u.UpdatedAt = time.Now()
	cp := *u
	s.users[u.ID] = &cp
	return nil
}

func (s *memStore) ListAll() ([]*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, cloneUser(u))
	}
	return out, nil
}

func (s *memStore) SaveCode(c *VerifyCode) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codeSeq++
	c.ID = s.codeSeq
	c.CreatedAt = time.Now()
	cp := *c
	s.codes = append(s.codes, &cp)
	return nil
}

func (s *memStore) GetLatestCode(target, purpose string) (*VerifyCode, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var latest *VerifyCode
	for _, c := range s.codes {
		if c.Target == target && c.Purpose == purpose {
			if latest == nil || c.ID > latest.ID {
				latest = c
			}
		}
	}
	if latest == nil {
		return nil, ErrNotFound
	}
	cp := *latest
	return &cp, nil
}

func (s *memStore) ConsumeCode(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.codes {
		if c.ID == id {
			c.Consumed = true
			return nil
		}
	}
	return ErrNotFound
}

func (s *memStore) SaveRefresh(rt *RefreshToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rt.CreatedAt = time.Now()
	cp := *rt
	s.refreshes[rt.TokenHash] = &cp
	return nil
}

func (s *memStore) GetRefresh(tokenHash string) (*RefreshToken, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rt, ok := s.refreshes[tokenHash]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *rt
	return &cp, nil
}

func (s *memStore) DeleteRefresh(tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.refreshes, tokenHash)
	return nil
}

func (s *memStore) DeleteUserRefreshes(userID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, rt := range s.refreshes {
		if rt.UserID == userID {
			delete(s.refreshes, h)
		}
	}
	return nil
}

func (s *memStore) SaveKYC(k *KYCSubmission) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *k
	s.kycs[k.UserID] = &cp
	return nil
}

func (s *memStore) GetKYC(userID int64) (*KYCSubmission, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.kycs[userID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *k
	return &cp, nil
}

func (s *memStore) UpdateKYC(k *KYCSubmission) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *k
	s.kycs[k.UserID] = &cp
	return nil
}

func (s *memStore) GetPreferences(userID int64) (*UserPreferences, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.prefs[userID]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (s *memStore) UpdatePreferences(p *UserPreferences) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.UpdatedAt = time.Now()
	cp := *p
	s.prefs[p.UserID] = &cp
	return nil
}

func cloneUser(u *User) *User {
	cp := *u
	return &cp
}

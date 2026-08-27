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
	byRefCode map[string]int64 // referral_code -> user_id
	seq       int64
	codes     []*VerifyCode
	codeSeq   int64
	refreshes map[string]*RefreshToken
	kycs      map[int64]*KYCSubmission
	prefs     map[int64]*UserPreferences

	apiKeys      map[int64][]*ApiKey    // userID -> keys（倒序展示由读取层处理）
	logins       map[int64][]*LoginEntry // userID -> 登录历史（追加）
	loginSeq     int64
	sessions     map[int64][]*Session   // userID -> 会话
	antiPhishing map[int64]string       // userID -> 防钓鱼码
}

// NewMemStore 构造内存存储。
func NewMemStore() Store {
	return &memStore{
		users:     make(map[int64]*User),
		byEmail:   make(map[string]int64),
		byPhone:   make(map[string]int64),
		byRefCode: make(map[string]int64),
		refreshes: make(map[string]*RefreshToken),
		kycs:      make(map[int64]*KYCSubmission),
		prefs:     make(map[int64]*UserPreferences),

		apiKeys:      make(map[int64][]*ApiKey),
		logins:       make(map[int64][]*LoginEntry),
		sessions:     make(map[int64][]*Session),
		antiPhishing: make(map[int64]string),
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
	if u.ReferralCode != "" {
		if _, ok := s.byRefCode[u.ReferralCode]; ok {
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
	if u.ReferralCode != "" {
		s.byRefCode[u.ReferralCode] = u.ID
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

func (s *memStore) GetByReferralCode(code string) (*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byRefCode[code]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneUser(s.users[id]), nil
}

func (s *memStore) GetReferrals(userID int64) ([]*User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*User
	for _, u := range s.users {
		if u.ReferrerID == userID {
			out = append(out, cloneUser(u))
		}
	}
	return out, nil
}

// ---- 安全中心：API Key ----

func (s *memStore) CreateApiKey(k *ApiKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	k.ID = s.seq
	if k.Permissions == nil {
		k.Permissions = []string{}
	}
	if k.IPWhitelist == nil {
		k.IPWhitelist = []string{}
	}
	k.CreatedAt = time.Now().UTC()
	s.apiKeys[k.UserID] = append(s.apiKeys[k.UserID], k)
	return nil
}

func (s *memStore) ListApiKeys(userID int64) ([]*ApiKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.apiKeys[userID]
	out := make([]*ApiKey, 0, len(list))
	// 倒序（最新在前）。
	for i := len(list) - 1; i >= 0; i-- {
		out = append(out, cloneApiKey(list[i]))
	}
	return out, nil
}

func (s *memStore) UpdateApiKeyStatus(userID, id int64, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.apiKeys[userID] {
		if k.ID == id {
			k.Status = status
			return nil
		}
	}
	return ErrNotFound
}

func (s *memStore) DeleteApiKey(userID, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.apiKeys[userID]
	for i, k := range list {
		if k.ID == id {
			s.apiKeys[userID] = append(list[:i], list[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

// ---- 安全中心：登录历史 ----

func (s *memStore) RecordLogin(e *LoginEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loginSeq++
	e.ID = s.loginSeq
	s.logins[e.UserID] = append(s.logins[e.UserID], e)
	return nil
}

func (s *memStore) ListLoginHistory(userID int64, limit int) ([]*LoginEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.logins[userID]
	out := make([]*LoginEntry, 0, len(list))
	for i := len(list) - 1; i >= 0 && len(out) < limit; i-- {
		cp := *list[i]
		out = append(out, &cp)
	}
	return out, nil
}

// ---- 安全中心：会话 ----

func (s *memStore) CreateSession(sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *sess
	s.sessions[sess.UserID] = append(s.sessions[sess.UserID], &cp)
	return nil
}

func (s *memStore) ListSessions(userID int64) ([]*Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := s.sessions[userID]
	out := make([]*Session, 0, len(list))
	for _, sess := range list {
		cp := *sess
		out = append(out, &cp)
	}
	return out, nil
}

func (s *memStore) TouchSession(userID int64, sessionID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions[userID] {
		if sess.ID == sessionID {
			sess.LastActiveAt = at
			return nil
		}
	}
	return ErrNotFound
}

func (s *memStore) DeleteSession(userID int64, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.sessions[userID]
	for i, sess := range list {
		if sess.ID == sessionID {
			s.sessions[userID] = append(list[:i], list[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

func (s *memStore) DeleteOtherSessions(userID int64, keepID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.sessions[userID]
	var kept []*Session
	var removed int64
	for _, sess := range list {
		if sess.ID == keepID {
			kept = append(kept, sess)
		} else {
			removed++
		}
	}
	s.sessions[userID] = kept
	return removed, nil
}

// ---- 安全中心：防钓鱼码 ----

func (s *memStore) GetAntiPhishing(userID int64) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.antiPhishing[userID], nil
}

func (s *memStore) SetAntiPhishing(userID int64, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if code == "" {
		delete(s.antiPhishing, userID)
		return nil
	}
	s.antiPhishing[userID] = code
	return nil
}

func cloneApiKey(k *ApiKey) *ApiKey {
	cp := *k
	cp.Permissions = append([]string(nil), k.Permissions...)
	cp.IPWhitelist = append([]string(nil), k.IPWhitelist...)
	return &cp
}

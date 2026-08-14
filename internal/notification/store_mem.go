package notification

import (
	"sync"
	"time"
)

// memStore 是内存实现，用于单测与无 MySQL 的 dev 降级。进程退出即丢失。
type memStore struct {
	mu  sync.RWMutex
	seq int64
	all map[int64]*Notification
}

// newMemStore 创建内存存储。
func newMemStore() *memStore {
	return &memStore{all: make(map[int64]*Notification)}
}

// NewMemStore 返回内存实现的 Store，供无 MySQL 的 dev 降级与单测使用。
func NewMemStore() Store {
	return newMemStore()
}

func (s *memStore) Create(n *Notification) (*Notification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	cp := *n
	cp.ID = s.seq
	cp.Status = StatusUnread
	cp.CreatedAt = time.Now().UTC()
	s.all[cp.ID] = &cp
	return &cp, nil
}

func (s *memStore) List(userID int64, onlyUnread bool, limit int) ([]*Notification, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Notification, 0)
	for _, n := range s.all {
		if n.UserID != userID {
			continue
		}
		if onlyUnread && n.Status != StatusUnread {
			continue
		}
		out = append(out, n)
	}
	// 按时间倒序（ID 单调递增近似）。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *memStore) ListAll(limit int) ([]*Notification, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Notification, 0, len(s.all))
	for _, n := range s.all {
		out = append(out, n)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *memStore) MarkRead(userID, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.all[id]
	if !ok || n.UserID != userID {
		return ErrNotFound
	}
	n.Status = StatusRead
	return nil
}

func (s *memStore) MarkAllRead(userID int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cnt int64
	for _, n := range s.all {
		if n.UserID == userID && n.Status == StatusUnread {
			n.Status = StatusRead
			cnt++
		}
	}
	return cnt, nil
}

func (s *memStore) CountUnread(userID int64) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var cnt int64
	for _, n := range s.all {
		if n.UserID == userID && n.Status == StatusUnread {
			cnt++
		}
	}
	return cnt, nil
}

package notification

import (
	"sort"
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
	// 按 ID 倒序（seq 单调递增近似时间倒序），确保与 MySQL 实现一致且确定。
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
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
	// 按 ID 倒序（seq 单调递增近似时间倒序），确保与 MySQL 实现一致且确定。
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListSince 返回 ID 严格大于 minID 的通知（按 ID 升序），供实时推送增量轮询。
func (s *memStore) ListSince(minID int64, limit int) ([]*Notification, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Notification, 0)
	for _, n := range s.all {
		if n.ID > minID {
			out = append(out, n)
		}
	}
	// 按 ID 升序（seq 单调递增）。
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
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

func (s *memStore) Delete(userID, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.all[id]
	if !ok || n.UserID != userID {
		return ErrNotFound
	}
	delete(s.all, id)
	return nil
}

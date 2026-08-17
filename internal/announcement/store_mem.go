package announcement

import (
	"sync"
	"time"
)

// memStore 是 Store 的内存实现（单测 / 无 DB 开发用），用 RWMutex 保证并发安全。
type memStore struct {
	mu    sync.RWMutex
	items map[int64]*Announcement
	seq   int64
}

// NewMemStore 构造内存存储。
func NewMemStore() Store {
	return &memStore{items: make(map[int64]*Announcement)}
}

func (s *memStore) Create(a *Announcement) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.seq++
	a.ID = s.seq
	a.CreatedAt = now
	a.UpdatedAt = now
	cp := *a
	s.items[a.ID] = &cp
	return nil
}

func (s *memStore) Update(a *Announcement) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.items[a.ID]
	if !ok {
		return ErrNotFound
	}
	a.CreatedAt = existing.CreatedAt
	a.UpdatedAt = time.Now()
	cp := *a
	s.items[a.ID] = &cp
	return nil
}

func (s *memStore) Delete(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[id]; !ok {
		return ErrNotFound
	}
	delete(s.items, id)
	return nil
}

func (s *memStore) Get(id int64) (*Announcement, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.items[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *a
	return &cp, nil
}

func (s *memStore) ListAll() ([]*Announcement, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Announcement, 0, len(s.items))
	for _, a := range s.items {
		cp := *a
		out = append(out, &cp)
	}
	sortByPublishedDesc(out)
	return out, nil
}

func (s *memStore) ListActive() ([]*Announcement, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Announcement, 0, len(s.items))
	for _, a := range s.items {
		if a.Active {
			cp := *a
			out = append(out, &cp)
		}
	}
	sortByPublishedDesc(out)
	return out, nil
}

// sortByPublishedDesc 按发布时间倒序，其次按 ID 倒序（保证稳定且草稿/未发布靠后）。
func sortByPublishedDesc(list []*Announcement) {
	for i := 0; i < len(list); i++ {
		for j := i + 1; j < len(list); j++ {
			if list[j].PublishedAt.After(list[i].PublishedAt) ||
				(list[j].PublishedAt.Equal(list[i].PublishedAt) && list[j].ID > list[i].ID) {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
}

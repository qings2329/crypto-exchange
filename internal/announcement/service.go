package announcement

import (
	"time"
)

// Service 实现公告业务（CRUD / 列表 / 发布态）。
type Service struct {
	store Store
}

// NewService 构造公告服务。
func NewService(store Store) *Service {
	return &Service{store: store}
}

// ListActive 返回对外发布的公告（按发布时间倒序），供首页/公告列表展示。
func (s *Service) ListActive() ([]*Announcement, error) {
	return s.store.ListActive()
}

// ListAll 返回全部公告（含草稿），供管理后台使用。
func (s *Service) ListAll() ([]*Announcement, error) {
	return s.store.ListAll()
}

// Get 取回单条公告。
func (s *Service) Get(id int64) (*Announcement, error) {
	return s.store.Get(id)
}

// Create 创建公告。校验等级与字段长度；发布态(true)且未指定发布时间时自动填充为当前时间。
func (s *Service) Create(in AnnouncementInput) (*Announcement, error) {
	a := &Announcement{}
	// 创建时必须提供标题（更新允许保持不变，故在 applyInput 之外单独校验）。
	if in.Title == nil {
		return nil, ErrTitleRequired
	}
	if in.Level == nil {
		a.Level = LevelInfo // 缺省等级
	}
	if err := applyInput(a, in); err != nil {
		return nil, err
	}
	if a.Active && a.PublishedAt.IsZero() {
		a.PublishedAt = time.Now()
	}
	if err := s.store.Create(a); err != nil {
		return nil, err
	}
	return a, nil
}

// Update 更新已存在公告。仅对提供的字段做补丁；切到发布态且尚无发布时间时自动填充。
func (s *Service) Update(id int64, in AnnouncementInput) (*Announcement, error) {
	a, err := s.store.Get(id)
	if err != nil {
		return nil, err
	}
	if err := applyInput(a, in); err != nil {
		return nil, err
	}
	if a.Active && a.PublishedAt.IsZero() {
		a.PublishedAt = time.Now()
	}
	if err := s.store.Update(a); err != nil {
		return nil, err
	}
	return a, nil
}

// Delete 删除公告。
func (s *Service) Delete(id int64) error {
	return s.store.Delete(id)
}

// applyInput 把入参补丁应用到公告对象，并做校验（等级合法性、长度上限）。
func applyInput(a *Announcement, in AnnouncementInput) error {
	if in.Level != nil {
		if !validLevel(*in.Level) {
			return ErrInvalidLevel
		}
		a.Level = *in.Level
	}
	if in.Title != nil {
		if len([]rune(*in.Title)) == 0 {
			return ErrTitleRequired
		}
		if len([]rune(*in.Title)) > maxTitleLen {
			return ErrTitleTooLong
		}
		a.Title = *in.Title
	}
	if in.Content != nil {
		if len([]rune(*in.Content)) > maxContentLen {
			return ErrContentTooLong
		}
		a.Content = *in.Content
	}
	if in.Active != nil {
		a.Active = *in.Active
	}
	return nil
}

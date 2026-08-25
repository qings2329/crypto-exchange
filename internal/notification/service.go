package notification

import "fmt"

// Service 是通知业务逻辑层，仅依赖 Store 接口。
type Service struct {
	store Store
}

// New 创建通知服务。
func New(store Store) *Service {
	return &Service{store: store}
}

// Publish 发布一条通知（供其它业务线调用，如 user/KYC、risk、ledger 充提）。
func (s *Service) Publish(in PublishInput) (*Notification, error) {
	if in.UserID <= 0 {
		return nil, fmt.Errorf("invalid user_id")
	}
	if in.Title == "" {
		return nil, fmt.Errorf("title required")
	}
	if !validType(in.Type) {
		in.Type = TypeSystem
	}
	return s.store.Create(&Notification{
		UserID: in.UserID,
		Type:   in.Type,
		Title:  in.Title,
		Body:   in.Body,
	})
}

// List 返回某用户通知（onlyUnread 控制是否仅未读）。
func (s *Service) List(userID int64, onlyUnread bool, limit int) ([]*Notification, error) {
	return s.store.List(userID, onlyUnread, limit)
}

// ListAll 返回全部通知（运营/排查用）。
func (s *Service) ListAll(limit int) ([]*Notification, error) {
	return s.store.ListAll(limit)
}

// UnreadCount 返回未读数量。
func (s *Service) UnreadCount(userID int64) (int64, error) {
	return s.store.CountUnread(userID)
}

// MarkRead 标记单条已读。
func (s *Service) MarkRead(userID, id int64) error {
	return s.store.MarkRead(userID, id)
}

// MarkAllRead 标记全部已读，返回受影响条数。
func (s *Service) MarkAllRead(userID int64) (int64, error) {
	return s.store.MarkAllRead(userID)
}

// Delete 删除某用户的一条通知。
func (s *Service) Delete(userID, id int64) error {
	return s.store.Delete(userID, id)
}

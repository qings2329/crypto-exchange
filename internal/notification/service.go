package notification

import "fmt"

// Service 是通知业务逻辑层，仅依赖 Store 接口。
type Service struct {
	store Store
	// pusher 为可选的实时推送钩子（如经 WebSocket Hub 推送给对应用户）。
	// Publish 成功写入后异步触发；设为 nil 则仅落库（前端仍可靠轮询兜底）。
	pusher func(*Notification)
}

// New 创建通知服务。
func New(store Store) *Service {
	return &Service{store: store}
}

// SetPusher 设置实时推送钩子：通知落库后推送给订阅方（如 WebSocket Hub）。
// 传入 nil 可清除。实时推送失败不应影响主流程。
func (s *Service) SetPusher(p func(*Notification)) {
	s.pusher = p
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
	n, err := s.store.Create(&Notification{
		UserID: in.UserID,
		Type:   in.Type,
		Title:  in.Title,
		Body:   in.Body,
	})
	if err != nil {
		return nil, err
	}
	// 实时推送：落库成功后触发（如 WS Hub 推送给该用户）。失败不影响主流程。
	if s.pusher != nil {
		s.pusher(n)
	}
	return n, nil
}

// List 返回某用户通知（onlyUnread 控制是否仅未读）。
func (s *Service) List(userID int64, onlyUnread bool, limit int) ([]*Notification, error) {
	return s.store.List(userID, onlyUnread, limit)
}

// ListAll 返回全部通知（运营/排查用）。
func (s *Service) ListAll(limit int) ([]*Notification, error) {
	return s.store.ListAll(limit)
}

// ListSince 返回 ID 严格大于 minID 的通知（升序），供实时推送增量轮询。
func (s *Service) ListSince(minID, limit int) ([]*Notification, error) {
	return s.store.ListSince(int64(minID), limit)
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

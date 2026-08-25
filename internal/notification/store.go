package notification

// Store 是通知的持久化抽象。Service 只依赖此接口，不感知底层实现。
type Store interface {
	// Create 写入一条通知，返回带 ID 与 CreatedAt 的对象。
	Create(n *Notification) (*Notification, error)
	// List 返回某用户的通知，按时间倒序；onlyUnread=true 时仅未读。
	List(userID int64, onlyUnread bool, limit int) ([]*Notification, error)
	// ListAll 返回全部用户的通知（运营/排查用），按时间倒序。
	ListAll(limit int) ([]*Notification, error)
	// MarkRead 将某条通知标记为已读；仅当属于该用户时生效。
	MarkRead(userID, id int64) error
	// MarkAllRead 将该用户全部未读标记为已读。
	MarkAllRead(userID int64) (int64, error)
	// CountUnread 返回某用户未读数量。
	CountUnread(userID int64) (int64, error)
	// Delete 删除某用户的一条通知；不属于该用户或不存在时返回 ErrNotFound。
	Delete(userID, id int64) error
}

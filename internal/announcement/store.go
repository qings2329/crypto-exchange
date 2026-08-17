package announcement

// Store 是公告服务的持久化抽象。生产用 MySQL 实现（ce_ 前缀表），
// 单测与无 DB 环境用内存实现。两种实现语义必须一致。
type Store interface {
	Create(a *Announcement) error
	Update(a *Announcement) error
	Delete(id int64) error
	Get(id int64) (*Announcement, error)
	ListAll() ([]*Announcement, error)
	ListActive() ([]*Announcement, error)
}

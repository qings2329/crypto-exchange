package copytrade

// Store 是跟单业务的持久化接口（MySQL 优先，失败回退内存）。
type Store interface {
	CreateLead(l *LeadTrader) error
	GetLead(id int64) (*LeadTrader, error)
	ListActiveLeads() ([]*LeadTrader, error)
	ListAllLeads() ([]*LeadTrader, error)
	CloseLead(id int64) error

	CreateFollow(f *Follow) error
	GetFollow(id int64) (*Follow, error)
	ListFollowsByLead(leadID int64) ([]*Follow, error)
	ListFollowsByFollower(uid int64) ([]*Follow, error)
	ListAllFollows() ([]*Follow, error)
	UpdateFollow(f *Follow) error

	CreateCopy(c *CopyRecord) error
	GetCopy(eventID string, followID int64) (*CopyRecord, error)
	ListCopiesByFollower(uid int64) ([]*CopyRecord, error)
	ListAllCopies() ([]*CopyRecord, error)
}

package referral

// Store 佣金记录存储接口。
type Store interface {
	// RecordCommission 记录一笔佣金（幂等：按 bizRef 去重）
	RecordCommission(c *ReferralCommission) error
	// GetCommissionByRef 按业务 ref 查询佣金记录
	GetCommissionByRef(bizRef string) (*ReferralCommission, error)
	// ListCommissionsByReferrer 按邀请人分页查询佣金
	ListCommissionsByReferrer(referrerID int64, limit, offset int) ([]*ReferralCommission, int, error)
	// ListAll 管理后台分页查询所有佣金
	ListAll(limit, offset int) ([]*ReferralCommission, int, error)
	// TotalByReferrer 按邀请人汇总佣金总额
	TotalByReferrer(referrerID int64) (map[string]int64, error)
}

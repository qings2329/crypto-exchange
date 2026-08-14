package margin

// Store 是杠杆账户的持久化抽象。Service 只依赖该接口，不关心底层是内存还是 MySQL。
type Store interface {
	// UpsertAccount 写入或更新一条杠杆账户（按 user_id + asset 唯一）。
	UpsertAccount(a *MarginAccount) error
	// GetAccount 取单条活跃/历史账户；不存在返回 ErrNotFound。
	GetAccount(userID int64, asset string) (*MarginAccount, error)
	// ListAccounts 取某用户全部账户（含已平仓/强平）。
	ListAccounts(userID int64) ([]*MarginAccount, error)
	// ListAllActive 取全量活跃账户（供后台计息/强平循环使用）。
	ListAllActive() ([]*MarginAccount, error)
	// DeleteAccount 删除某用户某资产账户。
	DeleteAccount(userID int64, asset string) error
}

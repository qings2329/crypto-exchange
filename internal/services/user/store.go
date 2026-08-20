package user

// Store 是用户服务的持久化抽象。生产用 MySQL 实现（ce_ 前缀表），
// 单测与无 DB 环境用内存实现。两种实现语义必须一致。
type Store interface {
	// 用户
	CreateUser(u *User) error
	GetByEmail(email string) (*User, error)
	GetByPhone(phone string) (*User, error)
	GetByID(id int64) (*User, error)
	UpdateUser(u *User) error
	ListAll() ([]*User, error)

	// 验证码
	SaveCode(c *VerifyCode) error
	GetLatestCode(target, purpose string) (*VerifyCode, error)
	ConsumeCode(id int64) error

	// 刷新令牌
	SaveRefresh(rt *RefreshToken) error
	GetRefresh(tokenHash string) (*RefreshToken, error)
	DeleteRefresh(tokenHash string) error
	DeleteUserRefreshes(userID int64) error

	// KYC
	SaveKYC(k *KYCSubmission) error
	GetKYC(userID int64) (*KYCSubmission, error)
	UpdateKYC(k *KYCSubmission) error

	// 个人偏好设置
	GetPreferences(userID int64) (*UserPreferences, error)
	UpdatePreferences(p *UserPreferences) error

	// 邀请
	GetByReferralCode(code string) (*User, error)
	GetReferrals(userID int64) ([]*User, error)
}

package user

import "time"

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

	// 安全中心：API Key
	CreateApiKey(k *ApiKey) error
	ListApiKeys(userID int64) ([]*ApiKey, error)
	UpdateApiKeyStatus(userID, id int64, status string) error
	DeleteApiKey(userID, id int64) error

	// 安全中心：登录历史
	RecordLogin(e *LoginEntry) error
	ListLoginHistory(userID int64, limit int) ([]*LoginEntry, error)

	// 安全中心：会话
	CreateSession(sess *Session) error
	ListSessions(userID int64) ([]*Session, error)
	TouchSession(userID int64, sessionID string, at time.Time) error
	DeleteSession(userID int64, sessionID string) error
	DeleteOtherSessions(userID int64, keepID string) (int64, error)

	// 安全中心：防钓鱼码
	GetAntiPhishing(userID int64) (string, error)
	SetAntiPhishing(userID int64, code string) error
}

package user

import (
	"errors"
	"time"
)

// 用户状态。
type Status int8

const (
	StatusNormal Status = 0
	StatusFrozen Status = 1
)

// KYC 等级（状态机：None -> Pending -> Verified / Rejected）。
type KYCLevel int8

const (
	KYCNone     KYCLevel = 0
	KYCPending  KYCLevel = 1
	KYCVerified KYCLevel = 2
	KYCRejected KYCLevel = 3
)

// 验证码用途。
const (
	PurposeRegister = "register" // 注册时校验账号归属
	PurposeVerify   = "verify"   // 注册后补充验证（如邮箱验证）
	PurposeReset    = "reset"    // 找回密码
	PurposeLogin    = "login"    // 登录二次验证（可选）
)

// User 是用户领域模型。密码不以明文存储，PassHash 为 bcrypt 哈希。
type User struct {
	ID           int64
	Email        string
	Phone        string
	PassHash     string
	Status       Status
	KYCLevel     KYCLevel
	TFASecret    string // TOTP 密钥（base32），启用 2FA 后写入；生产应加密存储
	TFAEnabled   bool
	EmailVerified bool
	PhoneVerified bool
	Nickname     string // 昵称（个人设置可编辑，可选）
	Avatar       string // 头像 URL（个人设置可编辑，可选）
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// UserPreferences 是用户个人偏好设置（与核心账户解耦，独立存储）。
type UserPreferences struct {
	UserID         int64     `json:"user_id"`
	Language       string    `json:"language"`        // 界面语言，如 zh-CN / en
	Theme          string    `json:"theme"`           // 主题，如 light / dark
	Timezone       string    `json:"timezone"`        // IANA 时区；空字符串 "" 表示跟随系统
	NotifyOrder    bool      `json:"notify_order"`    // 订单相关通知
	NotifySecurity bool      `json:"notify_security"` // 安全相关通知（登录/改密等）
	NotifyMarketing bool     `json:"notify_marketing"` // 营销推送
	UpdatedAt      time.Time `json:"updated_at"`
}

// VerifyCode 是一次性验证码（邮箱/短信）。
type VerifyCode struct {
	ID        int64
	UserID    int64 // 预注册阶段可能为 0，用 Target 定位
	Target    string // email 或 phone
	Purpose   string
	Code      string
	ExpiresAt time.Time
	Consumed  bool
	CreatedAt time.Time
}

// RefreshToken 是刷新令牌（仅存哈希，原始值只返回一次）。
type RefreshToken struct {
	ID        int64
	UserID    int64
	TokenHash string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// KYCSubmission 是 KYC 提交材料与审核状态。
type KYCSubmission struct {
	UserID      int64
	RealName    string
	IDType      string // 证件类型：id_card / passport / driver_license
	IDNumber    string
	DocFront    string // 证件正面（URL/引用）
	DocBack     string // 证件背面
	Status      KYCLevel
	RejectReason string
	SubmittedAt time.Time
	ReviewedAt  time.Time
	Reviewer    string
}

// 业务错误（调用方据此映射 HTTP 状态码）。
var (
	ErrUserExists      = errors.New("user already exists")
	ErrNotFound        = errors.New("user not found")
	ErrWrongPassword   = errors.New("invalid credential")
	ErrInvalidCode     = errors.New("invalid code")
	ErrCodeExpired     = errors.New("code expired")
	ErrCodeConsumed    = errors.New("code already used")
	ErrTFARequired     = errors.New("tfa code required")
	ErrTFAFailed       = errors.New("tfa verification failed")
	ErrTFANotEnabled   = errors.New("tfa not enabled")
	ErrInvalidAccount  = errors.New("invalid account format")
	ErrKYCPending      = errors.New("kyc already pending")
	ErrKYCNotPending   = errors.New("kyc submission not pending")
	ErrFrozen          = errors.New("user frozen")
	ErrRefreshInvalid  = errors.New("invalid refresh token")
	ErrSamePassword    = errors.New("new password must differ from current")
	ErrInvalidPref     = errors.New("invalid preferences")
	ErrNicknameTooLong = errors.New("nickname too long")
	ErrAvatarTooLong   = errors.New("avatar url too long")
	ErrPasswordTooShort = errors.New("password too short")
)

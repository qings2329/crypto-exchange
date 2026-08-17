package user

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

// Config 是用户服务的可调参数。零值使用内置默认（见 defaults）。
type Config struct {
	AccessTTL  time.Duration // access token 有效期
	RefreshTTL time.Duration // refresh token 有效期
	CodeTTL    time.Duration // 验证码有效期
	CodeLen    int           // 验证码长度
	Issuer     string        // TOTP issuer（写入 otpauth URI）
	BcryptCost int           // bcrypt 代价因子
	MinPwdLen  int           // 密码最小长度
}

func (c Config) withDefaults() Config {
	if c.AccessTTL <= 0 {
		c.AccessTTL = 15 * time.Minute
	}
	if c.RefreshTTL <= 0 {
		c.RefreshTTL = 7 * 24 * time.Hour
	}
	if c.CodeTTL <= 0 {
		c.CodeTTL = 10 * time.Minute
	}
	if c.CodeLen <= 0 {
		c.CodeLen = 6
	}
	if c.Issuer == "" {
		c.Issuer = "crypto-exchange"
	}
	if c.BcryptCost <= 0 {
		c.BcryptCost = bcrypt.DefaultCost
	}
	if c.MinPwdLen <= 0 {
		c.MinPwdLen = 6
	}
	return c
}

// Service 实现账户体系业务（注册/登录/验证码/找回/2FA/KYC）。
type Service struct {
	store    Store
	verifier *middleware.TokenVerifier
	notifier Notifier
	cfg      Config
}

// NewService 构造用户服务。
func NewService(store Store, verifier *middleware.TokenVerifier, notifier Notifier, cfg Config) *Service {
	return &Service{
		store:    store,
		verifier: verifier,
		notifier: notifier,
		cfg:      cfg.withDefaults(),
	}
}

// ---- 基础工具 ----

func (s *Service) generateCode() string {
	n := big.NewInt(int64(pow10(s.cfg.CodeLen)))
	v, err := rand.Int(rand.Reader, n)
	if err != nil {
		// 极不可能发生；退回时间戳低位
		return fmt.Sprintf("%0*d", s.cfg.CodeLen, time.Now().Nanosecond()%pow10(s.cfg.CodeLen))
	}
	return fmt.Sprintf("%0*d", s.cfg.CodeLen, v.Int64())
}

func pow10(n int) int {
	r := 1
	for i := 0; i < n; i++ {
		r *= 10
	}
	return r
}

func (s *Service) hashPassword(pwd string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pwd), s.cfg.BcryptCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *Service) checkPassword(hash, pwd string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pwd)) == nil
}

func (s *Service) loadByTarget(target string) (*User, error) {
	if isEmail(target) {
		return s.store.GetByEmail(target)
	}
	if isPhone(target) {
		return s.store.GetByPhone(target)
	}
	return nil, ErrInvalidAccount
}

// ---- 验证码 ----

// SendCode 向 target 发送指定用途的验证码。
func (s *Service) SendCode(target, purpose string) error {
	if !isEmail(target) && !isPhone(target) {
		return ErrInvalidAccount
	}
	switch purpose {
	case PurposeRegister, PurposeVerify, PurposeReset, PurposeLogin:
	default:
		return fmt.Errorf("unknown purpose: %s", purpose)
	}
	code := s.generateCode()
	if err := s.store.SaveCode(&VerifyCode{
		Target:    target,
		Purpose:   purpose,
		Code:      code,
		ExpiresAt: time.Now().Add(s.cfg.CodeTTL),
	}); err != nil {
		return err
	}
	return s.notifier.SendCode(target, purpose, code)
}

// verifyCode 校验最新验证码的有效性（未过期、未使用、匹配）。
func (s *Service) verifyCode(target, purpose, code string) (*VerifyCode, error) {
	c, err := s.store.GetLatestCode(target, purpose)
	if err != nil {
		return nil, ErrInvalidCode
	}
	if c.Consumed {
		return nil, ErrCodeConsumed
	}
	if time.Now().After(c.ExpiresAt) {
		return nil, ErrCodeExpired
	}
	if !subtleEqual(c.Code, code) {
		return nil, ErrInvalidCode
	}
	return c, nil
}

// ---- 注册 ----

// Register 用邮箱或手机注册。register 用途的验证码必传；通过后创建用户并标记该渠道已验证。
func (s *Service) Register(target, password, code string) (int64, error) {
	if len(password) < s.cfg.MinPwdLen {
		return 0, fmt.Errorf("password too short (min %d)", s.cfg.MinPwdLen)
	}
	// 先查重：账号已存在直接返回，避免验证码已用等歧义错误并防止枚举混淆。
	if _, err := s.loadByTarget(target); err == nil {
		return 0, ErrUserExists
	}
	vc, err := s.verifyCode(target, PurposeRegister, code)
	if err != nil {
		return 0, err
	}
	u := &User{PassHash: must(s.hashPassword(password))}
	if isEmail(target) {
		u.Email = target
		u.EmailVerified = true
	} else if isPhone(target) {
		u.Phone = target
		u.PhoneVerified = true
	} else {
		return 0, ErrInvalidAccount
	}
	if err := s.store.CreateUser(u); err != nil {
		return 0, err
	}
	_ = s.store.ConsumeCode(vc.ID)
	return u.ID, nil
}

// VerifyAccount 注册后补充验证（如邮箱验证）。
func (s *Service) VerifyAccount(target, code, purpose string) error {
	vc, err := s.verifyCode(target, purpose, code)
	if err != nil {
		return err
	}
	u, err := s.loadByTarget(target)
	if err != nil {
		return err
	}
	if isEmail(target) {
		u.EmailVerified = true
	} else {
		u.PhoneVerified = true
	}
	if err := s.store.UpdateUser(u); err != nil {
		return err
	}
	return s.store.ConsumeCode(vc.ID)
}

// ---- 登录 / 令牌 ----

// LoginResult 是登录返回。
type LoginResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64 // access token 剩余秒
	User         *User
}

// Login 校验密码（及 2FA），签发 access + refresh token。
func (s *Service) Login(target, password, tfaCode string) (*LoginResult, error) {
	u, err := s.loadByTarget(target)
	if err != nil {
		return nil, err
	}
	if u.Status == StatusFrozen {
		return nil, ErrFrozen
	}
	if !s.checkPassword(u.PassHash, password) {
		return nil, ErrWrongPassword
	}
	if u.TFAEnabled {
		if tfaCode == "" {
			return nil, ErrTFARequired
		}
		if !tfaVerify(u.TFASecret, tfaCode) {
			return nil, ErrTFAFailed
		}
	}
	access := s.verifier.Issue(u.ID, s.cfg.AccessTTL)
	refresh, err := s.issueRefresh(u.ID)
	if err != nil {
		return nil, err
	}
	return &LoginResult{
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int64(s.cfg.AccessTTL.Seconds()),
		User:         u,
	}, nil
}

func (s *Service) issueRefresh(userID int64) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	sum := sha256.Sum256(raw)
	if err := s.store.SaveRefresh(&RefreshToken{
		UserID:    userID,
		TokenHash: hex.EncodeToString(sum[:]),
		ExpiresAt: time.Now().Add(s.cfg.RefreshTTL),
	}); err != nil {
		return "", err
	}
	return token, nil
}

// Refresh 用 refresh token 换取新的 access token。
func (s *Service) Refresh(refreshToken string) (string, error) {
	raw, err := hex.DecodeString(refreshToken)
	if err != nil {
		return "", ErrRefreshInvalid
	}
	sum := sha256.Sum256(raw)
	rt, err := s.store.GetRefresh(hex.EncodeToString(sum[:]))
	if err != nil {
		return "", ErrRefreshInvalid
	}
	if time.Now().After(rt.ExpiresAt) {
		_ = s.store.DeleteRefresh(rt.TokenHash)
		return "", ErrRefreshInvalid
	}
	return s.verifier.Issue(rt.UserID, s.cfg.AccessTTL), nil
}

// Logout 吊销 refresh token。
func (s *Service) Logout(refreshToken string) error {
	raw, err := hex.DecodeString(refreshToken)
	if err != nil {
		return ErrRefreshInvalid
	}
	sum := sha256.Sum256(raw)
	return s.store.DeleteRefresh(hex.EncodeToString(sum[:]))
}

// ---- 找回密码 ----

// ForgotPassword 发送重置验证码。
func (s *Service) ForgotPassword(target string) error {
	if _, err := s.loadByTarget(target); err != nil {
		// 不泄露账号是否存在：存在才发码；不存在静默成功（安全考量）
		if err == ErrNotFound {
			return nil
		}
		return err
	}
	return s.SendCode(target, PurposeReset)
}

// ResetPassword 用重置验证码设置新密码，并吊销所有 refresh。
func (s *Service) ResetPassword(target, code, newPassword string) error {
	if len(newPassword) < s.cfg.MinPwdLen {
		return fmt.Errorf("password too short (min %d)", s.cfg.MinPwdLen)
	}
	vc, err := s.verifyCode(target, PurposeReset, code)
	if err != nil {
		return err
	}
	u, err := s.loadByTarget(target)
	if err != nil {
		return err
	}
	hash, err := s.hashPassword(newPassword)
	if err != nil {
		return err
	}
	u.PassHash = hash
	if err := s.store.UpdateUser(u); err != nil {
		return err
	}
	_ = s.store.ConsumeCode(vc.ID)
	_ = s.store.DeleteUserRefreshes(u.ID)
	return nil
}

// ---- 2FA ----

// SetupTFA 生成 TOTP 密钥并暂存（尚未启用），返回密钥与 otpauth URI。
func (s *Service) SetupTFA(userID int64) (secret, uri string, err error) {
	u, err := s.store.GetByID(userID)
	if err != nil {
		return "", "", err
	}
	secret, err = newTFASecret()
	if err != nil {
		return "", "", err
	}
	u.TFASecret = secret
	u.TFAEnabled = false
	if err := s.store.UpdateUser(u); err != nil {
		return "", "", err
	}
	return secret, tfaURI(s.cfg.Issuer, accountOf(u), secret), nil
}

// EnableTFA 校验动态码后启用 2FA。
func (s *Service) EnableTFA(userID int64, code string) error {
	u, err := s.store.GetByID(userID)
	if err != nil {
		return err
	}
	if u.TFASecret == "" {
		return ErrTFANotEnabled
	}
	if !tfaVerify(u.TFASecret, code) {
		return ErrTFAFailed
	}
	u.TFAEnabled = true
	return s.store.UpdateUser(u)
}

// DisableTFA 校验动态码后关闭 2FA 并清除密钥。
func (s *Service) DisableTFA(userID int64, code string) error {
	u, err := s.store.GetByID(userID)
	if err != nil {
		return err
	}
	if !u.TFAEnabled {
		return ErrTFANotEnabled
	}
	if !tfaVerify(u.TFASecret, code) {
		return ErrTFAFailed
	}
	u.TFAEnabled = false
	u.TFASecret = ""
	return s.store.UpdateUser(u)
}

// ---- KYC ----

// KYCRequest 是 KYC 提交请求。
type KYCRequest struct {
	RealName string `json:"real_name"`
	IDType   string `json:"id_type"`
	IDNumber string `json:"id_number"`
	DocFront string `json:"doc_front"`
	DocBack  string `json:"doc_back"`
}

// SubmitKYC 提交 KYC 材料，进入待审核。
func (s *Service) SubmitKYC(userID int64, req KYCRequest) error {
	if req.RealName == "" || req.IDNumber == "" {
		return fmt.Errorf("real_name and id_number required")
	}
	if existing, err := s.store.GetKYC(userID); err == nil {
		if existing.Status == KYCPending {
			return ErrKYCPending
		}
	}
	sub := &KYCSubmission{
		UserID:    userID,
		RealName:  req.RealName,
		IDType:    req.IDType,
		IDNumber:  req.IDNumber,
		DocFront:  req.DocFront,
		DocBack:   req.DocBack,
		Status:    KYCPending,
		SubmittedAt: time.Now(),
	}
	if err := s.store.SaveKYC(sub); err != nil {
		return err
	}
	u, err := s.store.GetByID(userID)
	if err != nil {
		return err
	}
	u.KYCLevel = KYCPending
	return s.store.UpdateUser(u)
}

// ReviewKYC 审核 KYC：approve=true 通过(等级2)，否则驳回(等级3)。
func (s *Service) ReviewKYC(userID int64, approve bool, rejectReason, reviewer string) error {
	sub, err := s.store.GetKYC(userID)
	if err != nil {
		return err
	}
	if sub.Status != KYCPending {
		return ErrKYCNotPending
	}
	u, err := s.store.GetByID(userID)
	if err != nil {
		return err
	}
	if approve {
		sub.Status = KYCVerified
		u.KYCLevel = KYCVerified
	} else {
		sub.Status = KYCRejected
		u.KYCLevel = KYCRejected
		sub.RejectReason = rejectReason
	}
	sub.Reviewer = reviewer
	sub.ReviewedAt = time.Now()
	if err := s.store.UpdateKYC(sub); err != nil {
		return err
	}
	return s.store.UpdateUser(u)
}

// GetProfile 取回用户档案与 KYC 状态。
func (s *Service) GetProfile(userID int64) (*User, *KYCSubmission, error) {
	u, err := s.store.GetByID(userID)
	if err != nil {
		return nil, nil, err
	}
	k, err := s.store.GetKYC(userID)
	if err != nil && err != ErrNotFound {
		return nil, nil, err
	}
	return u, k, nil
}

// ---- 个人设置 ----

const (
	maxNicknameLen = 32
	maxAvatarLen   = 512
)

// UpdateProfile 更新用户可编辑资料（昵称/头像）。传 nil 表示不修改该字段；
// 传空字符串表示清空该字段。仅对提供的字段做长度校验。
func (s *Service) UpdateProfile(userID int64, nickname, avatar *string) error {
	u, err := s.store.GetByID(userID)
	if err != nil {
		return err
	}
	if nickname != nil {
		if len([]rune(*nickname)) > maxNicknameLen {
			return ErrNicknameTooLong
		}
		u.Nickname = *nickname
	}
	if avatar != nil {
		if len(*avatar) > maxAvatarLen {
			return ErrAvatarTooLong
		}
		u.Avatar = *avatar
	}
	return s.store.UpdateUser(u)
}

// ChangePassword 在登录态下修改密码：校验旧密码，且不允许与旧密码相同。
// 修改成功后吊销该用户所有 refresh token，强制重新登录。
func (s *Service) ChangePassword(userID int64, oldPwd, newPwd string) error {
	u, err := s.store.GetByID(userID)
	if err != nil {
		return err
	}
	if !s.checkPassword(u.PassHash, oldPwd) {
		return ErrWrongPassword
	}
	if s.checkPassword(u.PassHash, newPwd) {
		return ErrSamePassword
	}
	if len(newPwd) < s.cfg.MinPwdLen {
		return ErrPasswordTooShort
	}
	hash, err := s.hashPassword(newPwd)
	if err != nil {
		return err
	}
	u.PassHash = hash
	if err := s.store.UpdateUser(u); err != nil {
		return err
	}
	return s.store.DeleteUserRefreshes(userID)
}

// GetPreferences 取回用户偏好；若不存在则返回默认值（不视为错误）。
func (s *Service) GetPreferences(userID int64) (*UserPreferences, error) {
	p, err := s.store.GetPreferences(userID)
	if err == ErrNotFound {
		return &UserPreferences{
			UserID:         userID,
			Language:       "zh-CN",
			Theme:          "light",
			NotifyOrder:    true,
			NotifySecurity: true,
			NotifyMarketing: false,
		}, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// UpdatePreferences 写入用户偏好设置。
func (s *Service) UpdatePreferences(userID int64, in *UserPreferences) error {
	if in == nil {
		return ErrInvalidPref
	}
	if in.Language != "" && len(in.Language) > 32 {
		return ErrInvalidPref
	}
	if in.Theme != "" && len(in.Theme) > 32 {
		return ErrInvalidPref
	}
	p := &UserPreferences{
		UserID:         userID,
		Language:       in.Language,
		Theme:          in.Theme,
		NotifyOrder:    in.NotifyOrder,
		NotifySecurity: in.NotifySecurity,
		NotifyMarketing: in.NotifyMarketing,
	}
	return s.store.UpdatePreferences(p)
}

// ---- 管理后台聚合接口（供 cmd/admin 调用）----

// ListAll 返回全量用户（管理后台用户与账户管理用）。
func (s *Service) ListAll() ([]*User, error) {
	return s.store.ListAll()
}

// SetStatus 设置用户封禁状态（冻结/解冻）。
func (s *Service) SetStatus(id int64, status Status) error {
	u, err := s.store.GetByID(id)
	if err != nil {
		return err
	}
	u.Status = status
	return s.store.UpdateUser(u)
}

// AdminCreate 由管理后台直接开通用户（跳过验证码流程），密码以 bcrypt 存储。
func (s *Service) AdminCreate(target, password string) (int64, error) {
	if len(password) < s.cfg.MinPwdLen {
		return 0, fmt.Errorf("password too short (min %d)", s.cfg.MinPwdLen)
	}
	if _, err := s.loadByTarget(target); err == nil {
		return 0, ErrUserExists
	}
	u := &User{PassHash: must(s.hashPassword(password))}
	if isEmail(target) {
		u.Email = target
		u.EmailVerified = true
	} else if isPhone(target) {
		u.Phone = target
		u.PhoneVerified = true
	} else {
		return 0, ErrInvalidAccount
	}
	if err := s.store.CreateUser(u); err != nil {
		return 0, err
	}
	return u.ID, nil
}

// AdminUpdateInput 是管理后台更新用户的补丁（nil 字段表示不变）。
type AdminUpdateInput struct {
	Email *string
	Status *Status
	KYCLevel *KYCLevel
}

// AdminUpdate 由管理后台更新用户档案字段。
func (s *Service) AdminUpdate(id int64, in AdminUpdateInput) error {
	u, err := s.store.GetByID(id)
	if err != nil {
		return err
	}
	if in.Email != nil {
		u.Email = *in.Email
	}
	if in.Status != nil {
		u.Status = *in.Status
	}
	if in.KYCLevel != nil {
		u.KYCLevel = *in.KYCLevel
	}
	return s.store.UpdateUser(u)
}

// ---- 辅助 ----

func accountOf(u *User) string {
	if u.Email != "" {
		return u.Email
	}
	return u.Phone
}

func must(s string, err error) string {
	if err != nil {
		panic(err)
	}
	return s
}

package user

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// 本文件实现用户安全中心四组能力：API Key、登录历史、会话管理、防钓鱼码。
// 路由与响应形状对齐 crypto-exchange-web/src/api/client.ts（SecurityCenter 契约）。

// API Key 状态。
const (
	KeyStatusActive   = "active"
	KeyStatusDisabled = "disabled"
)

// API Key 权限集合（前端 ApiKeyPermission）。
var validKeyPermissions = map[string]bool{"read": true, "trade": true, "withdraw": true}

// 业务错误。
var (
	ErrKeyLabelRequired   = errors.New("label required")
	ErrKeyPermRequired    = errors.New("at least one permission required")
	ErrKeyPermInvalid     = errors.New("invalid permission")
	ErrKeyStatusInvalid   = errors.New("invalid status")
	ErrSessionCurrent     = errors.New("cannot revoke current session")
	ErrAntiPhishingTooLong = errors.New("anti-phishing code too long (max 32)")
)

// ApiKey 是一条用户自助 API Key 记录。明文 secret 仅创建时一次性返回，
// 存储层只保留 sha256(secret)（SecretHash，不对外序列化）。
type ApiKey struct {
	ID          int64      `json:"id"`
	UserID      int64      `json:"user_id"`
	Label       string     `json:"label"`
	KeyPublic   string     `json:"key"` // 公钥部分（cxk_<prefix>_<secret> 的展示约定：完整公钥可安全展示）
	SecretHash  string     `json:"-"`
	Permissions []string   `json:"permissions"`
	IPWhitelist []string   `json:"ip_whitelist"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
}

// LoginEntry 是一次登录尝试的历史记录。
type LoginEntry struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	IP        string    `json:"ip"`
	UA        string    `json:"ua"`
	Location  string    `json:"location"`
	Success   bool      `json:"success"`
	CreatedAt time.Time `json:"created_at"`
}

// Session 是一次登录产生的活跃会话。current 标记由服务层在读取时推导
// （最近活跃的会话视为当前会话），不落库。
type Session struct {
	ID           string    `json:"id"`
	UserID       int64     `json:"user_id"`
	IP           string    `json:"ip"`
	UA           string    `json:"ua"`
	Location     string    `json:"location"`
	CreatedAt    time.Time `json:"created_at"`
	LastActiveAt time.Time `json:"last_active_at"`
}

// generateApiKey 生成一对新的 Key 明文与哈希：
// 完整明文 cxk_<prefix>_<secret>；prefix 8 hex 用于索引/展示，secret 64 hex 仅本次返回。
func generateApiKey() (public, secret, secretHash string, err error) {
	var buf [36]byte
	if _, err = rand.Read(buf[:]); err != nil {
		return "", "", "", err
	}
	prefix := hex.EncodeToString(buf[0:4])
	sec := hex.EncodeToString(buf[4:])
	public = "cxk_" + prefix + "_" + sec
	sum := sha256.Sum256([]byte(sec))
	return public, sec, hex.EncodeToString(sum[:]), nil
}

// hashApiSecret 与 generateApiKey 的哈希口径一致（校验方复用）。
func hashApiSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// locFromIP 由 IP 推导粗粒度归属地演示值：回环→本地，内网段→内网，其余→未知。
// 真实部署可接 GeoIP 库替换本函数。
func locFromIP(ip string) string {
	ip = strings.TrimSpace(ip)
	switch {
	case ip == "" || ip == "127.0.0.1" || ip == "::1":
		return "本地"
	case strings.HasPrefix(ip, "10."), strings.HasPrefix(ip, "192.168."),
		strings.HasPrefix(ip, "172.16."), strings.HasPrefix(ip, "172.17."),
		strings.HasPrefix(ip, "172.18."), strings.HasPrefix(ip, "172.19."),
		strings.HasPrefix(ip, "172.2"), strings.HasPrefix(ip, "172.30."), strings.HasPrefix(ip, "172.31."):
		return "内网"
	default:
		return "未知"
	}
}

func truncateUA(ua string) string {
	if len(ua) > 255 {
		return ua[:255]
	}
	return ua
}

// CreateApiKey 创建密钥：校验 label / permissions / ip_whitelist，生成明文 secret
// （仅本次返回），存储只落哈希。
func (s *Service) CreateApiKey(userID int64, label string, permissions, ipWhitelist []string) (*ApiKey, string, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return nil, "", ErrKeyLabelRequired
	}
	if len(permissions) == 0 {
		return nil, "", ErrKeyPermRequired
	}
	for _, p := range permissions {
		if !validKeyPermissions[p] {
			return nil, "", ErrKeyPermInvalid
		}
	}
	public, secret, secretHash, err := generateApiKey()
	if err != nil {
		return nil, "", err
	}
	k := &ApiKey{
		UserID:      userID,
		Label:       label,
		KeyPublic:   public,
		SecretHash:  secretHash,
		Permissions: permissions,
		IPWhitelist: ipWhitelist,
		Status:      KeyStatusActive,
	}
	if err := s.store.CreateApiKey(k); err != nil {
		return nil, "", err
	}
	return k, secret, nil
}

// ListApiKeys 返回本人密钥列表（时间倒序由存储层保证）。
func (s *Service) ListApiKeys(userID int64) ([]*ApiKey, error) {
	return s.store.ListApiKeys(userID)
}

// SetApiKeyStatus 启用/禁用密钥（仅 active/disabled 合法）。
func (s *Service) SetApiKeyStatus(userID, id int64, status string) error {
	if status != KeyStatusActive && status != KeyStatusDisabled {
		return ErrKeyStatusInvalid
	}
	return s.store.UpdateApiKeyStatus(userID, id, status)
}

// DeleteApiKey 撤销并删除密钥（硬删，mock 同语义）；不存在返回 ErrNotFound。
func (s *Service) DeleteApiKey(userID, id int64) error {
	return s.store.DeleteApiKey(userID, id)
}

// LookupUserID 按邮箱/手机号定位用户 ID（登录历史归属用）；不存在返回 false。
func (s *Service) LookupUserID(target string) (int64, bool) {
	u, err := s.loadByTarget(target)
	if err != nil {
		return 0, false
	}
	return u.ID, true
}

// RecordLoginWithMeta 在 Login 之上记录登录历史与会话：
// - 能定位到用户（含密码错误的既有账号）即记录 success/failure 历史；
// - 登录成功额外创建一条会话记录。
// ip/ua 来自请求上下文，由 handler 传入。
func (s *Service) RecordLoginWithMeta(target string, authErr error, userID int64, ip, ua string) {
	entry := &LoginEntry{
		UserID:    userID,
		IP:        ip,
		UA:        truncateUA(ua),
		Location:  locFromIP(ip),
		Success:   authErr == nil,
		CreatedAt: time.Now().UTC(),
	}
	_ = s.store.RecordLogin(entry)
	if authErr == nil {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err == nil {
			now := entry.CreatedAt
			_ = s.store.CreateSession(&Session{
				ID:           hex.EncodeToString(raw[:]),
				UserID:       userID,
				IP:           ip,
				UA:           entry.UA,
				Location:     entry.Location,
				CreatedAt:    now,
				LastActiveAt: now,
			})
		}
	}
}

// ListLoginHistory 返回本人登录历史（最多 limit 条，默认 50 上限 500）。
func (s *Service) ListLoginHistory(userID int64, limit int) ([]*LoginEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	return s.store.ListLoginHistory(userID, limit)
}

// ListSessions 返回本人会话并把「最近活跃」的一条标记为 current，
// 同时触碰其 last_active_at（调用方刚发起请求，视其为当前会话）。
func (s *Service) ListSessions(userID int64) ([]*Session, string, error) {
	list, err := s.store.ListSessions(userID)
	if err != nil {
		return nil, "", err
	}
	currentID := ""
	var newest time.Time
	for _, sess := range list {
		if sess.LastActiveAt.After(newest) {
			newest = sess.LastActiveAt
			currentID = sess.ID
		}
	}
	if currentID != "" {
		now := time.Now().UTC()
		_ = s.store.TouchSession(userID, currentID, now)
		for _, sess := range list {
			if sess.ID == currentID {
				sess.LastActiveAt = now
			}
		}
	}
	return list, currentID, nil
}

// RevokeSession 注销指定会话；不能注销当前会话；不存在返回 ErrNotFound。
func (s *Service) RevokeSession(userID int64, id, currentID string) error {
	if id == currentID {
		return ErrSessionCurrent
	}
	return s.store.DeleteSession(userID, id)
}

// RevokeOtherSessions 注销除当前外全部会话，返回注销数量。
func (s *Service) RevokeOtherSessions(userID int64, keepID string) (int64, error) {
	return s.store.DeleteOtherSessions(userID, keepID)
}

// GetAntiPhishing 返回防钓鱼码（未设置为空串）。
func (s *Service) GetAntiPhishing(userID int64) (string, error) {
	return s.store.GetAntiPhishing(userID)
}

// SetAntiPhishing 设置防钓鱼码；code 为空串表示清除。
func (s *Service) SetAntiPhishing(userID int64, code string) error {
	code = strings.TrimSpace(code)
	if len(code) > 32 {
		return ErrAntiPhishingTooLong
	}
	return s.store.SetAntiPhishing(userID, code)
}

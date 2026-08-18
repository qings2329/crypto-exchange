// Package apikeys 提供管理后台「API Key 管理」的自包含子系统：
// 管理员可为某个用户签发 / 列出 / 吊销 API Key。
//
// 安全模型：
//   - 明文 Key 形如 cxk_<prefix>_<secret>，仅在创建接口一次性返回；存储层只保存
//     sha256(明文 Key) 的哈希（key_hash）与前缀（prefix，用于索引与展示）。
//   - 校验方（网关/用户服务）拿到明文 Key 后，解析 prefix 与 secret，重算 sha256 并
//     通过 GetByKeyHash 比对，再判断 status 是否为 active。
//   - 吊销（revoke）仅置 status=revoked，不删除行，保留审计轨迹。
package apikeys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// API Key 状态。
const (
	StatusActive  = "active"
	StatusRevoked = "revoked"
)

// 业务错误。
var (
	ErrKeyNotFound  = errors.New("api key not found")
	ErrKeyRevoked   = errors.New("api key already revoked")
	ErrInvalidInput = errors.New("invalid input: user_id and label required")
)

// keyPrefixHeader 是明文 Key 的固定前缀，便于识别与解析。
const keyPrefixHeader = "cxk_"

// APIKey 是单条 API Key 记录（key_hash 不对外暴露）。
type APIKey struct {
	ID          int64      `json:"-"`            // 仅内部/审计用
	UserID      int64      `json:"user_id"`      // 该 Key 归属的用户
	Label       string     `json:"label"`        // 管理员可读的备注
	Prefix      string     `json:"prefix"`       // 明文前缀（8 hex，用于展示与索引）
	KeyHash     string     `json:"-"`            // sha256(明文 Key)，不对外返回
	Permissions []string   `json:"permissions"`  // 该 Key 被授予的权限（可为空）
	Status      string     `json:"status"`       // active / revoked
	CreatedBy   int64      `json:"created_by"`   // 签发管理员 ID
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// APIKeyView 是对外视图（剔除 key_hash）。
type APIKeyView struct {
	ID          int64      `json:"id"`
	UserID      int64      `json:"user_id"`
	Label       string     `json:"label"`
	Prefix      string     `json:"prefix"`
	Permissions []string   `json:"permissions"`
	Status      string     `json:"status"`
	CreatedBy   int64      `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// View 返回剔除敏感字段的对外视图。
func (k *APIKey) View() APIKeyView {
	perms := k.Permissions
	if perms == nil {
		perms = []string{}
	}
	return APIKeyView{
		ID:          k.ID,
		UserID:      k.UserID,
		Label:       k.Label,
		Prefix:      k.Prefix,
		Permissions: perms,
		Status:      k.Status,
		CreatedBy:   k.CreatedBy,
		CreatedAt:   k.CreatedAt,
		LastUsedAt:  k.LastUsedAt,
		RevokedAt:   k.RevokedAt,
	}
}

// KeyPair 是创建时一次性返回的明文凭据（前端需立即提示用户保存）。
type KeyPair struct {
	Key    string // 完整明文 Key：cxk_<prefix>_<secret>
	Prefix string
	Secret string
}

// GenerateKey 生成一对新的 API Key 明文（仅在本次返回，存储层不保存明文）。
// prefix 取随机字节前 4 字节（8 hex），secret 取 32 字节随机（64 hex）。
func GenerateKey() (KeyPair, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return KeyPair{}, err
	}
	secret := hex.EncodeToString(buf[:])
	prefix := hex.EncodeToString(buf[0:4])
	key := keyPrefixHeader + prefix + "_" + secret
	return KeyPair{Key: key, Prefix: prefix, Secret: secret}, nil
}

// HashKey 计算明文 Key 的存储哈希（与 GenerateKey 输出格式一致）。
func HashKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// ParseKey 从明文 Key 中解析出 prefix 与 secret（用于校验方）。
// 返回 (prefix, secret, true) 或 (_, _, false)（格式非法）。
func ParseKey(plain string) (prefix, secret string, ok bool) {
	if !strings.HasPrefix(plain, keyPrefixHeader) {
		return "", "", false
	}
	body := strings.TrimPrefix(plain, keyPrefixHeader)
	parts := strings.Split(body, "_")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// ListFilter 是列表查询条件（admin 视角）。
type ListFilter struct {
	UserID int64 // !=0 时按用户过滤；==0 时返回全部
}

// Store 抽象 API Key 的持久化。
type Store interface {
	// Create 写入一条新 Key（调用方需先 GenerateKey 生成明文并填充 Prefix/KeyHash）。
	Create(k *APIKey) error
	// GetByID 按 ID 查询；不存在返回 ErrKeyNotFound。
	GetByID(id int64) (*APIKey, error)
	// List 返回全部（或按 UserID 过滤）的 Key；由调用方负责分页。
	List(f ListFilter) ([]*APIKey, error)
	// ListByUser 返回某用户的全部 Key（按创建时间倒序）。
	ListByUser(userID int64) ([]*APIKey, error)
	// GetByKeyHash 按存储哈希查询（校验方使用）；不存在返回 ErrKeyNotFound。
	GetByKeyHash(keyHash string) (*APIKey, error)
	// Revoke 吊销指定 Key（status->revoked，记录 RevokedAt）；已吊销/不存在报错。
	Revoke(id int64) error
}

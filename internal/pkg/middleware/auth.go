package middleware

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ctxUserID 是鉴权后写入 gin 上下文的用户身份 key。
const ctxUserID = "user_id"

// ctxRole 是鉴权后写入 gin 上下文的角色 key（"user" | "admin"）。
const ctxRole = "role"

// ctxPerms 是鉴权后写入 gin 上下文的权限集合 key。
const ctxPerms = "perms"

// RoleUser 是普通用户角色（默认）。
const RoleUser = "user"

// RoleAdmin 是管理员角色。仅携带该角色的 Token 可通过 AdminGuard。
const RoleAdmin = "admin"

// TokenClaims 是 Bearer Token 的明文载荷（base64url 编码）。
type TokenClaims struct {
	UserID int64    `json:"uid"`
	Role   string   `json:"role"` // 角色；空字符串按 RoleUser 处理，保持向后兼容。
	Perms  []string `json:"perms,omitempty"` // 权限集合（管理后台 RBAC 用）；缺省为空（普通用户）。
	Exp    int64    `json:"exp"`  // 过期时间，unix 秒；<=0 表示不过期
}

// TokenVerifier 用共享密钥（HMAC-SHA256）签发与校验 Bearer Token。
// 不引入外部 JWT 依赖：token 形如 `<base64url(claims)>.<base64url(HMAC(claims))>`，
// 服务端用同一密钥重算签名并比对，且校验 exp。生产可替换为 JWT/API Key 体系。
type TokenVerifier struct {
	secret []byte
}

// NewTokenVerifier 以共享密钥构造校验器。密钥为空时 Verify 永远失败（fail-closed）。
func NewTokenVerifier(secret string) *TokenVerifier {
	return &TokenVerifier{secret: []byte(secret)}
}

func (v *TokenVerifier) sign(payload string) string {
	mac := hmac.New(sha256.New, v.secret)
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Issue 为 userID 签发有效期 ttl、角色为 RoleUser 的 Token（保持旧签名兼容）。
func (v *TokenVerifier) Issue(userID int64, ttl time.Duration) string {
	return v.IssueRole(userID, RoleUser, ttl)
}

// IssueRole 为 userID 签发有效期 ttl、指定角色（role）的 Token。
// 管理后台登录应传 RoleAdmin，普通用户登录传 RoleUser（或直接使用 Issue）。
func (v *TokenVerifier) IssueRole(userID int64, role string, ttl time.Duration) string {
	if role == "" {
		role = RoleUser
	}
	c := TokenClaims{UserID: userID, Role: role, Exp: time.Now().Add(ttl).Unix()}
	b, _ := json.Marshal(c)
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + v.sign(payload)
}

// IssueAdmin 为管理后台账户签发带角色与权限集合的 Token。
// perms 为该账户当前生效的细粒度权限（由角色权限聚合而来）；role 一般为 RoleAdmin。
func (v *TokenVerifier) IssueAdmin(userID int64, role string, perms []string, ttl time.Duration) string {
	if role == "" {
		role = RoleAdmin
	}
	c := TokenClaims{UserID: userID, Role: role, Perms: perms, Exp: time.Now().Add(ttl).Unix()}
	b, _ := json.Marshal(c)
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + v.sign(payload)
}

// Verify 校验 token 签名与有效期，返回用户 ID 与是否通过。
// 为保持向后兼容，本方法忽略角色字段；需要角色请用 VerifyFull。
func (v *TokenVerifier) Verify(token string) (int64, bool) {
	uid, _, ok := v.VerifyFull(token)
	return uid, ok
}

// VerifyFull 校验 token 并返回用户 ID、角色与是否通过。
// 角色为空时按 RoleUser 处理，因此旧式仅含 uid/exp 的 token 仍被识别为普通用户。
func (v *TokenVerifier) VerifyFull(token string) (int64, string, bool) {
	if len(v.secret) == 0 {
		return 0, "", false
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return 0, "", false
	}
	payload, sig := parts[0], parts[1]
	if !hmac.Equal([]byte(sig), []byte(v.sign(payload))) {
		return 0, "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return 0, "", false
	}
	var c TokenClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return 0, "", false
	}
	if c.Exp > 0 && time.Now().Unix() > c.Exp {
		return 0, "", false
	}
	role := c.Role
	if role == "" {
		role = RoleUser
	}
	return c.UserID, role, true
}

// VerifyFullClaims 校验 token 并返回完整 claims（含权限集合）。
func (v *TokenVerifier) VerifyFullClaims(token string) (TokenClaims, bool) {
	if len(v.secret) == 0 {
		return TokenClaims{}, false
	}
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return TokenClaims{}, false
	}
	payload, sig := parts[0], parts[1]
	if !hmac.Equal([]byte(sig), []byte(v.sign(payload))) {
		return TokenClaims{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return TokenClaims{}, false
	}
	var c TokenClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return TokenClaims{}, false
	}
	if c.Exp > 0 && time.Now().Unix() > c.Exp {
		return TokenClaims{}, false
	}
	return c, true
}

// Auth 返回鉴权中间件：校验 Authorization: Bearer <token>，失败返回 401；
// 成功将 user_id 与 role 写入上下文（用 UserID / Role 取回）。
func Auth(v *TokenVerifier) gin.HandlerFunc {
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "missing or invalid authorization",
			})
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		claims, ok := v.VerifyFullClaims(token)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"code":    401,
				"message": "invalid or expired token",
			})
			return
		}
		c.Set(ctxUserID, claims.UserID)
		c.Set(ctxRole, claims.Role)
		perms := claims.Perms
		if perms == nil {
			perms = []string{}
		}
		c.Set(ctxPerms, perms)
		c.Next()
	}
}

// UserID 从上下文取回鉴权后的用户 ID；未鉴权时 ok=false。
func UserID(c *gin.Context) (int64, bool) {
	v, ok := c.Get(ctxUserID)
	if !ok {
		return 0, false
	}
	uid, ok := v.(int64)
	return uid, ok
}

// Role 从上下文取回鉴权后的角色；未鉴权时 ok=false。
func Role(c *gin.Context) (string, bool) {
	v, ok := c.Get(ctxRole)
	if !ok {
		return "", false
	}
	role, ok := v.(string)
	return role, ok
}

// RequireRole 返回角色守卫中间件：仅允许 token 角色在 roles 中的请求通过，
// 否则返回 403。常用于把管理后台接口限定为 RoleAdmin。
// 注意：RequireRole 本身不做基础鉴权，调用方应把它放在 Auth 之后。
func RequireRole(roles ...string) gin.HandlerFunc {
	allow := make(map[string]bool, len(roles))
	for _, r := range roles {
		allow[r] = true
	}
	return func(c *gin.Context) {
		role, ok := Role(c)
		if !ok || !allow[role] {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"code":    403,
				"message": "insufficient role",
			})
			return
		}
		c.Next()
	}
}

// AdminGuard 是 RequireRole(RoleAdmin) 的便捷别名，用于保护管理后台接口。
func AdminGuard() gin.HandlerFunc {
	return RequireRole(RoleAdmin)
}

// Perms 从上下文取回鉴权后的权限集合；未鉴权时 ok=false。
func Perms(c *gin.Context) ([]string, bool) {
	v, ok := c.Get(ctxPerms)
	if !ok {
		return nil, false
	}
	p, ok := v.([]string)
	return p, ok
}

// RequirePerm 返回权限守卫中间件：要求 token 至少拥有 perms 中的一项，否则 403。
// 用于把管理后台接口按细粒度权限限定（如 RequirePerm(PermUserWrite)）。
// 注意：RequirePerm 本身不做基础鉴权，调用方应把它放在 Auth 之后。
func RequirePerm(perms ...string) gin.HandlerFunc {
	need := make(map[string]bool, len(perms))
	for _, p := range perms {
		need[p] = true
	}
	return func(c *gin.Context) {
		got, ok := Perms(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": 403, "message": "forbidden"})
			return
		}
		for _, gp := range got {
			if need[gp] {
				c.Next()
				return
			}
		}
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": 403, "message": "insufficient permission"})
	}
}

// RequireAllPerm 要求 token 必须拥有 perms 中的全部权限，否则 403。
func RequireAllPerm(perms ...string) gin.HandlerFunc {
	need := make(map[string]bool, len(perms))
	for _, p := range perms {
		need[p] = true
	}
	return func(c *gin.Context) {
		got, ok := Perms(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": 403, "message": "forbidden"})
			return
		}
		have := make(map[string]bool, len(got))
		for _, gp := range got {
			have[gp] = true
		}
		for p := range need {
			if !have[p] {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"code": 403, "message": "insufficient permission"})
				return
			}
		}
		c.Next()
	}
}

package adminapi

import (
	"errors"
	"net/http"
	"time"

	"golang.org/x/crypto/bcrypt"
	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

// ---- 视图结构（不暴露密码哈希 / TOTP secret） ----

// AdminView 是管理员账户的对外视图。
type AdminView struct {
	ID          int64     `json:"id"`
	Username    string    `json:"username"`
	Status      string    `json:"status"`
	RoleID      int64     `json:"role_id"`
	RoleName    string    `json:"role_name"`
	TOTPEnabled bool      `json:"totp_enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// RoleView 是角色（含其权限集合）的对外视图。
type RoleView struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Permissions []string  `json:"permissions"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// MeView 是当前登录管理员的信息。
type MeView struct {
	ID          int64     `json:"id"`
	Username    string    `json:"username"`
	Status      string    `json:"status"`
	RoleID      int64     `json:"role_id"`
	RoleName    string    `json:"role_name"`
	Permissions []string  `json:"permissions"`
	TOTPEnabled bool      `json:"totp_enabled"`
}

func (s *Server) toAdminView(a *AdminAccount) AdminView {
	v := AdminView{
		ID: a.ID, Username: a.Username, Status: a.Status, RoleID: a.RoleID,
		TOTPEnabled: a.TOTPEnabled, CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
	if a.RoleID != 0 {
		if r, err := s.adminStore.GetRoleByID(a.RoleID); err == nil {
			v.RoleName = r.Name
		}
	}
	return v
}

func (s *Server) toRoleView(r *Role) RoleView {
	v := RoleView{ID: r.ID, Name: r.Name, Description: r.Description, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	if p, err := s.adminStore.GetRolePermissions(r.ID); err == nil {
		v.Permissions = p
	} else {
		v.Permissions = []string{}
	}
	return v
}

// ---- 当前管理员自身 ----

// adminMe 返回当前登录管理员的信息（含角色与权限）。
func (s *Server) adminMe(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		s.fail(c, http.StatusUnauthorized, "unauthorized")
		return
	}
	acc, err := s.adminStore.GetAccountByID(uid)
	if err != nil {
		s.fail(c, http.StatusNotFound, "admin not found")
		return
	}
	me := MeView{ID: acc.ID, Username: acc.Username, Status: acc.Status, RoleID: acc.RoleID, TOTPEnabled: acc.TOTPEnabled}
	if acc.RoleID != 0 {
		if r, e := s.adminStore.GetRoleByID(acc.RoleID); e == nil {
			me.RoleName = r.Name
		}
	}
	if p, e := s.adminStore.GetRolePermissions(acc.RoleID); e == nil {
		me.Permissions = p
	} else {
		me.Permissions = []string{}
	}
	s.ok(c, me)
}

// getAdminPreferences 返回当前登录管理员的界面偏好（语言/主题/时区）。
func (s *Server) getAdminPreferences(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		s.fail(c, http.StatusUnauthorized, "unauthorized")
		return
	}
	p, err := s.adminStore.GetPreferences(uid)
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "get preferences failed")
		return
	}
	s.ok(c, p)
}

// updateAdminPreferences 更新当前登录管理员的界面偏好。
func (s *Server) updateAdminPreferences(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		s.fail(c, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req AdminPreferences
	if err := c.ShouldBindJSON(&req); err != nil {
		s.fail(c, http.StatusBadRequest, "invalid body")
		return
	}
	req.AdminID = uid // 强制以当前登录管理员身份写入，避免越权
	if err := s.adminStore.UpdatePreferences(&req); err != nil {
		s.fail(c, http.StatusInternalServerError, "update preferences failed")
		return
	}
	s.ok(c, gin.H{"ok": true})
}

// changePassword 修改当前登录管理员的密码（需校验旧密码）。
func (s *Server) changePassword(c *gin.Context) {
	uid, _ := middleware.UserID(c)
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		s.fail(c, http.StatusBadRequest, "invalid body")
		return
	}
	if len(req.NewPassword) < 6 {
		s.fail(c, http.StatusBadRequest, "new password too short (>=6)")
		return
	}
	acc, err := s.adminStore.GetAccountByID(uid)
	if err != nil {
		s.fail(c, http.StatusNotFound, "admin not found")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(acc.PasswordHash), []byte(req.OldPassword)) != nil {
		s.fail(c, http.StatusUnauthorized, "old password incorrect")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "hash password failed")
		return
	}
	acc.PasswordHash = string(hash)
	if err := s.adminStore.UpdateAccount(acc); err != nil {
		s.fail(c, http.StatusInternalServerError, "update failed")
		return
	}
	s.ok(c, gin.H{"updated": true})
}

// ---- MFA（Google Authenticator） ----

// setupMFA 为当前管理员生成 TOTP secret 并暂存（未启用），返回 secret 与 otpauth URI（供扫码）。
func (s *Server) setupMFA(c *gin.Context) {
	uid, _ := middleware.UserID(c)
	acc, err := s.adminStore.GetAccountByID(uid)
	if err != nil {
		s.fail(c, http.StatusNotFound, "admin not found")
		return
	}
	secret, err := GenerateTOTPSecret()
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "generate secret failed")
		return
	}
	acc.TOTPSecret = secret
	acc.TOTPEnabled = false
	if err := s.adminStore.UpdateAccount(acc); err != nil {
		s.fail(c, http.StatusInternalServerError, "save secret failed")
		return
	}
	s.ok(c, gin.H{
		"secret":      secret,
		"otpauth_uri": OTPAuthURI("CryptoExchangeAdmin", acc.Username, secret),
	})
}

// enableMFA 校验当前管理员输入的动态码，校验通过则启用 MFA（之后登录必须输入 TOTP）。
func (s *Server) enableMFA(c *gin.Context) {
	uid, _ := middleware.UserID(c)
	var req struct {
		Code string `json:"code"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Code == "" {
		s.fail(c, http.StatusBadRequest, "code required")
		return
	}
	acc, err := s.adminStore.GetAccountByID(uid)
	if err != nil {
		s.fail(c, http.StatusNotFound, "admin not found")
		return
	}
	if acc.TOTPSecret == "" {
		s.fail(c, http.StatusBadRequest, "please setup mfa first")
		return
	}
	if !VerifyTOTP(acc.TOTPSecret, req.Code, time.Now()) {
		s.fail(c, http.StatusBadRequest, "invalid code")
		return
	}
	acc.TOTPEnabled = true
	if err := s.adminStore.UpdateAccount(acc); err != nil {
		s.fail(c, http.StatusInternalServerError, "enable failed")
		return
	}
	s.ok(c, gin.H{"totp_enabled": true})
}

// disableMFA 关闭当前管理员的 MFA（若已启用需校验动态码）。
func (s *Server) disableMFA(c *gin.Context) {
	uid, _ := middleware.UserID(c)
	var req struct {
		Code string `json:"code"`
	}
	_ = c.ShouldBindJSON(&req)
	acc, err := s.adminStore.GetAccountByID(uid)
	if err != nil {
		s.fail(c, http.StatusNotFound, "admin not found")
		return
	}
	if acc.TOTPEnabled {
		if !VerifyTOTP(acc.TOTPSecret, req.Code, time.Now()) {
			s.fail(c, http.StatusBadRequest, "invalid code")
			return
		}
	}
	acc.TOTPSecret = ""
	acc.TOTPEnabled = false
	if err := s.adminStore.UpdateAccount(acc); err != nil {
		s.fail(c, http.StatusInternalServerError, "disable failed")
		return
	}
	s.ok(c, gin.H{"totp_enabled": false})
}

// ---- 管理员账户管理（需 admin:manage） ----

// listAdmins 列出全部管理员账户（不暴露密码哈希 / TOTP secret）。
func (s *Server) listAdmins(c *gin.Context) {
	accounts, err := s.adminStore.ListAccounts()
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]AdminView, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, s.toAdminView(a))
	}
	limit, offset := parsePage(c)
	s.ok(c, pageEnvelope(out, limit, offset))
}

// createAdmin 新增管理员（设定初始密码与角色），默认状态 pending（待激活）。
func (s *Server) createAdmin(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
		RoleID   int64  `json:"role_id"`
		RoleName string `json:"role_name"`
		Status   string `json:"status"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		s.fail(c, http.StatusBadRequest, "invalid body")
		return
	}
	if req.Username == "" || len(req.Password) < 6 {
		s.fail(c, http.StatusBadRequest, "username required and password >= 6")
		return
	}
	// 解析角色
	var roleID int64
	if req.RoleID != 0 {
		roleID = req.RoleID
	} else if req.RoleName != "" {
		r, err := s.adminStore.GetRoleByName(req.RoleName)
		if err != nil {
			s.fail(c, http.StatusBadRequest, "role not found")
			return
		}
		roleID = r.ID
	} else {
		s.fail(c, http.StatusBadRequest, "role_id or role_name required")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "hash password failed")
		return
	}
	status := req.Status
	if status == "" {
		status = AdminStatusPending
	}
	acc := &AdminAccount{Username: req.Username, PasswordHash: string(hash), Status: status, RoleID: roleID}
	if err := s.adminStore.CreateAccount(acc); err != nil {
		if err == ErrAdminExists {
			s.fail(c, http.StatusConflict, "username already exists")
			return
		}
		s.fail(c, http.StatusInternalServerError, "create failed")
		return
	}
	s.ok(c, s.toAdminView(acc))
}

// updateAdmin 更新管理员（补丁角色与状态）。
func (s *Server) updateAdmin(c *gin.Context) {
	id, ok := parseInt64(c, "id")
	if !ok {
		s.fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		RoleID *int64  `json:"role_id"`
		Status string  `json:"status"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		s.fail(c, http.StatusBadRequest, "invalid body")
		return
	}
	acc, err := s.adminStore.GetAccountByID(id)
	if err != nil {
		s.fail(c, http.StatusNotFound, "admin not found")
		return
	}
	if req.RoleID != nil {
		if _, e := s.adminStore.GetRoleByID(*req.RoleID); e != nil {
			s.fail(c, http.StatusBadRequest, "role not found")
			return
		}
		acc.RoleID = *req.RoleID
	}
	if req.Status != "" {
		acc.Status = req.Status
	}
	if err := s.adminStore.UpdateAccount(acc); err != nil {
		s.fail(c, http.StatusInternalServerError, "update failed")
		return
	}
	s.ok(c, s.toAdminView(acc))
}

// activateAdmin 激活管理员账户（pending -> active）。
func (s *Server) activateAdmin(c *gin.Context) {
	s.setAdminStatus(c, AdminStatusActive)
}

// disableAdmin 停用管理员账户（active -> disabled）。
func (s *Server) disableAdmin(c *gin.Context) {
	s.setAdminStatus(c, AdminStatusDisabled)
}

func (s *Server) setAdminStatus(c *gin.Context, status string) {
	id, ok := parseInt64(c, "id")
	if !ok {
		s.fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	acc, err := s.adminStore.GetAccountByID(id)
	if err != nil {
		s.fail(c, http.StatusNotFound, "admin not found")
		return
	}
	acc.Status = status
	if err := s.adminStore.UpdateAccount(acc); err != nil {
		s.fail(c, http.StatusInternalServerError, "update failed")
		return
	}
	s.ok(c, s.toAdminView(acc))
}

// resetAdminPassword 重置指定管理员的密码（管理员管理权限，无需旧密码）。
func (s *Server) resetAdminPassword(c *gin.Context) {
	id, ok := parseInt64(c, "id")
	if !ok {
		s.fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.Password) < 6 {
		s.fail(c, http.StatusBadRequest, "password required (>=6)")
		return
	}
	acc, err := s.adminStore.GetAccountByID(id)
	if err != nil {
		s.fail(c, http.StatusNotFound, "admin not found")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "hash password failed")
		return
	}
	acc.PasswordHash = string(hash)
	if err := s.adminStore.UpdateAccount(acc); err != nil {
		s.fail(c, http.StatusInternalServerError, "update failed")
		return
	}
	s.ok(c, gin.H{"updated": true})
}

// ---- 角色与权限管理（需 role:manage） ----

// listRoles 列出全部角色及其权限。
func (s *Server) listRoles(c *gin.Context) {
	roles, err := s.adminStore.ListRoles()
	if err != nil {
		s.fail(c, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]RoleView, 0, len(roles))
	for _, r := range roles {
		out = append(out, s.toRoleView(r))
	}
	limit, offset := parsePage(c)
	s.ok(c, pageEnvelope(out, limit, offset))
}

// createRole 新建自定义角色（初始无权限，需再调用 setRolePermissions 分配）。
func (s *Server) createRole(c *gin.Context) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Name == "" {
		s.fail(c, http.StatusBadRequest, "name required")
		return
	}
	r := &Role{Name: req.Name, Description: req.Description}
	if err := s.adminStore.CreateRole(r); err != nil {
		if err == ErrRoleExists {
			s.fail(c, http.StatusConflict, "role name already exists")
			return
		}
		s.fail(c, http.StatusInternalServerError, "create failed")
		return
	}
	s.ok(c, s.toRoleView(r))
}

// setRolePermissions 为角色分配（全量覆盖）细粒度权限。
func (s *Server) setRolePermissions(c *gin.Context) {
	id, ok := parseInt64(c, "id")
	if !ok {
		s.fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		Permissions []string `json:"permissions"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		s.fail(c, http.StatusBadRequest, "invalid body")
		return
	}
	if _, err := s.adminStore.GetRoleByID(id); err != nil {
		s.fail(c, http.StatusNotFound, "role not found")
		return
	}
	perms := ValidatePermissions(req.Permissions)
	if err := s.adminStore.SetRolePermissions(id, perms); err != nil {
		s.fail(c, http.StatusInternalServerError, "update failed")
		return
	}
	r, _ := s.adminStore.GetRoleByID(id)
	s.ok(c, s.toRoleView(r))
}

// deleteRole 删除角色。
func (s *Server) deleteRole(c *gin.Context) {
	id, ok := parseInt64(c, "id")
	if !ok {
		s.fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	if err := s.adminStore.DeleteRole(id); err != nil {
		if errors.Is(err, ErrRoleInUse) {
			s.fail(c, http.StatusConflict, "角色仍被管理员引用，无法删除")
			return
		}
		s.fail(c, http.StatusNotFound, "role not found")
		return
	}
	s.ok(c, gin.H{"deleted": true})
}

// updateRole 编辑角色名与描述（权限分配走 setRolePermissions，避免混用）。
func (s *Server) updateRole(c *gin.Context) {
	id, ok := parseInt64(c, "id")
	if !ok {
		s.fail(c, http.StatusBadRequest, "invalid id")
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Name == "" {
		s.fail(c, http.StatusBadRequest, "name required")
		return
	}
	r := &Role{ID: id, Name: req.Name, Description: req.Description}
	if err := s.adminStore.UpdateRole(r); err != nil {
		if err == ErrRoleExists {
			s.fail(c, http.StatusConflict, "role name already exists")
			return
		}
		if err == ErrAdminNotFound {
			s.fail(c, http.StatusNotFound, "role not found")
			return
		}
		s.fail(c, http.StatusInternalServerError, "update failed")
		return
	}
	updated, _ := s.adminStore.GetRoleByID(id)
	s.ok(c, s.toRoleView(updated))
}

// listPermissionDict 返回全部可授予的权限字典（供权限分配 UI）。
func (s *Server) listPermissionDict(c *gin.Context) {
	s.ok(c, AllPermissions())
}

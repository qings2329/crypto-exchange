package adminapi

import (
	"errors"
	"sync"
	"time"
)

// 本文件定义管理后台"操作员账户 / 角色 / 权限"的持久化抽象（AdminStore），
// 含内存实现（无 MySQL 时回退）、MySQL 工厂与首次启动的引导种子。

// 管理员账户状态。
const (
	AdminStatusPending  = "pending"  // 已创建，待授权者激活
	AdminStatusActive   = "active"   // 可登录
	AdminStatusDisabled = "disabled" // 已停用
)

// AdminAccount 是管理后台操作员账户（区别于 user 服务的普通用户）。
type AdminAccount struct {
	ID           int64
	Username     string
	PasswordHash string
	Status       string
	RoleID       int64
	TOTPSecret   string // base32；绑定 GA 但未启用时暂存于此
	TOTPEnabled  bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Role 是自定义角色（含其分配的权限集合）。
type Role struct {
	ID          int64
	Name        string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ErrAdminNotFound 表示账户/角色不存在。
var ErrAdminNotFound = errors.New("admin entity not found")

// ErrAdminExists 表示用户名已存在。
var ErrAdminExists = errors.New("admin username already exists")

// ErrRoleExists 表示角色名已存在。
var ErrRoleExists = errors.New("role name already exists")

// ErrRoleInUse 表示角色仍被管理员账户引用，禁止删除（避免孤儿引用）。
var ErrRoleInUse = errors.New("role is still in use by admin accounts")

// AdminStore 抽象管理员账户 / 角色 / 权限的持久化。
type AdminStore interface {
	// 账户
	CountAccounts() (int, error)
	CreateAccount(a *AdminAccount) error
	GetAccountByUsername(username string) (*AdminAccount, error)
	GetAccountByID(id int64) (*AdminAccount, error)
	ListAccounts() ([]*AdminAccount, error)
	UpdateAccount(a *AdminAccount) error // 按 ID 更新全部可变字段

	// 角色
	CreateRole(r *Role) error
	GetRoleByName(name string) (*Role, error)
	GetRoleByID(id int64) (*Role, error)
	ListRoles() ([]*Role, error)
	UpdateRole(r *Role) error // 按 ID 更新 name/description
	DeleteRole(id int64) error

	// 角色权限
	SetRolePermissions(roleID int64, perms []string) error
	GetRolePermissions(roleID int64) ([]string, error)
}

// ---- 内存实现（无 MySQL 时回退） ----

type memAdminStore struct {
	mu       sync.RWMutex
	seqAcc   int64
	seqRole  int64
	accounts map[int64]*AdminAccount
	byName   map[string]int64 // username -> id
	roles    map[int64]*Role
	roleByName map[string]int64
	rolePerms map[int64][]string
}

// NewMemAdminStore 构造内存实现（启动 seed 由调用方负责）。
func NewMemAdminStore() AdminStore {
	return &memAdminStore{
		accounts:   map[int64]*AdminAccount{},
		byName:     map[string]int64{},
		roles:      map[int64]*Role{},
		roleByName: map[string]int64{},
		rolePerms:  map[int64][]string{},
	}
}

func (s *memAdminStore) CountAccounts() (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.accounts), nil
}

func (s *memAdminStore) CreateAccount(a *AdminAccount) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byName[a.Username]; ok {
		return ErrAdminExists
	}
	s.seqAcc++
	now := time.Now()
	a.ID = s.seqAcc
	a.CreatedAt = now
	a.UpdatedAt = now
	cp := *a
	s.accounts[a.ID] = &cp
	s.byName[a.Username] = a.ID
	return nil
}

func (s *memAdminStore) GetAccountByUsername(username string) (*AdminAccount, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byName[username]
	if !ok {
		return nil, ErrAdminNotFound
	}
	cp := *s.accounts[id]
	return &cp, nil
}

func (s *memAdminStore) GetAccountByID(id int64) (*AdminAccount, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.accounts[id]
	if !ok {
		return nil, ErrAdminNotFound
	}
	cp := *a
	return &cp, nil
}

func (s *memAdminStore) ListAccounts() ([]*AdminAccount, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*AdminAccount, 0, len(s.accounts))
	for _, a := range s.accounts {
		cp := *a
		out = append(out, &cp)
	}
	return out, nil
}

func (s *memAdminStore) UpdateAccount(a *AdminAccount) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, ok := s.accounts[a.ID]
	if !ok {
		return ErrAdminNotFound
	}
	// 若改名，需同步 byName 索引
	if old.Username != a.Username {
		if _, conflict := s.byName[a.Username]; conflict && s.byName[a.Username] != a.ID {
			return ErrAdminExists
		}
		delete(s.byName, old.Username)
		s.byName[a.Username] = a.ID
	}
	a.UpdatedAt = time.Now()
	cp := *a
	s.accounts[a.ID] = &cp
	return nil
}

func (s *memAdminStore) CreateRole(r *Role) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.roleByName[r.Name]; ok {
		return ErrRoleExists
	}
	s.seqRole++
	now := time.Now()
	r.ID = s.seqRole
	r.CreatedAt = now
	r.UpdatedAt = now
	cp := *r
	s.roles[r.ID] = &cp
	s.roleByName[r.Name] = r.ID
	return nil
}

func (s *memAdminStore) GetRoleByName(name string) (*Role, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.roleByName[name]
	if !ok {
		return nil, ErrAdminNotFound
	}
	cp := *s.roles[id]
	return &cp, nil
}

func (s *memAdminStore) GetRoleByID(id int64) (*Role, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.roles[id]
	if !ok {
		return nil, ErrAdminNotFound
	}
	cp := *r
	return &cp, nil
}

func (s *memAdminStore) ListRoles() ([]*Role, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Role, 0, len(s.roles))
	for _, r := range s.roles {
		cp := *r
		out = append(out, &cp)
	}
	return out, nil
}

// UpdateRole 按 ID 更新角色名与描述；改名时需保证不与其它角色重名。
func (s *memAdminStore) UpdateRole(r *Role) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.roles[r.ID]
	if !ok {
		return ErrAdminNotFound
	}
	if r.Name != cur.Name {
		if _, dup := s.roleByName[r.Name]; dup {
			return ErrRoleExists
		}
		delete(s.roleByName, cur.Name)
		s.roleByName[r.Name] = cur.ID
		cur.Name = r.Name
	}
	cur.Description = r.Description
	cur.UpdatedAt = time.Now()
	return nil
}

func (s *memAdminStore) DeleteRole(id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.roles[id]
	if !ok {
		return ErrAdminNotFound
	}
	// 先检查是否仍被管理员账户引用，避免删除后出现孤儿引用（同时规避删除后解引用 nil 指针）。
	for _, a := range s.accounts {
		if a.RoleID == id {
			return ErrRoleInUse
		}
	}
	name := r.Name
	delete(s.roles, id)
	delete(s.roleByName, name)
	delete(s.rolePerms, id)
	return nil
}

func (s *memAdminStore) SetRolePermissions(roleID int64, perms []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.roles[roleID]; !ok {
		return ErrAdminNotFound
	}
	cp := make([]string, len(perms))
	copy(cp, perms)
	s.rolePerms[roleID] = cp
	return nil
}

func (s *memAdminStore) GetRolePermissions(roleID int64) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.rolePerms[roleID]
	if !ok {
		return nil, ErrAdminNotFound
	}
	cp := make([]string, len(p))
	copy(cp, p)
	return cp, nil
}

// NewAdminStore 优先返回 MySQL 实现；若 DSN 为空或连接/迁移失败，则回退内存实现
// （isMem=true），并在生产环境应通过日志告警。本地无 MySQL 时可正常运行以验证逻辑。
func NewAdminStore(dsn string) (store AdminStore, isMem bool, err error) {
	if dsn == "" {
		return NewMemAdminStore(), true, nil
	}
	ms, e := NewMySQLAdminStore(dsn)
	if e != nil {
		return NewMemAdminStore(), true, e
	}
	return ms, false, nil
}

// SeedBootstrap 在库中无账户时，写入默认三角色（super_admin/admin/operator）与
// 由 config 引导的超级管理员账户。已有账户则跳过。
// bootstrapHash 为 bcrypt 哈希（来自 config 的 password_hash 或明文回退哈希）。
func SeedBootstrap(store AdminStore, bootstrapUsername, bootstrapHash string) error {
	n, err := store.CountAccounts()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if bootstrapUsername == "" {
		bootstrapUsername = "admin"
	}
	if bootstrapHash == "" {
		return errors.New("bootstrap admin password hash is empty")
	}

	// 默认角色与权限
	defs := []struct {
		name string
		desc string
		perms []string
	}{
		{RoleSuperAdmin, "超级管理员（全部权限）", allPermKeys()},
		{RoleAdmin, "运营管理员（不含系统管理）", []string{
			PermDashboardView, PermUserRead, PermUserWrite,
			PermSymbolRead, PermSymbolWrite, PermChainRead, PermChainWrite,
			PermCoinRead, PermCoinWrite, PermDepositRead, PermWithdrawApproval,
			PermNotificationManage, PermLedgerRead, PermServiceRead,
			PermTradeRead, PermTradeManage,
		}},
		{RoleOperator, "只读操作员", []string{
			PermDashboardView, PermUserRead, PermSymbolRead, PermChainRead,
			PermCoinRead, PermDepositRead, PermLedgerRead, PermServiceRead,
			PermTradeRead,
		}},
	}
	for _, d := range defs {
		r := &Role{Name: d.name, Description: d.desc}
		if err := store.CreateRole(r); err != nil {
			return err
		}
		if err := store.SetRolePermissions(r.ID, d.perms); err != nil {
			return err
		}
	}
	super, err := store.GetRoleByName(RoleSuperAdmin)
	if err != nil {
		return err
	}
	acc := &AdminAccount{
		Username:     bootstrapUsername,
		PasswordHash: bootstrapHash,
		Status:       AdminStatusActive,
		RoleID:       super.ID,
	}
	return store.CreateAccount(acc)
}

// allPermKeys 返回权限字典中的全部 key（供超级管理员角色使用）。
func allPermKeys() []string {
	keys := make([]string, 0, len(allPermissionDefs))
	for _, p := range allPermissionDefs {
		keys = append(keys, p.Key)
	}
	return keys
}

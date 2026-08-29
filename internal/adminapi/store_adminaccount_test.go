package adminapi

import "testing"

// TestMemAdminStoreDeleteRoleInUse 锁定删除仍被管理员引用的角色的行为：
//  1. 未被引用时可正常删除；
//  2. 被引用时返回 ErrRoleInUse，且角色本身不被删（也不应触发 nil 指针 panic）。
func TestMemAdminStoreDeleteRoleInUse(t *testing.T) {
	s := NewMemAdminStore()

	// 未被引用 -> 可删除
	unused := &Role{Name: "unused", Description: "d"}
	if err := s.CreateRole(unused); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	if err := s.DeleteRole(unused.ID); err != nil {
		t.Fatalf("删除未被引用角色应成功: %v", err)
	}
	if _, err := s.GetRoleByID(unused.ID); err == nil {
		t.Fatal("未被引用角色删除后不应再存在")
	}

	// 被引用 -> 返回 ErrRoleInUse，角色保留
	used := &Role{Name: "used", Description: "d"}
	if err := s.CreateRole(used); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	acc := &AdminAccount{
		Username:     "ref_user",
		PasswordHash: "x",
		Status:       AdminStatusActive,
		RoleID:       used.ID,
	}
	if err := s.CreateAccount(acc); err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}

	if err := s.DeleteRole(used.ID); err == nil {
		t.Fatal("删除被引用角色应返回错误，实际为 nil")
	} else if err != ErrRoleInUse {
		t.Fatalf("删除被引用角色应返回 ErrRoleInUse，实际: %v", err)
	}
	if _, err := s.GetRoleByID(used.ID); err != nil {
		t.Fatal("被引用角色删除失败后应当仍然存在")
	}
}

// TestMemAdminStoreUpdateRole 锁定角色编辑行为：
//  1. 改名/改描述生效；
//  2. 改名与已有角色冲突返回 ErrRoleExists；
//  3. 编辑不存在的角色返回 ErrAdminNotFound。
func TestMemAdminStoreUpdateRole(t *testing.T) {
	s := NewMemAdminStore()
	a := &Role{Name: "role_a", Description: "desc_a"}
	if err := s.CreateRole(a); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}
	b := &Role{Name: "role_b", Description: "desc_b"}
	if err := s.CreateRole(b); err != nil {
		t.Fatalf("CreateRole: %v", err)
	}

	// 1) 改名 + 改描述
	if err := s.UpdateRole(&Role{ID: a.ID, Name: "role_a", Description: "updated"}); err != nil {
		t.Fatalf("UpdateRole desc: %v", err)
	}
	got, err := s.GetRoleByID(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != "updated" {
		t.Fatalf("description 未更新, got %q", got.Description)
	}

	// 2) 改名为已存在的角色名 -> ErrRoleExists
	if err := s.UpdateRole(&Role{ID: a.ID, Name: "role_b"}); err != ErrRoleExists {
		t.Fatalf("重名应返回 ErrRoleExists, got %v", err)
	}

	// 3) 编辑不存在的角色 -> ErrAdminNotFound
	if err := s.UpdateRole(&Role{ID: 9999, Name: "ghost"}); err != ErrAdminNotFound {
		t.Fatalf("不存在角色应返回 ErrAdminNotFound, got %v", err)
	}
}

// TestSeedBootstrapSelfHeal 锁定 seed 的自愈能力（针对"管理员/角色管理 insufficient permission"问题）：
//  1. 首次播种：bootstrap 管理员归属 super_admin，且 super_admin 持有全量权限
//     （含 admin:manage、role:manage）。
//  2. 历史数据修复：super_admin 角色存在但权限被旧版本/改动遗漏时，再次播种会补全
//     admin:manage/role:manage 全量权限。
//  3. 角色漂移修复：bootstrap 管理员被移到其他角色时，再次播种会归位 super_admin。
func TestSeedBootstrapSelfHeal(t *testing.T) {
	has := func(perms []string, key string) bool {
		for _, p := range perms {
			if p == key {
				return true
			}
		}
		return false
	}

	s := NewMemAdminStore()

	// --- 1) 首次播种 ---
	if err := SeedBootstrap(s, "admin", "$2a$10$fakehash"); err != nil {
		t.Fatalf("首次播种失败: %v", err)
	}
	super, err := s.GetRoleByName(RoleSuperAdmin)
	if err != nil {
		t.Fatalf("super_admin 角色应存在: %v", err)
	}
	perms, err := s.GetRolePermissions(super.ID)
	if err != nil {
		t.Fatalf("GetRolePermissions: %v", err)
	}
	if !has(perms, PermAdminManage) || !has(perms, PermRoleManage) {
		t.Fatalf("播种后 super_admin 应含 admin:manage/role:manage，实际: %v", perms)
	}
	acc, err := s.GetAccountByUsername("admin")
	if err != nil {
		t.Fatalf("bootstrap admin 应存在: %v", err)
	}
	if acc.RoleID != super.ID || acc.Status != AdminStatusActive {
		t.Fatalf("bootstrap admin 应归属 super_admin 且 active，got role=%d status=%s", acc.RoleID, acc.Status)
	}

	// --- 2) 历史权限缺失时自愈 ---
	if err := s.SetRolePermissions(super.ID, []string{PermOpsView}); err != nil {
		t.Fatalf("SetRolePermissions: %v", err)
	}
	if err := SeedBootstrap(s, "admin", "$2a$10$fakehash"); err != nil {
		t.Fatalf("二次播种失败: %v", err)
	}
	perms2, err := s.GetRolePermissions(super.ID)
	if err != nil {
		t.Fatalf("GetRolePermissions: %v", err)
	}
	if !has(perms2, PermAdminManage) || !has(perms2, PermRoleManage) {
		t.Fatalf("二次播种应补全 super_admin 权限，实际: %v", perms2)
	}

	// --- 3) 角色漂移时归位 ---
	op, err := s.GetRoleByName(RoleOperator)
	if err != nil {
		t.Fatalf("operator 角色应存在: %v", err)
	}
	acc3, err := s.GetAccountByUsername("admin")
	if err != nil {
		t.Fatal(err)
	}
	acc3.RoleID = op.ID
	if err := s.UpdateAccount(acc3); err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}
	if err := SeedBootstrap(s, "admin", "$2a$10$fakehash"); err != nil {
		t.Fatalf("三次播种失败: %v", err)
	}
	acc4, err := s.GetAccountByUsername("admin")
	if err != nil {
		t.Fatal(err)
	}
	if acc4.RoleID != super.ID {
		t.Fatalf("角色漂移后 bootstrap admin 应归位 super_admin，got role=%d", acc4.RoleID)
	}
}


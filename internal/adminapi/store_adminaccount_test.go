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

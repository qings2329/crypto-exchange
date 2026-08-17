package adminapi_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

// roleID 从创建/更新接口的响应包络中提取角色 ID。
func roleID(t *testing.T, data interface{}) int64 {
	t.Helper()
	raw, _ := json.Marshal(data)
	var r struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("解析角色响应失败: %v", err)
	}
	if r.ID == 0 {
		t.Fatal("角色响应缺少有效 id")
	}
	return r.ID
}

// TestAdminRoleDelete 端到端校验角色删除接口（DELETE /roles/:id）：
//  1. 未带 token → 401；
//  2. 成功删除后列表不再包含该角色；
//  3. 删除仍被管理员引用的角色 → 409（ErrRoleInUse）。
func TestAdminRoleDelete(t *testing.T) {
	r, _ := newTestServer(t)

	// 登录拿 admin token（默认账户拥有 role:manage）。
	_, data := postJSON(t, r, "/api/admin/login", "", map[string]string{
		"username": "admin", "password": "admin123",
	})
	tok, _ := data.(map[string]interface{})["token"].(string)
	if tok == "" {
		t.Fatal("登录未返回 token")
	}

	// 1) 未带 token 删除 → 401
	code, _ := deleteJSON(t, r, "/api/admin/roles/1", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("未鉴权删除应 401，got %d", code)
	}

	// 2) 创建并成功删除
	_, created := postJSON(t, r, "/api/admin/roles", tok, map[string]string{
		"name": "tmp_role", "description": "to delete",
	})
	id := roleID(t, created)
	code, _ = deleteJSON(t, r, "/api/admin/roles/"+strconv.FormatInt(id, 10), tok)
	if code != http.StatusOK {
		t.Fatalf("删除角色应 200，got %d", code)
	}
	// 列表不再包含该角色
	_, list := getJSON(t, r, "/api/admin/roles", tok)
	raw, _ := json.Marshal(list)
	var env struct {
		Items []struct {
			ID int64 `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("解析角色列表失败: %v", err)
	}
	for _, it := range env.Items {
		if it.ID == id {
			t.Fatal("角色删除后仍出现在列表中")
		}
	}

	// 3) 删除被引用的角色 → 409
	_, used := postJSON(t, r, "/api/admin/roles", tok, map[string]string{
		"name": "used_role", "description": "in use",
	})
	usedID := roleID(t, used)
	// 新建一个绑定该角色的管理员账户（默认 admin 拥有 admin:manage）。
	codeCreate, _ := postJSON(t, r, "/api/admin/admins", tok, map[string]any{
		"username": "refuser", "password": "secret1", "role_id": usedID,
	})
	if codeCreate != http.StatusOK {
		t.Fatalf("创建引用角色的管理员应 200，got %d", codeCreate)
	}
	code, _ = deleteJSON(t, r, "/api/admin/roles/"+strconv.FormatInt(usedID, 10), tok)
	if code != http.StatusConflict {
		t.Fatalf("删除被引用角色应 409，got %d", code)
	}
}

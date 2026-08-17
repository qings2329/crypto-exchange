package adminapi

import (
	"sync"
	"time"
)

// AuditEntry 是一条管理员操作审计记录（仅记录元数据，不落具体请求体，避免泄露口令等敏感信息）。
type AuditEntry struct {
	ID      int64  `json:"id"`
	AdminID int64  `json:"admin_id"`
	Method  string `json:"method"`
	Path    string `json:"path"`   // 路由模式（如 /api/admin/withdrawals/:id/approve）
	Action  string `json:"action"` // create / update / delete
	Target  string `json:"target"` // 具体路径（含真实参数）
	Status  int    `json:"status"` // HTTP 响应状态码（含失败尝试）
	Detail  string `json:"detail"` // 简短说明
	IP      string `json:"ip"`
	Time    int64  `json:"time"` // Unix 纳秒
}

// AuditStore 抽象审计日志的持久化（内存 / MySQL）。
type AuditStore interface {
	Append(e AuditEntry) error
	// List 按时间倒序返回一页；limit<=0 表示不限制条数；offset 为跳过的最新条数。
	List(limit, offset int) ([]AuditEntry, int64, error)
}

// MemAuditStore 内存实现，用于本地无 MySQL 开发或回退。
type MemAuditStore struct {
	mu     sync.RWMutex
	items  []AuditEntry
	nextID int64
}

func NewMemAuditStore() *MemAuditStore { return &MemAuditStore{} }

func (s *MemAuditStore) Append(e AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	e.ID = s.nextID
	if e.Time == 0 {
		e.Time = time.Now().UnixNano()
	}
	s.items = append(s.items, e)
	return nil
}

func (s *MemAuditStore) List(limit, offset int) ([]AuditEntry, int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := int64(len(s.items))
	ordered := make([]AuditEntry, len(s.items))
	for i, e := range s.items {
		ordered[len(s.items)-1-i] = e // 最新在前
	}
	if offset < 0 {
		offset = 0
	}
	if offset > len(ordered) {
		offset = len(ordered)
	}
	end := offset + limit
	if limit <= 0 {
		end = len(ordered)
	} else if end > len(ordered) {
		end = len(ordered)
	}
	return ordered[offset:end], total, nil
}

// NewAuditStore 优先返回 MySQL 实现；DSN 为空或连接/迁移失败则回退内存。
func NewAuditStore(dsn string) (AuditStore, bool, error) {
	if dsn == "" {
		return NewMemAuditStore(), true, nil
	}
	ms, e := NewMySQLAuditStore(dsn)
	if e != nil {
		return NewMemAuditStore(), true, e
	}
	return ms, false, nil
}

package futuresapi

import (
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// TPSLStore 是 TP-SL 配置的持久化抽象。读路径走内存热缓存（s.tpsl），
// 写路径同步落库（写穿），重启时从库恢复。接口极简：全量加载 + Upsert + Delete。
type TPSLStore interface {
	// LoadAll 全量加载，供启动时填充内存热缓存。
	LoadAll() (map[int64]map[string]TPState, error)
	// Upsert 新增或更新；tp/sl 为 nil 表示清除该字段（两项均 nil 等价 Delete）。
	Upsert(uid int64, key string, tp, sl *float64) error
	// Delete 删除指定条目。
	Delete(uid int64, key string) error
}

// ---- 内存实现（测试/无 DB 降级用） ----

type memTPSLStore struct {
	mu    sync.RWMutex
	data  map[int64]map[string]TPState // uid -> key -> TPState
}

func NewMemTPSLStore() TPSLStore {
	return &memTPSLStore{data: make(map[int64]map[string]TPState)}
}

func (m *memTPSLStore) LoadAll() (map[int64]map[string]TPState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[int64]map[string]TPState, len(m.data))
	for uid, km := range m.data {
		cp := make(map[string]TPState, len(km))
		for k, v := range km {
			cp[k] = v
		}
		out[uid] = cp
	}
	return out, nil
}

func (m *memTPSLStore) Upsert(uid int64, key string, tp, sl *float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data[uid] == nil {
		m.data[uid] = make(map[string]TPState)
	}
	m.data[uid][key] = TPState{TP: tp, SL: sl}
	return nil
}

func (m *memTPSLStore) Delete(uid int64, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if km, ok := m.data[uid]; ok {
		delete(km, key)
		if len(km) == 0 {
			delete(m.data, uid)
		}
	}
	return nil
}

// ---- MySQL 实现 ----

const tpslMigVer = 9951

var tpslMigrations = []migrate.Migration{
	{
		Version: tpslMigVer,
		Name:    "create_ce_futures_tpsl",
		Up: `CREATE TABLE IF NOT EXISTS ce_futures_tpsl (
    user_id   BIGINT      NOT NULL,
    tpsl_key  VARCHAR(64) NOT NULL COMMENT 'uid|symbol|side',
    tp        DOUBLE      NULL,
    sl        DOUBLE      NULL,
    updated_at DATETIME(3) NOT NULL,
    PRIMARY KEY (user_id, tpsl_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: "DROP TABLE IF EXISTS ce_futures_tpsl;",
	},
}

type mysqlTPSLStore struct {
	db *sql.DB
}

func NewMySQLTPSLStore(db *sql.DB) (TPSLStore, error) {
	if err := migrate.New(db, tpslMigrations).Up(); err != nil {
		return nil, fmt.Errorf("tpsl migrate: %w", err)
	}
	return &mysqlTPSLStore{db: db}, nil
}

func (s *mysqlTPSLStore) LoadAll() (map[int64]map[string]TPState, error) {
	rows, err := s.db.Query(`SELECT user_id, tpsl_key, tp, sl FROM ce_futures_tpsl`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]map[string]TPState)
	for rows.Next() {
		var uid int64
		var key string
		var tp, sl sql.NullFloat64
		if err := rows.Scan(&uid, &key, &tp, &sl); err != nil {
			return nil, err
		}
		if out[uid] == nil {
			out[uid] = make(map[string]TPState)
		}
		state := TPState{}
		if tp.Valid {
			state.TP = &tp.Float64
		}
		if sl.Valid {
			state.SL = &sl.Float64
		}
		out[uid][key] = state
	}
	return out, rows.Err()
}

func (s *mysqlTPSLStore) Upsert(uid int64, key string, tp, sl *float64) error {
	_, err := s.db.Exec(
		`INSERT INTO ce_futures_tpsl (user_id, tpsl_key, tp, sl, updated_at) VALUES (?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE tp = VALUES(tp), sl = VALUES(sl), updated_at = VALUES(updated_at)`,
		uid, key, tp, sl, time.Now().UTC())
	return err
}

func (s *mysqlTPSLStore) Delete(uid int64, key string) error {
	_, err := s.db.Exec(`DELETE FROM ce_futures_tpsl WHERE user_id = ? AND tpsl_key = ?`, uid, key)
	return err
}

package persist

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coldlar/crypto-exchange/internal/matching"
	_ "github.com/go-sql-driver/mysql"
)

// MySQLStore 以 MySQL 为共享后端的 Store 实现，支撑撮合引擎的多实例部署与崩溃恢复。
// 所有状态都落在 ce_ 前缀的表（见 Migrations）：
//   - ce_matching_wal：订单提交/撤单的 WAL（seq 自增，全局有序）；
//   - ce_matching_snapshot：全量订单簿快照（version=所覆盖的最大 WAL seq）；
//   - ce_matching_seq：全局订单号计数器（LAST_INSERT_ID 保证单调递增、跨实例唯一）；
//   - ce_matching_leader：单写者 leader 锁（基于过期时间的租约）。
type MySQLStore struct {
	db *sql.DB
}

// NewMySQLStore 打开 MySQL 连接并校验可达性。
func NewMySQLStore(dsn string) (*MySQLStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(8)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return &MySQLStore{db: db}, nil
}

// DB 返回底层 *sql.DB，供调用方执行迁移。
func (s *MySQLStore) DB() *sql.DB { return s.db }

// Close 关闭连接池。
func (s *MySQLStore) Close() error { return s.db.Close() }

// NextOrderID 通过 ce_matching_seq 的 LAST_INSERT_ID 技巧保证全局单调递增、跨实例唯一。
func (s *MySQLStore) NextOrderID(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE ce_matching_seq SET val=LAST_INSERT_ID(val+1) WHERE id=1"); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, "SELECT LAST_INSERT_ID()").Scan(&id); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// SetMinOrderID 保证计数器严格大于 id（恢复后对齐；MySQL 下仅在必要时更新）。
func (s *MySQLStore) SetMinOrderID(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE ce_matching_seq SET val=? WHERE id=1 AND val<?", id, id)
	return err
}

// Append 持久化一条 WAL 事件（payload 为事件 JSON，seq 由自增列分配）。
func (s *MySQLStore) Append(ctx context.Context, ev matching.OrderEvent) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		"INSERT INTO ce_matching_wal (symbol, event_type, payload, ts) VALUES (?, ?, ?, ?)",
		ev.Symbol, string(ev.Type), payload, ev.Ts)
	return err
}

// SaveSnapshot 覆盖式保存快照（id=1）。
func (s *MySQLStore) SaveSnapshot(ctx context.Context, version int64, state []byte) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO ce_matching_snapshot (id, version, state, updated_at) VALUES (1, ?, ?, NOW(3))
ON DUPLICATE KEY UPDATE version=VALUES(version), state=VALUES(state), updated_at=NOW(3)`,
		version, state)
	return err
}

// LoadSnapshot 返回最近快照；无记录时 version=-1、state=nil。
func (s *MySQLStore) LoadSnapshot(ctx context.Context) (int64, []byte, error) {
	var ver int64
	var state []byte
	err := s.db.QueryRowContext(ctx, "SELECT version, state FROM ce_matching_snapshot WHERE id=1").Scan(&ver, &state)
	if err == sql.ErrNoRows {
		return -1, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}
	if state == nil {
		return ver, nil, nil
	}
	return ver, state, nil
}

// Replay 返回 seq>afterVersion 的事件（升序），payload 反序列化为 OrderEvent。
func (s *MySQLStore) Replay(ctx context.Context, afterVersion int64) ([]matching.OrderEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT seq, symbol, event_type, payload FROM ce_matching_wal WHERE seq>? ORDER BY seq ASC", afterVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []matching.OrderEvent
	for rows.Next() {
		var seq int64
		var symbol string
		var typ string
		var payload []byte
		if err := rows.Scan(&seq, &symbol, &typ, &payload); err != nil {
			return nil, err
		}
		var ev matching.OrderEvent
		if err := json.Unmarshal(payload, &ev); err != nil {
			return nil, err
		}
		ev.Seq = seq
		ev.Symbol = symbol
		ev.Type = matching.EventType(typ)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// MaxSeq 返回当前 WAL 最大 seq。
func (s *MySQLStore) MaxSeq(ctx context.Context) (int64, error) {
	var max sql.NullInt64
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0) FROM ce_matching_wal").Scan(&max); err != nil {
		return 0, err
	}
	return max.Int64, nil
}

// PruneWAL 删除 seq<=seq 的历史记录（快照完成后削减）。
func (s *MySQLStore) PruneWAL(ctx context.Context, seq int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM ce_matching_wal WHERE seq<=?", seq)
	return err
}

// TryAcquireLeader 尝试获得 leader 租约：当前无主、或自己是主、或租约已过期时可成功。
func (s *MySQLStore) TryAcquireLeader(ctx context.Context, node string, ttl time.Duration) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE ce_matching_leader
SET holder=?, expires_at=DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), heartbeat=NOW(3)
WHERE id=1 AND (holder=? OR holder='' OR expires_at < NOW(3))`,
		node, ttl.Microseconds(), node)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// RenewLeader 续约（仅当前 holder 成功）。
func (s *MySQLStore) RenewLeader(ctx context.Context, node string, ttl time.Duration) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
UPDATE ce_matching_leader
SET expires_at=DATE_ADD(NOW(3), INTERVAL ? MICROSECOND), heartbeat=NOW(3)
WHERE id=1 AND holder=?`,
		ttl.Microseconds(), node)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// ReleaseLeader 主动放弃 leadership。
func (s *MySQLStore) ReleaseLeader(ctx context.Context, node string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE ce_matching_leader SET holder='' WHERE id=1 AND holder=?", node)
	return err
}

// IsLeader 报告 node 是否为有效（未过期）leader。
func (s *MySQLStore) IsLeader(ctx context.Context, node string) (bool, error) {
	var holder string
	var expires time.Time
	if err := s.db.QueryRowContext(ctx, "SELECT holder, expires_at FROM ce_matching_leader WHERE id=1").Scan(&holder, &expires); err != nil {
		return false, err
	}
	return holder == node && time.Now().Before(expires), nil
}

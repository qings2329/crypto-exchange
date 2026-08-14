package ledger

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// 账本快照的 MySQL 持久化：沿用既有 Snapshot()/Restore() 的 JSON 序列化结构，把整份快照
// 作为一个 JSON blob 存入 ce_ledger_snapshots 表的单行（按 ledgerID 唯一键 UPSERT）。
// 这样可零改造复用已验证的 Snapshot/Restore 不变量，同时把"文件"后端换成"数据库"后端，
// 获得并发安全、集中备份与多实例共享的能力。生产若需审计检索，可再对子结构做规范化拆分。
// 表结构由 internal/ledger/migrations.go 的迁移定义（ce_ 前缀约定），openMySQL 启动时执行。

// openAndPing 打开 MySQL 连接池并验证连通性。
func openAndPing(dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	return db, nil
}

// dbNameOf 从 go-sql-driver DSN 中提取数据库名（位于最后一个 '/' 之后、首个 '?' 之前）。
func dbNameOf(dsn string) string {
	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		return ""
	}
	rest := dsn[i+1:]
	if q := strings.IndexByte(rest, '?'); q >= 0 {
		return rest[:q]
	}
	return rest
}

// stripDBName 去掉 DSN 中的库名部分，得到可连 server 但不选库的 DSN（用于自动建库）。
func stripDBName(dsn string) string {
	i := strings.LastIndex(dsn, "/")
	if i < 0 {
		return dsn
	}
	rest := dsn[i+1:]
	if q := strings.IndexByte(rest, '?'); q >= 0 {
		return dsn[:i+1] + rest[q:]
	}
	return dsn[:i+1]
}

// openMySQL 打开连接池并自动建表（首次访问即保证 schema 就绪），连接失败返回错误。
// 若 DSN 指定了库名但该库不存在，会尝试 CREATE DATABASE（需账号有建库权限）后重连；
// 仍失败则返回明确错误，便于排查远程账号是否有建库权限或需手动建库。
func openMySQL(dsn string) (*sql.DB, error) {
	db, err := openAndPing(dsn)
	if err != nil {
		if dbName := dbNameOf(dsn); dbName != "" && strings.Contains(err.Error(), "Unknown database") {
			if base, berr := openAndPing(stripDBName(dsn)); berr == nil {
				if _, cerr := base.Exec(fmt.Sprintf(
					"CREATE DATABASE IF NOT EXISTS `%s` CHARACTER SET utf8mb4", dbName)); cerr == nil {
					_ = base.Close()
					db, err = openAndPing(dsn)
				} else {
					_ = base.Close()
					return nil, fmt.Errorf("create database %q: %w", dbName, cerr)
				}
			}
		}
		if err != nil {
			return nil, err
		}
	}
	// 执行账本迁移（建 ce_ledger_snapshots 等），幂等、可重入。
	if err := migrate.New(db, LedgerMigrations).Up(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("run ledger migrations: %w", err)
	}
	return db, nil
}

// SaveToMySQL 把当前账本状态序列化为 JSON 并 UPSERT 进 MySQL（按 ledgerID 唯一键）。
// ledgerID 为空时默认 "default"。失败时返回错误（不破坏内存态账本）。
func (l *Ledger) SaveToMySQL(dsn, ledgerID string) error {
	if ledgerID == "" {
		ledgerID = "default"
	}
	db, err := openMySQL(dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	snap := l.Snapshot()
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshal snapshot: %w", err)
	}
	if _, err := db.Exec(
		`INSERT INTO ce_ledger_snapshots (id, data, updated_at) VALUES (?, ?, ?)
		 ON DUPLICATE KEY UPDATE data = VALUES(data), updated_at = VALUES(updated_at)`,
		ledgerID, string(data), time.Now(),
	); err != nil {
		return fmt.Errorf("upsert snapshot: %w", err)
	}
	return nil
}

// LoadSnapshotFromMySQL 从 MySQL 读取指定 ledgerID 的快照。
// 返回 (snap, ok, err)：ok=false 表示该行不存在（新实例，应执行种子充值）；err!=nil 表示连接/查询失败。
func LoadSnapshotFromMySQL(dsn, ledgerID string) (LedgerSnapshot, bool, error) {
	if ledgerID == "" {
		ledgerID = "default"
	}
	db, err := openMySQL(dsn)
	if err != nil {
		return LedgerSnapshot{}, false, err
	}
	defer db.Close()
	var data string
	var updatedAt time.Time
	err = db.QueryRow(
		`SELECT data, updated_at FROM ce_ledger_snapshots WHERE id = ?`, ledgerID,
	).Scan(&data, &updatedAt)
	if err == sql.ErrNoRows {
		return LedgerSnapshot{}, false, nil
	}
	if err != nil {
		return LedgerSnapshot{}, false, fmt.Errorf("query snapshot: %w", err)
	}
	var snap LedgerSnapshot
	if err := json.Unmarshal([]byte(data), &snap); err != nil {
		return LedgerSnapshot{}, false, fmt.Errorf("unmarshal snapshot: %w", err)
	}
	return snap, true, nil
}

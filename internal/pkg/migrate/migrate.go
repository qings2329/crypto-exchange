// Package migrate 提供极简、可回滚的数据库迁移运行器。
//
// 约定：本项目所有业务表必须以 ce_ 开头（见 docs/CONVENTIONS.md）。迁移 SQL 在代码中
// 以有序列表声明（亦可改为读取 .sql 文件），每条含唯一版本号与升降级语句。
// 已应用的版本记录在 ce_schema_migrations 表，保证幂等、可重入。
//
// 重要：版本号在整个数据库内全局唯一（所有模块共用同一张 ce_schema_migrations），
// 不同模块必须使用互不重叠的版本区间（例如 ledger 用 1，matching 用 2，
// market 用 3……或按模块保留 100/200/300 段），切勿各自从 1 开始，否则会冲突。
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Migration 是一条迁移：Version 全局唯一且单调递增；Up 应用变更，Down 回滚变更。
type Migration struct {
	Version int
	Name    string
	Up      string
	Down    string
}

// Runner 执行迁移。
type Runner struct {
	db         *sql.DB
	table      string
	migrations []Migration
}

// New 构造运行器。migrations 必须按 Version 升序传入。
func New(db *sql.DB, migrations []Migration) *Runner {
	return &Runner{db: db, table: "ce_schema_migrations", migrations: migrations}
}

// queryExecutor 是 *sql.DB 与 *sql.Conn 共同满足的查询接口。迁移在专用连接上执行
// （配合 GET_LOCK 串行化，避免并发/共享库下的重复迁移/重复建表，见 Up/Down）。
type queryExecutor interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
}

// ensureVersionTable 建版本记录表（ce_ 前缀）。
func (r *Runner) ensureVersionTable(ctx context.Context, q queryExecutor) error {
	_, err := q.ExecContext(ctx, fmt.Sprintf(`
CREATE TABLE IF NOT EXISTS %s (
    version   INT         NOT NULL,
    name      VARCHAR(128) NOT NULL,
    applied_at DATETIME(3) NOT NULL,
    PRIMARY KEY (version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`, r.table))
	return err
}

// appliedVersions 返回已应用的版本集合。
func (r *Runner) appliedVersions(ctx context.Context, q queryExecutor) (map[int]bool, error) {
	rows, err := q.QueryContext(ctx, fmt.Sprintf("SELECT version FROM %s", r.table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		m[v] = true
	}
	return m, rows.Err()
}

// withLock 获取专用连接并在其上持有一条 MySQL 命名锁（GET_LOCK），串行化迁移，
// 避免同一远程库被多个集成测试/进程并发访问时的重复建表或版本记录竞争。连接归还连接池时
// 锁自动失效，调用方需在 defer 中关闭 conn。
func (r *Runner) withLock(ctx context.Context, fn func(ctx context.Context, conn *sql.Conn) error) error {
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: get conn: %w", err)
	}
	defer conn.Close()
	var got int
	// GET_LOCK 返回 1=成功获取，0=超时，NULL=错误。超时/失败必须显式报错，
	// 否则会静默地在无锁状态下执行迁移，失去串行化保护作用。
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK('ce_migrate', 10)").Scan(&got); err != nil {
		return fmt.Errorf("migrate: acquire lock: %w", err)
	}
	if got != 1 {
		return fmt.Errorf("migrate: failed to acquire lock (got %d)", got)
	}
	return fn(ctx, conn)
}

// Up 应用所有未执行的迁移（按版本升序）。在专用连接上持锁执行，保证串行化与幂等。
func (r *Runner) Up() error {
	return r.withLock(context.Background(), func(ctx context.Context, conn *sql.Conn) error {
		if err := r.ensureVersionTable(ctx, conn); err != nil {
			return fmt.Errorf("ensure version table: %w", err)
		}
		applied, err := r.appliedVersions(ctx, conn)
		if err != nil {
			return err
		}
		for _, m := range r.migrations {
			if applied[m.Version] {
				continue
			}
			if strings.TrimSpace(m.Up) == "" {
				return fmt.Errorf("migration %d (%s) has empty Up", m.Version, m.Name)
			}
			// 逐条语句执行（简单按 ';' 切分，足以覆盖 DDL）。
			for _, stmt := range splitSQL(m.Up) {
				if _, err := conn.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("apply migration %d (%s): %w", m.Version, m.Name, err)
				}
			}
			// INSERT IGNORE：版本记录幂等，避免并发/重跑时唯一键冲突（同一远程库被多个集成测试共享）。
			if _, err := conn.ExecContext(ctx,
				fmt.Sprintf("INSERT IGNORE INTO %s (version, name, applied_at) VALUES (?, ?, NOW(3))", r.table),
				m.Version, m.Name); err != nil {
				return fmt.Errorf("record migration %d: %w", m.Version, err)
			}
		}
		return nil
	})
}

// Down 回滚最近一次未指定数量的迁移（toVersion<0 表示回滚全部）。在专用连接上持锁执行。
func (r *Runner) Down(toVersion int) error {
	return r.withLock(context.Background(), func(ctx context.Context, conn *sql.Conn) error {
		if err := r.ensureVersionTable(ctx, conn); err != nil {
			return fmt.Errorf("ensure version table: %w", err)
		}
		applied, err := r.appliedVersions(ctx, conn)
		if err != nil {
			return err
		}
		// 从高版本向低版本回滚。
		for i := len(r.migrations) - 1; i >= 0; i-- {
			m := r.migrations[i]
			if !applied[m.Version] {
				continue
			}
			if toVersion >= 0 && m.Version <= toVersion {
				continue
			}
			for _, stmt := range splitSQL(m.Down) {
				if _, err := conn.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("rollback migration %d (%s): %w", m.Version, m.Name, err)
				}
			}
			if _, err := conn.ExecContext(ctx,
				fmt.Sprintf("DELETE FROM %s WHERE version = ?", r.table), m.Version); err != nil {
				return fmt.Errorf("unrecord migration %d: %w", m.Version, err)
			}
			if toVersion >= 0 {
				return nil
			}
		}
		return nil
	})
}

// splitSQL 按分号切分 SQL（忽略空语句）。不处理存储过程等高级语法。
func splitSQL(s string) []string {
	parts := strings.Split(s, ";")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

package margin

import (
	"database/sql"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// MarginMigrations 是 margin 服务的建表迁移。
// 版本号使用 92xx 段（全局唯一，避免与其他模块冲突，见 migrate 包约定）。
var MarginMigrations = []migrate.Migration{
	{
		Version: 9201,
		Name:    "create_ce_margin_accounts",
		Up: `CREATE TABLE IF NOT EXISTS ce_margin_accounts (
				user_id            BIGINT        NOT NULL,
				asset              VARCHAR(32)   NOT NULL,
				collateral_asset   VARCHAR(32)   NOT NULL DEFAULT '',
				collateral_amount  DOUBLE        NOT NULL DEFAULT 0,
				debt               DOUBLE        NOT NULL DEFAULT 0,
				interest_accrued   DOUBLE        NOT NULL DEFAULT 0,
				leverage           INT           NOT NULL DEFAULT 1,
				status             VARCHAR(16)   NOT NULL DEFAULT 'active',
				last_accrual       DATETIME(3)   NOT NULL,
				created_at         DATETIME(3)   NOT NULL,
				updated_at         DATETIME(3)   NOT NULL,
				PRIMARY KEY (user_id, asset)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_margin_accounts`,
	},
}

// NewMySQLStore 打开 MySQL 并跑迁移，返回 MySQL 版 Store。
func NewMySQLStore(dsn string) (*mysqlStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	runner := migrate.New(db, MarginMigrations)
	if err := runner.Up(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &mysqlStore{db: db}, nil
}

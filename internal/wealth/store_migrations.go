package wealth

import (
	"database/sql"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// WealthMigrations 是 wealth 服务的建表迁移。
// 版本号使用 97xx 段（全局唯一，避免与其他模块冲突，见 migrate 包约定）。
var WealthMigrations = []migrate.Migration{
	{
		Version: 9701,
		Name:    "create_ce_wealth_products",
		Up: `CREATE TABLE IF NOT EXISTS ce_wealth_products (
				id             BIGINT        NOT NULL AUTO_INCREMENT,
				name           VARCHAR(128)  NOT NULL DEFAULT '',
				asset          VARCHAR(32)   NOT NULL,
				type           VARCHAR(16)   NOT NULL,
				annual_rate    DOUBLE        NOT NULL DEFAULT 0,
				duration_days  INT           NOT NULL DEFAULT 0,
				min_amount     DOUBLE        NOT NULL DEFAULT 0,
				status         VARCHAR(16)   NOT NULL DEFAULT 'open',
				created_at     DATETIME(3)   NOT NULL,
				updated_at     DATETIME(3)   NOT NULL,
				PRIMARY KEY (id),
				INDEX idx_status (status)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_wealth_products`,
	},
	{
		Version: 9702,
		Name:    "create_ce_wealth_holdings",
		Up: `CREATE TABLE IF NOT EXISTS ce_wealth_holdings (
				id              BIGINT        NOT NULL AUTO_INCREMENT,
				user_id         BIGINT        NOT NULL,
				product_id      BIGINT        NOT NULL,
				principal       DOUBLE        NOT NULL DEFAULT 0,
				accrued_yield   DOUBLE        NOT NULL DEFAULT 0,
				status          VARCHAR(16)   NOT NULL DEFAULT 'active',
				created_at      DATETIME(3)   NOT NULL,
				last_accrual_at DATETIME(3)   NOT NULL,
				redeemed_at     DATETIME(3)   NULL DEFAULT NULL,
				updated_at      DATETIME(3)   NOT NULL,
				PRIMARY KEY (id),
				INDEX idx_user (user_id),
				INDEX idx_product (product_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_wealth_holdings`,
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
	runner := migrate.New(db, WealthMigrations)
	if err := runner.Up(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &mysqlStore{db: db}, nil
}

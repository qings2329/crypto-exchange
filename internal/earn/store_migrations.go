package earn

import (
	"database/sql"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// EarnMigrations 是 earn 服务（理财中心 + Launchpool）的建表迁移。
// 版本号使用 96xx 段剩余号（全局唯一，避免与其他模块冲突，见 migrate 包约定）。
var EarnMigrations = []migrate.Migration{
	{
		Version: 9611,
		Name:    "create_ce_earn_products",
		Up: `CREATE TABLE IF NOT EXISTS ce_earn_products (
				id          BIGINT        NOT NULL AUTO_INCREMENT,
				name        VARCHAR(128)  NOT NULL DEFAULT '',
				asset       VARCHAR(32)   NOT NULL,
				term_days   INT           NOT NULL DEFAULT 0,
				apy         DOUBLE        NOT NULL DEFAULT 0,
				min_amount  DOUBLE        NOT NULL DEFAULT 0,
				max_amount  DOUBLE        NOT NULL DEFAULT 0,
				status      VARCHAR(16)   NOT NULL DEFAULT 'open',
				created_at  DATETIME(3)   NOT NULL,
				updated_at  DATETIME(3)   NOT NULL,
				PRIMARY KEY (id),
				INDEX idx_status (status)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_earn_products`,
	},
	{
		Version: 9612,
		Name:    "create_ce_earn_subscriptions",
		Up: `CREATE TABLE IF NOT EXISTS ce_earn_subscriptions (
				id              BIGINT        NOT NULL AUTO_INCREMENT,
				user_id         BIGINT        NOT NULL,
				product_id      BIGINT        NOT NULL,
				asset           VARCHAR(32)   NOT NULL DEFAULT '',
				principal       VARCHAR(64)   NOT NULL DEFAULT '0',
				accrued         VARCHAR(64)   NOT NULL DEFAULT '0',
				status          VARCHAR(16)   NOT NULL DEFAULT 'active',
				created_at      DATETIME(3)   NOT NULL,
				last_accrual_at DATETIME(3)   NOT NULL,
				redeemed_at     DATETIME(3)   NULL DEFAULT NULL,
				redeemed_amount VARCHAR(64)   NOT NULL DEFAULT '0',
				PRIMARY KEY (id),
				INDEX idx_user (user_id),
				INDEX idx_product (product_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_earn_subscriptions`,
	},
	{
		Version: 9613,
		Name:    "create_ce_launch_projects",
		Up: `CREATE TABLE IF NOT EXISTS ce_launch_projects (
				id           BIGINT        NOT NULL AUTO_INCREMENT,
				name         VARCHAR(128)  NOT NULL DEFAULT '',
				token        VARCHAR(32)   NOT NULL DEFAULT '',
				total_supply VARCHAR(64)   NOT NULL DEFAULT '0',
				starts_at    DATETIME(3)   NOT NULL,
				ends_at      DATETIME(3)   NOT NULL,
				pools_json   JSON          NOT NULL,
				funded_total VARCHAR(64)   NOT NULL DEFAULT '0',
				created_at   DATETIME(3)   NOT NULL,
				PRIMARY KEY (id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_launch_projects`,
	},
	{
		Version: 9614,
		Name:    "create_ce_launch_positions",
		Up: `CREATE TABLE IF NOT EXISTS ce_launch_positions (
				id               BIGINT        NOT NULL AUTO_INCREMENT,
				user_id          BIGINT        NOT NULL,
				project_id       BIGINT        NOT NULL,
				pool_id          VARCHAR(32)   NOT NULL DEFAULT '',
				asset            VARCHAR(32)   NOT NULL DEFAULT '',
				token            VARCHAR(32)   NOT NULL DEFAULT '',
				staked           VARCHAR(64)   NOT NULL DEFAULT '0',
				rewards_pending  VARCHAR(64)   NOT NULL DEFAULT '0',
				harvested_total  VARCHAR(64)   NOT NULL DEFAULT '0',
				status           VARCHAR(16)   NOT NULL DEFAULT 'active',
				stake_seq        BIGINT        NOT NULL DEFAULT 0,
				unstake_seq      BIGINT        NOT NULL DEFAULT 0,
				harvest_seq      BIGINT        NOT NULL DEFAULT 0,
				created_at       DATETIME(3)   NOT NULL,
				last_accrual_at  DATETIME(3)   NOT NULL,
				PRIMARY KEY (id),
				UNIQUE KEY uk_user_project_pool (user_id, project_id, pool_id),
				INDEX idx_project (project_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_launch_positions`,
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
	runner := migrate.New(db, EarnMigrations)
	if err := runner.Up(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &mysqlStore{db: db}, nil
}

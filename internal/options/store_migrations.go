package options

import (
	"database/sql"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// OptionsMigrations 是 options 服务的建表迁移。
// 版本号使用 95xx 段（全局唯一，避免与其他模块冲突，见 migrate 包约定）。
var OptionsMigrations = []migrate.Migration{
	{
		Version: 9501,
		Name:    "create_ce_option_contracts",
		Up: `CREATE TABLE IF NOT EXISTS ce_option_contracts (
				id            BIGINT         NOT NULL AUTO_INCREMENT,
				underlying    VARCHAR(32)    NOT NULL,
				quote_asset   VARCHAR(32)    NOT NULL DEFAULT '',
				strike        DOUBLE         NOT NULL DEFAULT 0,
				expiry        DATETIME(3)    NOT NULL,
				type          VARCHAR(8)     NOT NULL,
				style         VARCHAR(16)    NOT NULL,
				contract_size DOUBLE         NOT NULL DEFAULT 1,
				premium       DOUBLE         NOT NULL DEFAULT 0,
				created_at    DATETIME(3)    NOT NULL,
				updated_at    DATETIME(3)    NOT NULL,
				PRIMARY KEY (id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_option_contracts`,
	},
	{
		Version: 9502,
		Name:    "create_ce_option_positions",
		Up: `CREATE TABLE IF NOT EXISTS ce_option_positions (
				id           BIGINT        NOT NULL AUTO_INCREMENT,
				user_id      BIGINT        NOT NULL,
				contract_id  BIGINT        NOT NULL,
				side         VARCHAR(8)    NOT NULL,
				quantity     DOUBLE        NOT NULL DEFAULT 0,
				premium      DOUBLE        NOT NULL DEFAULT 0,
				margin       DOUBLE        NOT NULL DEFAULT 0,
				status       VARCHAR(16)   NOT NULL DEFAULT 'open',
				opened_at    DATETIME(3)   NOT NULL,
				updated_at   DATETIME(3)   NOT NULL,
				PRIMARY KEY (id),
				INDEX idx_user (user_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_option_positions`,
	},
	{
		// F2：premium/margin 由 DOUBLE 改为 VARCHAR(64) 以字符串精确存储 AssetAmount.HumanString
		// （最小单位十进制），避免 float64 列精度丢失；新增 quote_asset 列用于扫描时推导小数位。
		Version: 9503,
		Name:    "alter_ce_option_positions_fixedpoint",
		Up: `ALTER TABLE ce_option_positions
				ADD COLUMN quote_asset VARCHAR(16) NOT NULL DEFAULT '' AFTER quantity,
				MODIFY premium VARCHAR(64) NOT NULL DEFAULT '0',
				MODIFY margin  VARCHAR(64) NOT NULL DEFAULT '0'`,
		Down: `ALTER TABLE ce_option_positions
				MODIFY premium DOUBLE NOT NULL DEFAULT 0,
				MODIFY margin  DOUBLE NOT NULL DEFAULT 0,
				DROP COLUMN quote_asset`,
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
	runner := migrate.New(db, OptionsMigrations)
	if err := runner.Up(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &mysqlStore{db: db}, nil
}

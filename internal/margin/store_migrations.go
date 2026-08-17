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
	{
		// F2：collateral_amount/debt/interest_accrued 由 DOUBLE 改为 VARCHAR(64)，以字符串精确存储
		// AssetAmount.HumanString（最小单位十进制），避免 float64 列精度丢失与利息定点累加漂移；
		// 去除 1e-9 浮点容差，改用 AssetAmount.IsZero 判断还清。
		Version: 9202,
		Name:    "alter_ce_margin_accounts_fixedpoint",
		Up: `ALTER TABLE ce_margin_accounts
				MODIFY collateral_amount VARCHAR(64) NOT NULL DEFAULT '0',
				MODIFY debt             VARCHAR(64) NOT NULL DEFAULT '0',
				MODIFY interest_accrued VARCHAR(64) NOT NULL DEFAULT '0'`,
		Down: `ALTER TABLE ce_margin_accounts
				MODIFY collateral_amount DOUBLE NOT NULL DEFAULT 0,
				MODIFY debt             DOUBLE NOT NULL DEFAULT 0,
				MODIFY interest_accrued DOUBLE NOT NULL DEFAULT 0`,
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

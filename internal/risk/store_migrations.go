package risk

import "github.com/coldlar/crypto-exchange/internal/pkg/migrate"

// RiskMigrations 是风控服务的建表迁移。版本号占用 94xx 段，避免与
// ledger(9001)/user(9101)/margin(9201)/notification(9301) 冲突。
var RiskMigrations = []migrate.Migration{
	{
		Version: 9401,
		Name:    "create_ce_risk_rules",
		Up: `CREATE TABLE IF NOT EXISTS ce_risk_rules (
			id                BIGINT        NOT NULL AUTO_INCREMENT,
			name              VARCHAR(128)  NOT NULL DEFAULT '',
			kind              VARCHAR(32)   NOT NULL,
			scope             VARCHAR(16)   NOT NULL DEFAULT 'global',
			user_id           BIGINT        NOT NULL DEFAULT 0,
			asset             VARCHAR(32)   NOT NULL DEFAULT '',
			max_amount_per_day DOUBLE       NOT NULL DEFAULT 0,
			max_count_per_day  INT          NOT NULL DEFAULT 0,
			min_kyc_level      INT          NOT NULL DEFAULT 0,
			enabled            TINYINT(1)   NOT NULL DEFAULT 1,
			created_at         DATETIME(3)  NOT NULL,
			PRIMARY KEY (id),
			KEY idx_kind (kind)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_risk_rules`,
	},
	{
		Version: 9402,
		Name:    "create_ce_risk_blacklist",
		Up: `CREATE TABLE IF NOT EXISTS ce_risk_blacklist (
			id         BIGINT        NOT NULL AUTO_INCREMENT,
			target     VARCHAR(128)  NOT NULL,
			kind       VARCHAR(16)   NOT NULL,
			reason     VARCHAR(255)  NOT NULL DEFAULT '',
			created_at DATETIME(3)   NOT NULL,
			PRIMARY KEY (id),
			UNIQUE KEY uk_target_kind (target, kind)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_risk_blacklist`,
	},
	{
		Version: 9403,
		Name:    "create_ce_risk_events",
		Up: `CREATE TABLE IF NOT EXISTS ce_risk_events (
			id         BIGINT        NOT NULL AUTO_INCREMENT,
			user_id    BIGINT        NOT NULL DEFAULT 0,
			kind       VARCHAR(32)   NOT NULL,
			detail     TEXT,
			created_at DATETIME(3)   NOT NULL,
			PRIMARY KEY (id),
			KEY idx_user (user_id),
			KEY idx_created (created_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_risk_events`,
	},
}

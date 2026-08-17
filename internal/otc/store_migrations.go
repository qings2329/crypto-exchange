package otc

import (
	"database/sql"

	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// OtcMigrations 是 otc 服务的建表迁移。
// 版本号使用 96xx 段（全局唯一，避免与其他模块冲突，见 migrate 包约定）。
var OtcMigrations = []migrate.Migration{
	{
		Version: 9601,
		Name:    "create_ce_otc_advertisements",
		Up: `CREATE TABLE IF NOT EXISTS ce_otc_advertisements (
				id             BIGINT        NOT NULL AUTO_INCREMENT,
				user_id        BIGINT        NOT NULL,
				side           VARCHAR(8)    NOT NULL,
				asset          VARCHAR(32)   NOT NULL,
				fiat_currency  VARCHAR(8)    NOT NULL DEFAULT '',
				price          DOUBLE        NOT NULL DEFAULT 0,
				min_amount     DOUBLE        NOT NULL DEFAULT 0,
				max_amount     DOUBLE        NOT NULL DEFAULT 0,
				payment_methods VARCHAR(255) NOT NULL DEFAULT '',
				status         VARCHAR(16)   NOT NULL DEFAULT 'open',
				created_at     DATETIME(3)   NOT NULL,
				updated_at     DATETIME(3)   NOT NULL,
				PRIMARY KEY (id),
				INDEX idx_user (user_id),
				INDEX idx_side_asset (side, asset)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_otc_advertisements`,
	},
	{
		Version: 9602,
		Name:    "create_ce_otc_orders",
		Up: `CREATE TABLE IF NOT EXISTS ce_otc_orders (
				id             BIGINT        NOT NULL AUTO_INCREMENT,
				ad_id          BIGINT        NOT NULL,
				maker_id       BIGINT        NOT NULL,
				taker_id       BIGINT        NOT NULL,
				side           VARCHAR(8)    NOT NULL,
				asset          VARCHAR(32)   NOT NULL,
				fiat_currency  VARCHAR(8)    NOT NULL DEFAULT '',
				crypto_amount  DOUBLE        NOT NULL DEFAULT 0,
				price          DOUBLE        NOT NULL DEFAULT 0,
				fiat_amount    DOUBLE        NOT NULL DEFAULT 0,
				payment_method VARCHAR(64)   NOT NULL DEFAULT '',
				status         VARCHAR(16)   NOT NULL DEFAULT 'pending',
				rating         INT           NOT NULL DEFAULT 0,
				created_at     DATETIME(3)   NOT NULL,
				paid_at        DATETIME(3)   NULL DEFAULT NULL,
				completed_at   DATETIME(3)   NULL DEFAULT NULL,
				updated_at     DATETIME(3)   NOT NULL,
				PRIMARY KEY (id),
				INDEX idx_maker (maker_id),
				INDEX idx_taker (taker_id),
				INDEX idx_status (status)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_otc_orders`,
	},
	{
		Version: 9603,
		Name:    "create_ce_otc_counterparties",
		Up: `CREATE TABLE IF NOT EXISTS ce_otc_counterparties (
				id              BIGINT        NOT NULL AUTO_INCREMENT,
				user_id         BIGINT        NOT NULL,
				counterparty_id BIGINT        NOT NULL,
				trades_total    INT           NOT NULL DEFAULT 0,
				trades_completed INT          NOT NULL DEFAULT 0,
				rating_sum      INT           NOT NULL DEFAULT 0,
				rating_count    INT           NOT NULL DEFAULT 0,
				created_at      DATETIME(3)   NOT NULL,
				updated_at      DATETIME(3)   NOT NULL,
				PRIMARY KEY (id),
				UNIQUE KEY uniq_pair (user_id, counterparty_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_otc_counterparties`,
	},
	{
		// F2：crypto_amount 由 DOUBLE 改为 VARCHAR(64)，以字符串精确存储 AssetAmount.HumanString
		// （最小单位十进制），避免 float64 列截断/精度丢失，使锁与释放使用完全一致的整数金额。
		Version: 9604,
		Name:    "alter_ce_otc_orders_crypto_amount_to_varchar",
		Up: `ALTER TABLE ce_otc_orders MODIFY crypto_amount VARCHAR(64) NOT NULL DEFAULT '0'`,
		Down: `ALTER TABLE ce_otc_orders MODIFY crypto_amount DOUBLE NOT NULL DEFAULT 0`,
	},
	{
		Version: 9605,
		Name:    "create_ce_otc_messages",
		Up: `CREATE TABLE IF NOT EXISTS ce_otc_messages (
				id         BIGINT       NOT NULL AUTO_INCREMENT,
				order_id   BIGINT       NOT NULL,
				sender_id  BIGINT       NOT NULL,
				content    TEXT         NOT NULL,
				created_at DATETIME(3)  NOT NULL,
				PRIMARY KEY (id),
				INDEX idx_order (order_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_otc_messages`,
	},
	{
		Version: 9606,
		Name:    "create_ce_otc_proofs",
		Up: `CREATE TABLE IF NOT EXISTS ce_otc_proofs (
				id           BIGINT       NOT NULL AUTO_INCREMENT,
				order_id     BIGINT       NOT NULL,
				uploader_id  BIGINT       NOT NULL,
				file_name    VARCHAR(255) NOT NULL DEFAULT '',
				content_type VARCHAR(128) NOT NULL DEFAULT '',
				size         BIGINT       NOT NULL DEFAULT 0,
				url          VARCHAR(512) NOT NULL DEFAULT '',
				created_at   DATETIME(3)  NOT NULL,
				PRIMARY KEY (id),
				INDEX idx_order (order_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_otc_proofs`,
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
	runner := migrate.New(db, OtcMigrations)
	if err := runner.Up(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &mysqlStore{db: db}, nil
}

package notification

import "github.com/coldlar/crypto-exchange/internal/pkg/migrate"

// NotificationMigrations 是通知服务的建表迁移。版本号占用 93xx 段，避免与
// 其它模块（ledger 9001、user 9101、margin 9201）冲突。
var NotificationMigrations = []migrate.Migration{
	{
		Version: 9320,
		Name:    "create_ce_notifications",
		Up: `CREATE TABLE IF NOT EXISTS ce_notifications (
			id          BIGINT        NOT NULL AUTO_INCREMENT,
			user_id     BIGINT        NOT NULL,
			type        VARCHAR(32)   NOT NULL DEFAULT '',
			title       VARCHAR(255)  NOT NULL DEFAULT '',
			body        TEXT,
			status      VARCHAR(16)   NOT NULL DEFAULT 'unread',
			created_at  DATETIME(3)   NOT NULL,
			PRIMARY KEY (id),
			KEY idx_user_status (user_id, status),
			KEY idx_created (created_at)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		Down: `DROP TABLE IF EXISTS ce_notifications`,
	},
}

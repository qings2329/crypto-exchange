package bot

import (
	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// Migrations 是机器人业务建表迁移。版本号落在 9800+ 空闲区间（9803/9804）。
var Migrations = []migrate.Migration{
	{
		Version: 9803,
		Up: `CREATE TABLE IF NOT EXISTS ce_bot_strategies (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			user_id BIGINT NOT NULL,
			name VARCHAR(128) NOT NULL,
			market VARCHAR(16) NOT NULL,
			symbol VARCHAR(32) NOT NULL,
			side VARCHAR(8) NOT NULL,
			type VARCHAR(16) NOT NULL,
			params JSON NOT NULL,
			status VARCHAR(16) NOT NULL DEFAULT 'stopped',
			user_token VARCHAR(512) NOT NULL DEFAULT '',
			created_at BIGINT NOT NULL,
			INDEX idx_user (user_id)
		)`,
		Down: `DROP TABLE IF EXISTS ce_bot_strategies`,
	},
	{
		Version: 9804,
		Up: `CREATE TABLE IF NOT EXISTS ce_bot_orders (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			strategy_id BIGINT NOT NULL,
			user_id BIGINT NOT NULL,
			market VARCHAR(16) NOT NULL,
			symbol VARCHAR(32) NOT NULL,
			side VARCHAR(8) NOT NULL,
			price DOUBLE NOT NULL DEFAULT 0,
			qty DOUBLE NOT NULL DEFAULT 0,
			client_oid VARCHAR(128) NOT NULL DEFAULT '',
			exchange_order_id VARCHAR(128) NOT NULL DEFAULT '',
			status VARCHAR(16) NOT NULL DEFAULT '',
			created_at BIGINT NOT NULL,
			INDEX idx_strategy (strategy_id)
		)`,
		Down: `DROP TABLE IF EXISTS ce_bot_orders`,
	},
}

package copytrade

import (
	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// Migrations 是跟单业务建表迁移。版本号落在 9800+ 空闲区间（9805/9806/9807）。
var Migrations = []migrate.Migration{
	{
		Version: 9805,
		Up: `CREATE TABLE IF NOT EXISTS ce_copytrade_leads (
			id BIGINT PRIMARY KEY,
			name VARCHAR(128) NOT NULL DEFAULT '',
			bio VARCHAR(512) NOT NULL DEFAULT '',
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			created_at BIGINT NOT NULL,
			INDEX idx_status (status)
		)`,
		Down: `DROP TABLE IF EXISTS ce_copytrade_leads`,
	},
	{
		Version: 9806,
		Up: `CREATE TABLE IF NOT EXISTS ce_copytrade_follows (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			lead_id BIGINT NOT NULL,
			follower_id BIGINT NOT NULL,
			copy_ratio DOUBLE NOT NULL DEFAULT 1,
			allocated_amount DOUBLE NOT NULL DEFAULT 0,
			follower_token VARCHAR(512) NOT NULL DEFAULT '',
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			created_at BIGINT NOT NULL,
			stopped_at BIGINT NOT NULL DEFAULT 0,
			INDEX idx_lead (lead_id),
			INDEX idx_follower (follower_id)
		)`,
		Down: `DROP TABLE IF EXISTS ce_copytrade_follows`,
	},
	{
		Version: 9807,
		Up: `CREATE TABLE IF NOT EXISTS ce_copytrade_copies (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			event_id VARCHAR(128) NOT NULL,
			lead_id BIGINT NOT NULL,
			follow_id BIGINT NOT NULL,
			follower_id BIGINT NOT NULL,
			symbol VARCHAR(32) NOT NULL,
			side VARCHAR(8) NOT NULL,
			price DOUBLE NOT NULL DEFAULT 0,
			qty DOUBLE NOT NULL DEFAULT 0,
			notional DOUBLE NOT NULL DEFAULT 0,
			fee_amount DOUBLE NOT NULL DEFAULT 0,
			exchange_order_id VARCHAR(128) NOT NULL DEFAULT '',
			status VARCHAR(16) NOT NULL DEFAULT '',
			created_at BIGINT NOT NULL,
			UNIQUE KEY uniq_event_follow (event_id, follow_id),
			INDEX idx_follower (follower_id)
		)`,
		Down: `DROP TABLE IF EXISTS ce_copytrade_copies`,
	},
}

package staking

import (
	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// Migrations 是质押业务建表迁移。版本号落在 9800+ 空闲区间（当前最大 9703），
// 与各业务线全局错开，避免 ce_schema_migrations 冲突。
var Migrations = []migrate.Migration{
	{
		Version: 9800,
		Up: `CREATE TABLE IF NOT EXISTS ce_staking_products (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			name VARCHAR(128) NOT NULL,
			chain VARCHAR(32) NOT NULL,
			validator VARCHAR(128) NOT NULL,
			contract_addr VARCHAR(128) NOT NULL,
			asset VARCHAR(16) NOT NULL,
			annual_rate DOUBLE NOT NULL DEFAULT 0,
			duration_days INT NOT NULL DEFAULT 0,
			min_amount BIGINT NOT NULL DEFAULT 0,
			min_amount_decimals INT NOT NULL DEFAULT 0,
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			created_at BIGINT NOT NULL
		)`,
		Down: `DROP TABLE IF EXISTS ce_staking_products`,
	},
	{
		Version: 9802,
		Up: `CREATE TABLE IF NOT EXISTS ce_staking_delegations (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			user_id BIGINT NOT NULL,
			product_id BIGINT NOT NULL,
			principal BIGINT NOT NULL,
			principal_decimals INT NOT NULL,
			status VARCHAR(16) NOT NULL,
			tx_hash VARCHAR(128) NOT NULL DEFAULT '',
			created_at BIGINT NOT NULL,
			unbond_at BIGINT NOT NULL DEFAULT 0,
			unbonded_at BIGINT NOT NULL DEFAULT 0,
			INDEX idx_user (user_id),
			INDEX idx_product (product_id)
		);
		CREATE TABLE IF NOT EXISTS ce_staking_rewards (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			delegation_id BIGINT NOT NULL,
			amount BIGINT NOT NULL,
			amount_decimals INT NOT NULL,
			accrued_at BIGINT NOT NULL,
			INDEX idx_delegation (delegation_id)
		)`,
		Down: `DROP TABLE IF EXISTS ce_staking_delegations; DROP TABLE IF EXISTS ce_staking_rewards`,
	},
}

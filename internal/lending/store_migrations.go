package lending

import "github.com/coldlar/crypto-exchange/internal/pkg/migrate"

// Migrations 是借贷业务建表迁移。版本号落在9900+空闲区间。
var Migrations = []migrate.Migration{
	{
		Version: 9901,
		Up: `CREATE TABLE IF NOT EXISTS ce_lending_pools (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			asset VARCHAR(32) NOT NULL UNIQUE,
			total_supply_json VARCHAR(128) NOT NULL DEFAULT '{}',
			total_borrow_json VARCHAR(128) NOT NULL DEFAULT '{}',
			available_json VARCHAR(128) NOT NULL DEFAULT '{}',
			utilization DOUBLE NOT NULL DEFAULT 0,
			interest_rate DOUBLE NOT NULL DEFAULT 0.05,
			collateral_req DOUBLE NOT NULL DEFAULT 1.5,
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			created_at BIGINT NOT NULL,
			INDEX idx_asset (asset)
		)`,
		Down: `DROP TABLE IF EXISTS ce_lending_pools`,
	},
	{
		Version: 9902,
		Up: `CREATE TABLE IF NOT EXISTS ce_lend_orders (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			user_id BIGINT NOT NULL,
			pool_id BIGINT NOT NULL,
			amount_json VARCHAR(128) NOT NULL DEFAULT '{}',
			rate DOUBLE NOT NULL DEFAULT 0,
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			created_at BIGINT NOT NULL,
			INDEX idx_user (user_id),
			INDEX idx_pool (pool_id)
		)`,
		Down: `DROP TABLE IF EXISTS ce_lend_orders`,
	},
	{
		Version: 9903,
		Up: `CREATE TABLE IF NOT EXISTS ce_borrow_orders (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			user_id BIGINT NOT NULL,
			pool_id BIGINT NOT NULL,
			amount_json VARCHAR(128) NOT NULL DEFAULT '{}',
			collateral_json VARCHAR(128) NOT NULL DEFAULT '{}',
			rate DOUBLE NOT NULL DEFAULT 0,
			interest_acc_json VARCHAR(128) NOT NULL DEFAULT '{}',
			status VARCHAR(16) NOT NULL DEFAULT 'active',
			created_at BIGINT NOT NULL,
			repaid_at BIGINT NOT NULL DEFAULT 0,
			INDEX idx_user (user_id),
			INDEX idx_pool (pool_id)
		)`,
		Down: `DROP TABLE IF EXISTS ce_borrow_orders`,
	},
	{
		Version: 9904,
		Up: `CREATE TABLE IF NOT EXISTS ce_lending_interest (
			id BIGINT PRIMARY KEY AUTO_INCREMENT,
			pool_id BIGINT NOT NULL,
			user_id BIGINT NOT NULL,
			type VARCHAR(32) NOT NULL,
			amount_json VARCHAR(128) NOT NULL DEFAULT '{}',
			recorded_at BIGINT NOT NULL,
			INDEX idx_user (user_id),
			INDEX idx_pool (pool_id)
		)`,
		Down: `DROP TABLE IF EXISTS ce_lending_interest`,
	},
}

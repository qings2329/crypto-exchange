package persist

import "github.com/coldlar/crypto-exchange/internal/pkg/migrate"

// Migrations 返回撮合引擎持久化所需的数据库迁移（版本号落在 200 段，避免与其它模块冲突）。
// 表名均以前缀 ce_ 开头（见 docs/CONVENTIONS.md）。
func Migrations() []migrate.Migration {
	return []migrate.Migration{
		{
			Version: 200,
			Name:    "matching_wal",
			Up: `
CREATE TABLE IF NOT EXISTS ce_matching_wal (
    seq        BIGINT       NOT NULL AUTO_INCREMENT,
    symbol     VARCHAR(32)  NOT NULL,
    event_type VARCHAR(16)  NOT NULL,
    payload    JSON,
    ts         BIGINT       NOT NULL,
    PRIMARY KEY (seq),
    INDEX idx_matching_wal_symbol (symbol)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
			Down: `DROP TABLE IF EXISTS ce_matching_wal;`,
		},
		{
			Version: 201,
			Name:    "matching_snapshot",
			Up: `
CREATE TABLE IF NOT EXISTS ce_matching_snapshot (
    id         INT          NOT NULL,
    version    BIGINT       NOT NULL,
    state      LONGBLOB,
    updated_at DATETIME(3)  NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
			Down: `DROP TABLE IF EXISTS ce_matching_snapshot;`,
		},
		{
			Version: 202,
			Name:    "matching_seq",
			Up: `
CREATE TABLE IF NOT EXISTS ce_matching_seq (
    id  INT    NOT NULL,
    val BIGINT NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
INSERT INTO ce_matching_seq (id, val) VALUES (1, 0) ON DUPLICATE KEY UPDATE id = id;`,
			Down: `DROP TABLE IF EXISTS ce_matching_seq;`,
		},
		{
			Version: 203,
			Name:    "matching_leader",
			Up: `
CREATE TABLE IF NOT EXISTS ce_matching_leader (
    id         INT          NOT NULL,
    holder     VARCHAR(128) NOT NULL,
    expires_at DATETIME(3)  NOT NULL,
    heartbeat  DATETIME(3)  NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
INSERT INTO ce_matching_leader (id, holder, expires_at, heartbeat)
VALUES (1, '', '1970-01-01 00:00:00', '1970-01-01 00:00:00')
ON DUPLICATE KEY UPDATE id = id;`,
			Down: `DROP TABLE IF EXISTS ce_matching_leader;`,
		},
	}
}

package ledger

import (
	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
)

// LedgerMigrations 是账本模块的有序迁移。所有表名以 ce_ 开头（见 docs/CONVENTIONS.md）。
// 新增表/字段时在此追加一条更高版本的 Migration，切勿修改已有条目（保证已部署环境可重入）。
var LedgerMigrations = []migrate.Migration{
	{
		Version: 1,
		Name:    "create_ce_ledger_snapshots",
		Up: `
CREATE TABLE IF NOT EXISTS ce_ledger_snapshots (
    id         VARCHAR(64)  NOT NULL,
    data       MEDIUMTEXT   NOT NULL,
    updated_at DATETIME(3)  NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: `DROP TABLE IF EXISTS ce_ledger_snapshots;`,
	},
}

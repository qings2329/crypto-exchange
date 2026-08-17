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
	{
		// #26：账本操作级幂等指纹持久化。Transfer/Freeze 族在调用方传入非空 ref 时，
		// 由账本层对指纹去重（纵深防双付）。原实现仅用内存 map，进程重启即丢失 →
		// 重启后同 ref 重复提交会双付。本表以唯一约束 (ledger_id, kind, fp) 持久化指纹，
		// 重启后仍可检测重复。kind 冗余区分 transfer/freeze 指纹空间。
		Version: 2,
		Name:    "create_ce_ledger_idempotency",
		Up: `
CREATE TABLE IF NOT EXISTS ce_ledger_idempotency (
    id         BIGINT       NOT NULL AUTO_INCREMENT,
    ledger_id  VARCHAR(64)  NOT NULL,
    kind       VARCHAR(16)  NOT NULL,
    fp         VARCHAR(512) NOT NULL,
    created_at DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY uniq_ledger_kind_fp (ledger_id, kind, fp)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: `DROP TABLE IF EXISTS ce_ledger_idempotency;`,
	},
}

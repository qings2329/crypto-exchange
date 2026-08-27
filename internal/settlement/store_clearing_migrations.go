package settlement

import "github.com/coldlar/crypto-exchange/internal/pkg/migrate"

// clearingMigVer 是清算流水表的迁移版本号（错开 ledger 90xx、matching 200、admin 92xx）。
const clearingMigVer = 9811

// ClearingMigrations 是交易清算流水表的建表迁移，运行时由 NewMySQLClearingStore 应用。
var ClearingMigrations = []migrate.Migration{
	{
		Version: clearingMigVer,
		Name:    "create_ce_settlement_trades",
		Up: `CREATE TABLE IF NOT EXISTS ce_settlement_trades (
    id          BIGINT       NOT NULL,          -- 成交幂等键（FNV64 of 交易字段）
    symbol      VARCHAR(32)  NOT NULL,
    price       DOUBLE       NOT NULL,
    qty         DOUBLE       NOT NULL,
    taker_id    BIGINT       NOT NULL,
    maker_id    BIGINT       NOT NULL,
    taker_side  VARCHAR(8)   NOT NULL,
    fee         DOUBLE       NOT NULL,
    ts          BIGINT       NOT NULL,          -- 撮合成交 unix 毫秒
    cleared_at  DATETIME(3)  NOT NULL,
    PRIMARY KEY (id),
    KEY idx_symbol (symbol),
    KEY idx_cleared_at (cleared_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
		Down: "DROP TABLE IF EXISTS ce_settlement_trades;",
	},
}

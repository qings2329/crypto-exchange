package catalog

import (
	"database/sql"
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

// TestLoadChainRPCEndpoints 在设置了 ADMIN_MYSQL_TEST_DSN 时，对真实 MySQL 验证：
// 仅返回 rpc_endpoint 非空的行，且能以 symbol 为键取回 URL。未设置时跳过。
func TestLoadChainRPCEndpoints(t *testing.T) {
	dsn := os.Getenv("ADMIN_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("set ADMIN_MYSQL_TEST_DSN to run catalog rpc endpoint tests")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("TRUNCATE TABLE ce_admin_chains"); err != nil {
		t.Fatalf("truncate ce_admin_chains: %v", err)
	}
	// 一行有端点、一行无端点。
	if _, err := db.Exec(
		`INSERT INTO ce_admin_chains (name, symbol, confirmations, deposit_enabled, withdraw_enabled, rpc_endpoint, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, NOW(3))`,
		"Bitcoin", "BTC", 3, 1, 1, "http://rpcuser:rpcpass@127.0.0.1:8332",
	); err != nil {
		t.Fatalf("insert btc: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO ce_admin_chains (name, symbol, confirmations, deposit_enabled, withdraw_enabled, rpc_endpoint, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, NOW(3))`,
		"Litecoin", "LTC", 6, 1, 1, "",
	); err != nil {
		t.Fatalf("insert ltc: %v", err)
	}

	rpcMap, err := LoadChainRPCEndpoints(db)
	if err != nil {
		t.Fatalf("LoadChainRPCEndpoints: %v", err)
	}
	if rpcMap["BTC"] != "http://rpcuser:rpcpass@127.0.0.1:8332" {
		t.Fatalf("应返回 BTC 端点, 实际 %+v", rpcMap)
	}
	if _, ok := rpcMap["LTC"]; ok {
		t.Fatalf("空端点的 LTC 不应出现在结果中, 实际 %+v", rpcMap)
	}
}

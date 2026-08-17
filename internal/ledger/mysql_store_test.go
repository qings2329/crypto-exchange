package ledger

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// amt 把人类单位浮点按资产标准小数位包装为 AssetAmount（测试边界用）。
func amt(asset string, human float64) settlement.AssetAmount {
	return settlement.AssetAmountFromFloat(human, settlement.AssetDecimalsByName(asset))
}

// eqAmt 比较实际 AssetAmount 与人类单位期望值（按 actual.Decimals 对齐）。
func eqAmt(actual settlement.AssetAmount, wantHuman float64) bool {
	return actual.Cmp(settlement.AssetAmountFromFloat(wantHuman, actual.Decimals)) == 0
}

// TestSaveLoadMySQL 是 MySQL 持久化的集成测试，仅在设置了 MYSQL_TEST_DSN 环境变量时运行
// （指向一个测试库，表会自动创建）。CI/本地无 MySQL 时自动跳过，避免阻塞单元测试。
// 它验证 SaveToMySQL -> LoadSnapshotFromMySQL 的往返与既有 Snapshot/Restore 不变量一致。
func TestSaveLoadMySQL(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL integration test")
	}
	const ledgerID = "test_mysql_roundtrip"

	// 准备一个含多种资金安全态的账本，校验子结构均随快照正确落库/恢复。
	l := New()
	l.SetWithdrawHoldPeriod(50 * time.Millisecond)
	l.SetAddressVerifyPeriod(0)
	l.EnableRiskEngine(true, true)
	l.SetRiskThresholds(60*time.Second, 0, 2, 0)
	_ = l.Deposit(70001, "USDT", amt("USDT", 100000), "seed")
	_, _ = l.AddWithdrawAddress(70001, "USDT", "ETH", "0xsafe", "main")
	_ = l.ConfirmWithdrawAddress(70001, "USDT", "ETH", "0xsafe")
	if _, _, err := l.RequestWithdrawHold(70001, "USDT", amt("USDT", 100), amt("USDT", 0), "ETH", "0xsafe"); err != nil {
		t.Fatalf("seed withdraw hold: %v", err)
	}
	// 第二次触发风控自动冻结，应被拒（这正是我们希望随快照持久化的状态之一）。
	if _, _, err := l.RequestWithdrawHold(70001, "USDT", amt("USDT", 100), amt("USDT", 0), "ETH", "0xsafe"); err == nil {
		t.Fatal("2nd withdraw should be blocked by risk auto-freeze")
	}
	if !l.AutoFrozenByRisk() {
		t.Fatal("expected risk auto-freeze before save")
	}

	if err := l.SaveToMySQL(dsn, ledgerID); err != nil {
		t.Fatalf("SaveToMySQL: %v", err)
	}
	snap, ok, err := LoadSnapshotFromMySQL(dsn, ledgerID)
	if err != nil {
		t.Fatalf("LoadSnapshotFromMySQL: %v", err)
	}
	if !ok {
		t.Fatal("expected a saved snapshot row")
	}

	// 用快照重建一个新账本，验证资金安全态完整恢复。
	l2 := New()
	l2.Restore(snap)
	if !l2.AutoFrozenByRisk() {
		t.Fatal("auto_frozen_by_risk should survive MySQL roundtrip")
	}
	if l2.RiskEventCount() != 1 {
		t.Fatalf("risk events should survive roundtrip, got %d", l2.RiskEventCount())
	}
	if l2.PendingWithdrawHoldCount() != 1 {
		t.Fatalf("withdraw holds should survive roundtrip, got %d", l2.PendingWithdrawHoldCount())
	}
	if avail, _, ok := l2.Balance(70001, "USDT"); !ok || !eqAmt(avail, 99900) {
		t.Fatalf("available balance should reflect 1 frozen hold (~99900), got %v ok=%v", avail, ok)
	}
}

// TestIdempotencyPersistedToMySQL 验证 #26 的幂等指纹持久化：进程重启（全新 Ledger 实例、
// 内存去重 map 为空）后，同 ref 的重复提交会被 DB 唯一约束拦截，从而杜绝"重启后同 ref 双付"。
// 仅在设置 MYSQL_TEST_DSN 时运行；无 MySQL 时跳过。
func TestIdempotencyPersistedToMySQL(t *testing.T) {
	dsn := os.Getenv("MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("MYSQL_TEST_DSN not set; skipping MySQL integration test")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("db.Ping: %v", err)
	}

	const ledgerID = "idem_test"
	// 清理可能残留的同一 ledger_id 指纹，保证测试可重复（SETUP）。
	if _, err := db.Exec("DELETE FROM ce_ledger_idempotency WHERE ledger_id=?", ledgerID); err != nil {
		t.Fatalf("cleanup idempotency rows: %v", err)
	}

	// 第一次运行（模拟进程生命周期 1）：存款并发起带 ref 的转账，指纹落库。
	l1 := New()
	if err := l1.SetIdempotencyDB(db, ledgerID); err != nil {
		t.Fatalf("SetIdempotencyDB l1: %v", err)
	}
	_ = l1.Deposit(1, "USDT", amt("USDT", 100000), "seed")
	if err := l1.Transfer(1, 2, "USDT", amt("USDT", 5000), "transfer", "R1"); err != nil {
		t.Fatalf("l1 transfer R1: %v", err)
	}
	if avail, _, ok := l1.Balance(1, "USDT"); !ok || !eqAmt(avail, 95000) {
		t.Fatalf("l1 sender after R1: %v want 95000", avail)
	}

	// 模拟重启：全新 Ledger 实例（内存幂等 map 为空），但共享同一 DB 后端。
	l2 := New()
	if err := l2.SetIdempotencyDB(db, ledgerID); err != nil {
		t.Fatalf("SetIdempotencyDB l2: %v", err)
	}
	// 关键前提：重启后 l2 尚未感知过 R1（内存 map 空），若 DB 不拦截就会双付。
	if err := l2.Transfer(1, 2, "USDT", amt("USDT", 5000), "transfer", "R1"); err != nil {
		t.Fatalf("l2 transfer R1 (dup): %v", err)
	}
	// DB 命中唯一约束 → 跳过 → l2 余额未变化（发送方仍是满额，接收方仍是 0）。
	if avail, _, ok := l2.Balance(1, "USDT"); !ok || !eqAmt(avail, 100000) {
		t.Fatalf("l2 sender after dup R1: %v want 100000 (no double pay across restart)", avail)
	}
	if recv, _, ok := l2.Balance(2, "USDT"); !ok || !eqAmt(recv, 0) {
		t.Fatalf("l2 receiver after dup R1: %v want 0 (no double pay across restart)", recv)
	}

	// 指纹仅写入一次（INSERT IGNORE 收敛并发/重启重复写入）。
	fp := transferFingerprint(1, 2, "USDT", amt("USDT", 5000), "transfer", "R1")
	var cnt int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM ce_ledger_idempotency WHERE ledger_id=? AND kind='transfer' AND fp=?",
		ledgerID, fp,
	).Scan(&cnt); err != nil {
		t.Fatalf("count idempotency rows: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("expected exactly 1 idempotency row for R1, got %d", cnt)
	}
}

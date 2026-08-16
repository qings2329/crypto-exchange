package ledger

import (
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

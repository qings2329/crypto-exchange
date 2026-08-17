package ledger

import "testing"

// TestBatchAllApply 整组操作全部成功时，所有步骤均生效（多腿业务事务的常态路径）。
func TestBatchAllApply(t *testing.T) {
	l := New()
	_ = l.Deposit(1, "USDT", amt("USDT", 100000), "seed")
	logBefore := len(l.log)

	ops := []Op{
		{Kind: OpTransfer, From: 1, To: 2, Asset: "USDT", Amount: amt("USDT", 5000), Biz: "transfer", Ref: "b1"},
		{Kind: OpTransfer, From: 1, To: 3, Asset: "USDT", Amount: amt("USDT", 5000), Biz: "transfer", Ref: "b2"},
	}
	if err := l.Batch(ops); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if a, _, _ := l.Balance(1, "USDT"); !eqAmt(a, 90000) {
		t.Fatalf("sender %v want 90000", a)
	}
	if b, _, _ := l.Balance(2, "USDT"); !eqAmt(b, 5000) {
		t.Fatalf("receiver2 %v want 5000", b)
	}
	if c, _, _ := l.Balance(3, "USDT"); !eqAmt(c, 5000) {
		t.Fatalf("receiver3 %v want 5000", c)
	}
	// 每笔转账记两条流水，共 4 条；审计轨迹完整。
	if got := len(l.log) - logBefore; got != 4 {
		t.Fatalf("log grew by %d entries, want 4", got)
	}
}

// TestBatchAtomicRollback 任一步失败则整组回滚：余额、幂等 map、审计流水全部还原，
// 且无部分生效（无双付/无半冻结）。回滚后同组操作可安全重试。
func TestBatchAtomicRollback(t *testing.T) {
	l := New()
	_ = l.Deposit(1, "USDT", amt("USDT", 100000), "seed")
	logBefore := len(l.log)

	// 第一步转账成功（1->2, 5000），第二步冻结 99999 因余额不足（仅余 95000）失败。
	ops := []Op{
		{Kind: OpTransfer, From: 1, To: 2, Asset: "USDT", Amount: amt("USDT", 5000), Biz: "transfer", Ref: "rb1"},
		{Kind: OpFreeze, User: 1, Asset: "USDT", Amount: amt("USDT", 99999), Ref: "rb2"},
	}
	if err := l.Batch(ops); err == nil {
		t.Fatal("expected batch to fail on insufficient freeze, got nil")
	}
	// 回滚：发送方全额、接收方为零、无孤儿流水。
	if a, _, _ := l.Balance(1, "USDT"); !eqAmt(a, 100000) {
		t.Fatalf("sender after rollback %v want 100000 (no partial transfer)", a)
	}
	if b, _, _ := l.Balance(2, "USDT"); !eqAmt(b, 0) {
		t.Fatalf("receiver after rollback %v want 0 (no partial transfer)", b)
	}
	if got := len(l.log) - logBefore; got != 0 {
		t.Fatalf("log grew by %d entries after rollback, want 0 (no orphan audit)", got)
	}

	// 回滚后同组操作可安全重试：转账指纹（rb1）未被残留的 transferSeen 误杀。
	if err := l.Batch([]Op{
		{Kind: OpTransfer, From: 1, To: 2, Asset: "USDT", Amount: amt("USDT", 5000), Biz: "transfer", Ref: "rb1"},
	}); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	if a, _, _ := l.Balance(1, "USDT"); !eqAmt(a, 95000) {
		t.Fatalf("sender after retry %v want 95000", a)
	}
	if b, _, _ := l.Balance(2, "USDT"); !eqAmt(b, 5000) {
		t.Fatalf("receiver after retry %v want 5000", b)
	}
}

// TestBatchIdempotentRef 同 ref 的批量操作被内存去重跳过（与单笔 Transfer 幂等一致）。
func TestBatchIdempotentRef(t *testing.T) {
	l := New()
	_ = l.Deposit(1, "USDT", amt("USDT", 100000), "seed")
	ops := []Op{
		{Kind: OpTransfer, From: 1, To: 2, Asset: "USDT", Amount: amt("USDT", 5000), Biz: "transfer", Ref: "dup"},
	}
	if err := l.Batch(ops); err != nil {
		t.Fatalf("batch#1: %v", err)
	}
	if a, _, _ := l.Balance(1, "USDT"); !eqAmt(a, 95000) {
		t.Fatalf("after#1 sender %v want 95000", a)
	}
	// 重复提交同 ref：应被跳过（余额与流水均不变）。
	if err := l.Batch(ops); err != nil {
		t.Fatalf("batch#2: %v", err)
	}
	if a, _, _ := l.Balance(1, "USDT"); !eqAmt(a, 95000) {
		t.Fatalf("after#2 sender %v want 95000 (no double pay)", a)
	}
	if b, _, _ := l.Balance(2, "USDT"); !eqAmt(b, 5000) {
		t.Fatalf("after#2 receiver %v want 5000 (no double pay)", b)
	}
}

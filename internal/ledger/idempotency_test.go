package ledger

import "testing"

// TestTransferIdempotentByRefE2E 端到端验证 Transfer 带 ref 的幂等：
// 同 ref 重复提交被跳过（余额与账本流水条目均不变，证明未双付、未重复记账）。
func TestTransferIdempotentByRefE2E(t *testing.T) {
	l := New()
	_ = l.Deposit(1, "USDT", amt("USDT", 100000), "seed")
	ref := "tx-1001"

	if err := l.Transfer(1, 2, "USDT", amt("USDT", 5000), "transfer", ref); err != nil {
		t.Fatalf("transfer#1: %v", err)
	}
	a1, _, _ := l.Balance(1, "USDT")
	b1, _, _ := l.Balance(2, "USDT")
	entriesAfterFirst := len(l.log)
	if !eqAmt(a1, 95000) {
		t.Fatalf("sender after#1: %v want 95000", a1)
	}
	if !eqAmt(b1, 5000) {
		t.Fatalf("receiver after#1: %v want 5000", b1)
	}

	// 同 ref 重复提交：应被账本层跳过。
	if err := l.Transfer(1, 2, "USDT", amt("USDT", 5000), "transfer", ref); err != nil {
		t.Fatalf("transfer#2: %v", err)
	}
	a2, _, _ := l.Balance(1, "USDT")
	b2, _, _ := l.Balance(2, "USDT")
	entriesAfterSecond := len(l.log)
	if !eqAmt(a2, 95000) {
		t.Fatalf("sender after#2: %v want 95000 (no double pay)", a2)
	}
	if !eqAmt(b2, 5000) {
		t.Fatalf("receiver after#2: %v want 5000 (no double pay)", b2)
	}
	if entriesAfterSecond != entriesAfterFirst {
		t.Fatalf("entries changed: %d -> %d (duplicate must not add ledger entries)", entriesAfterFirst, entriesAfterSecond)
	}
}

// TestFreezeIdempotentByRefE2E 端到端验证 Freeze 带 ref 的幂等：
// 同 ref 重复冻结不再扣减可用/增加冻结。
func TestFreezeIdempotentByRefE2E(t *testing.T) {
	l := New()
	_ = l.Deposit(1, "USDT", amt("USDT", 100000), "seed")
	ref := "frz-1"

	if err := l.Freeze(1, "USDT", amt("USDT", 1000), ref); err != nil {
		t.Fatalf("freeze#1: %v", err)
	}
	avail1, frozen1, _ := l.Balance(1, "USDT")
	if !eqAmt(avail1, 99000) || !eqAmt(frozen1, 1000) {
		t.Fatalf("after#1 avail=%v frozen=%v", avail1, frozen1)
	}

	if err := l.Freeze(1, "USDT", amt("USDT", 1000), ref); err != nil {
		t.Fatalf("freeze#2: %v", err)
	}
	avail2, frozen2, _ := l.Balance(1, "USDT")
	if !eqAmt(avail2, 99000) || !eqAmt(frozen2, 1000) {
		t.Fatalf("after#2 avail=%v frozen=%v (no double freeze)", avail2, frozen2)
	}
}

// TestIdempotencyDistinctRefsApply 不同 ref 各自的相同操作都应生效（不误去重）。
func TestIdempotencyDistinctRefsApply(t *testing.T) {
	l := New()
	_ = l.Deposit(1, "USDT", amt("USDT", 100000), "seed")
	if err := l.Transfer(1, 2, "USDT", amt("USDT", 1000), "transfer", "r-a"); err != nil {
		t.Fatalf("r-a: %v", err)
	}
	if err := l.Transfer(1, 2, "USDT", amt("USDT", 1000), "transfer", "r-b"); err != nil {
		t.Fatalf("r-b: %v", err)
	}
	a, _, _ := l.Balance(1, "USDT")
	b, _, _ := l.Balance(2, "USDT")
	if !eqAmt(a, 98000) {
		t.Fatalf("sender %v want 98000 (both applied)", a)
	}
	if !eqAmt(b, 2000) {
		t.Fatalf("receiver %v want 2000 (both applied)", b)
	}
}

// TestNoRefDistinctOpsBothApply 无 ref 时，互不相同的操作（不同金额）仍各自生效，
// 证明 #26 仅对 ref!="" 触达 DB、未改变无 ref 路径的既有行为（内存 map 仅对完全相同的
// 指纹去重，这是 transferSeen 的既有语义，不属于本次改动）。
func TestNoRefDistinctOpsBothApply(t *testing.T) {
	l := New()
	_ = l.Deposit(1, "USDT", amt("USDT", 100000), "seed")
	if err := l.Transfer(1, 2, "USDT", amt("USDT", 1000), "transfer", ""); err != nil {
		t.Fatalf("t1: %v", err)
	}
	if err := l.Transfer(1, 2, "USDT", amt("USDT", 2000), "transfer", ""); err != nil {
		t.Fatalf("t2: %v", err)
	}
	a, _, _ := l.Balance(1, "USDT")
	if !eqAmt(a, 97000) {
		t.Fatalf("sender %v want 97000 (both applied, no ref, distinct amounts)", a)
	}
}

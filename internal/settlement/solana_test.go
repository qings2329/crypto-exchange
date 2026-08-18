package settlement

import (
	"context"
	"testing"
	"time"
)

const b58Charset = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func isBase58(s string) bool {
	for i := 0; i < len(s); i++ {
		if !containsByte(b58Charset, s[i]) {
			return false
		}
	}
	return true
}

func containsByte(set string, b byte) bool {
	for i := 0; i < len(set); i++ {
		if set[i] == b {
			return true
		}
	}
	return false
}

// TestSolanaAddressFormat Solana 充值地址为确定性、base58、ed25519 风格的 32–44 字符地址。
func TestSolanaAddressFormat(t *testing.T) {
	a1 := deriveSolanaAddress(1)
	a1b := deriveSolanaAddress(1)
	if a1 != a1b {
		t.Fatalf("deriveSolanaAddress not deterministic: %s vs %s", a1, a1b)
	}
	if len(a1) < 32 || len(a1) > 44 {
		t.Fatalf("solana address length out of range: %d (%s)", len(a1), a1)
	}
	if !isBase58(a1) {
		t.Fatalf("solana address not base58: %s", a1)
	}
	// 与其他链格式区分。
	if GenerateAddress(1, ChainSOL) == GenerateAddress(1, ChainETH) {
		t.Fatal("SOL address must differ from ETH address")
	}
	// 经 GenerateAddress 暴露的 SOL 地址也是 base58（非 mock 占位下划线格式）。
	if !isBase58(GenerateAddress(1, ChainSOL)) {
		t.Fatalf("GenerateAddress(SOL) not base58: %s", GenerateAddress(1, ChainSOL))
	}
}

// TestSolanaDepositMock 充值网关对 ChainSOL 透明支持：达到安全确认数后入账并推送事件。
func TestSolanaDepositMock(t *testing.T) {
	g := NewMockChainGateway(2, time.Hour) // required=2，手动 Tick
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := g.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := g.SubmitDeposit(1, "SOL", ChainSOL, AssetAmountFromFloat(1, 9), "")
	if err != nil {
		t.Fatal(err)
	}
	g.Tick() // 1/2 确认
	g.Tick() // 2/2 -> Credited
	select {
	case got := <-ch:
		if got.Status != DepositCredited || got.TxHash != ev.TxHash {
			t.Fatalf("unexpected deposit event %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for credited deposit event")
	}
	if ev.Status != DepositCredited {
		t.Fatalf("expect DepositCredited, got %s", ev.Status)
	}
}

// TestSolanaWithdrawMock 提现网关对 ChainSOL 透明支持：达到安全确认数后入账。
func TestSolanaWithdrawMock(t *testing.T) {
	g := NewMockWithdrawGateway(2, time.Hour)
	ev, err := g.SubmitWithdraw(1, "SOL", ChainSOL, AssetAmountFromFloat(1, 9), AssetAmountFromFloat(0.001, 9), "addr", false)
	if err != nil {
		t.Fatal(err)
	}
	g.Tick()
	g.Tick()
	if ev.Status != WithdrawCredited {
		t.Fatalf("expect WithdrawCredited, got %s", ev.Status)
	}
}

// TestSolanaSPLDepositMock SPL 代币（USDC，6 位小数）在 ChainSOL 上同样可被充值网关接受。
func TestSolanaSPLDepositMock(t *testing.T) {
	g := NewMockChainGateway(2, time.Hour)
	ev, err := g.SubmitDeposit(1, "USDC", ChainSOL, AssetAmountFromFloat(10, 6), "")
	if err != nil {
		t.Fatal(err)
	}
	g.Tick()
	g.Tick()
	if ev.Status != DepositCredited {
		t.Fatalf("expect DepositCredited for SPL USDC, got %s", ev.Status)
	}
}

// TestSolanaDecimals 校验 Solana 小数位：原生 SOL=9，SPL（USDC）=6；资产白名单含 SOL。
func TestSolanaDecimals(t *testing.T) {
	if d := AssetDecimals(ChainSOL, "SOL"); d != 9 {
		t.Fatalf("AssetDecimals(SOL,SOL) want 9, got %d", d)
	}
	if d := AssetDecimals(ChainSOL, "USDC"); d != 6 {
		t.Fatalf("AssetDecimals(SOL,USDC) want 6, got %d", d)
	}
	if d := AssetDecimalsByName("SOL"); d != 9 {
		t.Fatalf("AssetDecimalsByName(SOL) want 9, got %d", d)
	}
	if !KnownAsset("SOL") {
		t.Fatal("KnownAsset(SOL) should be true (F5 资产白名单)")
	}
	if KnownAsset("NOT_A_COIN") {
		t.Fatal("KnownAsset must reject unknown asset")
	}
}

// TestSolanaF5Boundary F5 边界：非正金额充值/提现被拒；资产白名单正确拒绝未知资产。
func TestSolanaF5Boundary(t *testing.T) {
	g := NewMockChainGateway(2, time.Hour)
	if _, err := g.SubmitDeposit(1, "SOL", ChainSOL, AssetAmountFromFloat(0, 9), ""); err == nil {
		t.Fatal("expect error for zero-amount deposit")
	}
	wg := NewMockWithdrawGateway(2, time.Hour)
	if _, err := wg.SubmitWithdraw(1, "SOL", ChainSOL, AssetAmountFromFloat(0, 9), AssetAmountFromFloat(0.001, 9), "addr", false); err == nil {
		t.Fatal("expect error for zero-amount withdraw")
	}
	if _, err := wg.SubmitWithdraw(1, "SOL", ChainSOL, AssetAmountFromFloat(-1, 9), AssetAmountFromFloat(0.001, 9), "addr", false); err == nil {
		t.Fatal("expect error for negative-amount withdraw")
	}
}

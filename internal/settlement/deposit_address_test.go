package settlement

import (
	"regexp"
	"testing"

	"github.com/tyler-smith/go-bip32"
)

// deriveTestXPUB 用固定种子构造 master，按 BIP44 硬化路径 m/44'/0'/0'/0 派生（硬化步骤需私钥，
// 模拟 HSM 在内部用 xprv 完成），取外部链级 xpub 字符串返回。进程侧只拿到这个 xpub。
func deriveTestXPUB(t *testing.T) string {
	t.Helper()
	seed := []byte("fixed-seed-for-deposit-address-test-0123456789")
	m, err := bip32.NewMasterKey(seed)
	if err != nil {
		t.Fatalf("NewMasterKey: %v", err)
	}
	h := func(k *bip32.Key, idx uint32) *bip32.Key {
		c, err := k.NewChildKey(idx + bip32.FirstHardenedChild)
		if err != nil {
			t.Fatalf("hardened child %d: %v", idx, err)
		}
		return c
	}
	acct := h(h(m, 44), 0) // 44'/0'
	acct = h(acct, 0)      // 0' (account)
	external, err := acct.NewChildKey(0)
	if err != nil {
		t.Fatalf("external child: %v", err)
	}
	return external.PublicKey().String()
}

func setGenForTest(t *testing.T, g *DepositAddressGenerator) {
	t.Helper()
	prev := depositAddrGen
	SetDepositAddressGenerator(g)
	t.Cleanup(func() { SetDepositAddressGenerator(prev) })
}

func TestDepositAddressHDDerivation(t *testing.T) {
	xpub := deriveTestXPUB(t)
	gen, err := NewDepositAddressGenerator(DepositConfig{XPUB: xpub, BTCAddressType: "p2wpkh"})
	if err != nil {
		t.Fatalf("NewDepositAddressGenerator: %v", err)
	}

	ethRe := regexp.MustCompile(`^0x[0-9a-fA-F]{40}$`)
	tronRe := regexp.MustCompile(`^T[1-9A-HJ-NP-Za-km-z]{33}$`)
	btcRe := regexp.MustCompile(`^bc1[qpzry9x8gf2tvdw0s3jn54khce6mua7l]+$`)

	for _, uid := range []int64{1, 2, 100, 999999} {
		eth, err := gen.Address(uid, ChainETH)
		if err != nil {
			t.Fatalf("ETH Address(%d): %v", uid, err)
		}
		if !ethRe.MatchString(eth) {
			t.Fatalf("ETH 地址格式非法: %q", eth)
		}
		tron, err := gen.Address(uid, ChainTRON)
		if err != nil {
			t.Fatalf("TRON Address(%d): %v", uid, err)
		}
		if !tronRe.MatchString(tron) {
			t.Fatalf("TRON 地址格式非法: %q", tron)
		}
		btc, err := gen.Address(uid, ChainBTC)
		if err != nil {
			t.Fatalf("BTC Address(%d): %v", uid, err)
		}
		if !btcRe.MatchString(btc) {
			t.Fatalf("BTC 地址格式非法: %q", btc)
		}
	}

	// 确定性：同入参两次相同。
	if a, _ := gen.Address(7, ChainETH); a != mustGen(t, gen, 7, ChainETH) {
		t.Fatalf("ETH 地址应确定性一致")
	}
	// 每用户不同：不同 userID 地址不同。
	if mustGen(t, gen, 1, ChainETH) == mustGen(t, gen, 2, ChainETH) {
		t.Fatalf("不同 userID 应得到不同地址")
	}
	// 跨链地址不同（同一子公钥、不同编码）。
	if mustGen(t, gen, 1, ChainETH) == mustGen(t, gen, 1, ChainTRON) {
		t.Fatalf("同用户 ETH/TRON 地址应不同")
	}
}

func mustGen(t *testing.T, g *DepositAddressGenerator, uid int64, chain Chain) string {
	t.Helper()
	a, err := g.Address(uid, chain)
	if err != nil {
		t.Fatalf("Address(%d,%s): %v", uid, chain, err)
	}
	return a
}

// TestDepositAddressFromXPUBOnly 验证「进程只持 xpub（不接触 xprv）」即可派生子地址，契合 HSM 模型。
func TestDepositAddressFromXPUBOnly(t *testing.T) {
	xpub := deriveTestXPUB(t)
	// 仅反序列化 xpub 字符串，确认其为公钥扩展密钥（不含私钥）。
	master, err := bip32.B58Deserialize(xpub)
	if err != nil {
		t.Fatalf("B58Deserialize xpub: %v", err)
	}
	if master.IsPrivate {
		t.Fatalf("xpub 不应含私钥")
	}
	gen, err := NewDepositAddressGenerator(DepositConfig{XPUB: xpub})
	if err != nil {
		t.Fatalf("NewDepositAddressGenerator: %v", err)
	}
	// 仅用 xpub 派生，不应触及任何私钥。
	addr, err := gen.Address(42, ChainETH)
	if err != nil || addr == "" {
		t.Fatalf("仅持 xpub 应可派生地址: addr=%q err=%v", addr, err)
	}
}

func TestGenerateAddressRealWhenConfigured(t *testing.T) {
	xpub := deriveTestXPUB(t)
	gen, err := NewDepositAddressGenerator(DepositConfig{XPUB: xpub, BTCAddressType: "p2wpkh"})
	if err != nil {
		t.Fatalf("NewDepositAddressGenerator: %v", err)
	}
	setGenForTest(t, gen)

	addr := GenerateAddress(5, ChainETH)
	if len(addr) < 2 || addr[:2] != "0x" {
		t.Fatalf("配置后应返回真实 ETH 地址，got %q", addr)
	}
	if GenerateAddress(5, ChainTRON)[:1] != "T" {
		t.Fatalf("配置后应返回真实 TRON 地址")
	}
	if GenerateAddress(5, ChainBTC)[:3] != "bc1" {
		t.Fatalf("配置后应返回真实 BTC 地址(bech32)")
	}
}

func TestGenerateAddressFallbackUnconfigured(t *testing.T) {
	// 确保全局为 nil（无生成器）→ 回退 mock 占位地址（兼容既有 TestGenerateHelpers 的 "ETH" 前缀断言）。
	prev := depositAddrGen
	SetDepositAddressGenerator(nil)
	t.Cleanup(func() { SetDepositAddressGenerator(prev) })

	addr := GenerateAddress(1, ChainETH)
	if len(addr) < 3 || addr[:3] != "ETH" {
		t.Fatalf("未配置应回退 mock 地址(以 ETH 前缀)，got %q", addr)
	}
}

func TestGenerateAddressDeriveErrorFallsBack(t *testing.T) {
	xpub := deriveTestXPUB(t)
	gen, err := NewDepositAddressGenerator(DepositConfig{XPUB: xpub})
	if err != nil {
		t.Fatalf("NewDepositAddressGenerator: %v", err)
	}
	setGenForTest(t, gen)

	// 非法 userID（越界）→ Address 报错 → GenerateAddress 回退 mock（fail-degraded 不变式）。
	addr := GenerateAddress(-1, ChainETH)
	if len(addr) < 3 || addr[:3] != "ETH" {
		t.Fatalf("派生失败应回退 mock 地址，got %q", addr)
	}
	if _, err := gen.Address(-1, ChainETH); err == nil {
		t.Fatalf("负 userID 应返回错误")
	}
}

func TestDepositAddressBTCTypes(t *testing.T) {
	xpub := deriveTestXPUB(t)
	for _, typ := range []string{"p2wpkh", "p2pkh"} {
		gen, err := NewDepositAddressGenerator(DepositConfig{XPUB: xpub, BTCAddressType: typ})
		if err != nil {
			t.Fatalf("NewDepositAddressGenerator(%s): %v", typ, err)
		}
		addr, err := gen.Address(1, ChainBTC)
		if err != nil {
			t.Fatalf("BTC Address(%s): %v", typ, err)
		}
		switch typ {
		case "p2wpkh":
			if addr[:3] != "bc1" {
				t.Fatalf("p2wpkh 应得 bech32 地址，got %q", addr)
			}
		case "p2pkh":
			if addr[:1] != "1" {
				t.Fatalf("p2pkh 应得 base58 地址，got %q", addr)
			}
		}
	}
}

func TestNewDepositAddressGeneratorInvalid(t *testing.T) {
	if _, err := NewDepositAddressGenerator(DepositConfig{XPUB: ""}); err == nil {
		t.Fatalf("空 xpub 应报错")
	}
	if _, err := NewDepositAddressGenerator(DepositConfig{XPUB: "not-a-valid-xpub"}); err == nil {
		t.Fatalf("非法 xpub 应报错")
	}
}

// TestConfigureDepositAddresses 覆盖配置驱动接线函数的各分支：未启用/空 xpub 不注册；
// 启用且 xpub 合法→注册；xpub 非法→不 panic、不注册（fail-degraded）。
func TestConfigureDepositAddresses(t *testing.T) {
	prev := depositAddrGen
	t.Cleanup(func() { SetDepositAddressGenerator(prev) })

	xpub := deriveTestXPUB(t)

	// 未启用 → 不注册（全局保持原状，此处为 nil）。
	SetDepositAddressGenerator(nil)
	ConfigureDepositAddresses(DepositConfig{Enabled: false, XPUB: xpub})
	if depositAddrGen != nil {
		t.Fatalf("未启用不应注册生成器")
	}

	// 启用但 xpub 空 → 不注册。
	ConfigureDepositAddresses(DepositConfig{Enabled: true, XPUB: ""})
	if depositAddrGen != nil {
		t.Fatalf("xpub 空不应注册生成器")
	}

	// 启用且 xpub 合法 → 注册成功。
	ConfigureDepositAddresses(DepositConfig{Enabled: true, XPUB: xpub, BTCAddressType: "p2wpkh"})
	if depositAddrGen == nil {
		t.Fatalf("启用且 xpub 合法应注册生成器")
	}
	if _, err := depositAddrGen.Address(1, ChainETH); err != nil {
		t.Fatalf("注册后派生失败: %v", err)
	}

	// xpub 非法 → 不 panic、不覆盖（保持上一步注册的合法生成器）。
	ConfigureDepositAddresses(DepositConfig{Enabled: true, XPUB: "bad-xpub"})
	if depositAddrGen == nil {
		t.Fatalf("xpub 非法不应清空已注册生成器")
	}
}

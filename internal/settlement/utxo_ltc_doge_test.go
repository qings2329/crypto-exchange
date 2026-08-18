package settlement

import (
	"bytes"
	"context"
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// TestDepositAddressLTCAndDOGE 验证 LTC/DOGE 按链参数化地址派生与反向解析自洽：
//   - LTC 支持 segwit(ltc1) 与 P2PKH(L/M…)；
//   - DOGE 无原生 segwit，即使 preferSegwit=true 也强制 P2PKH(D…)。
func TestDepositAddressLTCAndDOGE(t *testing.T) {
	pub := btcTestKey(t).PubKey()
	comp := pub.SerializeCompressed()
	h160 := hash160(comp)

	// LTC 默认 segwit (ltc1)
	ltc1 := deriveUTXOAddress(pub, ChainLTC, true)
	if !strings.HasPrefix(ltc1, "ltc1") {
		t.Fatalf("LTC segwit should start ltc1, got %q", ltc1)
	}
	if spk, err := addressToScriptPubKeyFor(ltc1, 0x30, []string{"ltc"}); err != nil || !bytes.Equal(spk, p2wpkhScript(h160)) {
		t.Fatalf("LTC segwit round-trip failed: spk=%v err=%v", spk, err)
	}

	// LTC P2PKH (L/M…)
	ltcP := deriveUTXOAddress(pub, ChainLTC, false)
	if ltcP[0] != 'L' && ltcP[0] != 'M' {
		t.Fatalf("LTC p2pkh should start L/M, got %q", ltcP)
	}
	if spk, err := addressToScriptPubKeyFor(ltcP, 0x30, []string{"ltc"}); err != nil || !bytes.Equal(spk, p2pkhScript(h160)) {
		t.Fatalf("LTC p2pkh round-trip failed: spk=%v err=%v", spk, err)
	}

	// DOGE 无 segwit：即使 preferSegwit=true 也强制 P2PKH (D…)
	doge := deriveUTXOAddress(pub, ChainDOGE, true)
	if doge[0] != 'D' {
		t.Fatalf("DOGE should start D, got %q", doge)
	}
	if spk, err := addressToScriptPubKeyFor(doge, 0x1E, nil); err != nil || !bytes.Equal(spk, p2pkhScript(h160)) {
		t.Fatalf("DOGE round-trip failed: spk=%v err=%v", spk, err)
	}
}

// TestSignUTXOChains 验证 LTC（segwit/P2PKH）与 DOGE（P2PKH）离线签名：金额守恒 +
// 签名自校验通过 + 收款地址按链参数化前缀正确。复用 BTC 的独立解析/校验辅助。
func TestSignUTXOChains(t *testing.T) {
	s, err := newRealSigner(HotWalletConfig{SignerKey: btcTestPriv})
	if err != nil {
		t.Fatalf("newRealSigner: %v", err)
	}
	pub := s.key.Public()
	utxos := btcTestUTXOs(t, pub)
	cases := []struct {
		name     string
		chain    Chain
		segwit   bool
		prefix   string
		isSegwit bool
	}{
		{"LTC-segwit", ChainLTC, true, "ltc1", true},
		{"LTC-p2pkh", ChainLTC, false, "L", false},
		{"DOGE-p2pkh", ChainDOGE, true, "D", false}, // DOGE 强制 p2pkh
	}
	for _, c := range cases {
		tx := &UnsignedTx{
			Chain:        c.chain,
			To:           deriveUTXOAddress(pub, c.chain, c.segwit),
			Amount:       amt(c.chain, 0.6),
			Asset:        string(c.chain),
			UTXOs:        utxos,
			FeeRatePerKB: 1000,
		}
		raw, err := s.Sign(context.Background(), tx)
		if err != nil {
			t.Fatalf("%s sign: %v", c.name, err)
		}
		// 收款地址前缀按链参数化。
		okPrefix := strings.HasPrefix(tx.To, c.prefix) ||
			(c.chain == ChainLTC && !c.isSegwit && strings.HasPrefix(tx.To, "M"))
		if !okPrefix {
			t.Fatalf("%s recipient prefix got %q want %q", c.name, tx.To, c.prefix)
		}
		verifyBTCSignatures(t, raw, utxos, pub.SerializeCompressed())
		verifyBTCValueConservation(t, raw, utxos, amt(c.chain, 0.6))
		if c.isSegwit {
			b, _ := hex.DecodeString(raw)
			if !(b[4] == 0x00 && b[5] == 0x01) {
				t.Fatalf("%s expected segwit marker 0x0001 after version", c.name)
			}
		}
	}
}

// TestRealSignerExternalLTCAndDOGE 验证 LTC/DOGE 路径在 external 后端（HSM/KMS 缝）下同样工作：
// 签名经外部 KeySigner 完成，复用独立解析 + ecdsa.Verify + 金额守恒校验。
func TestRealSignerExternalLTCAndDOGE(t *testing.T) {
	priv, err := parseSignerKey(btcTestPriv)
	if err != nil {
		t.Fatalf("parseSignerKey: %v", err)
	}
	keyID := "kms-utxo-1"
	backend := NewExternalKeySigner(priv.PubKey(), func(ctx context.Context, d [32]byte) (*big.Int, *big.Int, error) {
		c := ecdsa.SignCompact(priv, d[:], false)
		return new(big.Int).SetBytes(c[1:33]), new(big.Int).SetBytes(c[33:65]), nil
	})
	RegisterExternalSigner(keyID, backend)
	defer UnregisterExternalSigner(keyID)

	s, err := newRealSignerWithSource(HotWalletConfig{SignerKey: keyID, SignerBackend: "external"}, SignerSources{})
	if err != nil {
		t.Fatalf("newRealSignerWithSource: %v", err)
	}
	pub := s.key.Public()
	utxos := btcTestUTXOs(t, pub)
	for _, chain := range []Chain{ChainLTC, ChainDOGE} {
		tx := &UnsignedTx{Chain: chain, To: deriveUTXOAddress(pub, chain, true), Amount: amt(chain, 0.6), Asset: string(chain), UTXOs: utxos, FeeRatePerKB: 1000}
		raw, err := s.Sign(context.Background(), tx)
		if err != nil {
			t.Fatalf("%s sign: %v", chain, err)
		}
		verifyBTCSignatures(t, raw, utxos, pub.SerializeCompressed())
		verifyBTCValueConservation(t, raw, utxos, amt(chain, 0.6))
	}
}

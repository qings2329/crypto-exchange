package settlement

import (
	"bytes"
	"context"
	"encoding/hex"
	"strconv"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// 测试用固定私钥（32 字节全 0x46，合法 secp256k1 私钥），便于确定性输出与地址派生验证。
const btcTestPriv = "4646464646464646464646464646464646464646464646464646464646464646"

func btcTestKey(t *testing.T) *secp256k1.PrivateKey {
	k, err := hex.DecodeString(btcTestPriv)
	if err != nil {
		t.Fatalf("decode priv: %v", err)
	}
	return secp256k1.PrivKeyFromBytes(k)
}

// TestRIPEMD160Vectors 用官方 RIPEMD-160 测试向量验证自实现哈希（HASH160 依赖它）。
// 覆盖空串、短串（单块）与 56/80 字节（多块）路径。
func TestRIPEMD160Vectors(t *testing.T) {
	long56 := "abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq"
	long80 := "12345678901234567890123456789012345678901234567890123456789012345678901234567890"
	vectors := map[string]string{
		"":                           "9c1185a5c5e9fc54612808977ee8f548b2258d31",
		"a":                          "0bdc9d2d256b3ee9daae347be6f4dc835a467ffe",
		"abc":                        "8eb208f7e05d987a9b044a8e98c6b087f15a0bfc",
		"message digest":             "5d0689ef49d2fae572b881b123a85ffa21595f36",
		"abcdefghijklmnopqrstuvwxyz": "f71c27109c692c1b56bbdceb5b9d2865b3708dbc",
		long56:                       "12a053384a9c0c88e405a06c27dcf49ada62eb2b",
		"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789": "b0e20b6e3116640286ed3a87a5713079b21f5189",
		long80: "9b752e45573d4b39f4dbd3323cab82bf63326bfb",
	}
	for in, want := range vectors {
		got := hex.EncodeToString(ripemd160Sum([]byte(in)))
		if got != want {
			t.Errorf("RIPEMD-160(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestBTCAddressDerivation 验证 P2PKH/P2WPKH 地址派生与反向解析自洽（HASH160 一致）。
func TestBTCAddressDerivation(t *testing.T) {
	pub := btcTestKey(t).PubKey()
	comp := pub.SerializeCompressed()
	h160 := hash160(comp)

	p2pkh := deriveP2PKHAddress(pub)
	if p2pkh[0] != '1' {
		t.Fatalf("P2PKH address should start with '1', got %q", p2pkh)
	}
	spk1, err := addressToScriptPubKey(p2pkh)
	if err != nil {
		t.Fatalf("parse P2PKH addr: %v", err)
	}
	if !bytes.Equal(spk1, p2pkhScript(h160)) {
		t.Fatalf("P2PKH round-trip script mismatch")
	}

	p2wpkh := deriveP2WPKHAddress(pub)
	if len(p2wpkh) < 3 || p2wpkh[:3] != "bc1" {
		t.Fatalf("P2WPKH address should start with 'bc1', got %q", p2wpkh)
	}
	spk2, err := addressToScriptPubKey(p2wpkh)
	if err != nil {
		t.Fatalf("parse P2WPKH addr: %v", err)
	}
	if !bytes.Equal(spk2, p2wpkhScript(h160)) {
		t.Fatalf("P2WPKH round-trip script mismatch")
	}
}

// 构造测试 UTXO 集合：含 P2PKH 与 P2WPKH，scriptPubKey 由测试私钥派生（可真实花费）。
func btcTestUTXOs(t *testing.T, pub *secp256k1.PublicKey) []UTXO {
	comp := pub.SerializeCompressed()
	h160 := hash160(comp)
	return []UTXO{
		{TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Vout: 0, Amount: 0.5, ScriptPubKey: hex.EncodeToString(p2pkhScript(h160))},
		{TxID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Vout: 1, Amount: 0.3, ScriptPubKey: hex.EncodeToString(p2wpkhScript(h160))},
		{TxID: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", Vout: 2, Amount: 0.2, ScriptPubKey: hex.EncodeToString(p2pkhScript(h160))},
	}
}

// TestSignBTCP2PKHSelection 验证 P2PKH UTXO 选择 + 找零 + 签名自洽（独立解析 + 独立摘要交叉验证）。
func TestSignBTCP2PKHSelection(t *testing.T) {
	s, err := newRealSigner(HotWalletConfig{SignerKey: btcTestPriv})
	if err != nil {
		t.Fatalf("newRealSigner: %v", err)
	}
	pub := s.priv.PubKey()
	recipient := deriveP2PKHAddress(pub) // 收款至自身 P2PKH

	// 仅给 P2PKH 类型的 UTXO（把第 2 个换成 P2PKH）。
	utxos := btcTestUTXOs(t, pub)
	utxos[1].ScriptPubKey = hex.EncodeToString(p2pkhScript(hash160(pub.SerializeCompressed())))

	tx := &UnsignedTx{
		Chain:        ChainBTC,
		To:           recipient,
		Amount:       0.6,
		Asset:        "BTC",
		UTXOs:        utxos,
		FeeRatePerKB: 1000,
	}
	raw, err := s.Sign(context.Background(), tx)
	if err != nil {
		t.Fatalf("signBTC: %v", err)
	}
	// 确定性：同一输入应产生同一 raw hex。
	raw2, err := s.Sign(context.Background(), tx)
	if err != nil {
		t.Fatalf("signBTC again: %v", err)
	}
	if raw != raw2 {
		t.Fatalf("BTC signing not deterministic:\n %s\n %s", raw, raw2)
	}
	verifyBTCSignatures(t, raw, utxos, pub.SerializeCompressed())
	verifyBTCValueConservation(t, raw, utxos, 0.6)
}

// TestSignBTCP2WPKHSelection 验证 P2WPKH（segwit）UTXO 选择 + 找零 + witness 签名自洽。
func TestSignBTCP2WPKHSelection(t *testing.T) {
	s, err := newRealSigner(HotWalletConfig{SignerKey: btcTestPriv})
	if err != nil {
		t.Fatalf("newRealSigner: %v", err)
	}
	pub := s.priv.PubKey()
	utxos := btcTestUTXOs(t, pub) // 含一个 P2WPKH（bbbb… vout1）

	tx := &UnsignedTx{
		Chain:        ChainBTC,
		To:           deriveP2WPKHAddress(pub),
		Amount:       0.6,
		Asset:        "BTC",
		UTXOs:        utxos,
		FeeRatePerKB: 1000,
	}
	raw, err := s.Sign(context.Background(), tx)
	if err != nil {
		t.Fatalf("signBTC: %v", err)
	}
	verifyBTCSignatures(t, raw, utxos, pub.SerializeCompressed())
	verifyBTCValueConservation(t, raw, utxos, 0.6)
	// segwit 交易应有标记字节（version 后紧跟 0x00 0x01）。
	b, _ := hex.DecodeString(raw)
	if !(b[4] == 0x00 && b[5] == 0x01) {
		t.Fatalf("expected segwit marker 0x0001 after version, got %x %x", b[4], b[5])
	}
}

// TestSignBTCInsufficient 验证余额不足（含手续费）时报错，不签名。
func TestSignBTCInsufficient(t *testing.T) {
	s, _ := newRealSigner(HotWalletConfig{SignerKey: btcTestPriv})
	pub := s.priv.PubKey()
	utxos := []UTXO{
		{TxID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Vout: 0, Amount: 0.1, ScriptPubKey: hex.EncodeToString(p2pkhScript(hash160(pub.SerializeCompressed())))},
	}
	tx := &UnsignedTx{Chain: ChainBTC, To: deriveP2PKHAddress(pub), Amount: 1.0, Asset: "BTC", UTXOs: utxos, FeeRatePerKB: 1000}
	if _, err := s.Sign(context.Background(), tx); err == nil {
		t.Fatalf("expected insufficient funds error")
	}
}

// TestSignBTCNoUTXOs 验证无 UTXO 且无 UTXO 源时报错（网关回退节点侧签名广播）。
func TestSignBTCNoUTXOs(t *testing.T) {
	s, _ := newRealSigner(HotWalletConfig{SignerKey: btcTestPriv})
	tx := &UnsignedTx{Chain: ChainBTC, To: deriveP2PKHAddress(s.priv.PubKey()), Amount: 0.1, Asset: "BTC"}
	if _, err := s.Sign(context.Background(), tx); err == nil {
		t.Fatalf("expected error when no UTXOs available")
	}
}

// ---------- 独立验证辅助 ----------

// verifyBTCSignatures 解析 raw tx，按 UTXO 集重建构建器，独立重算每个输入的 SIGHASH 摘要，
// 与签名器 digest 交叉比对，并用 ecdsa 校验签名（捕获序列化 / R-S / 摘要公式错误）。
func verifyBTCSignatures(t *testing.T, rawHex string, utxos []UTXO, compPub []byte) {
	t.Helper()
	p, err := parseBTCRaw(rawHex)
	if err != nil {
		t.Fatalf("parse raw: %v", err)
	}
	umap := map[string]UTXO{}
	for _, u := range utxos {
		umap[u.TxID+":"+strconv.Itoa(int(u.Vout))] = u
	}
	b := &btcTxBuilder{version: p.version, locktime: p.locktime, isSegwit: p.isSegwit}
	for _, in := range p.inputs {
		u, ok := umap[in.txid+":"+strconv.Itoa(int(in.vout))]
		if !ok {
			t.Fatalf("input %s:%d not in UTXO set", in.txid, in.vout)
		}
		spk, err := hex.DecodeString(u.ScriptPubKey)
		if err != nil {
			t.Fatalf("decode scriptPubKey: %v", err)
		}
		b.inputs = append(b.inputs, btcTxIn{
			txidLE:   reverseBytes(mustHex(in.txid)),
			vout:     in.vout,
			value:    btcToSatoshi(u.Amount),
			scriptPK: spk,
			sequence: in.sequence,
		})
	}
	for _, o := range p.outputs {
		b.outputs = append(b.outputs, btcTxOut{value: o.value, scriptPK: o.scriptPK})
	}

	pub, _ := secp256k1.ParsePubKey(compPub)
	for i, in := range p.inputs {
		digest := btcSighashIndependent(b.version, b.inputs, b.outputs, b.locktime, i, compPub, isP2WPKH(b.inputs[i].scriptPK))
		// 与签名器生产代码产出的 digest 交叉比对（捕获生产代码 typo）。
		if !bytes.Equal(digest, b.sighashDigest(i, compPub)) {
			t.Fatalf("input %d: independent digest != production digest", i)
		}
		var sig []byte
		if isP2WPKH(b.inputs[i].scriptPK) {
			sig = in.witness[0][:len(in.witness[0])-1] // 去掉末尾 SIGHASH_ALL
		} else {
			slen := int(in.sig[0])
			sig = in.sig[1 : 1+slen][:slen-1] // 去末尾 SIGHASH_ALL
		}
		parsed, err := ecdsa.ParseDERSignature(sig)
		if err != nil {
			t.Fatalf("input %d parse DER: %v", i, err)
		}
		if !parsed.Verify(digest, pub) {
			t.Fatalf("input %d signature does NOT verify", i)
		}
	}
}

// verifyBTCValueConservation 校验（实际花费的）inputs = 收款 + 找零 + 手续费（金额守恒）。
func verifyBTCValueConservation(t *testing.T, rawHex string, utxos []UTXO, targetBTC float64) {
	t.Helper()
	p, err := parseBTCRaw(rawHex)
	if err != nil {
		t.Fatalf("parse raw: %v", err)
	}
	umap := map[string]UTXO{}
	for _, u := range utxos {
		umap[u.TxID+":"+strconv.Itoa(int(u.Vout))] = u
	}
	var inSum int64
	for _, in := range p.inputs {
		u, ok := umap[in.txid+":"+strconv.Itoa(int(in.vout))]
		if !ok {
			t.Fatalf("input %s:%d not in UTXO set", in.txid, in.vout)
		}
		inSum += btcToSatoshi(u.Amount)
	}
	var outSum int64
	for _, o := range p.outputs {
		outSum += o.value
	}
	// 金额守恒：inputs = outputs + 手续费（差值即手续费，应为较小的正值）。
	fee := inSum - outSum
	if fee < 0 || fee >= 100000 {
		t.Fatalf("value not conserved: inputs=%d sat outputs=%d sat fee=%d", inSum, outSum, fee)
	}
	// 必含收款输出（金额 ≈ target）。
	var foundRecv bool
	targetSat := btcToSatoshi(targetBTC)
	for _, o := range p.outputs {
		if o.value == targetSat {
			foundRecv = true
		}
	}
	if !foundRecv {
		t.Fatalf("recipient output %d sat not found in outputs %v", targetSat, p.outputs)
	}
}

// btcSighashIndependent 是独立实现的 SIGHASH 摘要（与生产代码分开，用于交叉验证）。
// legacy(P2PKH) 与 segwit(P2WPKH/BIP143) 仅在是否包含输入 value 字段上不同，末态均为
// double-SHA256。
func btcSighashIndependent(version uint32, inputs []btcTxIn, outputs []btcTxOut, locktime uint32, i int, compPub []byte, segwit bool) []byte {
	sc := scriptCodeFor(compPub)
	var prevOuts, sequences, outBytes []byte
	for _, in := range inputs {
		prevOuts = append(prevOuts, in.txidLE...)
		prevOuts = append(prevOuts, le32(in.vout)...)
		sequences = append(sequences, le32(in.sequence)...)
	}
	for _, o := range outputs {
		outBytes = append(outBytes, le64i(o.value)...)
		outBytes = append(outBytes, varInt(len(o.scriptPK))...)
		outBytes = append(outBytes, o.scriptPK...)
	}
	var data []byte
	data = append(data, le32(version)...)
	data = append(data, doubleSHA256(prevOuts)...)
	data = append(data, doubleSHA256(sequences)...)
	data = append(data, inputs[i].txidLE...)
	data = append(data, le32(inputs[i].vout)...)
	data = append(data, varInt(len(sc))...)
	data = append(data, sc...)
	if segwit {
		data = append(data, le64i(inputs[i].value)...)
	}
	data = append(data, le32(inputs[i].sequence)...)
	data = append(data, doubleSHA256(outBytes)...)
	data = append(data, le32(locktime)...)
	data = append(data, le32(1)...) // SIGHASH_ALL
	return doubleSHA256(data)
}

// parsedBTC 是从原始交易字节反解析出的结构（仅用于测试验证）。
type parsedBTC struct {
	version  uint32
	locktime uint32
	isSegwit bool
	inputs   []parsedBTCIn
	outputs  []parsedBTCOut
}

type parsedBTCIn struct {
	txid     string
	vout     uint32
	sequence uint32
	sig      []byte
	witness  [][]byte
}
type parsedBTCOut struct {
	value    int64
	scriptPK []byte
}

// parseBTCRaw 反解析 BTC 原始交易（支持 segwit 与 P2PKH/P2WPKH 输入）。仅测试用。
func parseBTCRaw(rawHex string) (*parsedBTC, error) {
	b, err := hex.DecodeString(rawHex)
	if err != nil {
		return nil, err
	}
	p := &parsedBTC{}
	pos := 0
	p.version = uint32(b[pos]) | uint32(b[pos+1])<<8 | uint32(b[pos+2])<<16 | uint32(b[pos+3])<<24
	pos += 4
	if pos+2 <= len(b) && b[pos] == 0x00 && b[pos+1] == 0x01 {
		p.isSegwit = true
		pos += 2
	}
	readVarInt := func() (int, int) {
		v := b[pos]
		if v < 0xfd {
			return int(v), 1
		}
		if v == 0xfd {
			n := uint(b[pos+1]) | uint(b[pos+2])<<8
			return int(n), 3
		}
		if v == 0xfe {
			n := uint32(b[pos+1]) | uint32(b[pos+2])<<8 | uint32(b[pos+3])<<16 | uint32(b[pos+4])<<24
			return int(n), 5
		}
		n := uint64(b[pos+1]) | uint64(b[pos+2])<<8 | uint64(b[pos+3])<<16 | uint64(b[pos+4])<<24 |
			uint64(b[pos+5])<<32 | uint64(b[pos+6])<<40 | uint64(b[pos+7])<<48 | uint64(b[pos+8])<<56
		return int(n), 9
	}
	incount, n := readVarInt()
	pos += n
	for k := 0; k < incount; k++ {
		txidLE := b[pos : pos+32]
		txid := hex.EncodeToString(reverseBytes(append([]byte{}, txidLE...)))
		pos += 32
		vout := uint32(b[pos]) | uint32(b[pos+1])<<8 | uint32(b[pos+2])<<16 | uint32(b[pos+3])<<24
		pos += 4
		slen, sn := readVarInt()
		pos += sn
		sig := append([]byte{}, b[pos:pos+slen]...)
		pos += slen
		seq := uint32(b[pos]) | uint32(b[pos+1])<<8 | uint32(b[pos+2])<<16 | uint32(b[pos+3])<<24
		pos += 4
		p.inputs = append(p.inputs, parsedBTCIn{txid: txid, vout: vout, sequence: seq, sig: sig})
	}
	outcount, n := readVarInt()
	pos += n
	for k := 0; k < outcount; k++ {
		var value int64
		for j := 0; j < 8; j++ {
			value |= int64(b[pos+j]) << (8 * j)
		}
		pos += 8
		slen, sn := readVarInt()
		pos += sn
		spk := append([]byte{}, b[pos:pos+slen]...)
		pos += slen
		p.outputs = append(p.outputs, parsedBTCOut{value: value, scriptPK: spk})
	}
	if p.isSegwit {
		for k := range p.inputs {
			wcount, wn := readVarInt()
			pos += wn
			var wit [][]byte
			for j := 0; j < wcount; j++ {
				ilen, iln := readVarInt()
				pos += iln
				wit = append(wit, append([]byte{}, b[pos:pos+ilen]...))
				pos += ilen
			}
			p.inputs[k].witness = wit
		}
	}
	p.locktime = uint32(b[pos]) | uint32(b[pos+1])<<8 | uint32(b[pos+2])<<16 | uint32(b[pos+3])<<24
	return p, nil
}

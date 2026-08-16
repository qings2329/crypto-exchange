package settlement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// ----------------------------------------------------------------------------
// BTC 离线签名：UTXO 选择 + 找零 + SIGHASH_ALL 签名 + 交易序列化
//
// 支撑两种输出脚本（同一 secp256k1 公钥可同时派生）：
//   - P2PKH：base58check 地址（version 0x00），legacy 输入，scriptSig = <sig> <pubkey>。
//   - P2WPKH：bech32 地址（witness v0），segwit 输入，witness = <pubkey> <sig>。
//
// 输入类型由其 scriptPubKey 决定，逐输入用对应方式做 SIGHASH_ALL 摘要与签名；
// 找零默认回自身的 P2WPKH（segwit，手续费更低），可由 UnsignedTx.ChangeAddress 覆盖。
// 全部使用标准库哈希 + decred secp256k1，私钥不出进程（安全域）。
// ----------------------------------------------------------------------------

// UTXO 是一笔未花费输出（BTC 真实签名输入）。Amount 以 BTC 计；ScriptPubKey 为 hex。
type UTXO struct {
	TxID         string  `yaml:"txid" json:"txid"`
	Vout         uint32  `yaml:"vout" json:"vout"`
	Amount       float64 `yaml:"amount" json:"amount"`
	ScriptPubKey string  `yaml:"script_pubkey" json:"script_pubkey"`
}

// UTXOSource 按地址查询未花费输出；为空时签名器使用 UnsignedTx.UTXOs 内联提供。
type UTXOSource interface {
	ListUTXOs(ctx context.Context, addr string) ([]UTXO, error)
}

// rpcUTXOSource 用 JSON-RPC listunspent 查询 UTXO（真实节点路径）。节点不可达时返回错误，
// 由调用方回退节点侧签名广播（fail-degraded）。
type rpcUTXOSource struct {
	client *JSONRPCClient
}

// NewRPCUTXOSource 由通用 JSON-RPC 客户端构造 UTXO 源。
func NewRPCUTXOSource(client *JSONRPCClient) UTXOSource {
	return &rpcUTXOSource{client: client}
}

func (r *rpcUTXOSource) ListUTXOs(ctx context.Context, addr string) ([]UTXO, error) {
	res, err := r.client.Call(ctx, ChainBTC, "listunspent", []interface{}{0, 9999999, []string{addr}})
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Txid         string  `json:"txid"`
		Vout         uint32  `json:"vout"`
		Amount       float64 `json:"amount"`
		ScriptPubKey string  `json:"scriptPubKey"`
	}
	if err := json.Unmarshal(res, &raw); err != nil {
		return nil, err
	}
	out := make([]UTXO, 0, len(raw))
	for _, u := range raw {
		out = append(out, UTXO{TxID: u.Txid, Vout: u.Vout, Amount: u.Amount, ScriptPubKey: u.ScriptPubKey})
	}
	return out, nil
}

// ---------- 哈希 / 编码原语 ----------

// hash160 = RIPEMD160(SHA256(x))，BTC 地址与脚本的核心。
func hash160(b []byte) []byte {
	h := sha256.Sum256(b)
	return ripemd160Sum(h[:])
}

// doubleSHA256 = SHA256(SHA256(x))，用于 txid / SIGHASH 摘要（legacy）/ 校验和。
func doubleSHA256(b []byte) []byte {
	h := sha256.Sum256(b)
	h2 := sha256.Sum256(h[:])
	return h2[:]
}

// btcToSatoshi 把以 BTC 为单位的金额转为 satoshi（四舍五入，避免浮点误差累积）。
func btcToSatoshi(amount float64) int64 {
	return int64(math.Round(amount * 1e8))
}

// le32 / le64 小端编码。
func le32(v uint32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }
func le64(v uint64) []byte {
	b := make([]byte, 8)
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
	return b
}
func le64i(v int64) []byte { return le64(uint64(v)) }

// reverseBytes 翻转字节序（txid 在 human 表示与序列化间转换）。
func reverseBytes(b []byte) []byte {
	out := make([]byte, len(b))
	for i := range b {
		out[len(b)-1-i] = b[i]
	}
	return out
}

// varInt 编码正整数（BTC 交易里 input/output 计数用；本实现覆盖 < 0xfd 的常规情形，
// 并对更大值做完整 varint 编码以防万一）。
func varInt(n int) []byte {
	if n < 0xfd {
		return []byte{byte(n)}
	}
	if n <= 0xffff {
		return append([]byte{0xfd}, le32(uint32(n))[:2]...)
	}
	if n <= 0xffffffff {
		return append([]byte{0xfe}, le32(uint32(n))...)
	}
	return append([]byte{0xff}, le64(uint64(n))...)
}

// derEncode 把 ECDSA 的 (r, s) 编码为 DER（含低 S 规范化后的 S），供 BTC OP_CHECKSIG 校验。
func derEncode(r, s *big.Int) []byte {
	rb := r.Bytes()
	sb := s.Bytes()
	if rb[0]&0x80 != 0 {
		rb = append([]byte{0x00}, rb...)
	}
	if sb[0]&0x80 != 0 {
		sb = append([]byte{0x00}, sb...)
	}
	body := append(append([]byte{0x02, byte(len(rb))}, rb...), append([]byte{0x02, byte(len(sb))}, sb...)...)
	return append([]byte{0x30, byte(len(body))}, body...)
}

// ---------- base58check（P2PKH 地址） ----------

const b58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func base58Encode(input []byte) string {
	zeros := 0
	for zeros < len(input) && input[zeros] == 0 {
		zeros++
	}
	digits := []byte{}
	for i := zeros; i < len(input); i++ {
		carry := int(input[i])
		for j := len(digits) - 1; j >= 0; j-- {
			carry += 256 * int(digits[j])
			digits[j] = byte(carry % 58)
			carry /= 58
		}
		for carry > 0 {
			digits = append([]byte{byte(carry % 58)}, digits...)
			carry /= 58
		}
	}
	var out []byte
	for i := 0; i < zeros; i++ {
		out = append(out, '1')
	}
	for _, d := range digits {
		out = append(out, b58Alphabet[d])
	}
	return string(out)
}

func base58Decode(s string) ([]byte, error) {
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	var buf []byte
	for i := zeros; i < len(s); i++ {
		idx := strings.IndexByte(b58Alphabet, s[i])
		if idx < 0 {
			return nil, fmt.Errorf("invalid base58 char %q", s[i])
		}
		carry := idx
		for j := len(buf) - 1; j >= 0; j-- {
			carry += 58 * int(buf[j])
			buf[j] = byte(carry % 256)
			carry /= 256
		}
		for carry > 0 {
			buf = append([]byte{byte(carry % 256)}, buf...)
			carry /= 256
		}
	}
	out := make([]byte, zeros)
	out = append(out, buf...)
	return out, nil
}

func base58CheckEncode(payload []byte) string {
	cs := doubleSHA256(payload)[:4]
	return base58Encode(append(append([]byte{}, payload...), cs...))
}

func base58CheckDecode(s string) ([]byte, error) {
	b, err := base58Decode(s)
	if err != nil {
		return nil, err
	}
	if len(b) < 4 {
		return nil, errors.New("base58check too short")
	}
	payload, cs := b[:len(b)-4], b[len(b)-4:]
	if !equalBytes(doubleSHA256(payload)[:4], cs) {
		return nil, errors.New("base58check checksum mismatch")
	}
	return payload, nil
}

// ---------- bech32（P2WPKH 地址，BIP173） ----------

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func bech32Polymod(values []byte) uint32 {
	gen := []uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	c := uint32(1)
	for _, v := range values {
		c0 := c >> 25
		c = ((c & 0x1ffffff) << 5) ^ uint32(v)
		for i := 0; i < 5; i++ {
			if gen[i]&(c0<<i) != 0 {
				c ^= gen[i]
			}
		}
	}
	return c
}

func bech32HRPExpand(hrp string) []byte {
	out := make([]byte, 0, len(hrp)*2+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, byte(hrp[i]>>5))
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, byte(hrp[i]&0x1f))
	}
	return out
}

func bech32CreateChecksum(hrp string, data []byte) []byte {
	vals := append(bech32HRPExpand(hrp), data...)
	vals = append(vals, 0, 0, 0, 0, 0, 0)
	polymod := bech32Polymod(vals) ^ 1
	out := make([]byte, 6)
	for i := 0; i < 6; i++ {
		out[i] = byte((polymod >> (5 * (5 - i))) & 0x1f)
	}
	return out
}

func bech32VerifyChecksum(hrp string, data []byte) bool {
	return bech32Polymod(append(bech32HRPExpand(hrp), data...)) == 1
}

func bech32Encode(hrp string, data []byte) string {
	cs := bech32CreateChecksum(hrp, data)
	all := append(append([]byte{}, data...), cs...)
	chars := make([]byte, len(all))
	for i, v := range all {
		chars[i] = bech32Charset[v]
	}
	return hrp + "1" + string(chars)
}

func bech32Decode(s string) (string, []byte, error) {
	s = strings.ToLower(s)
	pos := strings.LastIndexByte(s, '1')
	if pos < 1 || pos+7 > len(s) {
		return "", nil, errors.New("invalid bech32")
	}
	hrp := s[:pos]
	data := make([]byte, 0, len(s)-pos-1)
	for _, c := range s[pos+1:] {
		idx := strings.IndexByte(bech32Charset, byte(c))
		if idx < 0 {
			return "", nil, errors.New("invalid bech32 char")
		}
		data = append(data, byte(idx))
	}
	if !bech32VerifyChecksum(hrp, data) {
		return "", nil, errors.New("bech32 checksum mismatch")
	}
	return hrp, data[:len(data)-6], nil
}

func bech32ConvertBits(data []byte, fromBits, toBits uint, pad bool) ([]byte, error) {
	acc := uint(0)
	bits := uint(0)
	maxv := uint((1 << toBits) - 1)
	var out []byte
	for _, b := range data {
		acc = (acc << fromBits) | uint(b)
		bits += fromBits
		for bits >= toBits {
			bits -= toBits
			out = append(out, byte((acc>>bits)&maxv))
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte((acc<<(toBits-bits))&maxv))
		}
		return out, nil
	}
	if bits >= fromBits || ((acc<<(toBits-bits))&maxv) != 0 {
		return nil, errors.New("invalid bech32 padding")
	}
	return out, nil
}

// ---------- 脚本 / 地址派生 ----------

const (
	btcPubKeyHashVersion = 0x00 // 主网 P2PKH
	btcDustSatoshi       = 546  // 低于此值的找零并入手续费（视为 dust）
)

// p2pkhScript 由 HASH160 公钥生成 P2PKH 锁定脚本：OP_DUP OP_HASH160 <20> OP_EQUALVERIFY OP_CHECKSIG。
func p2pkhScript(h160 []byte) []byte {
	return append([]byte{0x76, 0xa9, 0x14}, append(append([]byte{}, h160...), 0x88, 0xac)...)
}

// p2wpkhScript 由 HASH160 公钥生成 P2WPKH 锁定脚本：OP_0 <20>。
func p2wpkhScript(h160 []byte) []byte {
	return append([]byte{0x00, 0x14}, append([]byte{}, h160...)...)
}

// deriveP2PKHAddress / deriveP2WPKHAddress 由公钥派生对应地址（仅用于日志/ChangeAddress 默认）。
func deriveP2PKHAddress(pub *secp256k1.PublicKey) string {
	h160 := hash160(pub.SerializeCompressed())
	return base58CheckEncode(append([]byte{btcPubKeyHashVersion}, h160...))
}

func deriveP2WPKHAddress(pub *secp256k1.PublicKey) string {
	h160 := hash160(pub.SerializeCompressed())
	data, _ := bech32ConvertBits(h160, 8, 5, true)
	return bech32Encode("bc", append([]byte{0x00}, data...))
}

// isP2WPKH / isP2PKH 判定 scriptPubKey 类型。
func isP2WPKH(script []byte) bool {
	return len(script) == 22 && script[0] == 0x00 && script[1] == 0x14
}
func isP2PKH(script []byte) bool {
	return len(script) == 25 && script[0] == 0x76 && script[1] == 0xa9 && script[2] == 0x14 &&
		script[23] == 0x88 && script[24] == 0xac
}

// scriptCodeFor 返回签名用的 scriptCode：P2PKH/P2WPKH 均为标准 P2PKH 赎回脚本（BIP143 规定）。
func scriptCodeFor(compPub []byte) []byte {
	return p2pkhScript(hash160(compPub))
}

// addressToScriptPubKey 把 BTC 地址（base58 P2PKH 或 bech32 P2WPKH）解析为锁定脚本。
func addressToScriptPubKey(addr string) ([]byte, error) {
	if strings.HasPrefix(addr, "bc1") || strings.HasPrefix(addr, "tb1") || strings.HasPrefix(addr, "bcrt1") {
		hrp, data, err := bech32Decode(addr)
		if err != nil {
			return nil, err
		}
		if len(data) == 0 {
			return nil, errors.New("empty bech32 witness program")
		}
		version := data[0]
		if version != 0 {
			return nil, fmt.Errorf("unsupported witness version %d", version)
		}
		prog, err := bech32ConvertBits(data[1:], 5, 8, false)
		if err != nil {
			return nil, err
		}
		if len(prog) != 20 {
			return nil, fmt.Errorf("witness program must be 20 bytes for P2WPKH, got %d", len(prog))
		}
		_ = hrp
		return p2wpkhScript(prog), nil
	}
	payload, err := base58CheckDecode(addr)
	if err != nil {
		return nil, err
	}
	if payload[0] != btcPubKeyHashVersion {
		return nil, fmt.Errorf("unsupported base58 version 0x%02x", payload[0])
	}
	return p2pkhScript(payload[1:21]), nil
}

// ---------- 交易构建 / 签名 / 序列化 ----------

type btcTxIn struct {
	txidLE   []byte // 32 字节，小端（序列化中的 outpoint 形式）
	vout     uint32
	value    int64  // satoshi（P2WPKH 摘要与手续费需要）
	scriptPK []byte // 前序输出锁定脚本（判定类型 + scriptCode）
	sequence uint32
	sig      []byte   // P2PKH 的 scriptSig（<sig+sighash> <pubkey>）；P2WPKH 为空
	witness  [][]byte // P2WPKH 的 witness（[sig+sighash, pubkey]）
}

type btcTxOut struct {
	value    int64
	scriptPK []byte
}

type btcTxBuilder struct {
	version  uint32
	locktime uint32
	inputs   []btcTxIn
	outputs  []btcTxOut
	isSegwit bool
}

// weight 计算交易权重（weight units）；vbytes = ceil(weight/4)。
func (b *btcTxBuilder) weight() int {
	base := 4 + len(varInt(len(b.inputs))) + len(varInt(len(b.outputs))) + 4
	if b.isSegwit {
		base += 2 // segwit marker + flag
	}
	witnessWeight := 0
	for _, in := range b.inputs {
		if isP2WPKH(in.scriptPK) {
			base += 36 + 1 + 4 // outpoint + 空 scriptSig 长度字节 + sequence（witness 不计入 base）
			witnessWeight += 1 + (1 + 72) + (1 + 33)
		} else {
			base += 148 // P2PKH 输入（含 scriptSig）
		}
	}
	for _, out := range b.outputs {
		base += 8 + len(varInt(len(out.scriptPK))) + len(out.scriptPK)
	}
	return base*4 + witnessWeight
}

func (b *btcTxBuilder) vbytes() int {
	w := b.weight()
	if w%4 == 0 {
		return w / 4
	}
	return w/4 + 1
}

// estimateFee 按 sat/kvB 估算手续费（向上取整）；0 取默认 1000 sat/kvB。
func (b *btcTxBuilder) estimateFee(ratePerKB uint64) int64 {
	if ratePerKB == 0 {
		ratePerKB = 1000
	}
	vb := uint64(b.vbytes())
	return int64((vb*ratePerKB + 999) / 1000)
}

// hashPrevOuts / hashSequence / hashOutputs 是用于 SIGHASH 摘要的预哈希（整笔交易聚合）。
func (b *btcTxBuilder) hashPrevOuts() []byte {
	var buf []byte
	for _, in := range b.inputs {
		buf = append(buf, in.txidLE...)
		buf = append(buf, le32(in.vout)...)
	}
	return doubleSHA256(buf)
}

func (b *btcTxBuilder) hashSequence() []byte {
	var buf []byte
	for _, in := range b.inputs {
		buf = append(buf, le32(in.sequence)...)
	}
	return doubleSHA256(buf)
}

func (b *btcTxBuilder) hashOutputs() []byte {
	var buf []byte
	for _, out := range b.outputs {
		buf = append(buf, le64i(out.value)...)
		buf = append(buf, varInt(len(out.scriptPK))...)
		buf = append(buf, out.scriptPK...)
	}
	return doubleSHA256(buf)
}

// sighashDigest 计算第 i 个输入用于 ECDSA 的摘要：
//   - P2PKH（legacy）：doubleSHA256(version+hashPrevOuts+hashSequence+outpoint+scriptCode+value+sequence+hashOutputs+locktime+sighashType)。
//   - P2WPKH（BIP143）：SHA256(SHA256(version+hashPrevOuts+hashSequence+outpoint+scriptCode+value+sequence+hashOutputs+locktime+sighashType))。
func (b *btcTxBuilder) sighashDigest(i int, compPub []byte) []byte {
	const sighashAll = 1
	scriptCode := scriptCodeFor(compPub)
	var data []byte
	data = append(data, le32(b.version)...)
	data = append(data, b.hashPrevOuts()...)
	data = append(data, b.hashSequence()...)
	data = append(data, b.inputs[i].txidLE...)
	data = append(data, le32(b.inputs[i].vout)...)
	data = append(data, varInt(len(scriptCode))...)
	data = append(data, scriptCode...)
	if isP2WPKH(b.inputs[i].scriptPK) {
		// segwit（BIP143）在摘要中包含本输入的金额；legacy P2PKH 不含。
		data = append(data, le64i(b.inputs[i].value)...)
	}
	data = append(data, le32(b.inputs[i].sequence)...)
	data = append(data, b.hashOutputs()...)
	data = append(data, le32(b.locktime)...)
	data = append(data, le32(sighashAll)...)
	if isP2WPKH(b.inputs[i].scriptPK) {
		inner := doubleSHA256(data)
		return inner
	}
	return doubleSHA256(data)
}

// serialize 输出最终可广播的原始交易字节（含 segwit marker 与 witness）。
func (b *btcTxBuilder) serialize() []byte {
	var out []byte
	out = append(out, le32(b.version)...)
	if b.isSegwit {
		out = append(out, 0x00, 0x01)
	}
	out = append(out, varInt(len(b.inputs))...)
	for _, in := range b.inputs {
		out = append(out, in.txidLE...)
		out = append(out, le32(in.vout)...)
		out = append(out, varInt(len(in.sig))...)
		out = append(out, in.sig...)
		out = append(out, le32(in.sequence)...)
	}
	out = append(out, varInt(len(b.outputs))...)
	for _, o := range b.outputs {
		out = append(out, le64i(o.value)...)
		out = append(out, varInt(len(o.scriptPK))...)
		out = append(out, o.scriptPK...)
	}
	if b.isSegwit {
		for _, in := range b.inputs {
			out = append(out, varInt(len(in.witness))...)
			for _, w := range in.witness {
				out = append(out, varInt(len(w))...)
				out = append(out, w...)
			}
		}
	}
	out = append(out, le32(b.locktime)...)
	return out
}

// signBTC 构造一笔 BTC 提现：选择 UTXO → 估算手续费 → 生成找零 → 逐输入 SIGHASH_ALL 签名 → 序列化。
func (s *realSigner) signBTC(ctx context.Context, tx *UnsignedTx) (string, error) {
	compPub := s.key.Public().SerializeCompressed()

	// 1) 收集候选 UTXO：优先内联，其次 UTXO 源（按自身地址查询）。
	utxos := tx.UTXOs
	if len(utxos) == 0 && s.utxoSource != nil {
		addr := tx.ChangeAddress
		if addr == "" {
			addr = deriveP2WPKHAddress(s.key.Public())
		}
		src, err := s.utxoSource.ListUTXOs(ctx, addr)
		if err != nil {
			return "", fmt.Errorf("BTC 查询 UTXO 失败（回退节点侧签名）: %w", err)
		}
		utxos = src
	}
	if len(utxos) == 0 {
		return "", fmt.Errorf("BTC 真实签名缺少 UTXO（UnsignedTx.UTXOs 为空且无可用 UTXO 源）")
	}

	targetSat := btcToSatoshi(tx.Amount)
	if targetSat <= 0 {
		return "", fmt.Errorf("BTC 提现金额必须 > 0")
	}

	// 2) 解析收款与找零锁定脚本。
	recipientScript, err := addressToScriptPubKey(tx.To)
	if err != nil {
		return "", fmt.Errorf("BTC 收款地址非法: %w", err)
	}
	changeScript := recipientScript
	if tx.ChangeAddress != "" {
		changeScript, err = addressToScriptPubKey(tx.ChangeAddress)
		if err != nil {
			return "", fmt.Errorf("BTC 找零地址非法: %w", err)
		}
	} else {
		changeScript = p2wpkhScript(hash160(compPub)) // 默认回自身 P2WPKH
	}

	// 3) 解析 UTXO 为内部输入结构，按金额降序（贪心减少输入数 → 省手续费）。
	candidates := make([]btcTxIn, 0, len(utxos))
	for _, u := range utxos {
		spk, err := hex.DecodeString(u.ScriptPubKey)
		if err != nil {
			return "", fmt.Errorf("UTXO %s:%d scriptPubKey 非法: %w", u.TxID, u.Vout, err)
		}
		if !isP2PKH(spk) && !isP2WPKH(spk) {
			return "", fmt.Errorf("UTXO %s:%d 非 P2PKH/P2WPKH 脚本（暂不支持）", u.TxID, u.Vout)
		}
		txidLE, err := hex.DecodeString(u.TxID)
		if err != nil || len(txidLE) != 32 {
			return "", fmt.Errorf("UTXO %s txid 非法（需 32 字节 hex）", u.TxID)
		}
		candidates = append(candidates, btcTxIn{
			txidLE:   reverseBytes(txidLE),
			vout:     u.Vout,
			value:    btcToSatoshi(u.Amount),
			scriptPK: spk,
			sequence: 0xffffffff,
		})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].value > candidates[j].value })

	// 4) 贪心选择：累积到能覆盖 amount + 估算手续费为止（估算时假设存在找零输出，偏保守）。
	var selected []btcTxIn
	var sum int64
	for _, c := range candidates {
		selected = append(selected, c)
		sum += c.value
		if btcCovers(sum, targetSat, selected, recipientScript, changeScript, tx.FeeRatePerKB) {
			break
		}
	}
	if !btcCovers(sum, targetSat, selected, recipientScript, changeScript, tx.FeeRatePerKB) {
		return "", fmt.Errorf("BTC 余额不足：可用 %d sat，需 %d sat + 手续费", sum, targetSat)
	}

	// 5) 计算手续费与找零（找零低于 dust 则并入手续费，不生成找零输出）。
	change, fee := btcComputeChange(sum, targetSat, selected, recipientScript, changeScript, tx.FeeRatePerKB)
	// 金额守恒不变量：inputs = 收款 + 找零 + 手续费。
	if sum != targetSat+change+fee {
		return "", fmt.Errorf("BTC 金额不守恒：inputs=%d recv=%d change=%d fee=%d", sum, targetSat, change, fee)
	}

	// 6) 组装交易并逐输入签名。
	b := &btcTxBuilder{
		version:  1,
		locktime: 0,
		inputs:   selected,
		outputs:  []btcTxOut{{value: targetSat, scriptPK: recipientScript}},
	}
	if change > 0 {
		b.outputs = append(b.outputs, btcTxOut{value: change, scriptPK: changeScript})
	}
	for i := range b.inputs {
		if isP2WPKH(b.inputs[i].scriptPK) {
			b.isSegwit = true
		}
	}
	for i := range b.inputs {
		if err := s.signBTCInput(ctx, b, i, compPub); err != nil {
			return "", err
		}
	}

	raw := b.serialize()
	return hex.EncodeToString(raw), nil
}

// btcCovers 判断是否已选够：inputsTotal >= target + 估算手续费（含找零输出）。
func btcCovers(sum, target int64, selected []btcTxIn, recipient, change []byte, rate uint64) bool {
	if sum < target {
		return false
	}
	b := &btcTxBuilder{
		version:  1,
		isSegwit: isSegwitFrom(selected),
		inputs:   selected,
		outputs: []btcTxOut{
			{value: target, scriptPK: recipient},
			{value: 0, scriptPK: change}, // 占位找零以估算含找零的手续费
		},
	}
	return sum >= target+b.estimateFee(rate)
}

// btcComputeChange 计算最终手续费与找零；找零 < dust 则丢弃找零并重算（仅 1 个输出）。
func btcComputeChange(sum, target int64, selected []btcTxIn, recipient, change []byte, rate uint64) (int64, int64) {
	b := &btcTxBuilder{
		version:  1,
		isSegwit: isSegwitFrom(selected),
		inputs:   selected,
		outputs: []btcTxOut{
			{value: target, scriptPK: recipient},
			{value: 0, scriptPK: change},
		},
	}
	fee := b.estimateFee(rate)
	changeSat := sum - target - fee
	if changeSat < btcDustSatoshi {
		// 找零不足 dust：并入手续费，无找零输出。
		b2 := &btcTxBuilder{
			version:  1,
			isSegwit: isSegwitFrom(selected),
			inputs:   selected,
			outputs:  []btcTxOut{{value: target, scriptPK: recipient}},
		}
		fee = b2.estimateFee(rate)
		return 0, fee
	}
	return changeSat, fee
}

// isSegwitFrom 判断输入集中是否含 P2WPKH（决定 segwit 标记与 witness 重量）。
func isSegwitFrom(inputs []btcTxIn) bool {
	for _, in := range inputs {
		if isP2WPKH(in.scriptPK) {
			return true
		}
	}
	return false
}

// signBTCInput 对第 i 个输入做 secp256k1 签名并填入 scriptSig / witness。
func (s *realSigner) signBTCInput(ctx context.Context, b *btcTxBuilder, i int, compPub []byte) error {
	digest := b.sighashDigest(i, compPub)

	// secp256k1 ECDSA（经 KeySigner：软件或真实 HSM/KMS 后端，私钥不出域）；做低 S 规范化。
	r, ss, err := s.key.SignDigest(ctx, digest32(digest))
	if err != nil {
		return fmt.Errorf("BTC 输入 %d 签名失败: %w", i, err)
	}
	if ss.Cmp(secp256k1HalfN) > 0 {
		ss = new(big.Int).Sub(secp256k1Order, ss)
	}
	der := derEncode(r, ss)
	sigWithHash := append(append([]byte{}, der...), 0x01) // 末尾追加 SIGHASH_ALL

	if isP2WPKH(b.inputs[i].scriptPK) {
		b.inputs[i].witness = [][]byte{sigWithHash, compPub}
	} else {
		// P2PKH scriptSig = <sig+sighash> <pubkey>
		scriptSig := append(append([]byte{byte(len(sigWithHash))}, sigWithHash...), byte(len(compPub)))
		scriptSig = append(scriptSig, compPub...)
		b.inputs[i].sig = scriptSig
	}

	// 自校验：用同一摘要验证签名（捕获序列化/R/S 错误）。
	pub := s.key.Public()
	parsed, err := ecdsa.ParseDERSignature(der)
	if err != nil {
		return fmt.Errorf("BTC 输入 %d DER 解析失败: %w", i, err)
	}
	if !parsed.Verify(digest, pub) {
		return fmt.Errorf("BTC 输入 %d 签名自校验失败", i)
	}
	return nil
}

// ---- 小工具 ----

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

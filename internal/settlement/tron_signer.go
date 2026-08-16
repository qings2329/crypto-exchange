package settlement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// ----------------------------------------------------------------------------
// TRON 离线签名：地址派生 + protobuf raw_data 序列化 + SHA256 摘要 + 可恢复签名
//
// TRON 交易由 Transaction.raw_data（protobuf 序列化）承载业务数据，签名哈希为
// SHA256(raw_data)（即链上展示的 txID）；签名是 65 字节可恢复签名 [recID+27][R(32)][S(32)]，
// 用 secp256k1（与 ETH/BTC 同曲线）对摘要做 ECDSA —— 与 ETH 的 SignCompact 字节布局一致。
//
// 支持两类合约（同一 secp256k1 公钥派生 TRON 地址 0x41||HASH160(compPub)）：
//   - TransferContract（type=1，TRX 原生转账）；
//   - TriggerSmartContract（type=32，TRC20 transfer(address,uint256) 合约调用）。
//
// 私钥不出进程安全域（软件后端）或不出 HSM（外部后端），节点仅负责广播已签交易。
// ----------------------------------------------------------------------------

const (
	tronTransferContractType    = 1
	tronTriggerSmartContractType = 32
	// tronExpirationDeltaMs 是交易过期相对参考区块时间戳的窗口；TronWeb 默认约 60s（脚手架取此值）。
	tronExpirationDeltaMs = 60 * 1000
	// tronAddressPrefix 是 TRON 地址 payload 的版本字节（0x41），其后为 20 字节 HASH160。
	tronAddressPrefix = 0x41
	// tronTRC20TransferSelector 是 ERC20/TRC20 transfer(address,uint256) 的函数选择器。
	tronTRC20TransferSelector = "a9059cbb"
)

// ---------- TRON 地址派生（复用 BTC 的 hash160 / base58check，同曲线、同 RIPEMD160(SHA256)） ----------

// tronAddressBytes 由公钥派生 21 字节 TRON 地址负载：0x41 || HASH160(compressed pubkey)。
// 这是 protobuf 中 owner_address / to_address / contract_address 的二进制形式。
func tronAddressBytes(pub *secp256k1.PublicKey) []byte {
	h := hash160(pub.SerializeCompressed())
	return append([]byte{tronAddressPrefix}, h...)
}

// deriveTronAddress 由公钥派生人类可读的 TRON base58check 地址（用于日志/JSON 展示）。
// 校验和用 doubleSHA256（与 BTC base58check 一致）。
func deriveTronAddress(pub *secp256k1.PublicKey) string {
	return base58CheckEncode(tronAddressBytes(pub))
}

// tronAddressToBytes 把 TRON base58check 地址解析为 21 字节负载（0x41 || HASH160）。
// 错误用于捕获非法/非 TRON 地址（fail-degraded 回退）。
func tronAddressToBytes(addr string) ([]byte, error) {
	payload, err := base58CheckDecode(addr)
	if err != nil {
		return nil, err
	}
	if len(payload) != 21 || payload[0] != tronAddressPrefix {
		return nil, fmt.Errorf("非 TRON 地址（需 0x41 前缀的 21 字节）: %q", addr)
	}
	return payload, nil
}

// ---------- 最小化 protobuf 编码（仅覆盖 TRON raw_data / 合约所需字段） ----------

// pbVarintEnc 编码无符号 varint（含 int64 负值的两补码布局，便于未来扩展）。
func pbVarintEnc(v uint64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if v == 0 {
			break
		}
	}
	return out
}

// pbTag 计算字段头：(field_number << 3) | wire_type。
func pbTag(field, wire int) []byte {
	return pbVarintEnc(uint64(field)<<3 | uint64(wire))
}

// pbBytes 编码 length-delimited 字段（wire=2）：tag + 长度 varint + 内容。
func pbBytes(field int, b []byte) []byte {
	return append(append(pbTag(field, 2), pbVarintEnc(uint64(len(b)))...), b...)
}

// pbVarint 编码 varint 字段（wire=0）：tag + 值 varint。
func pbVarint(field int, v uint64) []byte {
	return append(pbTag(field, 0), pbVarintEnc(v)...)
}

// tronTransferContract 序列化 TransferContract 内部消息（TRX 转账）：
// field1 owner_address(bytes), field2 to_address(bytes), field3 amount(varint, sun=1e6 TRX)。
func tronTransferContract(owner, to []byte, amount int64) []byte {
	var m []byte
	m = append(m, pbBytes(1, owner)...)
	m = append(m, pbBytes(2, to)...)
	m = append(m, pbVarint(3, uint64(amount))...)
	return m
}

// tronTriggerContract 序列化 TriggerSmartContract 内部消息（合约调用）：
// field1 owner_address, field2 contract_address, field3 call_value(varint), field4 data(bytes)。
// TRC20 transfer 时 call_value=0、data=transfer 选择器 + 地址 + 金额。
func tronTriggerContract(owner, contract []byte, callValue int64, data []byte) []byte {
	var m []byte
	m = append(m, pbBytes(1, owner)...)
	m = append(m, pbBytes(2, contract)...)
	m = append(m, pbVarint(3, uint64(callValue))...)
	m = append(m, pbBytes(4, data)...)
	return m
}

// tronAny 把内部合约消息包进 google.protobuf.Any（parameter 字段的载体）：
// field1 type_url(string), field2 value(bytes = 内部消息序列化)。
func tronAny(typeURL string, value []byte) []byte {
	var m []byte
	m = append(m, pbBytes(1, []byte(typeURL))...)
	m = append(m, pbBytes(2, value)...)
	return m
}

// tronContract 序列化 raw_data.contract 元素：
// field1 type(enum varint), field2 parameter(bytes = Any)。
func tronContract(contractType int, param []byte) []byte {
	var m []byte
	m = append(m, pbVarint(1, uint64(contractType))...)
	m = append(m, pbBytes(2, param)...)
	return m
}

// tronRawDataProto 按字段号升序序列化 raw_data（签名哈希源）：
// field1 ref_block_bytes, field4 ref_block_hash, field8 expiration,
// field11 contract, field14 timestamp, field18 fee_limit(可选)。
func tronRawDataProto(refBlockBytes, refBlockHash []byte, expiration, timestamp int64, contractProto []byte, feeLimit int64) []byte {
	var raw []byte
	raw = append(raw, pbBytes(1, refBlockBytes)...)
	raw = append(raw, pbBytes(4, refBlockHash)...)
	raw = append(raw, pbVarint(8, uint64(expiration))...)
	raw = append(raw, pbBytes(11, contractProto)...)
	raw = append(raw, pbVarint(14, uint64(timestamp))...)
	if feeLimit > 0 {
		raw = append(raw, pbVarint(18, uint64(feeLimit))...)
	}
	return raw
}

// tronTRC20TransferData 构造 TRC20 transfer(address,uint256) 的调用数据：
// 4 字节选择器 a9059cbb + 32 字节地址字（20 字节 HASH160 右对齐，左补 12 零，符合 TronWeb ABI 编码）
// + 32 字节金额字（大端 uint256）。
func tronTRC20TransferData(toHash20 []byte, amount *big.Int) []byte {
	sel, _ := hex.DecodeString(tronTRC20TransferSelector)
	addrWord := make([]byte, 32)
	copy(addrWord[32-len(toHash20):], toHash20) // 右对齐填充（TRON ABI address = 20 字节哈希）
	amtWord := make([]byte, 32)
	if amount != nil {
		ab := amount.Bytes()
		copy(amtWord[32-len(ab):], ab)
	}
	return append(append(append([]byte{}, sel...), addrWord...), amtWord...)
}

// ---------- 签名：signTRON ----------

// signTRON 构造一笔 TRON 提现交易：取参考区块（ref_block_bytes/hash/时间戳）→ 构建 raw_data
// → SHA256(raw_data) 作为签名摘要与 txID → secp256k1 可恢复 65 字节签名 → 组装 broadcasttransaction
// 广播 JSON（含 raw_data 对象、raw_data_hex、txID、signature）。返回该广播 JSON 字符串，由
// JSONRPCClient.SendRaw(TRON) POST 到 /wallet/broadcasttransaction。
//
// 合约类型由 UnsignedTx.ContractAddress 决定：空 → TransferContract（TRX 转账，金额转 SUN）；
// 非空 → TriggerSmartContract（TRC20 transfer，data 携带选择器+收款地址+金额）。
//
// 需要 TRONState 源提供参考区块；未注入或取块失败返回错误，由网关回退节点侧签名广播（fail-degraded）。
func (s *realSigner) signTRON(ctx context.Context, tx *UnsignedTx) (string, error) {
	if s.tronState == nil {
		return "", fmt.Errorf("TRON 真实签名需要 TRONState 源（参考区块/时间戳），未注入时回退节点侧签名广播")
	}
	blockNum, blockID, blockTs, err := s.tronState.NowBlock(ctx, ChainTRON)
	if err != nil {
		return "", fmt.Errorf("TRON 取参考区块失败（回退节点侧签名）: %w", err)
	}
	_ = blockNum

	blockHash, err := hex.DecodeString(blockID)
	if err != nil || len(blockHash) != 32 {
		return "", fmt.Errorf("TRON blockID 非法（需 32 字节 hex）: %w", err)
	}
	refBlockBytes := blockHash[30:32] // 区块哈希末 2 字节
	refBlockHash := blockHash[0:8]    // 区块哈希前 8 字节

	timestamp := blockTs
	if timestamp == 0 {
		timestamp = time.Now().UTC().UnixMilli()
	}
	expiration := timestamp + tronExpirationDeltaMs

	owner := tronAddressBytes(s.key.Public())
	ownerB58 := deriveTronAddress(s.key.Public())

	var (
		param    []byte
		ctype    int
		typeURL  string
		feeLimit int64
		valueJSON interface{}
	)

	if tx.ContractAddress == "" {
		// TransferContract：TRX 原生转账（金额以 SUN 计，1 TRX = 1e6 SUN）。
		to, err := tronAddressToBytes(tx.To)
		if err != nil {
			return "", fmt.Errorf("TRON 收款地址非法: %w", err)
		}
		amountSun := int64(math.Round(tx.Amount * 1e6))
		if amountSun <= 0 {
			return "", fmt.Errorf("TRON 转账金额必须 > 0 (sun)")
		}
		value := tronTransferContract(owner, to, amountSun)
		param = tronAny("type.googleapis.com/protocol.TransferContract", value)
		ctype = tronTransferContractType
		typeURL = "TransferContract"
		valueJSON = tronTransferValue{Amount: amountSun, OwnerAddress: ownerB58, ToAddress: tx.To}
	} else {
		// TriggerSmartContract：TRC20 transfer(address,uint256) 合约调用。
		contract, err := tronAddressToBytes(tx.ContractAddress)
		if err != nil {
			return "", fmt.Errorf("TRON 合约地址非法: %w", err)
		}
		to, err := tronAddressToBytes(tx.To)
		if err != nil {
			return "", fmt.Errorf("TRON 收款地址非法: %w", err)
		}
		toHash := to[1:] // 20 字节 HASH160 用于 ABI address 编码（剥离 0x41）
		// tx.Amount 为人类单位（与 ETH/BTC 路径一致）；TRC20 默认 6 decimals，
		// 缩放为基础单位（如 1 USDT → 1e6）。缺缩放会导致金额少发约 1e6 倍。
		scaled := int64(math.Round(tx.Amount * 1e6))
		if scaled <= 0 {
			return "", fmt.Errorf("TRON TRC20 转账金额必须 > 0")
		}
		amount := new(big.Int).SetInt64(scaled)
		data := tronTRC20TransferData(toHash, amount)
		feeLimit = int64(tx.FeeLimit)
		value := tronTriggerContract(owner, contract, 0, data)
		param = tronAny("type.googleapis.com/protocol.TriggerSmartContract", value)
		ctype = tronTriggerSmartContractType
		typeURL = "TriggerSmartContract"
		valueJSON = tronTriggerValue{OwnerAddress: ownerB58, ContractAddress: tx.ContractAddress, CallValue: 0, Data: hex.EncodeToString(data)}
	}

	contractProto := tronContract(ctype, param)
	rawData := tronRawDataProto(refBlockBytes, refBlockHash, expiration, timestamp, contractProto, feeLimit)
	txID := sha256.Sum256(rawData) // txID = SHA256(raw_data)（同时是签名摘要）

	// secp256k1 ECDSA 可恢复签名（经 KeySigner：软件/HSM/KMS，私钥不出域）；低 S 规范化（抗可锻性）。
	r, sBig, err := s.key.SignDigest(ctx, txID)
	if err != nil {
		return "", fmt.Errorf("TRON 签名失败: %w", err)
	}
	if sBig.Cmp(secp256k1HalfN) > 0 {
		sBig = new(big.Int).Sub(secp256k1Order, sBig)
	}
	recID, err := recoverRecID(txID, r, sBig, s.key.Public())
	if err != nil {
		return "", err
	}
	sig := make([]byte, 65)
	sig[0] = byte(27 + recID)
	rb := r.Bytes()
	if len(rb) < 32 {
		rb = append(make([]byte, 32-len(rb)), rb...)
	}
	sb := sBig.Bytes()
	if len(sb) < 32 {
		sb = append(make([]byte, 32-len(sb)), sb...)
	}
	copy(sig[1:33], rb)
	copy(sig[33:65], sb)

	// 广播 JSON：raw_data 对象（节点据此重序列化，字段顺序不影响 txID，因 protobuf 按字段号排序）、
	// raw_data_hex（即上面签名的字节，便于核对）、txID、signature（65 字节 hex）。
	rawDataJSON := tronRawDataJSON{
		RefBlockBytes: hex.EncodeToString(refBlockBytes),
		RefBlockHash:  hex.EncodeToString(refBlockHash),
		Expiration:    expiration,
		Timestamp:     timestamp,
		Contract: []tronContractJSON{
			{
				Parameter: tronParameterJSON{Value: valueJSON, TypeURL: "type.googleapis.com/protocol." + typeURL},
				Type:      typeURL,
			},
		},
	}
	if feeLimit > 0 {
		rawDataJSON.FeeLimit = feeLimit
	}
	txn := map[string]interface{}{
		"raw_data":     rawDataJSON,
		"raw_data_hex": hex.EncodeToString(rawData),
		"txID":         hex.EncodeToString(txID[:]),
		"signature":    []string{hex.EncodeToString(sig)},
	}
	out, err := json.Marshal(txn)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ---------- TRON 广播 JSON 结构（字段顺序即声明顺序，与 protobuf 字段号升序一致） ----------

type tronRawDataJSON struct {
	RefBlockBytes string               `json:"ref_block_bytes"`
	RefBlockHash  string               `json:"ref_block_hash"`
	Expiration    int64                `json:"expiration"`
	Contract      []tronContractJSON   `json:"contract"`
	Timestamp     int64                `json:"timestamp"`
	FeeLimit      int64                `json:"fee_limit,omitempty"`
}

type tronContractJSON struct {
	Parameter tronParameterJSON `json:"parameter"`
	Type      string            `json:"type"`
}

type tronParameterJSON struct {
	Value   interface{} `json:"value"`
	TypeURL string      `json:"type_url"`
}

// tronTransferValue 是 TransferContract 的 raw_data JSON value（地址以 base58 展示，金额以 sun 计）。
type tronTransferValue struct {
	Amount      int64  `json:"amount"`
	OwnerAddress string `json:"owner_address"`
	ToAddress   string `json:"to_address"`
}

// tronTriggerValue 是 TriggerSmartContract 的 raw_data JSON value（data 以 hex 展示）。
type tronTriggerValue struct {
	OwnerAddress   string `json:"owner_address"`
	ContractAddress string `json:"contract_address"`
	CallValue      int64  `json:"call_value"`
	Data           string `json:"data"`
}

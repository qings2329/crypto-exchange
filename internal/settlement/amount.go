package settlement

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// AssetAmount 是「按资产最小单位存储的整数金额」。Value 为最小单位整数（如 wei/satoshi/sun），
// Decimals 为资产小数位（ETH=18、BTC=8、TRON/USDT-TRC20=6）。自描述，避免全局 decimals 表；
// 运算/渲染时按 Decimals 对齐。用于消除领域金额 float64 精度漂移（#6）。
//
// 注意：Value 视为不可变（运算返回新实例），调用方不得原地修改 Value。
type AssetAmount struct {
	Value    *big.Int
	Decimals int
}

// pow10 返回 10^n（n>=0）。调用方需保证 n 合理，避免溢出 panic。
func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// NewAssetAmount 用最小单位整数与小数位构造（Value 为 nil 时按 0 处理）。
func NewAssetAmount(value *big.Int, decimals int) AssetAmount {
	if value == nil {
		value = big.NewInt(0)
	}
	return AssetAmount{Value: new(big.Int).Set(value), Decimals: decimals}
}

// AssetAmountFromInt64 由人类单位整数（如 1000 个 USDT）按 decimals 缩放为最小单位。
func AssetAmountFromInt64(human int64, decimals int) AssetAmount {
	return AssetAmount{Value: new(big.Int).Mul(big.NewInt(human), pow10(decimals)), Decimals: decimals}
}

// AssetAmountFromFloat 由人类单位浮点（边界/测试用）按 decimals 缩放为最小单位。
// 仅用于 API/测试边界把 float 转精确整数；领域内部不应再产生 float 金额。
func AssetAmountFromFloat(human float64, decimals int) AssetAmount {
	f := new(big.Float).SetPrec(256).SetFloat64(human)
	f.Mul(f, new(big.Float).SetPrec(256).SetInt(pow10(decimals)))
	v, _ := f.Int(nil)
	if v == nil {
		v = big.NewInt(0)
	}
	return AssetAmount{Value: v, Decimals: decimals}
}

// AssetAmountFromString 解析十进制字符串（人类单位，如 "1.5"）为最小单位整数。
func AssetAmountFromString(s string, decimals int) (AssetAmount, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return AssetAmount{}, fmt.Errorf("invalid amount %q", s)
	}
	v := new(big.Int).Mul(r.Num(), pow10(decimals))
	v.Quo(v, r.Denom())
	return AssetAmount{Value: v, Decimals: decimals}, nil
}

// ToDecimals 返回对齐到目标小数位的等价金额（放大用乘、缩小用整除截断）。
// 供跨包把已有的 AssetAmount 重新标定到某资产的标准小数位（如从存储字符串解析后归一化）。
func (a AssetAmount) ToDecimals(dec int) AssetAmount { return a.toDecimals(dec) }

// toDecimals 把金额对齐到目标小数位（放大用乘、缩小用整除截断）。零值（Value 为 nil）
// 按 0 处理，避免对 nil *big.Int 解引用 panic。
func (a AssetAmount) toDecimals(dec int) AssetAmount {
	v := a.Value
	if v == nil {
		v = big.NewInt(0)
	}
	if a.Decimals == dec {
		return AssetAmount{Value: new(big.Int).Set(v), Decimals: dec}
	}
	d := dec - a.Decimals
	if d > 0 {
		return AssetAmount{Value: new(big.Int).Mul(v, pow10(d)), Decimals: dec}
	}
	return AssetAmount{Value: new(big.Int).Div(v, pow10(-d)), Decimals: dec}
}

// Add 返回 a+b（按两者较大 decimals 对齐）。
func (a AssetAmount) Add(b AssetAmount) AssetAmount {
	dec := max(a.Decimals, b.Decimals)
	x := a.toDecimals(dec)
	y := b.toDecimals(dec)
	return AssetAmount{Value: new(big.Int).Add(x.Value, y.Value), Decimals: dec}
}

// Sub 返回 a-b（按两者较大 decimals 对齐，缩小截断）。
func (a AssetAmount) Sub(b AssetAmount) AssetAmount {
	dec := max(a.Decimals, b.Decimals)
	x := a.toDecimals(dec)
	y := b.toDecimals(dec)
	return AssetAmount{Value: new(big.Int).Sub(x.Value, y.Value), Decimals: dec}
}

// Cmp 比较 a 与 b（按较大 decimals 对齐），返回 -1/0/1。
func (a AssetAmount) Cmp(b AssetAmount) int {
	dec := max(a.Decimals, b.Decimals)
	return a.toDecimals(dec).Value.Cmp(b.toDecimals(dec).Value)
}

// Sign 返回 Value 的符号（-1/0/1）。零值（Value 为 nil）视为 0。
func (a AssetAmount) Sign() int {
	if a.Value == nil {
		return 0
	}
	return a.Value.Sign()
}

// IsPositive 是否为正金额。
func (a AssetAmount) IsPositive() bool { return a.Value.Sign() > 0 }

// IsZero 是否为零（Value 为 nil 时视为 0，与 Sign 一致）。
func (a AssetAmount) IsZero() bool { return a.Sign() == 0 }

// HumanString 人类可读十进制（如 "1.5"），去除尾随零与可能的小数点。零值（Value 为 nil）返回 "0"。
func (a AssetAmount) HumanString() string {
	v := a.Value
	if v == nil {
		v = big.NewInt(0)
	}
	if a.Decimals <= 0 {
		return v.String()
	}
	neg := false
	s := v.String()
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	if len(s) <= a.Decimals {
		s = "0" + strings.Repeat("0", a.Decimals-len(s)) + s
	}
	intPart := s[:len(s)-a.Decimals]
	fracPart := strings.TrimRight(s[len(s)-a.Decimals:], "0")
	out := intPart
	if fracPart != "" {
		out += "." + fracPart
	}
	if neg {
		out = "-" + out
	}
	return out
}

// HumanFloat 人类单位浮点（仅展示/边界用；大数有 float64 精度限制）。零值（Value 为 nil）返回 0。
func (a AssetAmount) HumanFloat() float64 {
	if a.Value == nil {
		return 0
	}
	f := new(big.Float).SetPrec(256).SetInt(a.Value)
	den := new(big.Float).SetPrec(256).SetInt(pow10(a.Decimals))
	f.Quo(f, den)
	r, _ := f.Float64()
	return r
}

// MarshalJSON 序列化为 JSON 数字（人类可读），保持对外契约为数字而非字符串。
func (a AssetAmount) MarshalJSON() ([]byte, error) {
	return []byte(a.HumanString()), nil
}

// UnmarshalJSON 解析 JSON 数字或字符串为人类单位十进制，按小数位数推断 Decimals，
// 精确无损（终止小数可原样往返）。
func (a *AssetAmount) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	s = strings.Trim(s, `"`)
	if s == "null" || s == "" {
		*a = AssetAmount{Value: big.NewInt(0), Decimals: 0}
		return nil
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	dot := strings.IndexByte(s, '.')
	var intStr, fracStr string
	if dot < 0 {
		intStr = s
		fracStr = ""
	} else {
		intStr = s[:dot]
		fracStr = s[dot+1:]
	}
	digits := intStr + fracStr
	if digits == "" {
		digits = "0"
	}
	v, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return fmt.Errorf("invalid amount %q", s)
	}
	if neg {
		v.Neg(v)
	}
	*a = AssetAmount{Value: v, Decimals: len(fracStr)}
	return nil
}

// AssetDecimals 返回某链某资产的标准小数位（与现有扫描器口径一致）：
// ETH=18(wei)、BTC=8(satoshi)、TRON=6(sun)；其余默认 8。
func AssetDecimals(chain Chain, asset string) int {
	switch chain {
	case ChainBTC:
		return 8
	case ChainETH:
		return 18
	case ChainTRON:
		return 6
	case ChainSOL:
		// 原生 SOL 为 9 位小数；SPL 代币（如 USDC）统一 6 位小数，其余资产按 6 处理。
		if asset == "SOL" {
			return 9
		}
		return 6
	default:
		return 8
	}
}

// AssetDecimalsByName 按资产名返回标准小数位（不依赖链上下文），供 ledger 等无 chain
// 信息的模块把 float 金额包装为 AssetAmount，以及旧快照迁移使用。
// BTC=8、ETH=18、USDT/USDC/TRX/TRON=6，其余默认 8。
func AssetDecimalsByName(asset string) int {
	switch asset {
	case "BTC":
		return 8
	case "ETH":
		return 18
	case "USDT", "USDC", "TRX", "TRON", "TRC20":
		return 6
	case "SOL":
		return 9
	default:
		return 8
	}
}

// KnownAsset 判断资产是否在标准 decimals 表中（与 AssetDecimalsByName 口径一致）。
// 用于边界校验：拒绝未知/不受支持的资产，避免账本按默认 8 位小数缩放导致精度错配，
// 或在未知资产上凭空铸造/销毁余额（F5 资产白名单）。
//
// 注意：本函数与 AssetDecimalsByName 的 case 必须保持同步——新增受支持资产时两处都要改。
func KnownAsset(asset string) bool {
	switch asset {
	case "BTC", "ETH", "USDT", "USDC", "TRX", "TRON", "TRC20", "SOL":
		return true
	default:
		return false
	}
}

// Neg 返回金额的相反数（Value 取负，Decimals 不变）。
func (a AssetAmount) Neg() AssetAmount {
	if a.Value == nil {
		return AssetAmount{Value: big.NewInt(0), Decimals: a.Decimals}
	}
	return AssetAmount{Value: new(big.Int).Neg(a.Value), Decimals: a.Decimals}
}

// Min 返回 a、b 中较小者（按较大 decimals 对齐后比较）。
func (a AssetAmount) Min(b AssetAmount) AssetAmount {
	if a.Cmp(b) <= 0 {
		return a
	}
	return b
}

// Max 返回 a、b 中较大者（按较大 decimals 对齐后比较）。
func (a AssetAmount) Max(b AssetAmount) AssetAmount {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}

// 确保 AssetAmount 实现 json.Marshaler/Unmarshaler（编译期检查）。
var (
	_ json.Marshaler   = AssetAmount{}
	_ json.Unmarshaler = (*AssetAmount)(nil)
)

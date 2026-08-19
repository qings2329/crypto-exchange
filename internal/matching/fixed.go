package matching

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
)

// Fixed 是定点小数：值 = Raw / 10^Scale。用于撮合引擎的价格与数量，替代 float64 以消除
// 精度漂移（对应资金精度安全主线 F2 定点化）。作为值类型（仅含 int 字段），可直接作 map 键
// （如订单簿按价格分档 map[Fixed]*Level）。
//
// 与 settlement.AssetAmount 的区别：AssetAmount 语义绑定「资产最小单位整数 + 资产小数位」，
// 而撮合引擎的 Price/Qty 是独立于具体资产的「计价比/数量」，各自拥有 PriceScale/QtyScale，
// 因此这里用更轻量、解耦的 Fixed 类型；对外 JSON 统一序列化为字符串，无损且对前端友好。
type Fixed struct {
	Raw   int64
	Scale int
}

// pow10big 返回 10^n（n>=0），用于按小数位缩放。
func pow10big(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// FixedFromRaw 由最小单位整数与小数位构造。
func FixedFromRaw(raw int64, scale int) Fixed { return Fixed{Raw: raw, Scale: scale} }

// FixedFromFloat 由人类单位浮点构造（仅边界/测试用）；按 scale 缩放为最小单位整数，
// 用 big.Float 精确缩放，避免 float64 直接乘 10^scale 的取整误差。
func FixedFromFloat(f float64, scale int) Fixed {
	bf := new(big.Float).SetPrec(256).SetFloat64(f)
	bf.Mul(bf, new(big.Float).SetPrec(256).SetInt(pow10big(scale)))
	v, _ := bf.Int(nil)
	if v == nil {
		v = big.NewInt(0)
	}
	return Fixed{Raw: v.Int64(), Scale: scale}
}

// FixedFromString 解析十进制字符串为人类单位定点（如 "100.50" → Raw=10050,Scale=2）。
// 终止小数可原样精确往返；非法返回错误。
func FixedFromString(s string, scale int) (Fixed, error) {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(s))
	if !ok {
		return Fixed{}, fmt.Errorf("invalid fixed %q", s)
	}
	v := new(big.Int).Mul(r.Num(), pow10big(scale))
	v.Quo(v, r.Denom())
	if !v.IsInt64() {
		return Fixed{}, fmt.Errorf("fixed %q overflow at scale %d", s, scale)
	}
	return Fixed{Raw: v.Int64(), Scale: scale}, nil
}

// scaleUp 对齐到目标小数位（放大用乘、缩小用整除截断）。
func (a Fixed) scaleUp(scale int) Fixed {
	if scale == a.Scale {
		return a
	}
	diff := scale - a.Scale
	raw := new(big.Int).SetInt64(a.Raw)
	if diff > 0 {
		raw.Mul(raw, pow10big(diff))
	} else {
		raw.Quo(raw, pow10big(-diff))
	}
	if !raw.IsInt64() {
		// 对齐后溢出：保持原值并收紧 scale，避免 panic（极端大额场景保护）。
		return a
	}
	return Fixed{Raw: raw.Int64(), Scale: scale}
}

// AlignScale 返回对齐到目标小数位的等价定点（放大用乘、缩小用整除截断）。
// 供包外（如 cmd/matching handler）将边界解析出的 Fixed 对齐到该交易对配置的 scale。
func (a Fixed) AlignScale(scale int) Fixed { return a.scaleUp(scale) }

// ToBaseUnits 把人类单位定点（值 = Raw/10^Scale）按目标小数位 decimals 缩放为最小单位整数
// （如资产 base units），供把价格×数量等定点结果无损转换为 settlement.AssetAmount。
// 放大用乘、缩小用整除截断。Raw 为 int64 必可容纳，不会返回 nil。
func (a Fixed) ToBaseUnits(decimals int) *big.Int {
	raw := new(big.Int).SetInt64(a.Raw)
	diff := decimals - a.Scale
	if diff > 0 {
		raw.Mul(raw, pow10big(diff))
	} else if diff < 0 {
		raw.Quo(raw, pow10big(-diff))
	}
	return raw
}

// Cmp 比较 a 与 b（按较大 scale 对齐），返回 -1/0/1。
func (a Fixed) Cmp(b Fixed) int {
	scale := a.Scale
	if b.Scale > scale {
		scale = b.Scale
	}
	x := new(big.Int).SetInt64(a.scaleUp(scale).Raw)
	y := new(big.Int).SetInt64(b.scaleUp(scale).Raw)
	return x.Cmp(y)
}

// Add 返回 a+b（按较大 scale 对齐；结果 scale = max）。
func (a Fixed) Add(b Fixed) Fixed {
	scale := a.Scale
	if b.Scale > scale {
		scale = b.Scale
	}
	return Fixed{Raw: a.scaleUp(scale).Raw + b.scaleUp(scale).Raw, Scale: scale}
}

// Sub 返回 a-b（按较大 scale 对齐；结果 scale = max）。
func (a Fixed) Sub(b Fixed) Fixed {
	scale := a.Scale
	if b.Scale > scale {
		scale = b.Scale
	}
	return Fixed{Raw: a.scaleUp(scale).Raw - b.scaleUp(scale).Raw, Scale: scale}
}

// Mul 返回 a*b；结果 scale = a.Scale + b.Scale（用 big.Int 中间态防溢出）。
func (a Fixed) Mul(b Fixed) Fixed {
	raw := new(big.Int).Mul(new(big.Int).SetInt64(a.Raw), new(big.Int).SetInt64(b.Raw))
	if !raw.IsInt64() {
		// 乘积溢出 int64：撮合场景（价格×数量）规模远小于此，理论上不可达；饱和到上限保护。
		if raw.Sign() > 0 {
			return Fixed{Raw: 1<<63 - 1, Scale: a.Scale + b.Scale}
		}
		return Fixed{Raw: -1 << 63, Scale: a.Scale + b.Scale}
	}
	return Fixed{Raw: raw.Int64(), Scale: a.Scale + b.Scale}
}

// Div 返回 a/b（结果 scale = a.Scale，整除截断）；b 为零返回零值。
// 全程 big.Int 中间态：原实现先 scaleUp(a.Scale+b.Scale) 再除以 b.Raw，当 a 值较大时
// 中间值（如 notional 4.5e14 × 10^8）溢出 int64，scaleUp 会静默返回未对齐原值，
// 导致商按错误 scale 截断（如强平成交均价被缩小 1e8 倍）。
func (a Fixed) Div(b Fixed) Fixed {
	if b.Raw == 0 {
		return Fixed{Raw: 0, Scale: a.Scale}
	}
	// 商 = a.Raw × 10^b.Scale / b.Raw（结果即 a.Scale 尺度下的整数）。
	num := new(big.Int).Mul(new(big.Int).SetInt64(a.Raw), pow10big(b.Scale))
	raw := num.Quo(num, new(big.Int).SetInt64(b.Raw))
	if !raw.IsInt64() {
		// 商溢出 int64：撮合场景理论不可达，饱和到边界保护。
		if raw.Sign() > 0 {
			return Fixed{Raw: 1<<63 - 1, Scale: a.Scale}
		}
		return Fixed{Raw: -1 << 63, Scale: a.Scale}
	}
	return Fixed{Raw: raw.Int64(), Scale: a.Scale}
}

// Sign 返回 Raw 的符号（-1/0/1）。
func (a Fixed) Sign() int {
	switch {
	case a.Raw < 0:
		return -1
	case a.Raw > 0:
		return 1
	default:
		return 0
	}
}

// IsZero 是否为零。
func (a Fixed) IsZero() bool { return a.Raw == 0 }

// IsPositive 是否为正。
func (a Fixed) IsPositive() bool { return a.Raw > 0 }

// Float 人类单位浮点（仅边界/测试/兼容用；大数有 float64 精度限制）。
func (a Fixed) Float() float64 {
	f := new(big.Float).SetPrec(256).SetInt(new(big.Int).SetInt64(a.Raw))
	f.Quo(f, new(big.Float).SetPrec(256).SetInt(pow10big(a.Scale)))
	r, _ := f.Float64()
	return r
}

// String 人类可读十进制（如 "100.50"），去除尾随零与可能的小数点。
func (a Fixed) String() string {
	neg := a.Raw < 0
	// 用 big.Int 取绝对值：直接 -a.Raw 在 Raw==MinInt64 时回绕为负（Mul 饱和路径可产生）。
	mag := new(big.Int).SetInt64(a.Raw)
	if neg {
		mag.Neg(mag)
	}
	s := mag.String()
	if a.Scale <= 0 {
		if neg {
			return "-" + s
		}
		return s
	}
	if len(s) <= a.Scale {
		s = "0" + strings.Repeat("0", a.Scale-len(s)) + s
	}
	intPart := s[:len(s)-a.Scale]
	fracPart := strings.TrimRight(s[len(s)-a.Scale:], "0")
	out := intPart
	if fracPart != "" {
		out += "." + fracPart
	}
	if neg {
		out = "-" + out
	}
	return out
}

// MarshalJSON 序列化为 JSON 字符串（无损、对前端/跨语言友好）。
func (a Fixed) MarshalJSON() ([]byte, error) {
	return []byte(`"` + a.String() + `"`), nil
}

// UnmarshalJSON 解析 JSON 数字或字符串为人类单位十进制，按出现的小数位推断 Scale，
// 精确无损（终止小数可原样往返）。兼容旧客户端发送的数字与定点化后发送的字符串。
func (a *Fixed) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	s = strings.Trim(s, `"`)
	if s == "null" || s == "" {
		*a = Fixed{Raw: 0, Scale: 0}
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
		return fmt.Errorf("invalid fixed %q", s)
	}
	if neg {
		v.Neg(v)
	}
	if !v.IsInt64() {
		return fmt.Errorf("fixed %q overflow", s)
	}
	*a = Fixed{Raw: v.Int64(), Scale: len(fracStr)}
	return nil
}

// 确保 Fixed 实现 json.Marshaler/Unmarshaler（编译期检查）。
var (
	_ json.Marshaler   = Fixed{}
	_ json.Unmarshaler = (*Fixed)(nil)
)

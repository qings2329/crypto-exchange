package matching

import (
	"math"
	"testing"
)

// 定点化后测试统一的价/量构造 helper：
// 价格按 PriceScale=2、数量按 QtyScale=8 对齐（与 configs/config.yaml 默认一致）。
// FixedFromFloat 经 256bit big.Float 缩放，整数与短小数可精确往返。
func fxPrice(f float64) Fixed { return FixedFromFloat(f, 2) }
func fxQty(f float64) Fixed   { return FixedFromFloat(f, 8) }

// TestFixedDivAvgPrice 回归：notional(scale 10) / filled(scale 8) 求强平均价。
// 原实现 scaleUp(18) 在 notional 较大时（如 4.5e14）中间值溢出 int64，scaleUp 静默
// 返回未对齐原值，导致均价被错误缩小 1e8 倍（0.00045 而非 45000）。
func TestFixedDivAvgPrice(t *testing.T) {
	notional := fxPrice(45000).Mul(fxQty(1)) // 45000*1，scale 10
	filled := fxQty(1)                       // scale 8
	avg := notional.Div(filled)
	if got := avg.Float(); math.Abs(got-45000) > 1e-9 {
		t.Fatalf("avg price = %.4f, want 45000", got)
	}
	// 加权场景：43000*0.5 + 45000*0.5 = 44000
	notional = fxPrice(43000).Mul(fxQty(0.5)).Add(fxPrice(45000).Mul(fxQty(0.5)))
	avg = notional.Div(filled)
	if got := avg.Float(); math.Abs(got-44000) > 1e-6 {
		t.Fatalf("weighted avg = %.4f, want 44000", got)
	}
	// 除零返回零值不 panic。
	if z := fxPrice(100).Mul(fxQty(2)).Div(Fixed{}); !z.IsZero() {
		t.Fatalf("div-by-zero should return zero, got %v", z)
	}
}

// TestFixedStringEdge 覆盖绝对值边界：Raw==MinInt64（Mul 饱和路径可产生）不应输出 "--…"。
func TestFixedStringEdge(t *testing.T) {
	neg := Fixed{Raw: math.MinInt64, Scale: 2}
	if s := neg.String(); s != "-92233720368547758.08" {
		t.Fatalf("MinInt64 String() = %q", s)
	}
	if s := fxPrice(100.5).String(); s != "100.5" {
		t.Fatalf("100.5 String() = %q", s)
	}
	if s := (Fixed{Raw: 0, Scale: 8}).String(); s != "0" {
		t.Fatalf("zero String() = %q", s)
	}
}

// TestFixedToBaseUnits 验证定点人类单位 → 资产最小单位的无损转换（spot 预冻结/结算依赖）。
func TestFixedToBaseUnits(t *testing.T) {
	// 1.5 BTC @ QtyScale 8 → 1.5e8 最小单位（BTC decimals=8）。
	q := fxQty(1.5)
	if got := q.ToBaseUnits(8).String(); got != "150000000" {
		t.Fatalf("1.5 BTC base units = %s, want 150000000", got)
	}
	// cost = 100.12 * 1.5 = 150.18 USDT（USDT decimals=6）→ 150180000。
	cost := fxPrice(100.12).Mul(fxQty(1.5))
	if got := cost.ToBaseUnits(6).String(); got != "150180000" {
		t.Fatalf("150.18 USDT base units = %s, want 150180000", got)
	}
}

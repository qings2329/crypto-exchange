package options

import "math"

// normCDF 用误差函数近似标准正态分布的累积分布函数。
func normCDF(x float64) float64 {
	return 0.5 * (1 + math.Erf(x/math.Sqrt2))
}

// BlackScholes 计算欧式期权的理论价格（quote 计）与 Delta。
//   - typ:    call / put
//   - spot:   标的现价
//   - strike: 行权价
//   - t:      到期剩余时间（年）
//   - r:      无风险利率（年化）
//   - vol:    波动率（年化）
//
// 当 t<=0、vol<=0 或 spot<=0 时退化为内在价值（用于到期/无效输入情形）。
func BlackScholes(typ OptionType, spot, strike, t, r, vol float64) (price, delta float64) {
	if t <= 0 || vol <= 0 || spot <= 0 {
		iv := 0.0
		switch typ {
		case TypeCall:
			if spot > strike {
				iv = spot - strike
			}
			return iv, boolToF(spot > strike)
		case TypePut:
			if strike > spot {
				iv = strike - spot
			}
			return iv, boolToF(strike > spot) * -1
		}
		return 0, 0
	}
	d1 := (math.Log(spot/strike) + (r+0.5*vol*vol)*t) / (vol * math.Sqrt(t))
	d2 := d1 - vol*math.Sqrt(t)
	switch typ {
	case TypeCall:
		price = spot*normCDF(d1) - strike*math.Exp(-r*t)*normCDF(d2)
		delta = normCDF(d1)
	case TypePut:
		price = strike*math.Exp(-r*t)*normCDF(-d2) - spot*normCDF(-d1)
		delta = normCDF(d1) - 1
	}
	return price, delta
}

func boolToF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

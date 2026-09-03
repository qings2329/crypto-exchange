package adminapi_test

import (
	"testing"
)

// TestTradingFeeEndpoints 验证交易手续费配置接口的读/写权限与基本行为。
func TestTradingFeeEndpoints(t *testing.T) {
	r, tok := newC2CTestServer(t) // 复用已有 helper，token 含 super_admin 全权限

	// GET 无权限（未登录）应 401
	code, _ := getJSON(t, r, "/api/admin/trading-fees", "")
	if code != 401 {
		t.Fatalf("unauth trading-fees: got %d, want 401", code)
	}

	// GET 应返回默认快照
	code, data := getJSON(t, r, "/api/admin/trading-fees", tok)
	if code != 200 {
		t.Fatalf("get trading-fees: got %d, want 200", code)
	}
	snap, ok := data.(map[string]interface{})
	if !ok {
		t.Fatalf("get trading-fees: expected map, got %T", data)
	}
	if snap["global_taker_rate"] == nil {
		t.Fatalf("get trading-fees: missing global_taker_rate")
	}

	// PUT 更新全局费率
	code, data = putJSON(t, r, "/api/admin/trading-fees", tok, map[string]interface{}{
		"global_taker_rate": 0.0005,
		"global_maker_rate": -0.0001,
	})
	if code != 200 {
		t.Fatalf("put trading-fees: got %d, want 200", code)
	}
	snap = data.(map[string]interface{})
	if snap["global_taker_rate"] != 0.0005 {
		t.Fatalf("put taker rate = %v, want 0.0005", snap["global_taker_rate"])
	}
	if snap["global_maker_rate"] != -0.0001 {
		t.Fatalf("put maker rate = %v, want -0.0001", snap["global_maker_rate"])
	}

	// PUT 更新 VIP 折扣 + 交易对覆盖
	code, data = putJSON(t, r, "/api/admin/trading-fees", tok, map[string]interface{}{
		"vip_discounts": []map[string]interface{}{
			{"level": 3, "taker_discount": 0.5, "maker_discount": 0},
		},
		"symbol_overrides": map[string]interface{}{
			"BTC_USDT": map[string]interface{}{"taker_rate": 0.003, "maker_rate": 0},
		},
	})
	if code != 200 {
		t.Fatalf("put fees with vip+symbol: got %d, want 200", code)
	}
}

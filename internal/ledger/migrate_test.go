package ledger

import (
	"testing"

	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// TestMigrateV1ToV2CompositeKeys 验证 v1→v2 快照迁移对复合 key 的 decimals 解析正确。
// 复合 key：坏账归属 "userID:asset"、当日提现额 "uid:asset:YYYY-MM-DD"；普通 key 即资产名。
// 回归点：migrateMap 必须按 key 中的资产段（而非整个 key）取 decimals，否则 USDT(6) 会误用默认 8 位，
// 导致金额放大 100 倍。
func TestMigrateV1ToV2CompositeKeys(t *testing.T) {
	v1 := ledgerSnapshotV1{
		BadDebtByUser: map[string]float64{
			"1:USDT": 1000.0, // USDT=6 -> 1e9 最小单位
			"2:BTC":  0.5,    // BTC=8  -> 5e7 最小单位
		},
		DailyWithdrawUsed: map[string]float64{
			"1:USDT:2026-08-16": 500.0, // USDT=6 -> 5e8 最小单位
		},
		HotWallet: map[string]float64{
			"BTC": 1.0, // BTC=8 -> 1e8 最小单位
		},
		DailyWithdrawLimit: map[string]float64{
			"ETH": 2.0, // ETH=18
		},
	}

	snap := migrateV1ToV2(v1)

	// 坏账：USDT 应按 6 位缩放
	wantUSDT := settlement.AssetAmountFromFloat(1000.0, settlement.AssetDecimalsByName("USDT"))
	if got := snap.BadDebtByUser["1:USDT"]; got.Cmp(wantUSDT) != 0 {
		t.Fatalf("BadDebtByUser[1:USDT] = %s (dec %d), want %s (dec %d)",
			got.HumanString(), got.Decimals, wantUSDT.HumanString(), wantUSDT.Decimals)
	}
	// 坏账：BTC 应按 8 位缩放
	wantBTC := settlement.AssetAmountFromFloat(0.5, settlement.AssetDecimalsByName("BTC"))
	if got := snap.BadDebtByUser["2:BTC"]; got.Cmp(wantBTC) != 0 {
		t.Fatalf("BadDebtByUser[2:BTC] = %s (dec %d), want %s (dec %d)",
			got.HumanString(), got.Decimals, wantBTC.HumanString(), wantBTC.Decimals)
	}
	// 当日提现额：复合 key 第 2 段为资产名，输入 500.0 USDT -> 5e8 最小单位
	wantUSDTFive := settlement.AssetAmountFromFloat(500.0, settlement.AssetDecimalsByName("USDT"))
	if got := snap.DailyWithdrawUsed["1:USDT:2026-08-16"]; got.Cmp(wantUSDTFive) != 0 {
		t.Fatalf("DailyWithdrawUsed = %s (dec %d), want %s (dec %d)",
			got.HumanString(), got.Decimals, wantUSDT.HumanString(), wantUSDT.Decimals)
	}
	// 普通 key 资产名
	wantBTC1 := settlement.AssetAmountFromFloat(1.0, settlement.AssetDecimalsByName("BTC"))
	if got := snap.HotWallet["BTC"]; got.Cmp(wantBTC1) != 0 {
		t.Fatalf("HotWallet[BTC] = %s (dec %d), want %s (dec %d)",
			got.HumanString(), got.Decimals, wantBTC1.HumanString(), wantBTC1.Decimals)
	}
	wantETH := settlement.AssetAmountFromFloat(2.0, settlement.AssetDecimalsByName("ETH"))
	if got := snap.DailyWithdrawLimit["ETH"]; got.Cmp(wantETH) != 0 {
		t.Fatalf("DailyWithdrawLimit[ETH] = %s (dec %d), want %s (dec %d)",
			got.HumanString(), got.Decimals, wantETH.HumanString(), wantETH.Decimals)
	}
}

// TestAssetOfKey 直接验证复合 key 资产名解析（边界值）。
func TestAssetOfKey(t *testing.T) {
	cases := map[string]string{
		"1:USDT":               "USDT",
		"1:USDT:2026-08-16":    "USDT",
		"42:BTC:2026-01-02":    "BTC",
		"USDT":                 "USDT",
		"BTC":                  "BTC",
		"ETH":                  "ETH",
	}
	for k, want := range cases {
		if got := assetOfKey(k); got != want {
			t.Fatalf("assetOfKey(%q) = %q, want %q", k, got, want)
		}
	}
}

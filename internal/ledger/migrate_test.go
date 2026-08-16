package ledger

import (
	"encoding/json"
	"strings"
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

// TestMigrateV1ToV2Realistic 验证 v1→v2 迁移对「真实形状」快照（账户余额 / 流水 / 提现冻结 /
// 社会化分摊）的多资产定点化正确，且各资产总余额在迁移前后保持一致（无凭空增减）。
func TestMigrateV1ToV2Realistic(t *testing.T) {
	v1 := ledgerSnapshotV1{
		Accounts: []*accountV1{
			{UserID: 1, Asset: "BTC", Available: 1.5, Frozen: 0.1},
			{UserID: 2, Asset: "USDT", Available: 1000, Frozen: 0},
			{UserID: 3, Asset: "ETH", Available: 2.0, Frozen: 0.5},
		},
		Log: []entryV1{
			{ID: 1, UserID: 1, Asset: "BTC", Delta: 0.1, Balance: 1.5},
			{ID: 2, UserID: 2, Asset: "USDT", Delta: -10, Balance: 1000},
		},
		WithdrawHolds: []*withdrawHoldEntryV1{
			{ID: "h1", UserID: 1, Asset: "BTC", Amount: 0.5, Fee: 0.0001},
		},
		SocializeProposals: map[string]socializeProposalV1{
			"p1": {ID: "p1", Asset: "USDT", Recovered: 10, Detail: map[int64]float64{5: 4, 6: 6}},
		},
		BadDebtByUser:     map[string]float64{"1:USDT": 1000.0},
		HotWallet:         map[string]float64{"BTC": 1.0},
		ColdWallet:        map[string]float64{"USDT": 500000.0},
		DailyWithdrawUsed: map[string]float64{"1:USDT:2026-08-16": 500.0},
		Seq:               7,
	}

	snap := migrateV1ToV2(v1)

	// 账户：按各自资产标准 decimals 缩放。
	wantBTC := settlement.AssetAmountFromFloat(1.5, settlement.AssetDecimalsByName("BTC"))
	if got := snap.Accounts[0].Available; got.Cmp(wantBTC) != 0 {
		t.Fatalf("account BTC available = %s, want %s", got.HumanString(), wantBTC.HumanString())
	}
	wantUSDT := settlement.AssetAmountFromFloat(1000.0, settlement.AssetDecimalsByName("USDT"))
	if got := snap.Accounts[1].Available; got.Cmp(wantUSDT) != 0 {
		t.Fatalf("account USDT available = %s, want %s", got.HumanString(), wantUSDT.HumanString())
	}
	wantETH := settlement.AssetAmountFromFloat(2.0, settlement.AssetDecimalsByName("ETH"))
	if got := snap.Accounts[2].Available; got.Cmp(wantETH) != 0 {
		t.Fatalf("account ETH available = %s, want %s", got.HumanString(), wantETH.HumanString())
	}
	// 流水 delta/balance 同精度。
	if got := snap.Log[0].Delta; got.Cmp(settlement.AssetAmountFromFloat(0.1, 8)) != 0 {
		t.Fatalf("log BTC delta = %s, want 0.1 BTC", got.HumanString())
	}
	// 提现冻结 amount/fee。
	if got := snap.WithdrawHolds[0].Amount; got.Cmp(settlement.AssetAmountFromFloat(0.5, 8)) != 0 {
		t.Fatalf("withdraw hold BTC amount = %s, want 0.5 BTC", got.HumanString())
	}
	if got := snap.WithdrawHolds[0].Fee; got.Cmp(settlement.AssetAmountFromFloat(0.0001, 8)) != 0 {
		t.Fatalf("withdraw hold BTC fee = %s, want 0.0001 BTC", got.HumanString())
	}
	// 社会化分摊：提案额 + 各用户明细均按资产 decimals。
	if got := snap.SocializeProposals["p1"].Recovered; got.Cmp(settlement.AssetAmountFromFloat(10, 6)) != 0 {
		t.Fatalf("socialize recovered = %s, want 10 USDT", got.HumanString())
	}
	if got := snap.SocializeProposals["p1"].Detail[5]; got.Cmp(settlement.AssetAmountFromFloat(4, 6)) != 0 {
		t.Fatalf("socialize detail[5] = %s, want 4 USDT", got.HumanString())
	}
	// 冷钱包 USDT 500000 按 6 位缩放。
	if got := snap.ColdWallet["USDT"]; got.Cmp(settlement.AssetAmountFromFloat(500000.0, 6)) != 0 {
		t.Fatalf("cold wallet USDT = %s, want 500000 USDT", got.HumanString())
	}

	// 总余额守恒：各资产账户可用之和 == v1 人类单位按标准 decimals 缩放后的和。
	// BTC: 1.5；USDT: 1000；ETH: 2.0 —— 迁移后（最小单位）应精确等于这些值。
	sums := map[string]float64{"BTC": 1.5, "USDT": 1000.0, "ETH": 2.0}
	for i, a := range snap.Accounts {
		want := settlement.AssetAmountFromFloat(sums[a.Asset], settlement.AssetDecimalsByName(a.Asset))
		if a.Available.Cmp(want) != 0 {
			t.Fatalf("account[%d] %s available = %s, want %s (total not preserved)", i, a.Asset, a.Available.HumanString(), want.HumanString())
		}
	}
}

// TestParseSnapshotVersionRouting 验证 parseSnapshot 对 v1（无 schema_version）走迁移路径、
// 对 v2（schema_version>=2）走直读路径，两者都能正确解析。
func TestParseSnapshotVersionRouting(t *testing.T) {
	// v1 路径：序列化一个无 schema_version 的 v1 结构，应被迁移为 v2。
	v1 := ledgerSnapshotV1{
		Accounts:      []*accountV1{{UserID: 1, Asset: "USDT", Available: 1000}},
		BadDebtByUser: map[string]float64{"1:USDT": 1000.0},
		Seq:           3,
	}
	v1Bytes, err := json.Marshal(v1)
	if err != nil {
		t.Fatalf("marshal v1: %v", err)
	}
	snap1, err := parseSnapshot(v1Bytes)
	if err != nil {
		t.Fatalf("parseSnapshot v1: %v", err)
	}
	if snap1.SchemaVersion != ledgerSnapshotSchemaVersion {
		t.Fatalf("v1 path should yield schema_version=%d, got %d", ledgerSnapshotSchemaVersion, snap1.SchemaVersion)
	}
	if got := snap1.Accounts[0].Available; got.Cmp(settlement.AssetAmountFromFloat(1000.0, 6)) != 0 {
		t.Fatalf("v1 path account USDT = %s, want 1000 USDT", got.HumanString())
	}

	// v2 路径：序列化一个含 schema_version=2 的 v2 结构，应直读而非迁移。
	v2 := LedgerSnapshot{
		SchemaVersion: ledgerSnapshotSchemaVersion,
		Accounts:      []*Account{{UserID: 1, Asset: "USDT", Available: settlement.AssetAmountFromInt64(1000, 6)}},
		Seq:           3,
	}
	v2Bytes, err := json.Marshal(v2)
	if err != nil {
		t.Fatalf("marshal v2: %v", err)
	}
	snap2, err := parseSnapshot(v2Bytes)
	if err != nil {
		t.Fatalf("parseSnapshot v2: %v", err)
	}
	if got := snap2.Accounts[0].Available; got.Cmp(settlement.AssetAmountFromInt64(1000, 6)) != 0 {
		t.Fatalf("v2 path account USDT = %s, want 1000 USDT", got.HumanString())
	}
}

// TestValidateV1SnapshotAssets 验证未知资产预检：未知资产应告警，已知资产不应告警。
func TestValidateV1SnapshotAssets(t *testing.T) {
	v1 := ledgerSnapshotV1{
		Accounts:      []*accountV1{{UserID: 1, Asset: "DOGE", Available: 100}},
		BadDebtByUser: map[string]float64{"1:DOGE": 100},
		HotWallet:     map[string]float64{"USDT": 1.0}, // 已知资产，不告警
	}
	warns := ValidateV1SnapshotAssets(v1)
	// 应至少包含 DOGE 的两条告警（account + bad_debt_by_user），且不应包含 USDT。
	foundDOGE := 0
	for _, w := range warns {
		if strings.Contains(w, "DOGE") {
			foundDOGE++
		}
		if strings.Contains(w, "USDT") {
			t.Fatalf("known asset USDT should not warn: %q", w)
		}
	}
	if foundDOGE < 2 {
		t.Fatalf("expected >=2 DOGE warnings (account + bad_debt_by_user), got %d: %v", foundDOGE, warns)
	}
}

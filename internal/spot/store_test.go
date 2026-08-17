package spot

import (
	"testing"

	"github.com/coldlar/crypto-exchange/internal/matching"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// memStore 是 Store 的内存假实现，用于在无 MySQL 环境下验证「重启恢复幂等映射」的接线逻辑
// （mysqlStore 的 DSN 路径需真实数据库，由集成环境覆盖）。
type memStore struct {
	recs map[int64]OrderRecord
}

func newMemStore() *memStore { return &memStore{recs: make(map[int64]OrderRecord)} }

func (m *memStore) UpsertOrder(r OrderRecord) error {
	m.recs[r.OrderID] = r
	return nil
}

func (m *memStore) DeleteOrder(orderID int64) error {
	delete(m.recs, orderID)
	return nil
}

func (m *memStore) LoadOrders() ([]OrderRecord, error) {
	out := make([]OrderRecord, 0, len(m.recs))
	for _, r := range m.recs {
		out = append(out, r)
	}
	return out, nil
}

// TestRestoreOrdersRebuildsClientOIDMap 验证 #58 核心：重启后由持久化记录重建 clientOIDMap，
// 使同 client_oid 的重试仍被判重、不再双冻。无 client_oid 的记录不应进入映射（避免脏键）。
func TestRestoreOrdersRebuildsClientOIDMap(t *testing.T) {
	s := newTestServer()
	store := newMemStore()
	s.SetStore(store)

	btc := settlement.AssetAmountFromFloat(2, settlement.AssetDecimalsByName("BTC"))
	usdt := settlement.AssetAmountFromFloat(20000, settlement.AssetDecimalsByName("USDT"))

	recs := []OrderRecord{
		{OrderID: 101, User: 1, Side: int(matching.Buy), Symbol: "BTC_USDT", Base: "BTC", Quote: "USDT",
			FrozenQuote: usdt, FrozenBase: settlement.AssetAmount{}, ClientOID: "client-abc"},
		{OrderID: 102, User: 2, Side: int(matching.Sell), Symbol: "BTC_USDT", Base: "BTC", Quote: "USDT",
			FrozenQuote: settlement.AssetAmount{}, FrozenBase: btc, ClientOID: ""}, // 无幂等键
	}
	s.RestoreOrders(recs)

	if got, ok := s.clientOIDMap["1:client-abc"]; !ok || got != 101 {
		t.Fatalf("expect clientOIDMap[1:client-abc]=101, got ok=%v val=%d", ok, got)
	}
	if _, ok := s.clientOIDMap["2:"]; ok {
		t.Fatalf("empty client_oid must NOT be restored into map")
	}
	// 验证反向落库路径：Upsert/Load/Delete 在内存 store 上一致。
	if err := store.UpsertOrder(recs[0]); err != nil {
		t.Fatalf("upsert0: %v", err)
	}
	if err := store.UpsertOrder(recs[1]); err != nil {
		t.Fatalf("upsert1: %v", err)
	}
	loaded, err := store.LoadOrders()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expect 2 records, got %d", len(loaded))
	}
	if err := store.DeleteOrder(101); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := store.recs[101]; ok {
		t.Fatalf("expect order 101 deleted")
	}
}

// TestFreezeRecToRecord 验证预冻结记录到持久化记录的字段映射（含 clientOID 透传）。
func TestFreezeRecToRecord(t *testing.T) {
	usdt := settlement.AssetAmountFromFloat(20000, settlement.AssetDecimalsByName("USDT"))
	rec := &freezeRec{
		user:        7,
		side:        matching.Buy,
		symbol:      "BTC_USDT",
		base:        "BTC",
		quote:       "USDT",
		frozenQuote: usdt,
		clientOID:   "client-xyz",
	}
	r := freezeRecToRecord(555, rec, rec.clientOID)
	if r.OrderID != 555 || r.User != 7 || r.Side != int(matching.Buy) ||
		r.Symbol != "BTC_USDT" || r.Base != "BTC" || r.Quote != "USDT" ||
		r.FrozenQuote.Cmp(usdt) != 0 || r.ClientOID != "client-xyz" {
		t.Fatalf("freezeRecToRecord mapping wrong: %+v", r)
	}
}

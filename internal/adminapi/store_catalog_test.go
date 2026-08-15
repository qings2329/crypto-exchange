package adminapi

import (
	"errors"
	"os"
	"testing"
)

// TestCatalogMemCRUD 覆盖内存实现的全套 CRUD 与部分更新语义，不依赖 MySQL。
func TestCatalogMemCRUD(t *testing.T) {
	s := NewMemCatalogStore()

	// --- 交易对：Upsert 幂等（按 symbol 更新而非追加）---
	if syms, _ := s.ListSymbols(); len(syms) != 0 {
		t.Fatalf("空 store 的 symbols 应为 0，实际 %d", len(syms))
	}
	in := SymbolConfig{Symbol: "MEME_USDT", Base: "MEME", Quote: "USDT", Status: "online", FeeRate: 0.001}
	if out, err := s.UpsertSymbol(in); err != nil || out.Symbol != "MEME_USDT" {
		t.Fatalf("UpsertSymbol: %v / %+v", err, out)
	}
	if out, err := s.UpsertSymbol(SymbolConfig{Symbol: "MEME_USDT", Base: "MEME2", Quote: "USDT", Status: "offline", FeeRate: 0.002}); err != nil {
		t.Fatalf("upsert 更新: %v", err)
	} else if out.Status != "offline" || out.FeeRate != 0.002 || out.Base != "MEME2" {
		t.Fatalf("upsert 应为按 symbol 全量替换而非新增: %+v", out)
	}
	if syms, _ := s.ListSymbols(); len(syms) != 1 {
		t.Fatalf("upsert 相同 symbol 不应追加，期望 1，实际 %d", len(syms))
	}

	// --- 公链：创建 + 部分更新 + 不存在返回 ErrCatalogNotFound ---
	ch, err := s.CreateChain(Chain{Name: "Solana", Symbol: "SOL", Confirmations: 32, DepositEnabled: true, WithdrawEnabled: false})
	if err != nil {
		t.Fatalf("CreateChain: %v", err)
	}
	if ch.ID == 0 {
		t.Fatal("CreateChain 应分配非零 ID")
	}
	// 部分更新：仅覆盖 Name 与 Confirmations；布尔按 patch 值覆盖（此处 false）。
	if up, err := s.UpdateChain(ch.ID, Chain{Name: "Solana Mainnet", Confirmations: 40, DepositEnabled: false}); err != nil {
		t.Fatalf("UpdateChain: %v", err)
	} else if up.Name != "Solana Mainnet" || up.Confirmations != 40 || up.DepositEnabled != false || up.WithdrawEnabled != false {
		t.Fatalf("UpdateChain 部分更新结果不对: %+v", up)
	}
	// 空 patch 不应擦除现有字符串/数值，但布尔仍按 patch(false) 覆盖。
	if up, err := s.UpdateChain(ch.ID, Chain{}); err != nil {
		t.Fatalf("UpdateChain empty patch: %v", err)
	} else if up.Name != "Solana Mainnet" || up.Confirmations != 40 || up.Symbol != "SOL" {
		t.Fatalf("空 patch 不应擦除非空字段: %+v", up)
	}
	if _, err := s.UpdateChain(99999, Chain{Name: "ghost"}); !errors.Is(err, ErrCatalogNotFound) {
		t.Fatalf("更新不存在的链应返回 ErrCatalogNotFound，实际 %v", err)
	}

	// --- 币种：创建 + 部分更新 + 不存在返回 ErrCatalogNotFound ---
	c, err := s.CreateCoin(Coin{Symbol: "SOL", Name: "Solana", Chain: "Solana", Precision: 9, WithdrawFee: 0.01})
	if err != nil {
		t.Fatalf("CreateCoin: %v", err)
	}
	if c.ID == 0 {
		t.Fatal("CreateCoin 应分配非零 ID")
	}
	if up, err := s.UpdateCoin(c.ID, Coin{Name: "Solana Token", Precision: 6}); err != nil {
		t.Fatalf("UpdateCoin: %v", err)
	} else if up.Name != "Solana Token" || up.Precision != 6 || up.Chain != "Solana" || up.WithdrawFee != 0.01 {
		t.Fatalf("UpdateCoin 部分更新结果不对: %+v", up)
	}
	if _, err := s.UpdateCoin(99999, Coin{}); !errors.Is(err, ErrCatalogNotFound) {
		t.Fatalf("更新不存在的币种应返回 ErrCatalogNotFound，实际 %v", err)
	}

	// --- 本地通知：创建 + 列表含 Source=local + 删除 + 重复删除返回 ErrCatalogNotFound ---
	n, err := s.CreateNotification(Notification{Title: "维护", Body: "x", Level: "info"})
	if err != nil {
		t.Fatalf("CreateNotification: %v", err)
	}
	if n.ID == 0 || n.Source != "local" {
		t.Fatalf("CreateNotification 应分配 ID 且 Source=local: %+v", n)
	}
	ns, _ := s.ListNotifications()
	found := false
	for _, x := range ns {
		if x.ID == n.ID && x.Source == "local" {
			found = true
		}
	}
	if !found {
		t.Fatal("ListNotifications 应包含刚创建的本地公告")
	}
	if err := s.DeleteNotification(n.ID); err != nil {
		t.Fatalf("DeleteNotification: %v", err)
	}
	if ns, _ := s.ListNotifications(); len(ns) != 0 {
		t.Fatalf("删除后通知应为 0，实际 %d", len(ns))
	}
	if err := s.DeleteNotification(n.ID); !errors.Is(err, ErrCatalogNotFound) {
		t.Fatalf("重复删除应返回 ErrCatalogNotFound，实际 %v", err)
	}
}

// TestNewCatalogStoreFallback 验证工厂的回退语义：空 DSN 直接内存；
// 非法/不可达 DSN 连接失败后也回退内存（而非返回 nil），并记录错误。
func TestNewCatalogStoreFallback(t *testing.T) {
	// 空 DSN -> 内存实现，无错误。
	s, mem, err := NewCatalogStore("")
	if err != nil || !mem {
		t.Fatalf("空 DSN 应回退内存实现且无错误，got mem=%v err=%v", mem, err)
	}
	if _, err := s.UpsertSymbol(SymbolConfig{Symbol: "FB_USDT"}); err != nil {
		t.Fatalf("回退内存实现应可写: %v", err)
	}

	// 不可达 DSN -> 仍回退内存（mem=true），但返回连接错误。
	badDSN := "root@tcp(127.0.0.1:1)/catalog_test?parseTime=true"
	s2, mem2, err2 := NewCatalogStore(badDSN)
	if !mem2 {
		t.Fatal("不可达 DSN 应回退内存实现（mem=true）")
	}
	if err2 == nil {
		t.Fatal("不可达 DSN 应返回连接错误")
	}
	if s2 == nil {
		t.Fatal("回退时 store 不得为 nil")
	}
	// 回退实现仍应可工作（写入不 panic）。
	if _, err := s2.CreateChain(Chain{Name: "x"}); err != nil {
		t.Fatalf("回退内存实现 CreateChain 应成功: %v", err)
	}
}

// TestSeedCatalogIdempotent 验证 SeedCatalog 在已有数据时跳过、空数据时播种。
func TestSeedCatalogIdempotent(t *testing.T) {
	s := NewMemCatalogStore()
	if err := SeedCatalog(s); err != nil {
		t.Fatalf("SeedCatalog: %v", err)
	}
	if syms, _ := s.ListSymbols(); len(syms) != 3 {
		t.Fatalf("首次 seed 应写入 3 个交易对，实际 %d", len(syms))
	}
	if chs, _ := s.ListChains(); len(chs) != 3 {
		t.Fatalf("首次 seed 应写入 3 条公链，实际 %d", len(chs))
	}
	// 再次 seed 不应重复（按 symbol 幂等 / 列表非 0 即跳过）。
	if err := SeedCatalog(s); err != nil {
		t.Fatalf("二次 SeedCatalog: %v", err)
	}
	if syms, _ := s.ListSymbols(); len(syms) != 3 {
		t.Fatalf("二次 seed 不应重复写入，实际 %d", len(syms))
	}
}

// TestCatalogMySQLCRUD 在设置了 ADMIN_MYSQL_TEST_DSN 时，对真实 MySQL 做 CRUD 验证。
// 未设置时跳过（不污染无 MySQL 的本地/CI 环境）。
func TestCatalogMySQLCRUD(t *testing.T) {
	dsn := os.Getenv("ADMIN_MYSQL_TEST_DSN")
	if dsn == "" {
		t.Skip("set ADMIN_MYSQL_TEST_DSN to run MySQL catalog CRUD tests")
	}
	store, err := NewMySQLCatalogStore(dsn)
	if err != nil {
		t.Fatalf("NewMySQLCatalogStore: %v", err)
	}
	ms, ok := store.(*mysqlCatalogStore)
	if !ok {
		t.Fatal("NewMySQLCatalogStore 应返回 *mysqlCatalogStore")
	}
	// 清空四张表以获得确定性起点（测试库专用，无外键依赖，可安全 TRUNCATE）。
	for _, tbl := range []string{"ce_admin_symbols", "ce_admin_chains", "ce_admin_coins", "ce_admin_notifications"} {
		if _, err := ms.db.Exec("TRUNCATE TABLE " + tbl); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}

	// 交易对：upsert 后可在列表查到，且二次 upsert 同 symbol 为全量替换（非追加）。
	if _, err := store.UpsertSymbol(SymbolConfig{Symbol: "T_MYSQL_USDT", Base: "T", Quote: "USDT", Status: "online"}); err != nil {
		t.Fatalf("mysql UpsertSymbol: %v", err)
	}
	if _, err := store.UpsertSymbol(SymbolConfig{Symbol: "T_MYSQL_USDT", Base: "T", Quote: "USDT", Status: "offline", FeeRate: 0.002}); err != nil {
		t.Fatalf("mysql upsert 更新: %v", err)
	}
	syms, err := store.ListSymbols()
	if err != nil {
		t.Fatalf("mysql ListSymbols: %v", err)
	}
	if len(syms) != 1 || syms[0].Status != "offline" || syms[0].FeeRate != 0.002 || syms[0].Base != "T" {
		t.Fatalf("mysql 交易对应按 symbol 全量替换: %+v", syms)
	}

	// 公链：创建 + 部分更新 + 不存在返回 ErrCatalogNotFound。
	ch, err := store.CreateChain(Chain{Name: "MyChain", Symbol: "MC", Confirmations: 10, DepositEnabled: true, WithdrawEnabled: false})
	if err != nil {
		t.Fatalf("mysql CreateChain: %v", err)
	}
	if ch.ID == 0 {
		t.Fatal("mysql CreateChain 应分配 ID")
	}
	up, err := store.UpdateChain(ch.ID, Chain{Name: "MyChain2", Confirmations: 20})
	if err != nil {
		t.Fatalf("mysql UpdateChain: %v", err)
	}
	if up.Name != "MyChain2" || up.Confirmations != 20 || up.Symbol != "MC" {
		t.Fatalf("mysql UpdateChain 部分更新不对: %+v", up)
	}
	if _, err := store.UpdateChain(999999, Chain{Name: "x"}); !errors.Is(err, ErrCatalogNotFound) {
		t.Fatalf("mysql 更新不存在的链应返回 ErrCatalogNotFound，实际 %v", err)
	}

	// 币种：创建 + 部分更新 + 不存在返回 ErrCatalogNotFound。
	c, err := store.CreateCoin(Coin{Symbol: "MC", Name: "MyCoin", Chain: "MyChain", Precision: 8, WithdrawFee: 0.001})
	if err != nil {
		t.Fatalf("mysql CreateCoin: %v", err)
	}
	cup, err := store.UpdateCoin(c.ID, Coin{Name: "MyCoin2", Precision: 4})
	if err != nil {
		t.Fatalf("mysql UpdateCoin: %v", err)
	}
	if cup.Name != "MyCoin2" || cup.Precision != 4 || cup.Chain != "MyChain" {
		t.Fatalf("mysql UpdateCoin 部分更新不对: %+v", cup)
	}
	if _, err := store.UpdateCoin(999999, Coin{}); !errors.Is(err, ErrCatalogNotFound) {
		t.Fatalf("mysql 更新不存在的币种应返回 ErrCatalogNotFound，实际 %v", err)
	}

	// 通知：创建 + 列表含 Source=local + 删除 + 重复删除返回 ErrCatalogNotFound。
	n, err := store.CreateNotification(Notification{Title: "mysql 通知", Body: "hi", Level: "info"})
	if err != nil {
		t.Fatalf("mysql CreateNotification: %v", err)
	}
	if n.Source != "local" {
		t.Fatalf("mysql CreateNotification 应置 Source=local，实际 %q", n.Source)
	}
	ns, _ := store.ListNotifications()
	if len(ns) != 1 || ns[0].ID != n.ID {
		t.Fatalf("mysql ListNotifications 应含刚创建的通知: %+v", ns)
	}
	if err := store.DeleteNotification(n.ID); err != nil {
		t.Fatalf("mysql DeleteNotification: %v", err)
	}
	if err := store.DeleteNotification(n.ID); !errors.Is(err, ErrCatalogNotFound) {
		t.Fatalf("mysql 重复删除应返回 ErrCatalogNotFound，实际 %v", err)
	}
}

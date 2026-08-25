package futuresapi

import (
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/futures"
)

// TestTPSLStoreRoundtrip 存储层往返：Upsert → LoadAll 验证持久化一致性；
// Delete 后 LoadAll 确认条目已清除。覆盖 mem 实现（MySQL 实现语义相同，集成测试覆盖）。
func TestTPSLStoreRoundtrip(t *testing.T) {
	store := NewMemTPSLStore()
	tp, sl := 55000.0, 48000.0
	key := tpslKey(1, "BTC_USDT_PERP", "long")

	// 空库加载。
	loaded, err := store.LoadAll()
	if err != nil || len(loaded) != 0 {
		t.Fatalf("empty load: %v entries=%d", err, len(loaded))
	}

	// 写入。
	if err := store.Upsert(1, key, &tp, &sl); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	state, ok := loaded[1][key]
	if !ok {
		t.Fatal("key not found after upsert")
	}
	if state.TP == nil || *state.TP != tp || state.SL == nil || *state.SL != sl {
		t.Fatalf("values mismatch: %+v", state)
	}

	// 覆盖写（仅 TP，清除 SL）。
	if err := store.Upsert(1, key, &tp, nil); err != nil {
		t.Fatal(err)
	}
	loaded, _ = store.LoadAll()
	state = loaded[1][key]
	if state.TP == nil || *state.TP != tp || state.SL != nil {
		t.Fatalf("overwrite failed: %+v", state)
	}

	// 删除。
	if err := store.Delete(1, key); err != nil {
		t.Fatal(err)
	}
	loaded, _ = store.LoadAll()
	if _, ok := loaded[1]; ok {
		t.Fatal("key not deleted")
	}
}

// TestTPSLWriteThroughViaServer 端到端：通过 handler 设置 TP-SL，
// 读取 store 确认已落库；再建新 Server 实例共用同一 store，
// 验证 decorateWithTPSL 注入已恢复的值（模拟重启恢复）。
func TestTPSLWriteThroughViaServer(t *testing.T) {
	s := newGapServer(t)
	s.tpslStore = NewMemTPSLStore()
	s.liquidator = futures.NewLiquidator(func(ev futures.LiquidationEvent) {})

	// 注入持仓（通过 Liquidator 的内部机制）。
	s.liquidator.Register("BTC_USDT_PERP")
	s.liquidator.OpenCross("BTC_USDT_PERP", 1, futures.Long, 0.5, 50000, 25000, 1, 0)

	// 设置 TP-SL（走 handler 写穿路径）。
	w, c := authed(http.MethodPut, "/api/v1/futures/tpsl", gin.H{
		"symbol": "BTC_USDT_PERP", "pos_side": "long", "tp": 55000, "sl": 48000,
	})
	s.handleSetTPSL(c)
	if w.Code != 200 {
		t.Fatalf("set tpsl: %d %s", w.Code, w.Body.String())
	}

	// 验证 store 已落库。
	key := tpslKey(1, "BTC_USDT_PERP", "long")
	loaded, _ := s.tpslStore.LoadAll()
	if _, ok := loaded[1][key]; !ok {
		t.Fatal("tpsl not persisted to store")
	}

	// 第二个 Server 实例：共用同一 store（模拟重启）。
	s2 := &Server{
		log:       s.log,
		tpsl:      make(map[int64]map[string]TPState),
		tpslStore: s.tpslStore,
	}
	loaded, _ = s.tpslStore.LoadAll()
	s2.tpsl = loaded

	// 验证 decorateWithTPSL 恢复。
	pos := []futures.Position{{UserID: 1, Symbol: "BTC_USDT_PERP", Side: futures.Long}}
	out := s2.decorateWithTPSL(pos)
	if len(out) != 1 || out[0].TP == nil || *out[0].TP != 55000 || out[0].SL == nil || *out[0].SL != 48000 {
		t.Fatalf("TP-SL not restored: %+v", out)
	}
}

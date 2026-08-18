package bot

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"
)

// mockExec 记录 bot 代用户下单的调用，便于验证 F1(client_oid) / F4(userToken) 绑定。
type mockExec struct {
	mu      sync.Mutex
	calls   []execCall
	orderID int64
}

type execCall struct {
	userToken string
	market    string
	symbol    string
	side      string
	price     float64
	qty       float64
	clientOID string
}

func (m *mockExec) Execute(_ context.Context, userToken, market, symbol, side string, price, qty float64, clientOID string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.orderID++
	m.calls = append(m.calls, execCall{userToken, market, symbol, side, price, qty, clientOID})
	return "ex-ord-1", nil
}

func newTestService() (*Service, *mockExec, *MemStore) {
	store := NewMemStore()
	mock := &mockExec{}
	svc := NewService(store, nil, mock, Config{}, nil)
	return svc, mock, store
}

func validStrategy(uid int64) *BotStrategy {
	return &BotStrategy{
		UserID:    uid,
		Name:      "demo",
		Market:    MarketSpot,
		Symbol:    "BTC_USDT",
		Side:      "buy",
		Type:      StrategyDCA,
		UserToken: "tok-user-1",
		Params:    BotParams{OrderAmount: 100, DCAIntervalSec: 60, DCAAmount: 100},
	}
}

// F5：非法参数应被 CreateStrategy 拒绝。
func TestCreateStrategyF5Validation(t *testing.T) {
	svc, _, _ := newTestService()

	cases := []struct {
		name   string
		mutate func(*BotStrategy)
	}{
		{"bad market", func(s *BotStrategy) { s.Market = "forex" }},
		{"bad side", func(s *BotStrategy) { s.Side = "hold" }},
		{"non-positive amount", func(s *BotStrategy) { s.Params.OrderAmount = 0 }},
		{"unknown type", func(s *BotStrategy) { s.Type = "magic" }},
		{"dca zero interval", func(s *BotStrategy) { s.Params.DCAIntervalSec = 0 }},
		{"grid lower>=upper", func(s *BotStrategy) {
			s.Type = StrategyGrid
			s.Params.GridLower = 100
			s.Params.GridUpper = 100
		}},
		{"ma long<=short", func(s *BotStrategy) {
			s.Type = StrategyMA
			s.Params.MAShort = 30
			s.Params.MALong = 10
		}},
	}
	for _, c := range cases {
		st := validStrategy(1)
		c.mutate(st)
		if err := svc.CreateStrategy(st); err != ErrInvalidParam {
			t.Errorf("%s: want ErrInvalidParam, got %v", c.name, err)
		}
	}
}

// F4：非本人不得启动/停止策略。
func TestStartStopF4Owner(t *testing.T) {
	svc, _, _ := newTestService()
	st := validStrategy(1)
	if err := svc.CreateStrategy(st); err != nil {
		t.Fatalf("create: %v", err)
	}
	// 他人启动应被拒绝。
	if err := svc.StartStrategy(2, st.ID); err != ErrNotOwner {
		t.Errorf("StartStrategy by other: want ErrNotOwner, got %v", err)
	}
	// 本人启动成功。
	if err := svc.StartStrategy(1, st.ID); err != nil {
		t.Errorf("StartStrategy by owner: unexpected err %v", err)
	}
	// 他人停止应被拒绝。
	if err := svc.StopStrategy(2, st.ID); err != ErrNotOwner {
		t.Errorf("StopStrategy by other: want ErrNotOwner, got %v", err)
	}
}

// F1：同一策略每轮 tick 使用幂等 client_oid = bot:<id>:<round>，下游去重。
func TestTickF1IdempotentKey(t *testing.T) {
	svc, mock, _ := newTestService()
	st := validStrategy(1)
	if err := svc.CreateStrategy(st); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.StartStrategy(1, st.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	s := &st.ID
	got, _ := svc.store.GetStrategy(*s)
	// 手动触发两轮 tick。
	for i := 0; i < 2; i++ {
		if err := svc.tick(got); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	calls := mock.calls
	if len(calls) != 2 {
		t.Fatalf("want 2 order calls, got %d", len(calls))
	}
	// F4：代下单须携带用户授权 token。
	if calls[0].userToken != "tok-user-1" {
		t.Errorf("F4: order should carry user token, got %q", calls[0].userToken)
	}
	// F1：client_oid 形如 bot:<id>:0 / bot:<id>:1，下游据此外部去重。
	want0 := "bot:" + itoa(st.ID) + ":0"
	want1 := "bot:" + itoa(st.ID) + ":1"
	if calls[0].clientOID != want0 || calls[1].clientOID != want1 {
		t.Errorf("F1: client_oid mismatch: %q / %q", calls[0].clientOID, calls[1].clientOID)
	}
	// 越仓控制：MaxPosition 限制累计下单额。
	if st.Params.MaxPosition > 0 { /* 本用例不设上限 */ }
}

// F1/F4 综合：越仓上限触发后不再下单。
func TestTickMaxPositionGuard(t *testing.T) {
	svc, mock, _ := newTestService()
	st := validStrategy(1)
	st.Params.MaxPosition = 150 // 两轮后 (2*100=200) 超过上限，第三轮应被拦。
	if err := svc.CreateStrategy(st); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.StartStrategy(1, st.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	got, _ := svc.store.GetStrategy(st.ID)
	for i := 0; i < 3; i++ {
		_ = svc.tick(got)
	}
	if len(mock.calls) != 2 {
		t.Errorf("MaxPosition guard: want 2 calls, got %d", len(mock.calls))
	}
}

// Run 后台循环在 tick 内应忽略错误且不 panic（用 mock 验证至少驱动可启动策略）。
func TestRunLoopDrivesActive(t *testing.T) {
	svc, mock, _ := newTestService()
	st := validStrategy(1)
	if err := svc.CreateStrategy(st); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.StartStrategy(1, st.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	svc.cfg.TickInterval = 20 * time.Millisecond
	go svc.Run(ctx)
	time.Sleep(120 * time.Millisecond)
	cancel()
	if len(mock.calls) < 2 {
		t.Errorf("Run should have driven multiple ticks, got %d", len(mock.calls))
	}
}

// F5：行情非法（NaN/Inf/0/负）时本轮下单应被拒绝，不触发任何订单。
func TestTickF5InvalidPrice(t *testing.T) {
	for _, p := range []float64{math.NaN(), math.Inf(1), 0, -1} {
		store := NewMemStore()
		mock := &mockExec{}
		svc := NewService(store, &badPrice{p: p}, mock, Config{}, nil)
		st := validStrategy(1)
		if err := svc.CreateStrategy(st); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := svc.StartStrategy(1, st.ID); err != nil {
			t.Fatalf("start: %v", err)
		}
		got, _ := svc.store.GetStrategy(st.ID)
		if err := svc.tick(got); err == nil {
			t.Errorf("price %v: expected error, got nil", p)
		}
		if len(mock.calls) != 0 {
			t.Errorf("price %v: no order should be placed, calls=%d", p, len(mock.calls))
		}
	}
}

// badPrice 返回指定（可能非法的）行情价，用于 F5 校验。
type badPrice struct{ p float64 }

func (b *badPrice) Price(_, _ string) (float64, error) { return b.p, nil }

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	buf := make([]byte, 0, 20)
	for i > 0 {
		buf = append([]byte{byte('0' + i%10)}, buf...)
		i /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

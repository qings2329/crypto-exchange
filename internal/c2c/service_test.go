package c2c

import (
	"testing"
)

func TestCreateValidation(t *testing.T) {
	svc := NewService(NewMemStore())
	cases := []struct {
		name          string
		side          Side
		coin          string
		amount, price float64
		wantErr       bool
	}{
		{"合法买入", SideBuy, "USDT", 100, 7.2, false},
		{"合法卖出", SideSell, "BTC", 0.5, 398000, false},
		{"非法方向", Side("hold"), "USDT", 100, 7.2, true},
		{"空币种", SideBuy, "", 100, 7.2, true},
		{"数量为0", SideBuy, "USDT", 0, 7.2, true},
		{"负数量", SideBuy, "USDT", -1, 7.2, true},
		{"价格为0", SideBuy, "USDT", 100, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, err := svc.Create(1, tc.side, tc.coin, tc.amount, tc.price, "")
			if (err != nil) != tc.wantErr {
				t.Fatalf("Create err = %v, wantErr=%v", err, tc.wantErr)
			}
			if err == nil {
				if o.UserID != 1 || o.Total != tc.amount*tc.price {
					t.Fatalf("unexpected order: %+v", o)
				}
				if o.Status != StatusOpen {
					t.Fatalf("new order status = %v, want open", o.Status)
				}
			}
		})
	}
}

func TestTransitionStateMachine(t *testing.T) {
	svc := NewService(NewMemStore())
	o, err := svc.Create(1, SideBuy, "USDT", 100, 7.2, "")
	if err != nil {
		t.Fatal(err)
	}

	// open -> locked
	locked, err := svc.Freeze(o.ID)
	if err != nil || locked.Status != StatusLocked {
		t.Fatalf("freeze = %v, %v", locked, err)
	}

	// 重复冻结应报 ErrBadTransition（状态已是 locked，非 open）
	if _, err := svc.Freeze(o.ID); err != ErrBadTransition {
		t.Fatalf("second freeze err = %v, want ErrBadTransition", err)
	}

	// locked -> open
	released, err := svc.Release(o.ID)
	if err != nil || released.Status != StatusOpen {
		t.Fatalf("release = %v, %v", released, err)
	}

	// open -> completed
	done, err := svc.Complete(o.ID)
	if err != nil || done.Status != StatusCompleted {
		t.Fatalf("complete = %v, %v", done, err)
	}
}

func TestListFilterAndPage(t *testing.T) {
	svc := NewService(NewMemStore())
	for i := 0; i < 5; i++ {
		if _, err := svc.Create(10, SideBuy, "USDT", float64(i+1), 7.0, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Create(20, SideBuy, "USDT", 9, 7.0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(10, SideSell, "BTC", 1, 398000, ""); err != nil {
		t.Fatal(err)
	}

	// 按用户过滤
	items, total, err := svc.List(OrderFilter{UserID: 10}, 10, 0)
	if err != nil || total != 6 || len(items) != 6 {
		t.Fatalf("filter by user: items=%d total=%d err=%v", len(items), total, err)
	}

	// 按方向+币种过滤 + 分页
	items, total, err = svc.List(OrderFilter{Side: SideBuy, Coin: "USDT"}, 2, 2)
	if err != nil || total != 6 || len(items) != 2 {
		t.Fatalf("filter + page: items=%d total=%d err=%v", len(items), total, err)
	}

	// 非法方向过滤
	if _, _, err := svc.List(OrderFilter{Side: Side("bad")}, 10, 0); err != ErrInvalidSide {
		t.Fatalf("invalid side filter err = %v", err)
	}
}

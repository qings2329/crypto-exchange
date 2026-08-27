package bot

import (
	"testing"
)

func TestCalcGridConfig(t *testing.T) {
	p := BotParams{GridLower: 90, GridUpper: 110, GridNum: 10}
	cfg, err := CalcGridConfig(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Lower != 90 || cfg.Upper != 110 || cfg.Num != 10 {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if cfg.StepSize != 2.0 {
		t.Fatalf("expected step 2.0, got %f", cfg.StepSize)
	}
}

func TestCalcGridConfigInvalid(t *testing.T) {
	_, err := CalcGridConfig(BotParams{GridLower: 100, GridUpper: 90, GridNum: 10})
	if err == nil {
		t.Fatal("expected error for lower >= upper")
	}
	_, err = CalcGridConfig(BotParams{GridLower: 90, GridUpper: 110, GridNum: 0})
	if err == nil {
		t.Fatal("expected error for num <= 0")
	}
}

func TestCalcGridConfigMinTwo(t *testing.T) {
	p := BotParams{GridLower: 90, GridUpper: 110, GridNum: 1}
	cfg, err := CalcGridConfig(p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Num != 2 {
		t.Fatalf("expected num=2, got %d", cfg.Num)
	}
}

func TestGridLevels(t *testing.T) {
	cfg := GridConfig{Lower: 90, Upper: 110, Num: 4, StepSize: 5}
	levels := cfg.GridLevels()
	if len(levels) != 5 {
		t.Fatalf("expected 5 levels, got %d", len(levels))
	}
	expected := []float64{90, 95, 100, 105, 110}
	for i, v := range expected {
		if levels[i] != v {
			t.Fatalf("level[%d] expected %f, got %f", i, v, levels[i])
		}
	}
}

func TestInitGridState(t *testing.T) {
	cfg := GridConfig{Lower: 90, Upper: 110, Num: 4, StepSize: 5}
	state := InitGridState(cfg, 100, 1000)
	if len(state.Levels) != 5 {
		t.Fatalf("expected 5 levels, got %d", len(state.Levels))
	}
	for _, lv := range state.Levels {
		switch {
		case lv.Price <= 100:
			if lv.Side != "buy" {
				t.Fatalf("price %f should be buy, got %s", lv.Price, lv.Side)
			}
		case lv.Price > 100:
			if lv.Side != "sell" {
				t.Fatalf("price %f should be sell, got %s", lv.Price, lv.Side)
			}
		}
	}
}

func TestTickGridInitialPlacement(t *testing.T) {
	cfg := GridConfig{Lower: 90, Upper: 110, Num: 4, StepSize: 5}
	state := InitGridState(cfg, 100, 1000)

	orders := TickGrid(state, cfg, 100, 1000, 0)
	if len(orders) != 5 {
		t.Fatalf("expected 5 initial orders, got %d: %+v", len(orders), orders)
	}
	if !state.Initialized {
		t.Fatal("expected Initialized=true after first tick")
	}
	for _, lv := range state.Levels {
		if !lv.Placed {
			t.Fatalf("level at %f should be placed", lv.Price)
		}
	}
	buys, sells := 0, 0
	for _, o := range orders {
		if o.Side == "buy" {
			buys++
		} else {
			sells++
		}
	}
	if buys != 3 || sells != 2 {
		t.Fatalf("expected 3 buys 2 sells, got %d buys %d sells", buys, sells)
	}
}

func TestTickGridBuyFillTriggersSell(t *testing.T) {
	cfg := GridConfig{Lower: 90, Upper: 110, Num: 4, StepSize: 5}
	state := InitGridState(cfg, 100, 1000)

	// Tick 1: place initial orders at price=100
	TickGrid(state, cfg, 100, 1000, 0)

	// Tick 2: price drops to 95 → buy at95 crosses down (prev=100 >=95, curr=95 <95? No, 95 is not <95)
	// Actually need price to go STRICTLY below the buy level to trigger.
	// Buy at95: prev=100 >=95 AND curr <95. So price must drop to 94 or below.
	// But buy at100: prev=100 >=100 AND curr=95 <100 → YES, this fills.
	orders := TickGrid(state, cfg, 95, 1000, 0)

	// Buy at100 should fill (prev=100 >=100, curr=95<100)
	// Sell at105 should be placed as profit-take
	found := false
	for _, o := range orders {
		if o.Side == "sell" && o.Price == 105 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected sell at105 after buy fill at100, got orders: %+v", orders)
	}
}

func TestTickGridSellFillTriggersBuy(t *testing.T) {
	cfg := GridConfig{Lower: 90, Upper: 110, Num: 4, StepSize: 5}
	state := InitGridState(cfg, 100, 1000)

	// Tick 1: place initial orders at price=100
	TickGrid(state, cfg, 100, 1000, 0)

	// Tick 2: price rises to 105 → sell at105 crosses up (prev=100 <=105, curr=105>105? No)
	// Sell at105: prev=100 <=105 AND curr=105>105? No.
	// Sell at110: prev=100 <=110 AND curr=105>110? No.
	// Hmm, none fill with strict >. Need price to go above 105.
	// Sell at105: prev=100 <=105 AND curr=106 >105 → YES
	orders := TickGrid(state, cfg, 106, 1000, 0)

	found := false
	for _, o := range orders {
		if o.Side == "buy" && o.Price == 100 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected buy at100 after sell fill at105, got orders: %+v", orders)
	}
}

func TestTickGridPositionTracking(t *testing.T) {
	cfg := GridConfig{Lower: 90, Upper: 110, Num: 4, StepSize: 5}
	state := InitGridState(cfg, 100, 1000)

	if state.Position != 0 {
		t.Fatalf("expected initial position 0, got %f", state.Position)
	}

	// Tick 1: init
	TickGrid(state, cfg, 100, 1000, 0)

	// Tick 2: price drops to 94 → buy at95 fills (prev=100>=95, curr=94<95), buy at100 fills (prev=100>=100, curr=94<100)
	TickGrid(state, cfg, 94, 1000, 0)

	if state.Position <= 0 {
		t.Fatalf("expected positive position after buy fills, got %f", state.Position)
	}
}

func TestTickGridMaxPosition(t *testing.T) {
	cfg := GridConfig{Lower: 90, Upper: 110, Num: 4, StepSize: 5}
	state := InitGridState(cfg, 100, 1000)

	TickGrid(state, cfg, 100, 1000, 0)

	// Price drops to 88: buys at90,95,100 fill → position increases
	TickGrid(state, cfg, 88, 1000, 0)

	// Record position after fills
	posAfterFills := state.Position

	// Now set maxPosition to limit. Price rises back, some sells fill.
	// After sells, position decreases. Then price drops again.
	// The maxPosition guard should prevent new buy orders.
	state2 := InitGridState(cfg, 100, 1000)
	TickGrid(state2, cfg, 100, 1000, 0)

	// Set maxPosition = 500 (very small). Price drops to 88, buys fill, position*price will exceed.
	TickGrid(state2, cfg, 88, 1000, 500)

	// After price drops and buys fill, position exceeds maxPosition.
	// On next tick, no new buy orders should be placed.
	prevPos := state2.Position
	orders := TickGrid(state2, cfg, 85, 1000, 500)

	// Verify no new buy orders placed
	for _, o := range orders {
		if o.Side == "buy" {
			t.Fatalf("expected no buy orders when over maxPosition, got buy at %f", o.Price)
		}
	}
	// Position shouldn't increase from new buys (may change from sells)
	_ = prevPos
	_ = posAfterFills
}

func TestTickGridInvalidPrice(t *testing.T) {
	cfg := GridConfig{Lower: 90, Upper: 110, Num: 4, StepSize: 5}
	state := InitGridState(cfg, 100, 1000)

	orders := TickGrid(state, cfg, 0, 1000, 0)
	if orders != nil {
		t.Fatalf("expected nil for zero price, got %+v", orders)
	}
	orders = TickGrid(state, cfg, -1, 1000, 0)
	if orders != nil {
		t.Fatalf("expected nil for negative price, got %+v", orders)
	}
}

func TestTickGridPnLTracking(t *testing.T) {
	cfg := GridConfig{Lower: 90, Upper: 110, Num: 4, StepSize: 5}
	state := InitGridState(cfg, 100, 1000)

	TickGrid(state, cfg, 100, 1000, 0)

	// Price drops to94: buy at95 and100 fill
	TickGrid(state, cfg, 94, 1000, 0)
	if state.TradeCnt < 1 {
		t.Fatalf("expected at least 1 trade, got %d", state.TradeCnt)
	}
	if state.PnL >= 0 {
		t.Fatalf("expected negative PnL after buys, got %f", state.PnL)
	}
}

func TestTickGridStablePrice(t *testing.T) {
	cfg := GridConfig{Lower: 90, Upper: 110, Num: 4, StepSize: 5}
	state := InitGridState(cfg, 100, 1000)

	TickGrid(state, cfg, 100, 1000, 0)

	// Same price: no crossings, no fills
	orders := TickGrid(state, cfg, 100, 1000, 0)
	if len(orders) != 0 {
		t.Fatalf("expected no orders on stable price, got %+v", orders)
	}
}

func TestGridSummary(t *testing.T) {
	cfg := GridConfig{Lower: 90, Upper: 110, Num: 4, StepSize: 5}
	state := InitGridState(cfg, 100, 1000)
	TickGrid(state, cfg, 100, 1000, 0)
	summary := GridSummary(state, cfg)

	if summary["grid_num"] != 4 {
		t.Fatalf("expected grid_num=4, got %v", summary["grid_num"])
	}
	if summary["pending_buys"].(int) != 3 {
		t.Fatalf("expected 3 pending buys, got %v", summary["pending_buys"])
	}
	if summary["pending_sells"].(int) != 2 {
		t.Fatalf("expected 2 pending sells, got %v", summary["pending_sells"])
	}
}

func TestEstimateGridPnL(t *testing.T) {
	cfg := GridConfig{Lower: 90, Upper: 110, Num: 10, StepSize: 2}
	pnl := EstimateGridPnL(cfg, 1000, 100)
	if pnl <= 0 {
		t.Fatalf("expected positive PnL estimate, got %f", pnl)
	}
}

func TestTickGridMultiTickSimulation(t *testing.T) {
	cfg := GridConfig{Lower: 95, Upper: 105, Num: 5, StepSize: 2}
	state := InitGridState(cfg, 100, 500)

	// Tick 1: init at100
	TickGrid(state, cfg, 100, 500, 0)

	// Tick 2: price drops to 97 → buy at100 fills (prev=100>=100, curr=97<100)
	TickGrid(state, cfg, 97, 500, 0)

	// Tick 3: price rises to103 → sell at102 fills (prev=97<=102, curr=103>102) [profit-take from buy at100→sell at102]
	TickGrid(state, cfg, 103, 500, 0)

	// Tick 4: price drops to 98
	TickGrid(state, cfg, 98, 500, 0)

	if state.TradeCnt == 0 {
		t.Fatal("expected at least 1 trade in simulation")
	}
	t.Logf("simulation: trades=%d position=%.4f pnl=%.2f",
		state.TradeCnt, state.Position, state.PnL)
}

func TestTickGridBoundaryPrices(t *testing.T) {
	cfg := GridConfig{Lower: 90, Upper: 110, Num: 4, StepSize: 5}
	state := InitGridState(cfg, 100, 1000)

	TickGrid(state, cfg, 100, 1000, 0)

	// Price drops below lower boundary → buy at90 fills (prev=100>=90, curr=89<90)
	TickGrid(state, cfg, 89, 1000, 0)

	// Price rises above upper boundary → sell at110 fills (prev=89<=110, curr=111>110)
	TickGrid(state, cfg, 111, 1000, 0)

	for _, lv := range state.Levels {
		if lv.Price == 90 && !lv.Filled {
			t.Fatal("level at lower boundary should be filled")
		}
		if lv.Price == 110 && !lv.Filled {
			t.Fatal("level at upper boundary should be filled")
		}
	}
}

func TestTickGridOrderReasons(t *testing.T) {
	cfg := GridConfig{Lower: 90, Upper: 110, Num: 4, StepSize: 5}
	state := InitGridState(cfg, 100, 1000)

	orders := TickGrid(state, cfg, 100, 1000, 0)
	for _, o := range orders {
		if o.Reason == "" {
			t.Fatalf("order at level %d should have a reason", o.Level)
		}
	}
}

func TestGridStatePersistence(t *testing.T) {
	store := NewMemStore()
	st := &BotStrategy{
		UserID: 1, Name: "grid-test", Market: MarketSpot, Symbol: "BTC-USDT",
		Side: "buy", Type: StrategyGrid, UserToken: "test-token",
		Params: BotParams{GridLower: 90, GridUpper: 110, GridNum: 4, OrderAmount: 1000},
	}
	if err := store.CreateStrategy(st); err != nil {
		t.Fatal(err)
	}
	cfg, _ := CalcGridConfig(st.Params)
	st.GridState = InitGridState(cfg, 100, 1000)
	TickGrid(st.GridState, cfg, 100, 1000, 0)
	if err := store.UpdateStrategy(st); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetStrategy(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.GridState == nil {
		t.Fatal("expected GridState to be persisted")
	}
	if !got.GridState.Initialized {
		t.Fatal("expected Initialized=true after persistence")
	}
	if len(got.GridState.Levels) != 5 {
		t.Fatalf("expected 5 levels, got %d", len(got.GridState.Levels))
	}
}

func TestServiceTickGrid(t *testing.T) {
	store := NewMemStore()
	price := &MockPrice{P: 100}
	exec := &mockExec{}
	svc := NewService(store, price, exec, Config{TickInterval: 0}, nil)

	st := &BotStrategy{
		UserID: 1, Name: "grid-integration", Market: MarketSpot, Symbol: "BTC-USDT",
		Side: "buy", Type: StrategyGrid, UserToken: "test-token",
		Params: BotParams{GridLower: 90, GridUpper: 110, GridNum: 4, OrderAmount: 1000},
	}
	if err := svc.CreateStrategy(st); err != nil {
		t.Fatal(err)
	}
	if err := svc.StartStrategy(1, st.ID); err != nil {
		t.Fatal(err)
	}

	if err := svc.Tick(nil, st.ID); err != nil {
		t.Fatal(err)
	}

	got, _ := store.GetStrategy(st.ID)
	if got.GridState == nil {
		t.Fatal("expected GridState after tick")
	}
	if !got.GridState.Initialized {
		t.Fatal("expected Initialized=true")
	}
	if len(got.GridState.Levels) != 5 {
		t.Fatalf("expected 5 levels, got %d", len(got.GridState.Levels))
	}
}

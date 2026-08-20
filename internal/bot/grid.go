package bot

import (
	"fmt"
	"math"
	"sort"
)

// GridLevel 代表一个网格价格级别及其挂单状态。
type GridLevel struct {
	Price     float64 `json:"price"`      // 网格价格
	Qty       float64 `json:"qty"`        // 挂单数量
	Side      string  `json:"side"`       // buy / sell
	Placed    bool    `json:"placed"`     // 是否已挂单
	OrderID   string  `json:"order_id"`   // 交易所订单号
	Filled    bool    `json:"filled"`     // 是否已成交
	FillPrice float64 `json:"fill_price"` // 成交价
}

// GridState 网格策略的运行时状态（可序列化到 BotStrategy 的 JSON 字段）。
type GridState struct {
	Levels      []*GridLevel `json:"levels"`       // 所有网格级别
	Position    float64      `json:"position"`     // 当前持仓（正=多，负=空）
	PnL         float64      `json:"pnl"`          // 已实现盈亏（计价）
	TradeCnt    int          `json:"trade_count"`  // 成交次数
	LastPrice   float64      `json:"last_price"`   // 上次 tick 时的价格
	PrevPrice   float64      `json:"prev_price"`   // 上上次 tick 的价格（用于穿越检测）
	Initialized bool         `json:"initialized"`  // 是否已完成初次挂单
}

// GridConfig 网格策略的计算参数。
type GridConfig struct {
	Lower    float64 // 区间下沿
	Upper    float64 // 区间上沿
	Num      int     // 网格数
	StepSize float64 // 每格间距（自动计算）
}

// CalcGridConfig 从 BotParams 计算网格配置。
func CalcGridConfig(p BotParams) (GridConfig, error) {
	if p.GridLower >= p.GridUpper {
		return GridConfig{}, fmt.Errorf("grid: lower must be < upper")
	}
	if p.GridNum <= 0 {
		return GridConfig{}, fmt.Errorf("grid: grid_num must be > 0")
	}
	num := p.GridNum
	if num < 2 {
		num = 2
	}
	step := (p.GridUpper - p.GridLower) / float64(num)
	return GridConfig{
		Lower:    p.GridLower,
		Upper:    p.GridUpper,
		Num:      num,
		StepSize: step,
	}, nil
}

// GridLevels 计算所有网格价格（升序排列）。
func (c GridConfig) GridLevels() []float64 {
	levels := make([]float64, c.Num+1)
	for i := 0; i <= c.Num; i++ {
		levels[i] = c.Lower + float64(i)*c.StepSize
	}
	return levels
}

// InitGridState 初始化网格状态（不含挂单，等首次 TickGrid 触发挂单）。
func InitGridState(cfg GridConfig, currentPrice float64, orderAmount float64) *GridState {
	levels := cfg.GridLevels()
	state := &GridState{
		Levels:    make([]*GridLevel, 0, len(levels)),
		LastPrice: currentPrice,
		PrevPrice: currentPrice,
	}
	for _, price := range levels {
		qty := orderAmount / maxfGrid(price, 1e-8)
		side := "buy"
		if price > currentPrice {
			side = "sell"
		}
		state.Levels = append(state.Levels, &GridLevel{
			Price: price,
			Qty:   qty,
			Side:  side,
		})
	}
	return state
}

// TickGrid 执行一轮网格策略。
//
// 分两阶段：
//  1. 初次启动（Initialized == false）：在当前价格两侧的所有网格级别放置初始挂单。
//  2. 运行中（Initialized == true）：用前一 tick 价格与当前价格的穿越关系检测成交，
//     成交后在其相邻级别放置反向止盈单。
//
// 穿越检测保证价格恰好等于挂单价时不会误触发，只有价格真正穿越才会成交。
func TickGrid(state *GridState, cfg GridConfig, currentPrice float64, orderAmount float64, maxPosition float64) []GridOrder {
	if math.IsNaN(currentPrice) || math.IsInf(currentPrice, 0) || currentPrice <= 0 {
		return nil
	}
	var orders []GridOrder

	if !state.Initialized {
		for i, lv := range state.Levels {
			if lv.Filled {
				continue
			}
			side := "buy"
			if lv.Price > currentPrice {
				side = "sell"
			}
			lv.Side = side
			lv.Qty = orderAmount / maxfGrid(lv.Price, 1e-8)
			orders = append(orders, GridOrder{
				Level:  i,
				Price:  lv.Price,
				Qty:    lv.Qty,
				Side:   side,
				Reason: "grid initial " + side,
			})
			lv.Placed = true
			lv.OrderID = fmt.Sprintf("grid:%d:init", i)
		}
		state.Initialized = true
		state.PrevPrice = currentPrice
		state.LastPrice = currentPrice
		return orders
	}

	prevPrice := state.PrevPrice
	for i, lv := range state.Levels {
		if !lv.Placed || lv.Filled {
			continue
		}
		// Buy: prevPrice >= lv.Price AND currentPrice < lv.Price → 向下穿越
		if lv.Side == "buy" && prevPrice >= lv.Price && currentPrice < lv.Price {
			lv.Filled = true
			lv.FillPrice = lv.Price
			lv.Placed = false
			state.PnL -= orderAmount
			state.Position += lv.Qty
			state.TradeCnt++
			if i+1 < len(state.Levels) && !state.Levels[i+1].Filled {
				nlv := state.Levels[i+1]
				sellQty := orderAmount / maxfGrid(nlv.Price, 1e-8)
				orders = append(orders, GridOrder{
					Level:  i + 1,
					Price:  nlv.Price,
					Qty:    sellQty,
					Side:   "sell",
					Reason: "grid profit-take after buy fill",
				})
				nlv.Placed = true
				nlv.Side = "sell"
				nlv.Qty = sellQty
				nlv.OrderID = fmt.Sprintf("grid:%d:%d", i+1, state.TradeCnt)
			}
		}
		// Sell: prevPrice <= lv.Price AND currentPrice > lv.Price → 向上穿越
		if lv.Side == "sell" && prevPrice <= lv.Price && currentPrice > lv.Price {
			lv.Filled = true
			lv.FillPrice = lv.Price
			lv.Placed = false
			state.PnL += lv.Qty * lv.Price
			state.Position -= lv.Qty
			state.TradeCnt++
			if i-1 >= 0 && !state.Levels[i-1].Filled {
				nlv := state.Levels[i-1]
				buyQty := orderAmount / maxfGrid(nlv.Price, 1e-8)
				orders = append(orders, GridOrder{
					Level:  i - 1,
					Price:  nlv.Price,
					Qty:    buyQty,
					Side:   "buy",
					Reason: "grid profit-take after sell fill",
				})
				nlv.Placed = true
				nlv.Side = "buy"
				nlv.Qty = buyQty
				nlv.OrderID = fmt.Sprintf("grid:%d:%d", i-1, state.TradeCnt)
			}
		}
	}

	state.PrevPrice = state.LastPrice
	state.LastPrice = currentPrice

	if maxPosition > 0 && state.Position*currentPrice >= maxPosition {
		filtered := orders[:0]
		for _, o := range orders {
			if o.Side != "buy" {
				filtered = append(filtered, o)
			}
		}
		orders = filtered
	}

	sort.Slice(orders, func(i, j int) bool { return orders[i].Level < orders[j].Level })
	return orders
}

// GridOrder 是网格策略本轮产出的下单指令。
type GridOrder struct {
	Level  int     `json:"level"`  // 网格级别索引
	Price  float64 `json:"price"`  // 下单价
	Qty    float64 `json:"qty"`    // 数量
	Side   string  `json:"side"`   // buy / sell
	Reason string  `json:"reason"` // 下单原因
}

// GridSummary 返回网格当前状态的摘要（供 API 返回）。
func GridSummary(state *GridState, cfg GridConfig) map[string]interface{} {
	filledBuys, filledSells, pendingBuys, pendingSells := 0, 0, 0, 0
	for _, lv := range state.Levels {
		if lv.Filled {
			if lv.Side == "buy" {
				filledBuys++
			} else {
				filledSells++
			}
		} else if lv.Placed {
			if lv.Side == "buy" {
				pendingBuys++
			} else {
				pendingSells++
			}
		}
	}
	return map[string]interface{}{
		"lower":         cfg.Lower,
		"upper":         cfg.Upper,
		"grid_num":      cfg.Num,
		"step_size":     cfg.StepSize,
		"position":      state.Position,
		"pnl":           state.PnL,
		"trade_count":   state.TradeCnt,
		"filled_buys":   filledBuys,
		"filled_sells":  filledSells,
		"pending_buys":  pendingBuys,
		"pending_sells": pendingSells,
		"last_price":    state.LastPrice,
	}
}

// EstimateGridPnL 估算网格策略的理论最大收益（假设价格在区间内均匀震荡）。
func EstimateGridPnL(cfg GridConfig, orderAmount float64, avgPrice float64) float64 {
	if avgPrice <= 0 {
		return 0
	}
	perGridProfit := orderAmount * cfg.StepSize / avgPrice
	return perGridProfit * float64(cfg.Num) * 2
}

func maxfGrid(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

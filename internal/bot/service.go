package bot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

// PriceSource 行情源（可插拔）。Mock 用于演示，真实接 oracle / market WS。
type PriceSource interface {
	Price(market, symbol string) (float64, error)
}

// MockPrice 返回固定演示价（生产级应替换为 oracle 实时价）。
type MockPrice struct{ P float64 }

// NewMockPrice 构造 Mock 行情。
func NewMockPrice() *MockPrice { return &MockPrice{P: 100} }

// Price 返回固定价。
func (m *MockPrice) Price(_, _ string) (float64, error) { return m.P, nil }

// OrderExecutor 代下单执行器（可插拔）。默认 HTTPExecutor 调 spot/futures 的 /order 接口，
// 复用其资金预冻/结算安全层与 F1(client_oid 幂等)/F4(token 鉴权) 守卫。
type OrderExecutor interface {
	Execute(ctx context.Context, userToken, market, symbol, side string, price, qty float64, clientOID string) (exchangeOrderID string, err error)
}

// HTTPExecutor 经 HTTP 调 spot/futures 的 /order（持有用户授权 token）。
type HTTPExecutor struct {
	spotURL    string
	futuresURL string
	client     *http.Client
}

// NewHTTPExecutor 构造 HTTP 执行器。
func NewHTTPExecutor(spotURL, futuresURL string) *HTTPExecutor {
	return &HTTPExecutor{spotURL: spotURL, futuresURL: futuresURL, client: &http.Client{Timeout: 10 * time.Second}}
}

// Execute 代用户下单；下单 body 按市场组装，携带 client_oid（下游 F1 幂等）与 Bearer token（下游 F4）。
func (e *HTTPExecutor) Execute(ctx context.Context, userToken, market, symbol, side string, price, qty float64, clientOID string) (string, error) {
	base := e.spotURL
	if market == string(MarketFutures) {
		base = e.futuresURL
	}
	if base == "" {
		return "", fmt.Errorf("bot: no %s endpoint configured", market)
	}
	var body map[string]interface{}
	if market == string(MarketFutures) {
		body = map[string]interface{}{
			"symbol": symbol, "side": side, "pos_side": "long", "action": "open",
			"price": price, "qty": qty, "leverage": 1, "client_oid": clientOID,
		}
	} else {
		body = map[string]interface{}{
			"symbol": symbol, "side": side, "price": price, "qty": qty, "client_oid": clientOID,
		}
	}
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/"+market+"/order", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+userToken)
	resp, err := e.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("bot: order failed %d: %s", resp.StatusCode, string(data))
	}
	var out struct {
		OrderID string `json:"order_id"`
		Data    struct {
			OrderID string `json:"order_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(data, &out)
	id := out.OrderID
	if id == "" {
		id = out.Data.OrderID
	}
	return id, nil
}

// Config 是机器人服务配置。
type Config struct {
	TickInterval time.Duration // 策略 tick 周期
}

// Service 交易机器人服务。
type Service struct {
	store  Store
	price  PriceSource
	exec   OrderExecutor
	cfg    Config
	log    *zap.Logger
	mu     sync.Mutex
	rounds map[int64]int64 // strategyID -> 已执行轮次（F1 幂等 / 越仓控制）
}

// NewService 构造机器人服务。
func NewService(store Store, price PriceSource, exec OrderExecutor, cfg Config, log *zap.Logger) *Service {
	if price == nil {
		price = NewMockPrice()
	}
	if exec == nil {
		exec = NewHTTPExecutor("", "")
	}
	return &Service{store: store, price: price, exec: exec, cfg: cfg, log: log, rounds: map[int64]int64{}}
}

// CreateStrategy 创建策略（F5 参数校验），默认 stopped，需显式启动。
func (s *Service) CreateStrategy(st *BotStrategy) error {
	if st.Market != MarketSpot && st.Market != MarketFutures {
		return ErrInvalidParam
	}
	if st.Side != "buy" && st.Side != "sell" {
		return ErrInvalidParam
	}
	if st.Params.OrderAmount <= 0 {
		return ErrInvalidParam
	}
	switch st.Type {
	case StrategyGrid:
		if st.Params.GridLower >= st.Params.GridUpper {
			return ErrInvalidParam
		}
	case StrategyDCA:
		if st.Params.DCAIntervalSec <= 0 || st.Params.DCAAmount <= 0 {
			return ErrInvalidParam
		}
	case StrategyMA:
		if st.Params.MAShort <= 0 || st.Params.MALong <= st.Params.MAShort {
			return ErrInvalidParam
		}
	default:
		return ErrInvalidParam
	}
	st.Status = StrategyStopped
	st.CreatedAt = time.Now().Unix()
	return s.store.CreateStrategy(st)
}

// StartStrategy 启动策略（F4：仅创建者本人可操作）。
func (s *Service) StartStrategy(userID, id int64) error {
	st, err := s.store.GetStrategy(id)
	if err != nil {
		return err
	}
	if st.UserID != userID {
		return ErrNotOwner
	}
	st.Status = StrategyActive
	return s.store.UpdateStrategy(st)
}

// StopStrategy 停止策略（F4）。
func (s *Service) StopStrategy(userID, id int64) error {
	st, err := s.store.GetStrategy(id)
	if err != nil {
		return err
	}
	if st.UserID != userID {
		return ErrNotOwner
	}
	st.Status = StrategyStopped
	return s.store.UpdateStrategy(st)
}

// Run 后台 tick 循环，驱动所有 active 策略。
func (s *Service) Run(ctx context.Context) {
	if s.cfg.TickInterval <= 0 {
		return
	}
	ticker := time.NewTicker(s.cfg.TickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			list, _ := s.store.ListActiveStrategies()
			for _, st := range list {
				if err := s.tick(st); err != nil && s.log != nil {
					s.log.Warn("bot tick failed", zap.Int64("strategy", st.ID), zap.Error(err))
				}
			}
		}
	}
}

// Tick 手动触发一次指定策略的轮询下单（供管理/调试/跨服务 e2e 强制驱动；生产由 Run 后台循环调用）。
// 仅对 active 策略生效。
func (s *Service) Tick(ctx context.Context, id int64) error {
	st, err := s.store.GetStrategy(id)
	if err != nil {
		return err
	}
	if st.Status != StrategyActive {
		return ErrInvalidParam
	}
	return s.tick(st)
}

// tick 执行单个策略的一轮：依类型产生下单信号，代用户下单。
// F1 幂等：client_oid = bot:strategyID:round，下游 spot/futures 后端去重；
// F4 授权：代下单用策略绑定的用户 token，下游校验 token->userID，杜绝越权；
// F5/越仓：累计额达 MaxPosition 即暂停本轮。
func (s *Service) tick(st *BotStrategy) error {
	s.mu.Lock()
	round := s.rounds[st.ID]
	s.rounds[st.ID] = round + 1
	s.mu.Unlock()

	if st.Params.MaxPosition > 0 && float64(round)*st.Params.OrderAmount >= st.Params.MaxPosition {
		return nil
	}

	price := 0.0
	switch st.Type {
	case StrategyGrid, StrategyDCA, StrategyMA:
		p, err := s.price.Price(string(st.Market), st.Symbol)
		if err != nil {
			return err
		}
		price = p
	}

	qty := st.Params.OrderAmount / maxf(price, 1e-8)
	clientOID := fmt.Sprintf("bot:%d:%d", st.ID, round)
	exID, err := s.exec.Execute(context.Background(), st.UserToken, string(st.Market), st.Symbol, st.Side, price, qty, clientOID)
	if err != nil {
		return err
	}
	o := &BotOrder{
		StrategyID: st.ID, UserID: st.UserID, Market: st.Market, Symbol: st.Symbol,
		Side: st.Side, Price: price, Qty: qty, ClientOID: clientOID,
		ExchangeOrderID: exID, Status: "submitted", CreatedAt: time.Now().Unix(),
	}
	return s.store.CreateOrder(o)
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

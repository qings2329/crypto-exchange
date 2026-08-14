// Package oracle 指数价预言机：从多个喂价源（交易所现货 REST、内部定价服务等）
// 聚合出稳健的指数价格，供合约标记价格与资金费率使用。
//
// 聚合策略（业界标准做法）：
//  1. 周期性向每个喂价源拉取同一交易对的现货价。
//  2. 收集成功返回的报价，计算中位数（中位对单边插针/异常源不敏感）。
//  3. 离群剔除：以中位为基准，剔除偏离超过容差（默认 2%）的报价，
//     对剩余「健康」报价再次取中位，得到最终指数价。
//  4. 健壮性：健康源不足时回退（保留上次有效值；仅 1 个源则用该源）。
//
// 生产环境喂价源应覆盖多家交易所（Binance/OKX/Coinbase 等）现货，
// 单家宕机或插针不影响指数价。此处 HTTPFeed 为真实 REST 适配器，
// StaticFeed 用于离线演示与单测。
package oracle

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"
)

// DefaultOutlierTolerance 离群剔除容差（相对中位偏离），默认 2%。
const DefaultOutlierTolerance = 0.02

// DefaultMinFeeds 聚合所需的最小健康源数；不足则回退到上次有效值或单源。
const DefaultMinFeeds = 2

// DefaultPollInterval 默认轮询间隔。
const DefaultPollInterval = 3 * time.Second

// PriceFeed 单一喂价源（某交易所/服务的现货报价）。
type PriceFeed interface {
	// Name 喂价源标识（如 "binance"、"okx"），用于日志与审计。
	Name() string
	// Fetch 拉取指定交易对的现货报价；失败返回 error。
	Fetch(ctx context.Context, symbol string) (float64, error)
}

// StaticFeed 固定报价源（演示/单测）：返回预设价，可选抖动模拟多交易所微小价差。
type StaticFeed struct {
	name  string
	price float64
}

// NewStaticFeed 创建固定报价源。
func NewStaticFeed(name string, price float64) *StaticFeed {
	return &StaticFeed{name: name, price: price}
}

func (f *StaticFeed) Name() string { return f.name }

func (f *StaticFeed) Fetch(ctx context.Context, symbol string) (float64, error) {
	if f.price <= 0 {
		return 0, fmt.Errorf("static feed %s price not set", f.name)
	}
	return f.price, nil
}

// HTTPFeedConfig HTTP 喂价源配置。
type HTTPFeedConfig struct {
	Name      string        // 源标识
	URL       string        // REST 地址，需含 %s 占位交易对，如 https://api.x.com/api/v3/ticker/price?symbol=%s
	Timeout   time.Duration // 请求超时
	ParseFunc func([]byte) (float64, error) // 响应解析（不同交易所字段不同）
}

// HTTPFeed 真实 REST 喂价源适配器。
type HTTPFeed struct {
	cfg    HTTPFeedConfig
	client *http.Client
}

// NewHTTPFeed 创建 HTTP 喂价源。
func NewHTTPFeed(cfg HTTPFeedConfig) *HTTPFeed {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	return &HTTPFeed{
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
	}
}

func (f *HTTPFeed) Name() string { return f.cfg.Name }

func (f *HTTPFeed) Fetch(ctx context.Context, symbol string) (float64, error) {
	url := fmt.Sprintf(f.cfg.URL, symbol)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("feed %s http %d", f.cfg.Name, resp.StatusCode)
	}
	var body []byte
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if rerr != nil {
			break
		}
		if len(body) >= 1<<20 {
			break
		}
	}
	return f.cfg.ParseFunc(body)
}

// Config 预言机配置。
type Config struct {
	PollInterval time.Duration
	Tolerance    float64 // 离群剔除容差（相对中位），<=0 用默认
	MinFeeds     int     // 最小健康源数，<=0 用默认
	Feeds        map[string][]PriceFeed // 交易对 -> 喂价源列表
}

// Oracle 指数价预言机。
type Oracle struct {
	cfg    Config
	mu     sync.RWMutex
	index  map[string]float64 // 最近一次有效指数价（按交易对）
	raw    map[string][]feedSample // 最近一次各源原始报价（调试/审计）
	stop   chan struct{}
	closed bool
}

type feedSample struct {
	Name  string
	Price float64
	OK    bool
	Err   string
}

// New 创建预言机。
func New(cfg Config) *Oracle {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = DefaultPollInterval
	}
	if cfg.Tolerance <= 0 {
		cfg.Tolerance = DefaultOutlierTolerance
	}
	if cfg.MinFeeds <= 0 {
		cfg.MinFeeds = DefaultMinFeeds
	}
	return &Oracle{
		cfg:   cfg,
		index: make(map[string]float64),
		raw:   make(map[string][]feedSample),
		stop:  make(chan struct{}),
	}
}

// Start 启动后台轮询；首次同步聚合一次，保证 Start 返回时已有指数价（消除启动竞态）。
func (o *Oracle) Start() {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	o.mu.Unlock()
	// 首次同步拉取（不持锁，pollSymbol 内部会独立加锁），避免空窗与死锁。
	o.pollAll()
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return
	}
	go o.run()
	o.mu.Unlock()
}

func (o *Oracle) run() {
	ticker := time.NewTicker(o.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-o.stop:
			return
		case <-ticker.C:
			o.pollAll()
		}
	}
}

// Stop 停止轮询。
func (o *Oracle) Stop() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	close(o.stop)
	o.closed = true
}

func (o *Oracle) pollAll() {
	for symbol, feeds := range o.cfg.Feeds {
		o.pollSymbol(symbol, feeds)
	}
}

func (o *Oracle) pollSymbol(symbol string, feeds []PriceFeed) {
	ctx, cancel := context.WithTimeout(context.Background(), o.cfg.PollInterval)
	defer cancel()

	samples := make([]feedSample, 0, len(feeds))
	prices := make([]float64, 0, len(feeds))
	for _, f := range feeds {
		p, err := f.Fetch(ctx, symbol)
		if err != nil || p <= 0 {
			samples = append(samples, feedSample{Name: f.Name(), OK: false, Err: errMsg(err)})
			continue
		}
		samples = append(samples, feedSample{Name: f.Name(), Price: p, OK: true})
		prices = append(prices, p)
	}

	o.mu.Lock()
	o.raw[symbol] = samples
	o.mu.Unlock()

	if idx, ok := aggregate(prices, o.cfg.Tolerance, o.cfg.MinFeeds); ok {
		o.mu.Lock()
		o.index[symbol] = idx
		o.mu.Unlock()
	}
	// 聚合失败（源不足且无可回退）：保留上次有效值，不更新。
}

// IndexPrice 返回交易对最近有效指数价；未初始化返回 (0, false)。
func (o *Oracle) IndexPrice(symbol string) (float64, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	v, ok := o.index[symbol]
	return v, ok
}

// Snapshot 返回所有交易对当前指数价（副本）。
func (o *Oracle) Snapshot() map[string]float64 {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make(map[string]float64, len(o.index))
	for k, v := range o.index {
		out[k] = v
	}
	return out
}

// RawSnapshot 返回最近一次各源原始报价（审计/调试）。
func (o *Oracle) RawSnapshot() map[string][]feedSample {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make(map[string][]feedSample, len(o.raw))
	for k, v := range o.raw {
		cp := make([]feedSample, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// aggregate 由多个报价聚合出指数价：
//  1. 报价不足 MinFeeds 且无历史：返回 (0, false)（交由调用方回退）。
//  2. 报价数 == 1：直接采用该源（但要求 MinFeeds<=1）。
//  3. 报价 >= 3：先取中位，剔除偏离 > 容差的离群，对剩余再取中位。
//     健康源不足 MinFeeds 时，回退到「全量中位」保证有值（宁可带噪声也不真空）。
func aggregate(prices []float64, tol float64, minFeeds int) (float64, bool) {
	n := len(prices)
	if n == 0 {
		return 0, false
	}
	if n == 1 {
		if minFeeds <= 1 {
			return prices[0], true
		}
		return 0, false
	}
	med := median(prices)
	if n < 3 {
		// 2 个源：偏离检查后取两者平均，但若偏差过大则保留上次（返回 false）。
		if math.Abs(prices[0]-prices[1])/med > tol {
			return 0, false
		}
		return (prices[0] + prices[1]) / 2, true
	}
	// 3 个及以上：剔除离群后取中位。
	clean := prices[:0]
	for _, p := range prices {
		if math.Abs(p-med)/med <= tol {
			clean = append(clean, p)
		}
	}
	if len(clean) >= minFeeds {
		return median(clean), true
	}
	// 健康源不足：回退全量中位（保证有值，带噪声优于真空）。
	return med, true
}

// median 对切片取中位（不修改入参）。
func median(in []float64) float64 {
	s := make([]float64, len(in))
	copy(s, in)
	sort.Float64s(s)
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

func errMsg(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// BinanceParse Binance ticker/price 响应解析：{"symbol":"BTCUSDT","price":"50000.00"}
func BinanceParse(body []byte) (float64, error) {
	var r struct {
		Price string `json:"price"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, err
	}
	var p float64
	if _, err := fmt.Sscanf(r.Price, "%f", &p); err != nil {
		return 0, err
	}
	return p, nil
}

// OKXParse OKX ticker 响应解析：{"data":[{"last":"50000.0"}]}
func OKXParse(body []byte) (float64, error) {
	var r struct {
		Data []struct {
			Last string `json:"last"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return 0, err
	}
	if len(r.Data) == 0 {
		return 0, fmt.Errorf("okx empty data")
	}
	var p float64
	if _, err := fmt.Sscanf(r.Data[0].Last, "%f", &p); err != nil {
		return 0, err
	}
	return p, nil
}

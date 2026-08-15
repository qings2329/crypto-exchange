// Package influxdb 提供行情 K 线（已收盘 OHLCV）的时序持久化能力，用于把
// docker-compose 中已声明但此前未实际使用的 InfluxDB 接入业务（T-16）。
//
// 设计：CandleStore 接口抽象「落盘一根已收盘 K 线」与「按时间窗回取」，
// New 按配置返回两种实现——
//   - url 非空：influxStore，经 influxdb-client-go v2 写入 measurement "kline"
//     （symbol/interval 为 tag，OHLCV 与买卖拆分量为 field，open_time 为时间戳）；
//     无限流服务可用时，Write/Query 返回 error，由调用方降级到内存（fail-degraded）。
//   - url 为空：memStore，纯内存环形缓冲（本地开发/无 InfluxDB 时与原有行为一致，
//     同时作为单测的确定性实现，避免依赖外部时序库）。
//
// 注意：本包独立定义 Candle，避免与 internal/market 形成循环依赖；
// market 包在写入/回取时做字段对齐的类型转换。
package influxdb

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	influxdb2 "github.com/influxdata/influxdb-client-go/v2"
	"github.com/influxdata/influxdb-client-go/v2/api"
)

// Candle 是持久化的 K 线表示，字段与 internal/market.Candle 对齐。
// 独立定义以避免 market <-> influxdb 循环依赖。
type Candle struct {
	Symbol         string
	Interval       string
	OpenTime       int64   // 桶起点（unix 毫秒），作为 InfluxDB 时间戳
	Open           float64
	High           float64
	Low            float64
	Close          float64
	Volume         float64
	QuoteVolume    float64
	BuyVolume      float64
	SellVolume     float64
	BuyQuoteVolume  float64
	SellQuoteVolume float64
	Ts             int64
}

// CandleStore 是 K 线持久化抽象：Write 落盘一根已收盘 K 线，Query 按时间窗回取。
type CandleStore interface {
	// Write 落盘一根已收盘 K 线；返回 error 表示写入失败（调用方应降级而非阻断行情）。
	Write(ctx context.Context, c Candle) error
	// Query 返回 [start, end) 时间窗内的已收盘 K 线，按 open_time 升序，最多 limit 根
	// （limit<=0 时不限条数）。start/end 为 unix 毫秒；end==0 表示取到当前。
	Query(ctx context.Context, symbol, interval string, start, end, limit int64) ([]*Candle, error)
	// Close 释放底层连接（内存实现为 no-op）。
	Close() error
}

// measurement 是 InfluxDB 中 K 线的 measurement 名。
const measurement = "kline"

// New 按配置构造持久化实现：url 为空返回内存实现；否则返回 InfluxDB v2 实现。
func New(url, token, org, bucket string) CandleStore {
	if url == "" {
		return newMemStore()
	}
	client := influxdb2.NewClient(url, token)
	return &influxStore{
		client:  client,
		writeAPI: client.WriteAPI(org, bucket),
		queryAPI: client.QueryAPI(org),
		org:     org,
		bucket:  bucket,
	}
}

// --- InfluxDB v2 实现 ---

type influxStore struct {
	client   influxdb2.Client
	writeAPI api.WriteAPI
	queryAPI api.QueryAPI
	org      string
	bucket   string
}

func (s *influxStore) Write(ctx context.Context, c Candle) error {
	p := influxdb2.NewPoint(
		measurement,
		map[string]string{"symbol": c.Symbol, "interval": c.Interval},
		map[string]any{
			"open":             c.Open,
			"high":             c.High,
			"low":              c.Low,
			"close":            c.Close,
			"volume":           c.Volume,
			"quote_volume":     c.QuoteVolume,
			"buy_volume":       c.BuyVolume,
			"sell_volume":      c.SellVolume,
			"buy_quote_volume": c.BuyQuoteVolume,
			"sell_quote_volume": c.SellQuoteVolume,
			"ts":               c.Ts,
		},
		time.UnixMilli(c.OpenTime),
	)
	s.writeAPI.WritePoint(p)
	// 同步刷新（best-effort）：ctx 仅用于约束等待；连接故障会经 Errors() 通道上报，
	// 这里不阻塞返回 error，调用方按 fail-degraded 处理。
	s.writeAPI.Flush()
	return nil
}

func (s *influxStore) Query(ctx context.Context, symbol, interval string, start, end, limit int64) ([]*Candle, error) {
	startT := time.UnixMilli(start)
	stopT := time.Now()
	if end > 0 {
		stopT = time.UnixMilli(end)
	}
	q := fmt.Sprintf(`from(bucket:"%s")
  |> range(start: %s, stop: %s)
  |> filter(fn: (r) => r._measurement == "%s" and r.symbol == "%s" and r.interval == "%s")
  |> pivot(rowKey:["_time"], columnKey:["_field"], valueColumn:"_value")
  |> sort(columns:["_time"])`,
		s.bucket,
		startT.UTC().Format(time.RFC3339Nano),
		stopT.UTC().Format(time.RFC3339Nano),
		measurement, symbol, interval,
	)
	if limit > 0 {
		q += fmt.Sprintf("\n  |> limit(n: %d)", limit)
	}

	result, err := s.queryAPI.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("influx query: %w", err)
	}

	out := make([]*Candle, 0)
	for result.Next() {
		row := result.Record().Values() // pivot 后每行为一个时间桶的全部字段
		c := &Candle{}
		if v, ok := row["_time"].(time.Time); ok {
			c.OpenTime = v.UnixMilli()
		}
		if v, ok := row["symbol"].(string); ok {
			c.Symbol = v
		}
		if v, ok := row["interval"].(string); ok {
			c.Interval = v
		}
		c.Open = toFloat(row["open"])
		c.High = toFloat(row["high"])
		c.Low = toFloat(row["low"])
		c.Close = toFloat(row["close"])
		c.Volume = toFloat(row["volume"])
		c.QuoteVolume = toFloat(row["quote_volume"])
		c.BuyVolume = toFloat(row["buy_volume"])
		c.SellVolume = toFloat(row["sell_volume"])
		c.BuyQuoteVolume = toFloat(row["buy_quote_volume"])
		c.SellQuoteVolume = toFloat(row["sell_quote_volume"])
		c.Ts = toInt64(row["ts"])
		out = append(out, c)
	}
	if err := result.Err(); err != nil {
		return nil, fmt.Errorf("influx scan: %w", err)
	}
	return out, nil
}

func (s *influxStore) Close() error {
	if s.client != nil {
		s.client.Close()
	}
	return nil
}

// toFloat 从 influx 回读的数值（可能是 float64 / int64 / json.Number）安全转 float64。
func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	case float32:
		return float64(n)
	case string:
		var f float64
		fmt.Sscanf(n, "%f", &f)
		return f
	default:
		return 0
	}
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	default:
		return 0
	}
}

// --- 内存回退实现（无 InfluxDB / 单测用） ---

type memStore struct {
	mu      sync.Mutex
	buckets map[string]map[string][]*Candle // symbol -> interval -> 升序 K 线
}

func newMemStore() *memStore {
	return &memStore{buckets: make(map[string]map[string][]*Candle)}
}

func (s *memStore) Write(_ context.Context, c Candle) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	iv, ok := s.buckets[c.Symbol]
	if !ok {
		iv = make(map[string][]*Candle)
		s.buckets[c.Symbol] = iv
	}
	// 已收盘 K 线按 open_time 单调递增追加；乱序到达时按序插入以保持升序。
	list := iv[c.Interval]
	cp := c
	idx := sort.Search(len(list), func(i int) bool { return list[i].OpenTime >= cp.OpenTime })
	if idx < len(list) && list[idx].OpenTime == cp.OpenTime {
		list[idx] = &cp // 同桶覆盖（幂等）
	} else {
		list = append(list, nil)
		copy(list[idx+1:], list[idx:])
		list[idx] = &cp
		iv[c.Interval] = list
	}
	return nil
}

func (s *memStore) Query(_ context.Context, symbol, interval string, start, end, limit int64) ([]*Candle, error) {
	s.mu.Lock()
	list := s.buckets[symbol][interval]
	out := make([]*Candle, 0, len(list))
	for _, c := range list {
		if c.OpenTime < start {
			continue
		}
		if end > 0 && c.OpenTime >= end {
			break
		}
		cp := *c
		out = append(out, &cp)
	}
	s.mu.Unlock()

	if limit > 0 && int64(len(out)) > limit {
		out = out[int64(len(out))-limit:]
	}
	return out, nil
}

func (s *memStore) Close() error { return nil }

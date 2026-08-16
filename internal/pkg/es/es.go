// Package es 提供成交（trade）的检索引擎能力，用于把 docker-compose 中已声明但此前
// 未实际使用的 Elasticsearch 接入业务（T-16）。
//
// 设计：TradeIndexer 接口抽象「落盘一条成交」与「按条件检索」，New 按配置返回两种实现——
//   - url 非空：esIndexer，经 github.com/elastic/go-elasticsearch/v8 把成交写入 index "trades"
//     （symbol/taker_side 为 keyword 便于精确过滤，ts 为时间窗检索与排序字段）；
//     未配置/不可达时 Index/Search 返回 error，由调用方降级（fail-degraded）。
//   - url 为空：memIndexer，纯内存实现（本地开发/无 ES 时与原有行为一致，同时作为
//     单测的确定性实现，避免依赖外部 ES）。
//
// doc id 以 FNV-64a 对成交关键字段哈希得到，保证 ES at-least-once / 重试下幂等（同笔成交仅存一份）。
package es

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"sync"

	es8 "github.com/elastic/go-elasticsearch/v8"
)

// TradeDoc 是写入 ES 的成交文档（字段对齐 mq.TradeEvent，附加可检索的 value 与确定性 id）。
type TradeDoc struct {
	ID        string  `json:"id"`         // 确定性幂等键（FNV-64a 对关键字段哈希）
	Symbol    string  `json:"symbol"`     // keyword，精确过滤
	Price     float64 `json:"price"`
	Qty       float64 `json:"qty"`
	TakerID   int64   `json:"taker_id"`
	MakerID   int64   `json:"maker_id"`
	TakerSide string  `json:"taker_side"` // buy/sell
	Value     float64 `json:"value"`      // price*qty（计价货币）
	Ts        int64   `json:"ts"`         // unix 毫秒，时间窗检索与排序
}

// TradeQuery 是成交检索条件（全部可选；空条件返回最近成交，按 ts 降序）。
type TradeQuery struct {
	Symbol string // 精确匹配，空=不限
	Side   string // buy/sell，空=不限
	From   int64  // ts 下界（含），unix 毫秒，0=不限
	To     int64  // ts 上界（含），unix 毫秒，0=不限
	Limit  int    // <=0 用默认 100
}

// TradeIndexer 是成交检索引擎抽象：Index 落盘一条成交，Search 按条件回取。
type TradeIndexer interface {
	// Index 落盘一条成交；返回 error 表示写入失败（调用方应降级而非阻断行情）。
	Index(ctx context.Context, doc TradeDoc) error
	// Search 按条件回取成交，按 ts 降序（最新在前），最多 Limit 条。
	Search(ctx context.Context, q TradeQuery) ([]TradeDoc, error)
	// Close 释放底层连接（内存实现为 no-op；ES v8 client 无需显式关闭）。
	Close() error
}

// indexName 是成交文档的默认索引名。
const indexName = "trades"

// New 按配置构造检索引擎：url 为空返回内存实现；否则返回 ES v8 实现。
func New(url, index string) TradeIndexer {
	if index == "" {
		index = indexName
	}
	if url == "" {
		return newMemIndexer()
	}
	client, err := es8.NewClient(es8.Config{Addresses: []string{url}})
	if err != nil {
		// 配置无法构造 client（极少见）：降级内存，保证服务可用。
		return newMemIndexer()
	}
	return &esIndexer{client: client, index: index}
}

// tradeDocID 以 FNV-64a 对成交关键字段哈希得到稳定幂等键，保证 ES 重试/at-least-once 下重复写入可跳过。
func tradeDocID(d TradeDoc) string {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%f|%f|%d|%d|%s|%d", d.Symbol, d.Price, d.Qty, d.TakerID, d.MakerID, d.TakerSide, d.Ts)
	return strconv.FormatUint(h.Sum64(), 16)
}

// --- Elasticsearch v8 实现 ---

type esIndexer struct {
	client *es8.Client
	index  string
}

func (e *esIndexer) Index(ctx context.Context, doc TradeDoc) error {
	body, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("es marshal: %w", err)
	}
	res, err := e.client.Index(
		e.index,
		bytes.NewReader(body),
		e.client.Index.WithDocumentID(tradeDocID(doc)),
		e.client.Index.WithContext(ctx),
	)
	if err != nil {
		return fmt.Errorf("es index: %w", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		return fmt.Errorf("es index error: %s", res.String())
	}
	return nil
}

func (e *esIndexer) Search(ctx context.Context, q TradeQuery) ([]TradeDoc, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}

	must := make([]map[string]any, 0)
	if q.Symbol != "" {
		must = append(must, map[string]any{"term": map[string]any{"symbol": q.Symbol}})
	}
	if q.Side != "" {
		must = append(must, map[string]any{"term": map[string]any{"taker_side": q.Side}})
	}
	rng := map[string]any{}
	if q.From > 0 {
		rng["gte"] = q.From
	}
	if q.To > 0 {
		rng["lte"] = q.To
	}
	if len(rng) > 0 {
		must = append(must, map[string]any{"range": map[string]any{"ts": rng}})
	}

	var queryBody map[string]any
	if len(must) > 0 {
		queryBody = map[string]any{"bool": map[string]any{"filter": must}}
	} else {
		queryBody = map[string]any{"match_all": map[string]any{}}
	}
	dsl := map[string]any{
		"query": queryBody,
		"sort":  []map[string]any{{"ts": map[string]any{"order": "desc"}}},
		"size":  limit,
	}
	body, err := json.Marshal(dsl)
	if err != nil {
		return nil, fmt.Errorf("es marshal query: %w", err)
	}

	res, err := e.client.Search(
		e.client.Search.WithContext(ctx),
		e.client.Search.WithIndex(e.index),
		e.client.Search.WithBody(bytes.NewReader(body)),
	)
	if err != nil {
		return nil, fmt.Errorf("es search: %w", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		return nil, fmt.Errorf("es search error: %s", res.String())
	}

	var r struct {
		Hits struct {
			Hits []struct {
				Source TradeDoc `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(res.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("es decode: %w", err)
	}
	out := make([]TradeDoc, 0, len(r.Hits.Hits))
	for _, h := range r.Hits.Hits {
		out = append(out, h.Source)
	}
	return out, nil
}

func (e *esIndexer) Close() error { return nil }

// --- 内存回退实现（无 ES / 单测用） ---

type memIndexer struct {
	mu     sync.Mutex
	docs   map[string]TradeDoc
	order  []string // 插入顺序的 id（去重）
	cap    int
}

func newMemIndexer() *memIndexer {
	return &memIndexer{docs: make(map[string]TradeDoc), cap: 10000}
}

func (m *memIndexer) Index(_ context.Context, doc TradeDoc) error {
	id := tradeDocID(doc)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.docs[id]; !ok {
		m.docs[id] = doc
		m.order = append(m.order, id)
		if len(m.order) > m.cap {
			delete(m.docs, m.order[0])
			m.order = m.order[1:]
		}
	} else {
		m.docs[id] = doc // 幂等覆盖
	}
	return nil
}

func (m *memIndexer) Search(_ context.Context, q TradeQuery) ([]TradeDoc, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	m.mu.Lock()
	matched := make([]TradeDoc, 0, len(m.docs))
	for _, id := range m.order {
		d := m.docs[id]
		if q.Symbol != "" && d.Symbol != q.Symbol {
			continue
		}
		if q.Side != "" && d.TakerSide != q.Side {
			continue
		}
		if q.From > 0 && d.Ts < q.From {
			continue
		}
		if q.To > 0 && d.Ts > q.To {
			continue
		}
		matched = append(matched, d)
	}
	m.mu.Unlock()

	// 按 ts 降序（最新在前），与 ES 行为一致。
	sort.Slice(matched, func(i, j int) bool { return matched[i].Ts > matched[j].Ts })
	if len(matched) > limit {
		matched = matched[:limit]
	}
	return matched, nil
}

func (m *memIndexer) Close() error { return nil }

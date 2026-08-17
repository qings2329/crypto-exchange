package otc

import (
	"sync"
	"time"
)

// memStore 是内存版 Store，供单测与无 MySQL 的演示环境使用。
type memStore struct {
	mu                sync.RWMutex
	ads               map[int64]*OtcAdvertisement
	orders            map[int64]*OtcOrder
	counterparties    map[string]*OtcCounterparty // key: userID:counterpartyID
	messages          map[int64]*OtcMessage
	proofs            map[int64]*OtcProof
	nextAdID          int64
	nextOrderID       int64
	nextCounterpartyID int64
	nextMessageID     int64
	nextProofID       int64
}

// NewMemStore 构造内存 Store。
func NewMemStore() Store {
	return &memStore{
		ads:               make(map[int64]*OtcAdvertisement),
		orders:            make(map[int64]*OtcOrder),
		counterparties:    make(map[string]*OtcCounterparty),
		messages:          make(map[int64]*OtcMessage),
		proofs:            make(map[int64]*OtcProof),
		nextAdID:          1,
		nextOrderID:       1,
		nextCounterpartyID: 1,
		nextMessageID:     1,
		nextProofID:       1,
	}
}

func cpKey(u, c int64) string { return itoa(u) + ":" + itoa(c) }

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	buf := make([]byte, 0, 20)
	for v > 0 {
		buf = append([]byte{byte('0' + v%10)}, buf...)
		v /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

// --- 广告 ---

func (s *memStore) CreateAd(a *OtcAdvertisement) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.ID == 0 {
		a.ID = s.nextAdID
		s.nextAdID++
	}
	now := time.Now()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	a.UpdatedAt = now
	cp := *a
	s.ads[cp.ID] = &cp
	return nil
}

func (s *memStore) GetAd(id int64) (*OtcAdvertisement, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.ads[id]
	if !ok {
		return nil, ErrAdNotFound
	}
	cp := *a
	return &cp, nil
}

func (s *memStore) ListAds(side AdSide, asset string) ([]*OtcAdvertisement, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*OtcAdvertisement, 0)
	for _, a := range s.ads {
		if a.Status != AdOpen {
			continue
		}
		if side != "" && a.Side != side {
			continue
		}
		if asset != "" && a.Asset != asset {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	return out, nil
}

func (s *memStore) UpdateAd(a *OtcAdvertisement) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.UpdatedAt = time.Now()
	cp := *a
	s.ads[cp.ID] = &cp
	return nil
}

// --- 订单 ---

func (s *memStore) CreateOrder(o *OtcOrder) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o.ID == 0 {
		o.ID = s.nextOrderID
		s.nextOrderID++
	}
	now := time.Now()
	if o.CreatedAt.IsZero() {
		o.CreatedAt = now
	}
	o.UpdatedAt = now
	cp := *o
	s.orders[cp.ID] = &cp
	return nil
}

func (s *memStore) GetOrder(id int64) (*OtcOrder, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orders[id]
	if !ok {
		return nil, ErrOrderNotFound
	}
	cp := *o
	return &cp, nil
}

func (s *memStore) UpdateOrder(o *OtcOrder) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o.UpdatedAt = time.Now()
	cp := *o
	s.orders[cp.ID] = &cp
	return nil
}

func (s *memStore) ListOrders(userID int64) ([]*OtcOrder, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*OtcOrder, 0)
	for _, o := range s.orders {
		if o.MakerID == userID || o.TakerID == userID {
			cp := *o
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (s *memStore) ListAllOrders() ([]*OtcOrder, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*OtcOrder, 0, len(s.orders))
	for _, o := range s.orders {
		cp := *o
		out = append(out, &cp)
	}
	return out, nil
}

// --- 对手方信用 ---

func (s *memStore) UpsertCounterparty(cp *OtcCounterparty) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cp.ID == 0 {
		cp.ID = s.nextCounterpartyID
		s.nextCounterpartyID++
	}
	cp.UpdatedAt = time.Now()
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = cp.UpdatedAt
	}
	c := *cp
	s.counterparties[cpKey(c.UserID, c.CounterpartyID)] = &c
	return nil
}

func (s *memStore) GetCounterparty(userID, counterpartyID int64) (*OtcCounterparty, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp, ok := s.counterparties[cpKey(userID, counterpartyID)]
	if !ok {
		return nil, ErrCounterpartyNotFound
	}
	c := *cp
	return &c, nil
}

func (s *memStore) ListCounterparties(userID int64) ([]*OtcCounterparty, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*OtcCounterparty, 0)
	for _, cp := range s.counterparties {
		if cp.UserID == userID {
			c := *cp
			out = append(out, &c)
		}
	}
	return out, nil
}

// --- 订单沟通 / 付款凭证 ---

func (s *memStore) CreateMessage(m *OtcMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ID == 0 {
		m.ID = s.nextMessageID
		s.nextMessageID++
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	cp := *m
	s.messages[cp.ID] = &cp
	return nil
}

func (s *memStore) ListMessages(orderID int64) ([]*OtcMessage, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*OtcMessage, 0)
	for _, m := range s.messages {
		if m.OrderID == orderID {
			cp := *m
			out = append(out, &cp)
		}
	}
	// 按创建时间升序返回
	sortMessages(out)
	return out, nil
}

func (s *memStore) CreateProof(p *OtcProof) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ID == 0 {
		p.ID = s.nextProofID
		s.nextProofID++
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	cp := *p
	s.proofs[cp.ID] = &cp
	return nil
}

func (s *memStore) ListProofs(orderID int64) ([]*OtcProof, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*OtcProof, 0)
	for _, p := range s.proofs {
		if p.OrderID == orderID {
			cp := *p
			out = append(out, &cp)
		}
	}
	sortProofs(out)
	return out, nil
}

func sortMessages(ms []*OtcMessage) {
	for i := 1; i < len(ms); i++ {
		for j := i; j > 0 && ms[j-1].CreatedAt.After(ms[j].CreatedAt); j-- {
			ms[j-1], ms[j] = ms[j], ms[j-1]
		}
	}
}

func sortProofs(ps []*OtcProof) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j-1].CreatedAt.After(ps[j].CreatedAt); j-- {
			ps[j-1], ps[j] = ps[j], ps[j-1]
		}
	}
}

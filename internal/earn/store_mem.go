package earn

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// memStore 是内存版 Store（演示/测试）。生产配置 DSN 时使用 MySQL 版（store_mysql.go）。
type memStore struct {
	mu       sync.RWMutex
	seq      int64
	products map[int64]*EarnProduct
	subs     map[int64]*EarnSubscription
	projects map[int64]*LaunchProject
	posByPK  map[string]*LaunchPosition // "user:project:pool" -> position（FindPosition 索引）
}

// NewMemStore 构造内存版 Store。
func NewMemStore() Store {
	return &memStore{
		products: map[int64]*EarnProduct{},
		subs:     map[int64]*EarnSubscription{},
		projects: map[int64]*LaunchProject{},
		posByPK:  map[string]*LaunchPosition{},
	}
}

func (m *memStore) nextID() int64 {
	m.seq++
	return m.seq
}

func posPK(userID, projectID int64, poolID string) string {
	return fmt.Sprintf("%d:%d:%s", userID, projectID, strings.ToLower(poolID))
}

// --- 理财产品 ---

func (m *memStore) CreateProduct(p *EarnProduct) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p.ID = m.nextID()
	now := time.Now()
	p.CreatedAt, p.UpdatedAt = now, now
	cp := *p
	m.products[p.ID] = &cp
	return nil
}

func (m *memStore) GetProduct(id int64) (*EarnProduct, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.products[id]
	if !ok {
		return nil, ErrProductNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *memStore) ListProducts(status ProductStatus) ([]*EarnProduct, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*EarnProduct
	for _, p := range m.products {
		if status == "" || p.Status == status {
			cp := *p
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memStore) UpdateProduct(p *EarnProduct) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.products[p.ID]; !ok {
		return ErrProductNotFound
	}
	p.UpdatedAt = time.Now()
	cp := *p
	m.products[p.ID] = &cp
	return nil
}

// --- 理财申购 ---

func (m *memStore) CreateSubscription(s *EarnSubscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.ID = m.nextID()
	cp := *s
	m.subs[s.ID] = &cp
	return nil
}

func (m *memStore) GetSubscription(id int64) (*EarnSubscription, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.subs[id]
	if !ok {
		return nil, ErrProductNotFound
	}
	cp := *s
	return &cp, nil
}

func (m *memStore) UpdateSubscription(s *EarnSubscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.subs[s.ID]; !ok {
		return ErrProductNotFound
	}
	cp := *s
	m.subs[s.ID] = &cp
	return nil
}

func (m *memStore) DeleteSubscription(id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.subs, id)
	return nil
}

func (m *memStore) ListSubscriptions(userID int64) ([]*EarnSubscription, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*EarnSubscription
	for _, s := range m.subs {
		if s.UserID == userID {
			cp := *s
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID }) // 最新在前
	return out, nil
}

func (m *memStore) ListAllSubscriptions() ([]*EarnSubscription, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*EarnSubscription
	for _, s := range m.subs {
		cp := *s
		out = append(out, &cp)
	}
	return out, nil
}

// --- Launchpool 项目 ---

func (m *memStore) CreateProject(p *LaunchProject) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p.ID = m.nextID()
	p.CreatedAt = time.Now()
	cp := *p
	cp.FundedTotal = settlement.AssetAmount{Value: new(big.Int), Decimals: settlement.AssetDecimalsByName(cp.Token)}
	m.projects[p.ID] = &cp
	return nil
}

func (m *memStore) GetProject(id int64) (*LaunchProject, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.projects[id]
	if !ok {
		return nil, ErrProjectNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *memStore) AddProjectFunded(id int64, d settlement.AssetAmount) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.projects[id]
	if !ok {
		return ErrProjectNotFound
	}
	if p.FundedTotal.Value == nil {
		p.FundedTotal = settlement.AssetAmount{Value: new(big.Int), Decimals: d.Decimals}
	}
	p.FundedTotal = p.FundedTotal.Add(d)
	return nil
}

func (m *memStore) ListProjects() ([]*LaunchProject, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*LaunchProject
	for _, p := range m.projects {
		cp := *p
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// --- Launchpool 仓位 ---

func (m *memStore) UpsertPosition(pos *LaunchPosition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pk := posPK(pos.UserID, pos.ProjectID, pos.PoolID)
	if existing, ok := m.posByPK[pk]; ok {
		pos.ID = existing.ID
		pos.CreatedAt = existing.CreatedAt
		cp := *pos
		m.posByPK[pk] = &cp
		return nil
	}
	pos.ID = m.nextID()
	pos.CreatedAt = time.Now()
	cp := *pos
	m.posByPK[pk] = &cp
	return nil
}

func (m *memStore) DeletePosition(id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for pk, pos := range m.posByPK {
		if pos.ID == id {
			delete(m.posByPK, pk)
			return nil
		}
	}
	return nil
}

func (m *memStore) GetPosition(id int64) (*LaunchPosition, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, pos := range m.posByPK {
		if pos.ID == id {
			cp := *pos
			return &cp, nil
		}
	}
	return nil, ErrPositionNotFound
}

func (m *memStore) FindPosition(userID, projectID int64, poolID string) (*LaunchPosition, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	pos, ok := m.posByPK[posPK(userID, projectID, poolID)]
	if !ok {
		return nil, ErrPositionNotFound
	}
	cp := *pos
	return &cp, nil
}

func (m *memStore) ListPositions(userID int64) ([]*LaunchPosition, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*LaunchPosition
	for _, pos := range m.posByPK {
		if pos.UserID == userID {
			cp := *pos
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *memStore) ListAllPositions() ([]*LaunchPosition, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*LaunchPosition
	for _, pos := range m.posByPK {
		cp := *pos
		out = append(out, &cp)
	}
	return out, nil
}

func (m *memStore) Close() error { return nil }

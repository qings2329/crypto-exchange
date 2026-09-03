package c2c

import (
	"time"
)

// Service 是 C2C 订单业务逻辑。
type Service struct {
	store Store
}

// NewService 构造 C2C 服务。
func NewService(store Store) *Service {
	return &Service{store: store}
}

func validateCreate(side Side, coin string, amount, price float64) error {
	if !ValidSide(side) {
		return ErrInvalidSide
	}
	if coin == "" {
		return ErrInvalidCoin
	}
	if amount <= 0 {
		return ErrInvalidAmount
	}
	if price <= 0 {
		return ErrInvalidPrice
	}
	return nil
}

// Create 用户发布一笔 C2C 挂单（买卖广告）。total = amount * price。
func (s *Service) Create(userID int64, side Side, coin string, amount, price float64, note string) (*Order, error) {
	if err := validateCreate(side, coin, amount, price); err != nil {
		return nil, err
	}
	o := &Order{
		Side:   side,
		Coin:   coin,
		Amount: amount,
		Price:  price,
		Total:  amount * price,
		UserID: userID,
		Status: StatusOpen,
		Note:   note,
	}
	if err := s.store.Create(o); err != nil {
		return nil, err
	}
	return o, nil
}

// List 分页查询，支持按 user/side/coin/status 过滤。
func (s *Service) List(filter OrderFilter, limit, offset int) ([]*Order, int, error) {
	if filter.Side != "" && !ValidSide(filter.Side) {
		return nil, 0, ErrInvalidSide
	}
	if filter.Status != "" && !ValidStatus(filter.Status) {
		return nil, 0, &OrderError{"invalid status filter"}
	}
	return s.store.List(filter, limit, offset)
}

// transition 执行状态机迁移：open/locked -> locked/completed，cancelled/disputed 不再变更。
func (s *Service) transition(id int64, from, to OrderStatus) (*Order, error) {
	ok, err := s.store.UpdateStatus(id, from, to)
	if err != nil {
		return nil, err
	}
	o, err := s.store.Get(id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return o, ErrBadTransition
	}
	return o, nil
}

// Freeze 运营冻结挂单（open -> locked）。
func (s *Service) Freeze(id int64) (*Order, error) {
	return s.transition(id, StatusOpen, StatusLocked)
}

// Release 运营解冻（locked -> open）。
func (s *Service) Release(id int64) (*Order, error) {
	return s.transition(id, StatusLocked, StatusOpen)
}

// Complete 让管理员把订单标记为已完成（仅 open 或 locked 可完成）。
func (s *Service) Complete(id int64) (*Order, error) {
	o, err := s.transition(id, StatusOpen, StatusCompleted)
	if err != nil && err == ErrBadTransition {
		// 允许从 locked 直接完成。
		return s.transition(id, StatusLocked, StatusCompleted)
	}
	return o, err
}

// SeedDemo 幂等写入若干演示订单（按用户/币种的确定性组合判重），
// 供管理后台「C2C 管理」页有真实数据可看（与公告演示种子同理，连库时重启不重复）。
func (s *Service) SeedDemo() (int, error) {
	demo := []Order{
		{Side: SideBuy, Coin: "USDT", Amount: 5000, Price: 7.2, UserID: 1001},
		{Side: SideSell, Coin: "USDT", Amount: 3200, Price: 7.18, UserID: 1002},
		{Side: SideBuy, Coin: "BTC", Amount: 0.5, Price: 398000, UserID: 1003},
		{Side: SideSell, Coin: "ETH", Amount: 2.0, Price: 16800, UserID: 1004},
	}
	created := 0
	for _, d := range demo {
		existing, _, err := s.store.List(OrderFilter{UserID: d.UserID, Coin: d.Coin, Side: d.Side}, 1, 0)
		if err != nil {
			return created, err
		}
		if len(existing) > 0 {
			continue // 已存在（幂等）
		}
		o := d
		o.Status = StatusOpen
		o.Total = o.Amount * o.Price
		now := time.Now()
		// 让部分演示订单呈现不同状态（open / completed），便于后台看到多种状态。
		t := o.UserID % 3
		switch t {
		case 1:
			o.Status = StatusCompleted
		case 2:
			o.Status = StatusLocked
		}
		o.UpdatedAt = now
		if err := s.store.Create(&o); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
}

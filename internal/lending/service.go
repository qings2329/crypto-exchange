package lending

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/settlement"
	"go.uber.org/zap"
)

// Config 借贷服务配置。
type Config struct {
	AccrueInterval   time.Duration // 利息归集周期
	MinBorrowAmount  float64       // 最小借款额
	MinLendAmount    float64       // 最小存款额
	BaseInterestRate float64       // 基础年化利率（如 0.05 = 5%）
	MaxInterestRate  float64       // 最大年化利率（如 1.0 = 100%）
}

// Service 借贷业务服务。
type Service struct {
	store  Store
	ledger *ledger.Ledger
	cfg    Config
	log    *zap.Logger
}

// NewService 构造借贷服务。
func NewService(store Store, l *ledger.Ledger, cfg Config, log *zap.Logger) *Service {
	return &Service{store: store, ledger: l, cfg: cfg, log: log}
}

// CreatePool 创建借贷池（管理员）。
func (s *Service) CreatePool(asset string, collateralReq float64) (*LendingPool, error) {
	if asset == "" {
		return nil, ErrInvalidParam
	}
	if collateralReq < 1.0 {
		collateralReq = 1.5 // 默认150%
	}
	existing, _ := s.store.GetPoolByAsset(asset)
	if existing != nil {
		return nil, ErrInvalidParam // 池已存在
	}
	p := &LendingPool{
		Asset:         asset,
		TotalSupply:   settlement.NewAssetAmount(big.NewInt(0), 8),
		TotalBorrow:   settlement.NewAssetAmount(big.NewInt(0), 8),
		Available:     settlement.NewAssetAmount(big.NewInt(0), 8),
		InterestRate:  s.cfg.BaseInterestRate,
		CollateralReq: collateralReq,
		Status:        PoolActive,
		CreatedAt:     time.Now().Unix(),
	}
	if err := s.store.CreatePool(p); err != nil {
		return nil, err
	}
	return p, nil
}

// Lend 用户存款（出借）：校验 -> 锁定本金 -> 入池。
// F1 幂等：以 lend_order:<id> 作为 ledger ref。
func (s *Service) Lend(userID, poolID int64, amount settlement.AssetAmount) (*LendOrder, error) {
	if amount.Sign() <= 0 {
		return nil, ErrBelowMinAmount
	}
	p, err := s.store.GetPool(poolID)
	if err != nil {
		return nil, err
	}
	if p.Status != PoolActive {
		return nil, ErrPoolNotActive
	}

	// 1) 创建存款订单占 ID
	order := &LendOrder{
		UserID:    userID,
		PoolID:    poolID,
		Amount:    amount,
		Rate:      p.InterestRate,
		Status:    "active",
		CreatedAt: time.Now().Unix(),
	}
	if err := s.store.CreateLendOrder(order); err != nil {
		return nil, err
	}

	// 2) 账本锁定：用户可用 -> SysLendingPool
	ref := fmt.Sprintf("lend_order:%d", order.ID)
	if err := s.ledger.Transfer(userID, ledger.SysLendingPool, p.Asset, amount, "lend_deposit", ref); err != nil {
		_ = s.store.UpdateLendOrder(&LendOrder{ID: order.ID, Status: "cancelled"})
		return nil, err
	}

	// 3) 更新池子状态
	p.TotalSupply = p.TotalSupply.Add(amount)
	p.Available = p.Available.Add(amount)
	if err := s.store.UpdatePool(p); err != nil {
		return nil, err
	}
	return order, nil
}

// Borrow 用户借款：校验抵押 -> 冻结抵押品 -> 放款。
func (s *Service) Borrow(userID, poolID int64, borrowAmt, collateralAmt settlement.AssetAmount) (*BorrowOrder, error) {
	if borrowAmt.Sign() <= 0 || collateralAmt.Sign() <= 0 {
		return nil, ErrBelowMinAmount
	}
	p, err := s.store.GetPool(poolID)
	if err != nil {
		return nil, err
	}
	if p.Status != PoolActive {
		return nil, ErrPoolNotActive
	}
	// 可用额度检查
	if p.Available.Cmp(borrowAmt) < 0 {
		return nil, ErrInsufficientLiquidity
	}
	// 抵押率检查：collateralAmt / borrowAmt >= CollateralReq
	collateralRatio := float64(collateralAmt.Value.Int64()) / float64(borrowAmt.Value.Int64())
	if collateralRatio < p.CollateralReq {
		return nil, ErrInsufficientCollateral
	}

	// 1) 创建借款订单
	order := &BorrowOrder{
		UserID:      userID,
		PoolID:      poolID,
		Amount:      borrowAmt,
		Collateral:  collateralAmt,
		Rate:        p.InterestRate,
		InterestAcc: settlement.NewAssetAmount(big.NewInt(0), 8),
		Status:      "active",
		CreatedAt:   time.Now().Unix(),
	}
	if err := s.store.CreateBorrowOrder(order); err != nil {
		return nil, err
	}

	// 2) 冻结抵押品：用户可用 -> SysLendingCollateral
	ref := fmt.Sprintf("borrow_collateral:%d", order.ID)
	if err := s.ledger.Transfer(userID, ledger.SysLendingCollateral, p.Asset, collateralAmt, "borrow_collateral", ref); err != nil {
		_ = s.store.UpdateBorrowOrder(&BorrowOrder{ID: order.ID, Status: "cancelled"})
		return nil, err
	}

	// 3) 放款：SysLendingPool -> 用户可用
	refLoan := fmt.Sprintf("borrow_loan:%d", order.ID)
	if err := s.ledger.Transfer(ledger.SysLendingPool, userID, p.Asset, borrowAmt, "borrow_loan", refLoan); err != nil {
		// 回退抵押品
		_ = s.ledger.Transfer(ledger.SysLendingCollateral, userID, p.Asset, collateralAmt, "borrow_collateral_revert", ref)
		_ = s.store.UpdateBorrowOrder(&BorrowOrder{ID: order.ID, Status: "cancelled"})
		return nil, err
	}

	// 4) 更新池子
	p.TotalBorrow = p.TotalBorrow.Add(borrowAmt)
	p.Available = p.Available.Sub(borrowAmt)
	if err := s.store.UpdatePool(p); err != nil {
		return nil, err
	}
	return order, nil
}

// Repay 用户还款：校验 -> 释放抵押品 -> 收回本息。
func (s *Service) Repay(userID, borrowOrderID int64) (*BorrowOrder, error) {
	o, err := s.store.GetBorrowOrder(borrowOrderID)
	if err != nil {
		return nil, err
	}
	if o.UserID != userID {
		return nil, ErrNotOwner
	}
	if o.Status != "active" {
		return nil, ErrAlreadyRepaid
	}

	p, err := s.store.GetPool(o.PoolID)
	if err != nil {
		return nil, err
	}

	// 还款额 = 本金 + 累计利息
	totalRepay := o.Amount.Add(o.InterestAcc)

	// 1) 收回本息：用户可用 -> SysLendingPool
	refRepay := fmt.Sprintf("borrow_repay:%d", o.ID)
	if err := s.ledger.Transfer(userID, ledger.SysLendingPool, p.Asset, totalRepay, "borrow_repay", refRepay); err != nil {
		return nil, err
	}

	// 2) 释放抵押品：SysLendingCollateral -> 用户可用
	refRelease := fmt.Sprintf("borrow_release:%d", o.ID)
	if err := s.ledger.Transfer(ledger.SysLendingCollateral, userID, p.Asset, o.Collateral, "borrow_collateral_release", refRelease); err != nil {
		return nil, err
	}

	// 3) 更新订单状态
	o.Status = "repaid"
	o.RepaidAt = time.Now().Unix()
	if err := s.store.UpdateBorrowOrder(o); err != nil {
		return nil, err
	}

	// 4) 更新池子
	p.TotalBorrow = p.TotalBorrow.Sub(o.Amount)
	p.Available = p.Available.Add(o.Amount)
	_ = s.store.UpdatePool(p)

	return o, nil
}

// Withdraw 用户取回存款：校验 -> 释放资金。
func (s *Service) Withdraw(userID, lendOrderID int64) (*LendOrder, error) {
	o, err := s.store.GetLendOrder(lendOrderID)
	if err != nil {
		return nil, err
	}
	if o.UserID != userID {
		return nil, ErrNotOwner
	}
	if o.Status != "active" {
		return nil, ErrAlreadyRepaid
	}

	p, err := s.store.GetPool(o.PoolID)
	if err != nil {
		return nil, err
	}

	// 释放：SysLendingPool -> 用户可用
	ref := fmt.Sprintf("lend_withdraw:%d", o.ID)
	if err := s.ledger.Transfer(ledger.SysLendingPool, userID, p.Asset, o.Amount, "lend_withdraw", ref); err != nil {
		return nil, err
	}

	o.Status = "withdrawn"
	_ = s.store.UpdateLendOrder(o)

	p.TotalSupply = p.TotalSupply.Sub(o.Amount)
	p.Available = p.Available.Sub(o.Amount)
	_ = s.store.UpdatePool(p)

	return o, nil
}

// Accrue 利息归集：遍历活跃借款，按时间累积利息。
func (s *Service) Accrue(now time.Time) error {
	borrows, err := s.store.ListActiveBorrowOrders()
	if err != nil {
		return err
	}
	for _, o := range borrows {
		p, err := s.store.GetPool(o.PoolID)
		if err != nil || p.Status != PoolActive {
			continue
		}
		// 计算利息：principal * rate * elapsed / (365*24*3600)
		elapsed := float64(now.Unix() - o.CreatedAt)
		if elapsed <= 0 {
			continue
		}
		interest := float64(o.Amount.Value.Int64()) * p.InterestRate * elapsed / (365 * 24 * 3600)
		if interest < 1 {
			continue // 忽略微小利息
		}
		interestAmt := settlement.AssetAmountFromFloat(interest, 8)
		o.InterestAcc = o.InterestAcc.Add(interestAmt)
		_ = s.store.UpdateBorrowOrder(o)

		// 记录利息
		_ = s.store.CreateInterestRecord(&InterestRecord{
			PoolID:     o.PoolID,
			UserID:     o.UserID,
			Type:       "borrow_interest",
			Amount:     interestAmt,
			RecordedAt: now.Unix(),
		})
	}
	return nil
}

// CalcInterestRate 动态利率：基于使用率线性插值。
// 使用率 0% → BaseInterestRate，100% → MaxInterestRate。
func (s *Service) CalcInterestRate(pool *LendingPool) float64 {
	if pool.TotalSupply.Sign() == 0 {
		return s.cfg.BaseInterestRate
	}
	utilization := float64(pool.TotalBorrow.Value.Int64()) / float64(pool.TotalSupply.Value.Int64())
	if utilization > 1.0 {
		utilization = 1.0
	}
	rate := s.cfg.BaseInterestRate + utilization*(s.cfg.MaxInterestRate-s.cfg.BaseInterestRate)
	return math.Round(rate*10000) / 10000 // 4位小数
}

// PoolInfo 返回池的汇总信息。
func (s *Service) PoolInfo(poolID int64) (map[string]interface{}, error) {
	p, err := s.store.GetPool(poolID)
	if err != nil {
		return nil, err
	}
	lendOrders, _ := s.store.ListLendOrdersByPool(poolID)
	borrowOrders, _ := s.store.ListBorrowOrdersByPool(poolID)
	return map[string]interface{}{
		"id":             p.ID,
		"asset":          p.Asset,
		"total_supply":   p.TotalSupply.HumanString(),
		"total_borrow":   p.TotalBorrow.HumanString(),
		"available":      p.Available.HumanString(),
		"interest_rate":  p.InterestRate,
		"collateral_req": p.CollateralReq,
		"status":         p.Status,
		"lenders":        len(lendOrders),
		"borrowers":      len(borrowOrders),
	}, nil
}

// RunLoop 后台利息归集循环。
func (s *Service) RunLoop(ctx context.Context) {
	if s.cfg.AccrueInterval <= 0 {
		return
	}
	ticker := time.NewTicker(s.cfg.AccrueInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Accrue(time.Now()); err != nil && s.log != nil {
				s.log.Warn("lending accrue failed", zap.Error(err))
			}
		}
	}
}

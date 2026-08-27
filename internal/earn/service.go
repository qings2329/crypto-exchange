package earn

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/settlement"
	"go.uber.org/zap"
)

// Config 服务配置。
type Config struct {
	AccrueInterval time.Duration // 后台计息/奖励结算周期（0 表示不启动循环，由测试/管理端点驱动）
}

// Service 理财中心 + Launchpool 业务服务。
type Service struct {
	store Store
	ledger *ledger.Ledger
	cfg    Config
	log    *zap.Logger
	now    nowFunc // 可注入时钟（测试）；生产为 time.Now

	mu sync.Mutex // 串行化所有资金操作：与后台循环、并发请求互斥（同 wealth/staking）
}

// NewService 构造服务。
func NewService(store Store, l *ledger.Ledger, cfg Config, log *zap.Logger) *Service {
	return &Service{store: store, ledger: l, cfg: cfg, log: log, now: time.Now}
}

// SetNowFunc 注入时钟（仅测试使用）。
func (s *Service) SetNowFunc(f func() time.Time) { s.now = f }

// zeroAmt 构造指定小数位的零值定点金额（Value 必须非 nil，避免 big.Int 空指针）。
func zeroAmt(dec int) settlement.AssetAmount {
	return settlement.AssetAmount{Value: new(big.Int), Decimals: dec}
}

// finitePositive 拦截 NaN/Inf 与非正值（M5）。
func finitePositive(x float64) bool {
	return !(x != x || x > 1e308 || x < -1e308) && x > 0
}

// ====================================================================
// 理财中心（Earn Hub）
// ====================================================================

// CreateProduct 创建理财产品（管理员）。
func (s *Service) CreateProduct(p *EarnProduct) error {
	if p.Asset == "" || !settlement.KnownAsset(p.Asset) {
		return ErrUnsupportedAsset
	}
	if p.APY < 0 || p.APY > MaxAnnualRate {
		return ErrInvalidRate
	}
	if p.TermDays < 0 {
		return ErrInvalidTerm
	}
	if !finitePositive(p.MinAmount) {
		return ErrInvalidAmount
	}
	if p.MaxAmount < 0 || (p.MaxAmount > 0 && p.MaxAmount < p.MinAmount) {
		return ErrInvalidAmount
	}
	if p.Status == "" {
		p.Status = ProductOpen
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.CreateProduct(p)
}

// ListProducts 在售产品列表；term 过滤："flexible"→活期，"fixed"→定期，空为全部。
func (s *Service) ListProducts(term string) ([]*EarnProduct, error) {
	ps, err := s.store.ListProducts(ProductOpen)
	if err != nil {
		return nil, err
	}
	switch term {
	case "flexible":
		var out []*EarnProduct
		for _, p := range ps {
			if p.TermDays == 0 {
				out = append(out, p)
			}
		}
		return out, nil
	case "fixed":
		var out []*EarnProduct
		for _, p := range ps {
			if p.TermDays > 0 {
				out = append(out, p)
			}
		}
		return out, nil
	default:
		return ps, nil
	}
}

// Subscribe 用户申购理财：校验 -> 落申购(占唯一ID) -> 账本本金入托管(SysWealth)。
// F1 幂等：ref 锚定申购 ID（earn_sub:<id>），重试安全跳过；F2 定点：全 AssetAmount；
// F3 原子：划转失败回删半成品申购；F5 校验：agreed 风险确认 / 正数 / 起购与限额 / 资产白名单。
func (s *Service) Subscribe(userID, productID int64, amount float64, agreed bool) (*EarnSubscription, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user required")
	}
	if !agreed {
		return nil, ErrAgreementRequired
	}
	if !finitePositive(amount) {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.store.GetProduct(productID)
	if err != nil {
		return nil, err
	}
	if p.Status != ProductOpen {
		return nil, ErrProductNotOpen
	}
	dec := settlement.AssetDecimalsByName(p.Asset)
	amt, err := settlement.AssetAmountFromFloatSafe(amount, dec)
	if err != nil {
		return nil, err
	}
	minAmt, err := settlement.AssetAmountFromFloatSafe(p.MinAmount, dec)
	if err != nil {
		return nil, err
	}
	if amt.Cmp(minAmt) < 0 {
		return nil, ErrBelowMinAmount
	}
	if p.MaxAmount > 0 {
		maxAmt, err := settlement.AssetAmountFromFloatSafe(p.MaxAmount, dec)
		if err != nil {
			return nil, err
		}
		if amt.Cmp(maxAmt) > 0 {
			return nil, ErrAboveMaxAmount
		}
	}
	avail, _, _ := s.ledger.Balance(userID, p.Asset)
	if avail.Cmp(amt) < 0 {
		return nil, ErrInsufficientBal
	}
	sub := &EarnSubscription{
		UserID: userID, ProductID: productID, Asset: p.Asset,
		Principal: amt, Status: SubFunding,
		CreatedAt: s.now(), LastAccrualAt: s.now(),
	}
	if err := s.store.CreateSubscription(sub); err != nil {
		return nil, err
	}
	ref := fmt.Sprintf("earn_sub product=%d user=%d id=%d", productID, userID, sub.ID)
	if err := s.ledger.Transfer(userID, ledger.SysWealth, p.Asset, amt, "earn_subscribe", ref); err != nil {
		_ = s.store.DeleteSubscription(sub.ID) // 回滚半成品
		return nil, fmt.Errorf("lock principal: %w", err)
	}
	sub.Status = SubActive
	if err := s.store.UpdateSubscription(sub); err != nil {
		return nil, err
	}
	return sub, nil
}

// accrueSub 对单笔申购应计收益到 now：SysWealth -> SysWealthYieldPayable 复式划转（F3-1，
// 同 wealth）。ref 带时间戳避免跨周期指纹碰撞被幂等去重误吞。返回本轮回填金额。
func (s *Service) accrueSub(sub *EarnSubscription, p *EarnProduct, now time.Time) settlement.AssetAmount {
	dec := settlement.AssetDecimalsByName(sub.Asset)
	delta := sub.YieldToAmount(now, p.APY, dec)
	if delta.Sign() > 0 {
		ref := fmt.Sprintf("earn_accrue sub=%d t=%d", sub.ID, now.Unix())
		if err := s.ledger.Transfer(ledger.SysWealth, ledger.SysWealthYieldPayable, sub.Asset, delta,
			"earn_accrue", ref); err != nil {
			if s.log != nil {
				s.log.Error("earn accrue: move yield failed", zap.Int64("sub", sub.ID), zap.Error(err))
			}
			return zeroAmt(dec)
		}
		sub.Accrued = sub.Accrued.Add(delta)
		sub.LastAccrualAt = now
	}
	return delta
}

// Redeem 用户赎回：活期随时；定期到期后。赎回前补齐应计，再原子兑付本金+收益。
// F1 幂等：终态短路；F3 原子：Batch 单事务，失败回滚终态。
func (s *Service) Redeem(userID, subID int64) (*EarnSubscription, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	sub, err := s.store.GetSubscription(subID)
	if err != nil {
		return nil, err
	}
	if sub.UserID != userID {
		return nil, ErrNotOwner
	}
	if sub.Status == SubRedeemed {
		return nil, ErrAlreadyRedeemed // 终态短路：幂等
	}
	if sub.Status != SubActive {
		return nil, fmt.Errorf("subscription not active")
	}
	p, err := s.store.GetProduct(sub.ProductID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	// 到期闸口与计息用同一 now（F5-4 统一时钟）。
	if mat := p.Maturity(sub); !mat.IsZero() && now.Before(mat) {
		return nil, ErrLocked
	}
	s.accrueSub(sub, p, now)

	principal := sub.Principal
	yield := sub.Accrued
	ops := []ledger.Op{
		{Kind: ledger.OpTransfer, From: ledger.SysWealth, To: userID, Asset: sub.Asset, Amount: principal,
			Biz: "earn_redeem_principal", Ref: fmt.Sprintf("earn_redeem sub=%d part=principal", sub.ID)},
	}
	if yield.Sign() > 0 {
		ops = append(ops, ledger.Op{Kind: ledger.OpTransfer, From: ledger.SysWealthYieldPayable, To: userID,
			Asset: sub.Asset, Amount: yield, Biz: "earn_redeem_yield",
			Ref: fmt.Sprintf("earn_redeem sub=%d part=yield", sub.ID)})
	}
	// 先落终态再出金（同 wealth）：出金失败则回滚终态，用户可重试。
	sub.Status = SubRedeemed
	sub.RedeemedAt = now
	sub.RedeemedAmount = principal.Add(yield)
	if err := s.store.UpdateSubscription(sub); err != nil {
		return nil, err
	}
	if err := s.ledger.Batch(ops); err != nil {
		sub.Status = SubActive
		sub.RedeemedAt = time.Time{}
		sub.RedeemedAmount = zeroAmt(sub.RedeemedAmount.Decimals)
		_ = s.store.UpdateSubscription(sub)
		return nil, fmt.Errorf("payout: %w", err)
	}
	return sub, nil
}

// MySubscriptions 用户申购列表。accrued 为「已入账 + 自上次入账点按公式投影」的实时读数：
// 读路径只做纯计算不动账本，真正的入账在后台循环/赎回时完成，保证账实最终一致。
type SubscriptionView struct {
	ID             int64     `json:"id"`
	ProductID      int64     `json:"product_id"`
	Asset          string    `json:"asset"`
	Amount         float64   `json:"amount"`
	APY            float64   `json:"apy"`
	TermDays       int       `json:"term_days"`
	StartAt        time.Time `json:"start_at"`
	Status         string    `json:"status"`
	Accrued        float64   `json:"accrued"`
	RedeemedAmount float64   `json:"redeemed_amount,omitempty"`
}

func (s *Service) MySubscriptions(userID int64) ([]*SubscriptionView, error) {
	subs, err := s.store.ListSubscriptions(userID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]*SubscriptionView, 0, len(subs))
	for _, sub := range subs {
		v := &SubscriptionView{
			ID: sub.ID, ProductID: sub.ProductID, Asset: sub.Asset,
			Amount: sub.Principal.HumanFloat(), StartAt: sub.CreatedAt,
			Status: string(sub.Status),
		}
		p, err := s.store.GetProduct(sub.ProductID)
		if err == nil {
			v.APY = p.APY
			v.TermDays = p.TermDays
		}
		switch sub.Status {
		case SubActive:
			v.Accrued = sub.Accrued.Add(sub.YieldToAmount(now, v.APY, sub.Accrued.Decimals)).HumanFloat()
		case SubRedeemed:
			v.RedeemedAmount = sub.RedeemedAmount.HumanFloat()
			v.Accrued = sub.Accrued.HumanFloat()
		}
		out = append(out, v)
	}
	return out, nil
}

// AccrueAll 全量计息一轮（后台循环/管理端点调用）。返回本轮回填总收益。
func (s *Service) AccrueAll(now time.Time) (settlement.AssetAmount, error) {
	total := settlement.AssetAmount{Value: big.NewInt(0), Decimals: 8}
	subs, err := s.store.ListAllSubscriptions()
	if err != nil {
		return total, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sub := range subs {
		if sub.Status != SubActive {
			continue
		}
		p, err := s.store.GetProduct(sub.ProductID)
		if err != nil || p.Status != ProductOpen {
			continue
		}
		d := s.accrueSub(sub, p, now)
		total = total.Add(d)
		if err := s.store.UpdateSubscription(sub); err != nil {
			if s.log != nil {
				s.log.Warn("earn accrue: persist failed", zap.Int64("sub", sub.ID), zap.Error(err))
			}
		}
	}
	return total, nil
}

// RunLoop 后台计息循环。
func (s *Service) RunLoop(ctx context.Context) {
	if s.cfg.AccrueInterval <= 0 {
		return
	}
	t := time.NewTicker(s.cfg.AccrueInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = s.AccrueAll(time.Now())
		}
	}
}

// ====================================================================
// 新币挖矿（Launchpool）
// ====================================================================

// CreateProject 创建 Launchpool 项目（管理员）。
// 奖励代币允许非白名单符号（新币），小数位按默认 8 处理；池资产必须为已知资产（真实质押物）。
func (s *Service) CreateProject(p *LaunchProject) error {
	if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Token) == "" {
		return fmt.Errorf("name and token required")
	}
	if !p.EndsAt.After(p.StartsAt) {
		return ErrInvalidWindow
	}
	if len(p.Pools) == 0 {
		return fmt.Errorf("at least one pool required")
	}
	seen := map[string]bool{}
	for i := range p.Pools {
		pl := &p.Pools[i]
		if pl.Asset == "" || !settlement.KnownAsset(pl.Asset) {
			return ErrUnsupportedAsset
		}
		if pl.APY < 0 || pl.APY > MaxAnnualRate {
			return ErrInvalidRate
		}
		if pl.ID == "" {
			pl.ID = strings.ToLower(pl.Asset)
		}
		key := strings.ToLower(pl.ID)
		if seen[key] {
			return fmt.Errorf("duplicate pool id %q", pl.ID)
		}
		seen[key] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.store.CreateProject(p)
}

// ListProjects 项目列表（含推导状态）。
func (s *Service) ListProjects() ([]*LaunchProject, error) {
	return s.store.ListProjects()
}

// FundProject 为项目充值奖励预算（管理员）：从管理员账户把 token 划入 SysStakingReward 池。
// F1 幂等：ref 带序号时间戳；预算不足后续领取会 fail-safe 失败，不会凭空发币。
func (s *Service) FundProject(adminUserID, projectID int64, amount float64) (settlement.AssetAmount, error) {
	if adminUserID <= 0 {
		return settlement.AssetAmount{}, fmt.Errorf("admin user required")
	}
	if !finitePositive(amount) {
		return settlement.AssetAmount{}, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	p, err := s.store.GetProject(projectID)
	if err != nil {
		return settlement.AssetAmount{}, err
	}
	dec := settlement.AssetDecimalsByName(p.Token)
	amt, err := settlement.AssetAmountFromFloatSafe(amount, dec)
	if err != nil {
		return settlement.AssetAmount{}, err
	}
	ref := fmt.Sprintf("lp_fund project=%d admin=%d t=%d", projectID, adminUserID, s.now().UnixNano())
	if err := s.ledger.Transfer(adminUserID, ledger.SysStakingReward, p.Token, amt, "lp_fund", ref); err != nil {
		return settlement.AssetAmount{}, fmt.Errorf("fund reward pool: %w", err)
	}
	if err := s.store.AddProjectFunded(projectID, amt); err != nil {
		s.log.Warn("lp fund: persist funded failed", zap.Int64("project", projectID), zap.Error(err))
	}
	return amt, nil
}

// accruePos 把自 LastAccrualAt 以来的挖矿奖励增量结入 RewardsPending（纯业务侧累计，不动账本；
// 账本在 Harvest 时经预充值的 SysStakingReward 池支出）。奖励以 token 计价，与质押资产按
// 1:1 约定换算（演示口径；生产应由预言机定价或按每小时定额发放表）。
func (s *Service) accruePos(pos *LaunchPosition, poolAPY float64, now time.Time) {
	from := pos.LastAccrualAt
	if from.IsZero() {
		from = pos.CreatedAt
	}
	nanos := now.Sub(from).Nanoseconds()
	rateScaled := int64(poolAPY*1e8 + 0.5)
	if nanos <= 0 || rateScaled <= 0 || pos.Staked.Value.Sign() <= 0 {
		if nanos > 0 {
			pos.LastAccrualAt = now
		}
		return
	}
	// 先在「池资产小数位」空间按比例算出奖励额，再换算到 token 小数位空间累加
	// （质押额与奖励币是两种资产，小数位可能不同，如 USDT@6 位挖 NEW@8 位）。
	num := new(big.Int).Mul(pos.Staked.Value, big.NewInt(rateScaled))
	num.Mul(num, big.NewInt(nanos))
	den := big.NewInt(365 * 24) // 年化按小时数
	den.Mul(den, big.NewInt(3600*1e9))
	den.Mul(den, big.NewInt(1e8))
	raw := new(big.Int).Quo(num, den)
	tokDec := settlement.AssetDecimalsByName(pos.Token)
	delta := settlement.AssetAmount{Value: raw, Decimals: settlement.AssetDecimalsByName(pos.Asset)}.ToDecimals(tokDec)
	if delta.Sign() > 0 {
		pos.RewardsPending = pos.RewardsPending.Add(delta)
	}
	pos.LastAccrualAt = now
}

// poolOf 解析项目的池（大小写不敏感）。
func poolOf(p *LaunchProject, poolID string) (LaunchPool, error) {
	pl, ok := p.Pool(poolID)
	if !ok {
		return LaunchPool{}, ErrPoolNotFound
	}
	return pl, nil
}

// Stake 质押进池：活动须 ongoing。首笔质押创建仓位（funding 两阶段，同 wealth subscribe）。
// F1：每笔质押 ref 唯一（lp_stake:<pos>:<seq>），重复质押不被幂等去重误吞；
// F3：先落仓位占 ID，划转失败删仓位回滚；存量质押期奖励先结清再变更基数。
func (s *Service) Stake(userID, projectID int64, poolID string, amount float64) (*LaunchPosition, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user required")
	}
	if !finitePositive(amount) {
		return nil, ErrInvalidAmount
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	p, err := s.store.GetProject(projectID)
	if err != nil {
		return nil, err
	}
	if p.Status(now) != "ongoing" {
		return nil, ErrProjectNotOngoing
	}
	pool, err := poolOf(p, poolID)
	if err != nil {
		return nil, err
	}
	dec := settlement.AssetDecimalsByName(pool.Asset)
	amt, err := settlement.AssetAmountFromFloatSafe(amount, dec)
	if err != nil {
		return nil, err
	}
	pos, ferr := s.store.FindPosition(userID, projectID, pool.ID)
	wasNew := ferr == ErrPositionNotFound
	if ferr != nil && !wasNew {
		return nil, ferr
	}
	if wasNew {
		pos = &LaunchPosition{
			UserID: userID, ProjectID: projectID, PoolID: pool.ID,
			Asset: pool.Asset, Token: p.Token, Status: PosFunding,
			CreatedAt: now, LastAccrualAt: now,
			RewardsPending: zeroAmt(settlement.AssetDecimalsByName(p.Token)),
			HarvestedTotal: zeroAmt(settlement.AssetDecimalsByName(p.Token)),
		}
		// 先落库占唯一 ID，保证 ref 可锚定到真实仓位（F1）。
		if err := s.store.UpsertPosition(pos); err != nil {
			return nil, err
		}
	} else {
		_ = s.accruePosBeforeMutation(pos, pool.APY, now)
	}
	pos.StakeSeq++
	if err := s.ledger.Transfer(userID, ledger.SysStaking, pool.Asset, amt, "lp_stake",
		fmt.Sprintf("lp_stake pos=%d seq=%d", pos.ID, pos.StakeSeq)); err != nil {
		pos.StakeSeq--
		if wasNew {
			_ = s.store.DeletePosition(pos.ID)
		}
		return nil, fmt.Errorf("stake: %w", err)
	}
	pos.Staked = pos.Staked.Add(amt)
	pos.Status = PosActive
	if err := s.store.UpsertPosition(pos); err != nil {
		return nil, err
	}
	return pos, nil
}

// accruePosBeforeMutation 变更质押额前先把存量质押期的奖励结入 Pending（防基数跳变吞奖励）。
func (s *Service) accruePosBeforeMutation(pos *LaunchPosition, poolAPY float64, now time.Time) bool {
	before := pos.RewardsPending.Value.String()
	s.accruePos(pos, poolAPY, now)
	return pos.RewardsPending.Value.String() != before
}

// Unstake 解押：amount<=0 视为全额。解押前先结清存量期奖励。
// F1：ref 唯一（lp_unstake:<pos>:<seq>）；F3：先改账本再更新仓位，失败回滚内存态并返回错误
// （账本为唯一事实源，未持久化即视为未发生）。
func (s *Service) Unstake(userID, positionID int64, amount float64) (*LaunchPosition, error) {
	if userID <= 0 {
		return nil, fmt.Errorf("user required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pos, err := s.store.GetPosition(positionID)
	if err != nil {
		return nil, err
	}
	if pos.UserID != userID {
		return nil, ErrNotOwner
	}
	if pos.Status != PosActive || pos.Staked.Sign() <= 0 {
		return nil, ErrInvalidUnstake
	}
	now := s.now()
	p, err := s.store.GetProject(pos.ProjectID)
	if err != nil {
		return nil, err
	}
	pool, err := poolOf(p, pos.PoolID)
	if err != nil {
		return nil, err
	}
	_ = s.accruePosBeforeMutation(pos, pool.APY, now)

	amt := pos.Staked
	if amount > 0 {
		dec := settlement.AssetDecimalsByName(pos.Asset)
		a, aerr := settlement.AssetAmountFromFloatSafe(amount, dec)
		if aerr != nil {
			return nil, aerr
		}
		if a.Cmp(pos.Staked) > 0 {
			return nil, ErrInvalidUnstake
		}
		amt = a
	}
	pos.UnstakeSeq++
	seq := pos.UnstakeSeq
	if err := s.ledger.Transfer(ledger.SysStaking, userID, pos.Asset, amt, "lp_unstake",
		fmt.Sprintf("lp_unstake pos=%d seq=%d", pos.ID, seq)); err != nil {
		pos.UnstakeSeq-- // 回滚序号，允许重试生成相同 ref
		return nil, fmt.Errorf("unstake: %w", err)
	}
	pos.Staked = pos.Staked.Sub(amt)
	if pos.Staked.Sign() == 0 {
		pos.Status = PosWithdrawn
	}
	if err := s.store.UpsertPosition(pos); err != nil {
		return nil, err
	}
	return pos, nil
}

// Harvest 领取奖励：从预充值的 SysStakingReward(token) 池划付。预算耗尽则失败（fail-safe，
// 不会凭空发币）。F1：ref 唯一（lp_harvest:<pos>:<seq>）；F3：账本失败回滚 Pending 结转。
func (s *Service) Harvest(userID, positionID int64) (claimed settlement.AssetAmount, err error) {
	if userID <= 0 {
		return settlement.AssetAmount{}, fmt.Errorf("user required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	pos, err := s.store.GetPosition(positionID)
	if err != nil {
		return settlement.AssetAmount{}, err
	}
	if pos.UserID != userID {
		return settlement.AssetAmount{}, ErrNotOwner
	}
	if pos.Status == PosFunding {
		return settlement.AssetAmount{}, ErrNothingToHarvest
	}
	now := s.now()
	p, perr := s.store.GetProject(pos.ProjectID)
	if perr != nil {
		return settlement.AssetAmount{}, perr
	}
	if pool, perr2 := poolOf(p, pos.PoolID); perr2 == nil {
		_ = s.accruePosBeforeMutation(pos, pool.APY, now)
	}
	if pos.RewardsPending.Sign() <= 0 {
		return settlement.AssetAmount{}, ErrNothingToHarvest
	}
	claimed = pos.RewardsPending
	// 预算闸口（fail-safe）：SysStakingReward 为系统账户允许透支，账本不会因余额不足拒绝，
	// 故在此显式校验奖励池余额，预算耗尽即拒付（不凭空发币）。
	poolBal, _, _ := s.ledger.Balance(ledger.SysStakingReward, pos.Token)
	if poolBal.Cmp(claimed) < 0 {
		// 拒付前先把本次结出的奖励持久化，避免用户应得奖励因预算不足而丢失。
		if uerr := s.store.UpsertPosition(pos); uerr != nil {
			return settlement.AssetAmount{}, uerr
		}
		return settlement.AssetAmount{}, ErrPoolExhausted
	}
	pos.HarvestSeq++
	seq := pos.HarvestSeq
	// 先结转 Pending→Harvested 并落库，账本划付失败则回滚（同 redeem 先终态再出金模式）。
	pos.HarvestedTotal = pos.HarvestedTotal.Add(claimed)
	pos.RewardsPending = zeroAmt(claimed.Decimals)
	if err := s.store.UpsertPosition(pos); err != nil {
		pos.HarvestSeq--
		return settlement.AssetAmount{}, err
	}
	if err := s.ledger.Transfer(ledger.SysStakingReward, userID, pos.Token, claimed, "lp_harvest",
		fmt.Sprintf("lp_harvest pos=%d seq=%d", pos.ID, seq)); err != nil {
		// 回滚结转，用户可重试；池预算不足时持续失败（fail-safe 不凭空发币）。
		pos.HarvestSeq--
		pos.RewardsPending = claimed
		pos.HarvestedTotal = pos.HarvestedTotal.Sub(claimed)
		_ = s.store.UpsertPosition(pos)
		return settlement.AssetAmount{}, fmt.Errorf("harvest: %w", err)
	}
	return claimed, nil
}

// PositionView 仓位读模型（对齐前端 LaunchPosition 契约；rewards 含实时投影）。
type PositionView struct {
	ID        int64   `json:"id"`
	ProjectID int64   `json:"project_id"`
	PoolID    string  `json:"pool_id"`
	Staked    float64 `json:"staked"`
	Rewards   float64 `json:"rewards"`
	Status    string  `json:"status,omitempty"`
}

func (s *Service) MyPositions(userID int64) ([]*PositionView, error) {
	positions, err := s.store.ListPositions(userID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	projCache := map[int64]*LaunchProject{}
	out := make([]*PositionView, 0, len(positions))
	for _, pos := range positions {
		rewards := pos.RewardsPending
		if pos.Status == PosActive {
			if p, ok := projCache[pos.ProjectID]; ok {
				if pool, e := poolOf(p, pos.PoolID); e == nil {
					cp := *pos
					s.accruePos(&cp, pool.APY, now)
					rewards = cp.RewardsPending
				}
			} else if p, e := s.store.GetProject(pos.ProjectID); e == nil {
				projCache[pos.ProjectID] = p
				if pool, e2 := poolOf(p, pos.PoolID); e2 == nil {
					cp := *pos
					s.accruePos(&cp, pool.APY, now)
					rewards = cp.RewardsPending
				}
			}
		}
		out = append(out, &PositionView{
			ID: pos.ID, ProjectID: pos.ProjectID, PoolID: pos.PoolID,
			Staked: pos.Staked.HumanFloat(), Rewards: rewards.HumanFloat(),
			Status: string(pos.Status),
		})
	}
	return out, nil
}

// Reconcile 业务对账（F3）：在押本金之和 vs SysStaking 余额；已领奖励之和 vs SysStakingReward
// 余额（token）。偏差非 0 应告警排查。
func (s *Service) Reconcile() map[string]settlement.AssetAmount {
	dev := make(map[string]settlement.AssetAmount)
	add := func(key string, d settlement.AssetAmount) {
		if prev, ok := dev[key]; ok {
			dev[key] = prev.Add(d)
		} else {
			dev[key] = d
		}
	}
	// 奖励池口径：余额 应等于 Σ已充值预算 - Σ已领取。预算记在项目侧（funded），领取记在仓位侧。
	projects, _ := s.store.ListProjects()
	fundedByToken := map[string]settlement.AssetAmount{}
	for _, pj := range projects {
		f := pj.FundedTotal
		if f.Value == nil {
			continue
		}
		if prev, ok := fundedByToken[pj.Token]; ok {
			fundedByToken[pj.Token] = prev.Add(f)
		} else {
			fundedByToken[pj.Token] = f
		}
	}
	positions, _ := s.store.ListAllPositions()
	harvestedByToken := map[string]settlement.AssetAmount{}
	stakedByAsset := map[string]settlement.AssetAmount{}
	for _, pos := range positions {
		prev := stakedByAsset[pos.Asset]
		if prev.Value == nil {
			prev = zeroAmt(pos.Staked.Decimals)
		}
		stakedByAsset[pos.Asset] = settlement.AssetAmount{
			Value:    new(big.Int).Add(prev.Value, pos.Staked.Value),
			Decimals: pos.Staked.Decimals,
		}
		hv := harvestedByToken[pos.Token]
		if hv.Value == nil {
			hv = zeroAmt(pos.HarvestedTotal.Decimals)
		}
		harvestedByToken[pos.Token] = hv.Add(pos.HarvestedTotal)
	}
	for asset, staked := range stakedByAsset {
		bal, _, _ := s.ledger.Balance(ledger.SysStaking, asset)
		add("staking:"+asset, bal.Sub(staked))
	}
	tokens := map[string]bool{}
	for t := range fundedByToken {
		tokens[t] = true
	}
	for t := range harvestedByToken {
		tokens[t] = true
	}
	for token := range tokens {
		hb, _, _ := s.ledger.Balance(ledger.SysStakingReward, token)
		expected := fundedByToken[token]
		if h, ok := harvestedByToken[token]; ok {
			if expected.Value == nil {
				expected = zeroAmt(h.Decimals)
			}
			expected = expected.Sub(h)
		}
		if expected.Value == nil {
			continue
		}
		add("reward:"+token, hb.Sub(expected))
	}
	for k, v := range dev {
		if v.Sign() == 0 {
			delete(dev, k)
		}
	}
	return dev
}

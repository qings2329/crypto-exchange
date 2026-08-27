// Package earn 实现理财中心（Earn Hub）与新币挖矿（Launchpool）业务线。
//
// 业务模型（中央托管模型，与 wealth/staking 同构）：
//   - 理财产品（EarnProduct）：平台发行的活期（term_days=0）/定期（term_days>0）产品，
//     含底层资产、年化收益率、起购/限额、上下架状态。
//   - 理财申购（EarnSubscription）：用户申购生成的持仓，本金转入 SysWealth 托管；
//     收益按「本金 × 年化 × 持有小时 / 8760」定点整数计息（复用 wealth 的 #47 整数化方案），
//     应计收益经 SysWealth→SysWealthYieldPayable 复式划转记账；定期产品到期前不可赎回。
//   - Launchpool 项目（LaunchProject）：平台以新币（project.token）为激励的限时挖矿活动，
//     含多个质押池（如 USDT 池 / FDUSD 池），活动状态由起止时间推导（upcoming/ongoing/ended）。
//   - 挖矿仓位（LaunchPosition）：用户在某项目某池的质押仓位。质押本金转入 SysStaking 托管；
//     新币奖励预算由管理员预先充值入 SysStakingReward（token 资产），领取时经该账户支出，
//     预算耗尽则领取失败（fail-safe，不会凭空发币）。奖励按「质押额 × 池 APY × 时长 / 年」
//     以 token 计价（与质押资产 1:1 约定，见 service.go 说明），在仓位变更/领取边界增量结算。
//
// 分层：本包为 Service + Store 层，HTTP 路由在 handler.go，装配在 cmd/earn/main.go。
// 所有持久化表名以 ce_ 开头（见 docs/CONVENTIONS.md）。
package earn

import (
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// 领域错误。
var (
	ErrProductNotFound   = errors.New("product not found")
	ErrProjectNotFound   = errors.New("launch project not found")
	ErrPoolNotFound      = errors.New("launch pool not found")
	ErrPositionNotFound  = errors.New("launch position not found")
	ErrInvalidAmount     = errors.New("amount must be positive")
	ErrInvalidRate       = errors.New("annual rate must be non-negative")
	ErrInvalidTerm       = errors.New("term days must be >= 0")
	ErrBelowMinAmount    = errors.New("amount below product minimum")
	ErrAboveMaxAmount    = errors.New("amount above product maximum")
	ErrProductNotOpen    = errors.New("product not open")
	ErrInsufficientBal   = errors.New("insufficient available balance")
	ErrNotOwner          = errors.New("not the owner")
	ErrAlreadyRedeemed   = errors.New("subscription already redeemed")
	ErrLocked            = errors.New("fixed term product is still locked before maturity")
	ErrUnsupportedAsset  = errors.New("unsupported asset")
	ErrAgreementRequired = errors.New("risk agreement not accepted")
	ErrProjectNotOngoing = errors.New("launch project is not ongoing")
	ErrInvalidUnstake    = errors.New("unstake amount exceeds staked")
	ErrNothingToHarvest  = errors.New("no pending rewards to harvest")
	ErrPoolExhausted     = errors.New("launch reward pool budget exhausted")
	ErrInvalidWindow     = errors.New("ends_at must be after starts_at")
)

// MaxAnnualRate 年化收益率安全上限（人类单位），护栏防止误设天文费率拖垮托管账户（F5-3）。
const MaxAnnualRate = 1000.0

// SubscriptionStatus 申购状态。
type SubscriptionStatus string

const (
	SubActive   SubscriptionStatus = "active"   // 持有中（已出资）
	SubFunding  SubscriptionStatus = "funding"  // 瞬态：已落库、本金划转进行中
	SubRedeemed SubscriptionStatus = "redeemed" // 已赎回（终态）
)

// ProductStatus 产品上下架状态。
type ProductStatus string

const (
	ProductOpen   ProductStatus = "open"
	ProductClosed ProductStatus = "closed"
)

// PositionStatus 仓位状态。
type PositionStatus string

const (
	PosActive    PositionStatus = "active"     // 质押中
	PosFunding   PositionStatus = "funding"    // 瞬态：首笔质押划转进行中
	PosWithdrawn PositionStatus = "withdrawn"  // 已全部解押（终态；仍可领取剩余奖励）
)

// EarnProduct 是一个理财产品。字段命名对齐前端 EarnProduct 契约。
type EarnProduct struct {
	ID        int64         `json:"id"`
	Name      string        `json:"name"`
	Asset     string        `json:"asset"`
	TermDays  int           `json:"term_days"` // 0 = 活期
	APY       float64       `json:"apy"`       // 0.065 = 6.5%
	MinAmount float64       `json:"min_amount"`
	MaxAmount float64       `json:"max_amount"` // 0 表示不限额
	Status    ProductStatus `json:"status"`
	CreatedAt time.Time     `json:"-"`
	UpdatedAt time.Time     `json:"-"`
}

// Maturity 返回定期产品的到期时间（活期为零值）。
func (p *EarnProduct) Maturity(s *EarnSubscription) time.Time {
	if p.TermDays <= 0 {
		return time.Time{}
	}
	return s.CreatedAt.AddDate(0, 0, p.TermDays)
}

// EarnSubscription 是用户的一笔理财申购。
type EarnSubscription struct {
	ID             int64                  `json:"-"`
	UserID         int64                  `json:"-"`
	ProductID      int64                  `json:"-"`
	Asset          string                 `json:"-"`
	Principal      settlement.AssetAmount `json:"-"`
	Accrued        settlement.AssetAmount `json:"-"` // 已入账应计收益
	Status         SubscriptionStatus     `json:"-"`
	CreatedAt      time.Time              `json:"-"`
	LastAccrualAt  time.Time              `json:"-"`
	RedeemedAt     time.Time              `json:"-"`
	RedeemedAmount settlement.AssetAmount `json:"-"`
}

// YieldToAmount 计算从 last_accrual_at（未计则取 created_at）到 t 的应计收益，
// 全程定点整数运算（同 wealth #47 方案，年化按 8760 小时）：
//
//	delta = principal.Value * round(apy*1e8) * nanos / (8760 * 3.6e12 * 1e8)
func (s *EarnSubscription) YieldToAmount(t time.Time, apy float64, dec int) settlement.AssetAmount {
	from := s.LastAccrualAt
	if from.IsZero() {
		from = s.CreatedAt
	}
	if t.Before(from) {
		return settlement.AssetAmount{Decimals: dec}
	}
	nanos := t.Sub(from).Nanoseconds()
	if nanos <= 0 {
		return settlement.AssetAmount{Decimals: dec}
	}
	rateScaled := int64(apy*1e8 + 0.5)
	if rateScaled <= 0 {
		return settlement.AssetAmount{Decimals: dec}
	}
	num := new(big.Int).Mul(s.Principal.Value, big.NewInt(rateScaled))
	num.Mul(num, big.NewInt(nanos))
	den := big.NewInt(8760)
	den.Mul(den, big.NewInt(3600*1e9))
	den.Mul(den, big.NewInt(1e8))
	return settlement.AssetAmount{Value: new(big.Int).Quo(num, den), Decimals: dec}
}

// LaunchPool 是 Launchpool 项目下的一个质押池。ID 即池键（前端 pool_id，大小写不敏感）。
type LaunchPool struct {
	ID    string  `json:"id"`    // 如 "usdt"、"fdusd"
	Asset string  `json:"asset"` // 质押资产，如 "USDT"
	APY   float64 `json:"apy"`   // 年化奖励率（以项目 token 计价，1:1 约定）
}

// LaunchProject 是一个新币挖矿项目。
type LaunchProject struct {
	ID          int64        `json:"-"`
	Name        string       `json:"-"`
	Token       string       `json:"-"` // 奖励代币资产名，如 "NEW"
	TotalSupply string       `json:"-"`
	StartsAt    time.Time    `json:"-"`
	EndsAt      time.Time    `json:"-"`
	Pools       []LaunchPool           `json:"-"`
	FundedTotal settlement.AssetAmount `json:"-"` // 已充值奖励预算累计（token 计价）
	CreatedAt   time.Time              `json:"-"`
}

// Status 由当前时间推导活动状态（不落库，避免时钟漂移写坏状态）。
func (p *LaunchProject) Status(now time.Time) string {
	switch {
	case now.Before(p.StartsAt):
		return "upcoming"
	case now.Before(p.EndsAt):
		return "ongoing"
	default:
		return "ended"
	}
}

// Pool 按池 ID（大小写不敏感）查池。
func (p *LaunchProject) Pool(id string) (LaunchPool, bool) {
	for _, pl := range p.Pools {
		if strings.EqualFold(pl.ID, id) {
			return pl, true
		}
	}
	return LaunchPool{}, false
}

// LaunchPosition 是用户在某项目某池的挖矿仓位。金额全为定点（F2）：
// Staked 以池资产计价；RewardsPending/HarvestedTotal 以项目 token 计价。
type LaunchPosition struct {
	ID              int64                  `json:"-"`
	UserID          int64                  `json:"-"`
	ProjectID       int64                  `json:"-"`
	PoolID          string                 `json:"-"`
	Asset           string                 `json:"-"` // 质押资产（推导小数位）
	Token           string                 `json:"-"` // 奖励代币（推导小数位）
	Staked          settlement.AssetAmount `json:"-"`
	RewardsPending  settlement.AssetAmount `json:"-"`
	HarvestedTotal  settlement.AssetAmount `json:"-"`
	Status          PositionStatus         `json:"-"`
	StakeSeq        int64                  `json:"-"` // 流水序号：生成唯一 ref 防指纹碰撞（F1）
	UnstakeSeq      int64                  `json:"-"`
	HarvestSeq      int64                  `json:"-"`
	CreatedAt       time.Time              `json:"-"`
	LastAccrualAt   time.Time              `json:"-"`
}

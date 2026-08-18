package copytrade

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// Config 是跟单服务配置。
type Config struct {
	CopyFeeRate  float64 // 平台复制费率（按粉丝复制名义额的占比，如 0.001=0.1%）
	MinNotional  float64 // 粉丝复制名义额下限（计价币），低于则跳过，避免粉尘单
	DefaultMarket string // 复制下单目标市场（默认 spot；v1 仅支持现货跟单）
}

// Service 跟单服务：消费撮合成交事件，识别被跟单的 lead，按比例复制下单并结算平台复制费。
type Service struct {
	store    Store
	ledger   *ledger.Ledger // copytrade 自身账本（仅用于平台复制费结算到 SysCopyTradeFee）
	exec     OrderExecutor
	cfg      Config
	log      *zap.Logger
	mu       sync.Mutex
	processed map[string]bool // eventID -> 已处理（F1 全局去重，跨粉丝/跨 lead）
}

// NewService 构造跟单服务。exec 为代粉丝下单执行器（默认 HTTPExecutor 指向 spot/futures）。
func NewService(store Store, lg *ledger.Ledger, exec OrderExecutor, cfg Config, log *zap.Logger) *Service {
	if cfg.CopyFeeRate <= 0 {
		cfg.CopyFeeRate = 0.001
	}
	if cfg.DefaultMarket == "" {
		cfg.DefaultMarket = "spot"
	}
	if exec == nil {
		exec = NewHTTPExecutor("", "")
	}
	return &Service{store: store, ledger: lg, exec: exec, cfg: cfg, log: log, processed: map[string]bool{}}
}

// CreateLead 注册用户为带单高手（F4：uid 即 lead id）。
func (s *Service) CreateLead(uid int64, name, bio string) (*LeadTrader, error) {
	l := &LeadTrader{ID: uid, Name: name, Bio: bio, Status: LeadActive}
	if err := s.store.CreateLead(l); err != nil {
		return nil, err
	}
	return l, nil
}

// CloseLead 关闭带单（F4：仅本人可关闭；管理员路径由 handler 直接调用 store.CloseLead）。
func (s *Service) CloseLead(uid, id int64) error {
	l, err := s.store.GetLead(id)
	if err != nil {
		return err
	}
	if l.ID != uid {
		return ErrNotOwner
	}
	return s.store.CloseLead(id)
}

// RegisterFollow 粉丝关注某 lead 并授权跟单（F4：uid 即 follower）。
func (s *Service) RegisterFollow(uid, leadID int64, copyRatio, allocatedAmount float64, followerToken string) (*Follow, error) {
	if copyRatio <= 0 {
		return nil, ErrInvalidParam
	}
	if followerToken == "" {
		return nil, ErrInvalidParam
	}
	l, err := s.store.GetLead(leadID)
	if err != nil {
		return nil, err
	}
	if l.Status != LeadActive {
		return nil, ErrInvalidParam
	}
	f := &Follow{
		LeadID:         leadID,
		FollowerID:     uid,
		CopyRatio:      copyRatio,
		AllocatedAmount: allocatedAmount,
		FollowerToken:  followerToken,
		Status:         FollowActive,
	}
	if err := s.store.CreateFollow(f); err != nil {
		return nil, err
	}
	return f, nil
}

// StopFollow 停止跟单（F4：仅本人可操作）。
func (s *Service) StopFollow(uid, followID int64) error {
	f, err := s.store.GetFollow(followID)
	if err != nil {
		return err
	}
	if f.FollowerID != uid {
		return ErrNotOwner
	}
	f.Status = FollowStopped
	f.StoppedAt = time.Now().Unix()
	return s.store.UpdateFollow(f)
}

// ---- 管理侧（AdminGuard 已校验调用者身份）----

// AdminCloseLead 管理员强制关闭带单（绕过本人校验）。
func (s *Service) AdminCloseLead(id int64) error {
	if _, err := s.store.GetLead(id); err != nil {
		return err
	}
	return s.store.CloseLead(id)
}

// AdminListLeads 全量带单列表。
func (s *Service) AdminListLeads() ([]*LeadTrader, error) { return s.store.ListAllLeads() }

// AdminListFollows 全量跟单关系列表。
func (s *Service) AdminListFollows() ([]*Follow, error) { return s.store.ListAllFollows() }

// AdminListCopies 全量复制成交列表。
func (s *Service) AdminListCopies() ([]*CopyRecord, error) { return s.store.ListAllCopies() }

// ListActiveLeads 列出在售带单高手。
func (s *Service) ListActiveLeads() ([]*LeadTrader, error) { return s.store.ListActiveLeads() }

// MyFollows 列出某粉丝的全部跟单关系。
func (s *Service) MyFollows(uid int64) ([]*Follow, error) { return s.store.ListFollowsByFollower(uid) }

// MyCopies 列出某粉丝的复制成交。
func (s *Service) MyCopies(uid int64) ([]*CopyRecord, error) { return s.store.ListCopiesByFollower(uid) }

// OnTrade 是成交事件入口：识别被跟单的 lead 并复制其成交给各粉丝。
// F1 幂等：同 eventID 全局只复制一次（processed map + 库唯一键双保险）；
// F4 授权：复制下单用粉丝授权 token，由 spot/futures 校验，copytrade 不接触粉丝私钥；
// F5 边界：仅处理计价资产已知的现货交易对，且名义额超过下限才复制；
// 平台复制费以定点（settlement.AssetAmount）从粉丝账户结算入 SysCopyTradeFee。
func (s *Service) OnTrade(ctx context.Context, ev mq.TradeEvent) {
	eid := eventID(ev)
	s.mu.Lock()
	if s.processed[eid] {
		s.mu.Unlock()
		return
	}
	s.processed[eid] = true
	s.mu.Unlock()

	for _, leadID := range uniqueIDs(ev.TakerID, ev.MakerID) {
		follows, err := s.store.ListFollowsByLead(leadID)
		if err != nil || len(follows) == 0 {
			continue
		}
		// lead 在此成交中的方向：taker 即 TakerSide；maker 为对手方，方向相反。
		leadSide := ev.TakerSide
		if leadID == ev.MakerID {
			leadSide = opposite(ev.TakerSide)
		}
		for _, f := range follows {
			s.replicate(ctx, ev, f, leadID, leadSide, eid)
		}
	}
}

func (s *Service) replicate(ctx context.Context, ev mq.TradeEvent, f *Follow, leadID int64, leadSide, eid string) {
	// F1：幂等键 (eid, f.ID)。
	if existing, _ := s.store.GetCopy(eid, f.ID); existing != nil {
		return
	}
	notional := ev.Price * ev.Qty
	followerNotional := notional * f.CopyRatio
	if f.AllocatedAmount > 0 && followerNotional > f.AllocatedAmount {
		followerNotional = f.AllocatedAmount
	}
	// F5：名义额低于下限则跳过（粉尘单不复制）。
	if followerNotional <= s.cfg.MinNotional {
		return
	}
	quote := quoteAsset(ev.Symbol)
	if quote == "" || !settlement.KnownAsset(quote) {
		// F5：不支持的计价资产，跳过——避免以错误资产结算复制费。
		if s.log != nil {
			s.log.Warn("copytrade: skip unsupported quote asset", zap.String("symbol", ev.Symbol), zap.String("quote", quote))
		}
		return
	}
	qty := followerNotional / maxf(ev.Price, 1e-8)
	clientOID := fmt.Sprintf("copytrade:%d:%s", f.ID, eid)

	rec := &CopyRecord{
		EventID:    eid,
		LeadID:     leadID,
		FollowID:   f.ID,
		FollowerID: f.FollowerID,
		Symbol:     ev.Symbol,
		Side:       leadSide,
		Price:      ev.Price,
		Qty:        qty,
		Notional:   followerNotional,
		CreatedAt:  time.Now().Unix(),
	}

	// F4：代粉丝以授权 token 下单，下游 spot/futures 校验 token->userID 并做资金预冻。
	exID, err := s.exec.Execute(ctx, f.FollowerToken, s.cfg.DefaultMarket, ev.Symbol, leadSide, ev.Price, qty, clientOID)
	rec.ExchangeOrderID = exID
	if err != nil {
		rec.Status = CopyFailed
		if s.log != nil {
			s.log.Warn("copytrade: follower order failed", zap.Int64("follow", f.ID), zap.Error(err))
		}
		_ = s.store.CreateCopy(rec)
		return
	}
	rec.Status = CopyDone

	// F2/F1 平台复制费结算：fee = followerNotional * CopyFeeRate，定点入账 SysCopyTradeFee。
	if s.ledger != nil && followerNotional > 0 {
		fee := followerNotional * s.cfg.CopyFeeRate
		dec := settlement.AssetDecimalsByName(quote)
		amt := settlement.AssetAmountFromFloat(fee, dec)
		ref := fmt.Sprintf("copytrade:%d:%s", f.ID, eid)
		if terr := s.ledger.Transfer(f.FollowerID, ledger.SysCopyTradeFee, quote, amt, "copytrade_fee", ref); terr != nil {
			// 粉丝在 copytrade 账本余额不足：仅记录告警，不阻断已下的市价单。
			if s.log != nil {
				s.log.Warn("copytrade: fee transfer failed (insufficient copytrade balance?)",
					zap.Int64("follower", f.FollowerID), zap.Error(terr))
			}
			rec.FeeAmount = 0
		} else {
			rec.FeeAmount = fee
		}
	}
	_ = s.store.CreateCopy(rec)
}

// eventID 由成交字段生成稳定指纹，用于 F1 全局去重。
func eventID(ev mq.TradeEvent) string {
	return fmt.Sprintf("%s:%d:%d:%s:%d:%.8f:%.8f", ev.Symbol, ev.TakerID, ev.MakerID, ev.TakerSide, ev.Ts, ev.Price, ev.Qty)
}

func uniqueIDs(ids ...int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id != 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

func opposite(side string) string {
	if side == "buy" {
		return "sell"
	}
	return "buy"
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

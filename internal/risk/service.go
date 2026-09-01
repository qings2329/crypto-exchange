package risk

import (
	"fmt"
	"strconv"
	"time"

	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// Service 是风控业务逻辑层，仅依赖 Store 接口。
type Service struct {
	store Store
}

// New 创建风控服务。
func New(store Store) *Service {
	return &Service{store: store}
}

// AddRule 新增或更新规则（ID>0 视为更新）。新建规则默认启用。
func (s *Service) AddRule(r *RiskRule) (*RiskRule, error) {
	if r.Kind == "" {
		return nil, fmt.Errorf("kind required")
	}
	if r.Name == "" {
		r.Name = r.Kind
	}
	if r.Scope == "" {
		r.Scope = ScopeGlobal
	}
	if r.ID == 0 {
		r.Enabled = true // 新建默认启用；如需禁用请更新时置 enabled=false
	}
	return s.store.UpsertRule(r)
}

// ListRules 列出规则，kind 为空表示全部。
func (s *Service) ListRules(kind string) ([]*RiskRule, error) {
	return s.store.ListRules(kind)
}

// AddBlacklist 加入黑名单（user_id 字符串或链上地址）。
func (s *Service) AddBlacklist(target, kind, reason string) (*BlacklistEntry, error) {
	if target == "" || kind == "" {
		return nil, fmt.Errorf("target and kind required")
	}
	return s.store.AddBlacklist(&BlacklistEntry{Target: target, Kind: kind, Reason: reason})
}

// RemoveBlacklist 移除黑名单。
func (s *Service) RemoveBlacklist(target string) error {
	return s.store.RemoveBlacklist(target)
}

// IsBlacklisted 判断目标是否在黑名单。
func (s *Service) IsBlacklisted(target string) (bool, error) {
	return s.store.IsBlacklisted(target)
}

// ListBlacklist 列出黑名单，kind 为空表示全部。
func (s *Service) ListBlacklist(kind string) ([]*BlacklistEntry, error) {
	return s.store.ListBlacklist(kind)
}

// ListEvents 列出触发事件，userID=0 表示全部。
func (s *Service) ListEvents(userID int64, limit int) ([]*RiskEvent, error) {
	return s.store.ListEvents(userID, limit)
}

// CheckWithdraw 评估提现风控：先查用户与地址黑名单，再按 withdraw_limit
// 规则校验单笔限额与 KYC 等级。返回是否放行。
func (s *Service) CheckWithdraw(userID int64, asset string, amount float64, kycLevel int, addr string) (CheckResult, error) {
	uidStr := strconv.FormatInt(userID, 10)
	// 黑名单查询出错必须 fail-closed：丢弃错误会让 DB 抖动期间黑名单完全失效，
	// 被封用户/地址照样放行提现。
	ok, err := s.store.IsBlacklisted(uidStr)
	if err != nil {
		return CheckResult{Allowed: false, Reason: "blacklist lookup failed"}, err
	}
	if ok {
		s.record(userID, KindWithdrawLimit, fmt.Sprintf("user %s in blacklist", uidStr))
		return CheckResult{Allowed: false, Reason: "user blacklisted"}, nil
	}
	if addr != "" {
		ok, err := s.store.IsBlacklisted(addr)
		if err != nil {
			return CheckResult{Allowed: false, Reason: "blacklist lookup failed"}, err
		}
		if ok {
			s.record(userID, KindWithdrawLimit, fmt.Sprintf("address %s in blacklist", addr))
			return CheckResult{Allowed: false, Reason: "address blacklisted"}, nil
		}
	}
	rule, err := s.matchRule(KindWithdrawLimit, asset, userID)
	if err != nil {
		return CheckResult{Allowed: false, Reason: "risk rule lookup failed"}, err
	}
	if rule == nil {
		return CheckResult{Allowed: true}, nil
	}
	if kycLevel < rule.MinKYCLevel {
		s.record(userID, KindWithdrawLimit, fmt.Sprintf("kyc level %d < required %d", kycLevel, rule.MinKYCLevel))
		return CheckResult{Allowed: false, Reason: "kyc level too low"}, nil
	}
	if amount <= 0 {
		s.record(userID, KindWithdrawLimit, fmt.Sprintf("amount %.8f invalid (must be positive)", amount))
		return CheckResult{Allowed: false, Reason: "amount must be positive"}, nil
	}
	dec := settlement.AssetDecimalsByName(asset)
	// M5：amount 来自请求，NaN/Inf 会静默记 0 绕过提现限额，须拦截。
	amt, err := settlement.AssetAmountFromFloatSafe(amount, dec)
	if err != nil {
		return CheckResult{Allowed: false, Reason: "invalid amount"}, nil
	}
	limit := rule.MaxAmountPerDay.ToDecimals(dec)
	if amt.Cmp(limit) > 0 {
		s.record(userID, KindWithdrawLimit, fmt.Sprintf("amount %s exceeds limit %s", amt.HumanString(), limit.HumanString()))
		return CheckResult{Allowed: false, Reason: "exceeds withdraw limit"}, nil
	}
	return CheckResult{Allowed: true}, nil
}

// CheckOrder 评估下单风控：按 order_limit 规则校验单笔数额与 KYC 等级。
func (s *Service) CheckOrder(userID int64, asset string, qty float64, kycLevel int) (CheckResult, error) {
	uidStr := strconv.FormatInt(userID, 10)
	ok, err := s.store.IsBlacklisted(uidStr)
	if err != nil {
		return CheckResult{Allowed: false, Reason: "blacklist lookup failed"}, err
	}
	if ok {
		s.record(userID, KindOrderLimit, fmt.Sprintf("user %s in blacklist", uidStr))
		return CheckResult{Allowed: false, Reason: "user blacklisted"}, nil
	}
	rule, err := s.matchRule(KindOrderLimit, asset, userID)
	if err != nil {
		return CheckResult{Allowed: false, Reason: "risk rule lookup failed"}, err
	}
	if rule == nil {
		return CheckResult{Allowed: true}, nil
	}
	if kycLevel < rule.MinKYCLevel {
		s.record(userID, KindOrderLimit, fmt.Sprintf("kyc level %d < required %d", kycLevel, rule.MinKYCLevel))
		return CheckResult{Allowed: false, Reason: "kyc level too low"}, nil
	}
	if qty <= 0 {
		s.record(userID, KindOrderLimit, fmt.Sprintf("qty %.8f invalid (must be positive)", qty))
		return CheckResult{Allowed: false, Reason: "qty must be positive"}, nil
	}
	dec := settlement.AssetDecimalsByName(asset)
	// M5：qty 来自请求，NaN/Inf 会静默记 0 绕过下单限额，须拦截。
	amt, err := settlement.AssetAmountFromFloatSafe(qty, dec)
	if err != nil {
		return CheckResult{Allowed: false, Reason: "invalid qty"}, nil
	}
	limit := rule.MaxAmountPerDay.ToDecimals(dec)
	if amt.Cmp(limit) > 0 {
		s.record(userID, KindOrderLimit, fmt.Sprintf("qty %s exceeds limit %s", amt.HumanString(), limit.HumanString()))
		return CheckResult{Allowed: false, Reason: "exceeds order limit"}, nil
	}
	return CheckResult{Allowed: true}, nil
}

// CheckPosition 评估持仓限额风控：positionSize 为用户当前持仓数量（按 asset 计价），
// 超过 position_limit 规则的 MaxAmountPerDay 时拒绝。
func (s *Service) CheckPosition(userID int64, asset string, positionSize float64, kycLevel int) (CheckResult, error) {
	uidStr := strconv.FormatInt(userID, 10)
	ok, err := s.store.IsBlacklisted(uidStr)
	if err != nil {
		return CheckResult{Allowed: false, Reason: "blacklist lookup failed"}, err
	}
	if ok {
		s.record(userID, KindPositionLimit, fmt.Sprintf("user %s in blacklist", uidStr))
		return CheckResult{Allowed: false, Reason: "user blacklisted"}, nil
	}
	rule, err := s.matchRule(KindPositionLimit, asset, userID)
	if err != nil {
		return CheckResult{Allowed: false, Reason: "risk rule lookup failed"}, err
	}
	if rule == nil {
		return CheckResult{Allowed: true}, nil
	}
	if kycLevel < rule.MinKYCLevel {
		s.record(userID, KindPositionLimit, fmt.Sprintf("kyc level %d < required %d", kycLevel, rule.MinKYCLevel))
		return CheckResult{Allowed: false, Reason: "kyc level too low"}, nil
	}
	if positionSize < 0 {
		s.record(userID, KindPositionLimit, fmt.Sprintf("position size %.8f invalid (must be non-negative)", positionSize))
		return CheckResult{Allowed: false, Reason: "position size must be non-negative"}, nil
	}
	dec := settlement.AssetDecimalsByName(asset)
	// M5：positionSize 来自请求/行情，NaN/Inf 会静默记 0 绕过持仓限额，须拦截。
	pos, err := settlement.AssetAmountFromFloatSafe(positionSize, dec)
	if err != nil {
		return CheckResult{Allowed: false, Reason: "invalid position size"}, nil
	}
	limit := rule.MaxAmountPerDay.ToDecimals(dec)
	if pos.Cmp(limit) > 0 {
		s.record(userID, KindPositionLimit, fmt.Sprintf("position %s exceeds limit %s", pos.HumanString(), limit.HumanString()))
		return CheckResult{Allowed: false, Reason: "exceeds position limit"}, nil
	}
	return CheckResult{Allowed: true}, nil
}

// CheckFrequency 评估操作频率风控：按 freq_limit 规则校验单日操作次数。
// 每次调用会原子递增计数器（在 window 窗口内），超过 MaxCountPerDay 时拒绝。
func (s *Service) CheckFrequency(userID int64, action string, window time.Duration) (CheckResult, error) {
	uidStr := strconv.FormatInt(userID, 10)
	ok, err := s.store.IsBlacklisted(uidStr)
	if err != nil {
		return CheckResult{Allowed: false, Reason: "blacklist lookup failed"}, err
	}
	if ok {
		s.record(userID, KindFreqLimit, fmt.Sprintf("user %s in blacklist", uidStr))
		return CheckResult{Allowed: false, Reason: "user blacklisted"}, nil
	}
	rule, err := s.matchRule(KindFreqLimit, "", userID)
	if err != nil {
		return CheckResult{Allowed: false, Reason: "risk rule lookup failed"}, err
	}
	if rule == nil {
		return CheckResult{Allowed: true}, nil
	}
	key := fmt.Sprintf("%d:%s", userID, action)
	count, err := s.store.IncFrequencyCount(key, window)
	if err != nil {
		return CheckResult{Allowed: false, Reason: "frequency counter error"}, err
	}
	if rule.MaxCountPerDay > 0 && count > rule.MaxCountPerDay {
		s.record(userID, KindFreqLimit, fmt.Sprintf("action %s count %d exceeds limit %d", action, count, rule.MaxCountPerDay))
		return CheckResult{Allowed: false, Reason: "exceeds frequency limit"}, nil
	}
	return CheckResult{Allowed: true}, nil
}

// matchRule 匹配作用域与资产都命中的第一条启用规则。
// 列出规则失败时返回 error：静默返回 nil 会被调用方理解为「无匹配规则」从而放行，
// 等于 DB 抖动期间所有限额都被绕过，故须由调用方 fail-closed。
func (s *Service) matchRule(kind, asset string, userID int64) (*RiskRule, error) {
	rules, err := s.store.ListRules(kind)
	if err != nil {
		return nil, err
	}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		if r.Scope == ScopeUser && r.UserID != userID {
			continue
		}
		if r.Asset != "" && r.Asset != asset {
			continue
		}
		return r, nil
	}
	return nil, nil
}

func (s *Service) record(userID int64, kind, detail string) {
	_, _ = s.store.RecordEvent(&RiskEvent{UserID: userID, Kind: kind, Detail: detail})
}

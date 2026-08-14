package risk

import (
	"fmt"
	"strconv"
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
	if ok, _ := s.store.IsBlacklisted(uidStr); ok {
		s.record(userID, KindWithdrawLimit, fmt.Sprintf("user %s in blacklist", uidStr))
		return CheckResult{Allowed: false, Reason: "user blacklisted"}, nil
	}
	if addr != "" {
		if ok, _ := s.store.IsBlacklisted(addr); ok {
			s.record(userID, KindWithdrawLimit, fmt.Sprintf("address %s in blacklist", addr))
			return CheckResult{Allowed: false, Reason: "address blacklisted"}, nil
		}
	}
	rule := s.matchRule(KindWithdrawLimit, asset, userID)
	if rule == nil {
		return CheckResult{Allowed: true}, nil
	}
	if kycLevel < rule.MinKYCLevel {
		s.record(userID, KindWithdrawLimit, fmt.Sprintf("kyc level %d < required %d", kycLevel, rule.MinKYCLevel))
		return CheckResult{Allowed: false, Reason: "kyc level too low"}, nil
	}
	if amount > rule.MaxAmountPerDay {
		s.record(userID, KindWithdrawLimit, fmt.Sprintf("amount %.8f exceeds limit %.8f", amount, rule.MaxAmountPerDay))
		return CheckResult{Allowed: false, Reason: "exceeds withdraw limit"}, nil
	}
	return CheckResult{Allowed: true}, nil
}

// CheckOrder 评估下单风控：按 order_limit 规则校验单笔数额与 KYC 等级。
func (s *Service) CheckOrder(userID int64, asset string, qty float64, kycLevel int) (CheckResult, error) {
	uidStr := strconv.FormatInt(userID, 10)
	if ok, _ := s.store.IsBlacklisted(uidStr); ok {
		s.record(userID, KindOrderLimit, fmt.Sprintf("user %s in blacklist", uidStr))
		return CheckResult{Allowed: false, Reason: "user blacklisted"}, nil
	}
	rule := s.matchRule(KindOrderLimit, asset, userID)
	if rule == nil {
		return CheckResult{Allowed: true}, nil
	}
	if kycLevel < rule.MinKYCLevel {
		s.record(userID, KindOrderLimit, fmt.Sprintf("kyc level %d < required %d", kycLevel, rule.MinKYCLevel))
		return CheckResult{Allowed: false, Reason: "kyc level too low"}, nil
	}
	if qty > rule.MaxAmountPerDay {
		s.record(userID, KindOrderLimit, fmt.Sprintf("qty %.8f exceeds limit %.8f", qty, rule.MaxAmountPerDay))
		return CheckResult{Allowed: false, Reason: "exceeds order limit"}, nil
	}
	return CheckResult{Allowed: true}, nil
}

// matchRule 匹配作用域与资产都命中的第一条启用规则。
func (s *Service) matchRule(kind, asset string, userID int64) *RiskRule {
	rules, err := s.store.ListRules(kind)
	if err != nil {
		return nil
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
		return r
	}
	return nil
}

func (s *Service) record(userID int64, kind, detail string) {
	_, _ = s.store.RecordEvent(&RiskEvent{UserID: userID, Kind: kind, Detail: detail})
}

package risk

import "time"

// Store 是风控持久化抽象。Service 仅依赖此接口。
type Store interface {
	// Rules
	UpsertRule(r *RiskRule) (*RiskRule, error)
	GetRule(id int64) (*RiskRule, error)
	ListRules(kind string) ([]*RiskRule, error)

	// Blacklist
	AddBlacklist(b *BlacklistEntry) (*BlacklistEntry, error)
	RemoveBlacklist(target string) error
	IsBlacklisted(target string) (bool, error)
	ListBlacklist(kind string) ([]*BlacklistEntry, error)

	// Events
	RecordEvent(e *RiskEvent) (*RiskEvent, error)
	ListEvents(userID int64, limit int) ([]*RiskEvent, error)

	// Frequency counting: atomically increment and return the count within window.
	// key is typically "userID:kind". Returns the number of occurrences in the current window.
	IncFrequencyCount(key string, window time.Duration) (int, error)
}

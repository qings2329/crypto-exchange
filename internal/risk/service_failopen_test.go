package risk

import (
	"errors"
	"testing"
	"time"
)

// errStore 让黑名单查询与规则列举全部失败，用于验证风控在存储故障期间是 fail-closed 而非放行。
type errStore struct{ Store }

func (errStore) IsBlacklisted(string) (bool, error) { return false, errors.New("db down") }
func (errStore) ListRules(string) ([]*RiskRule, error) {
	return nil, errors.New("db down")
}

// TestCheckFailsClosedOnStoreError 回归 RISK-F2：存储查询出错时风控必须拒绝而非放行。
// 修复前 `ok, _ := IsBlacklisted(...)` 丢弃错误（DB 抖动 = 黑名单完全失效），
// 且 matchRule 在 ListRules 失败时返回 nil 被当成「无匹配规则」→ 所有限额被绕过。
func TestCheckFailsClosedOnStoreError(t *testing.T) {
	svc := New(errStore{})

	// 提现：命中最关键的资金出口，必须阻断
	res, err := svc.CheckWithdraw(1, "USDT", 100, 2, "addr")
	if err == nil {
		t.Fatal("CheckWithdraw: expected error on store failure")
	}
	if res.Allowed {
		t.Fatal("CheckWithdraw must fail closed, got Allowed=true")
	}

	// 下单
	if res, err := svc.CheckOrder(1, "USDT", 1, 2); err == nil || res.Allowed {
		t.Fatalf("CheckOrder must fail closed, got allowed=%v err=%v", res.Allowed, err)
	}

	// 持仓
	if res, err := svc.CheckPosition(1, "USDT", 1, 2); err == nil || res.Allowed {
		t.Fatalf("CheckPosition must fail closed, got allowed=%v err=%v", res.Allowed, err)
	}

	// 频率
	if res, err := svc.CheckFrequency(1, "withdraw", time.Minute); err == nil || res.Allowed {
		t.Fatalf("CheckFrequency must fail closed, got allowed=%v err=%v", res.Allowed, err)
	}
}

// TestBlacklistLookupErrorFailsClosed 单独覆盖「地址黑名单查询失败」分支：
// 该分支修复前同样丢弃错误，会让被封地址在 DB 抖动期间放行提现。
type addrErrStore struct {
	*memStore
}

func (addrErrStore) IsBlacklisted(target string) (bool, error) {
	if target == "1" { // 用户维度查询正常返回未命中
		return false, nil
	}
	return false, errors.New("db down") // 地址维度查询失败
}

func TestBlacklistLookupErrorFailsClosed(t *testing.T) {
	svc := New(addrErrStore{memStore: NewMemStore().(*memStore)})
	res, err := svc.CheckWithdraw(1, "USDT", 100, 2, "0xsanctioned")
	if err == nil {
		t.Fatal("expected error when address blacklist lookup fails")
	}
	if res.Allowed {
		t.Fatal("address blacklist lookup failure must deny withdrawal")
	}
}

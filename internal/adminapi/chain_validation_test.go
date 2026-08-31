package adminapi

import (
	"errors"
	"testing"
)

func TestIsValidRPCEndpoint(t *testing.T) {
	cases := []struct {
		ep   string
		want bool
	}{
		{"https://node.example.com", true},
		{"http://1.2.3.4:8545", true},
		{"ws://node.example.com", true},
		{"wss://user:pass@node.example.com:443", true},
		{"", false},
		{"not-a-url", false},
		{"ftp://node.example.com", false},
		{"https://", false},           // host 空
		{"//node.example.com", false}, // scheme 空
	}
	for _, c := range cases {
		if got := isValidRPCEndpoint(c.ep); got != c.want {
			t.Errorf("isValidRPCEndpoint(%q)=%v want %v", c.ep, got, c.want)
		}
	}
}

func TestValidateChainCreate(t *testing.T) {
	base := Chain{Name: "Bitcoin", Confirmations: 3, RpcEndpoint: "https://node.example.com"}
	if err := validateChainCreate(base); err != nil {
		t.Fatalf("valid chain should pass: %v", err)
	}
	if err := validateChainCreate(Chain{}); err == nil {
		t.Error("empty chain should fail")
	}
	bad := base
	bad.Confirmations = 0
	if err := validateChainCreate(bad); err == nil {
		t.Error("confirmations<=0 should fail")
	}
	badRPC := base
	badRPC.RpcEndpoint = "javascript:alert(1)"
	if err := validateChainCreate(badRPC); err == nil {
		t.Error("invalid rpc should fail")
	}
	badDep := base
	badDep.RpcEndpoint = ""
	badDep.DepositEnabled = true
	if err := validateChainCreate(badDep); err == nil {
		t.Error("deposit enabled without rpc should fail")
	}
}

func TestValidateChainUpdate(t *testing.T) {
	if err := validateChainUpdate(Chain{Name: "X"}); err != nil {
		t.Errorf("name-only update should pass: %v", err)
	}
	if err := validateChainUpdate(Chain{Confirmations: -1}); err == nil {
		t.Error("negative confirmations should fail")
	}
	if err := validateChainUpdate(Chain{RpcEndpoint: "::::"}); err == nil {
		t.Error("invalid rpc should fail")
	}
}

// TestMemUpdateChainRejectsEnabledWithoutRPC 既验证组合校验拒绝，
// 也验证拒绝时不得部分写入（DepositEnabled 仍应为 false）。
func TestMemUpdateChainRejectsEnabledWithoutRPC(t *testing.T) {
	store := NewMemCatalogStore().(*memCatalogStore)
	ch, err := store.CreateChain(Chain{Name: "Bitcoin", Confirmations: 3, DepositEnabled: false, WithdrawEnabled: false})
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.UpdateChain(ch.ID, Chain{DepositEnabled: true})
	if !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("expected ErrCatalogInvalid, got %v", err)
	}
	// 未 mutate：DepositEnabled 仍应为 false
	chs, _ := store.ListChains()
	for _, c := range chs {
		if c.ID == ch.ID && c.DepositEnabled {
			t.Error("chain DepositEnabled should remain false after rejected update")
		}
	}

	// 合法：提供 rpc 后再启用
	if _, err := store.UpdateChain(ch.ID, Chain{RpcEndpoint: "https://node.example.com", DepositEnabled: true}); err != nil {
		t.Fatalf("valid update should succeed: %v", err)
	}
}

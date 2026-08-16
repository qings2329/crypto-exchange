package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadChainRPCEnvOverride 验证链上 RPC 的真实节点 URL 与热钱包私钥经环境变量注入覆盖
// YAML 默认值（生产部署不把含 API key 的 URL / 私钥写进配置文件）。
func TestLoadChainRPCEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
settlement:
  chain_rpc:
    enabled: false
    endpoints:
      ETH: ""
      BTC: ""
      TRON: ""
    hot_wallet:
      enabled: false
      signer_type: ""
      signer_key: ""
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}

	t.Setenv("CHAIN_RPC_ENABLED", "true")
	t.Setenv("CHAIN_RPC_ENDPOINT_ETH", "https://eth.example/v2/key")
	t.Setenv("CHAIN_RPC_ENDPOINT_BTC", "http://u:p@127.0.0.1:8332")
	t.Setenv("CHAIN_RPC_ENDPOINT_TRON", "https://api.trongrid.io")
	t.Setenv("CHAIN_RPC_REQUIRED_CONFIRMATIONS", "6")
	t.Setenv("HOT_WALLET_ENABLED", "true")
	t.Setenv("HOT_WALLET_SIGNER_TYPE", "hsm")
	t.Setenv("HOT_WALLET_SIGNER_BACKEND", "external")
	t.Setenv("HOT_WALLET_SIGNER_KEY", "deadbeef")
	t.Setenv("HOT_WALLET_ETH_CHAIN_ID", "42161")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rpc := c.Settlement.ChainRPC
	if !rpc.Enabled {
		t.Fatalf("expected CHAIN_RPC_ENABLED=true")
	}
	if rpc.Endpoints["ETH"] != "https://eth.example/v2/key" {
		t.Fatalf("ETH endpoint override failed: %q", rpc.Endpoints["ETH"])
	}
	if rpc.Endpoints["BTC"] != "http://u:p@127.0.0.1:8332" {
		t.Fatalf("BTC endpoint override failed: %q", rpc.Endpoints["BTC"])
	}
	if rpc.Endpoints["TRON"] != "https://api.trongrid.io" {
		t.Fatalf("TRON endpoint override failed: %q", rpc.Endpoints["TRON"])
	}
	if rpc.Required != 6 {
		t.Fatalf("required_confirmations override failed: %d", rpc.Required)
	}
	hw := rpc.HotWallet
	if !hw.Enabled || hw.SignerType != "hsm" || hw.SignerBackend != "external" || hw.SignerKey != "deadbeef" {
		t.Fatalf("hot_wallet override failed: %+v", hw)
	}
	if hw.EthChainID != 42161 {
		t.Fatalf("eth_chain_id override failed: %d", hw.EthChainID)
	}
}

// TestLoadChainRPCNoEnvFallsBackToYAML 验证未设置环境变量时沿用 YAML 默认值（fail-degraded
// 不会因缺环境变量而报错）。
func TestLoadChainRPCNoEnvFallsBackToYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
settlement:
  chain_rpc:
    enabled: true
    endpoints:
      ETH: "http://127.0.0.1:8545"
    hot_wallet:
      enabled: true
      signer_type: "hsm"
      eth_chain_id: 1
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Settlement.ChainRPC.Enabled {
		t.Fatalf("expected enabled=true from yaml")
	}
	if c.Settlement.ChainRPC.Endpoints["ETH"] != "http://127.0.0.1:8545" {
		t.Fatalf("expected yaml endpoint, got %q", c.Settlement.ChainRPC.Endpoints["ETH"])
	}
	if c.Settlement.ChainRPC.HotWallet.EthChainID != 1 {
		t.Fatalf("expected eth_chain_id=1 from yaml")
	}
}

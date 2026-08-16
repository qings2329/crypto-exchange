package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadHSMEnvOverride 验证 HSM_DEPLOYMENT.md §4 的配置驱动：HOT_WALLET_SIGNER_BACKEND 与
// HSM_KIND/HSM_ENDPOINT/HSM_API_KEY/HSM_PUBLIC_KEY 环境变量覆盖 YAML 默认值（生产敏感信息
// 经环境变量注入，不写进 configs/config.yaml）。
func TestLoadHSMEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `settlement:
  chain_rpc:
    enabled: false
    hot_wallet:
      enabled: false
      signer_type: ""
      signer_key: ""
      hsm:
        kind: ""
        endpoint: ""
        api_key: ""
        public_key: ""
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	t.Setenv("HOT_WALLET_SIGNER_BACKEND", "external")
	t.Setenv("HSM_KIND", "remote-http")
	t.Setenv("HSM_ENDPOINT", "http://hsm.internal/sign")
	t.Setenv("HSM_API_KEY", "secret-token")
	t.Setenv("HSM_PUBLIC_KEY", "03abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab")

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	hw := c.Settlement.ChainRPC.HotWallet
	if hw.SignerBackend != "external" {
		t.Fatalf("signer_backend 覆盖失败: %q", hw.SignerBackend)
	}
	if hw.HSM.Kind != "remote-http" {
		t.Fatalf("hsm.kind 覆盖失败: %q", hw.HSM.Kind)
	}
	if hw.HSM.Endpoint != "http://hsm.internal/sign" {
		t.Fatalf("hsm.endpoint 覆盖失败: %q", hw.HSM.Endpoint)
	}
	if hw.HSM.APIKey != "secret-token" {
		t.Fatalf("hsm.api_key 覆盖失败: %q", hw.HSM.APIKey)
	}
	if hw.HSM.PublicKey != "03abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab" {
		t.Fatalf("hsm.public_key 覆盖失败: %q", hw.HSM.PublicKey)
	}
}

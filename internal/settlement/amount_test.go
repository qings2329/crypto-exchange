package settlement

import "testing"

func TestKnownAsset(t *testing.T) {
	known := []string{"BTC", "ETH", "USDT", "USDC", "TRX", "TRON", "TRC20"}
	for _, a := range known {
		if !KnownAsset(a) {
			t.Errorf("KnownAsset(%q) = false, want true", a)
		}
	}
	unknown := []string{"", "xyz", "DOGE", "BNB", "USD", "foo"}
	for _, a := range unknown {
		if KnownAsset(a) {
			t.Errorf("KnownAsset(%q) = true, want false", a)
		}
	}
}

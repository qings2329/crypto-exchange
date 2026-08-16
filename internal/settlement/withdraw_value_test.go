package settlement

import (
	"context"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBroadcastETHValueNoOverflow 验证 ETH 广播金额用 big.Int 计算 wei，避免 int64(amount*1e18)
// 在大额（>~9.22 ETH）时溢出回绕为负（#5）。1e9 ETH × 1e18 = 1e27 wei，节点请求体中的 value
// 应为此正确十六进制而非溢出后的错误值。
func TestBroadcastETHValueNoOverflow(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Write([]byte(`{"result":"0xtxhash"}`))
	}))
	defer srv.Close()

	c := NewJSONRPCClient(map[string]string{"ETH": srv.URL})
	if _, err := c.Broadcast(context.Background(), ChainETH, "0xto", 1e9); err != nil {
		t.Fatalf("broadcast: %v", err)
	}
	want := "0x" + new(big.Int).Exp(big.NewInt(10), big.NewInt(27), nil).Text(16)
	if !strings.Contains(body, want) {
		t.Fatalf("ETH value 缩放/溢出错误: body=%s, want contains %s", body, want)
	}
}

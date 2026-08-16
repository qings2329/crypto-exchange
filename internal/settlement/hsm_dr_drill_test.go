package settlement

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// failoverProxy 是灾备演练用的「主备切换代理」：网关只配置这一个固定 endpoint，演练时把
// 流量在「主用 / 备用 / 全断」之间切换，验证网关无需改配置、无需重启即可完成 DR 切换与恢复
// （真实环境对应 LB VIP / 服务发现背后的 HSM 集群故障转移）。签名服务无状态，每次签名都是
// 一次独立 HTTP 调用，故后端恢复后网关自动回到 HSM 主路径。
type failoverProxy struct {
	t      *testing.T
	mu     sync.Mutex
	active string
	srv    *httptest.Server
}

func newFailoverProxy(t *testing.T, active string) *failoverProxy {
	p := &failoverProxy{t: t, active: active}
	p.srv = httptest.NewServer(http.HandlerFunc(p.handle))
	return p
}

func (p *failoverProxy) handle(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	target := p.active
	p.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	req, err := http.NewRequest(r.Method, target+r.URL.Path, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	copyHeader(r.Header, req.Header)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "backend unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	copyHeader(resp.Header, w.Header())
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (p *failoverProxy) setActive(url string) {
	p.mu.Lock()
	p.active = url
	p.mu.Unlock()
}

func (p *failoverProxy) url() string { return p.srv.URL }
func (p *failoverProxy) close()      { p.srv.Close() }

func copyHeader(src, dst http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

const drTo = "0x3535353535353535353535353535353535353535"

// newDRGateway 构造一个指向 failoverProxy 的提现网关（HSM 签名服务经 proxy 可达），并断言
// external 后端已按 HSMConfig 自动注册。rawSeen/gotNonce/gotGas 由调用方提供，便于读取节点侧
// 实际收到的 raw（离线签名主路径）。
func newDRGateway(t *testing.T, proxyURL, pubHex, keyID string, rawSeen *string, gotNonce, gotGas *bool) WithdrawGateway {
	t.Helper()
	UnregisterExternalSigner(keyID)
	node := ethNodeMock(t, rawSeen, gotNonce, gotGas)
	t.Cleanup(func() {
		node.Close()
		UnregisterExternalSigner(keyID)
	})
	g := NewWithdrawGateway(ChainRPCConfig{
		Enabled:   true,
		Endpoints: map[string]string{"ETH": node.URL},
		HotWallet: HotWalletConfig{
			Enabled:       true,
			SignerType:    "hsm",
			SignerBackend: "external",
			SignerKey:     keyID,
			EthChainID:    1,
			EthGasLimit:   21000,
			HSM:           HSMConfig{Kind: "remote-http", Endpoint: proxyURL + "/sign", PublicKey: pubHex},
		},
	})
	if _, ok := lookupExternalSigner(keyID); !ok {
		t.Fatalf("external 后端未自动注册")
	}
	return g
}

// TestDRHSMFailoverAndRecovery 灾备演练主流程（HSM_DEPLOYMENT.md §7.4/§8/§9）：
// 主用在线 → 主用故障切备用（同密钥，透明故障转移，无需改配置/重启）→ 主备全断（fail-degraded
// 保可用）→ 备用恢复（HSM 签名自动恢复）。全程网关不变、endpoint 不变。
func TestDRHSMFailoverAndRecovery(t *testing.T) {
	// 主用与备用持有同一密钥 K（透明故障转移：地址不变）。
	svc := mustSvc(t, knownVectorPriv)
	pubHex := svc.PublicKeyHex()
	srvPrimary := httptest.NewServer(mustSvc(t, knownVectorPriv).Handler())
	defer srvPrimary.Close()
	srvStandby := httptest.NewServer(mustSvc(t, knownVectorPriv).Handler())
	defer srvStandby.Close()

	proxy := newFailoverProxy(t, srvPrimary.URL)
	defer proxy.close()

	var rawSeen string
	var gotNonce, gotGas bool
	g := newDRGateway(t, proxy.url(), pubHex, "dr-failover", &rawSeen, &gotNonce, &gotGas)

	stage := func(name string) string {
		rawSeen = ""
		ev, err := g.SubmitWithdraw(1, "ETH", ChainETH, 1.0, 0.001, drTo, false)
		if err != nil {
			t.Fatalf("[%s] SubmitWithdraw 不应报错: %v", name, err)
		}
		return ev.TxHash
	}

	// 1) 主用在线：走 HSM 离线签名 → SendRaw，raw 恢复到 HSM 公钥。
	if h := stage("primary"); h != "0xrawsigned" {
		t.Fatalf("主用阶段应走 HSM 签名, got %q", h)
	}
	verifyETHSignature(t, rawSeen, mustSvc(t, knownVectorPriv).Public())

	// 2) 主用故障，切备用（同密钥）：DR 切换，无需改配置/重启网关。
	proxy.setActive(srvStandby.URL)
	if h := stage("failover-standby"); h != "0xrawsigned" {
		t.Fatalf("备用阶段应继续走 HSM 签名, got %q", h)
	}
	verifyETHSignature(t, rawSeen, mustSvc(t, knownVectorPriv).Public())

	// 3) 主备全断：fail-degraded，提现不中断（回退节点侧广播），且不产生 HSM 签名 raw。
	proxy.setActive("http://127.0.0.1:1")
	h := stage("total-outage")
	if h != "0xnodefallback" {
		t.Fatalf("全断应回退节点侧广播, got %q", h)
	}
	if rawSeen != "" {
		t.Fatalf("全断不应走 SendRaw，却收到 raw: %s", rawSeen)
	}

	// 4) 备用恢复：HSM 签名自动恢复，无需重启网关。
	proxy.setActive(srvStandby.URL)
	if h := stage("standby-restored"); h != "0xrawsigned" {
		t.Fatalf("备用恢复应走 HSM 签名, got %q", h)
	}
	verifyETHSignature(t, rawSeen, mustSvc(t, knownVectorPriv).Public())
}

// TestDRPublicKeyMismatchDetected 灾备/配置校验演练（HSM_DEPLOYMENT.md §6）：
// HSM 服务用密钥 K2 签名，但网关配置的 HSM_PUBLIC_KEY 是 K1（错配）。recoverRecID 在校验时
// 发现签名与配置公钥不匹配 → Sign 报错 → 网关 fail-degraded 回退节点侧广播（而非产生归属错误
// 地址的链上交易）。证明错配会被检测并以安全方式降级。
func TestDRPublicKeyMismatchDetected(t *testing.T) {
	// 服务实际用 K2 签名。
	svcK2 := mustSvc(t, tronTestPrivRecipient)
	srvK2 := httptest.NewServer(svcK2.Handler())
	defer srvK2.Close()
	// 网关误配成 K1 的公钥。
	pubK1 := mustSvc(t, knownVectorPriv).PublicKeyHex()

	var rawSeen string
	var gn, gg bool
	node := ethNodeMock(t, &rawSeen, &gn, &gg)
	defer node.Close()

	g := NewWithdrawGateway(ChainRPCConfig{
		Enabled:   true,
		Endpoints: map[string]string{"ETH": node.URL},
		HotWallet: HotWalletConfig{
			Enabled:       true,
			SignerType:    "hsm",
			SignerBackend: "external",
			SignerKey:     "dr-mismatch",
			EthChainID:    1,
			EthGasLimit:   21000,
			HSM:           HSMConfig{Kind: "remote-http", Endpoint: srvK2.URL + "/sign", PublicKey: pubK1},
		},
	})
	ev, err := g.SubmitWithdraw(1, "ETH", ChainETH, 1.0, 0.001, drTo, false)
	if err != nil {
		t.Fatalf("公钥错配应 fail-degraded 不报错: %v", err)
	}
	if ev.TxHash != "0xnodefallback" {
		t.Fatalf("公钥错配应被检测并回退节点侧广播, got %q", ev.TxHash)
	}
	if rawSeen != "" {
		t.Fatalf("公钥错配不应产生 HSM 签名 raw（否则归属错误地址）")
	}
}

// TestDRKeyLossRekey 密钥丢失后的重新密钥（re-key）演练（HSM_DEPLOYMENT.md §7.2）：
// K1 丢失前已签出的未确认交易（链上仍有效，可恢复到 K1 公钥）；丢失后用备用密钥 K2 重新签名，
// 新交易归属 K2 地址。验证轮换窗口内「旧交易有效、新交易用新密钥」的 DR 不变式。
func TestDRKeyLossRekey(t *testing.T) {
	pubK1 := mustSvc(t, knownVectorPriv).PublicKeyHex()
	pubK2 := mustSvc(t, tronTestPrivRecipient).PublicKeyHex()

	// —— K1 在线时签出一笔（模拟丢失前已广播、尚在确认中的旧交易）——
	var rawOld string
	{
		svc := mustSvc(t, knownVectorPriv)
		srv := httptest.NewServer(svc.Handler())
		defer srv.Close()
		var rs string
		var gn, gg bool
		node := ethNodeMock(t, &rs, &gn, &gg)
		defer node.Close()
		g := NewWithdrawGateway(ChainRPCConfig{
			Enabled:   true,
			Endpoints: map[string]string{"ETH": node.URL},
			HotWallet: HotWalletConfig{
				Enabled: true, SignerType: "hsm", SignerBackend: "external", SignerKey: "dr-k1",
				EthChainID: 1, EthGasLimit: 21000,
				HSM: HSMConfig{Kind: "remote-http", Endpoint: srv.URL + "/sign", PublicKey: pubK1},
			},
		})
		if _, err := g.SubmitWithdraw(1, "ETH", ChainETH, 1.0, 0.001, drTo, false); err != nil {
			t.Fatalf("K1 签名: %v", err)
		}
		rawOld = rs
		verifyETHSignature(t, rawOld, svc.Public()) // 旧交易由 K1 签发，有效
	}

	// —— K1 丢失，用备用密钥 K2 重新密钥（re-key）——
	var rawNew string
	{
		svc := mustSvc(t, tronTestPrivRecipient)
		srv := httptest.NewServer(svc.Handler())
		defer srv.Close()
		var rs string
		var gn, gg bool
		node := ethNodeMock(t, &rs, &gn, &gg)
		defer node.Close()
		g := NewWithdrawGateway(ChainRPCConfig{
			Enabled:   true,
			Endpoints: map[string]string{"ETH": node.URL},
			HotWallet: HotWalletConfig{
				Enabled: true, SignerType: "hsm", SignerBackend: "external", SignerKey: "dr-k2",
				EthChainID: 1, EthGasLimit: 21000,
				HSM: HSMConfig{Kind: "remote-http", Endpoint: srv.URL + "/sign", PublicKey: pubK2},
			},
		})
		if _, err := g.SubmitWithdraw(1, "ETH", ChainETH, 1.0, 0.001, drTo, false); err != nil {
			t.Fatalf("K2 签名: %v", err)
		}
		rawNew = rs
		verifyETHSignature(t, rawNew, svc.Public()) // 新交易由 K2 签发，有效
	}

	// 不变式：旧交易仍恢复到 K1（链上有效），新交易恢复到 K2；两者地址不同（re-key 生效）。
	verifyETHSignature(t, rawOld, mustSvc(t, knownVectorPriv).Public())
	verifyETHSignature(t, rawNew, mustSvc(t, tronTestPrivRecipient).Public())
	if deriveETHAddress(mustSvc(t, knownVectorPriv).Public()) == deriveETHAddress(mustSvc(t, tronTestPrivRecipient).Public()) {
		t.Fatalf("re-key 后新旧地址不应相同")
	}
}

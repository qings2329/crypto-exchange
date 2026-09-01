package settlement

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

// mustSvc 是测试辅助：从 32 字节 hex 私钥构造签名服务，失败即终止。
func mustSvc(t *testing.T, hexKey string) *SigningService {
	t.Helper()
	svc, err := NewSigningServiceFromKey(hexKey)
	if err != nil {
		t.Fatalf("NewSigningServiceFromKey: %v", err)
	}
	return svc
}

// ---- HTTP 辅助 ----

func doReq(t *testing.T, method, url string, body interface{}) (int, map[string]string) {
	t.Helper()
	var r *http.Request
	var err error
	if body != nil {
		raw, _ := json.Marshal(body)
		r, err = http.NewRequest(method, url, strings.NewReader(string(raw)))
	} else {
		r, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := map[string]string{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// recoverAndCheck 用 (r,s) 反推公钥，断言等于 want（验证签名由该密钥签出且有效）。
func recoverAndCheck(t *testing.T, digestHex, rHex, sHex string, want *secp256k1.PublicKey) {
	t.Helper()
	db, _ := hex.DecodeString(digestHex)
	rb, _ := hex.DecodeString(rHex)
	sb, _ := hex.DecodeString(sHex)
	for recID := 0; recID <= 1; recID++ {
		compact := make([]byte, 65)
		compact[0] = byte(27 + recID)
		copy(compact[1:33], pad32(rb))
		copy(compact[33:], pad32(sb))
		if p, _, err := ecdsa.RecoverCompact(compact, db); err == nil && p.IsEqual(want) {
			return
		}
	}
	t.Fatalf("签名无法恢复到期望公钥（HSM 密钥不匹配或签名无效）")
}

// TestSigningServiceHTTPContract 验证 HSM_DEPLOYMENT.md §3.1 的签名服务 HTTP 契约：
// POST /sign（rs/der 两种响应，且均能恢复到服务公钥）、GET /pubkey、GET /health、根路径 /
// 也受理签名、非法 digest 返回 400、错误方法返回 405。
func TestSigningServiceHTTPContract(t *testing.T) {
	const digest = "abcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabca" // 32 字节
	svc := mustSvc(t, knownVectorPriv)
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	// GET /pubkey 须等于服务导出公钥。
	_, pub := doReq(t, http.MethodGet, srv.URL+"/pubkey", nil)
	if pub["public_key"] != svc.PublicKeyHex() {
		t.Fatalf("/pubkey 不符：got %s want %s", pub["public_key"], svc.PublicKeyHex())
	}
	// GET /health 就绪探针。
	_, h := doReq(t, http.MethodGet, srv.URL+"/health", nil)
	if h["status"] != "ok" {
		t.Fatalf("/health 应为 ok，got %v", h)
	}

	// POST /sign（rs 模式）→ {r,s}，且能恢复到服务公钥。
	code, rs := doReq(t, http.MethodPost, srv.URL+"/sign", map[string]string{"digest": digest})
	if code != 200 {
		t.Fatalf("/sign rs 期望 200，got %d", code)
	}
	if rs["r"] == "" || rs["s"] == "" {
		t.Fatalf("/sign rs 响应缺 r/s：%v", rs)
	}
	recoverAndCheck(t, digest, rs["r"], rs["s"], svc.Public())

	// 根路径 POST / 也应受理签名（简易部署 / 单测友好）。
	_, rsRoot := doReq(t, http.MethodPost, srv.URL+"/", map[string]string{"digest": digest})
	if rsRoot["r"] == "" || rsRoot["s"] == "" {
		t.Fatalf("根路径 / 应受理 /sign，got %v", rsRoot)
	}

	// der 模式 → {signature: DER}，解析后恢复到服务公钥。
	svcD := mustSvc(t, knownVectorPriv)
	_ = svcD.SetResponseMode("der")
	srvD := httptest.NewServer(svcD.Handler())
	defer srvD.Close()
	_, der := doReq(t, http.MethodPost, srvD.URL+"/sign", map[string]string{"digest": digest})
	if der["signature"] == "" {
		t.Fatalf("/sign der 响应缺 signature：%v", der)
	}
	derBytes, err := hex.DecodeString(der["signature"])
	if err != nil {
		t.Fatalf("DER 解析失败: %v", err)
	}
	dr, ds, err := ParseExternalDERSignature(derBytes)
	if err != nil {
		t.Fatalf("ParseExternalDERSignature: %v", err)
	}
	recoverAndCheck(t, digest, hex.EncodeToString(dr.Bytes()), hex.EncodeToString(ds.Bytes()), svc.Public())

	// 非法 digest → 400。
	badCode, _ := doReq(t, http.MethodPost, srv.URL+"/sign", map[string]string{"digest": "zz"})
	if badCode != 400 {
		t.Fatalf("非法 digest 应返回 400，got %d", badCode)
	}
	// 错误方法 GET /sign → 405。
	methodCode, _ := doReq(t, http.MethodGet, srv.URL+"/sign", nil)
	if methodCode != 405 {
		t.Fatalf("GET /sign 应返回 405，got %d", methodCode)
	}
}

// ethNodeMock 返回一个模拟 ETH 节点的 httptest 服务，记录是否查询了 nonce/gas、是否收到
// 离线签名的 raw，并分别对「节点侧广播」(eth_sendTransaction) 与「离线签名广播」(eth_sendRawTransaction)
// 返回不同哈希，便于区分 fail-degraded 回退与主路径。
func ethNodeMock(t *testing.T, rawSeen *string, gotNonce, gotGas *bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "eth_getTransactionCount":
			*gotNonce = true
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x9"}`))
		case "eth_gasPrice":
			*gotGas = true
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x3b9aca00"}`))
		case "eth_sendRawTransaction":
			if len(req.Params) > 0 {
				*rawSeen = strings.Trim(string(req.Params[0]), `"`)
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xrawsigned"}`))
		case "eth_sendTransaction":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0xnodefallback"}`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`))
		}
	}))
}

// TestDeploymentGatewaySignsViaHSMService 端到端验证「配置驱动部署真实 HSM」主路径
// （HSM_DEPLOYMENT.md §3/§4/§6）：按 external+HSM_* 构造网关，网关自动注册真实后端，
// 提现经「离线签名（HSM 服务对 digest 签名）→ SendRaw 广播」，且链上 raw 可独立恢复到 HSM 公钥。
func TestDeploymentGatewaySignsViaHSMService(t *testing.T) {
	svc := mustSvc(t, knownVectorPriv)
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()
	pubHex := svc.PublicKeyHex()

	var rawSeen string
	var gotNonce, gotGas bool
	node := ethNodeMock(t, &rawSeen, &gotNonce, &gotGas)
	defer node.Close()

	g := NewWithdrawGateway(ChainRPCConfig{
		Enabled:   true,
		Endpoints: map[string]string{"ETH": node.URL},
		HotWallet: HotWalletConfig{
			Enabled:       true,
			SignerType:    "hsm",
			SignerBackend: "external",
			SignerKey:     "hsm-deploy",
			EthChainID:    1,
			EthGasLimit:   21000,
			HSM:           HSMConfig{Kind: "remote-http", Endpoint: srv.URL + "/sign", PublicKey: pubHex},
		},
	})
	if _, ok := lookupExternalSigner("hsm-deploy"); !ok {
		t.Fatalf("external 后端未按 HSMConfig 自动注册")
	}
	if _, ok := g.(*RPCWithdrawGateway); !ok {
		t.Fatalf("enabled 网关应为 *RPCWithdrawGateway，got %T", g)
	}

	to := "0x3535353535353535353535353535353535353535"
	// M4：经工厂装配且配置了离线签名器（enforceAuth=true），须先登记门控授权。
	g.(*RPCWithdrawGateway).AuthorizeWithdraw(1, "ETH", ChainETH, amt(ChainETH, 1.0), amt(ChainETH, 0.001), to, "")
	ev, err := g.SubmitWithdraw(1, "ETH", ChainETH, amt(ChainETH, 1.0), amt(ChainETH, 0.001), to, false)
	if err != nil {
		t.Fatalf("SubmitWithdraw: %v", err)
	}
	if !gotNonce || !gotGas {
		t.Fatalf("期望向节点查 nonce+gas，got nonce=%v gas=%v", gotNonce, gotGas)
	}
	if rawSeen == "" {
		t.Fatalf("eth_sendRawTransaction 未收到离线签名的 raw")
	}
	// 链上 raw 必须能恢复到 HSM 公钥（证明签名由该 HSM 密钥签出、地址归属正确）。
	verifyETHSignature(t, rawSeen, svc.Public())
	if ev.TxHash != "0xrawsigned" {
		t.Fatalf("期望离线签名广播返回的真实哈希，got %q", ev.TxHash)
	}
}

// TestDeploymentGatewayFailDegradedWhenHSMDown 验证 HSM_DEPLOYMENT.md §8/§9：HSM 不可达时
// 网关自动回退节点侧签名广播（fail-degraded），提现不中断；且不会走 SendRaw（因离线签名失败）。
func TestDeploymentGatewayFailDegradedWhenHSMDown(t *testing.T) {
	pubHex := mustSvc(t, knownVectorPriv).PublicKeyHex()
	var rawSeen string
	var gotNonce, gotGas bool
	node := ethNodeMock(t, &rawSeen, &gotNonce, &gotGas)
	defer node.Close()

	g := NewWithdrawGateway(ChainRPCConfig{
		Enabled:   true,
		Endpoints: map[string]string{"ETH": node.URL},
		HotWallet: HotWalletConfig{
			Enabled:       true,
			SignerType:    "hsm",
			SignerBackend: "external",
			SignerKey:     "hsm-down",
			EthChainID:    1,
			EthGasLimit:   21000,
			// 不可达端点（连接被拒），但公钥合法 ⇒ 构造成功、签名时失败回退。
			HSM: HSMConfig{Kind: "remote-http", Endpoint: "http://127.0.0.1:1/sign", PublicKey: pubHex},
		},
	})
	// M4：经工厂装配且配置了离线签名器（enforceAuth=true），须先登记门控授权。
	g.(*RPCWithdrawGateway).AuthorizeWithdraw(1, "ETH", ChainETH, amt(ChainETH, 1.0), amt(ChainETH, 0.001), "0x3535353535353535353535353535353535353535", "")
	ev, err := g.SubmitWithdraw(1, "ETH", ChainETH, amt(ChainETH, 1.0), amt(ChainETH, 0.001), "0x3535353535353535353535353535353535353535", false)
	if err != nil {
		t.Fatalf("fail-degraded 下 SubmitWithdraw 不应报错: %v", err)
	}
	if ev.TxHash != "0xnodefallback" {
		t.Fatalf("HSM 不可达应回退节点侧广播，got %q", ev.TxHash)
	}
	if rawSeen != "" {
		t.Fatalf("离线签名失败不应走 SendRaw，却收到 raw: %s", rawSeen)
	}
}

// TestDeploymentHSMUnreachableSignError 验证 external+HSM 后端在 HSM 不可达时，realSigner.Sign
// 返回错误（供网关 fail-degraded 回退），而非静默成功。
func TestDeploymentHSMUnreachableSignError(t *testing.T) {
	pubHex := mustSvc(t, knownVectorPriv).PublicKeyHex()
	conf := HotWalletConfig{
		SignerKey:     "hsm-unreachable",
		SignerBackend: "external",
		HSM:           HSMConfig{Kind: "remote-http", Endpoint: "http://127.0.0.1:1/sign", PublicKey: pubHex},
	}
	s, err := newRealSignerWithSource(conf, SignerSources{})
	if err != nil {
		t.Fatalf("构造应成功（构造不发起网络调用）: %v", err)
	}
	if _, err := s.Sign(context.Background(), &UnsignedTx{Chain: ChainETH, To: "0x3535353535353535353535353535353535353535", Amount: amt(ChainETH, 1.0), Nonce: 9, GasPriceWei: 1, GasLimit: 21000, ChainID: 1}); err == nil {
		t.Fatalf("HSM 不可达时 Sign 应返回错误")
	}
}

// TestDeploymentKeyRotationChangesAddress 验证 HSM_DEPLOYMENT.md §7.2 的密钥轮换：用不同 HSM
// 公钥构造的签名器，派生地址随之改变，且各自地址与其 HSM 公钥对应（轮换即生效）。
func TestDeploymentKeyRotationChangesAddress(t *testing.T) {
	keyA := "rot-a"
	keyB := "rot-b"
	UnregisterExternalSigner(keyA)
	UnregisterExternalSigner(keyB)
	defer UnregisterExternalSigner(keyA)
	defer UnregisterExternalSigner(keyB)

	pubA := mustSvc(t, knownVectorPriv).PublicKeyHex()
	pubB := mustSvc(t, tronTestPrivRecipient).PublicKeyHex()

	sA, err := newRealSignerWithSource(HotWalletConfig{
		SignerKey: keyA, SignerBackend: "external",
		HSM: HSMConfig{Kind: "remote-http", Endpoint: "http://127.0.0.1:1/sign", PublicKey: pubA},
	}, SignerSources{})
	if err != nil {
		t.Fatalf("构造 A 失败: %v", err)
	}
	sB, err := newRealSignerWithSource(HotWalletConfig{
		SignerKey: keyB, SignerBackend: "external",
		HSM: HSMConfig{Kind: "remote-http", Endpoint: "http://127.0.0.1:1/sign", PublicKey: pubB},
	}, SignerSources{})
	if err != nil {
		t.Fatalf("构造 B 失败: %v", err)
	}
	if sA.address == sB.address {
		t.Fatalf("轮换密钥后地址不应相同")
	}
	if sA.address != deriveETHAddress(mustSvc(t, knownVectorPriv).Public()) {
		t.Fatalf("A 地址应与 A 公钥派生地址一致")
	}
	if sB.address != deriveETHAddress(mustSvc(t, tronTestPrivRecipient).Public()) {
		t.Fatalf("B 地址应与 B 公钥派生地址一致")
	}
}

// doReqAuth 是 doReq 的变体：可携带额外的请求头（用于鉴权令牌）。
func doReqAuth(t *testing.T, method, url string, body interface{}, auth string) (int, map[string]string) {
	t.Helper()
	var r *http.Request
	var err error
	if body != nil {
		raw, _ := json.Marshal(body)
		r, err = http.NewRequest(method, url, strings.NewReader(string(raw)))
	} else {
		r, err = http.NewRequest(method, url, nil)
	}
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	r.Header.Set("Content-Type", "application/json")
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out := map[string]string{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestSigningServiceAuthToken 验证 /sign 的 Bearer 令牌鉴权（item #1）：
//   - 配置令牌后，正确令牌 → 200 并正常签名；
//   - 缺少令牌 / 错误令牌 → 401（fail-closed，拒绝签名）；
//   - 未配置令牌（兼容模式）→ 放行（仅告警）。
func TestSigningServiceAuthToken(t *testing.T) {
	const digest = "abcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabcabca"
	const token = "super-secret-hsm-token"

	// 1) 配置令牌且携带正确令牌 → 200 + 可恢复签名。
	svcTok := mustSvc(t, knownVectorPriv)
	svcTok.SetAuthToken(token)
	srvTok := httptest.NewServer(svcTok.Handler())
	defer srvTok.Close()
	okCode, okSig := doReqAuth(t, http.MethodPost, srvTok.URL+"/sign", map[string]string{"digest": digest}, "Bearer "+token)
	if okCode != 200 {
		t.Fatalf("正确令牌应 200，got %d", okCode)
	}
	if okSig["r"] == "" || okSig["s"] == "" {
		t.Fatalf("正确令牌下应返回签名，got %v", okSig)
	}
	recoverAndCheck(t, digest, okSig["r"], okSig["s"], svcTok.Public())

	// 2) 配置令牌但缺失令牌 → 401（拒绝签名，不得泄露任何 r/s）。
	missCode, missSig := doReqAuth(t, http.MethodPost, srvTok.URL+"/sign", map[string]string{"digest": digest}, "")
	if missCode != 401 {
		t.Fatalf("缺失令牌应 401，got %d", missCode)
	}
	if missSig["r"] != "" || missSig["s"] != "" {
		t.Fatalf("缺失令牌下不得返回签名（资金失窃面），got %v", missSig)
	}

	// 3) 配置令牌但错误令牌 → 401。
	badCode, _ := doReqAuth(t, http.MethodPost, srvTok.URL+"/sign", map[string]string{"digest": digest}, "Bearer wrong")
	if badCode != 401 {
		t.Fatalf("错误令牌应 401，got %d", badCode)
	}

	// 4) 兼容模式（未配置令牌）→ 放行。
	svcOpen := mustSvc(t, knownVectorPriv)
	srvOpen := httptest.NewServer(svcOpen.Handler())
	defer srvOpen.Close()
	openCode, openSig := doReqAuth(t, http.MethodPost, srvOpen.URL+"/sign", map[string]string{"digest": digest}, "")
	if openCode != 200 {
		t.Fatalf("兼容模式应放行 200，got %d", openCode)
	}
	if openSig["r"] == "" || openSig["s"] == "" {
		t.Fatalf("兼容模式应正常签名，got %v", openSig)
	}
}

// TestDeploymentGatewaySignsViaHSMServiceWithAuth 验证端到端「网关与签名服务均启用 Bearer 鉴权」
// 主路径：网关侧 HSMConfig.APIKey 与签名服务 -auth-token 一致时，离线签名成功并经 SendRaw 广播。
func TestDeploymentGatewaySignsViaHSMServiceWithAuth(t *testing.T) {
	const token = "shared-gateway-hsm-token"
	svc := mustSvc(t, knownVectorPriv)
	svc.SetAuthToken(token)
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()
	pubHex := svc.PublicKeyHex()

	var rawSeen string
	var gotNonce, gotGas bool
	node := ethNodeMock(t, &rawSeen, &gotNonce, &gotGas)
	defer node.Close()

	g := NewWithdrawGateway(ChainRPCConfig{
		Enabled:   true,
		Endpoints: map[string]string{"ETH": node.URL},
		HotWallet: HotWalletConfig{
			Enabled:       true,
			SignerType:    "hsm",
			SignerBackend: "external",
			SignerKey:     "hsm-deploy-auth",
			EthChainID:    1,
			EthGasLimit:   21000,
			HSM:           HSMConfig{Kind: "remote-http", Endpoint: srv.URL + "/sign", PublicKey: pubHex, APIKey: token},
		},
	})
	g.(*RPCWithdrawGateway).AuthorizeWithdraw(1, "ETH", ChainETH, amt(ChainETH, 1.0), amt(ChainETH, 0.001), "0x3535353535353535353535353535353535353535", "")
	if _, err := g.SubmitWithdraw(1, "ETH", ChainETH, amt(ChainETH, 1.0), amt(ChainETH, 0.001), "0x3535353535353535353535353535353535353535", false); err != nil {
		t.Fatalf("鉴权主路径 SubmitWithdraw 不应报错: %v", err)
	}
	if rawSeen == "" {
		t.Fatalf("鉴权主路径应经 SendRaw 广播离线签名 raw")
	}
	verifyETHSignature(t, rawSeen, svc.Public())
}

// TestDeploymentGatewaySignsViaHSMServiceAuthMismatch 验证网关与签名服务令牌不一致时，
// 请求被 401 拒绝，网关 fail-degraded 回退节点广播（不得用未授权签名）。
func TestDeploymentGatewaySignsViaHSMServiceAuthMismatch(t *testing.T) {
	svc := mustSvc(t, knownVectorPriv)
	svc.SetAuthToken("server-token")
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()
	pubHex := svc.PublicKeyHex()

	var rawSeen string
	var gotNonce, gotGas bool
	node := ethNodeMock(t, &rawSeen, &gotNonce, &gotGas)
	defer node.Close()

	g := NewWithdrawGateway(ChainRPCConfig{
		Enabled:   true,
		Endpoints: map[string]string{"ETH": node.URL},
		HotWallet: HotWalletConfig{
			Enabled:       true,
			SignerType:    "hsm",
			SignerBackend: "external",
			SignerKey:     "hsm-deploy-mismatch",
			EthChainID:    1,
			EthGasLimit:   21000,
			// 网关携带错误令牌 → 应被签名服务 401 拒绝 → 回退节点广播。
			HSM: HSMConfig{Kind: "remote-http", Endpoint: srv.URL + "/sign", PublicKey: pubHex, APIKey: "wrong-token"},
		},
	})
	g.(*RPCWithdrawGateway).AuthorizeWithdraw(1, "ETH", ChainETH, amt(ChainETH, 1.0), amt(ChainETH, 0.001), "0x3535353535353535353535353535353535353535", "")
	ev, err := g.SubmitWithdraw(1, "ETH", ChainETH, amt(ChainETH, 1.0), amt(ChainETH, 0.001), "0x3535353535353535353535353535353535353535", false)
	if err != nil {
		t.Fatalf("令牌不一致应 fail-degraded 回退而非报错: %v", err)
	}
	if ev.TxHash != "0xnodefallback" {
		t.Fatalf("令牌不一致应回退节点广播，got %q", ev.TxHash)
	}
	if rawSeen != "" {
		t.Fatalf("令牌不一致不得走 SendRaw（拒绝未授权签名），却收到 raw: %s", rawSeen)
	}
}

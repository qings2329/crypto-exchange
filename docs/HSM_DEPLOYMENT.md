# HSM 签名服务部署与运维手册（Runbook）

> 适用范围：链上提现网关的「离线签名边界」生产接入——把热钱包私钥收敛到安全域（内部签名服务
> 前置真实 HSM/KMS），由提现网关经配置驱动接入，私钥永不离开签名服务。
> 架构与代码位置见 [DEVELOPMENT_TASKS.md](DEVELOPMENT_TASKS.md) §27 续九/续十一/续十二。

---

## 1. 架构总览

```
                提现网关 (cmd/settlement / cmd/*)
   UnsignedTx ──► realSigner ──► KeySigner 接口
                                    │
                          external 后端 (SignerBackend="external")
                                    │  POST /sign  {"digest":"<64hex>"}
                                    ▼
                    内部签名服务 (cmd/hsm-signing-service)   ◄── 私钥驻留此处
                                    │         （生产替换为 PKCS#11 / AWS KMS / Vault transit）
                                    ▼
                    返回 {"r","s"} 或 {"signature":"<DER>"}
```

- **私钥位置**：仅存在于「内部签名服务」进程（或真实 HSM/KMS 安全域）。提现网关、settlement
  进程**不持有任何私钥**，只持有**公钥**（`HSM_PUBLIC_KEY`）用于派生地址、推导 recovery id、校验签名。
- **对外契约**：签名服务暴露 `POST /sign`，对传入的 **32 字节 digest** 直接做 secp256k1 ECDSA
  （不再二次哈希，与 HSM/KMS「签名预哈希」语义一致），返回 `{r,s}`(hex) 或 `{signature: DER-hex}`。
- **支持链**：ETH / BTC / TRON 共用 secp256k1 同一曲线，签名服务无需按链区分。
- **降级**：签名服务不可达或配置缺失时，网关**自动回退节点侧签名广播（fail-degraded）**，
  不阻断提现链路（牺牲私钥隔离换可用性，运维须监控此回退）。

---

## 2. 前置条件

- Go 1.26 工具链（构建签名服务二进制）。
- 已配置真实节点 RPC 端点（`CHAIN_RPC_ENABLED=true` + `CHAIN_RPC_ENDPOINT_*`），见
  `configs/config.yaml` 与 `.env.example`。
- 一个可达的「内部签名服务」实例（下文两种部署方式选一）。
- 运维侧准备好**导出公钥**（压缩 33 字节 hex 或 非压缩 65 字节 hex），来自签名服务启动打印
  或真实 HSM 的 `GetPublicKey`。

---

## 3. 部署方式

### 3.1 方式 A：使用内置内部签名服务（推荐起步 / 演示 / 前置真实 HSM 的壳）

仓库自带可部署二进制 `cmd/hsm-signing-service`，持有真实 secp256k1 密钥并对 digest 签名。
生产接入真实安全模块时，只需把 `SigningService.signDigest` 内部调用换成真实 HSM API，对外契约不变。

构建：

```bash
go build -o bin/hsm-signing-service ./cmd/hsm-signing-service
```

启动（生成新密钥）：

```bash
./bin/hsm-signing-service -addr 127.0.0.1:9100 -mode rs
```

启动（复用已知密钥，如从真实 HSM 导出的密钥材料）：

```bash
./bin/hsm-signing-service -addr 127.0.0.1:9100 -mode der -key <32字节hex私钥，0x可选>
```

| flag   | 默认      | 说明 |
|--------|-----------|------|
| `-addr`| `:9100`   | 监听地址（建议仅监听内网/Unix socket 前置反向代理） |
| `-key` | 空（生成）| 可选 32 字节 hex 私钥；留空则每次启动生成新密钥（**务必记下打印的公钥**） |
| `-mode`| `rs`      | `/sign` 响应形态：`rs` → `{"r","s"}`；`der` → `{"signature":"<DER>"}` |

启动即打印接入信息（**复制到网关环境变量**）：

```
  HSM_KIND=remote-http
  HSM_ENDPOINT=http://127.0.0.1:9100/sign
  HSM_PUBLIC_KEY=03a083cdd352c9bea4a1501d50b77f458ef667f95c8047d9d968c1f87ad105a627
```

**端点**：
- `POST /sign` — 请求 `{"digest":"<64 hex>"}`，返回签名（见 §5 形态）。根路径 `/` 也受理 `POST`，便于简易部署与单测。
- `GET  /pubkey` — 返回 `{"public_key":"<压缩hex>"}`，供核对/重新获取。
- `GET  /health` — 返回 `{"status":"ok"}`，就绪探针。
- 优雅退出：收到 `SIGINT`/`SIGTERM` 后停止接收新请求并退出。

### 3.2 方式 B：接入真实 HSM/KMS（AWS KMS / Vault transit / PKCS#11）

两种等价路径，私钥均永不离开安全模块，网关代码不变：

1. **HTTP 适配（推荐，最贴合现有契约）**：在真实 HSM 前包一层内部服务，使其暴露 §3.1 的
   `POST /sign` 契约（对 digest 直接 ECDSA，返回 `{r,s}` 或 DER）。可直接 fork `hsm_signing_service.go`
   把 `signDigest` 改为调用 `kms.Sign(...)` / `C_Sign(...)` / Vault `transit/sign`，其余不动。
2. **代码注入（非常规契约）**：实现 `KeySigner` 接口（见 `hsm_backend.go`），用
   `settlement.RegisterExternalSigner(keyID, backend)` 在部署期注册；此时无需 `HSM_*` 环境变量。

> 公钥必须来自真实设备（`GetPublicKey` / `C_GetAttributeValue CKA_PUBLIC_KEY`）并填入
> `HSM_PUBLIC_KEY`，且**与签名服务的私钥严格对应**——不一致会导致链上签名虽有效但归属于错误
> 地址（见 §6 验证）。

---

## 4. 网关配置（配置驱动，敏感信息走环境变量）

**原则**：私钥、HSM endpoint、公钥一律经环境变量注入，**不写进 `configs/config.yaml`**
（与 `AUTH_SECRET` 同模式，`.env` 已被 `.gitignore` 忽略）。

`configs/config.yaml` 仅保留骨架（已存在）：

```yaml
settlement:
  chain_rpc:
    enabled: true
    hot_wallet:
      enabled: true
      signer_type: "hsm"
      signer_backend: "external"   # 生产接 HSM/KMS
      signer_key: ""               # external 时作为 keyID（可经 HOT_WALLET_SIGNER_KEY 注入）
      eth_chain_id: 1
      eth_gas_price_wei: 0
      eth_gas_limit: 21000
      hsm:
        kind: ""
        endpoint: ""
        api_key: ""
        public_key: ""
```

`.env`（生产实际值）：

```bash
# 链上 RPC（略，见 .env.example）
CHAIN_RPC_ENABLED=true
CHAIN_RPC_ENDPOINT_ETH=https://eth-mainnet.g.alchemy.com/v2/<KEY>
CHAIN_RPC_ENDPOINT_BTC=http://<rpcuser>:<rpcpass>@127.0.0.1:8332
CHAIN_RPC_ENDPOINT_TRON=https://api.trongrid.io

# 离线签名边界
HOT_WALLET_ENABLED=true
HOT_WALLET_SIGNER_TYPE=hsm
HOT_WALLET_SIGNER_BACKEND=external          # 走 HSM 后端
HOT_WALLET_SIGNER_KEY=hsm-prod-1            # keyID（external 注册键）
HOT_WALLET_ETH_CHAIN_ID=1

# 生产 HSM 签名服务连接配置（来自 §3.1 启动打印）
HSM_KIND=remote-http
HSM_ENDPOINT=http://127.0.0.1:9100/sign      # 指向内部签名服务
HSM_API_KEY=                                # 可选：注入 Authorization: Bearer
HSM_PUBLIC_KEY=03a083cdd352c9bea4a1501d50b77f458ef667f95c8047d9d968c1f87ad105a627
```

环境变量覆盖顺序：`configs/config.yaml` → 环境变量覆盖（环境变量优先）。未设置则沿用 YAML 默认值。

---

## 5. 签名请求/响应形态

`POST /sign` 请求体：

```json
{ "digest": "000102...3f" }   // 32 字节 = 64 hex 字符，提现网关计算的待签摘要
```

响应（取决于服务 `-mode`）：

```json
// rs 模式（默认）
{ "r": "28ef61340bd939bc...", "s": "67cbe9d8997f761aecb703304b3800ccf555c9f3dc64214b297fb1966a3b6d83" }

// der 模式（与 AWS KMS Sign / Vault transit 返回一致，经 ParseExternalDERSignature 解出）
{ "signature": "3044022038fbad95be9b56ee...02001e1166ee584568920c818d899dd86ab399ba4670e1ab437ff6ceccb9552816f0" }
```

- 若配置了 `HSM_API_KEY`，网关会在请求头注入 `Authorization: Bearer <HSM_API_KEY>`，签名服务
  可据此做内部鉴权（本仓库内置服务未强制校验，生产反向代理/网关须开启 mTLS 或网络隔离）。
- 错误响应：400（digest 非法）、500（签名失败）、非 200 均触发网关 fail-degraded 回退。

---

## 6. 验证签名（部署后必须做）

目标：**密码学证明提现签名确由该 HSM 密钥签出、且地址归属正确**。两种方法任选。

### 6.1 自动化测试（持续集成）

```bash
go test -run TestDeployRealHMSSignAndVerify ./internal/settlement/
```

该测试启动真实签名服务、按 `external`+`HSMConfig` 自动注册后端，对 ETH/TRON 签名并独立恢复公钥
验证（ETH 重算 EIP-155 摘要后恢复、TRON 用 `txID=SHA256(raw_data)` 恢复），覆盖 `rs` 与 `der`
两种模式。绿色即通过。

### 6.2 手动核对（生产巡检）

1. 启动服务后，确认 `GET /pubkey` 返回的公钥 == 填入网关 `HSM_PUBLIC_KEY` 的值（**必须逐字符一致**）。
2. 用 `HSM_PUBLIC_KEY` 派生期望地址，与链上广播的提现交易 `from` 地址比对：
   - ETH：`keccak256(pubkey[1:])[12:]` → `0x...`；TRON：`0x41 || HASH160(pubkey)` base58check。
   - 仓库内置 `realSigner.address` 即由 `HSM_PUBLIC_KEY` 派生；若二者不符，说明公钥与 HSM 私钥不匹配。
3. 抽取一笔广播交易，独立恢复签名者公钥（任意 ECDSA recover 工具，如 `cast recover` / `web3`），
   断言等于 `HSM_PUBLIC_KEY`。本仓库 e2e 测试即执行此恢复并比对。

> 关键不变量：**`HSM_PUBLIC_KEY` 必须是签名服务实际持有私钥对应的公钥**。错配时交易在链上仍合法，
> 但归属于 HSM 真实地址而非网关预期地址，会造成资金不可控——务必在 §6.1/§6.2 验证通过后再放量。

---

## 7. 运维操作

### 7.1 启动 / 停止
- 启动：见 §3.1。建议用 systemd / k8s Deployment 托管，存活探针 `GET /health`。
- 停止：发 `SIGTERM`（k8s 默认 `terminationGracePeriodSeconds` 给 5–30s），服务优雅关闭。

### 7.2 密钥管理
- **生成**：`-key` 留空即生成新密钥，启动打印公钥。生产请记录并安全保管该密钥材料（或让真实 HSM 托管）。
- **导出公钥**：`GET /pubkey` 随时可取，无需重启。
- **轮换**：
  1. 用新密钥启动一个新签名服务实例（或 `-key <新密钥>` 重启），记下新公钥。
  2. 更新网关 `HSM_PUBLIC_KEY` 为新值（与 `HSM_ENDPOINT` 指向的新实例一致）。
  3. 滚动重启提现网关。网关每次启动按 `HSMConfig` 重新构造并覆盖注册（keyID 不变则覆盖），新密钥即时生效。
  4. 验证（§6）通过后，下线旧实例。
  - 注意：在轮换窗口内，**新交易用新密钥、旧未确认交易仍属旧密钥**——确保旧实例保留至旧交易全部上链确认。

### 7.3 监控与告警
- 探针：`GET /health` 返回 `ok` 即存活。
- 必须监控网关**回退节点侧签名广播**的事件（详见日志 `external 签名后端 ... 未注册` / 签名请求非 200）。
  一旦发生即表示 HSM 链路异常，私钥隔离失效，应触发 P1 告警。
- 对账：定期用 §6.2 核对广播交易的 `from` 地址 == `HSM_PUBLIC_KEY` 派生地址，发现偏差立即排查。

### 7.4 高可用
- 签名服务**无状态**（每次请求只做签名，不缓存），可多副本横向扩展，前面放负载均衡。
- 多副本共用同一密钥（或由真实 HSM 集群统一托管密钥），任一副本宕机不影响签名。
- 网关侧 `HSM_ENDPOINT` 指向 LB VIP / 服务发现名。

### 7.5 网络安全
- 签名服务**仅对内网暴露**，禁止公网直连；建议前置 mTLS 反向代理或放服务网格内。
- `-addr` 绑定内网地址（如 `127.0.0.1:9100` 配合 sidecar，或 `10.x:9100`）。
- 启用 `HSM_API_KEY` 做应用层 Bearer 鉴权（配合网络隔离纵深防御）。
- 传输建议 TLS（反向代理终止或在服务前加 TLS 终结）。

---

## 8. 故障排查

| 现象 | 可能原因 | 处理 |
|------|----------|------|
| 网关日志 `external 签名后端 ... 未注册且 hsm 配置不可用` | `HSM_KIND` 为空 / `HSM_PUBLIC_KEY` 非法 / `HSM_ENDPOINT` 空 | 补全 `HSM_*` 环境变量后重启网关 |
| 提现回退节点侧广播、日志出现 `remote HSM 签名返回 404` | `HSM_ENDPOINT` 路径不对（应为 `.../sign`）或签名服务未启动 | 修正 endpoint 指向 `/sign`，确认服务存活 |
| 提现回退、日志 `remote HSM 签名请求失败` / `返回 5xx` | 签名服务宕机 / 网络不通 / 真实 HSM 报错 | 查签名服务日志与健康，恢复后网关下次请求自动恢复（无状态） |
| 链上交易 `from` 地址 ≠ 预期热钱包地址 | `HSM_PUBLIC_KEY` 与签名服务私钥不匹配 | 重新核对 `GET /pubkey` 与 `HSM_PUBLIC_KEY`，按 §6 验证通过后再放量 |
| `HSM_PUBLIC_KEY` 解析失败（启动报错） | 公钥 hex 非 33/65 字节、含非法字符 | 用 `GET /pubkey` 的真实输出填入 |
| 签名服务启动无 `HSM_PUBLIC_KEY` 打印 | 日志级别/输出被吞 | 直接 `curl GET /pubkey` 获取 |

---

## 9. 回滚与降级

- **配置驱动降级**：将 `HOT_WALLET_SIGNER_BACKEND` 改回 `software`（或 `HOT_WALLET_ENABLED=false`）
  即回退到进程内软件私钥 / 节点侧签名广播；无需改代码、重启网关生效。
- **fail-degraded 自动降级**：HSM 链路异常时网关自动回退节点侧签名广播，**保证提现不中断**，
  但牺牲私钥隔离——这是「可用性优先」的兜底，必须配合 §7.3 告警，异常期间人工介入。
- **回滚步骤**：改 `.env` → 滚动重启提现网关 → 观察日志确认走预期路径（无 `external 签名后端`
  报错、无 `remote HSM 签名` 失败）→ 用 §6 验证。

---

## 10. 安全要点（checklist）

- [ ] 私钥仅存在于签名服务 / 真实 HSM，网关与 settlement 进程无私钥。
- [ ] `HSM_ENDPOINT` / `HSM_API_KEY` / `HSM_PUBLIC_KEY` 仅经环境变量注入，未进 `configs/config.yaml` 与版本库。
- [ ] 签名服务仅内网可达，启用 mTLS / 网络隔离 + `HSM_API_KEY` 鉴权。
- [ ] `HSM_PUBLIC_KEY` 经 §6 验证与签名服务实际密钥对应，且派生地址 == 链上 `from`。
- [ ] 监控覆盖「回退节点侧签名广播」告警；定期地址对账。
- [ ] 密钥轮换流程（§7.2）演练过，旧实例保留至旧交易全部确认。
- [ ] 多副本 + LB 高可用已部署（如需要）。

## 11. 配套自动化测试

以下测试使本文档的部署/运维声明可被持续验证（改代码或配置不致文档漂移）：

- `internal/settlement/hsm_deployment_test.go`
  - `TestSigningServiceHTTPContract` — §3.1 签名服务 HTTP 契约（`/sign` 的 rs/der 响应且均能恢复到服务公钥、`/pubkey`、`/health`、根路径 `/` 受理、非法 digest→400、错误方法→405）。
  - `TestDeploymentGatewaySignsViaHSMService` — §3/§4/§6 主路径：按 `external`+`HSM_*` 构造网关自动注册后端，提现经「离线签名→SendRaw」，链上 raw 独立恢复到 HSM 公钥。
  - `TestDeploymentGatewayFailDegradedWhenHSMDown` — §8/§9：HSM 不可达自动回退节点侧广播，提现不中断且不走 SendRaw。
  - `TestDeploymentHSMUnreachableSignError` — HSM 不可达时 `Sign` 返回错误（驱动 fail-degraded）。
  - `TestDeploymentKeyRotationChangesAddress` — §7.2：轮换公钥使派生地址改变且各自与公钥对应。
- `internal/pkg/config/hsm_env_test.go`
  - `TestLoadHSMEnvOverride` — §4：HSM 相关环境变量覆盖 YAML 默认值。

运行：

```bash
go test -run 'TestSigningServiceHTTPContract|TestDeployment' ./internal/settlement/
go test -run TestLoadHSMEnvOverride ./internal/pkg/config/
```

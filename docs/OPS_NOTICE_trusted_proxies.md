# 运维变更通知：配置受信任代理（trusted_proxies）

> 适用范围：所有生产/预发环境的后端服务（user / spot / market / futures / otc / options / margin / wealth / risk / settlement / notification / matching / gateway / admin）。
> 状态：**上线前必做**。代码已合入（见底部提交），但**配置需运维按部署拓扑手动填写**，不填则仍按旧行为（IP 归因失真）。

## 为什么要做

生产拓扑为 `客户端 → 网关/LB/CDN → 各业务服务`。服务解析客户端真实 IP 依赖 gin 的 `c.ClientIP()`，该值只在「受信任代理」名单配置后才会从 `X-Forwarded-For` 取真实来源；否则一律使用直连对端 IP（即网关 IP）。

未配置会导致：
- **全局限流误伤**：`middleware.Common` 的每 IP 限流会把所有请求当成来自同一个网关 IP，可能整体被限流、甚至误伤全体用户。
- **审计/安全日志失真**：日志里的客户端 IP 全部是网关 IP，事后无法溯源。
- **admin 登录 IP 限流失效**：限的是网关 IP 而非真实来源。

## 上线前必做（按部署拓扑）

1. 编辑对应环境的配置文件（通常 `configs/config.yaml`）的 `server` 段。
2. 把 **网关 / LB / CDN 的 IP 或 CIDR** 填入 `trusted_proxies`：
   ```yaml
   server:
     # 直连公网、前面无反向代理：保持留空（默认，无需改动）
     trusted_proxies: []
     # 位于网关/LB/CDN 之后：填入其地址，例如
     # trusted_proxies: ["10.0.0.0/8", "192.168.1.10"]
   ```
3. 若服务**直连公网**、前面没有反向代理：保持 `trusted_proxies: []`（默认即可，无需改动）。
4. 若**网关（cmd/gateway）自身也位于 CDN/LB 之后**：网关配置同样需填入其上游地址。

## 如何确认「配置完成」（三级验证）

**第 1 级 · 启动日志自证（最强，部署后必查）**
每个服务启动时会打印受信任代理生效状态，运维从日志即可确认配置是否已加载，无需读代码：
- 已正确配置：`server.trusted_proxies configured; c.ClientIP() reads real client IP from X-Forwarded-For` + 具体地址列表。
- 仍为空（需关注）：`server.trusted_proxies is EMPTY: not trusting any proxy; c.ClientIP() uses direct RemoteAddr ...`（仅当该服务确实直连公网时才可接受）。
- 检索示例：`kubectl logs <pod> | grep -i trusted_proxies`（或对应环境的日志查询）。

**第 2 级 · 配置静态校验（上线前/CI）**
仓库内置校验工具，直接检查部署配置文件、无需启动服务：
```bash
go build -o /tmp/checkconfig ./hack/checkconfig
/tmp/checkconfig <path-to-config.yaml>
# 退出码：0=OK（已配置且均为合法 IP/CIDR）；3=WARN（为空，直连公网可接受）；4=FAIL（含非法 IP/CIDR）
# 注意：用 go run 时退出码会被包装成 1，建议先 build 再运行以拿到真实退出码。
```

**第 3 级 · 运行时黑盒测试（可选， strongest 端到端）**
1. 经网关向某后端发起一次请求，请求头带一个可辨识的 `X-Forwarded-For: 203.0.113.9`（仅测试用，确认后端信任网关）。
2. 查看该后端审计/安全日志中的客户端 IP：
   - 若为 `203.0.113.9` → 归因正确，配置生效。
   - 若为网关 IP → 配置未生效，需回查第 1/2 级。
3. admin 服务可发起一次错误登录，确认 `[admin] SECURITY: failed login ...` 记录的 IP 是真实客户端而非网关。
- 建议先灰度一台验证通过，再全量推平。

## 回滚/风险

- 配置错误（如填了错误地址）**不会中断服务**，但会使 IP 归因不符合预期，需重新核对地址后重启。
- 所有服务读取各自进程的配置文件，修改后**需重启对应服务**生效。

## 确认回执模板（请运维逐项回复）

> 请运维同学完成配置后，按以下模板回复本通知，作为「已读 + 已配置 + 已验证」的闭环记录。

```
主题：[已办] trusted_proxies 配置 —— <环境：生产/预发> <日期>

1. 配置范围：已核对全部后端服务（user/spot/market/futures/otc/options/margin/wealth/
   risk/settlement/notification/matching/gateway/admin）的配置文件。
2. trusted_proxies 取值：<填写实际配置，如 10.0.0.0/8；直连公网服务填「留空」并注明>
3. 启动日志：已重启服务，日志中可见 "server.trusted_proxies configured" / 或 "is EMPTY"（仅直连公网）
   <附 1 条样例日志>
4. 静态校验：/tmp/checkconfig <config> 退出码 <0/3>，输出 <粘贴>
5. 运行时验证（可选）：经网关请求后后端日志客户端 IP = <真实 IP / 网关 IP>
6. 灰度：<已灰度 1 台验证通过 / 未灰度直接全量>
7. 遗留/风险：<无 / 说明>
```

## 背景与详情

- 部署文档与完整说明：见 `README.md` 的「部署与配置」小节、`configs/config.yaml` 的 `server.trusted_proxies` 注释。
- 对应代码提交：
  - `f85e710` —— 全部 14 个 cmd 调用 `r.SetTrustedProxies(cfg.Server.TrustedProxies)`。
  - `32a1cee` —— admin 先行配置。
  - `c53d5ee` —— 部署文档（README）补充与配置文件注释。
  - `0a6bb08` —— 本运维变更通知文档。
  - 最新提交 —— 抽取 `middleware.ConfigureTrustedProxies` 辅助函数（设置 + 启动日志），并新增 `hack/checkconfig` 静态校验工具与三级验证/回执模板。

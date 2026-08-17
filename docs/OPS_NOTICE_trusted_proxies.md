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

## 验证

- 发起请求后，查看后端审计/安全日志中的客户端 IP，应为**真实客户端 IP**，而非网关 IP。
- 可经网关发起一次 admin 登录失败，确认日志 `[admin] SECURITY: failed login ...` 记录的 IP 是真实客户端而非网关。
- 建议先灰度一台验证 IP 归因正确，再全量推平。

## 回滚/风险

- 配置错误（如填了错误地址）**不会中断服务**，但会使 IP 归因不符合预期，需重新核对地址后重启。
- 所有服务读取各自进程的配置文件，修改后**需重启对应服务**生效。

## 背景与详情

- 部署文档与完整说明：见 `README.md` 的「部署与配置」小节、`configs/config.yaml` 的 `server.trusted_proxies` 注释。
- 对应代码提交：
  - `f85e710` —— 全部 14 个 cmd 在 `r := gin.New()` 后调用 `r.SetTrustedProxies(cfg.Server.TrustedProxies)`。
  - `32a1cee` —— admin 先行配置。
  - `c53d5ee` —— 部署文档（README）补充与配置文件注释。

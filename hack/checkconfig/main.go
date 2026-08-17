// Command checkconfig 校验部署配置中 server.trusted_proxies 的设置，
// 用于「配置完成」的静态自检（无需启动服务）。
//
// 用法：
//
//	go run ./hack/checkconfig <path-to-config.yaml>
//
// 退出码：
//
//	0  OK：已配置且均为合法 IP/CIDR
//	2  用法错误（缺参数）
//	3  WARN：trusted_proxies 为空（直连公网可接受；位于代理后则有风险）
//	4  FAIL：存在非法 IP/CIDR 条目
//	1  配置文件加载失败
package main

import (
	"fmt"
	"net"
	"os"

	"github.com/coldlar/crypto-exchange/internal/pkg/config"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: checkconfig <path-to-config.yaml>")
		os.Exit(2)
	}
	cfg, err := config.Load(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(1)
	}

	tp := cfg.Server.TrustedProxies
	if len(tp) == 0 {
		fmt.Println("WARN: server.trusted_proxies is EMPTY")
		fmt.Println("  -> not trusting any proxy; c.ClientIP() uses direct RemoteAddr.")
		fmt.Println("  -> 若本服务位于网关/LB/CDN 之后，必须设置 server.trusted_proxies 为上游 IP/CIDR，")
		fmt.Println("     否则所有客户端 IP 都会退化成网关 IP（全局限流可能误伤全体用户、审计/admin IP 限流失效）。")
		os.Exit(3)
	}

	bad := 0
	for _, e := range tp {
		if _, _, perr := net.ParseCIDR(e); perr != nil {
			// 允许裸 IP（ParseCIDR 会失败），再用 ParseIP 校验。
			if net.ParseIP(e) == nil {
				fmt.Printf("  ERROR: %q is not a valid IP or CIDR\n", e)
				bad++
			}
		}
	}
	if bad > 0 {
		fmt.Printf("FAIL: %d invalid trusted_proxies entry/entries\n", bad)
		os.Exit(4)
	}

	fmt.Printf("OK: server.trusted_proxies = %v\n", tp)
	fmt.Println("  -> c.ClientIP() 将从 X-Forwarded-For 读取真实客户端 IP。")
}

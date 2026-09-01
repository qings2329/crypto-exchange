// 内部 HSM 签名服务（可部署二进制）。
//
// 生产里交易所把真实 HSM/KMS 前置在一个「内部签名服务」后——本进程持有 secp256k1 私钥，对
// 外部仅暴露「对 32 字节摘要做 ECDSA 签名」的 HTTPS 端点；网关侧 remoteHSMKeySigner 把离线
// 签名请求转成对 /sign 的一次调用，私钥永不离开本服务。签名为真实 secp256k1 ECDSA（确定性
// nonce），可被任意节点/钱包独立验证。
//
// 真实部署要点：
//   - 把本服务的私钥替换为真实安全模块托管（PKCS#11 / AWS KMS / Vault transit）：仅改
//     SigningService.signDigest 内部调用，对外契约（请求/响应）不变。
//   - 启动后打印 HSM_PUBLIC_KEY（压缩 hex）与端点，运营填入网关的环境变量（HSM_KIND/
//     HSM_ENDPOINT/HSM_PUBLIC_KEY），无需改动 settlement 代码。
//
// 用法：
//
//	./hsm-signing-service -addr :9100 -mode rs
//	./hsm-signing-service -addr :9100 -key <32字节hex私钥> -mode der   # 复用已知密钥
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

func main() {
	addr := flag.String("addr", ":9100", "监听地址，如 :9100")
	key := flag.String("key", "", "可选：32 字节 hex 私钥（0x 可选）；留空则生成新密钥并打印公钥")
	mode := flag.String("mode", "rs", "响应形态：rs -> {r,s}(hex)；der -> {signature: DER-hex}")
	authToken := flag.String("auth-token", os.Getenv("HSM_AUTH_TOKEN"), "必需（生产）：/sign 端点的静态 Bearer 令牌；须在网关侧 HSMConfig.APIKey 配置相同值。留空则开放签名端点（仅告警）")
	flag.Parse()

	log, err := logger.New("release")
	if err != nil {
		panic(err)
	}
	defer func() { _ = log.Sync() }()

	var svc *settlement.SigningService
	if *key != "" {
		svc, err = settlement.NewSigningServiceFromKey(*key)
		if err != nil {
			log.Fatal("加载私钥失败", zap.Error(err))
		}
		log.Info("签名服务使用注入密钥")
	} else {
		svc = settlement.NewSigningService()
		log.Info("签名服务生成新密钥")
	}
	if err := svc.SetResponseMode(*mode); err != nil {
		log.Fatal("响应模式非法", zap.Error(err))
	}
	svc.SetAuthToken(*authToken)
	if *authToken != "" {
		log.Info("签名服务已启用 Bearer 鉴权（/sign 需携带正确令牌）")
	} else {
		log.Warn("签名服务未配置 -auth-token：/sign 端点对任何人开放，生产环境禁止此配置")
	}

	// 打印供运营注入网关的环境变量（与 remoteHSMKeySigner 契约对齐）。
	endpoint := fmt.Sprintf("http://%s/sign", *addr)
	fmt.Println("=========================================================")
	fmt.Println("  HSM 接入信息（填入网关环境变量）：")
	fmt.Printf("  HSM_KIND=%s\n", "remote-http")
	fmt.Printf("  HSM_ENDPOINT=%s\n", endpoint)
	fmt.Printf("  HSM_PUBLIC_KEY=%s\n", svc.PublicKeyHex())
	fmt.Println("=========================================================")

	srv := &http.Server{
		Addr:    *addr,
		Handler: svc.Handler(),
	}
	go func() {
		log.Info("HSM 签名服务监听", zap.String("addr", *addr), zap.String("mode", *mode))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal("签名服务异常退出", zap.Error(err))
		}
	}()

	// 优雅退出：收到 SIGINT/SIGTERM 后停止接收新请求并结束。
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Info("收到退出信号，关闭 HSM 签名服务")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Warn("优雅关闭超时", zap.Error(err))
	}
}

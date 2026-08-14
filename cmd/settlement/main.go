// 独立清算/手续费服务：把 internal/settlement 的手续费模型与链上网关封装为独立 HTTP 微服务。
// 生产环境由网关路由到本服务；此处暴露手续费估算与链上充值/提现网关的健康查询。
package main

import (
	"flag"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}
	log, _ := logger.New(cfg.Server.Mode)
	defer func() { _ = log.Sync() }()

	// 手续费模型：按 (链, 资产) 维度登记费率。生产应从配置/远程配置中心加载。
	feeModel := settlement.NewFeeModel()
	feeModel.Register(settlement.ChainETH, "USDT", 0.1, 0)
	feeModel.Register(settlement.ChainETH, "ETH", 0.001, 0)
	feeModel.Register(settlement.ChainBTC, "BTC", 0.0005, 0)
	feeModel.Register(settlement.ChainTRON, "USDT", 1, 0)

	r := gin.New()
	r.Use(middleware.Common(log, cfg)...)

	// 手续费估算：GET /api/v1/settlement/fee?chain=eth&asset=USDT&amount=1000
	r.GET("/api/v1/settlement/fee", func(c *gin.Context) {
		chain := settlement.Chain(strings.ToUpper(c.Query("chain")))
		asset := c.Query("asset")
		var amount float64
		if err := parseFloat(c.Query("amount"), &amount); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
			return
		}
		fee := feeModel.Estimate(chain, asset, amount)
		c.JSON(http.StatusOK, gin.H{
			"chain": chain,
			"asset": asset,
			"amount": amount,
			"fee":   fee,
		})
	})

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	addr := ":8086"
	log.Info("settlement service starting", zap.String("addr", addr))
	if err := cfg.Listen(r, addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}

func parseFloat(s string, out *float64) error {
	if s == "" {
		*out = 0
		return nil
	}
	var f float64
	if _, err := fmt.Sscanf(s, "%f", &f); err != nil {
		return err
	}
	*out = f
	return nil
}

// 独立清算/手续费服务：把 internal/settlement 的手续费模型与链上清结算网关封装为独立 HTTP 微服务。
// 生产环境由网关路由到本服务；此处暴露手续费估算、链上充值/提现网关健康查询，以及
// 实时交易清算（消费撮合引擎发布的 exchange.trades，落账到交易所手续费账户）。
//
// 交易清算：以 -tags kafka 构建且配置了 kafka.brokers 时，经消费组订阅 exchange.trades，
// 把每笔成交的手续费入账（ce_settlement_trades，幂等去重）；无 Kafka 时退回内存订阅器
// （Subscribe 为 no-op，清算消费不可用，仅 HTTP 费用估算/健康端点照常工作）。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/mq"
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
	feeModel.Register(settlement.ChainSOL, "SOL", 0.001, 0)  // 原生 SOL（9 位小数）
	feeModel.Register(settlement.ChainSOL, "USDC", 0.1, 0)   // SPL USDC（6 位小数）

	// 交易清算：持久化存储（有 DSN 走 MySQL ce_settlement_trades，否则内存）+
	// 清算处理器（消费 Kafka 成交流入账）。
	clearStore, isMem, cerr := settlement.NewClearingStore(cfg.MySQL.DSN)
	if cerr != nil {
		log.Warn("clearing store fallback to in-memory", zap.Error(cerr))
	}
	clearer := settlement.NewClearer(clearStore, cfg.Settlement.TradeFeeRate)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 交易清算消费端：订阅 exchange.trades，逐条解包为 TradeEvent 并清算入账。
	// handler 在 Kafka 消费组（或测试用 InMemSubscriber）上下文调用，需幂等——Clearer 已保证。
	tradeTopic := cfg.Kafka.TradeTopic
	if tradeTopic == "" {
		tradeTopic = "exchange.trades"
	}
	subHandler := func(c context.Context, topic string, data []byte) error {
		if topic != tradeTopic {
			return nil
		}
		var ev mq.TradeEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			log.Warn("clearing: bad trade payload", zap.Error(err))
			return nil // 坏消息仍提交位移，避免毒消息阻塞消费组
		}
		if err := clearer.Clear(ev); err != nil {
			log.Warn("clearing: record failed", zap.Error(err))
		}
		return nil
	}
	subscriber := mq.NewSubscriber(cfg.Kafka.Brokers, "settlement-clearer", subHandler)
	go func() {
		// Subscribe 阻塞直到 ctx 取消（Kafka）或 no-op（内存订阅器）。
		if err := subscriber.Subscribe(ctx, []string{tradeTopic}, subHandler); err != nil && ctx.Err() == nil {
			log.Warn("clearing subscriber stopped", zap.Error(err))
		}
	}()
	if isMem {
		log.Info("clearing: in-memory store (no MySQL), records volatile")
	} else {
		log.Info("clearing: mysql store ready", zap.String("topic", tradeTopic))
	}

	r := gin.New()
	// 配置受信任代理后，c.ClientIP() 从 X-Forwarded-For 取真实客户端 IP；
	// 留空则不信任任何代理，使用直连对端 IP（RemoteAddr）。这让审计 IP、全局限流
	// 都能正确归因到真实来源（避免经网关/LB 转发时所有请求被误判为同一上游 IP）。
	middleware.ConfigureTrustedProxies(r, cfg, log)
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
		fee := feeModel.Estimate(chain, asset, settlement.AssetAmountFromFloat(amount, settlement.AssetDecimals(chain, asset)))
		c.JSON(http.StatusOK, gin.H{
			"chain": chain,
			"asset": asset,
			"amount": amount,
			"fee":   fee,
		})
	})

	// 最近清算成交：GET /api/v1/settlement/cleared?limit=100
	r.GET("/api/v1/settlement/cleared", func(c *gin.Context) {
		var limit int
		_ = parseInt(c.Query("limit"), &limit)
		recs, err := clearer.Recent(limit)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"trades": recs})
	})

	// 清算聚合统计：GET /api/v1/settlement/stats
	r.GET("/api/v1/settlement/stats", func(c *gin.Context) {
		c.JSON(http.StatusOK, clearer.Stats())
	})

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	addr := ":8086"
	log.Info("settlement service starting", zap.String("addr", addr))
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		cancel()
		_ = subscriber.Close()
		_ = clearStore.Close()
		_ = log.Sync()
		os.Exit(0)
	}()
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

func parseInt(s string, out *int) error {
	if s == "" {
		*out = 100
		return nil
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		*out = 100
		return err
	}
	*out = n
	return nil
}

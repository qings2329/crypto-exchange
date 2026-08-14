package main

import (
	"flag"
	"log"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/adminapi"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
)

func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if cfg.Server.Mode != "" {
		gin.SetMode(cfg.Server.Mode)
	}
	// 把管理后台前端来源并入 CORS 白名单，避免跨域被拒。
	for _, o := range cfg.Admin.AllowedOrigins {
		if !contains(cfg.Server.AllowedOrigins, o) {
			cfg.Server.AllowedOrigins = append(cfg.Server.AllowedOrigins, o)
		}
	}

	logger, err := zap.NewProduction()
	if err != nil {
		log.Fatalf("init logger: %v", err)
	}

	r := gin.New()
	r.Use(middleware.Common(logger, cfg)...)

	srv := adminapi.NewServer(cfg)
	srv.RegisterRoutes(r)

	addr := cfg.Admin.Addr
	if addr == "" {
		addr = ":8090"
	}
	logger.Info("admin service starting", zap.String("addr", addr))
	if err := cfg.Listen(r, addr); err != nil {
		log.Fatalf("listen: %v", err)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

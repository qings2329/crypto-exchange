package main

import (
	"database/sql"
	"flag"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/announcement"
	"github.com/coldlar/crypto-exchange/internal/notification"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/logger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/services/user"
)

func main() {
	cfgPath := flag.String("config", "configs/config.yaml", "path to config file")
	addr := flag.String("addr", ":8081", "listen address")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		panic(err)
	}
	log, _ := logger.New(cfg.Server.Mode)
	defer func() { _ = log.Sync() }()

	// 存储：优先 MySQL（建 ce_ 表并迁移）；失败则降级为内存（重启即丢，仅开发用）。
	var store user.Store
	if cfg.MySQL.DSN != "" {
		store, err = user.NewMySQLStore(cfg.MySQL.DSN)
		if err != nil {
			log.Warn("user: mysql unavailable, fallback to in-memory store", zap.Error(err))
		}
	}
	if store == nil {
		store = user.NewMemStore()
	}

	verifier := middleware.NewTokenVerifier(cfg.Auth.Secret)
	notifSvc := newUserNotifSvc(cfg.MySQL.DSN, log)
	svc := user.NewService(store, verifier, user.NewLogNotifier(), notifSvc, user.Config{})
	h := user.NewHandler(svc, verifier)

	// 演示用户种子：无论内存还是连库模式都幂等注入若干测试账号，供管理后台「用户管理」列表/详情展示。
	// AdminCreate 对已存在的账号返回 ErrUserExists 而被跳过（不重复插入），故连库时数据可持久化、重启不丢；
	// Status/KYC 仅在新创建后回填，既有账号保持原状态。测试邮箱固定，不会重复产生数据。
	{
		demoUsers := []struct {
			email  string
			status user.Status
			kyc    user.KYCLevel
		}{
			{"alice@test.com", user.StatusNormal, user.KYCVerified},
			{"bob@test.com", user.StatusNormal, user.KYCPending},
			{"u123@test.com", user.StatusNormal, user.KYCNone},
			{"carol@test.com", user.StatusFrozen, user.KYCRejected},
			{"dave@test.com", user.StatusNormal, user.KYCVerified},
		}
		for _, du := range demoUsers {
			id, err := svc.AdminCreate(du.email, "test#Pwd123")
			if err != nil {
				if err != user.ErrUserExists {
					log.Warn("seed demo user failed", zap.Error(err))
				}
				continue
			}
			st, kyc := du.status, du.kyc
			if uerr := svc.AdminUpdate(id, user.AdminUpdateInput{Status: &st, KYCLevel: &kyc}); uerr != nil {
				log.Warn("seed demo user update failed", zap.Int64("id", id), zap.Error(uerr))
			}
		}

		// 额外演示用户走真实 KYC 提交路径（SubmitKYC 会写入 pending 材料），
		// 使管理后台「KYC 审核」页有待审核记录可展示/通过/驳回。
		kycApplicants := []struct {
			email, realName, idType, idNumber string
		}{
			{"eve@test.com", "Eve Lin", "id_card", "ID1234567"},
			{"frank@test.com", "Frank Wu", "passport", "P23456789"},
			{"grace@test.com", "Grace Zhang", "driver_license", "D34567890"},
		}
		for _, k := range kycApplicants {
			id, err := svc.AdminCreate(k.email, "test#Pwd123")
			if err != nil {
				if err != user.ErrUserExists {
					log.Warn("seed kyc applicant failed", zap.Error(err))
				}
				continue
			}
			if serr := svc.SubmitKYC(id, user.KYCRequest{
				RealName: k.realName, IDType: k.idType, IDNumber: k.idNumber,
				DocFront: "https://demo.example.com/doc/front.png", DocBack: "https://demo.example.com/doc/back.png",
			}); serr != nil {
				log.Warn("seed kyc submit failed", zap.Int64("id", id), zap.Error(serr))
			}
		}
		log.Info("seeded demo users (idempotent: existing skipped)")
	}

	// 公告模块：与用户模块共用同一数据库（同一份 ce_schema_migrations，版本号已错开）。
	// 优先 MySQL；DSN 缺失则降级为内存实现（重启即丢，仅开发用）。
	var aStore announcement.Store
	if cfg.MySQL.DSN != "" {
		aStore, err = announcement.NewMySQLStore(cfg.MySQL.DSN)
		if err != nil {
			log.Warn("announcement: mysql unavailable, fallback to in-memory store", zap.Error(err))
		}
	}
	if aStore == nil {
		aStore = announcement.NewMemStore()
	}
	aSvc := announcement.NewService(aStore)
	aH := announcement.NewHandler(aSvc)

	r := gin.New()
	// 配置受信任代理后，c.ClientIP() 从 X-Forwarded-For 取真实客户端 IP；
	// 留空则不信任任何代理，使用直连对端 IP（RemoteAddr）。这让审计 IP、全局限流
	// 都能正确归因到真实来源（避免经网关/LB 转发时所有请求被误判为同一上游 IP）。
	middleware.ConfigureTrustedProxies(r, cfg, log)
	r.Use(middleware.Common(log, cfg)...)
	h.Register(r)
	aH.Register(r, verifier)

	log.Info("user service starting", zap.String("addr", *addr))
	if err := cfg.Listen(r, *addr); err != nil {
		log.Fatal("server exited", zap.Error(err))
	}
}

// newUserNotifSvc 构造通知服务：优先 MySQL（持久化站内信），失败降级内存。
func newUserNotifSvc(dsn string, log *zap.Logger) *notification.Service {
	if dsn != "" {
		db, err := sql.Open("mysql", dsn)
		if err == nil {
			store, merr := notification.NewMySQLStore(db)
			if merr == nil {
				return notification.New(store)
			}
			log.Warn("user: notification mysql migrate failed, fallback to in-memory", zap.Error(merr))
			_ = db.Close()
		} else {
			log.Warn("user: notification mysql open failed, fallback to in-memory", zap.Error(err))
		}
	}
	return notification.New(notification.NewMemStore())
}

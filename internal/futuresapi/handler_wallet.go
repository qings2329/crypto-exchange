package futuresapi

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// registerWalletRoutes 注册钱包/风控/指标相关路由（由 RegisterRoutes 调用）。
// 鉴权策略（F1/F4 复审修复）：全局已 Use AuthWithSkips（仅校验登录）。
//   - 特权/管理操作（手动入账、重组、全局冻结/解冻、风控开关、社会化分摊治理、拒绝提现）
//     一律加 middleware.AdminGuard()，普通用户禁止调用。
//   - 用户本人资金操作（提现请求、提现地址增删确认、坏账补缴、下单）在 handler 内强制
//     取 token uid（middleware.UserID(c)），忽略请求体 user_id，杜绝冒充他人资金。
func (s *Server) registerWalletRoutes(r *gin.Engine) {
	r.GET("/api/v1/futures/wallet", s.handleWallet)
	r.POST("/api/v1/futures/wallet/deposit", middleware.AdminGuard(), s.handleDeposit)
	// 用户侧自助充值（演示语义）：uid 取 token；白名单/单笔上限/频控见 handleDepositSelf。
	r.POST("/api/v1/futures/wallet/deposit/self", s.handleDepositSelf)
	r.POST("/api/v1/futures/wallet/deposit/chain", middleware.AdminGuard(), s.handleDepositChain)
	r.GET("/api/v1/futures/wallet/deposits", s.handleDeposits)
	r.POST("/api/v1/futures/wallet/deposit/reorg", middleware.AdminGuard(), s.handleDepositReorg)
	r.POST("/api/v1/futures/wallet/deposit/reorg/depth", middleware.AdminGuard(), s.handleDepositReorgDepth)
	// 链上直接提现绕过冷静期（等同管理员放行），属特权操作，必须管理员角色（F4 修复）。
	r.POST("/api/v1/futures/wallet/withdraw/chain", middleware.AdminGuard(), s.handleWithdrawChain)
	r.GET("/api/v1/futures/wallet/withdraws", s.handleWithdraws)
	r.POST("/api/v1/futures/wallet/withdraw/reorg", middleware.AdminGuard(), s.handleWithdrawReorg)
	r.POST("/api/v1/futures/wallet/withdraw/reorg/depth", middleware.AdminGuard(), s.handleWithdrawReorgDepth)
	r.POST("/api/v1/futures/wallet/withdraw/request", s.handleWithdrawRequest)
	r.POST("/api/v1/futures/wallet/withdraw/finalize", s.handleWithdrawFinalize)
	r.POST("/api/v1/futures/wallet/withdraw/cancel", s.handleWithdrawCancel)
	// 管理员审批/拒绝提现（Admin 后台接真实后端，§25）：approve 跳过冷却期直接放行，
	// reject 退回冻结；与用户端 finalize/cancel 并存，路径避开 withdraw 下的静态兄弟段以免路由冲突。
	// approve 跳过冷静期属特权放行，必须管理员角色（F4 修复）。
	r.POST("/api/v1/futures/wallet/withdraw/approve/:hold_id", middleware.AdminGuard(), s.handleWithdrawApprove)
	r.POST("/api/v1/futures/wallet/withdraw/reject/:hold_id", middleware.AdminGuard(), s.handleWithdrawReject)
	r.POST("/api/v1/futures/wallet/withdraw/emergency/freeze", middleware.AdminGuard(), s.handleEmergencyFreeze)
	r.POST("/api/v1/futures/wallet/withdraw/emergency/resume", middleware.AdminGuard(), s.handleEmergencyResume)
	r.POST("/api/v1/futures/wallet/risk/enable", middleware.AdminGuard(), s.handleRiskEnable)
	r.GET("/api/v1/futures/wallet/risk/events", s.handleRiskEvents)
	r.GET("/api/v1/futures/wallet/withdraw/holds", s.handleWithdrawHolds)
	r.POST("/api/v1/futures/wallet/withdraw/address", s.handleWithdrawAddressAdd)
	r.POST("/api/v1/futures/wallet/withdraw/address/confirm", s.handleWithdrawAddressConfirm)
	r.DELETE("/api/v1/futures/wallet/withdraw/address", s.handleWithdrawAddressDelete)
	r.GET("/api/v1/futures/wallet/withdraw/addresses", s.handleWithdrawAddresses)
	r.GET("/api/v1/futures/wallet/balance", s.handleWalletBalance)
	r.GET("/api/v1/futures/wallet/balances", s.handleWalletBalances)
	// 用户侧资金流水（本人，按 token uid 过滤；忽略请求体 user_id 防冒充，F4）。
	// 可选 ?asset= 按资产过滤、?limit= 截断条数（0/不传表示全部）。倒序（最新在前）。
	r.GET("/api/v1/futures/wallet/ledger", s.handleLedgerHistory)
	r.GET("/api/v1/futures/wallet/fee", s.handleWalletFee)
	r.GET("/api/v1/futures/wallet/baddebt", s.handleBadDebt)
	r.POST("/api/v1/futures/wallet/baddebt/repay", s.handleBadDebtRepay)
	r.POST("/api/v1/futures/wallet/baddebt/socialize/propose", middleware.AdminGuard(), s.handleSocializePropose)
	r.POST("/api/v1/futures/wallet/baddebt/socialize/approve", middleware.AdminGuard(), s.handleSocializeApprove)
	r.GET("/api/v1/futures/wallet/reconcile", s.handleReconcile)
	r.GET("/api/v1/futures/wallet/snapshot", s.handleSnapshot)
	r.POST("/api/v1/futures/wallet/snapshot/save", s.handleSnapshotSave)
	r.GET("/api/v1/futures/wallet/inventory", s.handleInventory)
	r.POST("/api/v1/futures/wallet/sweep", s.handleSweep)
	r.POST("/api/v1/futures/wallet/unsweep", s.handleUnsweep)
	r.GET("/metrics", s.handleMetrics)
}

// selfDepositGuard 是用户侧自助充值的内存频控（uid -> 最近请求时间窗）。
var (
	selfDepositMu     sync.Mutex
	selfDepositWindow = map[int64][]time.Time{}
)

const (
	selfDepositMaxAmount = 10000.0            // 单笔上限（USDT 计）
	selfDepositMaxPerMin = 6                  // 每分钟最多次数
	selfDepositAssets    = "USDT,BTC,ETH"     // 资产白名单（对齐 mock 网关 WALLET_ASSETS）
)

// allowSelfDeposit 滑动窗口频控：每 uid 每分钟最多 selfDepositMaxPerMin 次。
func allowSelfDeposit(uid int64) bool {
	now := time.Now()
	selfDepositMu.Lock()
	defer selfDepositMu.Unlock()
	recent := selfDepositWindow[uid][:0]
	for _, ts := range selfDepositWindow[uid] {
		if now.Sub(ts) < time.Minute {
			recent = append(recent, ts)
		}
	}
	if len(recent) >= selfDepositMaxPerMin {
		selfDepositWindow[uid] = recent
		return false
	}
	selfDepositWindow[uid] = append(recent, now)
	return true
}

// handleDepositSelf 用户侧自助充值（模拟链上确认后即时入账的演示语义）。
// 安全边界：归属用户一律取 token uid（防冒充）；资产白名单；单笔上限 + 分钟级频控，
// 防止脚本无限刷入虚假资金。与管理端 faucet（POST /deposit，AdminGuard）并存。
func (s *Server) handleDepositSelf(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok || uid <= 0 {
		response.Error(c, http.StatusUnauthorized, 401, "unauthorized")
		return
	}
	var req struct {
		Asset   string          `json:"asset"`
		Amount  json.Number     `json:"amount"`
		Network string          `json:"network"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 400, "bad request")
		return
	}
	asset := strings.ToUpper(strings.TrimSpace(req.Asset))
	if asset == "" {
		asset = "USDT"
	}
	if !strings.Contains(","+selfDepositAssets+",", ","+asset+",") {
		response.Error(c, 400, 400, "unsupported deposit asset")
		return
	}
	amt, err := req.Amount.Float64()
	if err != nil || amt <= 0 || amt > selfDepositMaxAmount {
		response.Error(c, 400, 400, "invalid amount (0 < amount <= "+strconv.FormatFloat(selfDepositMaxAmount, 'f', -1, 64)+")")
		return
	}
	if !allowSelfDeposit(uid) {
		response.Error(c, 429, 429, "too many deposits, retry later")
		return
	}
	depAmt, err := settlement.AssetAmountFromFloatSafe(amt, settlement.AssetDecimalsByName(asset))
	if err != nil {
		response.Error(c, 400, 400, "invalid amount: "+err.Error())
		return
	}
	// ref 唯一化：同额快速连充不被账本指纹去重吞掉。
	ref := fmt.Sprintf("deposit:self:%d:%d", uid, time.Now().UnixNano())
	if err := s.ledgerSvc.Deposit(uid, asset, depAmt, ref); err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	avail, frozen, _ := s.ledgerSvc.Balance(uid, asset)
	response.JSON(c, gin.H{
		"status":    "ok",
		"asset":     asset,
		"available": avail.HumanFloat(),
		"frozen":    frozen.HumanFloat(),
	})
}

// handleWallet 钱包余额查询（可用 / 冻结 + 保险基金 / 资金池）。
func (s *Server) handleWallet(c *gin.Context) {
	var uid int64
	if _, err := fmt.Sscanf(c.Query("user_id"), "%d", &uid); err != nil {
		response.Error(c, 400, 400, "bad user_id")
		return
	}
	avail, frozen, ok := s.ledgerSvc.Balance(uid, "USDT")
	if !ok {
		response.Error(c, 404, 404, "wallet not found")
		return
	}
	insAvail, _, _ := s.ledgerSvc.Balance(ledger.SysInsurance, "USDT")
	poolAvail, _, _ := s.ledgerSvc.Balance(ledger.SysFundingPool, "USDT")
	response.JSON(c, gin.H{
		"user_id":      uid,
		"asset":        "USDT",
		"available":    avail,
		"frozen":       frozen,
		"insurance":    insAvail,
		"funding_pool": poolAvail,
	})
}

// handleDeposit 演示用充值入口（生产充值来自链上确认/清结算系统，不应由 API 直接暴露）。
func (s *Server) handleDeposit(c *gin.Context) {
	var req struct {
		UserID int64   `json:"user_id"`
		Amount float64 `json:"amount"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Amount <= 0 {
		response.Error(c, 400, 400, "bad request")
		return
	}
	// M5：req.Amount 来自用户请求，须拦截 NaN/Inf，避免充值 0 落账。
	depAmt, err := settlement.AssetAmountFromFloatSafe(req.Amount, settlement.AssetDecimalsByName("USDT"))
	if err != nil {
		response.Error(c, 400, 400, "invalid amount: "+err.Error())
		return
	}
	if err := s.ledgerSvc.Deposit(req.UserID, "USDT", depAmt, "deposit"); err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, gin.H{"status": "ok"})
}

// handleDepositChain 链上充值入口：提交一笔用户链上充值，返回待确认事件。
func (s *Server) handleDepositChain(c *gin.Context) {
	var req struct {
		UserID  int64   `json:"user_id"`
		Asset   string  `json:"asset"`
		Chain   string  `json:"chain"`
		Amount  float64 `json:"amount"`
		Address string  `json:"address"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Amount <= 0 {
		response.Error(c, 400, 400, "bad request")
		return
	}
	depAmt, err := settlement.AssetAmountFromFloatSafe(req.Amount, settlement.AssetDecimals(settlement.Chain(req.Chain), req.Asset))
	if err != nil {
		response.Error(c, 400, 400, "invalid amount: "+err.Error())
		return
	}
	ev, err := s.chainGateway.SubmitDeposit(req.UserID, req.Asset, settlement.Chain(req.Chain), depAmt, req.Address)
	if err != nil {
		response.Error(c, 400, 400, err.Error())
		return
	}
	response.JSON(c, gin.H{
		"status":        "pending",
		"tx_hash":       ev.TxHash,
		"address":       ev.Address,
		"confirmations": ev.Confirmations,
		"required":      ev.Required,
		"est_seconds":   int(ev.Required) * int(s.chainGateway.Interval().Seconds()),
	})
}

// handleDeposits 链上充值记录（可按 user_id 过滤）。
func (s *Server) handleDeposits(c *gin.Context) {
	uid := parseUserID(c.Query("user_id"))
	all := s.chainGateway.Pending()
	if uid == 0 {
		response.JSON(c, gin.H{"deposits": all})
		return
	}
	out := make([]settlement.DepositEvent, 0)
	for _, ev := range all {
		if ev.UserID == uid {
			out = append(out, ev)
		}
	}
	response.JSON(c, gin.H{"deposits": out})
}

// SeedDemoDeposits 演示种子：向链上充值网关提交若干挂起充值记录，
// 使管理后台「充提币记录」页面在纯内存部署下有可展示的测试数据。
func (s *Server) SeedDemoDeposits() {
	type dep struct {
		uid   int64
		asset string
		chain settlement.Chain
		amt   float64
	}
	demo := []dep{
		{1, "USDT", settlement.ChainTRON, 25000},
		{2, "USDT", settlement.ChainETH, 10000},
		{3, "BTC", settlement.ChainBTC, 0.5},
		{4, "ETH", settlement.ChainETH, 12.25},
	}
	for _, d := range demo {
		amt, err := settlement.AssetAmountFromFloatSafe(d.amt, settlement.AssetDecimals(d.chain, d.asset))
		if err != nil {
			continue
		}
		addr := settlement.GenerateAddress(d.uid, d.chain)
		if _, err := s.chainGateway.SubmitDeposit(d.uid, d.asset, d.chain, amt, addr); err != nil {
			s.log.Error("seed deposit failed", zap.Error(err))
		}
	}
}

// handleDepositReorg 孤块重组回滚入口（演示用）。
func (s *Server) handleDepositReorg(c *gin.Context) {
	var req struct {
		TxHash string `json:"tx_hash"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.TxHash == "" {
		response.Error(c, 400, 400, "bad request")
		return
	}
	ev, err := s.chainGateway.Reorg(req.TxHash)
	if err != nil {
		response.Error(c, 404, 404, err.Error())
		return
	}
	response.JSON(c, gin.H{
		"status":  "orphaned",
		"tx_hash": ev.TxHash,
		"user_id": ev.UserID,
		"amount":  ev.Amount,
		"asset":   ev.Asset,
	})
}

// handleDepositReorgDepth 充值深度重组（演示）：回退最近 depth 个区块内所有已最终确认的充值。
func (s *Server) handleDepositReorgDepth(c *gin.Context) {
	var req struct {
		Depth int `json:"depth"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Depth <= 0 {
		response.Error(c, 400, 400, "bad request: depth must be > 0")
		return
	}
	rolled := s.chainGateway.ReorgDepth(req.Depth)
	items := make([]gin.H, 0, len(rolled))
	for _, ev := range rolled {
		items = append(items, gin.H{
			"tx_hash": ev.TxHash,
			"user_id": ev.UserID,
			"amount":  ev.Amount,
			"asset":   ev.Asset,
		})
	}
	response.JSON(c, gin.H{
		"status":       "depth_reorged",
		"depth":        req.Depth,
		"rolled_count": len(rolled),
		"rolled":       items,
	})
}

// handleWithdrawChain 链上提现入口：冻结用户可用余额后受理提现，返回待广播事件。
func (s *Server) handleWithdrawChain(c *gin.Context) {
	var req struct {
		UserID   int64   `json:"user_id"`
		Asset    string  `json:"asset"`
		Chain    string  `json:"chain"`
		Amount   float64 `json:"amount"`
		Fee      float64 `json:"fee"`
		Address  string  `json:"address"`
		WillFail bool    `json:"will_fail"` // 仅演示/单测注入失败路径
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Amount <= 0 {
		response.Error(c, 400, 400, "bad request")
		return
	}
	asset := req.Asset
	if asset == "" {
		asset = "USDT"
	}
	dec := settlement.AssetDecimals(settlement.Chain(req.Chain), asset)
	// M5：req.Amount/req.Fee 来自用户请求，须拦截 NaN/Inf，避免提现 0/负值落账。
	amt, err := settlement.AssetAmountFromFloatSafe(req.Amount, dec)
	if err != nil {
		response.Error(c, 400, 400, "invalid amount: "+err.Error())
		return
	}
	var feeAmt settlement.AssetAmount
	if req.Fee > 0 {
		feeAmt, err = settlement.AssetAmountFromFloatSafe(req.Fee, dec)
		if err != nil {
			response.Error(c, 400, 400, "invalid fee: "+err.Error())
			return
		}
	} else {
		feeAmt = s.feeModel.Estimate(settlement.Chain(req.Chain), asset, amt)
	}
	if s.ledgerSvc.IsOutflowRestricted(req.UserID, asset) {
		response.Error(c, 403, 403, "outflow restricted: repay outstanding bad debt first")
		return
	}
	// RISK-F3 修复：管理员「代客直提」路径此前绕过 risk 规则引擎（黑名单/限额/低 KYC/负金额），
	// 现与用户端 /withdraw/request 一致——冻结资金前先经 risk.CheckWithdraw，对目标用户生效。
	// 命中黑名单/超限额/低 KYC/负金额一律拒绝（403）；user 服务不可达 fail-closed（503）。
	if s.riskSvc != nil {
		kyc := math.MaxInt
		if s.kycFetcherByID != nil {
			k, kerr := s.kycFetcherByID(req.UserID)
			if kerr != nil {
				response.Error(c, 503, 503, "risk: cannot verify kyc")
				return
			}
			kyc = k
		}
		res, rerr := s.riskSvc.CheckWithdraw(req.UserID, asset, req.Amount, kyc, req.Address)
		if rerr != nil {
			response.Error(c, 500, 500, rerr.Error())
			return
		}
		if !res.Allowed {
			response.Error(c, 403, 403, res.Reason)
			return
		}
	}
	// M4 + 真实 hold 绑定：经 ledger.RequestWithdrawHold 创建提现冻结记录（内部已做余额、
	// 地址白名单、风控引擎、每日限额校验），取得 holdID 作为离线签名器来源校验锚点。账本划出
	// 仍由 WatchWithdraw 的 WithdrawCredited 事件驱动（与原有 FreezeWithdraw 路径一致，无双划出）。
	id, _, err := s.ledgerSvc.RequestWithdrawHold(req.UserID, asset, amt, feeAmt, req.Chain, req.Address)
	if err != nil {
		response.Error(c, 400, 400, err.Error())
		return
	}
	// M4 网关侧鉴权门控：广播前登记授权（绑定真实 holdID），使离线签名器仅接受经门控的提现。
	s.authorizeWithdraw(req.UserID, asset, settlement.Chain(req.Chain), amt, feeAmt, req.Address, id)
	ev, err := s.chainWithdraw.SubmitWithdraw(req.UserID, asset, settlement.Chain(req.Chain),
		amt, feeAmt, req.Address, req.WillFail)
	if err != nil {
		_ = s.ledgerSvc.CancelWithdrawHold(id) // 受理失败回退提现冻结（释放当日预占额度）
		response.Error(c, 400, 400, err.Error())
		return
	}
	// 广播成功：固化 hold 广播状态（供 M4 校验 hold 已广播、审计追溯）。账本划出由 WatchWithdraw 事件负责。
	if err := s.ledgerSvc.SetWithdrawTxHash(id, ev.TxHash); err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, gin.H{
		"status":        "pending",
		"tx_hash":       ev.TxHash,
		"address":       ev.Address,
		"amount":        ev.Amount,
		"fee":           ev.Fee,
		"confirmations": ev.Confirmations,
		"required":      ev.Required,
		"est_seconds":   int(ev.Required) * int(s.chainWithdraw.Interval().Seconds()),
	})
}

// handleWithdraws 链上提现记录（可按 user_id 过滤）。
func (s *Server) handleWithdraws(c *gin.Context) {
	uid := parseUserID(c.Query("user_id"))
	all := s.chainWithdraw.WithdrawHistory()
	if uid == 0 {
		response.JSON(c, gin.H{"withdraws": all})
		return
	}
	out := make([]settlement.WithdrawEvent, 0)
	for _, ev := range all {
		if ev.UserID == uid {
			out = append(out, ev)
		}
	}
	response.JSON(c, gin.H{"withdraws": out})
}

// handleWithdrawReorg 提现孤块重组回滚入口（演示用）。
func (s *Server) handleWithdrawReorg(c *gin.Context) {
	var req struct {
		TxHash string `json:"tx_hash"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.TxHash == "" {
		response.Error(c, 400, 400, "bad request")
		return
	}
	ev, err := s.chainWithdraw.WithdrawReorg(req.TxHash)
	if err != nil {
		response.Error(c, 404, 404, err.Error())
		return
	}
	response.JSON(c, gin.H{
		"status":  "orphaned",
		"tx_hash": ev.TxHash,
		"user_id": ev.UserID,
		"amount":  ev.Amount,
		"fee":     ev.Fee,
		"asset":   ev.Asset,
	})
}

// handleWithdrawReorgDepth 提现深度重组（演示）。
func (s *Server) handleWithdrawReorgDepth(c *gin.Context) {
	var req struct {
		Depth int `json:"depth"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Depth <= 0 {
		response.Error(c, 400, 400, "bad request: depth must be > 0")
		return
	}
	rolled := s.chainWithdraw.WithdrawReorgDepth(req.Depth)
	items := make([]gin.H, 0, len(rolled))
	for _, ev := range rolled {
		items = append(items, gin.H{
			"tx_hash": ev.TxHash,
			"user_id": ev.UserID,
			"amount":  ev.Amount,
			"fee":     ev.Fee,
			"asset":   ev.Asset,
		})
	}
	response.JSON(c, gin.H{
		"status":       "depth_reorged",
		"depth":        req.Depth,
		"rolled_count": len(rolled),
		"rolled":       items,
	})
}

// handleWithdrawRequest 提现安全冷静期通道：受理即冻结资金并入队，待冷静期过后由 finalize 清算。
// 身份强制取自 token（F4），忽略请求体 user_id，用户只能对自己发起提现。
func (s *Server) handleWithdrawRequest(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	var req struct {
		Asset   string  `json:"asset"`
		Chain   string  `json:"chain"`
		Amount  float64 `json:"amount"`
		Fee     float64 `json:"fee"`
		Address string  `json:"address"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Amount <= 0 {
		response.Error(c, 400, 400, "bad request")
		return
	}
	asset := req.Asset
	if asset == "" {
		asset = "USDT"
	}
	dec := settlement.AssetDecimals(settlement.Chain(req.Chain), asset)
	// M5：req.Amount/req.Fee 来自用户请求，须拦截 NaN/Inf，避免提现 0/负值落账。
	amt, err := settlement.AssetAmountFromFloatSafe(req.Amount, dec)
	if err != nil {
		response.Error(c, 400, 400, "invalid amount: "+err.Error())
		return
	}
	var feeAmt settlement.AssetAmount
	if req.Fee > 0 {
		feeAmt, err = settlement.AssetAmountFromFloatSafe(req.Fee, dec)
		if err != nil {
			response.Error(c, 400, 400, "invalid fee: "+err.Error())
			return
		}
	} else {
		feeAmt = s.feeModel.Estimate(settlement.Chain(req.Chain), asset, amt)
	}
	// 冻结额由与链上广播同源的 AssetAmount 派生，确保账本冻结额 == 链上划出额，消除 float 漂移（F2）。
	total := amt.Add(feeAmt)
	avail, _, ok := s.ledgerSvc.Balance(uid, asset)
	if !ok || avail.Cmp(total) < 0 {
		response.Error(c, 400, 400, "insufficient available balance")
		return
	}
	// 风控强制网关：冻结资金前先经 risk.CheckWithdraw。命中黑名单/超限额/低 KYC/负金额
	// 一律拒绝（403）；user 服务不可达则 fail-closed（503），确保资金安全时阻断。
	if s.riskSvc != nil {
		kyc, kerr := s.kycFetcher(c)
		if kerr != nil {
			response.Error(c, 503, 503, "risk: cannot verify kyc")
			return
		}
		res, rerr := s.riskSvc.CheckWithdraw(uid, asset, req.Amount, kyc, req.Address)
		if rerr != nil {
			response.Error(c, 500, 500, rerr.Error())
			return
		}
		if !res.Allowed {
			response.Error(c, 403, 403, res.Reason)
			return
		}
	}
	id, holdUntil, err := s.ledgerSvc.RequestWithdrawHold(uid, asset, amt, feeAmt, req.Chain, req.Address)
	if err != nil {
		response.Error(c, 403, 403, err.Error())
		return
	}
	response.JSON(c, gin.H{
		"status":       "held",
		"hold_id":      id,
		"asset":        asset,
		"amount":       req.Amount,
		"fee":          feeAmt.HumanFloat(),
		"hold_until":   holdUntil.Unix(),
		"hold_seconds": int(holdUntil.Sub(time.Now()).Seconds()),
	})
}

// authorizeWithdraw 在链上广播前登记提现门控授权（M4）：网关据此放行离线签名器签名。
// holdID 为对应的 ledger 提现冻结记录 ID，网关在签名前据此真正查 ledger.WithdrawHold 校验
// hold 存在/状态/要素一致（绑定真实提现记录，纵深防御）。网关未实现 WithdrawAuthorizer
// （如旧测试桩 / 模拟网关）时静默跳过，不阻断既有行为。
func (s *Server) authorizeWithdraw(userID int64, asset string, chain settlement.Chain, amount, fee settlement.AssetAmount, address string, holdID string) {
	if s.chainAuthorizer == nil {
		return
	}
	if _, err := s.chainAuthorizer.AuthorizeWithdraw(userID, asset, chain, amount, fee, address, holdID); err != nil {
		log.Printf("[futuresapi] WARN withdraw authorize failed: user=%d asset=%s chain=%s to=%s hold=%s err=%v",
			userID, asset, chain, address, holdID, err)
	}
}

// finalizeHold 清算一笔冷静期提现：先校验 hold 状态，再链上广播 + 账本划出。
// requireCooling 为真时（用户端 finalize）须等冷静期过后；为假时（管理员审批 approve）
// 跳过冷静期直接放行——冷却期是防用户误操作的，不适用于管理员显式授权放行。
// 返回 (hold 记录, 链上 tx_hash, HTTP 状态码, 错误)；status==0 表示成功。
func (s *Server) finalizeHold(id string, requireCooling bool) (*ledger.WithdrawHoldEntry, string, int, error) {
	e, ok := s.ledgerSvc.WithdrawHold(id)
	if !ok {
		return nil, "", 404, fmt.Errorf("withdraw hold not found")
	}
	if e.Finalized {
		return nil, "", 409, fmt.Errorf("withdraw hold already finalized")
	}
	if e.Cancelled {
		return nil, "", 409, fmt.Errorf("withdraw hold cancelled")
	}
	if requireCooling && time.Now().Before(e.HoldUntil) {
		return nil, "", 409, fmt.Errorf("withdraw hold in cooling period")
	}
	// 原子占有广播槽：已广播（重试/并发）则复用既有 txHash 跳过 SubmitWithdraw，杜绝重复链上广播（F1）。
	claimedTx, already, berr := s.ledgerSvc.ClaimWithdrawBroadcast(id)
	if berr != nil {
		return nil, "", 409, berr
	}
	var txHash string
	if already {
		txHash = claimedTx
	} else {
		var ev *settlement.WithdrawEvent
		// M4 网关侧鉴权门控：广播前登记授权（绑定真实 holdID，要素与本次 SubmitWithdraw 一致），
		// 使离线签名器仅接受经门控、绑定真实提现记录的提现。
		s.authorizeWithdraw(e.UserID, e.Asset, settlement.Chain(e.Chain), e.Amount, e.Fee, e.Address, id)
		ev, berr = s.chainWithdraw.SubmitWithdraw(e.UserID, e.Asset, settlement.Chain(e.Chain),
			e.Amount, e.Fee, e.Address, false)
		if berr != nil {
			_ = s.ledgerSvc.ResetWithdrawBroadcast(id) // 广播失败释放槽，允许重试重新广播
			return nil, "", 502, fmt.Errorf("broadcast failed: %v", berr)
		}
		if err := s.ledgerSvc.SetWithdrawTxHash(id, ev.TxHash); err != nil {
			return nil, "", 502, err
		}
		txHash = ev.TxHash
	}
	var ferr error
	if requireCooling {
		_, ferr = s.ledgerSvc.FinalizeWithdrawHold(id)
	} else {
		// 管理员审批放行：跳过冷静期直接清算（§25）。
		_, ferr = s.ledgerSvc.FinalizeWithdrawHoldForce(id)
	}
	if ferr != nil {
		return nil, "", 409, ferr
	}
	return e, txHash, 0, nil
}

// handleWithdrawFinalize 清算一笔冷静期提现（用户端）：先链上广播，成功后账本划出。
// 受冷静期守卫——冷静期内拒绝放行（防用户误操作/被钓鱼）。
func (s *Server) handleWithdrawFinalize(c *gin.Context) {
	var req struct {
		HoldID string `json:"hold_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.HoldID == "" {
		response.Error(c, 400, 400, "bad request")
		return
	}
	e, txHash, status, ferr := s.finalizeHold(req.HoldID, true)
	if ferr != nil {
		response.Error(c, status, status, ferr.Error())
		return
	}
	response.JSON(c, gin.H{
		"status":  "finalized",
		"hold_id": e.ID,
		"tx_hash": txHash,
		"amount":  e.Amount,
		"fee":     e.Fee,
	})
}

// handleWithdrawApprove 管理员审批通过一笔提现：跳过冷静期直接放行（链上广播 + 账本划出）。
// 这是管理后台提币审核的真正落地闸门（§25），替代此前仅写内存会话态的伪审批。
func (s *Server) handleWithdrawApprove(c *gin.Context) {
	id := c.Param("hold_id")
	if id == "" {
		response.Error(c, 400, 400, "bad request")
		return
	}
	e, txHash, status, ferr := s.finalizeHold(id, false)
	if ferr != nil {
		response.Error(c, status, status, ferr.Error())
		return
	}
	response.JSON(c, gin.H{
		"status":  "approved",
		"hold_id": e.ID,
		"tx_hash": txHash,
		"amount":  e.Amount,
		"fee":     e.Fee,
	})
}

// handleWithdrawReject 管理员拒绝一笔提现：退回冻结资金到可用（不链上广播）。
func (s *Server) handleWithdrawReject(c *gin.Context) {
	id := c.Param("hold_id")
	if id == "" {
		response.Error(c, 400, 400, "bad request")
		return
	}
	e, ok := s.ledgerSvc.WithdrawHold(id)
	if !ok {
		response.Error(c, 404, 404, "withdraw hold not found")
		return
	}
	if e.Finalized {
		response.Error(c, 409, 409, "withdraw hold already finalized")
		return
	}
	if e.Cancelled {
		response.Error(c, 409, 409, "withdraw hold cancelled")
		return
	}
	if err := s.ledgerSvc.CancelWithdrawHold(id); err != nil {
		response.Error(c, 409, 409, err.Error())
		return
	}
	response.JSON(c, gin.H{"status": "rejected", "hold_id": id})
}

// handleWithdrawCancel 撤销一笔未清算的提现：退回冻结资金到可用。
func (s *Server) handleWithdrawCancel(c *gin.Context) {
	var req struct {
		HoldID string `json:"hold_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.HoldID == "" {
		response.Error(c, 400, 400, "bad request")
		return
	}
	if err := s.ledgerSvc.CancelWithdrawHold(req.HoldID); err != nil {
		response.Error(c, 409, 409, err.Error())
		return
	}
	response.JSON(c, gin.H{"status": "cancelled", "hold_id": req.HoldID})
}

// handleEmergencyFreeze 全局紧急冻结：一键冻结所有出金受理。
func (s *Server) handleEmergencyFreeze(c *gin.Context) {
	s.ledgerSvc.SetGlobalWithdrawalFreeze(true)
	response.JSON(c, gin.H{"status": "frozen", "global_withdrawal_freeze": true})
}

// handleEmergencyResume 全局解冻：解除冻结并清零风控自动冻结标记。
func (s *Server) handleEmergencyResume(c *gin.Context) {
	s.ledgerSvc.SetGlobalWithdrawalFreeze(false)
	s.ledgerSvc.ClearRiskAutoFreeze()
	response.JSON(c, gin.H{"status": "resumed", "global_withdrawal_freeze": false})
}

// handleRiskEnable 风控引擎开关/配置。
func (s *Server) handleRiskEnable(c *gin.Context) {
	var req struct {
		Enabled    bool    `json:"enabled"`
		AutoFreeze bool    `json:"auto_freeze"`
		WindowSec  int     `json:"window_sec"`
		VelAmount  float64 `json:"velocity_amount"`
		VelCount   int     `json:"velocity_count"`
		AddrBurst  int     `json:"addr_burst"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 400, "bad request")
		return
	}
	s.ledgerSvc.EnableRiskEngine(req.Enabled, req.AutoFreeze)
	if req.WindowSec > 0 || req.VelAmount > 0 || req.VelCount > 0 || req.AddrBurst > 0 {
		s.ledgerSvc.SetRiskThresholds(
			time.Duration(req.WindowSec)*time.Second,
			req.VelAmount, req.VelCount, req.AddrBurst)
	}
	response.JSON(c, gin.H{
		"status":              "ok",
		"risk_enabled":        s.ledgerSvc.IsRiskEngineEnabled(),
		"auto_frozen_by_risk": s.ledgerSvc.AutoFrozenByRisk(),
	})
}

// handleRiskEvents 风控事件查询（by user_id 过滤，0 表示全部）。
func (s *Server) handleRiskEvents(c *gin.Context) {
	uid := parseUserID(c.Query("user_id"))
	response.JSON(c, gin.H{
		"events":              s.ledgerSvc.ListRiskEvents(uid),
		"risk_enabled":        s.ledgerSvc.IsRiskEngineEnabled(),
		"auto_frozen_by_risk": s.ledgerSvc.AutoFrozenByRisk(),
	})
}

// handleWithdrawHolds 提现冷静期队列查询。
func (s *Server) handleWithdrawHolds(c *gin.Context) {
	uid := parseUserID(c.Query("user_id"))
	holds := s.ledgerSvc.ListWithdrawHolds(uid)
	response.JSON(c, gin.H{
		"holds":         holds,
		"global_freeze": s.ledgerSvc.IsGlobalWithdrawalFrozen(),
		"hold_period":   int(s.ledgerSvc.WithdrawHoldPeriod().Seconds()),
	})
}

// handleWithdrawAddressAdd 预登记一条出金地址（默认未验证）。身份强制取自 token（F4）。
func (s *Server) handleWithdrawAddressAdd(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	var req struct {
		Asset   string `json:"asset"`
		Chain   string `json:"chain"`
		Address string `json:"address"`
		Label   string `json:"label"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Address == "" {
		response.Error(c, 400, 400, "bad request: address required")
		return
	}
	if req.Asset == "" {
		req.Asset = "USDT"
	}
	addr, err := s.ledgerSvc.AddWithdrawAddress(uid, req.Asset, req.Chain, req.Address, req.Label)
	if err != nil {
		response.Error(c, 409, 409, err.Error())
		return
	}
	response.JSON(c, gin.H{
		"status":         "registered",
		"user_id":        addr.UserID,
		"asset":          addr.Asset,
		"chain":          addr.Chain,
		"address":        addr.Address,
		"label":          addr.Label,
		"verified":       addr.Verified,
		"verify_until":   addr.VerifyUntil.Unix(),
		"verify_seconds": int(addr.VerifyUntil.Sub(time.Now()).Seconds()),
	})
}

// handleWithdrawAddressConfirm 验证一条预登记地址（模拟 2FA/邮件验证通过）。身份强制取自 token（F4）。
func (s *Server) handleWithdrawAddressConfirm(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	var req struct {
		Asset   string `json:"asset"`
		Chain   string `json:"chain"`
		Address string `json:"address"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Address == "" {
		response.Error(c, 400, 400, "bad request: address required")
		return
	}
	if req.Asset == "" {
		req.Asset = "USDT"
	}
	if err := s.ledgerSvc.ConfirmWithdrawAddress(uid, req.Asset, req.Chain, req.Address); err != nil {
		response.Error(c, 404, 404, err.Error())
		return
	}
	response.JSON(c, gin.H{"status": "verified", "user_id": uid, "asset": req.Asset, "chain": req.Chain, "address": req.Address})
}

// handleWithdrawAddressDelete 撤销一条已登记地址。身份强制取自 token（F4）。
func (s *Server) handleWithdrawAddressDelete(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	var req struct {
		Asset   string `json:"asset"`
		Chain   string `json:"chain"`
		Address string `json:"address"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Address == "" {
		response.Error(c, 400, 400, "bad request: address required")
		return
	}
	if req.Asset == "" {
		req.Asset = "USDT"
	}
	if err := s.ledgerSvc.RemoveWithdrawAddress(uid, req.Asset, req.Chain, req.Address); err != nil {
		response.Error(c, 404, 404, err.Error())
		return
	}
	response.JSON(c, gin.H{"status": "removed", "user_id": uid, "asset": req.Asset, "chain": req.Chain, "address": req.Address})
}

// handleWithdrawAddresses 查询提现地址白名单（可按 user_id 过滤）。
func (s *Server) handleWithdrawAddresses(c *gin.Context) {
	uid := parseUserID(c.Query("user_id"))
	addrs := s.ledgerSvc.ListWithdrawAddresses(uid)
	items := make([]gin.H, 0, len(addrs))
	now := time.Now()
	for _, a := range addrs {
		items = append(items, gin.H{
			"user_id":      a.UserID,
			"asset":        a.Asset,
			"chain":        a.Chain,
			"address":      a.Address,
			"label":        a.Label,
			"verified":     a.Verified,
			"verify_until": a.VerifyUntil.Unix(),
			"usable":       a.Verified && !now.Before(a.VerifyUntil),
		})
	}
	response.JSON(c, gin.H{
		"addresses":         items,
		"verify_period":     int(s.ledgerSvc.AddressVerifyPeriod().Seconds()),
		"whitelisted_total": s.ledgerSvc.WithdrawAddressCount(),
	})
}

// handleWalletBalance 余额查询（可用/冻结/提现冻结）。
func (s *Server) handleWalletBalance(c *gin.Context) {
	uid := parseUserID(c.Query("user_id"))
	asset := c.DefaultQuery("asset", "USDT")
	if uid <= 0 {
		response.Error(c, 400, 400, "user_id required")
		return
	}
	response.JSON(c, s.walletSummary(uid, asset))
}

// handleWalletBalances 用户侧全资产余额（本人）。身份强制来自鉴权 token（F4），
// 忽略任何 user_id 查询参数/请求体，防止冒充他人查询。返回该用户 USDT/BTC/ETH
// 各资产的可用/冻结/提现冻结汇总，等价于 mock 网关 /futures/wallet/balance 的数组契约，
// 供前端下单面板/资产总览直接使用。
func (s *Server) handleWalletBalances(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok || uid <= 0 {
		response.Error(c, http.StatusUnauthorized, 401, "unauthorized")
		return
	}
	rows := make([]gin.H, 0, 3)
	for _, asset := range strings.Split(selfDepositAssets, ",") {
		avail, frozen, okB := s.ledgerSvc.Balance(uid, asset)
		wf, _ := s.ledgerSvc.WithdrawFrozenBalance(uid, asset)
		if !okB && avail.IsZero() && frozen.IsZero() && wf.IsZero() {
			// 无任何持仓/流水的资产不返回（对齐 mock：仅返回发生过资金活动的资产）。
			continue
		}
		rows = append(rows, gin.H{
			"asset":           asset,
			"available":       avail,
			"frozen":          frozen,
			"withdraw_frozen": wf,
			"exists":          okB,
		})
	}
	response.JSON(c, rows)
}

// handleLedgerHistory 用户侧资金流水（本人）。身份强制来自鉴权 token（F4），
// 忽略任何请求体/查询中的 user_id，杜绝冒充查询他人流水。可选 asset/limit 过滤。
func (s *Server) handleLedgerHistory(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	asset := c.Query("asset")
	limit, _ := strconv.Atoi(c.Query("limit"))

	entries := s.ledgerSvc.UserHistory(uid)
	if asset != "" {
		filtered := make([]ledger.Entry, 0, len(entries))
		for _, e := range entries {
			if e.Asset == asset {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	response.JSON(c, gin.H{"entries": entries})
}

// handleWalletFee 手续费估算：按多链/多资产费率模型返回某笔提现的手续费。
func (s *Server) handleWalletFee(c *gin.Context) {
	chain := c.DefaultQuery("chain", string(settlement.ChainETH))
	asset := c.DefaultQuery("asset", "USDT")
	// 解析失败（非数字）或负数不得静默当 0，否则返回误导性的 fee=0 估算（F5a 修复）。
	raw := c.DefaultQuery("amount", "0")
	amount, err := strconv.ParseFloat(raw, 64)
	if err != nil || amount < 0 {
		response.Error(c, 400, 400, "bad amount")
		return
	}
	f, ok := s.feeModel.Lookup(settlement.Chain(chain), asset)
	// M5：query 字符串 "NaN" 经 ParseFloat 会得到 NaN 且无错误，须 Safe 拦截，避免 fee 估算落 0。
	amt, err := settlement.AssetAmountFromFloatSafe(amount, settlement.AssetDecimals(settlement.Chain(chain), asset))
	if err != nil {
		response.Error(c, 400, 400, "bad amount")
		return
	}
	est := s.feeModel.Estimate(settlement.Chain(chain), asset, amt)
	response.JSON(c, gin.H{
		"chain":      chain,
		"asset":      asset,
		"amount":     amount,
		"fee":        est,
		"base":       f.Base,
		"rate":       f.Rate,
		"registered": ok,
	})
}

// handleBadDebt 坏账查询。
func (s *Server) handleBadDebt(c *gin.Context) {
	asset := c.DefaultQuery("asset", "USDT")
	total := s.ledgerSvc.BadDebtTotal(asset)
	bdBal, _, _ := s.ledgerSvc.Balance(ledger.SysBadDebt, asset)
	response.JSON(c, gin.H{
		"asset":            asset,
		"bad_debt_total":   total,
		"sys_bad_debt_bal": bdBal,
	})
}

// handleBadDebtRepay 坏账补缴：用户主动用可用余额冲抵交易所垫付的坏账。身份强制取自 token（F4）。
func (s *Server) handleBadDebtRepay(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	var req struct {
		Asset  string  `json:"asset"`
		Amount float64 `json:"amount"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Amount <= 0 {
		response.Error(c, 400, 400, "bad request")
		return
	}
	if req.Asset == "" {
		req.Asset = "USDT"
	}
	ref := fmt.Sprintf("repay:%d", uid)
	// M5：req.Amount 来自用户请求，须拦截 NaN/Inf，避免坏债还款落 0。
	repayAmt, err := settlement.AssetAmountFromFloatSafe(req.Amount, settlement.AssetDecimalsByName(req.Asset))
	if err != nil {
		response.Error(c, 400, 400, "invalid amount: "+err.Error())
		return
	}
	if err := s.ledgerSvc.RepayBadDebt(uid, req.Asset, repayAmt, ref); err != nil {
		response.Error(c, 400, 400, err.Error())
		return
	}
	response.JSON(c, gin.H{
		"status":          "repaid",
		"user_id":         uid,
		"asset":           req.Asset,
		"repaid":          req.Amount,
		"bad_debt_remain": s.ledgerSvc.BadDebtTotal(req.Asset),
	})
}

// handleSocializePropose 坏账社会化分摊治理（两步审批）：先 propose 生成待审批提案。
func (s *Server) handleSocializePropose(c *gin.Context) {
	var req struct {
		Asset string `json:"asset"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 400, "bad request")
		return
	}
	if req.Asset == "" {
		req.Asset = "USDT"
	}
	id, preview, err := s.ledgerSvc.ProposeSocialize(req.Asset)
	if err != nil {
		response.Error(c, 400, 400, err.Error())
		return
	}
	shares := make(map[string]float64, len(preview.Detail))
	for uid, amt := range preview.Detail {
		shares[fmt.Sprintf("%d", uid)] = amt.HumanFloat()
	}
	response.JSON(c, gin.H{
		"proposal_id": id,
		"asset":       preview.Asset,
		"recovered":   preview.Recovered,
		"shares":      shares,
		"status":      preview.Status,
	})
}

// handleSocializeApprove 坏账社会化分摊治理：approve 执行分摊。
func (s *Server) handleSocializeApprove(c *gin.Context) {
	var req struct {
		Asset      string `json:"asset"`
		ProposalID string `json:"proposal_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, 400, 400, "bad request")
		return
	}
	if req.Asset == "" {
		req.Asset = "USDT"
	}
	detail, recovered, err := s.ledgerSvc.ApproveSocialize(req.Asset, req.ProposalID)
	if err != nil {
		response.Error(c, 400, 400, err.Error())
		return
	}
	resp := gin.H{
		"status":          "socialized",
		"asset":           req.Asset,
		"recovered":       recovered,
		"bad_debt_remain": s.ledgerSvc.BadDebtTotal(req.Asset),
	}
	shares := make(map[string]float64, len(detail))
	for uid, amt := range detail {
		shares[fmt.Sprintf("%d", uid)] = amt.HumanFloat()
	}
	resp["shares"] = shares
	response.JSON(c, resp)
}

// handleReconcile 复式记账对账探针：返回每个资产的借贷平衡偏差与链上库存-负债不变量。
func (s *Server) handleReconcile(c *gin.Context) {
	dev := s.ledgerSvc.Reconcile()
	balanced := s.ledgerSvc.IsBalanced()
	stats := s.ledgerSvc.LastReconcile()
	resp := gin.H{
		"balanced":  balanced,
		"deviation": dev,
		"monitor": gin.H{
			"last_run":        stats.LastRun.Unix(),
			"last_balanced":   stats.LastBalanced,
			"imbalance_count": stats.ImbalanceCount,
			"last_deviation":  stats.LastDeviation,
		},
	}
	invDev := make(map[string]float64)
	for asset := range dev {
		invDev[asset] = s.ledgerSvc.InventoryMatchesLiability(asset).HumanFloat()
	}
	resp["inventory_deviation"] = invDev
	if !balanced {
		resp["alert"] = "LEDGER_IMBALANCE: non-zero deviation detected, possible unpaired minting or lost entries"
	}
	resp["inventory_alert"] = "INVENTORY_MISMATCH: on-chain holding diverges from user liabilities, possible minting/lost coins"
	response.JSON(c, resp)
}

// handleSnapshot 账本快照下载（审计/外部备份用）。
func (s *Server) handleSnapshot(c *gin.Context) {
	snap := s.ledgerSvc.Snapshot()
	response.JSON(c, gin.H{
		"accounts":            snap.Accounts,
		"restricted":          snap.Restricted,
		"bad_debt_by_user":    snap.BadDebtByUser,
		"socialize_proposals": snap.SocializeProposals,
		"seq":                 snap.Seq,
		"entries":             len(snap.Log),
	})
}

// handleSnapshotSave 账本快照主动保存：把当前账本状态落库到 MySQL。
func (s *Server) handleSnapshotSave(c *gin.Context) {
	if s.dsn == "" {
		response.Error(c, 400, 400, "mysql persistence not configured")
		return
	}
	var req struct {
		LedgerID string `json:"ledger_id"`
	}
	_ = c.ShouldBindJSON(&req)
	id := req.LedgerID
	if id == "" {
		id = "futures"
	}
	if err := s.ledgerSvc.SaveToMySQL(s.dsn, id); err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	response.JSON(c, gin.H{"status": "saved", "ledger_id": id})
}

// handleInventory 链上钱包库存与风险敞口。
func (s *Server) handleInventory(c *gin.Context) {
	asset := c.DefaultQuery("asset", "")
	assets := []string{}
	if asset != "" {
		assets = []string{asset}
	} else {
		assets = []string{"USDT", "ETH", "BTC"}
	}
	items := make([]gin.H, 0, len(assets))
	for _, a := range assets {
		items = append(items, gin.H{
			"asset":            a,
			"hot_wallet":       s.ledgerSvc.HotWalletBalance(a),
			"cold_wallet":      s.ledgerSvc.ColdWalletBalance(a),
			"hot_cap":          s.ledgerSvc.HotWalletCap(a),
			"hot_excess":       s.ledgerSvc.HotWalletExcess(a),
			"onchain_total":    s.ledgerSvc.HotWalletBalance(a).Add(s.ledgerSvc.ColdWalletBalance(a)),
			"inv_vs_liability": s.ledgerSvc.InventoryMatchesLiability(a),
		})
	}
	response.JSON(c, gin.H{"inventory": items})
}

// handleSweep 手动归集：将热钱包资金归集到冷钱包。
func (s *Server) handleSweep(c *gin.Context) {
	var req struct {
		Asset  string  `json:"asset"`
		Amount float64 `json:"amount"` // 0 表示全额归集热钱包余额
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Asset == "" {
		response.Error(c, 400, 400, "bad request: asset required")
		return
	}
	// M5：req.Amount 来自用户请求（0 表示全额归集），须拦截 NaN/Inf。
	amount, err := settlement.AssetAmountFromFloatSafe(req.Amount, settlement.AssetDecimalsByName(req.Asset))
	if err != nil {
		response.Error(c, 400, 400, "invalid amount: "+err.Error())
		return
	}
	if amount.Sign() <= 0 {
		amount = s.ledgerSvc.HotWalletBalance(req.Asset)
	}
	swept, err := s.ledgerSvc.SweepToCold(req.Asset, amount)
	if err != nil {
		response.Error(c, 400, 400, err.Error())
		return
	}
	response.JSON(c, gin.H{
		"status":      "swept",
		"asset":       req.Asset,
		"swept":       swept,
		"hot_wallet":  s.ledgerSvc.HotWalletBalance(req.Asset),
		"cold_wallet": s.ledgerSvc.ColdWalletBalance(req.Asset),
	})
}

// handleUnsweep 手动回调：从冷钱包调拨资金回热钱包。
func (s *Server) handleUnsweep(c *gin.Context) {
	var req struct {
		Asset  string  `json:"asset"`
		Amount float64 `json:"amount"` // 0 表示全额调拨冷钱包余额
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Asset == "" {
		response.Error(c, 400, 400, "bad request: asset required")
		return
	}
	// M5：req.Amount 来自用户请求（0 表示全额调拨），须拦截 NaN/Inf。
	amount, err := settlement.AssetAmountFromFloatSafe(req.Amount, settlement.AssetDecimalsByName(req.Asset))
	if err != nil {
		response.Error(c, 400, 400, "invalid amount: "+err.Error())
		return
	}
	if amount.Sign() <= 0 {
		amount = s.ledgerSvc.ColdWalletBalance(req.Asset)
	}
	moved, err := s.ledgerSvc.UnsweepFromCold(req.Asset, amount)
	if err != nil {
		response.Error(c, 400, 400, err.Error())
		return
	}
	response.JSON(c, gin.H{
		"status":      "unswept",
		"asset":       req.Asset,
		"unswept":     moved,
		"hot_wallet":  s.ledgerSvc.HotWalletBalance(req.Asset),
		"cold_wallet": s.ledgerSvc.ColdWalletBalance(req.Asset),
	})
}

// handleMetrics Prometheus 风格指标端点（手写 exposition 格式，不引入额外依赖）。
func (s *Server) handleMetrics(c *gin.Context) {
	dev := s.ledgerSvc.Reconcile()
	stats := s.ledgerSvc.LastReconcile()
	var b strings.Builder
	b.WriteString("# HELP crypto_exchange_ledger_deviation 复式记账各资产借贷偏差（应恒为0）\n")
	b.WriteString("# TYPE crypto_exchange_ledger_deviation gauge\n")
	for asset, v := range dev {
		fmt.Fprintf(&b, "crypto_exchange_ledger_deviation{asset=%q} %.8f\n", asset, v.HumanFloat())
	}
	b.WriteString("# HELP crypto_exchange_bad_debt_total 各资产未冲抵坏账总额\n")
	b.WriteString("# TYPE crypto_exchange_bad_debt_total gauge\n")
	for asset := range dev {
		fmt.Fprintf(&b, "crypto_exchange_bad_debt_total{asset=%q} %.8f\n", asset, s.ledgerSvc.BadDebtTotal(asset).HumanFloat())
	}
	b.WriteString("# HELP crypto_exchange_reconcile_imbalance_total 对账巡检累计不平账次数\n")
	b.WriteString("# TYPE crypto_exchange_reconcile_imbalance_total counter\n")
	fmt.Fprintf(&b, "crypto_exchange_reconcile_imbalance_total %d\n", stats.ImbalanceCount)
	b.WriteString("# HELP crypto_exchange_restricted_users 处于出金限制的用户-资产条目数\n")
	b.WriteString("# TYPE crypto_exchange_restricted_users gauge\n")
	fmt.Fprintf(&b, "crypto_exchange_restricted_users %d\n", s.ledgerSvc.RestrictedCount())
	b.WriteString("# HELP crypto_exchange_hot_wallet_balance 热钱包链上持仓（持续暴露于热私钥泄露风险）\n")
	b.WriteString("# TYPE crypto_exchange_hot_wallet_balance gauge\n")
	for _, asset := range []string{"USDT", "ETH", "BTC"} {
		fmt.Fprintf(&b, "crypto_exchange_hot_wallet_balance{asset=%q} %.8f\n", asset, s.ledgerSvc.HotWalletBalance(asset).HumanFloat())
	}
	b.WriteString("# HELP crypto_exchange_cold_wallet_balance 冷钱包链上持仓（离线多签保管）\n")
	b.WriteString("# TYPE crypto_exchange_cold_wallet_balance gauge\n")
	for _, asset := range []string{"USDT", "ETH", "BTC"} {
		fmt.Fprintf(&b, "crypto_exchange_cold_wallet_balance{asset=%q} %.8f\n", asset, s.ledgerSvc.ColdWalletBalance(asset).HumanFloat())
	}
	b.WriteString("# HELP crypto_exchange_hot_wallet_excess 热钱包超出风险敞口上限的额度（>0 应告警并归集）\n")
	b.WriteString("# TYPE crypto_exchange_hot_wallet_excess gauge\n")
	for _, asset := range []string{"USDT", "ETH", "BTC"} {
		fmt.Fprintf(&b, "crypto_exchange_hot_wallet_excess{asset=%q} %.8f\n", asset, s.ledgerSvc.HotWalletExcess(asset).HumanFloat())
	}
	b.WriteString("# HELP crypto_exchange_inventory_mismatch 链上持仓与对用户净负债的偏差（应恒为0，否则账本与链脱节）\n")
	b.WriteString("# TYPE crypto_exchange_inventory_mismatch gauge\n")
	for _, asset := range []string{"USDT", "ETH", "BTC"} {
		fmt.Fprintf(&b, "crypto_exchange_inventory_mismatch{asset=%q} %.8f\n", asset, s.ledgerSvc.InventoryMatchesLiability(asset).HumanFloat())
	}
	b.WriteString("# HELP crypto_exchange_pending_withdraw_holds 处于冷静期、尚未链上清算的提现请求数\n")
	b.WriteString("# TYPE crypto_exchange_pending_withdraw_holds gauge\n")
	fmt.Fprintf(&b, "crypto_exchange_pending_withdraw_holds %d\n", s.ledgerSvc.PendingWithdrawHoldCount())
	b.WriteString("# HELP crypto_exchange_global_withdrawal_freeze 全局紧急冻结开关（1=冻结所有出金，0=正常）\n")
	b.WriteString("# TYPE crypto_exchange_global_withdrawal_freeze gauge\n")
	fz := 0
	if s.ledgerSvc.IsGlobalWithdrawalFrozen() {
		fz = 1
	}
	fmt.Fprintf(&b, "crypto_exchange_global_withdrawal_freeze %d\n", fz)
	b.WriteString("# HELP crypto_exchange_whitelisted_addresses 提现地址白名单条目总数\n")
	b.WriteString("# TYPE crypto_exchange_whitelisted_addresses gauge\n")
	fmt.Fprintf(&b, "crypto_exchange_whitelisted_addresses %d\n", s.ledgerSvc.WithdrawAddressCount())
	b.WriteString("# HELP crypto_exchange_risk_events_total 风控引擎累计可疑行为事件数\n")
	b.WriteString("# TYPE crypto_exchange_risk_events_total gauge\n")
	fmt.Fprintf(&b, "crypto_exchange_risk_events_total %d\n", s.ledgerSvc.RiskEventCount())
	b.WriteString("# HELP crypto_exchange_risk_auto_freeze 当前全局冻结是否由风控引擎自动触发\n")
	b.WriteString("# TYPE crypto_exchange_risk_auto_freeze gauge\n")
	af := 0
	if s.ledgerSvc.AutoFrozenByRisk() {
		af = 1
	}
	fmt.Fprintf(&b, "crypto_exchange_risk_auto_freeze %d\n", af)
	c.Data(200, "text/plain; version=0.0.4", []byte(b.String()))
}

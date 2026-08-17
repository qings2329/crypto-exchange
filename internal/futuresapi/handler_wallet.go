package futuresapi

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// registerWalletRoutes 注册钱包/风控/指标相关路由（由 RegisterRoutes 调用）。
func (s *Server) registerWalletRoutes(r *gin.Engine) {
	r.GET("/api/v1/futures/wallet", s.handleWallet)
	r.POST("/api/v1/futures/wallet/deposit", s.handleDeposit)
	r.POST("/api/v1/futures/wallet/deposit/chain", s.handleDepositChain)
	r.GET("/api/v1/futures/wallet/deposits", s.handleDeposits)
	r.POST("/api/v1/futures/wallet/deposit/reorg", s.handleDepositReorg)
	r.POST("/api/v1/futures/wallet/deposit/reorg/depth", s.handleDepositReorgDepth)
	// 链上直接提现绕过冷静期（等同管理员放行），属特权操作，必须管理员角色（F4 修复）。
	r.POST("/api/v1/futures/wallet/withdraw/chain", middleware.AdminGuard(), s.handleWithdrawChain)
	r.GET("/api/v1/futures/wallet/withdraws", s.handleWithdraws)
	r.POST("/api/v1/futures/wallet/withdraw/reorg", s.handleWithdrawReorg)
	r.POST("/api/v1/futures/wallet/withdraw/reorg/depth", s.handleWithdrawReorgDepth)
	r.POST("/api/v1/futures/wallet/withdraw/request", s.handleWithdrawRequest)
	r.POST("/api/v1/futures/wallet/withdraw/finalize", s.handleWithdrawFinalize)
	r.POST("/api/v1/futures/wallet/withdraw/cancel", s.handleWithdrawCancel)
	// 管理员审批/拒绝提现（Admin 后台接真实后端，§25）：approve 跳过冷却期直接放行，
	// reject 退回冻结；与用户端 finalize/cancel 并存，路径避开 withdraw 下的静态兄弟段以免路由冲突。
	// approve 跳过冷静期属特权放行，必须管理员角色（F4 修复）。
	r.POST("/api/v1/futures/wallet/withdraw/approve/:hold_id", middleware.AdminGuard(), s.handleWithdrawApprove)
	r.POST("/api/v1/futures/wallet/withdraw/reject/:hold_id", s.handleWithdrawReject)
	r.POST("/api/v1/futures/wallet/withdraw/emergency/freeze", s.handleEmergencyFreeze)
	r.POST("/api/v1/futures/wallet/withdraw/emergency/resume", s.handleEmergencyResume)
	r.POST("/api/v1/futures/wallet/risk/enable", s.handleRiskEnable)
	r.GET("/api/v1/futures/wallet/risk/events", s.handleRiskEvents)
	r.GET("/api/v1/futures/wallet/withdraw/holds", s.handleWithdrawHolds)
	r.POST("/api/v1/futures/wallet/withdraw/address", s.handleWithdrawAddressAdd)
	r.POST("/api/v1/futures/wallet/withdraw/address/confirm", s.handleWithdrawAddressConfirm)
	r.DELETE("/api/v1/futures/wallet/withdraw/address", s.handleWithdrawAddressDelete)
	r.GET("/api/v1/futures/wallet/withdraw/addresses", s.handleWithdrawAddresses)
	r.GET("/api/v1/futures/wallet/balance", s.handleWalletBalance)
	r.GET("/api/v1/futures/wallet/fee", s.handleWalletFee)
	r.GET("/api/v1/futures/wallet/baddebt", s.handleBadDebt)
	r.POST("/api/v1/futures/wallet/baddebt/repay", s.handleBadDebtRepay)
	r.POST("/api/v1/futures/wallet/baddebt/socialize/propose", s.handleSocializePropose)
	r.POST("/api/v1/futures/wallet/baddebt/socialize/approve", s.handleSocializeApprove)
	r.GET("/api/v1/futures/wallet/reconcile", s.handleReconcile)
	r.GET("/api/v1/futures/wallet/snapshot", s.handleSnapshot)
	r.POST("/api/v1/futures/wallet/snapshot/save", s.handleSnapshotSave)
	r.GET("/api/v1/futures/wallet/inventory", s.handleInventory)
	r.POST("/api/v1/futures/wallet/sweep", s.handleSweep)
	r.POST("/api/v1/futures/wallet/unsweep", s.handleUnsweep)
	r.GET("/metrics", s.handleMetrics)
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
	if err := s.ledgerSvc.Deposit(req.UserID, "USDT", settlement.AssetAmountFromFloat(req.Amount, settlement.AssetDecimalsByName("USDT")), "deposit"); err != nil {
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
	ev, err := s.chainGateway.SubmitDeposit(req.UserID, req.Asset, settlement.Chain(req.Chain),
		settlement.AssetAmountFromFloat(req.Amount, settlement.AssetDecimals(settlement.Chain(req.Chain), req.Asset)), req.Address)
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
	amt := settlement.AssetAmountFromFloat(req.Amount, dec)
	var feeAmt settlement.AssetAmount
	if req.Fee > 0 {
		feeAmt = settlement.AssetAmountFromFloat(req.Fee, dec)
	} else {
		feeAmt = s.feeModel.Estimate(settlement.Chain(req.Chain), asset, amt)
	}
	// 冻结额由与链上广播同源的 AssetAmount 派生，确保账本冻结额 == 链上划出额，消除 float 漂移（F2）。
	total := amt.Add(feeAmt)
	if s.ledgerSvc.IsOutflowRestricted(req.UserID, asset) {
		response.Error(c, 403, 403, "outflow restricted: repay outstanding bad debt first")
		return
	}
	avail, _, ok := s.ledgerSvc.Balance(req.UserID, asset)
	if !ok || avail.Cmp(total) < 0 {
		response.Error(c, 400, 400, "insufficient available balance")
		return
	}
	if err := s.ledgerSvc.FreezeWithdraw(req.UserID, asset, total); err != nil {
		response.Error(c, 500, 500, err.Error())
		return
	}
	ev, err := s.chainWithdraw.SubmitWithdraw(req.UserID, asset, settlement.Chain(req.Chain),
		amt, feeAmt, req.Address, req.WillFail)
	if err != nil {
		_ = s.ledgerSvc.UnfreezeWithdraw(req.UserID, asset, total) // 受理失败回退提现冻结
		response.Error(c, 400, 400, err.Error())
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
func (s *Server) handleWithdrawRequest(c *gin.Context) {
	var req struct {
		UserID  int64   `json:"user_id"`
		Asset   string  `json:"asset"`
		Chain   string  `json:"chain"`
		Amount  float64 `json:"amount"`
		Fee     float64 `json:"fee"`
		Address string  `json:"address"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Amount <= 0 || req.UserID == 0 {
		response.Error(c, 400, 400, "bad request")
		return
	}
	asset := req.Asset
	if asset == "" {
		asset = "USDT"
	}
	dec := settlement.AssetDecimals(settlement.Chain(req.Chain), asset)
	amt := settlement.AssetAmountFromFloat(req.Amount, dec)
	var feeAmt settlement.AssetAmount
	if req.Fee > 0 {
		feeAmt = settlement.AssetAmountFromFloat(req.Fee, dec)
	} else {
		feeAmt = s.feeModel.Estimate(settlement.Chain(req.Chain), asset, amt)
	}
	// 冻结额由与链上广播同源的 AssetAmount 派生，确保账本冻结额 == 链上划出额，消除 float 漂移（F2）。
	total := amt.Add(feeAmt)
	avail, _, ok := s.ledgerSvc.Balance(req.UserID, asset)
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
	id, holdUntil, err := s.ledgerSvc.RequestWithdrawHold(req.UserID, asset, amt, feeAmt, req.Chain, req.Address)
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

// handleWithdrawAddressAdd 预登记一条出金地址（默认未验证）。
func (s *Server) handleWithdrawAddressAdd(c *gin.Context) {
	var req struct {
		UserID  int64  `json:"user_id"`
		Asset   string `json:"asset"`
		Chain   string `json:"chain"`
		Address string `json:"address"`
		Label   string `json:"label"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID <= 0 || req.Address == "" {
		response.Error(c, 400, 400, "bad request: user_id and address required")
		return
	}
	if req.Asset == "" {
		req.Asset = "USDT"
	}
	addr, err := s.ledgerSvc.AddWithdrawAddress(req.UserID, req.Asset, req.Chain, req.Address, req.Label)
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

// handleWithdrawAddressConfirm 验证一条预登记地址（模拟 2FA/邮件验证通过）。
func (s *Server) handleWithdrawAddressConfirm(c *gin.Context) {
	var req struct {
		UserID  int64  `json:"user_id"`
		Asset   string `json:"asset"`
		Chain   string `json:"chain"`
		Address string `json:"address"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID <= 0 || req.Address == "" {
		response.Error(c, 400, 400, "bad request: user_id and address required")
		return
	}
	if req.Asset == "" {
		req.Asset = "USDT"
	}
	if err := s.ledgerSvc.ConfirmWithdrawAddress(req.UserID, req.Asset, req.Chain, req.Address); err != nil {
		response.Error(c, 404, 404, err.Error())
		return
	}
	response.JSON(c, gin.H{"status": "verified", "user_id": req.UserID, "asset": req.Asset, "chain": req.Chain, "address": req.Address})
}

// handleWithdrawAddressDelete 撤销一条已登记地址。
func (s *Server) handleWithdrawAddressDelete(c *gin.Context) {
	var req struct {
		UserID  int64  `json:"user_id"`
		Asset   string `json:"asset"`
		Chain   string `json:"chain"`
		Address string `json:"address"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID <= 0 || req.Address == "" {
		response.Error(c, 400, 400, "bad request: user_id and address required")
		return
	}
	if req.Asset == "" {
		req.Asset = "USDT"
	}
	if err := s.ledgerSvc.RemoveWithdrawAddress(req.UserID, req.Asset, req.Chain, req.Address); err != nil {
		response.Error(c, 404, 404, err.Error())
		return
	}
	response.JSON(c, gin.H{"status": "removed", "user_id": req.UserID, "asset": req.Asset, "chain": req.Chain, "address": req.Address})
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
	est := s.feeModel.Estimate(settlement.Chain(chain), asset,
		settlement.AssetAmountFromFloat(amount, settlement.AssetDecimals(settlement.Chain(chain), asset)))
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

// handleBadDebtRepay 坏账补缴：用户主动用可用余额冲抵交易所垫付的坏账。
func (s *Server) handleBadDebtRepay(c *gin.Context) {
	var req struct {
		UserID int64   `json:"user_id"`
		Asset  string  `json:"asset"`
		Amount float64 `json:"amount"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.UserID <= 0 || req.Amount <= 0 {
		response.Error(c, 400, 400, "bad request")
		return
	}
	if req.Asset == "" {
		req.Asset = "USDT"
	}
	ref := fmt.Sprintf("repay:%d", req.UserID)
	if err := s.ledgerSvc.RepayBadDebt(req.UserID, req.Asset, settlement.AssetAmountFromFloat(req.Amount, settlement.AssetDecimalsByName(req.Asset)), ref); err != nil {
		response.Error(c, 400, 400, err.Error())
		return
	}
	response.JSON(c, gin.H{
		"status":          "repaid",
		"user_id":         req.UserID,
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
	amount := settlement.AssetAmountFromFloat(req.Amount, settlement.AssetDecimalsByName(req.Asset))
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
	amount := settlement.AssetAmountFromFloat(req.Amount, settlement.AssetDecimalsByName(req.Asset))
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

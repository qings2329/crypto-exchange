package futuresapi

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/pkg/middleware"
	"github.com/coldlar/crypto-exchange/internal/pkg/response"
	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// AddrBookEntry 交易白名单（地址簿）条目：用户维护的可信提现/转账地址。
// 契约对齐 mock 网关 /api/v1/futures/wallet/address-book（前端钱包白名单功能）。
type AddrBookEntry struct {
	ID      int64  `json:"id"`
	UserID  int64  `json:"user_id"`
	Asset   string `json:"asset"`
	Network string `json:"network"`
	Address string `json:"address"`
	Label   string `json:"label"`
	AddedAt int64  `json:"added_at"`
}

// TPState 持仓止盈止损配置。
type TPState struct {
	TP *float64 `json:"TP,omitempty"`
	SL *float64 `json:"SL,omitempty"`
}

// tpslKey 生成 (uid|symbol|side) 维一键。
func tpslKey(uid int64, symbol, side string) string {
	return fmt.Sprintf("%d|%s|%s", uid, symbol, side)
}

// registerGapRoutes 注册本轮补齐的端点（地址簿/内部划转/TP-SL），契约对齐 mock 网关。
func (s *Server) registerGapRoutes(r *gin.Engine) {
	r.GET("/api/v1/futures/wallet/address-book", s.handleAddrBookList)
	r.POST("/api/v1/futures/wallet/address-book", s.handleAddrBookAdd)
	r.DELETE("/api/v1/futures/wallet/address-book/:id", s.handleAddrBookDelete)
	r.POST("/api/v1/futures/wallet/transfer", s.handleTransfer)
	r.PUT("/api/v1/futures/tpsl", s.handleSetTPSL)
}

// ---- 地址簿（交易白名单）----

func (s *Server) handleAddrBookList(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	s.addrBookMu.Lock()
	list := append([]AddrBookEntry(nil), s.addrBook[uid]...)
	s.addrBookMu.Unlock()
	for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
		list[i], list[j] = list[j], list[i]
	}
	response.JSON(c, gin.H{
		"entries":          list,
		"whitelist_active": len(list) > 0,
	})
}

func (s *Server) handleAddrBookAdd(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	var b struct {
		Asset   string `json:"asset"`
		Network string `json:"network"`
		Address string `json:"address"`
		Label   string `json:"label"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		response.Error(c, 400, 400, "bad request")
		return
	}
	asset := normalizeAsset(b.Asset)
	address := strings.TrimSpace(b.Address)
	label := strings.TrimSpace(b.Label)
	if asset == "" {
		response.Error(c, 400, 400, "资产必填")
		return
	}
	if !isValidCryptoAddress(address) {
		response.Error(c, 400, 400, "地址格式不正确")
		return
	}
	s.addrBookMu.Lock()
	book := s.addrBook[uid]
	for _, x := range book {
		if strings.EqualFold(x.Address, address) {
			s.addrBookMu.Unlock()
			response.Error(c, 409, 409, "该地址已存在")
			return
		}
	}
	id := atomic.AddInt64(&s.addrSeq, 1)
	entry := AddrBookEntry{
		ID:      id,
		UserID:  uid,
		Asset:   asset,
		Network: strings.TrimSpace(b.Network),
		Address: address,
		Label:   orDefault(label, "未命名"),
		AddedAt: time.Now().Unix(),
	}
	s.addrBook[uid] = append(book, entry)
	s.addrBookMu.Unlock()
	response.JSON(c, entry)
}

func (s *Server) handleAddrBookDelete(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	id := parseUserID(c.Param("id"))
	if id <= 0 {
		response.Error(c, 400, 400, "invalid id")
		return
	}
	s.addrBookMu.Lock()
	book := s.addrBook[uid]
	idx := -1
	for i, x := range book {
		if x.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.addrBookMu.Unlock()
		response.Error(c, 404, 404, "地址不存在")
		return
	}
	s.addrBook[uid] = append(book[:idx], book[idx+1:]...)
	s.addrBookMu.Unlock()
	response.JSON(c, gin.H{"ok": true})
}

// ---- 内部划转：资金账户(可用) ⇄ 合约保证金(冻结) ----

func (s *Server) handleTransfer(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	var b struct {
		Asset     string  `json:"asset"`
		Amount    float64 `json:"amount"`
		Direction string  `json:"direction"` // to_futures(可用->保证金) / to_funding(保证金->可用)
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		response.Error(c, 400, 400, "bad request")
		return
	}
	asset := normalizeAsset(b.Asset)
	amount := b.Amount
	if !supportedTransferAsset(asset) {
		response.Error(c, 400, 400, "不支持的划转资产")
		return
	}
	if !(amount > 0) {
		response.Error(c, 400, 400, "划转金额必须大于 0")
		return
	}
	if b.Direction != "to_futures" && b.Direction != "to_funding" {
		response.Error(c, 400, 400, "direction 必须为 to_futures/to_funding")
		return
	}

	amt := settlement.AssetAmountFromFloat(amount, settlement.AssetDecimalsByName(asset))

	s.marginMu.Lock()
	ma, okMA := s.marginAcct[uid]
	if !okMA {
		ma = make(map[string]float64)
		s.marginAcct[uid] = ma
	}
	marginBal := ma[asset]
	s.marginMu.Unlock()

	switch b.Direction {
	case "to_futures":
		avail, _, okB := s.ledgerSvc.Balance(uid, asset)
		if !okB || avail.HumanFloat() < amount {
			response.Error(c, 400, 400, "可用余额不足")
			return
		}
		if err := s.ledgerSvc.DebitAvailable(uid, asset, amt, "transfer", "to_futures"); err != nil {
			response.Error(c, 400, 400, "扣减可用失败: "+err.Error())
			return
		}
		s.marginMu.Lock()
		s.marginAcct[uid][asset] = marginBal + amount
		s.marginMu.Unlock()
	case "to_funding":
		if marginBal < amount {
			response.Error(c, 400, 400, "合约保证金不足")
			return
		}
		if err := s.ledgerSvc.CreditAvailable(uid, asset, amt, "transfer", "to_funding"); err != nil {
			response.Error(c, 400, 400, "解冻可用失败: "+err.Error())
			return
		}
		s.marginMu.Lock()
		s.marginAcct[uid][asset] = marginBal - amount
		s.marginMu.Unlock()
	}

	s.marginMu.Lock()
	newMargin := s.marginAcct[uid][asset]
	s.marginMu.Unlock()
	avail, _, _ := s.ledgerSvc.Balance(uid, asset)
	response.JSON(c, gin.H{
		"asset":     asset,
		"available": avail.HumanFloat(),
		"frozen":    newMargin,
	})
}

// ---- 持仓止盈止损 (TP/SL) ----

func (s *Server) handleSetTPSL(c *gin.Context) {
	uid, ok := middleware.UserID(c)
	if !ok {
		response.Error(c, 401, 401, "unauthorized")
		return
	}
	var b struct {
		Symbol  string   `json:"symbol"`
		PosSide string   `json:"pos_side"` // long / short
		TP      *float64 `json:"tp"`
		SL      *float64 `json:"sl"`
	}
	if err := c.ShouldBindJSON(&b); err != nil {
		response.Error(c, 400, 400, "bad request")
		return
	}
	if b.PosSide != "long" && b.PosSide != "short" {
		response.Error(c, 400, 400, "pos_side 必须为 long/short")
		return
	}
	if b.TP != nil && !(*b.TP > 0) {
		response.Error(c, 400, 400, "tp 非法")
		return
	}
	if b.SL != nil && !(*b.SL > 0) {
		response.Error(c, 400, 400, "sl 非法")
		return
	}
	if b.TP == nil && b.SL == nil {
		response.Error(c, 400, 400, "tp/sl 至少填一项")
		return
	}
	positions := s.liquidator.AllPositions(b.Symbol)
	found := false
	for _, p := range positions {
		if p.UserID == uid && sideName(p.Side) == b.PosSide {
			found = true
			break
		}
	}
	if !found {
		response.Error(c, 404, 404, "position not found")
		return
	}
	key := tpslKey(uid, b.Symbol, b.PosSide)
	s.tpslMu.Lock()
	if s.tpsl[uid] == nil {
		s.tpsl[uid] = make(map[string]TPState)
	}
	s.tpsl[uid][key] = TPState{TP: b.TP, SL: b.SL}
	s.tpslMu.Unlock()
	response.JSON(c, gin.H{"symbol": b.Symbol, "pos_side": b.PosSide, "tp": b.TP, "sl": b.SL})
}

// ---- 辅助 ----

func normalizeAsset(a string) string {
	return strings.ToUpper(strings.TrimSpace(a))
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func supportedTransferAsset(a string) bool {
	switch a {
	case "USDT", "USDC", "BTC", "ETH", "BNB":
		return true
	}
	return false
}

// isValidCryptoAddress 轻量地址校验：非空、长度与字符集合理（非严格链上校验）。
func isValidCryptoAddress(addr string) bool {
	if len(addr) < 8 || len(addr) > 90 {
		return false
	}
	for _, r := range addr {
		if !(isAlphaNum(r) || r == '_') {
			return false
		}
	}
	return true
}

func isAlphaNum(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

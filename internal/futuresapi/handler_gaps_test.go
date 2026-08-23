package futuresapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/coldlar/crypto-exchange/internal/futures"
)

// authed 构造带登录身份的测试上下文（middleware.UserID 读取 c.Get("user_id")），返回 (recorder, ctx)。
func authed(method, path string, body interface{}) (*httptest.ResponseRecorder, *gin.Context) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", int64(1))
	var r *bytes.Reader
	if body != nil {
		bb, _ := json.Marshal(body)
		r = bytes.NewReader(bb)
	} else {
		r = bytes.NewReader(nil)
	}
	c.Request, _ = http.NewRequest(method, path, r)
	c.Request.Header.Set("Content-Type", "application/json")
	return w, c
}

func itoa(i int64) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	buf := make([]byte, 0, 20)
	for i > 0 {
		buf = append([]byte{byte('0' + i%10)}, buf...)
		i /= 10
	}
	if neg {
		buf = append([]byte{'-'}, buf...)
	}
	return string(buf)
}

// newGapServer 构造最小 futures Server 并初始化本轮补齐端点的内存存储。
func newGapServer(t *testing.T) *Server {
	t.Helper()
	s := newWithdrawServer(t)
	s.addrBook = make(map[int64][]AddrBookEntry)
	s.tpsl = make(map[int64]map[string]TPState)
	s.marginAcct = make(map[int64]map[string]float64)
	return s
}

func TestAddressBookCRUD(t *testing.T) {
	s := newGapServer(t)

	w, c := authed(http.MethodPost, "/api/v1/futures/wallet/address-book", gin.H{
		"asset": "USDT", "network": "ERC20", "address": "0xAbC1234567890aBcDeF1234567890aBcDeF123456", "label": "冷钱包",
	})
	s.handleAddrBookAdd(c)
	if w.Code != http.StatusOK {
		t.Fatalf("add want 200 got %d %s", w.Code, w.Body.String())
	}
	var entry AddrBookEntry
	unmarshalData(t, w, &entry)
	if entry.ID <= 0 || entry.Asset != "USDT" || entry.Label != "冷钱包" {
		t.Fatalf("unexpected entry %+v", entry)
	}

	// 重复地址应 409
	w, c = authed(http.MethodPost, "/api/v1/futures/wallet/address-book", gin.H{
		"asset": "USDT", "address": "0xAbC1234567890aBcDeF1234567890aBcDeF123456",
	})
	s.handleAddrBookAdd(c)
	if w.Code != http.StatusConflict {
		t.Fatalf("dup want 409 got %d", w.Code)
	}

	w, c = authed(http.MethodGet, "/api/v1/futures/wallet/address-book", nil)
	s.handleAddrBookList(c)
	if w.Code != http.StatusOK {
		t.Fatalf("list want 200 got %d", w.Code)
	}
	var listResp struct {
		Entries         []AddrBookEntry `json:"entries"`
		WhitelistActive bool             `json:"whitelist_active"`
	}
	unmarshalData(t, w, &listResp)
	if len(listResp.Entries) != 1 || !listResp.WhitelistActive {
		t.Fatalf("list unexpected %+v", listResp)
	}

	w, c = authed(http.MethodDelete, "/api/v1/futures/wallet/address-book/"+itoa(entry.ID), nil)
	c.Params = gin.Params{{Key: "id", Value: itoa(entry.ID)}}
	s.handleAddrBookDelete(c)
	if w.Code != http.StatusOK {
		t.Fatalf("del want 200 got %d %s", w.Code, w.Body.String())
	}
	w, c = authed(http.MethodGet, "/api/v1/futures/wallet/address-book", nil)
	s.handleAddrBookList(c)
	unmarshalData(t, w, &listResp)
	if len(listResp.Entries) != 0 {
		t.Fatalf("after del want empty, got %+v", listResp)
	}
}

func TestTransfer(t *testing.T) {
	s := newGapServer(t) // uid=1 充值 10000 USDT

	w, c := authed(http.MethodPost, "/api/v1/futures/wallet/transfer", gin.H{
		"asset": "USDT", "amount": 500, "direction": "to_futures",
	})
	s.handleTransfer(c)
	if w.Code != http.StatusOK {
		t.Fatalf("to_futures want 200 got %d %s", w.Code, w.Body.String())
	}
	var tr struct {
		Asset     string  `json:"asset"`
		Available float64 `json:"available"`
		Frozen    float64 `json:"frozen"`
	}
	unmarshalData(t, w, &tr)
	if tr.Available != 9500 || tr.Frozen != 500 {
		t.Fatalf("after to_futures unexpected %+v", tr)
	}

	w, c = authed(http.MethodPost, "/api/v1/futures/wallet/transfer", gin.H{
		"asset": "USDT", "amount": 200, "direction": "to_funding",
	})
	s.handleTransfer(c)
	unmarshalData(t, w, &tr)
	if tr.Available != 9700 || tr.Frozen != 300 {
		t.Fatalf("after to_funding unexpected %+v", tr)
	}

	w, c = authed(http.MethodPost, "/api/v1/futures/wallet/transfer", gin.H{
		"asset": "USDT", "amount": 9999, "direction": "to_funding",
	})
	s.handleTransfer(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("insufficient margin want 400 got %d", w.Code)
	}
}

func TestSetTPSLValidation(t *testing.T) {
	s := newGapServer(t)
	s.liquidator = futures.NewLiquidator(func(ev futures.LiquidationEvent) {})

	// 持仓不存在 -> 404
	w, c := authed(http.MethodPut, "/api/v1/futures/tpsl", gin.H{
		"symbol": "BTC_USDT_PERP", "pos_side": "long", "tp": 70000, "sl": 60000,
	})
	s.handleSetTPSL(c)
	if w.Code != http.StatusNotFound {
		t.Fatalf("no position want 404 got %d %s", w.Code, w.Body.String())
	}

	// 入参非法：pos_side
	w, c = authed(http.MethodPut, "/api/v1/futures/tpsl", gin.H{
		"symbol": "BTC_USDT_PERP", "pos_side": "sideways", "tp": 70000,
	})
	s.handleSetTPSL(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad pos_side want 400 got %d", w.Code)
	}

	// tp/sl 均未提供
	w, c = authed(http.MethodPut, "/api/v1/futures/tpsl", gin.H{
		"symbol": "BTC_USDT_PERP", "pos_side": "long",
	})
	s.handleSetTPSL(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("empty tp/sl want 400 got %d", w.Code)
	}
}

// unmarshalData 解包信封并把 data 反序列化到 v。
func unmarshalData(t *testing.T, w *httptest.ResponseRecorder, v interface{}) {
	t.Helper()
	if err := json.Unmarshal(decodeData(t, w), v); err != nil {
		t.Fatalf("unmarshal data: %v (body=%s)", err, w.Body.String())
	}
}

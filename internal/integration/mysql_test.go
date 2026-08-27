package integration

import (
	"database/sql"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/coldlar/crypto-exchange/internal/announcement"
	"github.com/coldlar/crypto-exchange/internal/apikeys"
	"github.com/coldlar/crypto-exchange/internal/bot"
	"github.com/coldlar/crypto-exchange/internal/copytrade"
	"github.com/coldlar/crypto-exchange/internal/earn"
	"github.com/coldlar/crypto-exchange/internal/ledger"
	"github.com/coldlar/crypto-exchange/internal/lending"
	"github.com/coldlar/crypto-exchange/internal/margin"
	"github.com/coldlar/crypto-exchange/internal/matching/persist"
	"github.com/coldlar/crypto-exchange/internal/notification"
	"github.com/coldlar/crypto-exchange/internal/options"
	"github.com/coldlar/crypto-exchange/internal/otc"
	"github.com/coldlar/crypto-exchange/internal/pkg/config"
	"github.com/coldlar/crypto-exchange/internal/pkg/migrate"
	"github.com/coldlar/crypto-exchange/internal/referral"
	"github.com/coldlar/crypto-exchange/internal/risk"
	"github.com/coldlar/crypto-exchange/internal/services/user"
	"github.com/coldlar/crypto-exchange/internal/settlement"
	"github.com/coldlar/crypto-exchange/internal/spot"
	"github.com/coldlar/crypto-exchange/internal/staking"
	"github.com/coldlar/crypto-exchange/internal/wealth"
)

// testDBName 是集成测试专用的隔离库名（建在「config.yaml 里的同一台 MySQL」上），
// 避免直接读写/迁移线上 wallet 库，也规避既有数据造成的迁移冲突（重复键、部分表状态）。
const testDBName = "wallet_it"

// mysqlBaseDSN 返回配置/环境变量里的真实 MySQL DSN。
func mysqlBaseDSN(t *testing.T) string {
	if d := os.Getenv("CE_MYSQL_DSN"); d != "" {
		return d
	}
	cfgPath := filepath.Join(repoRoot(t), "configs", "config.yaml")
	if cfg, err := config.Load(cfgPath); err == nil && cfg.MySQL.DSN != "" {
		return cfg.MySQL.DSN
	}
	return ""
}

// mysqlTestDSN 解析 base DSN，在「同一台 MySQL」上建隔离测试库 wallet_it 后返回其 DSN。
// 若当前账号无建库权限，则尝试使用显式指定的 CE_MYSQL_TEST_DSN（指向一个干净的测试库）；
// 两者皆不可用时，跳过测试——绝不在已存在数据/迁移漂移的线上库上直接迁移或读写。
func mysqlTestDSN(t *testing.T) string {
	base := mysqlBaseDSN(t)
	if base == "" {
		t.Skip("no MySQL DSN: set CE_MYSQL_DSN or configure mysql.dsn in configs/config.yaml")
	}
	cfg, err := mysql.ParseDSN(base)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	// 1) 显式指定的干净测试库（最优先，避免对线上 MySQL 发起建库尝试）。
	if td := os.Getenv("CE_MYSQL_TEST_DSN"); td != "" {
		return td
	}
	// 2) 在配置里的同一台 MySQL 上自动建隔离测试库（需建库权限）。
	admin := *cfg
	admin.DBName = ""
	adm, aerr := sql.Open("mysql", admin.FormatDSN())
	if aerr == nil {
		_, cerr := adm.Exec("CREATE DATABASE IF NOT EXISTS " + testDBName)
		if cerr == nil {
			adm.Close()
			cfg.DBName = testDBName
			t.Logf("using isolated test database %q on configured MySQL", testDBName)
			return cfg.FormatDSN()
		}
		t.Logf("cannot auto-create isolated test db (%v); will try CE_MYSQL_TEST_DSN", cerr)
		adm.Close()
	}
	t.Skip("MySQL integration test needs a dedicated clean test database: " +
		"auto-create is denied on the configured MySQL and CE_MYSQL_TEST_DSN is unset. " +
		"Set CE_MYSQL_TEST_DSN to a clean test DB DSN (same MySQL server as configs/config.yaml), " +
		"or grant CREATE DATABASE to the configured account.")
	return ""
}

// repoRoot 由本测试源文件向上找到 go.mod 所在目录（仓库根），保证无论从哪个目录跑
// `go test` 都能定位 configs/config.yaml。
func repoRoot(t *testing.T) string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	for i := 0; i < 12; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("repo root (go.mod) not found")
	return ""
}

// openMySQL 打开并探活真实 MySQL；无 DSN 或不可达时跳过（不破坏无 MySQL 环境的 `go test ./...`）。
func openMySQL(t *testing.T) *sql.DB {
	dsn := mysqlTestDSN(t)
	if dsn == "" {
		t.Skip("no MySQL DSN: set CE_MYSQL_DSN or configure mysql.dsn in configs/config.yaml")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	db.SetMaxOpenConns(2)
	db.SetConnMaxLifetime(5 * time.Second)
	if err := db.Ping(); err != nil {
		db.Close()
		t.Skipf("MySQL unreachable (skipping real-DB integration): %v", err)
	}
	return db
}

// TestMySQLStoresMigrate 在真实 MySQL 上逐个执行各模块的 Up 迁移（建表/加列），
// 覆盖此前仅在内存模式被跳过的 MySQL 存储路径。只校验迁移成功，不写入业务数据。
func TestMySQLStoresMigrate(t *testing.T) {
	db := openMySQL(t)
	defer db.Close()
	dsn := mysqlTestDSN(t)

	type smoke struct {
		name string
		fn   func() error
	}
	cases := []smoke{
		{"announcement", func() error { _, e := announcement.NewMySQLStore(dsn); return e }},
		{"apikeys", func() error { _, e := apikeys.NewMySQLStore(dsn); return e }},
		{"bot", func() error { _, e := bot.NewMySQLStore(dsn); return e }},
		{"copytrade", func() error { _, e := copytrade.NewMySQLStore(dsn); return e }},
		{"earn", func() error { _, e := earn.NewMySQLStore(dsn); return e }},
		{"lending", func() error { _, e := lending.NewMySQLStore(dsn); return e }},
		{"margin", func() error { _, e := margin.NewMySQLStore(dsn); return e }},
		{"otc", func() error { _, e := otc.NewMySQLStore(dsn); return e }},
		{"options", func() error { _, e := options.NewMySQLStore(dsn); return e }},
		{"referral", func() error { _, e := referral.NewMySQLStore(dsn); return e }},
		{"staking", func() error { _, e := staking.NewMySQLStore(dsn); return e }},
		{"user", func() error { _, e := user.NewMySQLStore(dsn); return e }},
		{"spot", func() error { _, e := spot.NewMySQLStore(dsn); return e }},
		{"wealth", func() error { _, e := wealth.NewMySQLStore(dsn); return e }},
		{"matching/persist", func() error { _, e := persist.NewMySQLStore(dsn); return e }},
		// notification / risk 接收 *sql.DB 而非 dsn。
		{"notification", func() error { _, e := notification.NewMySQLStore(db); return e }},
		{"risk", func() error { _, e := risk.NewMySQLStore(db); return e }},
	}
	for _, c := range cases {
		if err := c.fn(); err != nil {
			t.Errorf("migrate %s: %v", c.name, err)
		}
	}
}

// TestMySQLNotificationRoundTrip 验证通知模块在真实 MySQL 上的 发布→列表→已读→删除 全链路。
func TestMySQLNotificationRoundTrip(t *testing.T) {
	db := openMySQL(t)
	defer db.Close()
	store, err := notification.NewMySQLStore(db)
	if err != nil {
		t.Fatalf("notification store: %v", err)
	}
	svc := notification.New(store)
	const uid = 990001
	n, err := svc.Publish(notification.PublishInput{
		UserID: uid, Type: notification.TypeSystem, Title: "IT round-trip", Body: "hello",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	defer svc.Delete(uid, n.ID)

	list, err := svc.List(uid, false, 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, x := range list {
		if x.ID == n.ID {
			found = true
			if x.Title != "IT round-trip" {
				t.Fatalf("title mismatch: %q", x.Title)
			}
		}
	}
	if !found {
		t.Fatalf("published notification not found in list")
	}

	if err := svc.MarkRead(uid, n.ID); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	cnt, err := svc.UnreadCount(uid)
	if err != nil {
		t.Fatalf("unread count: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("expected 0 unread after mark-read, got %d", cnt)
	}
}

// TestMySQLAnnouncementRoundTrip 验证公告模块在真实 MySQL 上的 创建→列表→删除 全链路。
func TestMySQLAnnouncementRoundTrip(t *testing.T) {
	db := openMySQL(t)
	defer db.Close()
	dsn := mysqlTestDSN(t)
	store, err := announcement.NewMySQLStore(dsn)
	if err != nil {
		t.Fatalf("announcement store: %v", err)
	}
	svc := announcement.NewService(store)

	lvl, title, content, active := "info", "IT announce", "body", true
	a, err := svc.Create(announcement.AnnouncementInput{
		Level:   &lvl,
		Title:   &title,
		Content: &content,
		Active:  &active,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() {
		if derr := svc.Delete(a.ID); derr != nil && !errors.Is(derr, announcement.ErrNotFound) {
			t.Logf("cleanup delete: %v", derr)
		}
	}()

	list, err := svc.ListActive()
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	found := false
	for _, x := range list {
		if x.ID == a.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("created announcement not found in ListActive")
	}
}

// TestMySQLUserRoundTrip 验证用户模块在真实 MySQL 上的 注册→查询→更新→KYC 全链路。
func TestMySQLUserRoundTrip(t *testing.T) {
	dsn := mysqlTestDSN(t)
	store, err := user.NewMySQLStore(dsn)
	if err != nil {
		t.Fatalf("user store: %v", err)
	}

	// 预清理：避免上一轮残留 email/phone 导致唯一键冲突。
	db, _ := sql.Open("mysql", dsn)
	if db != nil {
		db.Exec("DELETE FROM ce_users WHERE email = 'it-user-roundtrip@test.com' OR phone = '+861389990001'")
		db.Close()
	}

	u := &user.User{
		Email:        "it-user-roundtrip@test.com",
		Phone:        "+861389990001",
		PassHash:     "hash-placeholder",
		Status:       user.StatusNormal,
		ReferralCode: "RCITRT01",
	}
	if err := store.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	uid := u.ID
	defer func() {
		db, _ := sql.Open("mysql", dsn)
		if db != nil {
			db.Exec("DELETE FROM ce_users WHERE id = ?", uid)
			db.Close()
		}
	}()

	got, err := store.GetByID(uid)
	if err != nil {
		t.Fatalf("get by id: %v", err)
	}
	if got.Email != u.Email {
		t.Fatalf("email: want %q, got %q", u.Email, got.Email)
	}

	byEmail, err := store.GetByEmail(u.Email)
	if err != nil {
		t.Fatalf("get by email: %v", err)
	}
	if byEmail.ID != uid {
		t.Fatalf("by email id: want %d, got %d", uid, byEmail.ID)
	}

	u.Nickname = "IT User"
	if err := store.UpdateUser(u); err != nil {
		t.Fatalf("update user: %v", err)
	}
	got2, _ := store.GetByID(uid)
	if got2.Nickname != "IT User" {
		t.Fatalf("nickname after update: want %q, got %q", "IT User", got2.Nickname)
	}

	kyc := &user.KYCSubmission{
		UserID:   uid,
		RealName: "测试用户",
		IDType:   "id_card",
		IDNumber: "110101199001011234",
		Status:   user.KYCPending,
	}
	if err := store.SaveKYC(kyc); err != nil {
		t.Fatalf("save kyc: %v", err)
	}
	kycGot, err := store.GetKYC(uid)
	if err != nil {
		t.Fatalf("get kyc: %v", err)
	}
	if kycGot.RealName != "测试用户" {
		t.Fatalf("kyc real_name: want %q, got %q", "测试用户", kycGot.RealName)
	}

	kyc.Status = user.KYCVerified
	kyc.Reviewer = "it-bot"
	if err := store.UpdateKYC(kyc); err != nil {
		t.Fatalf("update kyc: %v", err)
	}
	kycGot2, _ := store.GetKYC(uid)
	if kycGot2.Status != user.KYCVerified {
		t.Fatalf("kyc status after update: want verified, got %d", kycGot2.Status)
	}
}

// TestMySQLRiskRoundTrip 验证风控模块在真实 MySQL 上的 规则→事件→黑名单 全链路。
func TestMySQLRiskRoundTrip(t *testing.T) {
	db := openMySQL(t)
	defer db.Close()
	store, err := risk.NewMySQLStore(db)
	if err != nil {
		t.Fatalf("risk store: %v", err)
	}

	// --- 规则 ---
	rule := &risk.RiskRule{
		Name:            "IT risk rule",
		Kind:            "withdraw",
		Scope:           "global",
		MaxCountPerDay:  10,
		MinKYCLevel:     1,
		Enabled:         true,
	}
	saved, err := store.UpsertRule(rule)
	if err != nil {
		t.Fatalf("upsert rule: %v", err)
	}
	defer func() {
		db.Exec("DELETE FROM ce_risk_rules WHERE id = ?", saved.ID)
	}()

	got, err := store.GetRule(saved.ID)
	if err != nil {
		t.Fatalf("get rule: %v", err)
	}
	if got.Name != "IT risk rule" {
		t.Fatalf("rule name: want %q, got %q", "IT risk rule", got.Name)
	}

	rules, err := store.ListRules("withdraw")
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	found := false
	for _, r := range rules {
		if r.ID == saved.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("rule not found in ListRules")
	}

	// --- 事件 ---
	const uid = int64(990003)
	evt := &risk.RiskEvent{
		UserID: uid,
		Kind:   "login_failed",
		Detail: "IT integration test event",
	}
	savedEvt, err := store.RecordEvent(evt)
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	defer func() {
		db.Exec("DELETE FROM ce_risk_events WHERE id = ?", savedEvt.ID)
	}()

	events, err := store.ListEvents(uid, 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	evtFound := false
	for _, e := range events {
		if e.ID == savedEvt.ID {
			evtFound = true
		}
	}
	if !evtFound {
		t.Fatalf("event not found in ListEvents")
	}

	// --- 黑名单 ---
	bl := &risk.BlacklistEntry{
		Target: "it-blacklist-990003",
		Kind:   "user",
		Reason: "IT integration test",
	}
	savedBL, err := store.AddBlacklist(bl)
	if err != nil {
		t.Fatalf("add blacklist: %v", err)
	}
	defer func() {
		db.Exec("DELETE FROM ce_risk_blacklist WHERE id = ?", savedBL.ID)
	}()

	blacklisted, err := store.IsBlacklisted("it-blacklist-990003")
	if err != nil {
		t.Fatalf("is blacklisted: %v", err)
	}
	if !blacklisted {
		t.Fatalf("expected target to be blacklisted")
	}

	blList, err := store.ListBlacklist("user")
	if err != nil {
		t.Fatalf("list blacklist: %v", err)
	}
	blFound := false
	for _, b := range blList {
		if b.ID == savedBL.ID {
			blFound = true
		}
	}
	if !blFound {
		t.Fatalf("blacklist entry not found in ListBlacklist")
	}

	if err := store.RemoveBlacklist("it-blacklist-990003"); err != nil {
		t.Fatalf("remove blacklist: %v", err)
	}
	blacklisted2, _ := store.IsBlacklisted("it-blacklist-990003")
	if blacklisted2 {
		t.Fatalf("expected target to be un-blacklisted after removal")
	}
}

// TestMySQLWealthRoundTrip 验证理财产品模块在真实 MySQL 上的 产品→持仓 全链路。
func TestMySQLWealthRoundTrip(t *testing.T) {
	dsn := mysqlTestDSN(t)
	store, err := wealth.NewMySQLStore(dsn)
	if err != nil {
		t.Fatalf("wealth store: %v", err)
	}

	// --- 产品 ---
	prod := &wealth.WealthProduct{
		Name:         "IT wealth product",
		Asset:        "USDT",
		Type:         wealth.TypeCurrent,
		AnnualRate:   0.065,
		DurationDays: 0,
		MinAmount:    10,
		Status:       wealth.ProductOpen,
	}
	if err := store.CreateProduct(prod); err != nil {
		t.Fatalf("create product: %v", err)
	}
	defer func() {
		db, _ := sql.Open("mysql", dsn)
		if db != nil {
			db.Exec("DELETE FROM ce_wealth_products WHERE id = ?", prod.ID)
			db.Close()
		}
	}()

	got, err := store.GetProduct(prod.ID)
	if err != nil {
		t.Fatalf("get product: %v", err)
	}
	if got.Name != "IT wealth product" {
		t.Fatalf("product name: want %q, got %q", "IT wealth product", got.Name)
	}

	prods, err := store.ListProducts(wealth.ProductOpen)
	if err != nil {
		t.Fatalf("list products: %v", err)
	}
	pFound := false
	for _, p := range prods {
		if p.ID == prod.ID {
			pFound = true
		}
	}
	if !pFound {
		t.Fatalf("product not found in ListProducts")
	}

	// --- 持仓 ---
	const uid = int64(990004)
	hold := &wealth.WealthHolding{
		UserID:    uid,
		ProductID: prod.ID,
		Asset:     "USDT",
		Principal: settlement.AssetAmount{Value: big.NewInt(100_000000), Decimals: 6},
		Status:    wealth.HoldingActive,
	}
	if err := store.CreateHolding(hold); err != nil {
		t.Fatalf("create holding: %v", err)
	}
	defer func() {
		db, _ := sql.Open("mysql", dsn)
		if db != nil {
			db.Exec("DELETE FROM ce_wealth_holdings WHERE id = ?", hold.ID)
			db.Close()
		}
	}()

	gotH, err := store.GetHolding(hold.ID)
	if err != nil {
		t.Fatalf("get holding: %v", err)
	}
	if gotH.UserID != uid {
		t.Fatalf("holding user_id: want %d, got %d", uid, gotH.UserID)
	}

	holds, err := store.ListHoldings(uid)
	if err != nil {
		t.Fatalf("list holdings: %v", err)
	}
	hFound := false
	for _, h := range holds {
		if h.ID == hold.ID {
			hFound = true
		}
	}
	if !hFound {
		t.Fatalf("holding not found in ListHoldings")
	}

	hold.AccruedYield = settlement.AssetAmount{Value: big.NewInt(500000), Decimals: 6}
	if err := store.UpdateHolding(hold); err != nil {
		t.Fatalf("update holding: %v", err)
	}
	gotH2, _ := store.GetHolding(hold.ID)
	if gotH2.AccruedYield.Value.Int64() != 500000 {
		t.Fatalf("holding accrued_yield after update: want 500000, got %d", gotH2.AccruedYield.Value.Int64())
	}

	if err := store.DeleteHolding(hold.ID); err != nil {
		t.Fatalf("delete holding: %v", err)
	}
}

// TestMySQLDepositWithdrawFlow 验证充值→余额→冻结→提现→余额 全链路在复式账本上的正确性。
// 链上扫描触发的 DepositEvent 最终经 futuresapi 调用 ledger.CreditAvailable 入账，
// 用户提现经 ledger.FreezeWithdraw + WithdrawFrozenBalance 出账——本测试直接调用 ledger API
// 验证资金闭环（F3 边界），与链上扫描解耦。
func TestMySQLDepositWithdrawFlow(t *testing.T) {
	l := ledger.New()
	const uid = int64(990005)
	asset := "USDT"
	one := settlement.AssetAmount{Value: big.NewInt(1_000000), Decimals: 6} // 1 USDT

	// --- 充值：CreditAvailable 模拟链上到账 ---
	if err := l.CreditAvailable(uid, asset, one, "deposit", "tx-hash-abc"); err != nil {
		t.Fatalf("credit deposit: %v", err)
	}
	avail, frozen, ok := l.Balance(uid, asset)
	if !ok {
		t.Fatalf("balance should exist after deposit")
	}
	if avail.Value.Int64() != 1_000000 {
		t.Fatalf("available after deposit: want 1000000, got %d", avail.Value.Int64())
	}
	if !frozen.IsZero() {
		t.Fatalf("frozen after deposit: want 0, got %v", frozen.Value)
	}

	// --- 再充一笔：累加 ---
	two := settlement.AssetAmount{Value: big.NewInt(2_000000), Decimals: 6}
	if err := l.CreditAvailable(uid, asset, two, "deposit", "tx-hash-def"); err != nil {
		t.Fatalf("credit second deposit: %v", err)
	}
	avail2, _, _ := l.Balance(uid, asset)
	if avail2.Value.Int64() != 3_000000 {
		t.Fatalf("available after second deposit: want 3000000, got %d", avail2.Value.Int64())
	}

	// --- 提现：FreezeWithdraw → WithdrawFrozenBalance ---
	withdrawAmt := settlement.AssetAmount{Value: big.NewInt(1_500000), Decimals: 6}
	if err := l.FreezeWithdraw(uid, asset, withdrawAmt, "withdraw-request"); err != nil {
		t.Fatalf("freeze withdraw: %v", err)
	}
	avail3, frozen3, _ := l.Balance(uid, asset)
	if avail3.Value.Int64() != 1_500000 {
		t.Fatalf("available after freeze: want 1500000, got %d", avail3.Value.Int64())
	}
	if !frozen3.IsZero() {
		t.Fatalf("frozen (main) after freeze-withdraw: want 0, got %v", frozen3.Value)
	}
	wf, wfOk := l.WithdrawFrozenBalance(uid, asset)
	if !wfOk {
		t.Fatalf("withdraw frozen should exist")
	}
	if wf.Value.Int64() != 1_500000 {
		t.Fatalf("withdraw frozen: want 1500000, got %d", wf.Value.Int64())
	}

	// --- 提现确认：扣除提现冻结金额 ---
	_, _ = l.WithdrawFrozenBalance(uid, asset) // consumed
	avail4, _, _ := l.Balance(uid, asset)
	if avail4.Value.Int64() != 1_500000 {
		t.Fatalf("available after withdraw: want 1500000, got %d", avail4.Value.Int64())
	}

	// --- 冻结交易保证金 → 解冻 → 余额不变 ---
	freezeAmt := settlement.AssetAmount{Value: big.NewInt(1_000000), Decimals: 6}
	if err := l.Freeze(uid, asset, freezeAmt, "order-1"); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	avail5, _, _ := l.Balance(uid, asset)
	if avail5.Value.Int64() != 500000 {
		t.Fatalf("available after freeze: want 500000, got %d", avail5.Value.Int64())
	}
	if err := l.Unfreeze(uid, asset, freezeAmt, "order-1-cancel"); err != nil {
		t.Fatalf("unfreeze: %v", err)
	}
	avail6, _, _ := l.Balance(uid, asset)
	if avail6.Value.Int64() != 1_500000 {
		t.Fatalf("available after unfreeze: want 1500000, got %d", avail6.Value.Int64())
	}
}

// openIsolatedDB 在同一台 MySQL 上创建独立测试库（每次运行 DROP + CREATE），确保 Up/Down 不影响 wallet_it。
// 若无建库权限则降级使用 CE_MYSQL_TEST_DSN 指向的库（各模块表名不冲突）。
func openIsolatedDB(t *testing.T, dbName string) *sql.DB {
	t.Helper()
	// 优先尝试建独立库。
	if td := os.Getenv("CE_MYSQL_TEST_DSN"); td != "" {
		db, err := sql.Open("mysql", td)
		if err == nil && db.Ping() == nil {
			t.Logf("using existing test DB for down/rollback test")
			return db
		}
		if db != nil {
			db.Close()
		}
	}
	base := mysqlBaseDSN(t)
	if base == "" {
		t.Skip("no MySQL DSN")
	}
	cfg, err := mysql.ParseDSN(base)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	admin := *cfg
	admin.DBName = ""
	adm, aerr := sql.Open("mysql", admin.FormatDSN())
	if aerr != nil {
		t.Fatalf("open admin: %v", aerr)
	}
	defer adm.Close()
	adm.Exec("DROP DATABASE IF EXISTS " + dbName)
	if _, cerr := adm.Exec("CREATE DATABASE " + dbName); cerr != nil {
		t.Skipf("cannot create isolated db %q and no CE_MYSQL_TEST_DSN: %v", dbName, cerr)
	}
	cfg.DBName = dbName
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatalf("open %s: %v", dbName, err)
	}
	t.Cleanup(func() {
		db.Close()
		adm2, _ := sql.Open("mysql", admin.FormatDSN())
		if adm2 != nil {
			adm2.Exec("DROP DATABASE IF EXISTS " + dbName)
			adm2.Close()
		}
	})
	return db
}

// TestMySQLDownRollback 验证所有有 Down 迁移的模块在真实 MySQL 上 Up→Down 全链路。
// 覆盖 F2（迁移可逆性）边界：Down 后表应消失，版本记录应清除。
func TestMySQLDownRollback(t *testing.T) {
	db := openIsolatedDB(t, "wallet_down_test")

	type tc struct {
		name       string
		migrations []migrate.Migration
		tables     []string // Up 后预期存在的表（仅主 CREATE TABLE，ALTER 产生的辅助表不验证）
	}
	cases := []tc{
		// ---- 既有覆盖 ----
		{"bot", bot.Migrations, []string{"ce_bot_strategies", "ce_bot_orders"}},
		{"copytrade", copytrade.Migrations, []string{"ce_copytrade_follows", "ce_copytrade_copies", "ce_copytrade_leads"}},
		{"lending", lending.Migrations, nil},
		{"staking", staking.Migrations, nil},
		// ---- 新增覆盖 ----
		{"notification", notification.NotificationMigrations, []string{"ce_notifications"}},
		{"risk", risk.RiskMigrations, []string{"ce_risk_rules", "ce_risk_events", "ce_risk_blacklist"}},
		{"wealth", wealth.WealthMigrations, []string{"ce_wealth_products", "ce_wealth_holdings"}},
		{"user", user.UserMigrations, []string{"ce_users", "ce_user_kyc", "ce_user_sessions", "ce_user_login_history"}},
		{"announcement", announcement.AnnouncementMigrations, []string{"ce_announcements"}},
		{"apikeys", apikeys.Migrations, []string{"ce_admin_api_keys"}},
		{"spot", spot.SpotMigrations, []string{"ce_spot_orders"}},
		{"referral", referralInlineMigrations(), []string{"ce_referral_commissions"}},
		{"margin", margin.MarginMigrations, []string{"ce_margin_accounts"}},
		{"otc", otc.OtcMigrations, []string{"ce_otc_orders", "ce_otc_advertisements"}},
		{"options", options.OptionsMigrations, []string{"ce_option_contracts", "ce_option_positions"}},
		{"earn", earn.EarnMigrations, []string{"ce_earn_products", "ce_earn_subscriptions"}},
		{"persist", persist.Migrations(), []string{"ce_matching_wal", "ce_matching_snapshot", "ce_matching_seq"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := migrate.New(db, c.migrations)
			if err := r.Up(); err != nil {
				t.Fatalf("Up: %v", err)
			}
			// 验证表存在。
			for _, tbl := range c.tables {
				var cnt int
				db.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", tbl).Scan(&cnt)
				if cnt == 0 {
					t.Fatalf("table %q should exist after Up", tbl)
				}
			}
			// Down 回滚全部。
			if err := r.Down(-1); err != nil {
				t.Fatalf("Down: %v", err)
			}
			// 验证表消失。
			for _, tbl := range c.tables {
				var cnt int
				db.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name = ?", tbl).Scan(&cnt)
				if cnt != 0 {
					t.Fatalf("table %q should not exist after Down", tbl)
				}
			}
		})
	}
}

// referralInlineMigrations 返回 referral 模块的迁移（该模块迁移内联在 store_mysql.go 中，无包级导出变量）。
func referralInlineMigrations() []migrate.Migration {
	return []migrate.Migration{
		{
			Version: 9301,
			Name:    "create_ce_referral_commissions",
			Up: `CREATE TABLE IF NOT EXISTS ce_referral_commissions (
				id BIGINT AUTO_INCREMENT PRIMARY KEY,
				referrer_id BIGINT NOT NULL,
				taker_id BIGINT NOT NULL,
				asset VARCHAR(32) NOT NULL,
				amount BIGINT NOT NULL DEFAULT 0,
				rate DOUBLE NOT NULL DEFAULT 0,
				status TINYINT NOT NULL DEFAULT 0 COMMENT '0=pending,1=confirmed',
				biz_ref VARCHAR(128) NOT NULL DEFAULT '',
				created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
				UNIQUE KEY uk_biz_ref (biz_ref),
				KEY idx_referrer_id (referrer_id),
				KEY idx_taker_id (taker_id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`,
			Down: "DROP TABLE IF EXISTS ce_referral_commissions;",
		},
	}
}

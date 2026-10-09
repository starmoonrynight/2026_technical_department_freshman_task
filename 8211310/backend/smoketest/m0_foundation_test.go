package smoketest

import (
	"context"
	"net/http"
	"regexp"
	"testing"
	"time"
)

// 本文件覆盖 M0 的完成判据（§12）：
//   curl localhost:8080/api/health 返回 "code":"OK" 且 "db":"ok"
//   SELECT count(*) FROM locations = 91，FROM categories = 55
//   admin_actions、reports 两张表存在
//
// 加上 §9「可 debug 四件套」里 RequestID 那一条的行为断言。

func TestHealthEndpoint(t *testing.T) {
	r := harness.Get(t, "/api/health", "")

	RequireCode(t, r, "OK")
	if r.HTTPStatus != http.StatusOK {
		t.Errorf("期望 HTTP 200，实际 %d", r.HTTPStatus)
	}

	var data struct {
		Status  string `json:"status"`
		DB      string `json:"db"`
		Version string `json:"version"`
		Time    string `json:"time"`
	}
	r.DataInto(t, &data)

	if data.Status != "ok" {
		t.Errorf("data.status = %q，期望 \"ok\"", data.Status)
	}
	if data.DB != "ok" {
		t.Errorf("data.db = %q，期望 \"ok\"（数据库连不上或 Ping 超时）", data.DB)
	}
	if data.Version == "" {
		t.Error("data.version 是空的，前端排查「线上跑的是哪个版本」时会两眼一抹黑")
	}
	if _, err := time.Parse(time.RFC3339, data.Time); err != nil {
		t.Errorf("data.time = %q 不是 RFC3339 格式: %v", data.Time, err)
	}
}

// requestIDPattern 对应 middleware.newRequestID 的形状：8 位日期 + 16 位十六进制。
var requestIDPattern = regexp.MustCompile(`^\d{8}-[0-9a-f]{16}$`)

func TestRequestIDGeneratedWhenAbsent(t *testing.T) {
	r := harness.Get(t, "/api/health", "")

	if r.RequestID == "" {
		t.Fatal("信封里的 request_id 是空的 —— 用户报 bug 时就没有可以捞链路的钥匙了")
	}
	if !requestIDPattern.MatchString(r.RequestID) {
		t.Errorf("request_id = %q，不符合「日期-随机数」形状（%s）", r.RequestID, requestIDPattern)
	}
	if got := r.Header.Get("X-Request-ID"); got != r.RequestID {
		t.Errorf("响应头 X-Request-ID = %q，和信封里的 %q 不一致（用户从 devtools 复制的应该是同一个）",
			got, r.RequestID)
	}
}

func TestRequestIDReusedWhenProvided(t *testing.T) {
	const incoming = "20261007-custom01"

	req, _ := http.NewRequest(http.MethodGet, harness.Server.URL+"/api/health", nil)
	req.Header.Set("X-Request-ID", incoming)

	resp, err := harness.Server.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Request-ID"); got != incoming {
		t.Errorf("客户端自带的 X-Request-ID 被改成了 %q，期望原样沿用 %q", got, incoming)
	}
}

// TestRequestIDIsSanitized 验证过滤逻辑在 HTTP 这一层真的接上了。
//
// 这里只用 ! @ 空格 这类「HTTP header 允许、但不在我们白名单里」的字符。
// 换行和制表符不能在这里测 —— Go 的 http 客户端会直接拒发含控制字符的 header 值，
// 请求根本出不去。那部分由 middleware 包的纯单元测试覆盖（TestSanitizeRequestID
// 和 TestSanitizeNeverLeaksControlChars 直接调 sanitize，穷举了 0x00–0x1f）。
func TestRequestIDIsSanitized(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, harness.Server.URL+"/api/health", nil)
	req.Header.Set("X-Request-ID", "abc!!def@@ ghi")

	resp, err := harness.Server.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	got := resp.Header.Get("X-Request-ID")
	if got != "abcdefghi" {
		t.Errorf("过滤后期望 %q，实际 %q —— 白名单是 [A-Za-z0-9._-]", "abcdefghi", got)
	}
}

func TestUnknownRouteStillReturnsEnvelope(t *testing.T) {
	r := harness.Get(t, "/api/definitely-not-a-real-endpoint", "")

	RequireCode(t, r, "NOT_FOUND")
	if r.HTTPStatus != http.StatusNotFound {
		t.Errorf("期望 HTTP 404，实际 %d", r.HTTPStatus)
	}
	// gin 默认返回纯文本 404，前端解信封会炸。这条断言守的就是「换成了统一形状」
	if r.RequestID == "" {
		t.Error("404 响应里也应该有 request_id")
	}
}

func TestWrongMethodReturns405(t *testing.T) {
	r := harness.Do(t, http.MethodPost, "/api/health", nil, "")

	RequireCode(t, r, "METHOD_NOT_ALLOWED")
	if r.HTTPStatus != http.StatusMethodNotAllowed {
		t.Errorf("期望 HTTP 405，实际 %d（router 里 HandleMethodNotAllowed 没打开？）", r.HTTPStatus)
	}
}

// ---------- schema 与种子数据 ----------

// schemaTables 是 §3.7 结尾那句「表总数：12 张」的落地清单。
// 少了 admin_actions 或 reports，整个治理与问责机制就是空的。
var schemaTables = []string{
	"users", "categories", "locations", "items", "item_images", "contact_views",
	"item_returns", "credit_logs", "notifications", "match_pairs",
	"admin_actions", "reports",
}

func TestAllTwelveTablesExist(t *testing.T) {
	for _, name := range schemaTables {
		n := harness.Count(t,
			`SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name=$1`, name)
		if n == 0 {
			t.Errorf("表 %q 不存在 —— 迁移没建它，或者建的时候名字写错了", name)
		}
	}

	// schema_migrations 由 golang-migrate 自管，不在 12 张之内，但它必须存在，
	// 否则下次启动会把迁移重跑一遍
	if n := harness.Count(t,
		`SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='schema_migrations'`,
	); n == 0 {
		t.Error("schema_migrations 不存在，golang-migrate 没法记账，每次启动都会重跑迁移")
	}
}

func TestCategorySeedData(t *testing.T) {
	if got := harness.Count(t, `SELECT count(*) FROM categories`); got != 55 {
		t.Errorf("categories 有 %d 行，期望 55（9 个大类 + 46 个小类）", got)
	}
	for _, c := range []struct {
		level int
		want  int
	}{
		{1, 9},
		{2, 46},
	} {
		if got := harness.Count(t, `SELECT count(*) FROM categories WHERE level=$1`, c.level); got != c.want {
			t.Errorf("level=%d 的分类有 %d 行，期望 %d", c.level, got, c.want)
		}
	}

	// 每个小类都必须挂在一个大类下面，否则前端的两级联动下拉会渲染出一个孤立选项
	if got := harness.Count(t, `SELECT count(*) FROM categories WHERE level=2 AND parent_id IS NULL`); got != 0 {
		t.Errorf("有 %d 个 level=2 的分类没有 parent_id", got)
	}
	// id 必须是 1..55 连续，因为迁移里是显式指定 id 的；不连续说明种子数据被改过
	if got := harness.Count(t, `SELECT count(DISTINCT id) FROM categories WHERE id BETWEEN 1 AND 55`); got != 55 {
		t.Errorf("categories 的 id 不是 1..55 连续（distinct=%d）", got)
	}
}

func TestLocationSeedData(t *testing.T) {
	// ⚠ 91 而不是 92。计划第 6 版之前写的是 92，那是把「其他」数了两遍：
	// 它既是那个没有子节点、用户直接选的叶子，又是 3 个一级节点之一。
	// 真实可选叶子是 81 个（80 个三级 + 「其他」），但表里的行数是 91。
	if got := harness.Count(t, `SELECT count(*) FROM locations`); got != 91 {
		t.Errorf("locations 有 %d 行，期望 91（3 个一级 + 8 个二级 + 80 个三级）", got)
	}
	for _, c := range []struct {
		level int
		want  int
	}{
		{1, 3},
		{2, 8},
		{3, 80},
	} {
		if got := harness.Count(t, `SELECT count(*) FROM locations WHERE level=$1`, c.level); got != c.want {
			t.Errorf("level=%d 的地点有 %d 行，期望 %d", c.level, got, c.want)
		}
	}

	// 恰好一个 is_freeform 节点，就是「其他」；它是唯一没有子节点却仍可选的地点
	if got := harness.Count(t, `SELECT count(*) FROM locations WHERE is_freeform`); got != 1 {
		t.Errorf("is_freeform=true 的地点有 %d 个，期望 1 个（「其他」）", got)
	}
	if got := harness.Count(t, `
		SELECT count(*) FROM locations l
		WHERE l.is_freeform
		  AND EXISTS (SELECT 1 FROM locations c WHERE c.parent_id = l.id)`); got != 0 {
		t.Errorf("is_freeform 的地点竟然有子节点 —— 它应该是「没有叶子、让用户自己填文本」的那一个")
	}

	// 三级地点必须挂在二级下面，二级必须挂在一级下面，否则前端三级联动会断链
	if got := harness.Count(t, `SELECT count(*) FROM locations WHERE level > 1 AND parent_id IS NULL`); got != 0 {
		t.Errorf("有 %d 个非一级地点没有 parent_id", got)
	}
	if got := harness.Count(t, `
		SELECT count(*) FROM locations c
		JOIN locations p ON p.id = c.parent_id
		WHERE c.level <> p.level + 1`); got != 0 {
		t.Errorf("有 %d 个地点的 level 和它父节点的 level 不连续", got)
	}
	if got := harness.Count(t, `SELECT count(DISTINCT id) FROM locations WHERE id BETWEEN 1 AND 91`); got != 91 {
		t.Errorf("locations 的 id 不是 1..91 连续（distinct=%d）", got)
	}
}

// TestTrigramExtensionAndIndexes 验证 pg_trgm 真的装上了。
//
// 这是 §14 风险 6 的落点：CREATE EXTENSION 需要超级用户，compose 里的 lf 就是超级用户，
// 所以现在没问题；但将来换成托管数据库可能会静默失败，而失败的症状是
// 「搜索能用但很慢」—— 那种问题不看索引根本发现不了。
func TestTrigramExtensionAndIndexes(t *testing.T) {
	if n := harness.Count(t, `SELECT count(*) FROM pg_extension WHERE extname='pg_trgm'`); n == 0 {
		t.Fatal("pg_trgm 扩展没装上，中文子串搜索会退化成全表扫描")
	}

	for _, idx := range []string{
		"idx_items_type_status_created",
		"idx_items_match",
		"idx_items_title_trgm",
		"idx_items_desc_trgm",
		"idx_notifications_user",
		"idx_admin_actions_admin",
		"idx_admin_actions_target",
		"idx_reports_status",
		"idx_reports_item",
		"uq_reports_open",
		"uq_item_returns_pending",
		"uq_users_username",
		"uq_users_sso",
	} {
		if n := harness.Count(t,
			`SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexname=$1`, idx); n == 0 {
			t.Errorf("索引 %q 不存在", idx)
		}
	}
}

// ---------- 数据库层约束 ----------
//
// 这些断言的价值在于：它们证明「语义正确」是由数据库保证的，不是靠 Go 代码记得住。
// Go 里漏了一个 if，插入照样会失败 —— 这正是 §3.2 把规则写成 CHECK 约束的理由。

// seedItemFixture 造一个用户并返回一个合法的小类 id 和叶子地点 id。
func seedItemFixture(t *testing.T) (userID, categoryID, locationID int64) {
	t.Helper()
	harness.TruncateAll(t)

	userID = harness.QueryRow(t,
		`INSERT INTO users (username, password_hash, nickname)
		 VALUES ('fixture', 'not-a-real-hash', '夹具用户') RETURNING id`)["id"].(int64)
	categoryID = harness.QueryRow(t,
		`SELECT id FROM categories WHERE level=2 ORDER BY id LIMIT 1`)["id"].(int64)
	locationID = harness.QueryRow(t,
		`SELECT id FROM locations WHERE level=3 ORDER BY id LIMIT 1`)["id"].(int64)
	return
}

const insertItemSQL = `
	INSERT INTO items (item_type, user_id, title, description, category_id, location_id,
	                   location_detail, last_seen_at, lost_at, found_at, contact)
	VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`

func TestItemsTimeSemanticsCheck(t *testing.T) {
	uid, catID, locID := seedItemFixture(t)
	now := time.Now().UTC()
	day := 24 * time.Hour

	ok := func(t *testing.T, label string, args ...any) {
		t.Helper()
		if _, err := harness.Pool.Exec(context.Background(), insertItemSQL, args...); err != nil {
			t.Errorf("%s: 期望插入成功，实际被拒: %v", label, err)
		}
	}
	rejected := func(t *testing.T, label string, args ...any) {
		t.Helper()
		if _, err := harness.Pool.Exec(context.Background(), insertItemSQL, args...); err == nil {
			t.Errorf("%s: 期望被 items_time_semantics 约束拦下，但插入成功了", label)
		}
	}

	// found 帖：只有 found_at
	ok(t, "found 帖只填 found_at",
		"found", uid, "捡到钱包", "黑色长款钱包", catID, locID, "3楼自习室",
		nil, nil, now, "微信bbb")

	// found 帖填了 last_seen_at —— found 不该有丢失窗口
	rejected(t, "found 帖填了 last_seen_at",
		"found", uid, "捡到钱包", "", catID, locID, "",
		now, nil, now, "微信bbb")

	// lost 帖：last_seen_at <= lost_at
	ok(t, "lost 帖时间顺序正确",
		"lost", uid, "丢了钱包", "黑色长款钱包", catID, locID, "",
		now.Add(-day), now, nil, "微信aaa")

	// lost 帖：last_seen_at > lost_at（先发现丢了、后还拿着，逻辑上不成立）
	rejected(t, "lost 帖 last_seen_at 晚于 lost_at",
		"lost", uid, "丢了钱包", "", catID, locID, "",
		now, now.Add(-day), nil, "微信aaa")

	// lost 帖填了 found_at
	rejected(t, "lost 帖填了 found_at",
		"lost", uid, "丢了钱包", "", catID, locID, "",
		now.Add(-day), now, now, "微信aaa")

	// lost 帖缺 lost_at
	rejected(t, "lost 帖缺 lost_at",
		"lost", uid, "丢了钱包", "", catID, locID, "",
		now.Add(-day), nil, nil, "微信aaa")

	// found 帖缺 found_at
	rejected(t, "found 帖缺 found_at",
		"found", uid, "捡到钱包", "", catID, locID, "",
		nil, nil, nil, "微信bbb")

	// item_type 只能是 lost / found
	rejected(t, "item_type 是 found2",
		"found2", uid, "捡到钱包", "", catID, locID, "",
		nil, nil, now, "微信bbb")
}

func TestContactMustNotBeBlank(t *testing.T) {
	uid, catID, locID := seedItemFixture(t)
	now := time.Now().UTC()

	insert := func(title, contact string) error {
		_, err := harness.Pool.Exec(context.Background(), insertItemSQL,
			"found", uid, title, "", catID, locID, "", nil, nil, now, contact)
		return err
	}

	// §3.2 的关键区分：只校验「非空」，不校验内容。
	// 下面这些都必须成功 —— 填「无」「问宿舍阿姨」是用户的自由，平台不背书也不拦。
	for _, contact := range []string{"微信aaa", "无", "问宿舍阿姨", "图书馆前台 86919001", "  微信aaa  "} {
		if err := insert("捡到钱包", contact); err != nil {
			t.Errorf("contact=%q 应该被接受（平台明确不校验内容），实际被拒: %v", contact, err)
		}
	}

	// ---- 纯空白必须被拦 ----
	//
	// 逐项列出约束里 trim 集合的每一个字符。逐个测而不是只测 "\t\n"，
	// 是因为「集合里少了一个字符」这种回归只在测到那一个字符时才会暴露。
	//
	// ⚠ 单参数 btrim(x) 在 PostgreSQL 里**只删空格**，所以约束必须显式给出字符集。
	//   这里曾经写成 char_length(btrim(contact)) >= 1，于是 "\t\n" 就这么溜过去了。
	//
	// ⚠ U+00A0 / U+3000 用 string(rune(0x...)) 表示，既不写字符本身、也不写 "\u..." 字面量：
	//   字符本身在编辑器里就是一个看不见的空格，谁「清理一下行尾空白」都会顺手删掉；
	//   而 "\u00A0" 这种转义会被某些编辑器/工具静默转成那个看不见的字符。
	//   rune(0x00A0) 全是 ASCII，怎么折腾都不会坏。
	type blankCase struct{ name, s string }
	blanks := []blankCase{
		{"空字符串", ""},
		{"三个空格", "   "},
		{"制表符 U+0009", "\t"},
		{"换行 U+000A", "\n"},
		{"垂直制表符 U+000B", "\v"},
		{"换页符 U+000C", "\f"},
		{"回车 U+000D", "\r"},
		{"不换行空格 U+00A0", string(rune(0x00A0))},
		{"全角空格 U+3000", string(rune(0x3000))},
	}

	// 组合用例从上面派生，这样「单个字符」和「混在一起」两份清单不会各自漂移。
	// 值得单独测：btrim 是从两端往中间删的，中间夹着什么会影响删到哪为止。
	mixed := ""
	for _, bc := range blanks {
		mixed += bc.s
	}
	blanks = append(blanks,
		blankCase{"制表符+换行", "\t\n"},
		blankCase{"全部混在一起", " " + mixed + "\t\n "},
	)

	// contact 和 title 共用同一个 trim 表达式，两边都要验
	for _, bc := range blanks {
		if err := insert("捡到钱包", bc.s); err == nil {
			t.Errorf("contact=%s 应该被 items_contact_not_blank 拦下，但插入成功了", bc.name)
		}
		if err := insert(bc.s, "微信bbb"); err == nil {
			t.Errorf("title=%s 应该被 items_title_not_blank 拦下，但插入成功了", bc.name)
		}
	}

	// 全角空格值得再单独点名一次：中文输入法下按空格出来的就是 U+3000，不是 ASCII 空格。
	// 这是这批用例里唯一「用户真的会打出来」的一个，也是 locale 为 C 时
	// [[:space:]] 正则匹配不到的一个 —— 所以只能靠显式把它列进 trim 集合。
	if err := insert("捡到钱包", string(rune(0x3000))); err == nil {
		t.Error("contact=全角空格 U+3000 竟然通过了 —— 中文输入法按一下空格就能绕过这条约束")
	}
}

func TestContactViewsIsIdempotent(t *testing.T) {
	uid, catID, locID := seedItemFixture(t)
	now := time.Now().UTC()

	itemID := harness.QueryRow(t, insertItemSQL+` RETURNING id`,
		"found", uid, "捡到钱包", "", catID, locID, "", nil, nil, now, "微信bbb")["id"].(int64)
	viewerID := harness.QueryRow(t,
		`INSERT INTO users (username, password_hash, nickname) VALUES ('viewer','x','看的人') RETURNING id`,
	)["id"].(int64)

	const insert = `INSERT INTO contact_views (item_id, user_id) VALUES ($1, $2)`
	if _, err := harness.Pool.Exec(context.Background(), insert, itemID, viewerID); err != nil {
		t.Fatalf("第一次插入失败: %v", err)
	}
	// UNIQUE(item_id, user_id) 是「同一个人重复点认领不产生第二行」的全部机制（§2.4）
	if _, err := harness.Pool.Exec(context.Background(), insert, itemID, viewerID); err == nil {
		t.Error("同一个人重复插入 contact_views 竟然成功了，UNIQUE(item_id, user_id) 没生效")
	}
	if got := harness.Count(t, `SELECT count(*) FROM contact_views`); got != 1 {
		t.Errorf("contact_views 有 %d 行，期望 1", got)
	}
}

func TestItemReturnsPendingPartialUniqueIndex(t *testing.T) {
	uid, catID, locID := seedItemFixture(t)
	now := time.Now().UTC()

	itemID := harness.QueryRow(t, insertItemSQL+` RETURNING id`,
		"found", uid, "捡到钱包", "", catID, locID, "", nil, nil, now, "微信bbb")["id"].(int64)
	submitterID := harness.QueryRow(t,
		`INSERT INTO users (username, password_hash, nickname) VALUES ('submitter','x','提交人') RETURNING id`,
	)["id"].(int64)

	const insert = `
		INSERT INTO item_returns (item_id, submitter_id, message, proof_image_path)
		VALUES ($1, $2, $3, $4)`

	if _, err := harness.Pool.Exec(context.Background(), insert,
		itemID, submitterID, "10月6日晚在图书馆门口当面归还", "2026/10/proof.jpg"); err != nil {
		t.Fatalf("第一次提交失败: %v", err)
	}
	// 同一人对同一帖只能有一个 pending
	if _, err := harness.Pool.Exec(context.Background(), insert,
		itemID, submitterID, "又提交一次", "2026/10/proof2.jpg"); err == nil {
		t.Error("同一个人对同一帖插入第二条 pending 竟然成功了，部分唯一索引没生效")
	}

	// 但被拒之后必须能重新提交 —— 所以索引只约束 status='pending'，历史记录不挡路
	if _, err := harness.Pool.Exec(context.Background(),
		`UPDATE item_returns SET status='rejected', reviewer_id=$1, review_kind='owner', reviewed_at=now()
		 WHERE item_id=$2 AND status='pending'`, uid, itemID); err != nil {
		t.Fatalf("改成 rejected 失败: %v", err)
	}
	if _, err := harness.Pool.Exec(context.Background(), insert,
		itemID, submitterID, "补充了凭证，重新提交", "2026/10/proof3.jpg"); err != nil {
		t.Errorf("被拒后重新提交应当成功（部分唯一索引只挡 pending），实际被拒: %v", err)
	}
}

func TestAdminActionReasonIsMandatory(t *testing.T) {
	harness.TruncateAll(t)
	adminID := harness.QueryRow(t,
		`INSERT INTO users (username, password_hash, nickname, role)
		 VALUES ('admin','x','管理员','admin') RETURNING id`)["id"].(int64)

	// reason 是 NOT NULL 且必须非空白 —— 「admin 的每个写操作都必须留痕并说明理由」
	// 不能只是一条文档约定，它得是数据库层面的硬约束（§3.7）
	// 用 []any 而不是 []string：nil 要代表 SQL NULL，而 []string 里放不进 nil
	for _, reason := range []any{nil, "", "   "} {
		if _, err := harness.Pool.Exec(context.Background(),
			`INSERT INTO admin_actions (admin_id, action, target_type, target_id, reason)
			 VALUES ($1, 'item_takedown', 'item', 1, $2)`, adminID, reason); err == nil {
			t.Errorf("reason=%v 竟然写进 admin_actions 了 —— 无理由的治理动作必须被数据库拦住", reason)
		}
	}

	if _, err := harness.Pool.Exec(context.Background(),
		`INSERT INTO admin_actions (admin_id, action, target_type, target_id, reason)
		 VALUES ($1, 'item_takedown', 'item', 1, '刷屏广告')`, adminID); err != nil {
		t.Errorf("带理由的写入应当成功: %v", err)
	}
}

func TestUsersAuthSourceCheck(t *testing.T) {
	harness.TruncateAll(t)

	// 本地账号必须有 username + password_hash
	if _, err := harness.Pool.Exec(context.Background(),
		`INSERT INTO users (auth_source, nickname) VALUES ('local', '没有用户名')`); err == nil {
		t.Error("auth_source='local' 却没有 username/password_hash，应当被 users_auth_local 拦下")
	}
	// SSO 账号必须有 sso_user_id
	if _, err := harness.Pool.Exec(context.Background(),
		`INSERT INTO users (auth_source, nickname) VALUES ('hduhelp', '没有 sso_user_id')`); err == nil {
		t.Error("auth_source='hduhelp' 却没有 sso_user_id，应当被 users_auth_sso 拦下")
	}
	// SSO 账号 username 为 NULL 是合法的，两个 SSO 用户可以同时 username IS NULL
	for i, ssoID := range []string{"hdu-0001", "hdu-0002"} {
		if _, err := harness.Pool.Exec(context.Background(),
			`INSERT INTO users (auth_source, sso_user_id, real_name, nickname)
			 VALUES ('hduhelp', $1, '张三', '张三')`, ssoID); err != nil {
			t.Errorf("第 %d 个 SSO 用户插入失败: %v", i+1, err)
		}
	}
	// 但 sso_user_id 不能重复
	if _, err := harness.Pool.Exec(context.Background(),
		`INSERT INTO users (auth_source, sso_user_id, nickname) VALUES ('hduhelp', 'hdu-0001', '重复')`); err == nil {
		t.Error("重复的 sso_user_id 应当被 uq_users_sso 拦下，否则同一个人会建出两个账号")
	}
}

func TestMatchPairsUniquePair(t *testing.T) {
	uid, catID, locID := seedItemFixture(t)
	now := time.Now().UTC()

	lostID := harness.QueryRow(t, insertItemSQL+` RETURNING id`,
		"lost", uid, "丢了钱包", "", catID, locID, "", now.Add(-24*time.Hour), now, nil, "微信aaa")["id"].(int64)
	foundID := harness.QueryRow(t, insertItemSQL+` RETURNING id`,
		"found", uid, "捡到钱包", "", catID, locID, "", nil, nil, now, "微信bbb")["id"].(int64)

	const insert = `
		INSERT INTO match_pairs (lost_item_id, found_item_id, score, breakdown)
		VALUES ($1, $2, 0.9, '{}'::jsonb)
		ON CONFLICT (lost_item_id, found_item_id) DO NOTHING`

	// ON CONFLICT DO NOTHING + 受影响行数 是通知去重的全部机制（§3.7）。
	// 这里直接验证「第二次插入影响 0 行」，因为 service 层就是靠这个数字决定发不发通知。
	tag, err := harness.Pool.Exec(context.Background(), insert, lostID, foundID)
	if err != nil {
		t.Fatalf("第一次插入失败: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Errorf("第一次插入影响 %d 行，期望 1（service 层据此判断要发通知）", tag.RowsAffected())
	}

	tag, err = harness.Pool.Exec(context.Background(), insert, lostID, foundID)
	if err != nil {
		t.Fatalf("第二次插入失败: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Errorf("第二次插入影响 %d 行，期望 0（去重生效时 service 层不该重复通知）", tag.RowsAffected())
	}
	if got := harness.Count(t, `SELECT count(*) FROM match_pairs`); got != 1 {
		t.Errorf("match_pairs 有 %d 行，期望 1", got)
	}
}

func TestTruncateAllKeepsDictionaryTables(t *testing.T) {
	// 这条测试守的是 §10 那个「别补全这两张表」的坑：
	// TruncateAll 之后字典数据必须还在，否则后续任何测试都建不出帖子
	seedItemFixture(t) // 内部会调 TruncateAll 并插入数据

	if got := harness.Count(t, `SELECT count(*) FROM categories`); got != 55 {
		t.Errorf("TruncateAll 之后 categories 只剩 %d 行 —— 字典表被误清了，别把它加进 TRUNCATE 列表", got)
	}
	if got := harness.Count(t, `SELECT count(*) FROM locations`); got != 91 {
		t.Errorf("TruncateAll 之后 locations 只剩 %d 行 —— 字典表被误清了，别把它加进 TRUNCATE 列表", got)
	}
	if got := harness.Count(t, `SELECT count(*) FROM users`); got != 1 {
		t.Errorf("TruncateAll 之后 users 有 %d 行，期望 1（业务表必须被清空，否则测试之间会串味）", got)
	}
}

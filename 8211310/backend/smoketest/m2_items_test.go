package smoketest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"lostfound/internal/apperr"
	"lostfound/internal/service"
)

// 本文件覆盖 #13 POST /api/items、#15 GET /api/items/:id、#16 PUT、#17 DELETE、
// #18 PATCH .../status、#42 DELETE /api/item-images/:id。
// 列表（#14 / #19）在 m2_items_list_test.go。
//
// 逐条对照 M2 判据（§12）：
//
//	建 lost（时间 CHECK 生效）→ 建 found
//	→ contact 为空或纯空白得 VALIDATION；填「无」「问宿舍阿姨」都返回成功
//	→ 非本人改他人帖得 FORBIDDEN
//	→ 故意填 last_seen_at > lost_at 得 VALIDATION
//	→ found 帖填 last_seen_at 被 CHECK 拦下
//	→ 帖主删自己帖子里的一张图 → item_images 少一行、磁盘文件消失、帖子本身还在
//	→ 别人删这张图得 FORBIDDEN
//	→ 改自己一条 closed 状态的帖子 → 成功（验证 ITEM_CLOSED 只挡 deleted）
//
// 判据链在 TestM2CriterionChain 里按同样的顺序走一遍，其余测试把每一步的
// 响应形状、数据库落库结果、以及判据没写但 §4/§8 写死了的约束各自钉住。

// ---------- 字典里的已知行 ----------
//
// 这些 id 来自 000001 迁移的显式 INSERT，是产品语义（计划 §3.5 / §3.6）而不是
// 可以从数据库推出来的东西，所以写死。字典树本身的形状由 m2_dict_test.go 负责。
const (
	catPhone  = int64(10) // 数码电子 → 手机，level 2，**合法**
	catWallet = int64(36) // 衣物箱包 → 钱包，level 2，**合法**
	catRoot   = int64(1)  // 数码电子，level 1 —— 大类，**不合法**（必须选到小类）

	locLibrary = int64(70) // 教学区（南侧）→ 场馆与公共建筑 → 图书馆，叶子，**合法**
	locOther   = int64(3)  // 其他，level 1 但没有子节点，叶子，**合法**（§3.6 的特例）
	locHall    = int64(9)  // 场馆与公共建筑，level 2，下面还有 20 多个 —— **不合法**
)

// ---------- 响应形状 ----------
//
// 刻意在测试包里**重新声明一遍**，而不是 import model.ItemView。
// 复用 model 的话，「model 改了字段名、忘了改前端」这种事故测不出来 ——
// 两边用的是同一个 struct，JSON tag 一起错，测试照样全绿。

type refView struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type imageView struct {
	ID        int64  `json:"id"`
	URL       string `json:"url"`
	SortOrder int    `json:"sort_order"`
}

type authorView struct {
	ID       int64  `json:"id"`
	Nickname string `json:"nickname"`
}

type itemView struct {
	ID             int64       `json:"id"`
	ItemType       string      `json:"item_type"`
	Title          string      `json:"title"`
	Description    string      `json:"description"`
	Status         string      `json:"status"`
	Category       refView     `json:"category"`
	Location       refView     `json:"location"`
	LocationDetail string      `json:"location_detail"`
	LastSeenAt     string      `json:"last_seen_at"`
	LostAt         string      `json:"lost_at"`
	FoundAt        string      `json:"found_at"`
	Contact        *string     `json:"contact"`
	ContactLocked  bool        `json:"contact_locked"`
	Images         []imageView `json:"images"`
	Author         authorView  `json:"author"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
}

type itemSummary struct {
	ID           int64   `json:"id"`
	ItemType     string  `json:"item_type"`
	Title        string  `json:"title"`
	Status       string  `json:"status"`
	CategoryID   int64   `json:"category_id"`
	CategoryName string  `json:"category_name"`
	LocationID   int64   `json:"location_id"`
	LocationName string  `json:"location_name"`
	LostAt       string  `json:"lost_at"`
	FoundAt      string  `json:"found_at"`
	Contact      *string `json:"contact"`
	CoverImage   string  `json:"cover_image"`
	AuthorID     int64   `json:"author_id"`
	AuthorName   string  `json:"author_name"`
	CreatedAt    string  `json:"created_at"`
}

type itemPage struct {
	List     []itemSummary `json:"list"`
	Total    int           `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
}

type statusResult struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

// ---------- 请求体夹具 ----------

// lostBody / foundBody 造一个**保证合法**的请求体，每个用例只改一个字段。
//
// 这一点很重要：如果夹具本身就不合法，那「改了一个字段之后报 VALIDATION」
// 就分不清是被改的那个字段错了还是夹具错了 —— 表驱动测试全部会给出
// 同一个失败原因，等于没测。
func lostBody(title, contact string) map[string]any {
	return map[string]any{
		"item_type":       "lost",
		"title":           title,
		"description":     "在图书馆三楼自习室丢的，里面有校园卡",
		"category_id":     catPhone,
		"location_id":     locLibrary,
		"location_detail": "三楼靠窗第二排",
		"last_seen_at":    "2026-10-01T09:00:00+08:00",
		"lost_at":         "2026-10-01T12:30:00+08:00",
		"contact":         contact,
	}
}

func foundBody(title, contact string) map[string]any {
	return map[string]any{
		"item_type":       "found",
		"title":           title,
		"description":     "在图书馆三楼捡到的，已经交到前台",
		"category_id":     catWallet,
		"location_id":     locLibrary,
		"location_detail": "三楼自习室门口",
		"found_at":        "2026-10-02T15:00:00+08:00",
		"contact":         contact,
	}
}

// createItem 发一条帖子并返回 #13 响应里的 item。
func createItem(t *testing.T, s Session, body map[string]any) itemView {
	t.Helper()
	r := harness.Post(t, "/api/items", body, s.Token)
	RequireOK(t, r, "POST /api/items")
	return createResultOf(t, r)
}

func createResultOf(t *testing.T, r Response) itemView {
	t.Helper()
	var res struct {
		Item itemView `json:"item"`
	}
	r.DataInto(t, &res)
	if res.Item.ID == 0 {
		t.Fatalf("#13 的 data.item.id 是 0，无法继续\n%s", truncate(string(r.Data)))
	}
	return res.Item
}

// ---------- 判据链 ----------

// dictNode 是 #7 / #8 返回的树节点，分类树和地点树共用这一个形状。
type dictNode struct {
	ID       int64       `json:"id"`
	Name     string      `json:"name"`
	Level    int         `json:"level"`
	Children []*dictNode `json:"children"`
}

// pickLeaf 从一棵字典树里挑一个「可以被选中」的叶子（没有子节点的那种）。
//
// 判据链里刻意不用本文件顶上那几个 catPhone / locLibrary 常量：这条链要证明的是
// 「字典接口给出来的 id 真的能拿去发帖」。写死常量证明不了这件事 ——
// 常量是对的、接口是坏的（比如返回了 parent_id 而不是 id），链照样全绿。
func pickLeaf(t *testing.T, path string) dictNode {
	t.Helper()

	r := harness.Get(t, path, "")
	RequireOK(t, r, "GET "+path)
	var roots []*dictNode
	r.DataInto(t, &roots)
	if len(roots) == 0 {
		t.Fatalf("%s 返回了一棵空树，后面没法发帖", path)
	}

	var leaf func(nodes []*dictNode) (dictNode, bool)
	leaf = func(nodes []*dictNode) (dictNode, bool) {
		for _, n := range nodes {
			if len(n.Children) == 0 {
				return *n, true
			}
			if found, ok := leaf(n.Children); ok {
				return found, true
			}
		}
		return dictNode{}, false
	}
	out, ok := leaf(roots)
	if !ok {
		t.Fatalf("%s 里一个叶子节点都没有", path)
	}
	return out
}

// TestM2CriterionChain 按 §12 里 M2 那一行的顺序把判据走一遍。
//
// 读这个函数就等于读判据；其余测试各自钉住每一步的形状和边界。
// 和单点测试的区别是：这里的**状态是连续的** —— ⑨ 删的是 ①②③ 建出来的那条帖子的图，
// ⑪ 关的是同一条帖子。单点测试每次都从 TruncateAll 重新开始，
// 所以「先关帖再改帖」这种跨步骤的状态机行为只有链能测到。
func TestM2CriterionChain(t *testing.T) {
	harness.TruncateAll(t)

	const password = "correct-horse-battery"
	me := harness.RegisterAndLogin(t, "chainowner", password)
	other := harness.RegisterAndLogin(t, "chainother", password)

	// ⓪ 字典树（#7 / #8）：发帖表单的数据源
	cat := pickLeaf(t, "/api/categories")
	loc := pickLeaf(t, "/api/locations")
	t.Logf("字典挑出来的是：分类 %d(%s, level %d)、地点 %d(%s, level %d)",
		cat.ID, cat.Name, cat.Level, loc.ID, loc.Name, loc.Level)

	withDict := func(mk func(title, contact string) map[string]any, title, contact string) map[string]any {
		b := mk(title, contact)
		b["category_id"] = cat.ID
		b["location_id"] = loc.ID
		return b
	}

	// ① 上传假图片（#6）。两张，因为 ⑨ 要「删掉其中一张、另一张还在」。
	first := upload(t, me, 1024)
	second := upload(t, me, 1024)
	if !service.IsUploadPath(first.Path) || first.URL != "/uploads/"+first.Path {
		t.Fatalf("#6 的返回形状不对：%+v", first)
	}

	// ② 建 lost（#13）。「时间 CHECK 生效」在这里表现为：两个具名时间列都落库了，
	//    而且是从 +08:00 转成 UTC 存的。CHECK 本身被 ⑦⑧ 直接撞。
	lostReq := withDict(lostBody, "黑色长款钱包丢在图书馆", "13800000000")
	lostReq["image_paths"] = []string{first.Path, second.Path}
	lost := createItem(t, me, lostReq)

	if lost.ItemType != "lost" || lost.Status != "open" {
		t.Errorf("② 建出来的 lost 帖不对：type=%q status=%q", lost.ItemType, lost.Status)
	}
	// 请求里写的是 2026-10-01T09:00:00+08:00，落库必须是同一个时刻的 UTC 表示
	if lost.LastSeenAt != "2026-10-01T01:00:00Z" || lost.LostAt != "2026-10-01T04:30:00Z" {
		t.Errorf("② 时间没有按 UTC 存：last_seen_at=%q lost_at=%q", lost.LastSeenAt, lost.LostAt)
	}
	if lost.FoundAt != "" {
		t.Errorf("② lost 帖不该有 found_at，实际 %q", lost.FoundAt)
	}
	if len(lost.Images) != 2 {
		t.Fatalf("② 应该挂上两张图，实际 %d 张", len(lost.Images))
	}
	if lost.Category.ID != cat.ID || lost.Location.ID != loc.ID {
		t.Errorf("② 字典外键不对：category=%+v location=%+v", lost.Category, lost.Location)
	}

	// ③ 建 found（#13）
	found := createItem(t, me, withDict(foundBody, "在图书馆捡到一个卡包", "问图书馆前台"))
	if found.ItemType != "found" || found.FoundAt == "" || found.LostAt != "" {
		t.Errorf("③ 建出来的 found 帖不对：type=%q found_at=%q lost_at=%q",
			found.ItemType, found.FoundAt, found.LostAt)
	}

	// ④ 列表带 keyword / category / location / status 筛选（#14）
	square := func(query string) itemPage {
		r := harness.Get(t, "/api/items?"+query, "")
		RequireOK(t, r, "GET /api/items?"+query)
		var p itemPage
		r.DataInto(t, &p)
		return p
	}
	for _, tc := range []struct {
		query string
		want  []int64
	}{
		{"keyword=" + urlQueryEscape("长款钱包"), []int64{lost.ID}},
		{"keyword=" + urlQueryEscape("交到前台"), []int64{found.ID}},
		{"keyword=" + urlQueryEscape("根本不存在"), nil},
		{"category_id=" + itoa(cat.ID), []int64{lost.ID, found.ID}},
		{"location_id=" + itoa(loc.ID), []int64{lost.ID, found.ID}},
		{"item_type=lost", []int64{lost.ID}},
		{"item_type=found", []int64{found.ID}},
		{"status=open", []int64{lost.ID, found.ID}},
		{"status=closed", nil},
	} {
		if got := idsOf(square(tc.query)); !sameIDs(got, tc.want) {
			t.Errorf("④ ?%s 期望 %v，实际 %v", tc.query, tc.want, got)
		}
	}
	// deleted 在广场上是**不允许请求**的状态（#19 才允许）
	requireField(t, harness.Get(t, "/api/items?status=deleted", ""), "status")

	// ⑤ contact 为空或纯空白得 VALIDATION；填「无」「问宿舍阿姨」都返回成功。
	//    这一格是整个 M2 里最容易被「顺手加个手机号校验」破坏的契约（§3.2）。
	before := harness.Count(t, `SELECT count(*) FROM items`)
	for _, blank := range []string{"", "   ", "\t\n", "\u3000", "\u00a0"} {
		r := harness.Post(t, "/api/items", withDict(lostBody, "联系方式是空白的", blank), me.Token)
		requireField(t, r, "contact")
	}
	if after := harness.Count(t, `SELECT count(*) FROM items`); after != before {
		t.Errorf("⑤ 校验失败了却还是建出了帖子：items 从 %d 行变成 %d 行", before, after)
	}
	for _, weird := range []string{"无", "问宿舍阿姨", "图书馆前台", "13800000000", "wx: abc-123"} {
		r := harness.Post(t, "/api/items", withDict(foundBody, "联系方式内容不做校验", weird), me.Token)
		RequireOK(t, r, "⑤ contact="+weird+" 必须成功（平台不校验联系方式的内容）")
	}

	// ⑥ 非本人改他人帖得 FORBIDDEN（#16）
	strangerPut := harness.Do(t, http.MethodPut, "/api/items/"+itoa(lost.ID),
		withDict(lostBody, "被别人改过的标题", "13900000000"), other.Token)
	RequireCode(t, strangerPut, apperr.CodeForbidden)
	if strangerPut.HTTPStatus != http.StatusForbidden {
		t.Errorf("⑥ 期望 HTTP 403，实际 %d", strangerPut.HTTPStatus)
	}
	if now := fetchDetail(t, lost.ID, me.Token); now.Title != "黑色长款钱包丢在图书馆" {
		t.Errorf("⑥ 帖子被改动了：title=%q", now.Title)
	}

	// ⑦ 故意填 last_seen_at > lost_at 得 VALIDATION
	reversed := withDict(lostBody, "时间填反了", "13800000000")
	reversed["last_seen_at"] = "2026-10-05T09:00:00+08:00"
	reversed["lost_at"] = "2026-10-01T09:00:00+08:00"
	requireField(t, harness.Post(t, "/api/items", reversed, me.Token), "last_seen_at")

	// ⑧ found 帖填 last_seen_at 被 CHECK 拦下。
	//
	// 分两半断言，因为它们是**两道独立的防线**：
	//   (a) API 层先拦，用户拿到的是带字段名的中文 VALIDATION；
	//   (b) 绕过 service 直接 INSERT，撞的是数据库的 items_time_semantics。
	// 只测 (a) 的话，CHECK 被某次迁移悄悄删掉也不会有人发现 ——
	// 而 CHECK 防的正是那些不走我们代码的写入（Adminer、M6 的管理员 SQL、未来的脚本）。
	mixed := withDict(foundBody, "found 帖填了 last_seen_at", "13800000000")
	mixed["last_seen_at"] = "2026-10-01T09:00:00+08:00"
	requireField(t, harness.Post(t, "/api/items", mixed, me.Token), "last_seen_at")

	_, err := harness.Pool.Exec(context.Background(),
		`INSERT INTO items (item_type, user_id, title, category_id, location_id, contact,
		                    last_seen_at, found_at)
		 VALUES ('found', $1, '绕过 service 的一行', $2, $3, '13800000000',
		         '2026-10-01T01:00:00Z', '2026-10-02T01:00:00Z')`,
		me.UserID, cat.ID, loc.ID)
	requireCheckViolation(t, err, "items_time_semantics")

	// ⑨ 帖主删自己帖子里的一张图（#42）→ item_images 少一行、磁盘文件消失、帖子本身还在
	target := lost.Images[0]
	absTarget := imageAbsPath(t, target.ID)
	if _, err := os.Stat(absTarget); err != nil {
		t.Fatalf("⑨ 删之前文件就该在 %s: %v", absTarget, err)
	}
	RequireOK(t, harness.Do(t, http.MethodDelete, "/api/item-images/"+itoa(target.ID), nil, me.Token),
		"DELETE /api/item-images/:id")

	if n := harness.Count(t, `SELECT count(*) FROM item_images WHERE item_id = $1`, lost.ID); n != 1 {
		t.Errorf("⑨ item_images 应该从 2 行变成 1 行，实际 %d 行", n)
	}
	if _, err := os.Stat(absTarget); !os.IsNotExist(err) {
		t.Errorf("⑨ 磁盘文件 %s 应该已经没了，stat 结果 %v", absTarget, err)
	}
	alive := fetchDetail(t, lost.ID, me.Token)
	if alive.ID != lost.ID || alive.Status != "open" {
		t.Errorf("⑨ 帖子本身不该被动过：%+v", alive)
	}
	if len(alive.Images) != 1 || alive.Images[0].ID != lost.Images[1].ID {
		t.Errorf("⑨ 应该只剩第二张图（id=%d），实际 %+v", lost.Images[1].ID, alive.Images)
	}

	// ⑩ 别人删这张图得 FORBIDDEN（⚠ admin 也不行，admin 删图是 #45 / M6）
	survivor := alive.Images[0]
	absSurvivor := imageAbsPath(t, survivor.ID)
	RequireCode(t, harness.Do(t, http.MethodDelete, "/api/item-images/"+itoa(survivor.ID), nil, other.Token),
		apperr.CodeForbidden)
	if n := harness.Count(t, `SELECT count(*) FROM item_images WHERE id = $1`, survivor.ID); n != 1 {
		t.Errorf("⑩ 那一行不该被删掉，实际 %d 行", n)
	}
	if _, err := os.Stat(absSurvivor); err != nil {
		t.Errorf("⑩ 文件不该被删掉: %v", err)
	}

	// ⑪ 改自己一条 closed 状态的帖子 → 成功（验证 ITEM_CLOSED 只挡 deleted）
	closeRes := harness.Do(t, http.MethodPatch, "/api/items/"+itoa(lost.ID)+"/status",
		map[string]any{"status": "closed"}, me.Token)
	RequireOK(t, closeRes, "PATCH status=closed")
	var closed statusResult
	closeRes.DataInto(t, &closed)
	if closed.Status != "closed" {
		t.Errorf("⑪ 关帖之后 status 应该是 closed，实际 %q", closed.Status)
	}

	edited := withDict(lostBody, "已经还回来了（电话换了）", "13900000000")
	upd := harness.Do(t, http.MethodPut, "/api/items/"+itoa(lost.ID), edited, me.Token)
	RequireOK(t, upd, "⑪ PUT 一条 closed 的帖子必须成功")
	var got itemView
	upd.DataInto(t, &got)
	if got.Status != "closed" {
		t.Errorf("⑪ 改帖不该顺带把状态改回去，实际 %q", got.Status)
	}
	if got.Contact == nil || *got.Contact != "13900000000" {
		t.Errorf("⑪ contact 应该改过来了，实际 %+v", got.Contact)
	}

	// 只有 deleted 才吃 ITEM_CLOSED
	harness.SetItemStatus(t, lost.ID, "deleted")
	gone := harness.Do(t, http.MethodPut, "/api/items/"+itoa(lost.ID), edited, me.Token)
	RequireCode(t, gone, apperr.CodeItemClosed)
	if gone.HTTPStatus != http.StatusConflict {
		t.Errorf("⑪ ITEM_CLOSED 期望 HTTP 409，实际 %d", gone.HTTPStatus)
	}
}

// ---------- #13 发帖 ----------

func TestM2CreateLostItem(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "lostowner", "correct-horse-battery")

	r := harness.Post(t, "/api/items", lostBody("黑色 iPhone 15", "13800000000"), me.Token)
	RequireOK(t, r, "POST /api/items (lost)")
	if r.HTTPStatus != http.StatusOK {
		t.Errorf("#13 成功应该是 HTTP 200，实际 %d", r.HTTPStatus)
	}
	got := createResultOf(t, r)

	if got.ItemType != "lost" {
		t.Errorf("item_type 应该是 lost，实际 %q", got.ItemType)
	}
	if got.Status != "open" {
		t.Errorf("新帖子应该是 open，实际 %q", got.Status)
	}
	if got.Category.ID != catPhone || got.Category.Name != "手机" {
		t.Errorf("category 应该是 {10, 手机}，实际 %+v", got.Category)
	}
	if got.Location.ID != locLibrary || got.Location.Name != "图书馆" {
		t.Errorf("location 应该是 {70, 图书馆}，实际 %+v", got.Location)
	}
	if got.Author.ID != me.UserID {
		t.Errorf("author.id 应该是发帖人 %d，实际 %d", me.UserID, got.Author.ID)
	}

	// 时间是 RFC3339 **且带 Z**（§3.1「一律存 UTC」，出口也统一成 UTC）。
	// 传上去的是 +08:00，回来还带着 +08:00 的话，前端就得猜这个偏移是谁的。
	for _, tc := range []struct {
		field string
		value string
		want  string
	}{
		{"last_seen_at", got.LastSeenAt, "2026-10-01T01:00:00Z"},
		{"lost_at", got.LostAt, "2026-10-01T04:30:00Z"},
		{"found_at", got.FoundAt, ""}, // lost 帖没有 found_at，是空串不是 null
	} {
		if tc.value != tc.want {
			t.Errorf("%s 应该是 %q，实际 %q", tc.field, tc.want, tc.value)
		}
	}

	// 自己的新帖子：contact 一定可见。
	if got.ContactLocked {
		t.Error("发帖人看自己的新帖子，contact_locked 不该是 true")
	}
	if got.Contact == nil || *got.Contact != "13800000000" {
		t.Errorf("发帖响应里应该带回 contact，实际 %+v", got.Contact)
	}
	if got.Images == nil {
		t.Error("images 应该是 [] 而不是 null（前端直接 .map）")
	}

	// 落库核对：数据库里的值必须和响应一致。
	row := harness.QueryRow(t, `SELECT item_type, status, category_id, location_id, contact,
	                                   last_seen_at, lost_at, found_at
	                            FROM items WHERE id = $1`, got.ID)
	if row["contact"] != "13800000000" {
		t.Errorf("库里的 contact 不对：%v", row["contact"])
	}
	if row["found_at"] != nil {
		t.Errorf("lost 帖的 found_at 应该是 NULL，实际 %v", row["found_at"])
	}
	if row["last_seen_at"] == nil || row["lost_at"] == nil {
		t.Errorf("lost 帖的两个时间都该有值：%v / %v", row["last_seen_at"], row["lost_at"])
	}
}

func TestM2CreateFoundItem(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "foundowner", "correct-horse-battery")

	got := createItem(t, me, foundBody("捡到一个黑色钱包", "问图书馆前台"))

	if got.ItemType != "found" {
		t.Errorf("item_type 应该是 found，实际 %q", got.ItemType)
	}
	if got.FoundAt != "2026-10-02T07:00:00Z" {
		t.Errorf("found_at 应该是 2026-10-02T07:00:00Z（15:00+08:00 转 UTC），实际 %q", got.FoundAt)
	}
	if got.LastSeenAt != "" || got.LostAt != "" {
		t.Errorf("found 帖不该有两个 lost 时间，实际 last_seen_at=%q lost_at=%q", got.LastSeenAt, got.LostAt)
	}

	// #13 的响应是给**发帖人自己**的，所以 contact 可见。
	// 「found 帖的联系方式锁着」是 #15 给**别人**看时的规则，不是入库规则。
	if got.ContactLocked {
		t.Error("#13 响应里 contact_locked 不该是 true（这是发帖人自己的帖子）")
	}
	if got.Contact == nil || *got.Contact != "问图书馆前台" {
		t.Errorf("#13 响应里应该带回 contact，实际 %+v", got.Contact)
	}

	row := harness.QueryRow(t, `SELECT last_seen_at, lost_at, found_at FROM items WHERE id = $1`, got.ID)
	if row["last_seen_at"] != nil || row["lost_at"] != nil {
		t.Errorf("found 帖的 last_seen_at / lost_at 必须是 NULL，实际 %v / %v",
			row["last_seen_at"], row["lost_at"])
	}
}

// TestM2CreateWithImages 验图片从 #6 一路走到 #15。
//
// 三个端点在这里串起来：#6 返回 path → #13 把 path 写进 item_images →
// #15 把每一行拼成 {id, url, sort_order} 返回。id 必须在响应里，
// 因为 #42 删单张图走的就是它。
func TestM2CreateWithImages(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "withpics", "correct-horse-battery")

	first := upload(t, me, 1024)
	second := upload(t, me, 2048)

	body := lostBody("带图的钱包", "13800000000")
	body["image_paths"] = []string{first.Path, second.Path}
	got := createItem(t, me, body)

	if len(got.Images) != 2 {
		t.Fatalf("应该有 2 张图，实际 %d 张", len(got.Images))
	}
	// 顺序即 sort_order，即请求里 image_paths 的顺序 —— 前端靠它决定哪张是封面。
	if got.Images[0].URL != first.URL || got.Images[1].URL != second.URL {
		t.Errorf("图片顺序不对：期望 [%s, %s]，实际 [%s, %s]",
			first.URL, second.URL, got.Images[0].URL, got.Images[1].URL)
	}
	for i, im := range got.Images {
		if im.SortOrder != i {
			t.Errorf("第 %d 张图的 sort_order 应该是 %d，实际 %d", i, i, im.SortOrder)
		}
		if im.ID == 0 {
			t.Errorf("第 %d 张图的 id 是 0 —— #42 删图靠这个 id，不能没有", i)
		}
	}
	if n := harness.Count(t, `SELECT count(*) FROM item_images WHERE item_id = $1`, got.ID); n != 2 {
		t.Errorf("item_images 应该有 2 行，实际 %d 行", n)
	}

	// #15 读回来的图片必须和 #13 返回的一模一样。
	detail := fetchDetail(t, got.ID, me.Token)
	if len(detail.Images) != 2 || detail.Images[0].ID != got.Images[0].ID {
		t.Errorf("#15 返回的图片和 #13 不一致：%+v vs %+v", detail.Images, got.Images)
	}
}

// TestM2CreateRejectsBadImagePaths 钉住 image_paths 的白名单校验。
//
// 这一条是安全边界而不只是格式校验：这些字符串会被原样写进 item_images.path，
// 之后拼成 /uploads/<path> 返给前端，删图时还会拿它去 os.Remove。
// 不校验形状的话，"../../.env" 就是一个指向 uploads 目录之外的路径。
func TestM2CreateRejectsBadImagePaths(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "badpaths", "correct-horse-battery")
	real := upload(t, me, 256)

	cases := []struct {
		name  string
		paths []string
		field string
	}{
		{"路径穿越", []string{"../../.env"}, "image_paths[0]"},
		{"绝对路径", []string{"/etc/passwd"}, "image_paths[0]"},
		{"反斜杠", []string{`..\..\secret.txt`}, "image_paths[0]"},
		{"HTML 注入", []string{"<script>alert(1)</script>"}, "image_paths[0]"},
		{"可执行扩展名", []string{"2026/10/0123456789abcdef0123456789abcdef.php"}, "image_paths[0]"},
		{"随便一个字符串", []string{"photo.png"}, "image_paths[0]"},
		{"合法路径混在非法路径后面", []string{real.Path, "nope"}, "image_paths[1]"},
		{"重复路径", []string{real.Path, real.Path}, "image_paths[1]"},
		{"超过 9 张", tenPaths(), "image_paths"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := lostBody("图片路径不合法", "13800000000")
			body["image_paths"] = tc.paths
			r := harness.Post(t, "/api/items", body, me.Token)
			requireField(t, r, tc.field)
			if n := harness.Count(t, `SELECT count(*) FROM items WHERE title = $1`,
				"图片路径不合法"); n != 0 {
				t.Errorf("校验失败了却还是建出了 %d 条帖子", n)
			}
		})
	}
}

func tenPaths() []string {
	out := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		// 32 位十六进制的合法形状，只有最后一位不同 —— 数量上限要在
		// 形状校验**之前**被撞到，否则这条用例测的是形状而不是数量。
		out = append(out, "2026/10/0123456789abcdef0123456789abcde"+strconv.FormatInt(int64(i), 16)+".jpg")
	}
	return out
}

// TestM2CreateRejectsBadDictRefs 钉住 §3.2 的两条引用规则。
//
//	category_id 必须是**小类**（level 2）：分类的半分机制只在两级都确定时才算得出来，
//	  允许选大类的话「数码电子」和「手机」之间就没法判断是不是同一个小类。
//	location_id 必须是**叶子节点**：叶子的定义是「没有子节点」而不是「level=3」，
//	  因为「其他」是 level=1 且 is_freeform=true 的一级叶子，它必须能被选中。
//
// ⚠ 全部报 VALIDATION，**没有一条报 NOT_FOUND**：对发帖表单来说
// 「你选的分类不存在」是一个字段错误（前端要把焦点移回选择器），
// 而 NOT_FOUND 会被前端那个「404 就跳首页」的拦截器接走，把用户从填了一半的表单里踢出去。
func TestM2CreateRejectsBadDictRefs(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "badrefs", "correct-horse-battery")

	cases := []struct {
		name   string
		mutate func(body map[string]any)
		field  string
	}{
		{"分类选到大类", func(b map[string]any) { b["category_id"] = catRoot }, "category_id"},
		{"分类不存在", func(b map[string]any) { b["category_id"] = 999999 }, "category_id"},
		{"分类是 0", func(b map[string]any) { b["category_id"] = 0 }, "category_id"},
		{"地点选到中间层", func(b map[string]any) { b["location_id"] = locHall }, "location_id"},
		{"地点不存在", func(b map[string]any) { b["location_id"] = 999999 }, "location_id"},
		{"地点是 0", func(b map[string]any) { b["location_id"] = 0 }, "location_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := lostBody("字典引用不合法", "13800000000")
			tc.mutate(body)
			requireField(t, harness.Post(t, "/api/items", body, me.Token), tc.field)
		})
	}

	// 反向：「其他」这个一级叶子必须能选中。这条如果坏了，
	// 用户在字典里找不到对应地点时就彻底发不了帖 —— 而从字典树接口上完全看不出来。
	t.Run("一级叶子「其他」可以选", func(t *testing.T) {
		body := foundBody("在其他地方捡到的", "13800000000")
		body["location_id"] = locOther
		body["location_detail"] = "体育馆东门外"
		got := createItem(t, me, body)
		if got.Location.ID != locOther || got.Location.Name != "其他" {
			t.Errorf("location 应该是 {3, 其他}，实际 %+v", got.Location)
		}
	})
}

// TestM2ContactIsNeverValidated 是 M2 判据里写得最重的一条，也是最容易被
// 「顺手加个校验」破坏的一条。
//
// 计划 §3.2：contact **只校验非空，不校验内容**。
// 明确不做的三件事：格式校验、占位值黑名单、真实性/敏感词过滤。
// 理由（定位原则 1）：平台一旦开始校验，就要为校验结果背书 ——
// 「你通过了我的校验，所以这个联系方式是真的」。而 lost+found 本质是靠自觉维持的，
// 填错的后果由填错的人自己承担：没人联系得上他，他的东西就还不了。
// 这个反馈闭环不需要平台插手。
//
// 判据原文要求验证「填『无』『问宿舍阿姨』都返回成功」，所以那两条在 must 里。
func TestM2ContactIsNeverValidated(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "contactrule", "correct-horse-battery")

	must := []struct {
		name    string
		contact string
	}{
		{"判据点名的「无」", "无"},
		{"判据点名的「问宿舍阿姨」", "问宿舍阿姨"},
		{"一个占位词", "图书馆前台"},
		{"看起来不像手机号的数字", "1380000"},
		{"纯字母", "abc"},
		{"手机号", "13800138000"},
		{"带分隔的手机号", "138-0013-8000"},
		{"微信号", "wx: zhang_san-2026"},
		{"邮箱", "someone@example.com"},
		{"QQ 群", "QQ群 123456789"},
		{"一句完整的话", "打我电话，打不通就去宿管那里问"},
		{"HTML（存进去，前端转义）", "<script>alert(1)</script>"},
		{"表情", "联系我 🙏"},
		{"首尾带空白（会被 trim）", "  13800138000  "},
		{"正好 100 个字符", strings.Repeat("联", 100)},
	}
	for _, tc := range must {
		t.Run("接受 "+tc.name, func(t *testing.T) {
			got := createItem(t, me, lostBody("联系方式："+tc.name, tc.contact))
			want := strings.TrimSpace(tc.contact)
			if got.Contact == nil || *got.Contact != want {
				t.Errorf("contact 应该原样存下来（%q），实际 %+v", want, got.Contact)
			}
			db := harness.QueryRow(t, `SELECT contact FROM items WHERE id = $1`, got.ID)
			if db["contact"] != want {
				t.Errorf("库里的 contact 是 %v，应该是 %q", db["contact"], want)
			}
		})
	}

	// 唯一的约束：去首尾空白后非空。
	// 空白字符集必须和数据库 CHECK 的 btrim 一致 —— 尤其是 U+3000，
	// 中文输入法下按空格出来的就是它，只 trim ASCII 空格的话它能溜过去。
	blank := []struct {
		name    string
		contact string
	}{
		{"空串", ""},
		{"一个空格", " "},
		{"多个空格", "     "},
		{"制表符", "\t"},
		{"换行", "\n"},
		{"回车换行", "\r\n"},
		{"不换行空格 U+00A0", "\u00a0"},
		{"全角空格 U+3000", "\u3000"},
		{"混合空白", " \t\u3000\n\u00a0 "},
	}
	for _, tc := range blank {
		t.Run("拒绝 "+tc.name, func(t *testing.T) {
			requireField(t, harness.Post(t, "/api/items", lostBody("空联系方式", tc.contact), me.Token), "contact")
		})
	}

	t.Run("拒绝超过 100 个字符", func(t *testing.T) {
		requireField(t, harness.Post(t, "/api/items",
			lostBody("联系方式太长", strings.Repeat("联", 101)), me.Token), "contact")
	})
}

// TestM2TimeSemanticsRejectedByAPI 钉住三个时间字段的跨字段规则。
//
// 这些规则数据库也有一条 items_time_semantics CHECK 兜底，但**必须在 app 层先查**：
// CHECK 被撞到时 pgx 抛出来的是英文技术细节，用户看不出该改哪个字段；
// 这里的错误是带字段名的中文，能直接显示在表单上。
func TestM2TimeSemanticsRejectedByAPI(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "timesem", "correct-horse-battery")

	type timeCase struct {
		name   string
		mutate func(body map[string]any)
		field  string
	}

	lostCases := []timeCase{
		// 判据点名的一条：last_seen_at > lost_at。
		{"last_seen_at 晚于 lost_at", func(b map[string]any) {
			b["last_seen_at"] = "2026-10-05T09:00:00+08:00"
			b["lost_at"] = "2026-10-01T09:00:00+08:00"
		}, "last_seen_at"},
		{"缺 last_seen_at", func(b map[string]any) { b["last_seen_at"] = "" }, "last_seen_at"},
		{"缺 lost_at", func(b map[string]any) { b["lost_at"] = "" }, "lost_at"},
		{"两个时间都缺", func(b map[string]any) { b["last_seen_at"] = ""; b["lost_at"] = "" }, "last_seen_at"},
		{"填了 found_at", func(b map[string]any) {
			b["found_at"] = "2026-10-03T09:00:00+08:00"
		}, "found_at"},
		{"lost_at 格式不是 RFC3339", func(b map[string]any) {
			b["lost_at"] = "2026-10-01 12:30"
		}, "lost_at"},
		// M7 的 <input type="datetime-local"> 产出的就是这个形状，
		// 前端必须先 new Date(v).toISOString()。这里刻意不宽容解析：
		// 接受多种格式意味着同一个时刻有多种写法，而写错时区的那一种
		// 会静默地把时间挪 8 小时，匹配算法随后给出完全错误的结果。
		{"datetime-local 的形状", func(b map[string]any) {
			b["lost_at"] = "2026-10-01T12:30"
		}, "lost_at"},
		{"时间是个乱七八糟的字符串", func(b map[string]any) {
			b["last_seen_at"] = "上周三"
		}, "last_seen_at"},
	}
	for _, tc := range lostCases {
		t.Run("lost · "+tc.name, func(t *testing.T) {
			body := lostBody("时间不对", "13800000000")
			tc.mutate(body)
			requireField(t, harness.Post(t, "/api/items", body, me.Token), tc.field)
		})
	}

	foundCases := []timeCase{
		// 判据点名的另一条。API 这一层就挡住了，
		// 数据库的 CHECK 是第二道防线（TestM2TimeCheckIsEnforcedByDatabase）。
		{"填了 last_seen_at", func(b map[string]any) {
			b["last_seen_at"] = "2026-10-01T09:00:00+08:00"
		}, "last_seen_at"},
		{"填了 lost_at", func(b map[string]any) {
			b["lost_at"] = "2026-10-01T09:00:00+08:00"
		}, "lost_at"},
		{"缺 found_at", func(b map[string]any) { b["found_at"] = "" }, "found_at"},
		{"found_at 格式不对", func(b map[string]any) { b["found_at"] = "昨天下午" }, "found_at"},
	}
	for _, tc := range foundCases {
		t.Run("found · "+tc.name, func(t *testing.T) {
			body := foundBody("时间不对", "13800000000")
			tc.mutate(body)
			requireField(t, harness.Post(t, "/api/items", body, me.Token), tc.field)
		})
	}

	// 边界：两个时间**相等**必须放行。「9:00 还拿着，9:00 发现没了」
	// 是一个完全合理的陈述（比如「我下课时发现它不在包里了，下课是 9 点」）。
	// 判的是 lastSeen.After(lost) 而不是 !lastSeen.Before(lost)，正是为了让这一条过。
	t.Run("lost · 两个时间相等放行", func(t *testing.T) {
		body := lostBody("时间相等", "13800000000")
		body["last_seen_at"] = "2026-10-01T09:00:00+08:00"
		body["lost_at"] = "2026-10-01T09:00:00+08:00"
		got := createItem(t, me, body)
		if got.LastSeenAt != got.LostAt {
			t.Errorf("两个时间应该相等，实际 %q / %q", got.LastSeenAt, got.LostAt)
		}
	})

	// 跨时区：+08:00 和 Z 指的是同一个时刻，必须都能接受，而且存下来是同一个值。
	t.Run("lost · 跨时区写法等价", func(t *testing.T) {
		a := lostBody("跨时区 A", "13800000000")
		a["last_seen_at"] = "2026-10-01T09:00:00+08:00"
		b := lostBody("跨时区 B", "13800000000")
		b["last_seen_at"] = "2026-10-01T01:00:00Z"
		gotA, gotB := createItem(t, me, a), createItem(t, me, b)
		if gotA.LastSeenAt != gotB.LastSeenAt {
			t.Errorf("同一个时刻的两种写法应该存成一样的值：%q vs %q", gotA.LastSeenAt, gotB.LastSeenAt)
		}
	})

	// item_type 本身也要查：请求体里没带、大小写不对、或者带了个别的值。
	for _, bad := range []string{"", "LOST", "Lost", "found ", "claim", "lostfound", " lost"} {
		t.Run("item_type="+strconv.Quote(bad), func(t *testing.T) {
			body := lostBody("类型不对", "13800000000")
			body["item_type"] = bad
			requireField(t, harness.Post(t, "/api/items", body, me.Token), "item_type")
		})
	}
}

// TestM2TimeCheckIsEnforcedByDatabase 直接撞数据库的 CHECK。
//
// 判据里写的是「found 帖填 last_seen_at **被 CHECK 拦下**」—— 而 API 那条路
// 永远走不到 CHECK（service 先挡住了），所以想验 CHECK 就必须绕过 service。
//
// 这不是多余的测试。CHECK 是最后一道防线，它要防的是**不经过我们代码的写入**：
// 从 Adminer 里手改一行、M6 的 admin 端点写错了 SQL、将来的数据迁移脚本。
// 而一道没被测过的防线，和一道不存在的防线没有区别 ——
// 写迁移的时候少打一个括号，CHECK 就静默地变成了恒真。
func TestM2TimeCheckIsEnforcedByDatabase(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "dbcheck", "correct-horse-battery")

	const insert = `INSERT INTO items
	    (item_type, user_id, title, category_id, location_id, contact,
	     last_seen_at, lost_at, found_at)
	    VALUES ($1, $2, 'db check', $3, $4, '13800000000', $5, $6, $7)`

	cases := []struct {
		name       string
		itemType   string
		lastSeen   any
		lost       any
		found      any
		constraint string
	}{
		// 判据点名的那一条
		{"found 帖填了 last_seen_at", "found", "2026-10-01T01:00:00Z", nil, "2026-10-02T01:00:00Z", "items_time_semantics"},
		{"found 帖填了 lost_at", "found", nil, "2026-10-01T01:00:00Z", "2026-10-02T01:00:00Z", "items_time_semantics"},
		{"found 帖没有 found_at", "found", nil, nil, nil, "items_time_semantics"},
		{"lost 帖没有 last_seen_at", "lost", nil, "2026-10-01T01:00:00Z", nil, "items_time_semantics"},
		{"lost 帖没有 lost_at", "lost", "2026-10-01T01:00:00Z", nil, nil, "items_time_semantics"},
		{"lost 帖填了 found_at", "lost", "2026-10-01T01:00:00Z", "2026-10-01T02:00:00Z", "2026-10-03T01:00:00Z", "items_time_semantics"},
		{"lost 帖 last_seen_at 晚于 lost_at", "lost", "2026-10-05T01:00:00Z", "2026-10-01T01:00:00Z", nil, "items_time_semantics"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := harness.Pool.Exec(context.Background(), insert,
				tc.itemType, me.UserID, catPhone, locLibrary, tc.lastSeen, tc.lost, tc.found)
			requireCheckViolation(t, err, tc.constraint)
		})
	}

	// 反向：两种类型的**合法**组合必须能插进去。
	// 少了这一半，把 CHECK 改成恒假也能让上面全绿。
	t.Run("合法的 lost 行能插进去", func(t *testing.T) {
		tag, err := harness.Pool.Exec(context.Background(), insert,
			"lost", me.UserID, catPhone, locLibrary,
			"2026-10-01T01:00:00Z", "2026-10-01T02:00:00Z", nil)
		if err != nil {
			t.Fatalf("合法的 lost 行插入失败: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Errorf("应该插入 1 行，实际 %d 行", tag.RowsAffected())
		}
	})
	t.Run("合法的 found 行能插进去", func(t *testing.T) {
		tag, err := harness.Pool.Exec(context.Background(), insert,
			"found", me.UserID, catWallet, locLibrary, nil, nil, "2026-10-02T01:00:00Z")
		if err != nil {
			t.Fatalf("合法的 found 行插入失败: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Errorf("应该插入 1 行，实际 %d 行", tag.RowsAffected())
		}
	})

	// contact / title 的非空 CHECK 是另外两条独立的约束。
	// 它们防的是同一件事：app 层的 TrimSpace 漏掉了某种空白字符。
	// U+3000 是最容易漏的那个 —— 中文输入法下按空格出来的就是它，
	// 而单参数版本的 btrim(x) **只删 ASCII 空格**，制表符和全角空格都会留下。
	blankCases := []struct {
		name       string
		title      string
		contact    string
		constraint string
	}{
		{"contact 是空串", "合法的标题", "", "items_contact_not_blank"},
		{"contact 是空格", "合法的标题", "   ", "items_contact_not_blank"},
		{"contact 是全角空格 U+3000", "合法的标题", "\u3000", "items_contact_not_blank"},
		{"contact 是不换行空格 U+00A0", "合法的标题", "\u00a0", "items_contact_not_blank"},
		{"contact 是制表符和换行", "合法的标题", "\t\n", "items_contact_not_blank"},
		{"title 是空串", "", "13800000000", "items_title_not_blank"},
		{"title 是全角空格", "\u3000", "13800000000", "items_title_not_blank"},
	}
	for _, tc := range blankCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := harness.Pool.Exec(context.Background(),
				`INSERT INTO items (item_type, user_id, title, category_id, location_id, contact, found_at)
				 VALUES ('found', $1, $2, $3, $4, $5, '2026-10-02T01:00:00Z')`,
				me.UserID, tc.title, catWallet, locLibrary, tc.contact)
			requireCheckViolation(t, err, tc.constraint)
		})
	}

	// item_type / status 的枚举 CHECK
	_, err := harness.Pool.Exec(context.Background(),
		`INSERT INTO items (item_type, user_id, title, category_id, location_id, contact, found_at)
		 VALUES ('claim', $1, 'x', $2, $3, 'c', '2026-10-02T01:00:00Z')`,
		me.UserID, catWallet, locLibrary)
	requireCheckViolation(t, err, "items_item_type_check")

	_, err = harness.Pool.Exec(context.Background(),
		`INSERT INTO items (item_type, user_id, title, category_id, location_id, contact, found_at, status)
		 VALUES ('found', $1, 'x', $2, $3, 'c', '2026-10-02T01:00:00Z', 'archived')`,
		me.UserID, catWallet, locLibrary)
	requireCheckViolation(t, err, "items_status_check")
}

// requireCheckViolation 断言一个错误是 PG 的 CHECK 违例（SQLSTATE 23514），
// 并且是**指定那条**约束报出来的。
//
// 断言约束名而不只断言 23514：一张表上可以有好几条 CHECK，
// 只查状态码的话「contact 的约束挂了、被 title 的约束先撞上」也照样绿。
func requireCheckViolation(t *testing.T, err error, constraint string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望被 CHECK %q 拦下，但插入成功了", constraint)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("期望一个 PG 错误，实际是 %T: %v", err, err)
	}
	if pgErr.Code != "23514" {
		t.Fatalf("期望 SQLSTATE 23514（check_violation），实际 %s：%s", pgErr.Code, pgErr.Message)
	}
	if pgErr.ConstraintName != constraint {
		t.Errorf("违例的约束应该是 %q，实际是 %q", constraint, pgErr.ConstraintName)
	}
}

// ---------- #15 详情 ----------

func fetchDetail(t *testing.T, id int64, token string) itemView {
	t.Helper()
	r := harness.Get(t, "/api/items/"+itoa(id), token)
	RequireOK(t, r, "GET /api/items/"+itoa(id))
	var v itemView
	r.DataInto(t, &v)
	return v
}

// TestM2DetailContactVisibility 钉住 §4「#15 的 contact 可见性规则」。
//
//	if item.item_type == 'lost':       不锁     # lost 帖联系方式公开
//	elif 当前用户是发帖人:              不锁     # 自己的帖子当然能看
//	elif 已登录且 contact_views 有记录:  不锁     # ← M4 才接上
//	else:                              锁
//
// M2 只有前两条（第三条要等 #21 解锁接口）。但形状现在就定死：
// contact 是 **JSON null** 而不是空串 —— 用空串表示「锁着」是错的，
// 前端分不清「锁着」和「这人填了个空串」，而 §3.2 禁止空串入库，
// 所以空串在业务上根本不该存在。
func TestM2DetailContactVisibility(t *testing.T) {
	harness.TruncateAll(t)
	owner := harness.RegisterAndLogin(t, "owner", "correct-horse-battery")
	other := harness.RegisterAndLogin(t, "other", "correct-horse-battery")

	const lostContact = "13800000000"
	const foundContact = "问图书馆前台"
	lost := createItem(t, owner, lostBody("丢了一台 iPhone", lostContact))
	found := createItem(t, owner, foundBody("捡到一个钱包", foundContact))

	// wantContact 是**变量**而不是常量：表格里要取它的地址来和响应里的
	// *string 比。用 &"字面量" 编译不过，而把两个常量提升成变量
	// 比在每个 case 里重抄一遍字符串更不容易写错。
	lostVisible, foundVisible := lostContact, foundContact

	cases := []struct {
		name        string
		itemID      int64
		token       string // "" = 未登录
		wantContact *string
		wantLocked  bool
	}{
		{"lost 帖 · 未登录", lost.ID, "", &lostVisible, false},
		{"lost 帖 · 陌生人", lost.ID, other.Token, &lostVisible, false},
		{"lost 帖 · 本人", lost.ID, owner.Token, &lostVisible, false},
		{"found 帖 · 未登录", found.ID, "", nil, true},
		{"found 帖 · 陌生人", found.ID, other.Token, nil, true},
		{"found 帖 · 本人", found.ID, owner.Token, &foundVisible, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fetchDetail(t, tc.itemID, tc.token)

			if got.ContactLocked != tc.wantLocked {
				t.Errorf("contact_locked 应该是 %v，实际 %v", tc.wantLocked, got.ContactLocked)
			}
			switch {
			case tc.wantContact == nil && got.Contact != nil:
				t.Errorf("contact 应该是 JSON null，实际是 %q —— 锁着的联系方式一个字都不该出现", *got.Contact)
			case tc.wantContact != nil && got.Contact == nil:
				t.Errorf("contact 应该是 %q，实际是 null", *tc.wantContact)
			case tc.wantContact != nil && *got.Contact != *tc.wantContact:
				t.Errorf("contact 应该是 %q，实际 %q", *tc.wantContact, *got.Contact)
			}

			// 锁着的时候，那个字符串**在整个响应体里都不能出现**。
			// 只查 contact 字段是不够的：万一哪天有人把它顺手塞进了
			// description 或者一个调试字段，字段级断言发现不了。
			if tc.wantLocked {
				r := harness.Get(t, "/api/items/"+itoa(tc.itemID), tc.token)
				if strings.Contains(string(r.Data), foundContact) {
					t.Errorf("锁着的联系方式出现在了响应体的别处：\n%s", truncate(string(r.Data)))
				}
			}
		})
	}
}

// TestM2DetailIsPublic 钉住 #15 是公开接口。
//
// 它挂的是 OptionalJWT 而不是 JWT：帖子内容对所有人开放，只有联系方式是锁着的。
// 挂成 JWT 的话，未登录的人连「这个钱包长什么样」都看不到，
// 而 §2.4 明确允许匿名浏览广场并点进详情。
//
// 反过来，**无效 token 也不能 401** —— OptionalJWT 的语义是「认不出来就当匿名」。
// 一个 token 过期的用户点开一条帖子，应该看到帖子内容（联系方式锁着），
// 而不是被一脚踢回登录页：他可能只是想看一眼这东西是不是自己的。
func TestM2DetailIsPublic(t *testing.T) {
	harness.TruncateAll(t)
	owner := harness.RegisterAndLogin(t, "publicdetail", "correct-horse-battery")
	item := createItem(t, owner, foundBody("公开可见的拾物帖", "13800000000"))

	t.Run("未登录能读", func(t *testing.T) {
		r := harness.Get(t, "/api/items/"+itoa(item.ID), "")
		RequireOK(t, r, "匿名 GET /api/items/:id")
	})
	t.Run("无效 token 当匿名处理", func(t *testing.T) {
		r := harness.Get(t, "/api/items/"+itoa(item.ID), "not-a-real-token")
		RequireOK(t, r, "带无效 token 的 GET /api/items/:id")
		var v itemView
		r.DataInto(t, &v)
		if !v.ContactLocked {
			t.Error("认不出身份就应该当匿名，found 帖的 contact 必须锁着")
		}
	})
	t.Run("被封禁的用户也当匿名处理", func(t *testing.T) {
		banned := harness.RegisterAndLogin(t, "bannedreader", "correct-horse-battery")
		harness.Ban(t, banned.UserID)
		r := harness.Get(t, "/api/items/"+itoa(item.ID), banned.Token)
		// OptionalJWT 里查不到用户（或查到是 banned）就退回匿名，不 401。
		// 这条断言的是「公开接口不会因为身份有问题而拒绝提供公开内容」。
		RequireOK(t, r, "被封禁用户读公开帖子")
		var v itemView
		r.DataInto(t, &v)
		if !v.ContactLocked {
			t.Error("被封禁的用户不该解锁任何联系方式")
		}
	})
}

// TestM2DetailNotFound 钉住 #15 的 NOT_FOUND 分支。
//
// 四个来源，全部报 NOT_FOUND：
//   - id 不是数字 / 不是正整数（handler.pathID）
//   - 库里没有这一行（repo）
//   - 这一行被软删了，而读的人不是作者也不是 admin（service.canSeeDeleted）
//
// 第三条刻意报 NOT_FOUND 而不是 FORBIDDEN：返回 403 等于确认了
// 「这个 id 曾经有过一条被删的帖子」—— 那是治理信息。
// 同一条纪律也用在登录接口上（不区分「用户名不存在」和「密码错」）。
func TestM2DetailNotFound(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "notfounder", "correct-horse-battery")
	stranger := harness.RegisterAndLogin(t, "notfoundstranger", "correct-horse-battery")

	// 注意这里**没有** "" 那一项：GET /api/items/ 会被 gin 的
	// RedirectTrailingSlash 301 到 /api/items（列表接口），那是 gin 的默认行为，
	// 和 #15 无关。真要钉它得单独测「重定向到了哪里」，而不是测 code。
	for _, raw := range []string{"abc", "0", "-1", "1.5", "99999999", "1e3", " 1"} {
		t.Run("id="+raw, func(t *testing.T) {
			RequireCode(t, harness.Get(t, "/api/items/"+raw, me.Token), apperr.CodeNotFound)
		})
	}

	t.Run("软删之后对陌生人是 NOT_FOUND", func(t *testing.T) {
		item := createItem(t, me, lostBody("即将被删的帖子", "13800000000"))
		RequireOK(t, harness.Do(t, http.MethodDelete, "/api/items/"+itoa(item.ID), nil, me.Token), "DELETE")

		r := harness.Get(t, "/api/items/"+itoa(item.ID), stranger.Token)
		RequireCode(t, r, apperr.CodeNotFound)
		if r.HTTPStatus != http.StatusNotFound {
			t.Errorf("期望 HTTP 404，实际 %d", r.HTTPStatus)
		}
		RequireCode(t, harness.Get(t, "/api/items/"+itoa(item.ID), ""), apperr.CodeNotFound)
	})

	t.Run("软删之后本人和 admin 还能看见", func(t *testing.T) {
		item := createItem(t, me, lostBody("被删但本人可见", "13800000000"))
		RequireOK(t, harness.Do(t, http.MethodDelete, "/api/items/"+itoa(item.ID), nil, me.Token), "DELETE")

		if got := fetchDetail(t, item.ID, me.Token); got.Status != "deleted" {
			t.Errorf("本人应该看到 status=deleted，实际 %q", got.Status)
		}
		admin := harness.MakeAdmin(t, stranger)
		if got := fetchDetail(t, item.ID, admin.Token); got.Status != "deleted" {
			t.Errorf("admin 应该看到 status=deleted，实际 %q", got.Status)
		}
	})
}

// TestM2DetailAuthorShapeIsMinimal 钉住 author 只有 {id, nickname}。
//
// ⚠ 没有 real_name。#15 是公开接口，未登录也能访问，而 real_name 是 SSO 带来的
// 真实姓名 —— 把它挂在一个人人可读的页面上，等于替全体用户做了一次实名公示。
// 计划里 real_name 只出现在 #22（解锁名单，仅发帖人和 admin 可见）那种
// 「你确实需要知道对方是谁」的场合。
//
// 也没有 credit_score（M5 才有意义，而且那是给「要不要相信这个人」用的，
// 公开详情页上挂一个信用分等于让平台替一次交易背书）。
//
// 这条断言查的是**原始 JSON**，不是解码后的结构体：结构体只会读到声明过的字段，
// 多出来的字段它一声不响地丢掉 —— 而那正是隐私泄漏会走的路。
func TestM2DetailAuthorShapeIsMinimal(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "minimalauthor", "correct-horse-battery")
	item := createItem(t, me, lostBody("作者信息最小化", "13800000000"))

	r := harness.Get(t, "/api/items/"+itoa(item.ID), "")
	RequireOK(t, r, "GET /api/items/:id")

	var raw struct {
		Author map[string]json.RawMessage `json:"author"`
	}
	r.DataInto(t, &raw)
	if len(raw.Author) != 2 {
		keys := make([]string, 0, len(raw.Author))
		for k := range raw.Author {
			keys = append(keys, k)
		}
		t.Errorf("author 应该只有 id 和 nickname 两个字段，实际有 %v", keys)
	}
	for _, forbidden := range []string{"real_name", "credit_score", "username", "student_id", "phone", "email"} {
		if _, ok := raw.Author[forbidden]; ok {
			t.Errorf("author 里出现了 %s —— 这个字段不该出现在公开详情页上", forbidden)
		}
	}
}

// ---------- #16 改帖 ----------

func TestM2UpdateOwnItem(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "updater", "correct-horse-battery")
	item := createItem(t, me, lostBody("改之前的标题", "13800000000"))

	body := lostBody("改之后的标题", "13900000000")
	body["description"] = "补充：手机壳是透明的"
	body["category_id"] = catWallet
	body["location_id"] = locOther
	body["location_detail"] = "体育馆东门"
	r := harness.Do(t, http.MethodPut, "/api/items/"+itoa(item.ID), body, me.Token)
	RequireOK(t, r, "PUT /api/items/:id")

	var got itemView
	r.DataInto(t, &got)
	if got.ID != item.ID {
		t.Errorf("改的应该是同一条帖子：%d → %d", item.ID, got.ID)
	}
	if got.Title != "改之后的标题" {
		t.Errorf("title 没改过来：%q", got.Title)
	}
	if got.Category.ID != catWallet {
		t.Errorf("category 没改过来：%+v", got.Category)
	}
	if got.Location.ID != locOther {
		t.Errorf("location 没改过来：%+v", got.Location)
	}
	if got.ItemType != "lost" {
		t.Errorf("item_type 不该变（请求体里根本没有这个字段），实际 %q", got.ItemType)
	}
	if got.Author.ID != me.UserID {
		t.Errorf("作者不该变，实际 %d", got.Author.ID)
	}
	if got.CreatedAt != item.CreatedAt {
		t.Errorf("created_at 不该变：%q → %q", item.CreatedAt, got.CreatedAt)
	}

	// updated_at 必须往前走。**但不能拿响应里的字符串比** —— 它是 RFC3339 秒精度，
	// 而「发帖」和「改帖」在测试里是背靠背的两个请求，几乎总是落在同一秒内，
	// 比字符串会得到一个和「有没有 bump」毫无关系的结论（改对了也相等）。
	// 所以这一条落到数据库上比：timestamptz 是微秒精度，PG 的 now() 取的是
	// 事务开始时刻，两个请求是两个事务，一定不相等。
	moved := harness.QueryRow(t,
		`SELECT updated_at, updated_at > created_at AS moved, updated_at = created_at AS same
		 FROM items WHERE id = $1`, item.ID)
	if moved["moved"] != true {
		t.Errorf("库里的 updated_at（%v）没有走到 created_at 之后 —— repo.Update 少写了 updated_at = now()？",
			moved["updated_at"])
	}
	if moved["same"] != false {
		t.Error("updated_at 和 created_at 一模一样，改帖没有 bump 时间戳")
	}

	// 操作者刚刚把 contact 作为请求体的一部分提交上来，所以响应里一定回给他 ——
	// 否则他改完自己的帖子，看到的却是一个 null，会以为改丢了。
	if got.Contact == nil || *got.Contact != "13900000000" {
		t.Errorf("改帖响应里应该带回新的 contact，实际 %+v", got.Contact)
	}
	if got.ContactLocked {
		t.Error("改自己的帖子，contact_locked 不该是 true")
	}

	db := harness.QueryRow(t, `SELECT title, category_id, location_id, contact, updated_at, created_at
	                           FROM items WHERE id = $1`, item.ID)
	if db["title"] != "改之后的标题" || db["contact"] != "13900000000" {
		t.Errorf("库里没改过来：%v", db)
	}
}

// TestM2UpdateCannotChangeTypeOrAuthor 钉住两样**不能改**的东西。
//
// item_type：#16 的请求体里没有这个字段，类型从库里已有的行取。
// 作者：repo.UpdateItemRow 里压根没有 user_id。
// 「归属可以转让」是这个系统最不该有的能力，所以它在类型层面就不存在 ——
// 不是一个 if 挡住了它，而是没有任何代码路径能表达它。
//
// 测试的办法是「硬塞这两个字段进去」：如果哪天有人给请求体加了 item_type，
// 这条测试会立刻变红，而他必须想清楚为什么要加。
func TestM2UpdateCannotChangeTypeOrAuthor(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "typechange", "correct-horse-battery")
	stranger := harness.RegisterAndLogin(t, "typestranger", "correct-horse-battery")
	item := createItem(t, me, foundBody("一条 found 帖", "13800000000"))

	body := foundBody("试着改类型", "13800000000")
	body["item_type"] = "lost"
	body["user_id"] = stranger.UserID
	body["author"] = map[string]any{"id": stranger.UserID}
	r := harness.Do(t, http.MethodPut, "/api/items/"+itoa(item.ID), body, me.Token)
	RequireOK(t, r, "PUT /api/items/:id")

	var got itemView
	r.DataInto(t, &got)
	if got.ItemType != "found" {
		t.Errorf("item_type 被改成了 %q —— 请求体里塞 item_type 不该有任何效果", got.ItemType)
	}
	if got.Author.ID != me.UserID {
		t.Errorf("作者被改成了 %d —— 「归属转让」这个能力不该存在", got.Author.ID)
	}
	db := harness.QueryRow(t, `SELECT item_type, user_id FROM items WHERE id = $1`, item.ID)
	if db["item_type"] != "found" || db["user_id"] != me.UserID {
		t.Errorf("库里被改了：%v", db)
	}
}

// TestM2UpdateForbiddenForOthers 是判据点名的一条：非本人改他人帖得 FORBIDDEN。
//
// 顺带钉住「权限判断走在字段校验前面」：请求体里那些字段全是非法的，
// 但得到的仍然是 FORBIDDEN 而不是 VALIDATION。反过来的话，
// 一个陌生人就能通过反复提交不同的字段，把别人帖子的校验规则摸清楚。
func TestM2UpdateForbiddenForOthers(t *testing.T) {
	harness.TruncateAll(t)
	owner := harness.RegisterAndLogin(t, "victim", "correct-horse-battery")
	stranger := harness.RegisterAndLogin(t, "attacker", "correct-horse-battery")
	item := createItem(t, owner, lostBody("别人的帖子", "13800000000"))

	t.Run("陌生人改", func(t *testing.T) {
		r := harness.Do(t, http.MethodPut, "/api/items/"+itoa(item.ID),
			lostBody("被我改掉了", "13900000000"), stranger.Token)
		RequireCode(t, r, apperr.CodeForbidden)
		if r.HTTPStatus != http.StatusForbidden {
			t.Errorf("期望 HTTP 403，实际 %d", r.HTTPStatus)
		}
		db := harness.QueryRow(t, `SELECT title FROM items WHERE id = $1`, item.ID)
		if db["title"] != "别人的帖子" {
			t.Errorf("帖子被改掉了：%v", db["title"])
		}
	})

	t.Run("未登录改", func(t *testing.T) {
		RequireCode(t, harness.Do(t, http.MethodPut, "/api/items/"+itoa(item.ID),
			lostBody("x", "y"), ""), apperr.CodeUnauthorized)
	})

	t.Run("陌生人删", func(t *testing.T) {
		r := harness.Do(t, http.MethodDelete, "/api/items/"+itoa(item.ID), nil, stranger.Token)
		RequireCode(t, r, apperr.CodeForbidden)
		if st := harness.QueryRow(t, `SELECT status FROM items WHERE id = $1`, item.ID)["status"]; st != "open" {
			t.Errorf("帖子被删掉了：status=%v", st)
		}
	})

	// 权限判断必须在字段校验前面：这里所有字段都是非法的，
	// 但陌生人拿到的是 FORBIDDEN，而不是「哪个字段错了」的清单。
	t.Run("非法字段也只报 FORBIDDEN", func(t *testing.T) {
		bad := map[string]any{
			"title":       "",
			"contact":     "",
			"category_id": 0,
			"location_id": -1,
			"lost_at":     "不是时间",
		}
		RequireCode(t, harness.Do(t, http.MethodPut, "/api/items/"+itoa(item.ID), bad, stranger.Token),
			apperr.CodeForbidden)
	})
}

// TestM2AdminUpdateRequiresReason 钉住 admin 动别人数据行的那条通道。
//
// 计划 §4：#16/#17 的 Auth 是「JWT（本人或 Admin）」，但 admin 分支必须填
// admin_reason —— 那个理由会写进 admin_actions（M6），让其他管理员看到
// 你为什么动了别人的帖子。M2 先把**日志**这一半做了，否则 M6 之前的
// 这段时间里 admin 改帖完全无痕。
//
// admin 改**自己**的帖子不算治理动作，不需要理由。
func TestM2AdminUpdateRequiresReason(t *testing.T) {
	harness.TruncateAll(t)
	owner := harness.RegisterAndLogin(t, "owned", "correct-horse-battery")
	admin := harness.MakeAdmin(t, harness.RegisterAndLogin(t, "root", "correct-horse-battery"))
	item := createItem(t, owner, foundBody("疑似垃圾帖", "13800000000"))

	t.Run("admin 不填理由 → VALIDATION", func(t *testing.T) {
		body := foundBody("admin 想改标题", "13800000000")
		requireField(t, harness.Do(t, http.MethodPut, "/api/items/"+itoa(item.ID), body, admin.Token),
			"admin_reason")
	})

	t.Run("admin 填空白理由 → VALIDATION", func(t *testing.T) {
		body := foundBody("admin 想改标题", "13800000000")
		body["admin_reason"] = "   "
		requireField(t, harness.Do(t, http.MethodPut, "/api/items/"+itoa(item.ID), body, admin.Token),
			"admin_reason")
	})

	t.Run("admin 填了理由 → 成功", func(t *testing.T) {
		body := foundBody("【已核实】钱包", "13800000000")
		body["admin_reason"] = "标题里带了广告链接，按举报处理"
		r := harness.Do(t, http.MethodPut, "/api/items/"+itoa(item.ID), body, admin.Token)
		RequireOK(t, r, "admin PUT /api/items/:id")
		var got itemView
		r.DataInto(t, &got)
		if got.Title != "【已核实】钱包" {
			t.Errorf("admin 的修改没生效：%q", got.Title)
		}
		if got.Author.ID != owner.UserID {
			t.Errorf("admin 改帖不该改变归属，实际 author=%d", got.Author.ID)
		}
	})

	// 普通用户带上 admin_reason 不该得到「理由不合格」，而是「你没权限」。
	// 反过来的话，等于向他确认了这个端点存在一条 admin 通道。
	t.Run("普通用户带 admin_reason 仍然是 FORBIDDEN", func(t *testing.T) {
		stranger := harness.RegisterAndLogin(t, "notadmin", "correct-horse-battery")
		body := foundBody("假装是管理员", "13800000000")
		body["admin_reason"] = "我是管理员"
		RequireCode(t, harness.Do(t, http.MethodPut, "/api/items/"+itoa(item.ID), body, stranger.Token),
			apperr.CodeForbidden)
	})

	t.Run("admin 改自己的帖子不需要理由", func(t *testing.T) {
		own := createItem(t, admin, lostBody("admin 自己的帖子", "13800000000"))
		body := lostBody("admin 改自己的帖子", "13800000000")
		r := harness.Do(t, http.MethodPut, "/api/items/"+itoa(own.ID), body, admin.Token)
		RequireOK(t, r, "admin 改自己的帖子")
	})
}

// TestM2UpdateClosedItemSucceeds 是判据点名的最后一条：
// 改自己一条 closed 状态的帖子 → 成功（验证 ITEM_CLOSED 只挡 deleted）。
//
// ITEM_CLOSED 这个码的字面意思是「帖子已关闭或已删除」，很容易顺手写成
// 「不是 open 就拒」。那样写的话：匹配候选只看 open，改一条 closed 帖
// 不会污染任何结果，而「东西已经还回来了，但描述里写错了电话」这种情况
// 就再也改不了了 —— 用户唯一的出路是删掉重发，而重发会丢掉 created_at
// 和已经发生的解锁记录。
func TestM2UpdateClosedItemSucceeds(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "closedowner", "correct-horse-battery")
	item := createItem(t, me, lostBody("已经还回来了", "13800000000"))

	// 走 #18 关掉，而不是直接改库 —— 顺便把 #18 的正常路径也过一遍。
	r := harness.Do(t, http.MethodPatch, "/api/items/"+itoa(item.ID)+"/status",
		map[string]any{"status": "closed"}, me.Token)
	RequireOK(t, r, "PATCH status=closed")

	body := lostBody("已经还回来了（电话换了）", "13900000000")
	upd := harness.Do(t, http.MethodPut, "/api/items/"+itoa(item.ID), body, me.Token)
	RequireOK(t, upd, "PUT 一条 closed 的帖子")

	var got itemView
	upd.DataInto(t, &got)
	if got.Status != "closed" {
		t.Errorf("改帖不该顺带改状态，实际 status=%q", got.Status)
	}
	if got.Contact == nil || *got.Contact != "13900000000" {
		t.Errorf("contact 应该改过来了，实际 %+v", got.Contact)
	}

	// deleted 才是 ITEM_CLOSED 要挡的那个状态。
	harness.SetItemStatus(t, item.ID, "deleted")
	d := harness.Do(t, http.MethodPut, "/api/items/"+itoa(item.ID), body, me.Token)
	RequireCode(t, d, apperr.CodeItemClosed)
	if d.HTTPStatus != http.StatusConflict {
		t.Errorf("ITEM_CLOSED 期望 HTTP 409，实际 %d", d.HTTPStatus)
	}
}

// ---------- #17 删帖 ----------

// TestM2DeleteIsSoft 钉住「软删」这三个字的全部含义。
//
// 物理删除会丢历史，而 item_images / contact_views / item_returns /
// match_pairs / reports 五张表都用外键指着 items，CASCADE 下去就是一次连带清库。
// 所以：status → deleted，**行还在，图片还在，磁盘文件还在**。
//
// 「磁盘文件还在」这一条容易被当成 bug 修掉（「删了帖子怎么还占着磁盘」）。
// 不是 bug：M4 的 contact_views 和 M5 的 item_returns 都还要引用这条帖子，
// 而归还凭证里的图片一旦被删，「私下和平台对上号」这件事就永远对不上了。
func TestM2DeleteIsSoft(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "deleter", "correct-horse-battery")
	stranger := harness.RegisterAndLogin(t, "deleterstranger", "correct-horse-battery")

	pic := upload(t, me, 2048)
	body := lostBody("要被删掉的帖子", "13800000000")
	body["image_paths"] = []string{pic.Path}
	item := createItem(t, me, body)

	r := harness.Do(t, http.MethodDelete, "/api/items/"+itoa(item.ID), nil, me.Token)
	RequireOK(t, r, "DELETE /api/items/:id")
	if r.HTTPStatus != http.StatusOK {
		t.Errorf("#17 成功应该是 HTTP 200，实际 %d", r.HTTPStatus)
	}

	if st := harness.QueryRow(t, `SELECT status FROM items WHERE id = $1`, item.ID)["status"]; st != "deleted" {
		t.Errorf("status 应该是 deleted，实际 %v", st)
	}
	if n := harness.Count(t, `SELECT count(*) FROM items WHERE id = $1`, item.ID); n != 1 {
		t.Errorf("行不该被物理删除，实际查到 %d 行", n)
	}
	if n := harness.Count(t, `SELECT count(*) FROM item_images WHERE item_id = $1`, item.ID); n != 1 {
		t.Errorf("item_images 不该跟着删，实际 %d 行", n)
	}
	if _, err := os.Stat(filepath.Join(harness.Cfg.UploadDir, filepath.FromSlash(pic.Path))); err != nil {
		t.Errorf("磁盘文件不该被删掉（M5 的归还凭证还要用它）: %v", err)
	}

	// 广场上消失了，别人的 #15 也 404 了，但本人的 #19 还看得见。
	var page itemPage
	list := harness.Get(t, "/api/items?status=open", "")
	RequireOK(t, list, "GET /api/items")
	list.DataInto(t, &page)
	for _, s := range page.List {
		if s.ID == item.ID {
			t.Error("软删的帖子不该出现在广场上")
		}
	}
	RequireCode(t, harness.Get(t, "/api/items/"+itoa(item.ID), stranger.Token), apperr.CodeNotFound)
	RequireCode(t, harness.Get(t, "/api/items/"+itoa(item.ID), ""), apperr.CodeNotFound)

	mine := harness.Get(t, "/api/my/items?status=deleted", me.Token)
	RequireOK(t, mine, "GET /api/my/items?status=deleted")
	var minePage itemPage
	mine.DataInto(t, &minePage)
	found := false
	for _, s := range minePage.List {
		if s.ID == item.ID {
			found = true
			if s.Status != "deleted" {
				t.Errorf("#19 里应该显示 status=deleted，实际 %q", s.Status)
			}
		}
	}
	if !found {
		t.Error("本人在 #19 里应该还能看到自己被删的帖子（否则他会以为帖子凭空消失了）")
	}

	// 重复删：ITEM_CLOSED 而不是当成功。重复删通常是前端连点两次，
	// 让用户看到「这条已经删了」比让他以为又删了一次更清楚。
	again := harness.Do(t, http.MethodDelete, "/api/items/"+itoa(item.ID), nil, me.Token)
	RequireCode(t, again, apperr.CodeItemClosed)
}

func TestM2AdminDeleteRequiresReason(t *testing.T) {
	harness.TruncateAll(t)
	owner := harness.RegisterAndLogin(t, "spamtarget", "correct-horse-battery")
	admin := harness.MakeAdmin(t, harness.RegisterAndLogin(t, "admindeleter", "correct-horse-battery"))
	item := createItem(t, owner, foundBody("批量发的垃圾帖", "13800000000"))

	requireField(t, harness.Do(t, http.MethodDelete, "/api/items/"+itoa(item.ID),
		map[string]any{}, admin.Token), "admin_reason")

	r := harness.Do(t, http.MethodDelete, "/api/items/"+itoa(item.ID),
		map[string]any{"admin_reason": "同一账号 30 分钟内发了 40 条重复帖"}, admin.Token)
	RequireOK(t, r, "admin DELETE")
	if st := harness.QueryRow(t, `SELECT status FROM items WHERE id = $1`, item.ID)["status"]; st != "deleted" {
		t.Errorf("admin 删帖没生效：status=%v", st)
	}
}

// ---------- #18 开帖/关帖 ----------

func TestM2ChangeStatus(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "statusowner", "correct-horse-battery")
	stranger := harness.RegisterAndLogin(t, "statusstranger", "correct-horse-battery")
	item := createItem(t, me, lostBody("状态机", "13800000000"))

	patch := func(status, token string) Response {
		return harness.Do(t, http.MethodPatch, "/api/items/"+itoa(item.ID)+"/status",
			map[string]any{"status": status}, token)
	}

	t.Run("open → closed", func(t *testing.T) {
		r := patch("closed", me.Token)
		RequireOK(t, r, "PATCH status=closed")
		var got statusResult
		r.DataInto(t, &got)
		if got.ID != item.ID || got.Status != "closed" {
			t.Errorf("data 应该是 {id, status}，实际 %+v", got)
		}
	})

	t.Run("重复设成同一个状态是幂等的", func(t *testing.T) {
		// 前端连点两次不该收到一个 409。
		RequireOK(t, patch("closed", me.Token), "再 PATCH 一次 closed")
	})

	t.Run("closed → open", func(t *testing.T) {
		RequireOK(t, patch("open", me.Token), "PATCH status=open")
		if st := harness.QueryRow(t, `SELECT status FROM items WHERE id = $1`, item.ID)["status"]; st != "open" {
			t.Errorf("库里应该是 open，实际 %v", st)
		}
	})

	t.Run("陌生人 → FORBIDDEN", func(t *testing.T) {
		RequireCode(t, patch("closed", stranger.Token), apperr.CodeForbidden)
	})

	// ⚠ admin 也不行，这是 §4 第 18 行「Auth = JWT（本人）」的直接含义。
	// 关掉一条帖子是「这东西已经还回来了」这个**社区事实**的表态，
	// 只有发帖人有资格表态；admin 能销毁内容，但制造不出归属（定位原则 5）。
	// admin 想让一条帖子从广场消失，用的是 #43 批量下架（status→deleted），
	// 那是一次治理动作，会落 admin_actions、会给作者发通知，和「关帖」是两件事。
	t.Run("admin → FORBIDDEN", func(t *testing.T) {
		admin := harness.MakeAdmin(t, stranger)
		r := patch("closed", admin.Token)
		RequireCode(t, r, apperr.CodeForbidden)
	})

	t.Run("未登录 → UNAUTHORIZED", func(t *testing.T) {
		RequireCode(t, patch("closed", ""), apperr.CodeUnauthorized)
	})

	for _, bad := range []string{"", "OPEN", "deleted", "archived", "returned", "open "} {
		t.Run("status="+bad, func(t *testing.T) {
			// deleted **不能**通过 #18 设置：那是 #17 的语义，
			// 混在一起会让「用户自己关帖」和「内容被销毁」变成同一个动作。
			requireField(t, patch(bad, me.Token), "status")
		})
	}

	t.Run("已删除的帖子不能改状态", func(t *testing.T) {
		harness.SetItemStatus(t, item.ID, "deleted")
		RequireCode(t, patch("open", me.Token), apperr.CodeItemClosed)
	})

	t.Run("不存在的帖子 → NOT_FOUND", func(t *testing.T) {
		RequireCode(t, harness.Do(t, http.MethodPatch, "/api/items/999999/status",
			map[string]any{"status": "closed"}, me.Token), apperr.CodeNotFound)
	})
}

// ---------- #42 帖主自删单张图片 ----------

// TestM2OwnerDeletesOwnImage 是判据点名的那条，三个断言一个都不能少：
//
//	item_images 少一行 · 磁盘文件消失 · 帖子本身还在
//
// 第三条最容易被漏掉，而它恰恰是这个端点的存在理由：
// 「真的捡到东西的人不该因为一张照片有问题就丢掉整条帖子」。
func TestM2OwnerDeletesOwnImage(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "imgowner", "correct-horse-battery")

	first := upload(t, me, 1024)
	second := upload(t, me, 1024)
	body := lostBody("有两张图的帖子", "13800000000")
	body["image_paths"] = []string{first.Path, second.Path}
	item := createItem(t, me, body)

	target := item.Images[0]
	absPath := imageAbsPath(t, target.ID)
	if _, err := os.Stat(absPath); err != nil {
		t.Fatalf("删之前文件就该在 %s: %v", absPath, err)
	}

	r := harness.Do(t, http.MethodDelete, "/api/item-images/"+itoa(target.ID), nil, me.Token)
	RequireOK(t, r, "DELETE /api/item-images/:id")
	if r.HTTPStatus != http.StatusOK {
		t.Errorf("#42 成功应该是 HTTP 200，实际 %d", r.HTTPStatus)
	}

	// ① item_images 少一行
	if n := harness.Count(t, `SELECT count(*) FROM item_images WHERE item_id = $1`, item.ID); n != 1 {
		t.Errorf("item_images 应该从 2 行变成 1 行，实际 %d 行", n)
	}
	if n := harness.Count(t, `SELECT count(*) FROM item_images WHERE id = $1`, target.ID); n != 0 {
		t.Errorf("被删的那一行还在")
	}

	// ② 磁盘文件消失
	if _, err := os.Stat(absPath); !os.IsNotExist(err) {
		t.Errorf("磁盘文件 %s 应该已经没了，stat 的结果是 %v", absPath, err)
	}

	// ③ 帖子本身还在，另一张图也还在
	still := fetchDetail(t, item.ID, me.Token)
	if still.ID != item.ID || still.Status != "open" {
		t.Errorf("帖子不该被动过：%+v", still)
	}
	if len(still.Images) != 1 {
		t.Fatalf("应该还剩 1 张图，实际 %d 张", len(still.Images))
	}
	if still.Images[0].ID != item.Images[1].ID {
		t.Errorf("剩下的应该是第二张图（id=%d），实际 id=%d", item.Images[1].ID, still.Images[0].ID)
	}
	if still.Images[0].URL != second.URL {
		t.Errorf("剩下的应该是第二张图 %s，实际 %s", second.URL, still.Images[0].URL)
	}
	if _, err := os.Stat(filepath.Join(harness.Cfg.UploadDir, filepath.FromSlash(second.Path))); err != nil {
		t.Errorf("另一张图的文件不该被删: %v", err)
	}

	// 封面自动变成剩下的那张：#14 用 LEFT JOIN LATERAL ... ORDER BY sort_order LIMIT 1
	// 现算封面，所以删掉第一张之后不需要任何「重算封面」的代码。
	// 这里断言的是那个 JOIN 的行为，不是一个存下来的字段。
	var page itemPage
	list := harness.Get(t, "/api/items?item_type=lost", "")
	RequireOK(t, list, "GET /api/items")
	list.DataInto(t, &page)
	for _, s := range page.List {
		if s.ID == item.ID && s.CoverImage != second.URL {
			t.Errorf("封面应该跟着变成 %s，实际 %q", second.URL, s.CoverImage)
		}
	}
}

// TestM2OthersCannotDeleteImage 是判据点名的另一条：别人删这张图得 FORBIDDEN。
//
// ⚠ admin 也不行。#42 的 Auth 列写的是「仅帖主本人」，admin 删图是 #45（M6）。
// 分成两个端点不是为了麻烦，是因为这两件事的**含义**不同：
// 帖主删图是「我不想让这张照片挂在这儿」，admin 删图是一次治理动作，
// 要填理由、要落 admin_actions。合成一个端点就分不出是谁删的了。
func TestM2OthersCannotDeleteImage(t *testing.T) {
	harness.TruncateAll(t)
	owner := harness.RegisterAndLogin(t, "imgvictim", "correct-horse-battery")
	stranger := harness.RegisterAndLogin(t, "imgattacker", "correct-horse-battery")
	admin := harness.MakeAdmin(t, stranger)

	pic := upload(t, owner, 1024)
	body := lostBody("别人不许删我的图", "13800000000")
	body["image_paths"] = []string{pic.Path}
	item := createItem(t, owner, body)
	imageID := item.Images[0].ID
	absPath := imageAbsPath(t, imageID)

	for _, tc := range []struct {
		name  string
		token string
		code  string
	}{
		{"陌生人 → FORBIDDEN", stranger.Token, apperr.CodeForbidden},
		{"admin → FORBIDDEN（admin 删图是 #45）", admin.Token, apperr.CodeForbidden},
		{"未登录 → UNAUTHORIZED", "", apperr.CodeUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := harness.Do(t, http.MethodDelete, "/api/item-images/"+itoa(imageID), nil, tc.token)
			RequireCode(t, r, tc.code)
			if n := harness.Count(t, `SELECT count(*) FROM item_images WHERE id = $1`, imageID); n != 1 {
				t.Error("被拒绝的删图请求不该动到数据库")
			}
			if _, err := os.Stat(absPath); err != nil {
				t.Errorf("被拒绝的删图请求不该动到磁盘文件: %v", err)
			}
		})
	}

	t.Run("不存在的图片 → NOT_FOUND", func(t *testing.T) {
		RequireCode(t, harness.Do(t, http.MethodDelete, "/api/item-images/999999", nil, owner.Token),
			apperr.CodeNotFound)
	})
	t.Run("id 不是数字 → NOT_FOUND", func(t *testing.T) {
		RequireCode(t, harness.Do(t, http.MethodDelete, "/api/item-images/abc", nil, owner.Token),
			apperr.CodeNotFound)
	})

	t.Run("删两次 → 第二次 NOT_FOUND", func(t *testing.T) {
		RequireOK(t, harness.Do(t, http.MethodDelete, "/api/item-images/"+itoa(imageID), nil, owner.Token),
			"第一次删")
		// 第二次是 NOT_FOUND 而不是幂等成功：图片 id 是一次性的，
		// 一个新的 id 不会撞上旧的。而「删一个不存在的东西」报成功，
		// 会让前端以为删掉了、刷新之后发现图还在，比报 404 难查得多。
		RequireCode(t, harness.Do(t, http.MethodDelete, "/api/item-images/"+itoa(imageID), nil, owner.Token),
			apperr.CodeNotFound)
	})
}

// TestM2UpdateReplacesImages 钉住 #16 的 image_paths 三态语义。
//
//	字段缺失 / null → 图片保持原样
//	[]              → 把图片全删掉
//	[a, b]          → 整个替换成这两张
//
// 用**指针切片**表达这个区别是 encoding/json 的天然行为：
// 字段缺失或 null → nil，[] → 指向空切片的指针。
// 少了这个区分，「改一下标题」就会顺手把所有图片清空 ——
// 而前端如果每次都把当前图片列表回传，这个 bug 又永远测不出来。
func TestM2UpdateReplacesImages(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "imgreplace", "correct-horse-battery")

	a := upload(t, me, 1024)
	b := upload(t, me, 1024)
	c := upload(t, me, 1024)
	body := lostBody("图片三态", "13800000000")
	body["image_paths"] = []string{a.Path, b.Path}
	item := createItem(t, me, body)

	absA := filepath.Join(harness.Cfg.UploadDir, filepath.FromSlash(a.Path))
	absB := filepath.Join(harness.Cfg.UploadDir, filepath.FromSlash(b.Path))

	t.Run("不带 image_paths → 图片不变", func(t *testing.T) {
		noImages := lostBody("只改标题", "13800000000")
		r := harness.Do(t, http.MethodPut, "/api/items/"+itoa(item.ID), noImages, me.Token)
		RequireOK(t, r, "PUT 不带 image_paths")
		var got itemView
		r.DataInto(t, &got)
		if len(got.Images) != 2 {
			t.Fatalf("图片应该保持 2 张，实际 %d 张", len(got.Images))
		}
		if got.Title != "只改标题" {
			t.Errorf("标题没改：%q", got.Title)
		}
	})

	t.Run("显式传 null → 图片不变", func(t *testing.T) {
		nullImages := lostBody("null 也是不变", "13800000000")
		nullImages["image_paths"] = nil
		r := harness.Do(t, http.MethodPut, "/api/items/"+itoa(item.ID), nullImages, me.Token)
		RequireOK(t, r, "PUT image_paths=null")
		var got itemView
		r.DataInto(t, &got)
		if len(got.Images) != 2 {
			t.Fatalf("null 应该等于「没带这个字段」，实际剩 %d 张", len(got.Images))
		}
	})

	t.Run("传 [] → 全删", func(t *testing.T) {
		empty := lostBody("清空图片", "13800000000")
		empty["image_paths"] = []string{}
		r := harness.Do(t, http.MethodPut, "/api/items/"+itoa(item.ID), empty, me.Token)
		RequireOK(t, r, "PUT image_paths=[]")
		var got itemView
		r.DataInto(t, &got)
		if len(got.Images) != 0 {
			t.Errorf("应该一张不剩，实际 %d 张", len(got.Images))
		}
		if got.Images == nil {
			t.Error("images 应该是 [] 而不是 null")
		}
		if n := harness.Count(t, `SELECT count(*) FROM item_images WHERE item_id = $1`, item.ID); n != 0 {
			t.Errorf("item_images 应该 0 行，实际 %d 行", n)
		}
		// 改帖清空图片时，磁盘文件**不删**。理由和软删一样：
		// 这些 path 可能还被别的行引用（用户可能只是把它们从这条帖子上摘下来），
		// 而删文件的唯一入口是 #42 / #45 —— 「哪个端点会删磁盘文件」必须是可数的。
		for _, p := range []string{absA, absB} {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("改帖不该删磁盘文件 %s: %v", p, err)
			}
		}
	})

	t.Run("传新的一组 → 整个替换", func(t *testing.T) {
		replaced := lostBody("换成一张新图", "13800000000")
		replaced["image_paths"] = []string{c.Path}
		r := harness.Do(t, http.MethodPut, "/api/items/"+itoa(item.ID), replaced, me.Token)
		RequireOK(t, r, "PUT image_paths=[c]")
		var got itemView
		r.DataInto(t, &got)
		if len(got.Images) != 1 || got.Images[0].URL != c.URL {
			t.Errorf("应该只剩 c，实际 %+v", got.Images)
		}
		if n := harness.Count(t, `SELECT count(*) FROM item_images WHERE item_id = $1`, item.ID); n != 1 {
			t.Errorf("item_images 应该 1 行，实际 %d 行", n)
		}
	})
}

// ---------- 辅助 ----------

// imageAbsPath 从数据库里读一张图片的 path，拼成磁盘上的绝对路径。
//
// 不拿 #15 返回的 url 去反推（把 /uploads/ 前缀砍掉）：那等于让测试相信
// 「url 就是 /uploads/ + path」这条拼法是对的，而这条拼法本身也是被测对象的一部分。
// 从库里读才是独立的第二个信源。
func imageAbsPath(t *testing.T, imageID int64) string {
	t.Helper()
	row := harness.QueryRow(t, `SELECT path FROM item_images WHERE id = $1`, imageID)
	path, ok := row["path"].(string)
	if !ok || path == "" {
		t.Fatalf("图片 %d 的 path 读不出来：%v", imageID, row["path"])
	}
	return filepath.Join(harness.Cfg.UploadDir, filepath.FromSlash(path))
}

// requireField 断言「code=VALIDATION 且 data.errors 里有一条指向 field」。
//
// 只断言 code 的话，「所有校验都报 title 字段」这种退化不会让任何测试变红 ——
// 而前端是靠 field 把红框画到具体输入框上的，画错了位置比不画还糟。
func requireField(t *testing.T, r Response, field string) {
	t.Helper()
	if r.Code != apperr.CodeValidation {
		t.Fatalf("期望 code=%s，实际 %s（HTTP %d）\nmessage: %s\ndata: %s",
			apperr.CodeValidation, r.Code, r.HTTPStatus, r.Message, truncate(string(r.Data)))
	}
	if r.HTTPStatus != http.StatusBadRequest {
		t.Errorf("VALIDATION 期望 HTTP 400，实际 %d", r.HTTPStatus)
	}

	var data struct {
		Errors []struct {
			Field string `json:"field"`
			Msg   string `json:"msg"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("data 解不开：%v\n%s", err, truncate(string(r.Data)))
	}
	for _, e := range data.Errors {
		if e.Field == field {
			if strings.TrimSpace(e.Msg) == "" {
				t.Errorf("字段 %q 的 msg 是空的，前端要拿它当提示文案", field)
			}
			return
		}
	}
	got := make([]string, 0, len(data.Errors))
	for _, e := range data.Errors {
		got = append(got, e.Field)
	}
	t.Fatalf("data.errors 里应该有一条指向 %q，实际是 %v\nmessage: %s", field, got, r.Message)
}

// itoa 是 strconv.FormatInt 的短名字。
//
// 拼 URL 的地方有几十处，一个四字符的函数名比一个到处重复的
// strconv.FormatInt(id, 10) 好读，也不会有人写错进制。
func itoa(id int64) string { return strconv.FormatInt(id, 10) }

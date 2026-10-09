package smoketest

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"lostfound/internal/apperr"
)

// 本文件是 M4 第②层的第二段：#30 收件箱、#31 未读数、#32 标记已读。
// 对照 §12 的 M4 判据里「notifications 全套」那一句。
//
// 这一段最重要的一条是**通知是从哪来的**：这里的每一条通知都由 M3 那条真实路径
// 产生（小李发 lost → 小王发相似的 found → 系统给小李发 new_match），
// 而不是 SQL 插进去的。理由是判据③那句「解锁之后 notifications 没有新增行」
// 需要一个**非零**的基线才有意义 —— 基线是 0 的时候，任何「读不出通知」的实现
// 也能让「数一下是 0」通过。
// 唯一用 SQL 直接插的地方是那条 item_id 为 NULL 的 admin_action：
// 它的写入方在 M6，M4 造不出第二条这样的行，而 #30 的 null 形状必须被测到。

// ---------- 响应形状 ----------

type notificationView struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	ItemID    *int64 `json:"item_id"`
	ReturnID  *int64 `json:"return_id"`
	IsRead    bool   `json:"is_read"`
	CreatedAt string `json:"created_at"`
	UserID    *int64 `json:"user_id"` // 只用来断言它**不出现**
}

type inboxPage struct {
	List     []notificationView `json:"list"`
	Total    int                `json:"total"`
	Page     int                `json:"page"`
	PageSize int                `json:"page_size"`
}

type unreadResult struct {
	Count int `json:"count"`
}

type markReadResult struct {
	UpdatedCount int `json:"updated_count"`
}

// ---------- 夹具 ----------

const inboxPassword = "correct-horse-battery"

// seedMatchedInbox 走一遍 M3 的真实链路，给 lost 作者们各产生一条 new_match。
//
// 返回那条 found 帖（通知里的 item_id 应该指向它）和它的作者（found 作者
// 永远不该收到通知，判据②的反面要用他的 token 去查收件箱）。
// 一次调用产生 len(lostAuthors) 条通知、同数量台账行 —— 这正是 §3.7 说的
// 「一条 found 帖命中多条 lost 帖时每人一条」。
func seedMatchedInbox(t *testing.T, lostAuthors ...Session) (itemView, Session) {
	t.Helper()

	for _, a := range lostAuthors {
		createItem(t, a, lostWalletBody(walletTitleLost, walletDesc, locLibrary, detailLibrary))
	}

	finder := harness.RegisterAndLogin(t, "m4finder", inboxPassword)
	r := harness.Post(t, "/api/items",
		foundWalletBody(walletTitleFound, walletDesc, locLibrary, detailLibrary), finder.Token)
	RequireOK(t, r, "小王发拾物帖（触发匹配和通知）")

	keys := dataKeys(t, r)
	var notified int
	decodeInto(t, keys["notified_count"], &notified)
	if notified != len(lostAuthors) {
		t.Fatalf("新建的 found 帖应该通知 %d 个失主，实际 notified_count=%d —— 收件箱测试的基线不成立了",
			len(lostAuthors), notified)
	}
	return createResultOf(t, r), finder
}

func fetchInbox(t *testing.T, query, token string) Response {
	t.Helper()
	return harness.Get(t, "/api/my/notifications"+query, token)
}

func inboxOf(t *testing.T, query, token string) inboxPage {
	t.Helper()
	r := fetchInbox(t, query, token)
	RequireOK(t, r, "GET /api/my/notifications"+query)
	var p inboxPage
	r.DataInto(t, &p)
	return p
}

func unreadCountOf(t *testing.T, token string) int {
	t.Helper()
	r := harness.Get(t, "/api/my/notifications/unread-count", token)
	RequireOK(t, r, "GET unread-count")
	var got unreadResult
	r.DataInto(t, &got)
	return got.Count
}

func markRead(t *testing.T, body any, token string) Response {
	t.Helper()
	return harness.Do(t, http.MethodPut, "/api/my/notifications/read", body, token)
}

// ---------- 判据链 ----------

// TestM4InboxChain 按「有通知 → 读出来 → 标已读 → 数字变小」的顺序走一遍。
//
// 刻意用一条链而不是三个单点：#31 那个数字必须是 #30 那一页的**子集统计**，
// 分开设三个测试的话，「标记已读改了 is_read 但没改 count 的查询条件」
// 这种不一致要靠人把三个测试的期望值对齐才能发现。
func TestM4InboxChain(t *testing.T) {
	harness.TruncateAll(t)

	li := harness.RegisterAndLogin(t, "m4inboxli", inboxPassword)
	found, finder := seedMatchedInbox(t, li)

	// #30 读出来
	p := inboxOf(t, "", li.Token)
	if p.Total != 1 || len(p.List) != 1 {
		t.Fatalf("小李的收件箱期望 1 条，实际 total=%d list=%d", p.Total, len(p.List))
	}
	if p.Page != 1 || p.PageSize != 20 {
		t.Errorf("收件箱的分页默认值应该是 (1,20)，实际 (%d,%d)", p.Page, p.PageSize)
	}

	n := p.List[0]
	if n.Type != "new_match" {
		t.Errorf("type 期望 new_match，实际 %q", n.Type)
	}
	// ⚠ item_id 指向那条 found 帖：点进通知就该到能联系到人的那一页
	if n.ItemID == nil || *n.ItemID != found.ID {
		t.Errorf("通知的 item_id 应该指向 found 帖 %d，实际 %v", found.ID, n.ItemID)
	}
	if n.IsRead {
		t.Error("新通知的 is_read 应该是 false")
	}
	if n.Title == "" || n.Content == "" {
		t.Errorf("通知的标题/正文不能是空的（那是用户在铃铛上唯一能看到的字）：%+v", n)
	}
	if n.CreatedAt == "" || !strings.HasSuffix(n.CreatedAt, "Z") {
		t.Errorf("created_at 应该是 UTC 的 RFC3339，实际 %q", n.CreatedAt)
	}
	// 收件人不该出现在响应里（user_id 来自 token，不是数据）
	if n.UserID != nil {
		t.Error("响应里出现了 user_id")
	}

	// #31 未读数
	if got := unreadCountOf(t, li.Token); got != 1 {
		t.Errorf("未读数期望 1，实际 %d", got)
	}
	// 不对称的另一半：拾主（found 帖的作者）收件箱是空的 —— §2.3 说他发完帖义务就完成了。
	if got := inboxOf(t, "", finder.Token); got.Total != 0 {
		t.Errorf("found 帖作者的收件箱有 %d 条通知 —— 平台不该通知拾主任何东西", got.Total)
	}

	// #32 按 id 标已读
	r := markRead(t, map[string]any{"ids": []int64{n.ID}}, li.Token)
	RequireOK(t, r, "标记已读")
	var res markReadResult
	r.DataInto(t, &res)
	if res.UpdatedCount != 1 {
		t.Errorf("updated_count 期望 1，实际 %d", res.UpdatedCount)
	}
	if got := unreadCountOf(t, li.Token); got != 0 {
		t.Errorf("标记之后未读数应该归零，实际 %d", got)
	}
	if after := inboxOf(t, "", li.Token); len(after.List) != 1 || !after.List[0].IsRead {
		t.Errorf("标记已读不该把通知本身弄丢（只是 is_read 变了）：%+v", after.List)
	}

	// ⚠ 幂等：同一条再标一次是 0，不是错误。
	// 这一条是「updated_count 数的是真的改了多少行」而不是「你提交了几个 id」的证据；
	// 如果实现写成了后者，这里会返回 1，而用户看到「标记了 1 条」其实一条都没变。
	again := markRead(t, map[string]any{"ids": []int64{n.ID}}, li.Token)
	RequireOK(t, again, "重复标记同一条")
	var againRes markReadResult
	again.DataInto(t, &againRes)
	if againRes.UpdatedCount != 0 {
		t.Errorf("重复标记的 updated_count 应该是 0（幂等的证据），实际 %d", againRes.UpdatedCount)
	}

	// 筛选项：未读的现在为空，已读的有一条
	if got := inboxOf(t, "?is_read=false", li.Token); got.Total != 0 || got.List == nil {
		t.Errorf("?is_read=false 期望空数组，实际 total=%d list=%v", got.Total, got.List)
	}
	if got := inboxOf(t, "?is_read=true", li.Token); got.Total != 1 {
		t.Errorf("?is_read=true 期望 1 条，实际 %d", got.Total)
	}
}

// TestM4MarkReadAllIsScopedToOwner 是 #32 最重要的一条鉴权断言。
//
// all=true 的含义是「**我的**全部未读」，不是「全部未读」。
// 这条测试用两个都有通知的人来跑：如果 repo 的 UPDATE 漏了 WHERE user_id，
// 小李点一下「全部已读」就会把小刘的通知一起标掉 ——
// 而那是不可逆的（系统里没有标回未读的端点），而且是别人的收件箱。
func TestM4MarkReadAllIsScopedToOwner(t *testing.T) {
	harness.TruncateAll(t)

	li := harness.RegisterAndLogin(t, "m4allli", inboxPassword)
	liu := harness.RegisterAndLogin(t, "m4allliu", inboxPassword)
	seedMatchedInbox(t, li, liu)

	// 基线：两个人各有一条未读
	if unreadCountOf(t, li.Token) != 1 || unreadCountOf(t, liu.Token) != 1 {
		t.Fatalf("两条通知的基线没建起来，后面的断言都没有意义")
	}
	liuID := inboxOf(t, "", liu.Token).List[0].ID

	r := markRead(t, map[string]any{"all": true}, li.Token)
	RequireOK(t, r, "全部标记")
	var res markReadResult
	r.DataInto(t, &res)
	if res.UpdatedCount != 1 {
		t.Errorf("小李只该改动自己那 1 条，updated_count 实际 %d", res.UpdatedCount)
	}

	if got := unreadCountOf(t, li.Token); got != 0 {
		t.Errorf("小李的未读应该归零，实际 %d", got)
	}
	// ⚠ 小刘的一条必须还是未读
	if got := unreadCountOf(t, liu.Token); got != 1 {
		t.Errorf("小李点「全部已读」把小刘的未读也标掉了（还剩 %d 条未读）—— "+
			"UPDATE 的 WHERE user_id 是这条端点唯一的安全边界", got)
	}
	if after := inboxOf(t, "", liu.Token); after.List[0].ID != liuID || after.List[0].IsRead {
		t.Errorf("小刘那条通知被改动或弄丢了：%+v", after.List)
	}

	// all=true 时没有 id 可查归属，所以 CountForeign 一次都不发（第①层也钉过；
	// 这里钉的是「真的没查」在真库上的样子）。
	if got := harness.Count(t, `SELECT count(*) FROM notifications WHERE user_id = $1 AND is_read = true`, li.UserID); got != 1 {
		t.Errorf("小李名下期望恰好 1 条已读，实际 %d", got)
	}
}

// TestM4MarkReadRejectsForeignIDs 是越权那半边：拿别人的 id 来标。
//
// 期望 FORBIDDEN 而不是「静默跳过然后返回 updated_count:1」——
// 后者会把越权伪装成成功，用户和小李都不会知道有人试过了。
func TestM4MarkReadRejectsForeignIDs(t *testing.T) {
	harness.TruncateAll(t)

	li := harness.RegisterAndLogin(t, "m4fwdli", inboxPassword)
	liu := harness.RegisterAndLogin(t, "m4fwdliu", inboxPassword)
	seedMatchedInbox(t, li, liu)

	liuNote := inboxOf(t, "", liu.Token).List[0]
	liNote := inboxOf(t, "", li.Token).List[0]

	// 只提交别人的 id
	r := markRead(t, map[string]any{"ids": []int64{liuNote.ID}}, li.Token)
	RequireCode(t, r, apperr.CodeForbidden)

	// 混在一起提交：一批里有一条别人的，整批拒绝（不「挑出能标的那几条标掉」）
	mixed := markRead(t, map[string]any{"ids": []int64{liNote.ID, liuNote.ID}}, li.Token)
	RequireCode(t, mixed, apperr.CodeForbidden)

	// ⚠ 两次拒绝之后数据库一个字都没变
	if unreadCountOf(t, li.Token) != 1 {
		t.Error("越权请求被拒了却还是把小李自己那条标掉了")
	}
	if unreadCountOf(t, liu.Token) != 1 {
		t.Error("越权请求把小刘的未读数改掉了")
	}
}

// TestM4MarkReadRejectsShapes 是 §4「ids 和 all 二选一」在 HTTP 层的样子。
func TestM4MarkReadRejectsShapes(t *testing.T) {
	harness.TruncateAll(t)
	li := harness.RegisterAndLogin(t, "m4shape", inboxPassword)
	seedMatchedInbox(t, li)
	realID := inboxOf(t, "", li.Token).List[0].ID

	// 一次能标记的 id 上限是 service 里那个 maxMarkReadIDs=200（第①层已经钉过它的
	// 判断顺序），这里从 HTTP 层验一次「真的过不来」：201 个必须被拒。
	longIDs := make([]int64, 201)
	for i := range longIDs {
		longIDs[i] = int64(i + 1)
	}

	cases := []struct {
		name string
		body any
	}{
		{"空对象（两个字段都没带）", map[string]any{}},
		{"ids 和 all 都带", map[string]any{"ids": []int64{realID}, "all": true}},
		{"all 是 false", map[string]any{"all": false}},
		{"ids 是空数组", map[string]any{"ids": []int64{}}},
		{"ids 里有 0", map[string]any{"ids": []int64{realID, 0}}},
		{"ids 里有负数", map[string]any{"ids": []int64{-1}}},
		{"ids 超过 200 个", map[string]any{"ids": longIDs}},
		{"ids 类型不对", map[string]any{"ids": "1,2,3"}},
		{"all 类型不对", map[string]any{"all": "yes"}},
		{"请求体不是 JSON", nil}, // 连 body 都不带
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			RequireCode(t, markRead(t, c.body, li.Token), apperr.CodeValidation)
		})
	}

	// 上面那十条一条都不该改动数据库
	if unreadCountOf(t, li.Token) != 1 {
		t.Error("被拒的标记请求改动了未读数")
	}
}

// ---------- 鉴权与「收件人不由参数决定」 ----------

func TestM4InboxRequiresAuth(t *testing.T) {
	harness.TruncateAll(t)
	li := harness.RegisterAndLogin(t, "m4authli", inboxPassword)
	seedMatchedInbox(t, li)

	for _, r := range []struct {
		name, method, path string
		body               any
	}{
		{"#30 收件箱", http.MethodGet, "/api/my/notifications", nil},
		{"#31 未读数", http.MethodGet, "/api/my/notifications/unread-count", nil},
		{"#32 标记已读", http.MethodPut, "/api/my/notifications/read", map[string]any{"all": true}},
	} {
		t.Run("没有 token "+r.name, func(t *testing.T) {
			RequireCode(t, harness.Do(t, r.method, r.path, r.body, ""), apperr.CodeUnauthorized)
		})
		t.Run("坏 token "+r.name, func(t *testing.T) {
			RequireCode(t, harness.Do(t, r.method, r.path, r.body, "not-a-real-token"), apperr.CodeUnauthorized)
		})
	}

	// 收件人永远来自 token：带一个别人的 user_id 参数不会读到别人的通知。
	// 这条断言的是「参数被无视」而不是「参数报错」—— 全站对多余参数的处理是忽略，
	// 但**忽略之后必须仍然只能看到自己的**。
	other := harness.RegisterAndLogin(t, "m4authother", inboxPassword)
	p := inboxOf(t, fmt.Sprintf("?user_id=%d", li.UserID), other.Token)
	if p.Total != 0 {
		t.Errorf("传了别人的 user_id 就读到了别人的通知（%d 条）—— 收件箱的归属必须由 token 决定", p.Total)
	}
	if got := unreadCountOf(t, other.Token); got != 0 {
		t.Errorf("未读数也被 user_id 参数影响了：%d", got)
	}
}

func TestM4InboxRejectsBadParams(t *testing.T) {
	harness.TruncateAll(t)
	li := harness.RegisterAndLogin(t, "m4param", inboxPassword)
	seedMatchedInbox(t, li)

	for _, q := range []string{
		"?is_read=1", "?is_read=0", "?is_read=yes", "?is_read=unread",
		"?page=0", "?page=-1", "?page=abc",
		"?page_size=0", "?page_size=99999", "?page_size=2.5",
	} {
		t.Run(q, func(t *testing.T) {
			RequireCode(t, fetchInbox(t, q, li.Token), apperr.CodeValidation)
		})
	}

	// 大小写不敏感 + 前后空格仍然合法（第①层测过，这里钉的是 HTTP 层同一条路径）
	RequireOK(t, fetchInbox(t, "?is_read=FALSE", li.Token), "is_read=FALSE")
	RequireOK(t, fetchInbox(t, "?is_read=true", li.Token), "is_read=true")
}

// ---------- 分页与 NULL 形状 ----------

func TestM4InboxPaging(t *testing.T) {
	harness.TruncateAll(t)

	// 一个失主、两条 found 帖 → 两条通知（同一人被通知两次是两条不同的事实）
	li := harness.RegisterAndLogin(t, "m4pageli", inboxPassword)
	first, _ := seedMatchedInbox(t, li)

	secondFinder := harness.RegisterAndLogin(t, "m4pagesecond", inboxPassword)
	secondBody := foundWalletBody(walletTitleFound, walletDesc, locLibrary, detailLibrary)
	secondBody["title"] = "又捡到一个黑色钱包"
	r := harness.Post(t, "/api/items", secondBody, secondFinder.Token)
	RequireOK(t, r, "第二条相似的拾物帖")
	second := createResultOf(t, r)

	if got := unreadCountOf(t, li.Token); got != 2 {
		t.Fatalf("两条通知的基线没建起来，未读数 %d", got)
	}

	all := inboxOf(t, "", li.Token)
	if all.Total != 2 || len(all.List) != 2 {
		t.Fatalf("期望 2 条，实际 %+v", all)
	}
	// 倒序：新的一条在前面，而且它的 item_id 就是第二条 found 帖
	if all.List[0].CreatedAt < all.List[1].CreatedAt {
		t.Errorf("收件箱应该按时间倒序：%q 在前但比 %q 早", all.List[0].CreatedAt, all.List[1].CreatedAt)
	}
	if all.List[0].ItemID == nil || *all.List[0].ItemID != second.ID {
		t.Errorf("最新那条通知应该指向后建的那条 found 帖 %d，实际 %v", second.ID, all.List[0].ItemID)
	}
	if all.List[1].ItemID == nil || *all.List[1].ItemID != first.ID {
		t.Errorf("较早那条通知应该指向第一条 found 帖 %d，实际 %v", first.ID, all.List[1].ItemID)
	}

	p1 := inboxOf(t, "?page_size=1", li.Token)
	if len(p1.List) != 1 || p1.Total != 2 || p1.Page != 1 || p1.PageSize != 1 {
		t.Fatalf("第 1 页形状不对：%+v", p1)
	}
	p2 := inboxOf(t, "?page_size=1&page=2", li.Token)
	if len(p2.List) != 1 || p2.List[0].ID == p1.List[0].ID {
		t.Errorf("第 2 页翻出了和第 1 页相同的行：%+v vs %+v", p1.List, p2.List)
	}
	// 越界的一页：空数组，total 仍然是 2（不是报错，也不是 null）
	pOut := inboxOf(t, "?page=9", li.Token)
	if pOut.List == nil || len(pOut.List) != 0 || pOut.Total != 2 {
		t.Errorf("越界页应该是 {list:[], total:2}，实际 %+v", pOut)
	}

	// 标记一条、留下一条，两个筛选项各自只有一条
	RequireOK(t, markRead(t, map[string]any{"ids": []int64{all.List[0].ID}}, li.Token), "标记新那条")
	if got := inboxOf(t, "?is_read=true", li.Token); got.Total != 1 || got.List[0].ID != all.List[0].ID {
		t.Errorf("?is_read=true 筛错了：%+v", got.List)
	}
	if got := inboxOf(t, "?is_read=false", li.Token); got.Total != 1 || got.List[0].ID != all.List[1].ID {
		t.Errorf("?is_read=false 筛错了：%+v", got.List)
	}
}

// TestM4InboxKeepsNullItemID 钉住 item_id / return_id 为 NULL 时**键仍然存在**。
//
// 这条通知（admin_action，批量下架那种不挂任何帖子的回执）M4 还造不出写入方 ——
// 它的产生点在 M6。所以这里用 SQL 预置，测的是**读**的那一半：
// 如果 service 用了 omitempty，键会整个消失，前端就分不清
// 「这条通知本来不挂帖子」和「后端忘了返回这个字段」，只能猜。
func TestM4InboxKeepsNullItemID(t *testing.T) {
	harness.TruncateAll(t)
	li := harness.RegisterAndLogin(t, "m4null", inboxPassword)

	seed := harness.RegisterAndLogin(t, "m4nullseed", inboxPassword)
	lost := createItem(t, seed, lostBody("用来被下架的帖子", "13712345678"))

	if _, err := harness.Pool.Exec(context.Background(),
		`INSERT INTO notifications (user_id, type, title, content, item_id)
		 VALUES ($1, 'admin_action', '帖子已被下架', '管理员下架了你发布的一条帖子', $2)`,
		li.UserID, lost.ID); err != nil {
		t.Fatalf("预置一条带 item_id 的通知失败: %v", err)
	}
	if _, err := harness.Pool.Exec(context.Background(),
		`INSERT INTO notifications (user_id, type, title, content)
		 VALUES ($1, 'admin_action', '批量处理通知', '你的 3 条帖子被下架了')`,
		li.UserID); err != nil {
		t.Fatalf("预置一条 item_id 为 NULL 的通知失败: %v（迁移里这一列是可空的吗？）", err)
	}

	r := fetchInbox(t, "", li.Token)
	RequireOK(t, r, "GET /api/my/notifications")

	// 直接对原始 JSON 断言，不经过 struct —— struct 会把「键不存在」和「键是 null」抹平成同一个 nil。
	body := string(r.Data)
	if !strings.Contains(body, `"item_id":null`) {
		t.Errorf("item_id 为 NULL 的那条通知，键整个消失了（前端无法区分「没挂帖子」和「后端漏字段」）：\n%s",
			truncate(body))
	}
	if !strings.Contains(body, `"return_id":null`) {
		t.Errorf("return_id 的键消失了，M5 之前它永远是 null，但键必须在：\n%s", truncate(body))
	}

	// 挂帖的那条仍然给出真实 id（两种情况混在同一页里，不能互相影响）
	p := inboxOf(t, "", li.Token)
	var withItem, withoutItem int
	for _, n := range p.List {
		if n.ItemID == nil {
			withoutItem++
		} else {
			withItem++
			if *n.ItemID != lost.ID {
				t.Errorf("通知挂错了帖子：期望 %d，实际 %d", lost.ID, *n.ItemID)
			}
		}
	}
	if withItem != 1 || withoutItem != 1 {
		t.Errorf("两条通知应该一条挂帖一条不挂，实际挂帖 %d 条、不挂 %d 条", withItem, withoutItem)
	}
}

// ---------- 「通知只能被读，不能被写」 ----------

// TestM4NoNotificationWriteEndpoint 是 §16 那条决定的路由级证据：
// 通知只由产生它的那件事写（M3 的匹配、M5 的归还、M6 的治理），
// 收件箱这三个端点**没有任何一个能造出一条通知**，也没有「标回未读」。
func TestM4NoNotificationWriteEndpoint(t *testing.T) {
	harness.TruncateAll(t)
	li := harness.RegisterAndLogin(t, "m4nowrap", inboxPassword)
	seedMatchedInbox(t, li)
	before := harness.Count(t, `SELECT count(*) FROM notifications`)

	// POST 到收件箱路径 → 405（这条路径只有 GET）
	RequireCode(t, harness.Post(t, "/api/my/notifications", map[string]any{"type": "new_match"}, li.Token),
		apperr.CodeMethodNotAllowed)
	// 「把已读的标回未读」这个端点刻意不存在：已读是不可逆的。
	RequireCode(t, harness.Do(t, http.MethodPut, "/api/my/notifications/unread", map[string]any{"all": true}, li.Token),
		apperr.CodeNotFound)
	RequireCode(t, harness.Do(t, http.MethodPut, "/api/my/notifications/unread-count", map[string]any{}, li.Token),
		apperr.CodeMethodNotAllowed)
	// 单条删除也不存在：通知是事实记录，不是用户的待办清单
	RequireCode(t, harness.Do(t, http.MethodDelete, "/api/my/notifications", nil, li.Token),
		apperr.CodeMethodNotAllowed)

	if got := harness.Count(t, `SELECT count(*) FROM notifications`); got != before {
		t.Errorf("四个「读端点」的请求就写出了 %d 条通知", got-before)
	}
	// 未读数也没被任何一次请求改动
	if got := unreadCountOf(t, li.Token); got != 1 {
		t.Errorf("未读数被这些不存在的端点改成了 %d", got)
	}
}

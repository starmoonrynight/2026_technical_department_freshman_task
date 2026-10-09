package smoketest

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
)

// 本文件是 M5 第②层的第四段：三个「读自己账本」的端点 #28 / #29 / #33。
//
// 前三个文件测的是「一次动作改变了什么」，这一个测的是「用户能不能看见自己那部分事实」。
// 三条最容易写错的规则恰好都不在状态机里：
//
//	① #28 和 #29 只差一个筛选列（r.submitter_id 对 i.user_id），
//	   差一个列就能实现成差整条 SQL —— 而两个列表互换是用户看得见的事故
//	② 「谁的列表」只能来自 JWT。任何一个 ?user_id= 都能把这三个端点变成
//	   全站最大的个人记录泄漏面（§4 对 #33 明写「一律取自 JWT」）
//	③ 列表元素里**没有**裁决材料（message / proof_image_path），
//	   它们只在 #24 里给当事人看；列表是「我有哪些事在办」，不是证据袋
//
// 还有一条贯穿全部三个端点的形状纪律：空的一页必须是 `[]` 而不是 `null`，
// 越界的一页 total 仍然要报真实总数。

// ---------- 夹具与小工具 ----------

// m5ListStage 是「同一条拾物帖上有两条 pending，而拾主自己在别人那儿也提交了一条」的舞台。
//
// 为什么需要第三条记录（拾主 → 小王的帖子）：只有前两条的话，
// #28 和 #29 在拾主身上分别是「空」和「非空」，一个把两个筛选列写反的实现
// 照样能通过对角线上的两个空列表 —— 必须有一个人两边都非空，
// 交换筛选条件才会立刻显形。
type m5ListStage struct {
	st       m5Stage
	wang     Session
	wangPost itemView
	onWang   int64 // 拾主在小王那条帖子上提交的确认（只有 #28 能看见它）
	fromLi   int64 // 小李在拾主帖子上提交的（只有 #29 能看见它）
	fromStra int64 // 路人在拾主帖子上提交的（同上，且它最新）
}

func setupM5Lists(t *testing.T) m5ListStage {
	t.Helper()

	l := m5ListStage{st: setupM5(t)}
	l.wang = harness.RegisterAndLogin(t, "m5wang", m5Password)
	// 地点选「其他」：这条帖子不该和谁的失物帖配上对，否则台账和小李的收件箱
	// 会多出与本页无关的行，而下面几条测试里有「通知数不变」那种断言。
	l.wangPost = createItem(t, l.wang,
		foundWalletBody("在食堂捡到一张校园卡", "食堂窗口捡到的校园卡，姓名已经看不清了",
			locOther, "二楼食堂窗口"))

	l.fromLi = requireSubmitted(t, l.st.found.ID, l.st.li).ID
	l.fromStra = requireSubmitted(t, l.st.found.ID, l.st.stranger).ID
	l.onWang = requireSubmitted(t, l.wangPost.ID, l.st.finder).ID
	return l
}

func returnListRaw(t *testing.T, kind, query, token string) Response {
	t.Helper()
	return harness.Get(t, "/api/my/returns/"+kind+query, token)
}

// returnList 取一页并直接 fatal —— 它在这一批测试里是夹具，不是被测对象。
// 真正要看错误码的用例一律走 returnListRaw。
func returnList(t *testing.T, kind, query, token string) returnPageView {
	t.Helper()
	r := returnListRaw(t, kind, query, token)
	RequireOK(t, r, "GET /api/my/returns/"+kind+query)
	var page returnPageView
	r.DataInto(t, &page)
	return page
}

func returnIDs(page returnPageView) []int64 {
	out := make([]int64, 0, len(page.List))
	for _, e := range page.List {
		out = append(out, e.ID)
	}
	return out
}

// wantReturnIDs 按**顺序**比 id 列表。
// 顺序在这里是被测对象（ORDER BY r.submitted_at DESC, r.id DESC），
// 所以不能用 assertSameSet 那种无序比较。
func wantReturnIDs(t *testing.T, what string, page returnPageView, want ...int64) {
	t.Helper()
	got := returnIDs(page)
	if len(got) != len(want) {
		t.Fatalf("%s 期望 %v，实际 %v", what, want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s 第 %d 条期望 id=%d，实际 %d（整列 %v）", what, i, want[i], got[i], got)
		}
	}
}

// jsonKeys 取一段 JSON 对象的键集合。给「这个键到底存不存在」那类断言用。
func jsonKeys(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("这段 JSON 不是对象，没法取键集合: %v\n原始: %s", err, truncate(string(raw)))
	}
	return m
}

func parseAPITime(t *testing.T, what, value string) time.Time {
	t.Helper()
	if !strings.HasSuffix(value, "Z") {
		t.Errorf("%s=%q，时间戳应当是带 Z 的 UTC RFC3339", what, value)
	}
	ts, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("%s=%q 解析不成 RFC3339: %v", what, value, err)
	}
	return ts
}

// ---------- #28 / #29：两个列表只差一个筛选列 ----------

// TestM5TwoListsDifferOnlyByWhoIsFiltered 是这一节的主测试。
//
// 六格全部要读：每个人 × 两个列表。缺任何一格，「把两个筛选条件写反」
// 这种实现就还能活下来 —— 而它的症状是「我提交的归还确认出现在了我收到的列表里」，
// 一个只有用户能看见的 bug。
func TestM5TwoListsDifferOnlyByWhoIsFiltered(t *testing.T) {
	l := setupM5Lists(t)

	// 拾主：收到两条（小李、路人），自己提交一条（在小王那儿）
	// 路人那条后提交，所以排在最前（submitted_at DESC）
	wantReturnIDs(t, "拾主 #29", returnList(t, "received", "?page_size=100", l.st.finder.Token),
		l.fromStra, l.fromLi)
	wantReturnIDs(t, "拾主 #28", returnList(t, "submitted", "?page_size=100", l.st.finder.Token),
		l.onWang)

	// 小李：提交过一条，没人在他的帖子上提交过（失物帖收不了归还确认）
	wantReturnIDs(t, "小李 #28", returnList(t, "submitted", "", l.st.li.Token), l.fromLi)
	if page := returnList(t, "received", "", l.st.li.Token); page.Total != 0 || page.List == nil {
		t.Errorf("小李没发过拾物帖，#29 期望 {list:[], total:0}，实际 %+v", page)
	}

	// 路人同上，方向相反
	wantReturnIDs(t, "路人 #28", returnList(t, "submitted", "", l.st.stranger.Token), l.fromStra)
	if page := returnList(t, "received", "", l.st.stranger.Token); page.Total != 0 || page.List == nil {
		t.Errorf("路人 #29 期望空，实际 %+v", page)
	}

	// 小王：只收到拾主那一条
	wantReturnIDs(t, "小王 #29", returnList(t, "received", "", l.wang.Token), l.onWang)
	if page := returnList(t, "submitted", "", l.wang.Token); page.Total != 0 {
		t.Errorf("小王没提交过，#28 期望 total=0，实际 %d", page.Total)
	}

	// admin 的这两个列表也只装他自己的：他是 admin 这件事不给他看别人账本的权限。
	// （§4 里 #28/#29 的鉴权列写的是 JWT，不是 Admin —— 治理面在 M6 的 #45。）
	admin := m5Admin(t)
	if page := returnList(t, "received", "", admin.Token); page.Total != 0 {
		t.Errorf("admin 的 #29 里有 %d 条，他不该通过这两个列表看到任何人的归还确认", page.Total)
	}
	if page := returnList(t, "submitted", "", admin.Token); page.Total != 0 {
		t.Errorf("admin 的 #28 里有 %d 条", page.Total)
	}
}

// TestM5ReturnItemSummaryIsThePostNotTheReader 检查列表元素里那个嵌套摘要指的是谁。
//
// 「拾主在小王帖子上提交的那条」是这一格最好的样本：读的人、帖子的作者、
// 记录里的提交人三方各不相同，任何一处把 author 填成当前用户都会在这里红。
func TestM5ReturnItemSummaryIsThePostNotTheReader(t *testing.T) {
	l := setupM5Lists(t)

	page := returnList(t, "submitted", "", l.st.finder.Token)
	if len(page.List) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(page.List))
	}
	entry := page.List[0]
	if entry.ID != l.onWang {
		t.Fatalf("取到了错误的记录 %d", entry.ID)
	}
	if entry.Item.ID != l.wangPost.ID {
		t.Errorf("item.id=%d，应当是那条拾物帖 %d", entry.Item.ID, l.wangPost.ID)
	}
	if entry.Item.AuthorID != l.wang.UserID {
		t.Errorf("item.author_id=%d，应当是帖子作者小王 %d 而不是读的人 %d",
			entry.Item.AuthorID, l.wang.UserID, l.st.finder.UserID)
	}
	if entry.Item.Title != l.wangPost.Title {
		t.Errorf("item.title=%q，库里那条是 %q", entry.Item.Title, l.wangPost.Title)
	}
	if entry.Item.Status != model.ItemStatusOpen {
		t.Errorf("item.status=%q", entry.Item.Status)
	}
	// 摘要一律不带联系方式（同 #14 广场、#20 匹配列表那条纪律）
	if entry.Item.Contact != nil {
		t.Errorf("嵌套摘要里出现了 contact=%v，解锁唯一的路径是 #21", *entry.Item.Contact)
	}
	if entry.Status != model.ReturnStatusPending {
		t.Errorf("status=%q", entry.Status)
	}

	// 反过来看 #29 那一格：读的人是帖子作者，所以 author 就是他自己 ——
	// 这一条只有在上面那条也成立时才有意义（否则两处可能都填成了当前用户）。
	recv := returnList(t, "received", "?page_size=100", l.st.finder.Token)
	for _, e := range recv.List {
		if e.Item.AuthorID != l.st.finder.UserID {
			t.Errorf("#29 里有一条 item.author_id=%d，这个列表筛的就是 i.user_id=%d",
				e.Item.AuthorID, l.st.finder.UserID)
		}
	}
}

// TestM5ReturnListsIgnoreForeignIDParams 钉住「谁的列表」这件事只能来自 JWT。
//
// 这一条是 §4 对 #33 写的那句「userID 一律取自 JWT 而不是查询参数」在
// #28/#29 上的同一份实现：handler 的 returnListQueryFrom 只读
// status / page / page_size 三个参数，其余一律不存在。
// 传进去的 ?user_id= 如果**被忽略**，两次请求应当拿到同一个列表；
// 如果被当成筛选条件，哪怕结果是空列表，都说明库里多了一条别人能指定的通道。
func TestM5ReturnListsIgnoreForeignIDParams(t *testing.T) {
	l := setupM5Lists(t)

	// 三个「别人能填的 id」全部塞进去：它们要么被忽略（对），
	// 要么把别人的账本交出来（错得离谱），要么安静地筛成空（看起来像「你没有记录」）。
	const noise = "?user_id=999999&item_id=888888&submitter_id=777777&page_size=100"
	if got := returnList(t, "submitted", noise, l.st.li.Token); !inSameOrder(returnIDs(got), []int64{l.fromLi}) {
		t.Errorf("带上 ?user_id= 之后 #28 变成了 %v，期望还是小李自己那一条 %d", returnIDs(got), l.fromLi)
	}
	// 更狠的一格：拿拾主的真实 id 去要「他的」列表，必须拿不到
	if got := returnList(t, "received",
		"?user_id="+itoa(l.st.finder.UserID)+"&page_size=100", l.st.li.Token); got.Total != 0 {
		t.Errorf("小李用 ?user_id=%d 读到了 %d 条别人的列表", l.st.finder.UserID, got.Total)
	}
	// 而拾主自己读仍然是那两条（证明上面那条不是「参数报错所以返回空」）
	if got := returnList(t, "received", "?page_size=100", l.st.finder.Token); got.Total != 2 {
		t.Errorf("拾主自己的 #29 期望 2 条，实际 %d", got.Total)
	}
}

// inSameOrder 是**顺序敏感**的 id 比较。
// 包里已有的 sameIDs 比的是多重集合（给广场排序不稳定的场景用），
// 而这里要的是「就是这一条，别混进一条别的」。
func inSameOrder(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestM5ReturnListEntryIsADecisionListNotAnEvidenceBag 钉住列表元素的字段集合。
//
// 计划 §4 第 28 行写的是 {id, item:摘要, status, owner_note, submitted_at, reviewed_at}，
// 六个键，多一个都算违约。最要紧的是**没有** message 和 proof_image_path：
// 那两个字段是提交人交给拾主看的材料（#24 里给当事人），
// 把它们摊进一个「我的归还确认」列表，等于把凭证图片挂在列表页上让人刷。
// owner_note 反过来是**允许**的：那是拾主自己写下的话。
func TestM5ReturnListEntryIsADecisionListNotAnEvidenceBag(t *testing.T) {
	l := setupM5Lists(t)

	r := returnListRaw(t, "received", "?page_size=100", l.st.finder.Token)
	RequireOK(t, r, "拾主查收到的归还确认")
	assertSameSet(t, "#28/#29 data", []string{"list", "total", "page", "page_size"}, jsonKeys(t, r.Data))

	rawList := jsonKeys(t, r.Data)["list"]
	var entries []json.RawMessage
	if err := json.Unmarshal(rawList, &entries); err != nil {
		t.Fatalf("list 不是数组: %v\n%s", err, truncate(string(rawList)))
	}
	if len(entries) != 2 {
		t.Fatalf("期望 2 条，实际 %d", len(entries))
	}
	for _, raw := range entries {
		assertSameSet(t, "归还确认列表元素",
			[]string{"id", "item", "status", "owner_note", "submitted_at", "reviewed_at"},
			jsonKeys(t, raw))
	}

	// 整个响应里不该出现「证据」这两个字对应的键名。
	// 键集合已经比过了，这条字符串扫描是为了抓「换了个名字塞进来」那种实现
	// （proof_url / evidence_image / message_text），它们能躲过键集合相等。
	body := string(r.Data)
	for _, banned := range []string{"proof", "message", "evidence"} {
		if strings.Contains(strings.ToLower(body), banned) {
			t.Errorf("列表响应里出现了 %q 片段，裁决材料只在 #24 给当事人看", banned)
		}
	}

	// item 那一层是帖子摘要，键集合必须和 #14 广场用的那一份完全相同
	assertSameSet(t, "列表里的 item 摘要", itemSummaryKeys, jsonKeys(t, jsonKeys(t, entries[0])["item"]))

	// pending 的记录 reviewed_at 是空串（键在、值诚实）
	var pending returnEntryView
	if err := json.Unmarshal(entries[0], &pending); err != nil {
		t.Fatalf("解列表元素失败: %v", err)
	}
	if pending.ReviewedAt != "" {
		t.Errorf("pending 的 reviewed_at=%q，还没审过就该是空串", pending.ReviewedAt)
	}
	if pending.OwnerNote != "" {
		t.Errorf("pending 的 owner_note=%q，还没人写过备注", pending.OwnerNote)
	}
	parseAPITime(t, "submitted_at", pending.SubmittedAt)

	// 确认之后：同一个位置（顺序没变）应当看到 note 和 reviewed_at 都填上了
	const note = "东西确实还给我了"
	RequireOK(t, confirmReturn(t, l.fromLi, note, l.st.finder.Token), "确认小李那条")
	after := returnList(t, "received", "?page_size=100", l.st.finder.Token)
	if len(after.List) != 2 {
		t.Fatalf("确认之后列表变成 %d 条了", len(after.List))
	}
	var confirmedEntry, pendingEntry returnEntryView
	for _, e := range after.List {
		if e.Status == model.ReturnStatusConfirmed {
			confirmedEntry = e
		} else {
			pendingEntry = e
		}
	}
	if confirmedEntry.ID != l.fromLi || confirmedEntry.OwnerNote != note {
		t.Errorf("确认后那条的 owner_note=%q (id=%d)，期望原话 %q",
			confirmedEntry.OwnerNote, confirmedEntry.ID, note)
	}
	parseAPITime(t, "reviewed_at", confirmedEntry.ReviewedAt)
	if pendingEntry.ReviewedAt != "" {
		t.Errorf("没被确认的那条 reviewed_at=%q", pendingEntry.ReviewedAt)
	}
}

// itemSummaryKeys 是 §4 里帖子摘要的十五个键（和 #14 广场、#20 匹配列表同一份形状）。
var itemSummaryKeys = []string{
	"id", "item_type", "title", "status", "category_id", "category_name",
	"location_id", "location_name", "lost_at", "found_at", "contact",
	"cover_image", "author_id", "author_name", "created_at",
}

// TestM5ReturnListsSurviveItemDeletion 检查这两个列表是**账本**不是广场。
//
// 帖子被 admin 下架（M6 才有 #44，这里按夹具惯例直接改库）之后，
// 广场上看不见它了，但「我提交过一条归还确认」这件事仍然要查得到 ——
// 记录消失了就等于用户的提交消失了，而那和「平台只记录事实」正好相反。
// 嵌套摘要要诚实显示 deleted，不能假装它还是 open。
func TestM5ReturnListsSurviveItemDeletion(t *testing.T) {
	l := setupM5Lists(t)

	// 先把那条拾物帖真的从广场上弄没，才谈得上「列表还在不在」
	harness.SetItemStatus(t, l.st.found.ID, model.ItemStatusDeleted)
	if containsID(squareIDs(t), l.st.found.ID) {
		t.Fatalf("帖子已下架，广场里还能看到 %d —— 那下面那条断言就测不到东西了", l.st.found.ID)
	}

	page := returnList(t, "received", "?page_size=100", l.st.finder.Token)
	wantReturnIDs(t, "下架之后 #29", page, l.fromStra, l.fromLi)
	for _, e := range page.List {
		if e.Item.Status != model.ItemStatusDeleted {
			t.Errorf("列表里那条的帖子摘要显示 %q，应当诚实是 deleted", e.Item.Status)
		}
	}
	if got := returnList(t, "submitted", "", l.st.li.Token); got.Total != 1 {
		t.Errorf("提交人这边 #28 期望还是 1 条，实际 %d", got.Total)
	}
}

// ---------- ?status= 白名单 ----------

// TestM5ReturnListStatusFilter 把四个状态各摆一条，然后逐个筛。
//
// 「白名单」这条规则的可执行形式是两半：四个合法值各返回自己那一组，
// 以及**第五个值必须报错而不是安静地返回空列表**。
// 后半重要得多：安静返回空的话，前端拼错一个字母就得到「你没有任何归还确认」，
// 而那是一个看起来完全正常的回答。
func TestM5ReturnListStatusFilter(t *testing.T) {
	l := setupM5Lists(t)
	finder, li := l.st.finder, l.st.li

	// 四个状态各摆一条，全部走小李的 #28：
	//   confirmed —— 就用夹具里小李在拾主帖子上那条（fromLi）
	//   rejected / cancelled —— 各新开一条帖子（同一个提交人在同一条帖子上只能有一条 pending）
	//   pending —— 在小王那条帖子上再提交一条（夹具里那条 pending 是**拾主**提交的，不是小李的）
	post2 := createItem(t, finder,
		foundWalletBody("在图书馆捡到一副耳机", "白色无线耳机，充电盒有划痕", locOther, "四楼自习室"))
	post3 := createItem(t, finder,
		foundWalletBody("在操场捡到一张公交卡", "公交卡，卡面上没有姓名", locOther, "主席台"))
	rejected := requireSubmitted(t, post2.ID, li)
	cancelled := requireSubmitted(t, post3.ID, li)
	pending := requireSubmitted(t, l.wangPost.ID, li)

	RequireOK(t, confirmReturn(t, l.fromLi, "还给我了", finder.Token), "到达 confirmed")
	RequireOK(t, rejectReturn(t, rejected.ID, "这条不是我那条", finder.Token), "到达 rejected")
	RequireOK(t, cancelReturn(t, cancelled.ID, li.Token), "到达 cancelled")

	for _, c := range []struct {
		status string
		want   []int64
	}{
		{model.ReturnStatusPending, []int64{pending.ID}},
		{model.ReturnStatusConfirmed, []int64{l.fromLi}},
		{model.ReturnStatusRejected, []int64{rejected.ID}},
		{model.ReturnStatusCancelled, []int64{cancelled.ID}},
	} {
		page := returnList(t, "submitted", "?status="+c.status, li.Token)
		wantReturnIDs(t, "?status="+c.status+"（小李的 #28）", page, c.want...)
		if page.Total != len(page.List) {
			t.Errorf("?status=%s 的 total=%d 而 list 有 %d 条，两者必须是同一个集合",
				c.status, page.Total, len(page.List))
		}
	}

	// 反过来的一格：拾主的 #29 里那两条现在一条 confirmed、一条 pending，
	// 同一个 status 值在两个列表里筛出的是完全不同的行。
	if got := returnList(t, "received", "?status="+model.ReturnStatusConfirmed, finder.Token); got.Total != 1 ||
		got.List[0].ID != l.fromLi {
		t.Errorf("拾主 #29 ?status=confirmed 筛错了：%+v", got.List)
	}
	if got := returnList(t, "received", "?status="+model.ReturnStatusPending, finder.Token); got.Total != 1 ||
		got.List[0].ID != l.fromStra {
		t.Errorf("拾主 #29 ?status=pending 筛错了：%+v", got.List)
	}

	// 不传 status = 全部四种都要出现（小李这里正好四种各一）
	all := returnList(t, "submitted", "?page_size=100", li.Token)
	if all.Total != 4 || len(all.List) != 4 {
		t.Errorf("不带 status 期望 4 条，实际 total=%d list=%d", all.Total, len(all.List))
	}
	// 空值等价于不传
	if empty := returnList(t, "submitted", "?status=", li.Token); empty.Total != 4 {
		t.Errorf("?status=（空值）期望 4 条，实际 %d", empty.Total)
	}

	for _, bad := range []string{"bogus", "PENDING", "Confirmed", "all", "审核中", "pending%20"} {
		r := returnListRaw(t, "submitted", "?status="+bad, li.Token)
		t.Run("非法值 "+bad, func(t *testing.T) {
			RequireCode(t, r, apperr.CodeValidation)
			requireField(t, r, "status")
		})
	}
}

// ---------- 分页 ----------

// TestM5ReturnListPaging 走 §4 那条统一分页：?page=1&page_size=20，上限 100。
func TestM5ReturnListPaging(t *testing.T) {
	l := setupM5Lists(t)

	// 拾主的 #29 有两条，一页一条正好能验顺序和 total 不变
	p1 := returnList(t, "received", "?page_size=1", l.st.finder.Token)
	if p1.Page != 1 || p1.PageSize != 1 || p1.Total != 2 || len(p1.List) != 1 {
		t.Fatalf("第 1 页形状不对：%+v", p1)
	}
	p2 := returnList(t, "received", "?page_size=1&page=2", l.st.finder.Token)
	if len(p2.List) != 1 || p2.List[0].ID == p1.List[0].ID {
		t.Errorf("第 2 页翻出了和第 1 页相同的行：%+v vs %+v", p2.List, p1.List)
	}
	if p2.Total != 2 {
		t.Errorf("第 2 页的 total=%d，翻页不该改总数", p2.Total)
	}
	// 顺序是 submitted_at DESC, id DESC：路人那条最后提交，必须排最前
	wantReturnIDs(t, "两页拼起来的顺序",
		returnPageView{List: append(p1.List, p2.List...)}, l.fromStra, l.fromLi)

	// 越界的一页：空数组 + 真实 total，不报错也不返回 null
	out := returnList(t, "received", "?page=9", l.st.finder.Token)
	if out.List == nil || len(out.List) != 0 || out.Total != 2 {
		t.Errorf("越界页应当是 {list:[], total:2}，实际 %+v", out)
	}

	// 参数边界：page_size 的上限是 maxPageSize=100，越界一律 VALIDATION 并且点名是哪个字段
	for _, bad := range []struct{ query, field string }{
		{"?page=abc", "page"},
		{"?page=0", "page"},
		{"?page=-1", "page"},
		{"?page_size=abc", "page_size"},
		{"?page_size=0", "page_size"},
		{"?page_size=-1", "page_size"},
		{"?page_size=101", "page_size"},
	} {
		r := returnListRaw(t, "received", bad.query, l.st.finder.Token)
		t.Run(bad.query, func(t *testing.T) {
			RequireCode(t, r, apperr.CodeValidation)
			requireField(t, r, bad.field)
		})
	}
}

// TestM5ReturnListsRequireAuth 是这两个端点的鉴权底线：它们是「我的」列表，
// 没有 token 就没有「我」。
func TestM5ReturnListsRequireAuth(t *testing.T) {
	l := setupM5Lists(t)
	before := harness.Count(t, `SELECT count(*) FROM item_returns`)

	for _, kind := range []string{"submitted", "received"} {
		r := returnListRaw(t, kind, "", "")
		t.Run(kind+" 不带 token", func(t *testing.T) {
			RequireCode(t, r, apperr.CodeUnauthorized)
			if r.HTTPStatus != http.StatusUnauthorized {
				t.Errorf("期望 HTTP 401，实际 %d", r.HTTPStatus)
			}
		})
	}
	// 读列表是纯读：不该推进任何状态，也不该动任何一行
	if got := harness.Count(t, `SELECT count(*) FROM item_returns`); got != before {
		t.Errorf("读列表把 item_returns 从 %d 行改成了 %d 行", before, got)
	}
	for _, id := range []int64{l.fromLi, l.fromStra, l.onWang} {
		if row := returnRow(t, id); row["status"] != model.ReturnStatusPending {
			t.Errorf("记录 %d 被读列表的操作推进成了 %v", id, row["status"])
		}
	}
}

// ---------- #33 积分流水 ----------

// TestM5CreditLogsExplainTheScore 是 §3.6 那条设计的可执行形式：
// 「credit_score 是一个被夹过的累计值，单看那个数字永远解释不了我为什么是 110」。
func TestM5CreditLogsExplainTheScore(t *testing.T) {
	st := setupM5(t)
	submitted := requireSubmitted(t, st.found.ID, st.li)
	const note = "确实是我的钱包"
	RequireOK(t, confirmReturn(t, submitted.ID, note, st.finder.Token), "确认归还")

	// 拾主：+10，来源就写在这次确认上
	owner := creditHistory(t, st.finder.Token)
	assertSameSet(t, "#33 data", []string{"credit_score", "list", "total", "page", "page_size"},
		jsonKeys(t, creditLogsRaw(t, "", st.finder.Token).Data))
	if owner.CreditScore != 110 || owner.Total != 1 || len(owner.List) != 1 {
		t.Fatalf("拾主的 #33 期望 {credit_score:110, total:1, list 1 条}，实际 %+v", owner)
	}
	row := owner.List[0]
	if row.Delta != model.CreditDeltaReturnOwner {
		t.Errorf("delta=%d，期望 %d", row.Delta, model.CreditDeltaReturnOwner)
	}
	if row.Reason != model.CreditReasonReturnOwner {
		t.Errorf("reason=%q，期望 %q", row.Reason, model.CreditReasonReturnOwner)
	}
	if row.RefType == nil || *row.RefType != model.CreditRefTypeReturn {
		t.Errorf("ref_type=%v，期望 %q", row.RefType, model.CreditRefTypeReturn)
	}
	if row.RefID == nil || *row.RefID != submitted.ID {
		t.Errorf("ref_id=%v，期望那条归还确认 %d", row.RefID, submitted.ID)
	}
	// 「我为什么是 110」这句话的全部依据：这一行能读出一个时间、一个原因、一个对象
	ts := parseAPITime(t, "created_at", row.CreatedAt)
	if time.Since(ts) > time.Hour {
		t.Errorf("created_at=%s，刚发生的加分不该是 %v 之前", row.CreatedAt, time.Since(ts))
	}
	// user_id 刻意不在形状里（model.CreditLogView 那条注释）
	assertSameSet(t, "#33 list 元素",
		[]string{"id", "delta", "reason", "ref_type", "ref_id", "created_at"},
		jsonKeys(t, creditLogRaw(t, st.finder.Token)))

	// 提交人：+2，同一条确认
	sub := creditHistory(t, st.li.Token)
	if sub.CreditScore != 102 || len(sub.List) != 1 {
		t.Fatalf("小李的 #33：%+v", sub)
	}
	if sub.List[0].Delta != model.CreditDeltaReturnSubmitter ||
		sub.List[0].Reason != model.CreditReasonReturnSubmitter {
		t.Errorf("小李那条流水 delta=%d reason=%q", sub.List[0].Delta, sub.List[0].Reason)
	}

	// 路人：什么都没做过，分数还是那个默认值，流水是空数组
	none := creditHistory(t, st.stranger.Token)
	if none.CreditScore != 100 {
		t.Errorf("路人分数=%d，他不该被任何人的确认牵连", none.CreditScore)
	}
	if none.List == nil || len(none.List) != 0 || none.Total != 0 {
		t.Errorf("路人的流水期望 {list:[], total:0}，实际 %+v", none)
	}

	// 这条对账从**接口**跑一遍：Σ(自己的流水) + 100 == 自己看到的 credit_score。
	// 前面用 SQL 对过一次账（reconcileAllCredits），那验的是库自洽；
	// 这一条验的是「用户从 #33 能拼出正确答案」，两者红的原因不一样。
	for name, page := range map[string]creditHistoryView{
		"拾主": owner, "小李": sub, "路人": none,
	} {
		var sum int
		for _, l := range page.List {
			sum += l.Delta
		}
		if page.CreditScore != 100+sum {
			t.Errorf("%s：接口给的 credit_score=%d，但用同一个接口的 list 算出来是 %d",
				name, page.CreditScore, 100+sum)
		}
	}

	// 拒绝和撤销都不该留下流水（这条在 #24/#26 那边从库断言过，这里从用户视角再断一次）。
	// 帖子得换一条：上面那次 confirm 已经把 st.found 关掉了，而关掉的帖子不再收新确认。
	post := createItem(t, st.finder,
		foundWalletBody("在图书馆捡到一张校园卡", "校园卡，卡套是蓝色的", locOther, "一楼服务台"))
	rejected := requireSubmitted(t, post.ID, st.stranger)
	RequireOK(t, rejectReturn(t, rejected.ID, "不是我的东西", st.finder.Token), "拒绝路人那条")
	if after := creditHistory(t, st.finder.Token); len(after.List) != 1 || after.CreditScore != 110 {
		t.Errorf("拒绝一次之后拾主的流水变成 %+v", after)
	}
	if after := creditHistory(t, st.stranger.Token); after.CreditScore != 100 || len(after.List) != 0 {
		t.Errorf("被拒绝的那条的提交人流水变成 %+v，拒绝不扣分", after)
	}

	// 撤销同样留下一条 cancelled 记录却不留流水
	resubmitted := requireSubmitted(t, post.ID, st.stranger)
	RequireOK(t, cancelReturn(t, resubmitted.ID, st.stranger.Token), "撤销重新提交的那条")
	if after := creditHistory(t, st.finder.Token); len(after.List) != 1 || after.CreditScore != 110 {
		t.Errorf("撤销之后拾主的流水变成 %+v", after)
	}
	if after := creditHistory(t, st.stranger.Token); after.CreditScore != 100 || len(after.List) != 0 {
		t.Errorf("撤销之后提交人的流水变成 %+v", after)
	}
}

// creditLogsRaw / creditHistory 是 #33 的两个读取入口：
// 一个给键集合断言用（要原始 JSON），一个给内容断言用（要解好的结构）。
func creditLogsRaw(t *testing.T, query, token string) Response {
	t.Helper()
	return harness.Get(t, "/api/my/credit-logs"+query, token)
}

func creditHistory(t *testing.T, token string) creditHistoryView {
	t.Helper()
	r := creditLogsRaw(t, "?page_size=100", token)
	RequireOK(t, r, "GET /api/my/credit-logs")
	var page creditHistoryView
	r.DataInto(t, &page)
	return page
}

// creditLogRaw 取 data.list 的第一个元素（原始 JSON），给键集合断言用。
func creditLogRaw(t *testing.T, token string) json.RawMessage {
	t.Helper()
	raw := jsonKeys(t, creditLogsRaw(t, "?page_size=100", token).Data)["list"]
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("list 不是数组: %v\n%s", err, truncate(string(raw)))
	}
	if len(items) == 0 {
		t.Fatal("list 是空的，取不到第一个元素")
	}
	return items[0]
}

// TestM5CreditLogsIgnoreForeignParams 检查 #33 的两个人都能踩的坑：
// 让别人指定「查谁的流水」，以及分数和流水来自两次不同的读。
//
// 第二个坑在这个测试里表现为：?user_id= 如果生效，用户会拿到别人的 list
// 配着自己的 credit_score —— 一个自相矛盾的响应，而它比单纯泄漏更难被发现。
func TestM5CreditLogsIgnoreForeignParams(t *testing.T) {
	st := setupM5(t)
	submitted := requireSubmitted(t, st.found.ID, st.li)
	RequireOK(t, confirmReturn(t, submitted.ID, "", st.finder.Token), "确认归还")

	// 小李去要拾主的账本
	r := creditLogsRaw(t, "?user_id="+itoa(st.finder.UserID), st.li.Token)
	RequireOK(t, r, "?user_id= 应当被忽略而不是报错")
	var page creditHistoryView
	r.DataInto(t, &page)
	if page.CreditScore != 102 {
		t.Errorf("小李带着 ?user_id= 拿到的 credit_score=%d，他自己的是 102", page.CreditScore)
	}
	if len(page.List) != 1 || page.List[0].Reason != model.CreditReasonReturnSubmitter {
		t.Errorf("小李拿到的流水是 %+v，里面混进了别人的记录", page.List)
	}

	// 匿名一律 401
	anon := creditLogsRaw(t, "", "")
	RequireCode(t, anon, apperr.CodeUnauthorized)

	// 分页参数照 §4 的统一规则
	for _, bad := range []struct{ query, field string }{
		{"?page=abc", "page"},
		{"?page_size=0", "page_size"},
		{"?page_size=101", "page_size"},
	} {
		r := creditLogsRaw(t, bad.query, st.finder.Token)
		t.Run(bad.query, func(t *testing.T) {
			RequireCode(t, r, apperr.CodeValidation)
			requireField(t, r, bad.field)
		})
	}
}

// TestM5CreditLogsRenderNullRef 钉住 ref_type / ref_id 为 NULL 时**键仍然存在**。
//
// 这张表允许「不挂任何单据」的流水行（夹具的 smoke_topup 就是这种，
// 将来 admin 手工补分也是这种）。如果 service 用了 omitempty，
// 前端就分不清「这条流水本来没有来源单据」和「后端忘了返回这个字段」，只能猜。
func TestM5CreditLogsRenderNullRef(t *testing.T) {
	st := setupM5(t)
	_, ceil := model.CreditScoreBounds()
	topUpTo(t, st.stranger.UserID, ceil-50)

	page := creditLogsRaw(t, "", st.stranger.Token)
	RequireOK(t, page, "读一条没有来源单据的流水")
	var items []json.RawMessage
	if err := json.Unmarshal(jsonKeys(t, page.Data)["list"], &items); err != nil {
		t.Fatalf("list 不是数组: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("期望 1 行流水，实际 %d", len(items))
	}
	keys := jsonKeys(t, items[0])
	for _, k := range []string{"ref_type", "ref_id"} {
		raw, present := keys[k]
		if !present {
			t.Errorf("%s 这个键整个消失了（omitempty？）", k)
			continue
		}
		if string(raw) != "null" {
			t.Errorf("%s=%s，期望 JSON 的 null", k, string(raw))
		}
	}
}

// TestM5CreditLogsAreNewestFirst 检查排序方向和翻页稳定性。
//
// 一次 confirm 给两个人各记一条流水，created_at 完全相同（同一个事务里同一个 now()），
// 而这两条分属两个用户，所以同一个人内部的并列只会来自「同一个人被记了两次」——
// 这里就是拾主确认两条归还。ORDER BY created_at DESC, id DESC 的第二键
// 让这种并列在翻页时顺序稳定，而稳定的顺序是前端敢用索引当 key 的前提。
func TestM5CreditLogsAreNewestFirst(t *testing.T) {
	st := setupM5(t)

	first := requireSubmitted(t, st.found.ID, st.li)
	RequireOK(t, confirmReturn(t, first.ID, "第一条还给我了", st.finder.Token), "确认第一条")

	// 第二条：换一条帖子，再让拾主确认一次
	post := createItem(t, st.finder,
		foundWalletBody("在图书馆捡到一串钥匙", "一串钥匙，挂着一个小熊挂饰", locOther, "一楼服务台"))
	second := requireSubmitted(t, post.ID, st.stranger)
	RequireOK(t, confirmReturn(t, second.ID, "是我的钥匙", st.finder.Token), "确认第二条")

	page := creditHistory(t, st.finder.Token)
	if page.CreditScore != 120 || len(page.List) != 2 {
		t.Fatalf("拾主确认两次之后：%+v", page)
	}
	// created_at DESC + id DESC ⇒ 后一次确认的流水在前
	if page.List[0].RefID == nil || *page.List[0].RefID != second.ID {
		t.Errorf("第 0 条指向确认 %v，期望最新那条 %d", page.List[0].RefID, second.ID)
	}
	if page.List[1].RefID == nil || *page.List[1].RefID != first.ID {
		t.Errorf("第 1 条指向确认 %v，期望较早那条 %d", page.List[1].RefID, first.ID)
	}

	// 一页一条翻两页，拼起来必须和一次拿到的一样
	p1 := creditLogsRaw(t, "?page_size=1", st.finder.Token)
	RequireOK(t, p1, "流水第 1 页")
	p2 := creditLogsRaw(t, "?page_size=1&page=2", st.finder.Token)
	RequireOK(t, p2, "流水第 2 页")
	var one, two creditHistoryView
	p1.DataInto(t, &one)
	p2.DataInto(t, &two)
	if len(one.List) != 1 || len(two.List) != 1 {
		t.Fatalf("翻页拿到 %d / %d 条", len(one.List), len(two.List))
	}
	if one.List[0].ID != page.List[0].ID || two.List[0].ID != page.List[1].ID {
		t.Errorf("翻页顺序和整页不一致：%d,%d vs %d,%d",
			one.List[0].ID, two.List[0].ID, page.List[0].ID, page.List[1].ID)
	}
	// total 在每一页都是全量（前端靠它算页码）
	if one.Total != 2 || two.Total != 2 {
		t.Errorf("两页的 total 分别是 %d / %d，都该是 2", one.Total, two.Total)
	}
	// 而分数仍然是同一个来源（同页响应里带，不用再拉一次 #5）
	if one.CreditScore != 120 || two.CreditScore != 120 {
		t.Errorf("分页响应里的 credit_score 是 %d / %d", one.CreditScore, two.CreditScore)
	}

	// 这条对账在「两次确认」之后仍然成立（夹到 200 顶之前每次都成立）
	reconcileAllCredits(t)
}

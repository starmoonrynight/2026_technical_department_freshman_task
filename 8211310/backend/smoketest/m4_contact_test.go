package smoketest

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"lostfound/internal/apperr"
)

// 本文件是计划 §10 的第②层里 M4 的第一段：#21 解锁联系方式、#15 的已解锁分支、
// #22 解锁名单。逐条对照 §12 的 M4 冒烟判据（前半段）：
//
//	① 未登录看 found 帖详情 → contact 为 null 且 contact_locked=true
//	② 未登录看 lost 帖详情  → contact 可见
//	③ 登录后解锁 → 拿到 contact，**且 notifications 表没有新增行**
//	④ 同一人解锁两次 → contact_views 只有一行（幂等）
//	⑤ 第二个用户也能解锁成功（非排他）
//	⑥ 对 lost 帖调解锁接口 → VALIDATION
//	⑦ 发帖人查 contact-views 看到两个用户
//	⑧ 非发帖人查 → FORBIDDEN
//
// 判据链 TestM4CriterionChain 按这个顺序走一遍（状态是连续的，跨步骤的行为只有链能测到），
// 其余测试各自钉住边界：鉴权、closed/deleted、软删后的可见性、分页、真实姓名的泄漏面。

// ---------- 响应形状（测试包里重新声明，理由见 m2_items_test.go 顶部）----------

type unlockView struct {
	Contact         string `json:"contact"`
	UnlockedAt      string `json:"unlocked_at"`
	AlreadyUnlocked bool   `json:"already_unlocked"`
}

type unlockerView struct {
	ID       int64  `json:"id"`
	Nickname string `json:"nickname"`
	RealName string `json:"real_name"`
}

type contactViewEntry struct {
	ID        int64        `json:"id"`
	User      unlockerView `json:"user"`
	CreatedAt string       `json:"created_at"`
}

type contactViewPage struct {
	List     []contactViewEntry `json:"list"`
	Total    int                `json:"total"`
	Page     int                `json:"page"`
	PageSize int                `json:"page_size"`
}

// ---------- 夹具 ----------

// SetRealName 直接给某个用户写 real_name。
//
// 为什么必须直接改库：real_name 只有 SSO（M8）才会填，注册和 #5 都碰不到这一列，
// 而 #22 那份名单的全部意义就是「真实姓名 + 昵称」—— 不预置它，
// 「发帖人看到的名单里有真实姓名」这条断言就只能对着空串打勾。
// 和 Ban / MakeAdmin 同一类：改库的是测试的**前置事实**，不是被测行为。
func (h *Harness) SetRealName(t *testing.T, userID int64, realName string) {
	t.Helper()
	if _, err := h.Pool.Exec(context.Background(),
		`UPDATE users SET real_name = $2, updated_at = now() WHERE id = $1`, userID, realName); err != nil {
		t.Fatalf("给 %d 预置真实姓名失败: %v", userID, err)
	}
}

func unlockContact(t *testing.T, itemID int64, token string) Response {
	t.Helper()
	return harness.Do(t, http.MethodPost, "/api/items/"+itoa(itemID)+"/unlock-contact", nil, token)
}

func requireUnlocked(t *testing.T, itemID int64, token, wantContact string) unlockView {
	t.Helper()
	r := unlockContact(t, itemID, token)
	RequireOK(t, r, "POST unlock-contact "+itoa(itemID))
	var v unlockView
	r.DataInto(t, &v)
	if v.Contact != wantContact {
		t.Fatalf("解锁返回的 contact 期望 %q，实际 %q", wantContact, v.Contact)
	}
	return v
}

func contactViewRowCount(t *testing.T, itemID int64) int {
	t.Helper()
	return harness.Count(t, `SELECT count(*) FROM contact_views WHERE item_id = $1`, itemID)
}

func fetchContactViews(t *testing.T, itemID int64, query, token string) Response {
	t.Helper()
	return harness.Get(t, "/api/items/"+itoa(itemID)+"/contact-views"+query, token)
}

func contactViewsOf(t *testing.T, itemID int64, token string) contactViewPage {
	t.Helper()
	r := fetchContactViews(t, itemID, "", token)
	RequireOK(t, r, "GET contact-views")
	var p contactViewPage
	r.DataInto(t, &p)
	return p
}

func nicknamesOf(p contactViewPage) []string {
	out := make([]string, 0, len(p.List))
	for _, e := range p.List {
		out = append(out, e.User.Nickname)
	}
	return out
}

// ---------- 判据链 ----------

// TestM4CriterionChain 是 §12 里 M4 那半条判据的可执行形式。
//
// 顺序是设计过的：found 帖先建、lost 帖后建 —— 因为通知只在「found 帖创建」
// 这条路径上产生（§5.8），如果反过来建，链上就多了一条与本里程碑无关的通知，
// 判据③那句「解锁后 notifications 零新增」就会被那条无关的行糊过去。
func TestM4CriterionChain(t *testing.T) {
	harness.TruncateAll(t)

	const password = "correct-horse-battery"
	const foundContact = "13800000000"

	owner := harness.RegisterAndLogin(t, "m4owner", password)
	li := harness.RegisterAndLogin(t, "m4li", password)
	wang := harness.RegisterAndLogin(t, "m4wang", password)
	never := harness.RegisterAndLogin(t, "m4never", password)
	harness.SetRealName(t, li.UserID, "李某某")
	harness.SetRealName(t, wang.UserID, "王小明")

	found := createItem(t, owner, foundBody("在图书馆捡到一个卡包", foundContact))
	lost := createItem(t, li, lostBody("丢了一台 iPhone", "13900000000"))

	// ① 未登录看 found 帖详情 → contact 是 JSON null、contact_locked=true
	anon := fetchDetail(t, found.ID, "")
	if anon.Contact != nil {
		t.Errorf("未登录就该看到 null，实际拿到了 %q", *anon.Contact)
	}
	if !anon.ContactLocked {
		t.Error("found 帖对匿名读者的 contact_locked 应该是 true")
	}
	// 判据②：未登录看 lost 帖详情 → contact 可见（丢东西的人巴不得被联系上）
	anonLost := fetchDetail(t, lost.ID, "")
	if anonLost.Contact == nil || *anonLost.Contact != "13900000000" {
		t.Errorf("lost 帖的 contact 对匿名读者应该是公开的，实际 %v", anonLost.Contact)
	}
	if anonLost.ContactLocked {
		t.Error("lost 帖的 contact_locked 永远不该是 true")
	}

	// ③ 小李登录后解锁 → 拿到 contact
	//    ⚠ 同一条断言里必须同时钉住 notifications 零新增（判据③的后半句）
	first := requireUnlocked(t, found.ID, li.Token, foundContact)
	if first.AlreadyUnlocked {
		t.Error("第一次解锁不该 already_unlocked=true")
	}
	if first.UnlockedAt == "" {
		t.Error("第一次解锁必须有 unlocked_at")
	}
	if got := contactViewRowCount(t, found.ID); got != 1 {
		t.Errorf("解锁一次之后 contact_views 期望 1 行，实际 %d", got)
	}
	if got := harness.Count(t, `SELECT count(*) FROM notifications`); got != 0 {
		t.Errorf("解锁写了 %d 条通知 —— `contact_unlocked` 这个 type 在第 4 版就被删掉了（§16），"+
			"解锁是用户自己的动作，没有任何人需要被告知", got)
	}

	// ④ 同一人解锁两次 → 仍然只有一行
	second := requireUnlocked(t, found.ID, li.Token, foundContact)
	if !second.AlreadyUnlocked {
		t.Error("第二次解锁必须 already_unlocked=true")
	}
	// ⚠ 时刻必须是第一次那个，不是 now()。这份名单的用途之一是
	// 「这个人从什么时候开始盯上我的帖子」，第二次覆盖成当前时间就把它毁掉了。
	if second.UnlockedAt != first.UnlockedAt {
		t.Errorf("重复解锁的 unlocked_at 变了：第一次 %q，第二次 %q", first.UnlockedAt, second.UnlockedAt)
	}
	if got := contactViewRowCount(t, found.ID); got != 1 {
		t.Errorf("同一个人解锁两次之后期望 1 行（幂等），实际 %d —— 名单会被同一个人刷满", got)
	}

	// ⑤ 第二个用户也能解锁成功（定位原则 3：认领非排他）
	//     这条判据防的是「第一个人解锁之后别人就解不开」那种排他实现 ——
	//     数据层连状态列都没有，所以它只能从 HTTP 层证。
	wangFirst := requireUnlocked(t, found.ID, wang.Token, foundContact)
	if wangFirst.AlreadyUnlocked {
		t.Error("小王第一次解锁，不该因为小李已经解锁过就报 already_unlocked")
	}
	if got := contactViewRowCount(t, found.ID); got != 2 {
		t.Errorf("两个人各解锁一次，期望 2 行，实际 %d", got)
	}

	// ⑥ 对 lost 帖调解锁接口 → VALIDATION，且一行都不写
	//     （用 never 这个路人来解锁小李的 lost 帖，避开「作者是本人」那条捷径）
	r := unlockContact(t, lost.ID, never.Token)
	RequireCode(t, r, apperr.CodeValidation)
	if got := contactViewRowCount(t, lost.ID); got != 0 {
		t.Errorf("被拒的解锁请求写了 %d 行 contact_views", got)
	}

	// ③ 的后半句：解锁之后，同一个人再读详情，联系方式就在了
	//     这是 §4 那四行规则里 M4 新接上的第三条，也是判据①的反面。
	afterLi := fetchDetail(t, found.ID, li.Token)
	if afterLi.Contact == nil || *afterLi.Contact != foundContact {
		t.Errorf("小李解锁过之后读详情应该拿到 contact，实际 %v", afterLi.Contact)
	}
	if afterLi.ContactLocked {
		t.Error("已经解锁过的人，contact_locked 必须变成 false")
	}

	// 没解锁过的人不受影响（非排他的另一面：解锁是每个人的独立事实）
	otherSide := fetchDetail(t, found.ID, never.Token)
	if otherSide.Contact != nil || !otherSide.ContactLocked {
		t.Errorf("没解锁过的人拿到了联系方式：%+v", otherSide)
	}

	// ⑦ 发帖人查 contact-views → 看到两个用户
	views := contactViewsOf(t, found.ID, owner.Token)
	if views.Total != 2 || len(views.List) != 2 {
		t.Fatalf("名单期望 2 个人，实际 total=%d list=%d", views.Total, len(views.List))
	}
	// 倒序：小王是后解锁的那个
	if got := nicknamesOf(views); got[0] != "m4wang" || got[1] != "m4li" {
		t.Errorf("名单应该按解锁时间倒序，实际 %v", got)
	}
	// ⚠ 真实姓名只在这一个接口里出现（§3.2：全项目唯一一处）
	if views.List[0].User.RealName != "王小明" || views.List[1].User.RealName != "李某某" {
		t.Errorf("名单里没有真实姓名，发帖人就看不懂这份名单：%+v", views.List)
	}
	if views.List[0].User.ID != wang.UserID || views.List[1].User.ID != li.UserID {
		t.Errorf("名单里的 user.id 和解锁的人对不上：%+v", views.List)
	}
	if views.List[0].ID == 0 || views.List[0].CreatedAt == "" {
		t.Errorf("名单每行都要有 contact_views 自己的行 id 和时间（治理时要用它引用）：%+v", views.List[0])
	}

	// admin 也能看（§4 第 22 行的鉴权列：发帖人或 Admin）
	admin := harness.MakeAdmin(t, harness.RegisterAndLogin(t, "m4admin", password))
	if got := contactViewsOf(t, found.ID, admin.Token); got.Total != 2 {
		t.Errorf("admin 查名单期望 2 行，实际 %d", got.Total)
	}

	// ⑧ 非发帖人查 → FORBIDDEN
	RequireCode(t, fetchContactViews(t, found.ID, "", never.Token), apperr.CodeForbidden)
	// ⚠ 解锁过的人也不能看名单。这条容易写错：
	// 「我都解锁过你的联系方式了，看看还有谁解锁过不过分吧」——
	// 但那份名单是发帖人的隐私，不是解锁者的福利。
	RequireCode(t, fetchContactViews(t, found.ID, "", li.Token), apperr.CodeForbidden)

	// 判据②之外再钉一次：真实姓名不能从详情接口泄漏
	for _, who := range []string{"", li.Token, owner.Token} {
		r := harness.Get(t, "/api/items/"+itoa(found.ID), who)
		if strings.Contains(string(r.Data), "李某某") || strings.Contains(string(r.Data), "王小明") {
			t.Errorf("真实姓名出现在了 #15 的响应里（token=%q）：\n%s", who, truncate(string(r.Data)))
		}
	}

	// 通知数在这整条链上始终是 0 —— 解锁、看名单、重复解锁都不该发明通知
	if got := harness.Count(t, `SELECT count(*) FROM notifications`); got != 0 {
		t.Errorf("整条 M4 链跑完多出 %d 条通知", got)
	}
}

// ---------- 鉴权与路由形状 ----------

// TestM4UnlockRequiresAuth 是 #21 的 401 面。
//
// 它是全站唯一一个会把别人隐私交出去的端点，未登录必须一个字都拿不到。
func TestM4UnlockRequiresAuth(t *testing.T) {
	harness.TruncateAll(t)
	owner := harness.RegisterAndLogin(t, "m4authowner", "correct-horse-battery")
	found := createItem(t, owner, foundBody("鉴权用的拾物帖", "wx_secret_contact"))

	r := unlockContact(t, found.ID, "")
	RequireCode(t, r, apperr.CodeUnauthorized)
	if got := contactViewRowCount(t, found.ID); got != 0 {
		t.Errorf("没有 token 却写了 %d 行", got)
	}

	// 伪造的 token 也一样进不来（和 m1 那条同源检查）
	r = unlockContact(t, found.ID, "not-a-real-token")
	RequireCode(t, r, apperr.CodeUnauthorized)
	r = unlockContact(t, found.ID, forgeTokenWithAnotherSecret(t, owner.UserID))
	RequireCode(t, r, apperr.CodeUnauthorized)
	if got := contactViewRowCount(t, found.ID); got != 0 {
		t.Errorf("无效 token 写了 %d 行", got)
	}

	// 被封禁的用户：JWT 中间件在查库时拦住，不留解锁记录
	banned := harness.RegisterAndLogin(t, "m4banned", "correct-horse-battery")
	harness.Ban(t, banned.UserID)
	RequireCode(t, unlockContact(t, found.ID, banned.Token), apperr.CodeUserBanned)
	if got := contactViewRowCount(t, found.ID); got != 0 {
		t.Errorf("被封禁的用户写了 %d 行解锁记录", got)
	}

	// 帖子不存在（含 id 不是数字）→ NOT_FOUND，走的是 handler 的 pathID
	for _, bad := range []string{"999999", "abc", "0", "-1"} {
		RequireCode(t, harness.Do(t, http.MethodPost, "/api/items/"+bad+"/unlock-contact", nil, owner.Token),
			apperr.CodeNotFound)
	}
}

// TestM4UnlockIsPostOnly 钉住 #21 只接受 POST。
//
// 这条不是风格问题：GET 会被浏览器预取、被爬虫抓、被缓存中间件重放。
// 如果解锁做成 GET，一次「随便点点」就会在 contact_views 里制造记录，
// 而那份名单是发帖人用来判断「谁在盯着我的帖子」的证据 —— 证据被污染了就废了。
func TestM4UnlockIsPostOnly(t *testing.T) {
	harness.TruncateAll(t)
	owner := harness.RegisterAndLogin(t, "m4method", "correct-horse-battery")
	found := createItem(t, owner, foundBody("方法用错的解锁帖", "13611112222"))

	RequireCode(t, harness.Get(t, "/api/items/"+itoa(found.ID)+"/unlock-contact", owner.Token),
		apperr.CodeMethodNotAllowed)
	if got := contactViewRowCount(t, found.ID); got != 0 {
		t.Errorf("一次 GET 就写了 %d 行解锁记录", got)
	}

	// #22 反过来：它是只读的，所以不该接受写方法
	RequireCode(t, harness.Do(t, http.MethodPost, "/api/items/"+itoa(found.ID)+"/contact-views", nil, owner.Token),
		apperr.CodeMethodNotAllowed)
}

// ---------- closed / deleted ----------

// TestM4UnlockOnClosedFoundPost 验 ITEM_CLOSED：东西已经还回去了，
// 这时候解锁联系方式只会给拾主添骚扰。
//
// 时序刻意是「先解锁、后关帖」，因为这条要同时证两件事：
//   - 关帖**挡住新的**解锁（判据的另一半在 TestM4UnlockRequiresAuth 那种 401 里）
//   - 关帖**不追溯**已经正当拿到的事实 —— 小李在帖子还开着的时候解锁过，
//     那条 contact_views 行是历史，帖子状态变了也不该把它收回。
func TestM4UnlockOnClosedFoundPost(t *testing.T) {
	harness.TruncateAll(t)
	const password = "correct-horse-battery"
	const contact = "13722223333"

	owner := harness.RegisterAndLogin(t, "m4closed", password)
	early := harness.RegisterAndLogin(t, "m4closedearly", password)
	late := harness.RegisterAndLogin(t, "m4closedlate", password)

	found := createItem(t, owner, foundBody("已经还回去的钱包", contact))

	// 帖子还开着的时候，早来的人正当解锁
	requireUnlocked(t, found.ID, early.Token, contact)

	RequireOK(t, harness.Do(t, http.MethodPatch, "/api/items/"+itoa(found.ID)+"/status",
		map[string]any{"status": "closed"}, owner.Token), "关闭帖子")

	// 关帖之后，没解锁过的人解不开，而且一行都不写
	r := unlockContact(t, found.ID, late.Token)
	RequireCode(t, r, apperr.CodeItemClosed)
	if got := contactViewRowCount(t, found.ID); got != 1 {
		t.Errorf("被 ITEM_CLOSED 拒掉的解锁把行数从 1 变成了 %d", got)
	}
	if d := fetchDetail(t, found.ID, late.Token); d.Contact != nil || !d.ContactLocked {
		t.Errorf("没解锁过的人在读到一条 closed 帖时拿到了联系方式：%+v", d)
	}

	// 作者自己不受影响：他改电话、事后核对之类的事仍然能看到自己的联系方式
	if d := fetchDetail(t, found.ID, owner.Token); d.Contact == nil {
		t.Error("closed 帖的作者应该仍然看得到自己的 contact")
	}

	// 已经解锁过的人**不**被反向夺走
	if d := fetchDetail(t, found.ID, early.Token); d.Contact == nil || *d.Contact != contact {
		t.Errorf("早先正当解锁的人在帖子关闭后拿不到 contact 了：%+v", d)
	}
	// 他再点一次 #21 仍然得到 ITEM_CLOSED —— 这不是 bug：详情页已经把 contact
	// 给他了，#21 的作用只发生在「第一次」，而第一次的门禁就是帖子状态。
	// 把它钉成断言是为了让将来改动这条顺序的人知道这是取舍而不是遗漏。
	RequireCode(t, unlockContact(t, found.ID, early.Token), apperr.CodeItemClosed)
	if got := contactViewRowCount(t, found.ID); got != 1 {
		t.Errorf("ITEM_CLOSED 之后名单行数变了：%d", got)
	}

	// 名单在帖子关闭之后仍然可读（发帖人要看的正是「关之前谁来找过我」）
	if p := contactViewsOf(t, found.ID, owner.Token); p.Total != 1 {
		t.Errorf("closed 帖的名单期望 1 行，实际 %d", p.Total)
	}
}

// TestM4UnlockOnDeletedPost 验「软删的帖对外不存在」这条纪律在 #21 上同样成立。
//
// 顺序很关键：deleted 必须排在类型判断和状态判断前面。
// 排错了的话，一条被下架的 found 帖会得到 ITEM_CLOSED 或 VALIDATION ——
// 那等于向外确认「这条帖子存在过，而且我们知道它是什么类型」。
func TestM4UnlockOnDeletedPost(t *testing.T) {
	harness.TruncateAll(t)
	owner := harness.RegisterAndLogin(t, "m4delowner", "correct-horse-battery")
	stranger := harness.RegisterAndLogin(t, "m4delreader", "correct-horse-battery")

	// 一条 deleted 的 lost 帖：如果顺序排错，它会得到 VALIDATION
	lostDeleted := createItem(t, owner, lostBody("被下架的失物帖", "13844445555"))
	harness.SetItemStatus(t, lostDeleted.ID, "deleted")
	RequireCode(t, unlockContact(t, lostDeleted.ID, stranger.Token), apperr.CodeNotFound)

	// 一条 deleted 的 found 帖：如果顺序排错，它会得到 ITEM_CLOSED 或正常放行
	foundDeleted := createItem(t, owner, foundBody("被下架的拾物帖", "13844446666"))
	harness.SetItemStatus(t, foundDeleted.ID, "deleted")
	RequireCode(t, unlockContact(t, foundDeleted.ID, stranger.Token), apperr.CodeNotFound)

	for _, id := range []int64{lostDeleted.ID, foundDeleted.ID} {
		if got := contactViewRowCount(t, id); got != 0 {
			t.Errorf("软删的帖子被写了 %d 行解锁记录", got)
		}
	}
}

// TestM4DetailViewAfterDeletion 钉住「先解锁、后发帖人被下架」这种时序：
// 解锁者不该因为帖子被删就拿回联系方式，帖子对外已经不存在了。
func TestM4DetailViewAfterDeletion(t *testing.T) {
	harness.TruncateAll(t)
	owner := harness.RegisterAndLogin(t, "m4d2owner", "correct-horse-battery")
	viewer := harness.RegisterAndLogin(t, "m4d2viewer", "correct-horse-battery")

	found := createItem(t, owner, foundBody("下架之后不该被读到", "13955556666"))
	requireUnlocked(t, found.ID, viewer.Token, "13955556666")

	RequireOK(t, harness.Do(t, http.MethodDelete, "/api/items/"+itoa(found.ID), nil, owner.Token), "本人删帖")

	RequireCode(t, harness.Get(t, "/api/items/"+itoa(found.ID), viewer.Token), apperr.CodeNotFound)
	RequireCode(t, harness.Get(t, "/api/items/"+itoa(found.ID), ""), apperr.CodeNotFound)
	// 作者自己和 admin 还能读到（M2 定的 canSeeDeleted 规则，M4 不许改）
	RequireOK(t, harness.Get(t, "/api/items/"+itoa(found.ID), owner.Token), "作者读自己已删的帖子")
}

// ---------- #22 的分页与形状 ----------

func TestM4ContactViewsPagination(t *testing.T) {
	harness.TruncateAll(t)
	const password = "correct-horse-battery"
	owner := harness.RegisterAndLogin(t, "m4pageowner", password)
	found := createItem(t, owner, foundBody("分页用的拾物帖", "13700001111"))

	unlockers := make([]Session, 0, 3)
	for _, name := range []string{"m4page1", "m4page2", "m4page3"} {
		s := harness.RegisterAndLogin(t, name, password)
		requireUnlocked(t, found.ID, s.Token, "13700001111")
		unlockers = append(unlockers, s)
	}

	// 默认第 1 页 20 条，三条都在
	p := contactViewsOf(t, found.ID, owner.Token)
	if p.Total != 3 || len(p.List) != 3 || p.Page != 1 || p.PageSize != 20 {
		t.Fatalf("默认分页不对：%+v", p)
	}

	// page_size=2 → 第一页 2 条、total 仍然是 3（前端靠 total 算页数）
	r1 := fetchContactViews(t, found.ID, "?page_size=2", owner.Token)
	RequireOK(t, r1, "名单分页")
	var firstPage contactViewPage
	r1.DataInto(t, &firstPage)
	if len(firstPage.List) != 2 || firstPage.Total != 3 || firstPage.PageSize != 2 {
		t.Fatalf("page_size=2 的响应不对：%+v", firstPage)
	}

	r2 := fetchContactViews(t, found.ID, "?page_size=2&page=2", owner.Token)
	RequireOK(t, r2, "名单第 2 页")
	var secondPage contactViewPage
	r2.DataInto(t, &secondPage)
	if len(secondPage.List) != 1 || secondPage.Page != 2 {
		t.Fatalf("第 2 页期望 1 条：%+v", secondPage)
	}
	// 两页不能有重叠，也不能漏人
	seen := map[int64]bool{}
	for _, e := range append(firstPage.List, secondPage.List...) {
		if seen[e.User.ID] {
			t.Errorf("翻页翻出了重复的人：%d", e.User.ID)
		}
		seen[e.User.ID] = true
	}
	if len(seen) != 3 {
		t.Errorf("三页合起来只见到 %d 个人", len(seen))
	}

	// 最晚解锁的那个在第一页第一条（倒序）
	if firstPage.List[0].User.ID != unlockers[2].UserID {
		t.Errorf("倒序排错了，第一条是 %d，期望最后解锁的 %d",
			firstPage.List[0].User.ID, unlockers[2].UserID)
	}

	// 形状：外层 id 是 contact_views 的行 id，不是用户 id
	for _, e := range p.List {
		if e.ID == 0 || e.ID == e.User.ID {
			t.Errorf("名单里有一行的外层 id 可疑（0 或者和用户 id 撞了）：%+v", e)
		}
	}

	// 非法分页参数 → VALIDATION（和全站同一条纪律）
	for _, q := range []string{"?page=0", "?page=abc", "?page_size=0", "?page_size=99999"} {
		RequireCode(t, fetchContactViews(t, found.ID, q, owner.Token), apperr.CodeValidation)
	}

	// 没有一个人解锁过时：list 是 []，total 是 0（不是 null）
	fresh := createItem(t, owner, foundBody("还没人解锁的帖子", "13700002222"))
	rEmpty := fetchContactViews(t, fresh.ID, "", owner.Token)
	RequireOK(t, rEmpty, "空名单")
	if !strings.Contains(string(rEmpty.Data), `"list":[]`) {
		t.Errorf("空名单应该出现 \"list\":[]，实际 %s", truncate(string(rEmpty.Data)))
	}
	if strings.Contains(string(rEmpty.Data), `"list":null`) {
		t.Error("list 是 null，前端 .map 会崩")
	}
}

// TestM4ContactViewsIsReadableAfterOwnerSeesNothingWrong 是 #22 的一条隐私反面：
// SSO 用户（没有真实姓名）也在名单里，那一格是空串而不是整个字段消失。
func TestM4ContactViewsIsNullRealNameStillShows(t *testing.T) {
	harness.TruncateAll(t)
	owner := harness.RegisterAndLogin(t, "m4ssoowner", "correct-horse-battery")
	viewer := harness.RegisterAndLogin(t, "m4ssoviewer", "correct-horse-battery")
	// 刻意**不**预置 real_name：这是本地账号的真实状态（real_name 只有 SSO 会填）

	found := createItem(t, owner, foundBody("名单里有人没有真实姓名", "13700003333"))
	requireUnlocked(t, found.ID, viewer.Token, "13700003333")

	p := contactViewsOf(t, found.ID, owner.Token)
	if len(p.List) != 1 {
		t.Fatalf("名单期望 1 行，实际 %d", len(p.List))
	}
	if p.List[0].User.RealName != "" {
		t.Errorf("没有真实姓名时这一格该是空串，实际 %q", p.List[0].User.RealName)
	}
	// 昵称（回退成用户名）必须在，否则这份名单没法读
	if p.List[0].User.Nickname != "m4ssoviewer" {
		t.Errorf("名单里没有昵称：%+v", p.List[0])
	}
}

// TestM4UnlockAuthorWritesNoRow 是本里程碑最容易被「顺手补一行」破坏的一条：
// 作者解锁自己的帖子时不写 contact_views。
//
// 写进去的后果不是崩溃、不是报错，而是发帖人在自己的名单里看到自己 ——
// 那种假事实只有对着行数才能发现，所以这里数行。
func TestM4UnlockAuthorWritesNoRow(t *testing.T) {
	harness.TruncateAll(t)
	owner := harness.RegisterAndLogin(t, "m4self", "correct-horse-battery")
	found := createItem(t, owner, foundBody("作者自己解锁的帖子", "13700004444"))

	v := requireUnlocked(t, found.ID, owner.Token, "13700004444")
	if !v.AlreadyUnlocked {
		t.Error("作者本来就不需要解锁，already_unlocked 应为 true")
	}
	if v.UnlockedAt != "" {
		t.Errorf("作者名下没有 contact_views 行，unlocked_at 该是空串而不是编造的时刻：%q", v.UnlockedAt)
	}
	if got := contactViewRowCount(t, found.ID); got != 0 {
		t.Errorf("作者解锁往名单里塞了 %d 行假事实", got)
	}
}

// TestM4UnlockIsNonExclusiveForDeletedLeader 是判据⑤的一个边角：
// 名单里的人被下架/删除之后，剩下的人照样能解锁。
//
// 「非排他」不只意味着「第二个人能解锁」，还意味着**没有任何人**能阻止别人解锁 ——
// 如果实现里存在「发帖人可以把某个解锁者拉黑」这种隐藏状态，这条会红。
func TestM4UnlockIsNonExclusiveAfterOneUnlockerLeaves(t *testing.T) {
	harness.TruncateAll(t)
	const password = "correct-horse-battery"
	owner := harness.RegisterAndLogin(t, "m4nxowner", password)
	first := harness.RegisterAndLogin(t, "m4nxfirst", password)
	second := harness.RegisterAndLogin(t, "m4nxsecond", password)

	found := createItem(t, owner, foundBody("第一个人走了还在不在", "13700005555"))
	requireUnlocked(t, found.ID, first.Token, "13700005555")

	// 第一个人被封禁（他解锁过的那一行留在表里 —— 那是已经发生过的事实）
	harness.Ban(t, first.UserID)
	RequireCode(t, unlockContact(t, found.ID, first.Token), apperr.CodeUserBanned)

	requireUnlocked(t, found.ID, second.Token, "13700005555")
	if got := contactViewRowCount(t, found.ID); got != 2 {
		t.Errorf("期望两行（两个人各一次），实际 %d —— 有人被处理时不许抹掉历史解锁记录", got)
	}
}

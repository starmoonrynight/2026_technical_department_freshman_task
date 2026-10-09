package smoketest

import (
	"net/http"
	"strings"
	"testing"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
)

// 本文件是 M5 第②层的第二段：#24 详情、#26 拒绝、#27 撤销，
// 以及那个给整个里程碑定调的负向断言 —— TestRejectDoesNotCloseItem。
//
// repo/item_return.go 和 service/item_return.go 的顶部注释都点名引用了这个文件里的
// TestRejectDoesNotCloseItem。那个名字**不能改**，文件也**不能改名或合并**：
// 那两处注释是后来人读到的唯一警告，而警告说「去看那条测试」。
//
// 这一段的三条主判据（§13 第 6 步）：
//
//	小刘从没解锁过联系方式 → 直接提交归还确认 → 成功
//	→ 小王 reject → items.status 仍然是 'open'（直接 SELECT，不看响应）
//	→ 只有小刘收到 return_rejected，小王自己一条都没有
//	→ 两个人的 credit_score 都不变
//	→ 小刘可以对同一条帖子重新提交

// ---------- 夹具 ----------

// m5Admin 注册一个**专门的** admin，而不是把 st.stranger 提升。
//
// 这不是省事，是正确性：MakeAdmin 改的是库里那一行的 role，
// 提升之后 st.stranger 这个 Session 从此就是 admin 身份 ——
// 于是本文件里所有「路人不该看到」的断言都会因为「他现在是 admin」而失去意义，
// 而 #24 那条「路人 FORBIDDEN」甚至会直接变成失败。
func m5Admin(t *testing.T) Session {
	t.Helper()
	return harness.MakeAdmin(t, harness.RegisterAndLogin(t, "m5admin", m5Password))
}

func decideURL(action string, returnID int64) string {
	return "/api/returns/" + itoa(returnID) + "/" + action
}

func confirmReturn(t *testing.T, returnID int64, note, token string) Response {
	t.Helper()
	return harness.Post(t, decideURL("confirm", returnID), map[string]any{"owner_note": note}, token)
}

func rejectReturn(t *testing.T, returnID int64, note, token string) Response {
	t.Helper()
	return harness.Post(t, decideURL("reject", returnID), map[string]any{"owner_note": note}, token)
}

func cancelReturn(t *testing.T, returnID int64, token string) Response {
	t.Helper()
	// #27 没有请求体（§4 第 27 行那一列写的是「—」），所以这里传 nil。
	// 传 map[string]any{} 也是一种合法请求，但传 nil 才测得出
	// 「handler 有没有偷偷 bind 一个计划里不存在的字段」。
	return harness.Do(t, http.MethodPost, decideURL("cancel", returnID), nil, token)
}

func detailReturn(t *testing.T, returnID int64, token string) Response {
	t.Helper()
	return harness.Get(t, "/api/returns/"+itoa(returnID), token)
}

func requireDetail(t *testing.T, returnID int64, token string) returnDetailView {
	t.Helper()
	r := detailReturn(t, returnID, token)
	RequireOK(t, r, "GET /api/returns/"+itoa(returnID))
	var v returnDetailView
	r.DataInto(t, &v)
	return v
}

// ---------- 本系统最容易实现错的一处 ----------

// TestRejectDoesNotCloseItem 是 §3.4 那个语义边界的守门人。
//
// 规则：**reject 拒绝的是那条归还确认记录，不是那条帖子。**
// 帖子必须仍然是 'open'，因为东西没还回来（或者归还被认定不成立），
// 拾主还在等下一个真正的主人 —— 关掉帖子恰好把这件事做反了。
//
// 断言方式刻意是「直接 SELECT items.status」而不是读 HTTP 响应：
// 关帖那一步真发生的话，发生在 repo.Reject 的事务里，
// 而 #26 的响应体里没有 status 字段可以泄漏这个错误 ——
// 只看响应的测试对一个「多写了一条 UPDATE items」的实现完全无感。
//
// 这一条同时覆盖 §13 第 6 步的全部五个核对项，因为它们是同一次 reject 的后果：
// 状态、归属两列、通知收件人只有提交人、双方分数不变、可以重提。
func TestRejectDoesNotCloseItem(t *testing.T) {
	st := setupM5(t)

	// 前提：提交人从没解锁过那条帖子的联系方式（§13 第 6 步的第一句）。
	if got := contactViewRowCount(t, st.found.ID); got != 0 {
		t.Fatalf("夹具前提不成立：contact_views 有 %d 行", got)
	}
	submitted := requireSubmitted(t, st.found.ID, st.li)

	finderNotifsBefore := notificationsFor(t, st.finder.UserID) // 1 条：return_submitted
	liNotifsBefore := notificationsFor(t, st.li.UserID)         // 1 条：new_match
	finderCredit := creditOf(t, st.finder.UserID)
	liCredit := creditOf(t, st.li.UserID)
	itemBefore := itemStatusOf(t, st.found.ID)
	if itemBefore != model.ItemStatusOpen {
		t.Fatalf("夹具的拾物帖应该是 open，实际 %q", itemBefore)
	}

	const note = "不是我的钱包，里面只有一张公交卡"
	rejected := rejectReturn(t, submitted.ID, note, st.finder.Token)
	RequireOK(t, rejected, "发帖人拒绝归还确认")
	var view decideReturnView
	rejected.DataInto(t, &view)
	if view.ID != submitted.ID {
		t.Errorf("#26 返回的 id=%d，处理的是 %d", view.ID, submitted.ID)
	}
	if view.Status != model.ReturnStatusRejected {
		t.Errorf("期望 status=rejected，实际 %q", view.Status)
	}

	// ① 帖子仍然 open —— 这条是本测试存在的理由，用 SQL 断言
	if got := itemStatusOf(t, st.found.ID); got != model.ItemStatusOpen {
		t.Errorf("reject 之后 items.status=%q，必须是 'open'（§3.4：拒绝的是记录，不是帖子）", got)
	}

	// ② 记录本身：status + 归属两列。review_kind='owner' 这一条很重要 ——
	//    它证明这次推进走的是 service（只有发帖人过得了身份校验的那条路径）。
	row := returnRow(t, submitted.ID)
	if row["status"] != "rejected" {
		t.Errorf("item_returns.status 期望 rejected，实际 %v", row["status"])
	}
	if got := asInt64(t, row["reviewer_id"]); got != st.finder.UserID {
		t.Errorf("reviewer_id 期望 %d，实际 %d", st.finder.UserID, got)
	}
	if row["review_kind"] != model.ReviewKindOwner {
		t.Errorf("review_kind 期望 %q，实际 %#v", model.ReviewKindOwner, row["review_kind"])
	}
	if got := row["owner_note"]; got != note {
		t.Errorf("owner_note 没存下发帖人那句理由：%#v", got)
	}
	if ts, ok := row["reviewed_at"].(string); !ok || len(ts) < 10 {
		t.Errorf("reviewed_at 应该是一个真实时间，实际 %#v", row["reviewed_at"])
	}

	// ③ 通知只发给提交人
	liNotifs := notificationsOf(t, st.li.UserID)
	if len(liNotifs) != liNotifsBefore+1 {
		t.Fatalf("小李期望 +%d 条通知，实际从 %d 变成 %d",
			1, liNotifsBefore, len(liNotifs))
	}
	last := liNotifs[len(liNotifs)-1]
	if last.Type != model.NotificationReturnRejected {
		t.Errorf("提交人收到的类型期望 %q，实际 %q", model.NotificationReturnRejected, last.Type)
	}
	if last.ReturnID == nil || *last.ReturnID != submitted.ID {
		t.Errorf("return_rejected 的 return_id=%v", last.ReturnID)
	}
	// 那句理由必须出现在通知里：那是提交人唯一能得到的解释（§4 第 26 行的设计根据）。
	if !strings.Contains(last.Content, note) {
		t.Errorf("通知里没有发帖人的留言，这条拒绝就没有任何可执行的信息：%q", last.Content)
	}
	if got := notificationsFor(t, st.finder.UserID); got != finderNotifsBefore {
		t.Errorf("发帖人自己收到了 %d 条通知（之前 %d）—— 他不需要被告知自己刚做的事",
			got, finderNotifsBefore)
	}

	// ④ 双方分数都不变：判断错了不罚人（§3.6）
	if got := creditOf(t, st.finder.UserID); got != finderCredit {
		t.Errorf("拒绝之后发帖人分数从 %d 变成 %d", finderCredit, got)
	}
	if got := creditOf(t, st.li.UserID); got != liCredit {
		t.Errorf("拒绝之后提交人分数从 %d 变成 %d", liCredit, got)
	}
	// 流水一行都不该多。断库而不只看分数：
	// 「分数没动但插了一行 delta=0 的流水」是一种会让 §33 那条对账假绿起来的写法。
	if got := harness.Count(t, `SELECT count(*) FROM credit_logs`); got != 0 {
		t.Errorf("拒绝写了 %d 行 credit_logs，一行都不该有", got)
	}

	// ⑤ 同一个人可以重新提交（那条部分唯一索引只管 status='pending'）
	retry := submitReturn(t, st.found.ID, m5Body(t, st.li, m5Message), st.li.Token)
	RequireOK(t, retry, "被拒之后重新提交")
	var again submitReturnView
	retry.DataInto(t, &again)
	if again.ID == submitted.ID {
		t.Errorf("重提拿到了同一个 id，说明那条 rejected 被复用了而不是新增")
	}
	if got := harness.Count(t,
		`SELECT count(*) FROM item_returns WHERE item_id = $1 AND submitter_id = $2`,
		st.found.ID, st.li.UserID); got != 2 {
		t.Errorf("同一个人对同一条帖期望 2 行（一拒一提），实际 %d", got)
	}

	if ids := unreviewedOutsideService(t); len(ids) != 0 {
		t.Errorf("出现了 service 之外推进的归还确认：%v", ids)
	}
}

// ---------- #26 的字段规则 ----------

// TestM5RejectRequiresNote 钉住那个刻意不对称：
// confirm 的 owner_note 可选，reject 的 owner_note **必填**（连纯空白都不算写）。
//
// §4 只给 #26 列了 VALIDATION，没给 #25 列 —— 那一列就是给「没写理由」留的。
func TestM5RejectRequiresNote(t *testing.T) {
	st := setupM5(t)
	submitted := requireSubmitted(t, st.found.ID, st.li)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"完全没有 owner_note 这个键", map[string]any{}},
		{"owner_note 是空串", map[string]any{"owner_note": ""}},
		{"owner_note 只有空格", map[string]any{"owner_note": "   "}},
		{"owner_note 只有换行和制表", map[string]any{"owner_note": "\n\t "}},
		{"owner_note 超过 500 个字", map[string]any{"owner_note": strings.Repeat("特", model.ReturnOwnerNoteMaxChars+1)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := harness.Post(t, decideURL("reject", submitted.ID), c.body, st.finder.Token)
			RequireCode(t, r, apperr.CodeValidation)
			requireField(t, r, "owner_note")
			if row := returnRow(t, submitted.ID); row["status"] != "pending" {
				t.Errorf("被拒的 reject 把记录推进成了 %v", row["status"])
			}
		})
	}

	// 500 个汉字（1500 字节）必须写满成功：CHECK 是 VARCHAR(500)，PostgreSQL 数的是字符。
	// 这一条从 HTTP 走一遍，因为字节/字符这个错位只有在真库上才撞得出来。
	full := rejectReturn(t, submitted.ID, strings.Repeat("特", model.ReturnOwnerNoteMaxChars), st.finder.Token)
	RequireOK(t, full, "500 个汉字的拒绝理由")

	// confirm 的 owner_note 可选：空 body、空串、纯空格、带别的键，四种都必须放行。
	//
	// 每个用例各拿一条新的 pending：共用一条的话，第一个成功就把它变成终态，
	// 后面三个都只能撞 RETURN_ILLEGAL_TRANSITION —— 那是「没报错」而不是「合法」，
	// 而把合法路径测成一堆 409 是最容易自我安慰的一种假绿。
	confirmCases := []struct {
		name string
		body any
	}{
		{"空 body", map[string]any{}},
		{"owner_note 是空串", map[string]any{"owner_note": ""}},
		{"owner_note 是纯空白", map[string]any{"owner_note": "  "}},
		{"完全没有 owner_note 这个键", nil},
	}
	for _, c := range confirmCases {
		st := setupM5(t)
		fresh := requireSubmitted(t, st.found.ID, st.li)
		t.Run("confirm 的 owner_note 可选："+c.name, func(t *testing.T) {
			r := harness.Post(t, decideURL("confirm", fresh.ID), c.body, st.finder.Token)
			RequireOK(t, r, "不写备注就点确认")
			var v confirmReturnView
			r.DataInto(t, &v)
			if v.Status != model.ReturnStatusConfirmed {
				t.Errorf("status=%q", v.Status)
			}
			if got := returnRow(t, fresh.ID)["owner_note"]; got != "" {
				t.Errorf("库里 owner_note=%#v，不写备注应当存成空串", got)
			}
		})
	}
}

// TestM5DecisionOrderPermissionBeforeField 把「权限判断走在字段校验之前」做成一条链。
//
// 这一条是从第①层那条同名测试搬上来的 HTTP 版本，值得重复一次：
// 第①层能证明 service 的顺序，但它证明不了 handler 在 bind 失败时
// 直接回一句 VALIDATION 就走（那正是最容易写出来的顺序）。
// 症状：路人对一条已经 confirmed 的记录点确认，如果先校验字段，
// 他会看到「当前状态不允许此操作」—— 那句话向他确认了这条记录存在且它现在长什么样。
func TestM5DecisionOrderPermissionBeforeField(t *testing.T) {
	st := setupM5(t)
	submitted := requireSubmitted(t, st.found.ID, st.li)

	// 路人 + 超长备注：应该拿到 FORBIDDEN，而不是备注的 VALIDATION
	r := rejectReturn(t, submitted.ID, strings.Repeat("特", model.ReturnOwnerNoteMaxChars+1), st.stranger.Token)
	RequireCode(t, r, apperr.CodeForbidden)

	// 路人 + 空备注（reject 必填）同理
	r = rejectReturn(t, submitted.ID, "", st.stranger.Token)
	RequireCode(t, r, apperr.CodeForbidden)

	// admin + 空备注同理（而且必须是 FORBIDDEN，不是「admin 特殊放行」）
	admin := m5Admin(t)
	r = rejectReturn(t, submitted.ID, "", admin.Token)
	RequireCode(t, r, apperr.CodeForbidden)

	// 提交人自己去 confirm（他是这条记录的主人，但不是帖子的主人）：FORBIDDEN
	r = confirmReturn(t, submitted.ID, "", st.li.Token)
	RequireCode(t, r, apperr.CodeForbidden)

	// 记录不存在时先给 NOT_FOUND，而不是先暴露权限模型
	r = confirmReturn(t, 987_654, "", st.finder.Token)
	RequireCode(t, r, apperr.CodeNotFound)

	// 上面这些一个都没改动任何东西
	if row := returnRow(t, submitted.ID); row["status"] != "pending" {
		t.Errorf("被拒的决定把记录推进成了 %v", row["status"])
	}
	if got := harness.Count(t, `SELECT count(*) FROM credit_logs`); got != 0 {
		t.Errorf("被拒的决定写了 %d 行流水", got)
	}
	if got := itemStatusOf(t, st.found.ID); got != model.ItemStatusOpen {
		t.Errorf("被拒的决定把帖子改成了 %q", got)
	}
}

// ---------- admin 没有审批权（定位原则 5 的代码边界） ----------

// TestM5AdminCannotDecide 是 §12 那句「用 admin token 去 confirm 得 FORBIDDEN
// （关键：admin 无审批权，service 层不看 role）」的完整形式：三个动作各试一遍。
//
// 为什么三个都要试而不是只试 confirm：这条规则的破坏方式从来不是「confirm 放开了」，
// 而是「加了一个 admin 专用的代审路径」。M6 会有 #46（admin 删归还确认），
// 那时最自然的顺手做法就是同时给 admin 一个代 confirm 的能力。
func TestM5AdminCannotDecide(t *testing.T) {
	st := setupM5(t)
	submitted := requireSubmitted(t, st.found.ID, st.li)
	another := requireSubmitted(t, st.found.ID, st.stranger)
	admin := m5Admin(t)

	cases := []struct {
		action string
		id     int64
		body   any
		token  string
	}{
		{"confirm", submitted.ID, map[string]any{"owner_note": "管理员代帖主确认"}, admin.Token},
		{"reject", submitted.ID, map[string]any{"owner_note": "管理员代帖主拒绝"}, admin.Token},
		{"cancel", submitted.ID, nil, admin.Token},
		{"cancel", another.ID, nil, admin.Token},
	}
	for _, c := range cases {
		t.Run("admin "+c.action, func(t *testing.T) {
			r := harness.Do(t, http.MethodPost, decideURL(c.action, c.id), c.body, c.token)
			RequireCode(t, r, apperr.CodeForbidden)
		})
	}

	// 一条都不许被推进：admin 能销毁内容和账号，但不能制造归属。
	for _, id := range []int64{submitted.ID, another.ID} {
		if row := returnRow(t, id); row["status"] != "pending" {
			t.Errorf("归还确认 %d 被 admin 推进成了 %v", id, row["status"])
		}
	}
	if got := itemStatusOf(t, st.found.ID); got != model.ItemStatusOpen {
		t.Errorf("admin 的失败尝试把帖子改成了 %q", got)
	}
	if got := harness.Count(t, `SELECT count(*) FROM credit_logs`); got != 0 {
		t.Errorf("admin 的失败尝试写了 %d 行流水", got)
	}

	// admin 能看（#24 的三方之一），但看不等于能改。
	// 这一对断言放在一起是有意的：它们合起来才是「admin 是审计者不是仲裁者」。
	view := requireDetail(t, submitted.ID, admin.Token)
	if view.Status != "pending" {
		t.Errorf("admin 看到的 status=%q", view.Status)
	}
	if view.Submitter.ID != st.li.UserID {
		t.Errorf("admin 看到的提交人 id=%d，期望 %d", view.Submitter.ID, st.li.UserID)
	}
	// 而联系方式仍然拿不到：嵌套摘要不是绕过 #21 的后门（§4 对 #15 那四条规则的镜像）。
	if view.Item.Contact != nil {
		t.Errorf("admin 通过 #24 的嵌套摘要拿到了联系方式：%q", *view.Item.Contact)
	}
}

// ---------- 状态机：终态没有出边 ----------

// TestM5TerminalStatesHaveNoOutgoingEdge 是 §12 那句「对已 confirmed 的再 reject
// 得 RETURN_ILLEGAL_TRANSITION」的全组合版本：三个终态 × 三个动作。
//
// 谁来做这件事都应当是 409，除了「发帖人对 pending 做 confirm/reject」那两条合法边。
// 这里刻意只让合法的那个角色来点非法动作 —— 因为权限判断排在状态判断前面，
// 用路人点的话得到的是 FORBIDDEN，那测的是另一条规则（上面那条测试已经测过了）。
func TestM5TerminalStatesHaveNoOutgoingEdge(t *testing.T) {
	cases := []struct {
		name   string
		reach  func(t *testing.T, st m5Stage) int64 // 把一条记录推进到某个终态，返回它的 id
		remain string
	}{
		{
			name: "confirmed",
			reach: func(t *testing.T, st m5Stage) int64 {
				id := requireSubmitted(t, st.found.ID, st.li).ID
				RequireOK(t, confirmReturn(t, id, "", st.finder.Token), "到达 confirmed")
				return id
			},
			remain: model.ReturnStatusConfirmed,
		},
		{
			name: "rejected",
			reach: func(t *testing.T, st m5Stage) int64 {
				id := requireSubmitted(t, st.found.ID, st.li).ID
				RequireOK(t, rejectReturn(t, id, "不是我的东西", st.finder.Token), "到达 rejected")
				return id
			},
			remain: model.ReturnStatusRejected,
		},
		{
			name: "cancelled",
			reach: func(t *testing.T, st m5Stage) int64 {
				id := requireSubmitted(t, st.found.ID, st.li).ID
				RequireOK(t, cancelReturn(t, id, st.li.Token), "到达 cancelled")
				return id
			},
			remain: model.ReturnStatusCancelled,
		},
	}

	// 每个动作都由「对这个动作本来有权限的人」来点：confirm / reject 是发帖人，
	// cancel 是提交人。这是这一组组合能不能测到状态机的前提 ——
	// 换错的人只会得到 FORBIDDEN（权限排在状态前面），
	// 于是那格测的是权限而不是状态机，而 9 格里会有 3 格是重复的权限测试。
	actorFor := func(action string, st m5Stage) string {
		if action == "cancel" {
			return st.li.Token
		}
		return st.finder.Token
	}

	for _, c := range cases {
		for _, action := range []string{"confirm", "reject", "cancel"} {
			// 每个 (终态 × 动作) 组合都要一个干净的舞台
			st := setupM5(t)
			id := c.reach(t, st)

			var body any = map[string]any{"owner_note": "再点一次"}
			if action == "cancel" {
				body = nil
			}
			r := harness.Do(t, http.MethodPost, decideURL(action, id), body, actorFor(action, st))
			t.Run(c.name+" 之后 "+action, func(t *testing.T) {
				RequireCode(t, r, apperr.CodeReturnIllegalTransition)
				if r.HTTPStatus != http.StatusConflict {
					t.Errorf("RETURN_ILLEGAL_TRANSITION 期望 HTTP 409，实际 %d", r.HTTPStatus)
				}
				if row := returnRow(t, id); row["status"] != c.remain {
					t.Errorf("非法转换之后库里那一行变成 %v，必须停在 %s", row["status"], c.remain)
				}
			})
		}
	}
}

// ---------- #27 撤销：三个动作里最轻的一个 ----------

// TestM5CancelIsSideEffectFree 断言撤销的后后果是**零**：
// 不发通知、不动分数、不动帖子、不写流水，也不留下任何审核痕迹。
//
// 「零通知」这一条是 §3.7 那三条禁令之一：撤销是提交人自己的动作，
// 没有任何别人需要被告知。发帖人本来就没答应过什么，所以他不受影响。
// 这条断言必须用「和之前比」而不是「表里是空的」—— 夹具里本来就有
// 一条 new_match 和一条 return_submitted，写死 0 的话实现必须整个不发通知才绿，
// 而那和「发给了错误的人」是两种不同的错误。
func TestM5CancelIsSideEffectFree(t *testing.T) {
	st := setupM5(t)
	submitted := requireSubmitted(t, st.found.ID, st.li)

	totalBefore := harness.Count(t, `SELECT count(*) FROM notifications`)
	finderBefore := notificationsFor(t, st.finder.UserID)
	liBefore := notificationsFor(t, st.li.UserID)
	strangerBefore := notificationsFor(t, st.stranger.UserID)
	finderCredit := creditOf(t, st.finder.UserID)
	liCredit := creditOf(t, st.li.UserID)

	r := cancelReturn(t, submitted.ID, st.li.Token)
	RequireOK(t, r, "提交人撤销自己的归还确认")

	// §4 第 27 行只有两个键。刻意不含 reviewed_at：那个词承诺「有人审过了」，而没有人审过。
	got := dataKeys(t, r)
	assertSameSet(t, "#27 data", []string{"id", "status"}, got)
	var v cancelReturnView
	r.DataInto(t, &v)
	if v.ID != submitted.ID || v.Status != model.ReturnStatusCancelled {
		t.Errorf("#27 返回了 %+v", v)
	}

	if n := harness.Count(t, `SELECT count(*) FROM notifications`); n != totalBefore {
		t.Errorf("撤销多发 %d 条通知（撤销是提交人自己的动作，没有任何人需要被告知）", n-totalBefore)
	}
	for who, pair := range map[string][2]int{
		"提交人": {liBefore, notificationsFor(t, st.li.UserID)},
		"发帖人": {finderBefore, notificationsFor(t, st.finder.UserID)},
		"路人":  {strangerBefore, notificationsFor(t, st.stranger.UserID)},
	} {
		if pair[0] != pair[1] {
			t.Errorf("撤销让%s的通知从 %d 变成 %d", who, pair[0], pair[1])
		}
	}
	if got := creditOf(t, st.finder.UserID); got != finderCredit {
		t.Errorf("撤销之后发帖人分数 %d → %d", finderCredit, got)
	}
	if got := creditOf(t, st.li.UserID); got != liCredit {
		t.Errorf("撤销之后提交人分数 %d → %d", liCredit, got)
	}
	if got := harness.Count(t, `SELECT count(*) FROM credit_logs`); got != 0 {
		t.Errorf("撤销写了 %d 行流水", got)
	}
	if got := itemStatusOf(t, st.found.ID); got != model.ItemStatusOpen {
		t.Errorf("撤销之后 items.status=%q", got)
	}

	// 库里那一行：状态改了，但三个「谁审的」字段必须全是 NULL。
	// reviewer_id 有值而 review_kind 为空 = admin 直接改库；
	// 两者都有值 = 有人绕过了「只有发帖人能决定」这条线。
	row := returnRow(t, submitted.ID)
	if row["status"] != "cancelled" {
		t.Errorf("库里 status 期望 cancelled，实际 %v", row["status"])
	}
	if row["reviewer_id"] != nil || row["review_kind"] != nil {
		t.Errorf("撤销留下了审核痕迹：reviewer_id=%v review_kind=%v", row["reviewer_id"], row["review_kind"])
	}
	// reviewed_at 在库里**是**有值的（它的实际含义是「这条记录退出 pending 的时刻」），
	// 而 #27 的响应里没有它。这一对不对称是刻意的，两处都得钉住。
	if ts, ok := row["reviewed_at"].(string); !ok || len(ts) < 10 {
		t.Errorf("库里 reviewed_at=%#v，应该记录退出 pending 的时刻", row["reviewed_at"])
	}
	if _, present := got["reviewed_at"]; present {
		t.Errorf("#27 的响应里出现了 reviewed_at —— 那个词承诺「有人审过了」，而没有人审过")
	}
	if ids := unreviewedOutsideService(t); len(ids) != 0 {
		t.Errorf("cancelled 的记录不该出现在那条自检里，实际命中 %v", ids)
	}
}

// TestM5CancelPermission 覆盖「谁能撤销」这一件事。
//
// 重点是第二行：**发帖人不能替提交人撤销**。这条规则反直觉（帖子是他的），
// 所以单独写出来。理由：那条记录是提交人的陈述，撤不撤是他自己的决定 ——
// 发帖人想表达不同意有他自己的按钮（reject），而那还会给提交人发一条带理由的通知。
func TestM5CancelPermission(t *testing.T) {
	st := setupM5(t)
	submitted := requireSubmitted(t, st.found.ID, st.li)

	// 帖主不能替提交人撤
	r := cancelReturn(t, submitted.ID, st.finder.Token)
	RequireCode(t, r, apperr.CodeForbidden)
	// 路人不能
	r = cancelReturn(t, submitted.ID, st.stranger.Token)
	RequireCode(t, r, apperr.CodeForbidden)
	// 提交人自己可以
	RequireOK(t, cancelReturn(t, submitted.ID, st.li.Token), "提交人撤销")

	// 撤销之后重新提交，仍然只有新的那条提交人能撤
	retry := requireSubmitted(t, st.found.ID, st.stranger)
	r = cancelReturn(t, retry.ID, st.li.Token)
	RequireCode(t, r, apperr.CodeForbidden)
	RequireOK(t, cancelReturn(t, retry.ID, st.stranger.Token), "新提交人撤销自己那条")
}

// TestM5ResubmitAfterEachTerminalState 把「终态之后还能不能再提一次」按三种情况分开钉。
//
// §13 第 6 步最后一个核对项只说了 rejected 那条，但三种终态的行为其实各不相同，
// 而且差别不是来自 item_returns 而是来自 items.status：
//   - rejected / cancelled 都不关帖 → 帖子还 open → 谁都能再提一条
//   - confirmed 会关帖 → 再提撞的是 ITEM_CLOSED（不是 RETURN_DUPLICATE）
//
// 这一条区分开的价值在于：如果有人把「重提」实现成「把那条终态记录改回 pending」，
// 三种情况里至少有一种会被发现 —— 因为 id 会变或者不会变，而这里都断言了。
func TestM5ResubmitAfterEachTerminalState(t *testing.T) {
	t.Run("rejected 之后同一个人重提 → 成功且是新的一行", func(t *testing.T) {
		st := setupM5(t)
		first := requireSubmitted(t, st.found.ID, st.li)
		RequireOK(t, rejectReturn(t, first.ID, "东西不在我这儿", st.finder.Token), "拒绝")

		retry := submitReturn(t, st.found.ID, m5Body(t, st.li, m5Message), st.li.Token)
		RequireOK(t, retry, "被拒之后重提")
		var v submitReturnView
		retry.DataInto(t, &v)
		if v.ID == first.ID {
			t.Errorf("重提复用了被拒的那条记录（id=%d）—— 那条拒绝就消失了", v.ID)
		}
		// 被拒的那条必须仍然完整存在：它是「谁在什么时候声称过什么」的审计记录。
		old := returnRow(t, first.ID)
		if old["status"] != "rejected" || old["reviewer_id"] == nil {
			t.Errorf("重提把原来那条 rejected 改掉了：%v / %v", old["status"], old["reviewer_id"])
		}
	})

	t.Run("cancelled 之后同一个人重提 → 成功", func(t *testing.T) {
		st := setupM5(t)
		first := requireSubmitted(t, st.found.ID, st.li)
		RequireOK(t, cancelReturn(t, first.ID, st.li.Token), "撤销")

		retry := submitReturn(t, st.found.ID, m5Body(t, st.li, m5Message), st.li.Token)
		RequireOK(t, retry, "撤销之后重提")
		if got := harness.Count(t,
			`SELECT count(*) FROM item_returns WHERE item_id = $1 AND submitter_id = $2`,
			st.found.ID, st.li.UserID); got != 2 {
			t.Errorf("撤销 + 重提期望 2 行，实际 %d", got)
		}
	})

	t.Run("confirmed 之后重提 → ITEM_CLOSED（因为帖子已经关了）", func(t *testing.T) {
		st := setupM5(t)
		first := requireSubmitted(t, st.found.ID, st.li)
		RequireOK(t, confirmReturn(t, first.ID, "", st.finder.Token), "确认")

		r := submitReturn(t, st.found.ID, m5Body(t, st.stranger, m5Message), st.stranger.Token)
		RequireCode(t, r, apperr.CodeItemClosed)
		if got := harness.Count(t, `SELECT count(*) FROM item_returns`); got != 1 {
			t.Errorf("关帖之后仍然收进了新记录，期望 1 行实际 %d", got)
		}
	})
}

// ---------- #24 详情 ----------

// TestM5DetailWhoMaySee 钉住 §4 第 24 行那一列鉴权：提交人 / 发帖人 / Admin 三方，
// 其他人 FORBIDDEN。
//
// 为什么给 403 而不是 404：这条记录的存在对路人不是秘密（他能看见那条帖子），
// 而藏起来（404）的代价是提交人拼错一个 id 会去翻「我到底提没提过」，
// 一眼能说明白的原因却被藏掉了。这个取舍 M4 在 #22 已经定过一次。
func TestM5DetailWhoMaySee(t *testing.T) {
	st := setupM5(t)
	submitted := requireSubmitted(t, st.found.ID, st.li)

	// 三方各自能看见，且看到的是同一条记录
	for name, tok := range map[string]string{
		"提交人": st.li.Token,
		"发帖人": st.finder.Token,
	} {
		t.Run(name+"能看", func(t *testing.T) {
			v := requireDetail(t, submitted.ID, tok)
			if v.ID != submitted.ID {
				t.Errorf("看到了 id=%d 的记录", v.ID)
			}
		})
	}
	admin := m5Admin(t)
	if v := requireDetail(t, submitted.ID, admin.Token); v.ID != submitted.ID {
		t.Errorf("admin 看到了 id=%d 而不是 %d", v.ID, submitted.ID)
	}

	// 第四个人：连那条帖子的作者都不是
	t.Run("路人 FORBIDDEN", func(t *testing.T) {
		r := detailReturn(t, submitted.ID, st.stranger.Token)
		RequireCode(t, r, apperr.CodeForbidden)
	})
	t.Run("未登录 UNAUTHORIZED", func(t *testing.T) {
		r := detailReturn(t, submitted.ID, "")
		RequireCode(t, r, apperr.CodeUnauthorized)
	})
	t.Run("不存在的记录 NOT_FOUND", func(t *testing.T) {
		r := detailReturn(t, 987_654, st.finder.Token)
		RequireCode(t, r, apperr.CodeNotFound)
	})
	t.Run("路径里的 id 不是数字 → NOT_FOUND", func(t *testing.T) {
		// 不是 VALIDATION：这条约定在 handler/params.go 里写着，全项目所有
		// 以 :id 为路径的端点共用 —— 「/api/returns/abc」指的是一条不存在的记录，
		// 而 VALIDATION 暗示「换个格式就能查到」，可没有任何格式能让 abc 变成一条归还确认。
		r := harness.Get(t, "/api/returns/abc", st.finder.Token)
		RequireCode(t, r, apperr.CodeNotFound)
	})
}

// TestM5DetailViewIsWhatTheOwnerNeeds 检查 #24 的形状：
// 它是发帖人判断真伪的**唯一现场**，所以每一格都得在那里。
//
// 这条测试的价值在断言「键恰好是那十一个」而不是「有那些键」：
// 少一个（比如 submitter.credit_score）#24 就没法独立完成判断，
// 多一个（比如 message 之外再塞一个 real_name）就是另一种泄漏。
func TestM5DetailViewIsWhatTheOwnerNeeds(t *testing.T) {
	st := setupM5(t)
	proof := m5Proof(t, st.li)
	r := submitReturn(t, st.found.ID,
		map[string]any{"message": "周三傍晚在图书馆三楼前台交给您的，这是当时的收据照片", "proof_image_path": proof},
		st.li.Token)
	RequireOK(t, r, "提交一条带指定说明和凭证的归还确认")
	var submitted submitReturnView
	r.DataInto(t, &submitted)

	view := requireDetail(t, submitted.ID, st.finder.Token)

	// 键集合直接从响应读，不用 struct 反推：struct 少一个字段是编译期看不出来的
	// （它就是那一份形状定义，而 #24 契约是键的集合）。
	keys := dataKeys(t, detailReturn(t, submitted.ID, st.finder.Token))
	assertSameSet(t, "#24 data", []string{
		"id", "item", "submitter", "message", "proof_image_url", "status",
		"owner_note", "reviewer_id", "review_kind", "submitted_at", "reviewed_at",
	}, keys)

	// 判断原料：文字原样、凭证图可打开、提交人昵称 + 信用分
	if view.Message != "周三傍晚在图书馆三楼前台交给您的，这是当时的收据照片" {
		t.Errorf("message 被改动过：%q", view.Message)
	}
	if view.ProofImageURL != "/uploads/"+proof {
		t.Errorf("proof_image_url 期望 %q，实际 %q", "/uploads/"+proof, view.ProofImageURL)
	}
	// 凭证图必须真的能通过 #40 取回来：上传→提交→审核是一条链，
	// 中间任何一环把路径写错（比如存了绝对 URL 或丢了扩展名）都会在这里现形。
	status, body, _ := harness.GetRaw(t, view.ProofImageURL, st.finder.Token)
	if status != http.StatusOK {
		t.Fatalf("凭证图取不回来：GET %s → %d", view.ProofImageURL, status)
	}
	if len(body) == 0 {
		t.Errorf("凭证图返回了 200 但是空文件")
	}
	if view.Submitter.ID != st.li.UserID {
		t.Errorf("submitter.id=%d，期望 %d", view.Submitter.ID, st.li.UserID)
	}
	if view.Submitter.Nickname != st.li.Username {
		t.Errorf("submitter.nickname=%q（注册时昵称就是用户名）", view.Submitter.Nickname)
	}
	if view.Submitter.CreditScore != 100 {
		t.Errorf("submitter.credit_score=%d，注册默认应该是 100 —— 少了这一格发帖人就没法判断",
			view.Submitter.CreditScore)
	}
	if view.Status != model.ReturnStatusPending {
		t.Errorf("status=%q", view.Status)
	}
	// 还没人审：三个字段必须是 null / 空串，而不是 0 或某个假时间
	if view.ReviewerID != nil {
		t.Errorf("pending 的记录 reviewer_id=%v", *view.ReviewerID)
	}
	if view.ReviewKind != nil {
		t.Errorf("pending 的记录 review_kind=%v", *view.ReviewKind)
	}
	if view.ReviewedAt != "" {
		t.Errorf("pending 的记录 reviewed_at=%q，应该是空串", view.ReviewedAt)
	}
	if view.OwnerNote != "" {
		t.Errorf("pending 的 owner_note=%q，提交人不该能写这一列", view.OwnerNote)
	}

	// 嵌套摘要不带联系方式（found 帖一律锁，见 service 里 summaryOfFound 那段）。
	if view.Item.Contact != nil {
		t.Errorf("item.contact=%q —— 帖子已经关了也不该在这里给出联系方式", *view.Item.Contact)
	}
	if view.Item.ID != st.found.ID || view.Item.Title != walletTitleFound {
		t.Errorf("item 摘要不对：%+v", view.Item)
	}
	if view.Item.Status != model.ItemStatusOpen {
		t.Errorf("item.status=%q", view.Item.Status)
	}
}

// TestM5DetailSurvivesItemDeletion 钉住 service.Detail 那个「软删的帖子照样返回」的决定。
//
// 场景是真实的治理情况：admin 下架了那条拾物帖，而提交人之前提过一条归还确认。
// 如果这里返回 404，提交人会看到自己的记录凭空蒸发 ——
// 那恰好是治理场景里最难解释的一种「什么都没发生」（定位原则 5 要求可追溯）。
func TestM5DetailSurvivesItemDeletion(t *testing.T) {
	st := setupM5(t)
	submitted := requireSubmitted(t, st.found.ID, st.li)

	// admin 下架那条帖子
	harness.SetItemStatus(t, st.found.ID, model.ItemStatusDeleted)

	// 广场上确实看不见它（先确认前提，否则下面那条断言是在测一个不存在的场景）
	if ids := squareIDs(t); containsID(ids, st.found.ID) {
		t.Fatalf("deleted 的帖子还在广场上，后面的断言没有意义")
	}

	view := requireDetail(t, submitted.ID, st.li.Token)
	if view.Item.Status != model.ItemStatusDeleted {
		t.Errorf("被下架的帖子在 #24 里应当诚实显示 deleted，实际 %q", view.Item.Status)
	}
	if view.ID != submitted.ID || view.Status != model.ReturnStatusPending {
		t.Errorf("记录本身被连带影响了：%+v", view)
	}

	// 但下架不等于关帖放行：deleted 的帖子仍然不收新的归还确认（#23 撞 NOT_FOUND）
	r := submitReturn(t, st.found.ID, m5Body(t, st.stranger, m5Message), st.stranger.Token)
	RequireCode(t, r, apperr.CodeNotFound)
}

func containsID(ids []int64, want int64) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestM5DecidedRecordKeepsItsTrail 走完 confirm 之后再看 #24：
// 那三个「谁审的」字段必须从 null 变成有值，而 owner_note 是发帖人写的那句。
//
// 这一条和 TestM5DetailViewIsWhatTheOwnerNeeds 是同一个形状的两个时刻（审前 / 审后）。
// 分成两条而不是在一条里连着看，是因为 review 前后的 null 形状是两条独立规则：
// 前者防「提交人能预先写好 owner_note」，后者防「审核痕迹不落库」。
func TestM5DecidedRecordKeepsItsTrail(t *testing.T) {
	st := setupM5(t)
	submitted := requireSubmitted(t, st.found.ID, st.li)

	const note = "确实是我的钱包，谢谢"
	RequireOK(t, confirmReturn(t, submitted.ID, note, st.finder.Token), "确认")

	view := requireDetail(t, submitted.ID, st.li.Token)
	if view.Status != model.ReturnStatusConfirmed {
		t.Errorf("status=%q", view.Status)
	}
	if view.ReviewerID == nil || *view.ReviewerID != st.finder.UserID {
		t.Errorf("reviewer_id=%v，应该是发帖人 %d", view.ReviewerID, st.finder.UserID)
	}
	if view.ReviewKind == nil || *view.ReviewKind != model.ReviewKindOwner {
		t.Errorf("review_kind=%v", view.ReviewKind)
	}
	if view.OwnerNote != note {
		t.Errorf("owner_note=%q", view.OwnerNote)
	}
	if view.ReviewedAt == "" {
		t.Errorf("已处理的记录必须有 reviewed_at")
	}
	if view.Item.Status != model.ItemStatusClosed {
		t.Errorf("confirm 之后 #24 里的帖子摘要应当显示 closed，实际 %q", view.Item.Status)
	}

	// 提交人在 #24 里同样能看到发帖人的那句留言（他要知道为什么被确认或被拒绝）
	asOwner := requireDetail(t, submitted.ID, st.finder.Token)
	if asOwner.OwnerNote != note {
		t.Errorf("发帖人自己看不到 owner_note")
	}
}

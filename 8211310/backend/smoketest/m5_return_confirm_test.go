package smoketest

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
)

// 本文件是 M5 第②层的第三段：#25 confirm 那一次连带写下去的六件事。
//
// confirm 是全项目副作用最大的一个用户动作，而它的危险不在「做不做得到」，
// 在「会不会只做一半」：
//
//	只写了 item_returns 没关帖   → 东西已经还了，帖子却继续在广场上等人来捡
//	只关了帖没加分              → 拾主做了好事却查无此事，§3.6 那条「分数可解释」失效
//	加了分没发通知              → 提交人永远不知道自己被确认了
//	关了帖没给 lost 作者收尾提示  → §14-4 那条「污染候选池」的补救手段之一消失
//
// 所以这个文件的每一条测试都在读**库**而不是只读响应：
// 响应是 service 拼出来的一个视图，视图里没有的那些表才是事实。
//
// 另外三条边界（都在 repo.Confirm 的那段注释里写着理由）：
//   - 关帖那句 WHERE 带 `AND status='open'`：这是**不复活**，不是幂等优化
//   - 加分记的是**实际生效**的增量：夹到 200 顶时流水记 0，否则对不上账
//   - hints 由 service 从台账反查再传进来：repo 不去猜谁配过对

// ---------- 判据链的主体 ----------

// TestM5ConfirmWritesTheWholeChain 把 §12 那串「confirm 之后应当核对的六件事」
// 一次走完成，每一件都从库里读回来。
//
// 这条测试的形状是「一次动作 + 六段断言」而不是六个小测试，是刻意的：
// 拆成六个的话，每个都得自己建一遍 pending，六个测试各测一份，
// 而真正要防的失败是「同一份数据上只发生了一半」。
func TestM5ConfirmWritesTheWholeChain(t *testing.T) {
	st := setupM5(t)
	submitted := requireSubmitted(t, st.found.ID, st.li)

	// 每一步之前的基线都要记下来：所有断言都是「相对变化」而不是绝对数字，
	// 夹具以后多加一条通知也不会让这里莫名其妙地红。
	finderNotifsBefore := notificationsFor(t, st.finder.UserID)
	liNotifsBefore := notificationsFor(t, st.li.UserID)
	strangerNotifsBefore := notificationsFor(t, st.stranger.UserID)

	const note = "确实是我的钱包，里面的校园卡也还在"
	r := confirmReturn(t, submitted.ID, note, st.finder.Token)
	RequireOK(t, r, "发帖人确认归还")
	var confirmed confirmReturnView
	r.DataInto(t, &confirmed)

	// ① 那条记录：状态 + 归属两列 + 发帖人的留言
	row := returnRow(t, submitted.ID)
	if row["status"] != "confirmed" {
		t.Errorf("① item_returns.status=%v", row["status"])
	}
	if row["review_kind"] != model.ReviewKindOwner {
		t.Errorf("① review_kind=%#v，只有发帖人本人过得了的那条路径才会写 'owner'", row["review_kind"])
	}
	if got := asInt64(t, row["reviewer_id"]); got != st.finder.UserID {
		t.Errorf("① reviewer_id=%d，期望发帖人 %d", got, st.finder.UserID)
	}
	if row["owner_note"] != note {
		t.Errorf("① owner_note=%#v", row["owner_note"])
	}

	// ② 帖子 closed
	if got := itemStatusOf(t, st.found.ID); got != model.ItemStatusClosed {
		t.Errorf("② items.status=%q，期望 closed", got)
	}

	// ③④ 两个人各自的分数
	if got := creditOf(t, st.finder.UserID); got != 100+model.CreditDeltaReturnOwner {
		t.Errorf("③ 拾主 credit_score=%d，期望 %d", got, 100+model.CreditDeltaReturnOwner)
	}
	if got := creditOf(t, st.li.UserID); got != 100+model.CreditDeltaReturnSubmitter {
		t.Errorf("④ 提交人 credit_score=%d，期望 %d", got, 100+model.CreditDeltaReturnSubmitter)
	}

	// ⑤ 两条流水：谁、多少、为什么、指向哪条记录
	logs := creditLogsInDB(t)
	if len(logs) != 2 {
		t.Fatalf("⑤ credit_logs 期望 2 行，实际 %d", len(logs))
	}
	ownerLog := pickReturnLog(t, logs, st.finder.UserID)
	if ownerLog.delta != model.CreditDeltaReturnOwner {
		t.Errorf("⑤ 拾主那条 delta=%d，期望 %d", ownerLog.delta, model.CreditDeltaReturnOwner)
	}
	if ownerLog.reason != model.CreditReasonReturnOwner {
		t.Errorf("⑤ 拾主那条 reason=%q，期望 %q", ownerLog.reason, model.CreditReasonReturnOwner)
	}
	submitterLog := pickReturnLog(t, logs, st.li.UserID)
	if submitterLog.delta != model.CreditDeltaReturnSubmitter {
		t.Errorf("⑤ 提交人那条 delta=%d，期望 %d", submitterLog.delta, model.CreditDeltaReturnSubmitter)
	}
	if submitterLog.reason != model.CreditReasonReturnSubmitter {
		t.Errorf("⑤ 提交人那条 reason=%q，期望 %q", submitterLog.reason, model.CreditReasonReturnSubmitter)
	}
	for _, l := range logs {
		if l.refType != model.CreditRefTypeReturn {
			t.Errorf("⑤ ref_type=%q，期望 %q", l.refType, model.CreditRefTypeReturn)
		}
		if l.refID != submitted.ID {
			t.Errorf("⑤ 有一条流水的 ref_id=%d，没指向那条归还确认 %d", l.refID, submitted.ID)
		}
	}

	// ⑥ 通知：提交人两条（return_confirmed + item_returned_hint），其他人零条
	liNotifs := notificationsOf(t, st.li.UserID)
	if len(liNotifs) != liNotifsBefore+2 {
		t.Fatalf("⑥ 小李期望 +%d 条通知，实际 +%d（%v → %v）",
			2, len(liNotifs)-liNotifsBefore, typesOf(liNotifs[:liNotifsBefore]), typesOf(liNotifs))
	}
	done := findByType(t, liNotifs[liNotifsBefore:], model.NotificationReturnConfirmed)
	if done.ReturnID == nil || *done.ReturnID != submitted.ID {
		t.Errorf("⑥ return_confirmed 的 return_id=%v", done.ReturnID)
	}
	if done.ItemID == nil || *done.ItemID != st.found.ID {
		t.Errorf("⑥ return_confirmed 的 item_id=%v", done.ItemID)
	}
	hint := findByType(t, liNotifs[liNotifsBefore:], model.NotificationItemReturnedHint)
	if hint.ItemID == nil || *hint.ItemID != st.found.ID {
		t.Errorf("⑥ item_returned_hint 的 item_id=%v，它要指向那条拾物帖", hint.ItemID)
	}
	// 提示是「请去把你的失物帖标成已找到」，所以它必须真的这么写：
	// 这句话是 §14-4 那个补救手段的全部作用，写成别的漂亮话就没有效果。
	if !strings.Contains(hint.Content, "已找到") {
		t.Errorf("item_returned_hint 没有告诉收件人该做什么：%q", hint.Content)
	}

	// 发帖人不该收到自己那次确认的通知，路人和之前那次提交之外的人更不该被牵连。
	if got := notificationsFor(t, st.finder.UserID); got != finderNotifsBefore {
		t.Errorf("⑥ 发帖人收到了 %d 条新通知（他刚自己点的确认）", got-finderNotifsBefore)
	}
	if got := notificationsFor(t, st.stranger.UserID); got != strangerNotifsBefore {
		t.Errorf("⑥ 路人收到了 %d 条通知", got-strangerNotifsBefore)
	}

	// 响应里的 credit_delta 和库里的流水必须一致（这是拾主唯一的回执）
	if confirmed.CreditDelta != ownerLog.delta {
		t.Errorf("响应 credit_delta=%d 而流水记的是 %d，两边对不上账", confirmed.CreditDelta, ownerLog.delta)
	}
	if confirmed.Status != model.ReturnStatusConfirmed {
		t.Errorf("响应 status=%q", confirmed.Status)
	}

	if ids := unreviewedOutsideService(t); len(ids) != 0 {
		t.Errorf("出现了 service 之外推进的归还确认：%v", ids)
	}
}

// ---------- 台账反查：item_returned_hint 发给谁 ----------

// TestM5ConfirmHintsEveryLedgerAuthor 证明那条反查不是「写死给提交人」的伪实现。
//
// 场景是三个失主：小李和小王的失物帖和这条拾物帖配过对（小王有两条），
// 赵什么的都没配上。confirm 之后：
//   - 提交人小李拿到 return_confirmed + item_returned_hint 两条
//   - 小王只拿到 **一条** item_returned_hint（SELECT DISTINCT 的是作者，不是配对行）
//   - 赵一条都没有 —— 他从没被 new_match 叫醒过，不该突然收到收尾
//
// 这一条同时是 §3.7「通知不对称」在 M5 的收尾：
// found 帖创建时只通知 lost 方，confirm 时也只通知 lost 方 —— 两头都不通知 found 作者。
func TestM5ConfirmHintsEveryLedgerAuthor(t *testing.T) {
	st, wang, zhao := setupM5MultiAuthor(t)

	submitted := requireSubmitted(t, st.found.ID, st.li)
	RequireOK(t, confirmReturn(t, submitted.ID, "", st.finder.Token), "确认归还")

	// 提交人：两条都有，且都指向这条归还确认
	liAll := notificationsOf(t, st.li.UserID)
	if got := findByTypeCount(t, liAll, model.NotificationReturnConfirmed); got != 1 {
		t.Errorf("提交人的 return_confirmed 期望 1 条，实际 %d", got)
	}
	if got := findByTypeCount(t, liAll, model.NotificationItemReturnedHint); got != 1 {
		t.Errorf("提交人的 item_returned_hint 期望 1 条，实际 %d", got)
	}
	if got := findByTypeCount(t, liAll, model.NotificationNewMatch); got != 1 {
		t.Errorf("提交人的 new_match 期望还是那 1 条（confirm 不该动它），实际 %d", got)
	}

	// 小王：配过对但**不是**提交人，所以只该有那条收尾提示，而且只有一条
	if got := findByTypeCount(t, notificationsOf(t, wang.UserID), model.NotificationItemReturnedHint); got != 1 {
		t.Errorf("小王的两条失物帖都配过对，提示期望按**作者**去重成 1 条，实际 %d", got)
	}
	if got := findByTypeCount(t, notificationsOf(t, wang.UserID), model.NotificationReturnConfirmed); got != 0 {
		t.Errorf("小王不是提交人，却收到了 %d 条 return_confirmed", got)
	}
	// 提示的 return_id 必须指向真实那条：收件人要点进去看的
	hint := findByType(t, notificationsOf(t, wang.UserID), model.NotificationItemReturnedHint)
	if hint.ReturnID == nil || *hint.ReturnID != submitted.ID {
		t.Errorf("小王那条提示的 return_id=%v，期望 %d", hint.ReturnID, submitted.ID)
	}

	// 反例：没配过对的人一条都没有
	if got := notificationsFor(t, zhao.UserID); got != 0 {
		t.Errorf("从没配过对的用户有 %d 条通知（基线就该是 0）", got)
	}
	// found 帖的作者永远不因为这件事被通知
	if got := notificationsFor(t, st.finder.UserID); got != 1 {
		t.Errorf("拾主应该有且仅有那一条 return_submitted，实际 %d 条", got)
	}
}

// setupM5MultiAuthor 是「一条拾物帖和两个不同作者的失物帖配过对」的舞台。
//
// 它不能复用 setupM5 再补发失物帖：匹配只在 found 帖创建那一刻跑（§3.7 那条不对称），
// 后建的 lost 帖既不进台账也不发通知 —— 反查就永远查不到那个后建的人，
// 而这个测试要的正是「台账里有两个人」。所以顺序必须是 lost 全在前、found 在最后。
//
// 小王的第二条失物帖和小李的那条**内容完全相同**，这是刻意的：
// 重复发帖在本系统里是合法的（§1 判据：记成两条），而去重要靠的就是
// 「DISTINCT 的是作者」这一句 —— 换一条描述不同的帖子，分数可能掉到通知线以下，
// 那时台账只剩 2 行，测试就悄悄不再测它想测的东西了。
func setupM5MultiAuthor(t *testing.T) (m5Stage, Session, Session) {
	t.Helper()
	harness.TruncateAll(t)

	st := m5Stage{
		finder:   harness.RegisterAndLogin(t, "m5finder", m5Password),
		li:       harness.RegisterAndLogin(t, "m5li", m5Password),
		stranger: harness.RegisterAndLogin(t, "m5stranger", m5Password),
	}
	wang := harness.RegisterAndLogin(t, "m5wang", m5Password)
	zhao := harness.RegisterAndLogin(t, "m5zhao", m5Password)

	st.lost = createItem(t, st.li,
		lostWalletBody(walletTitleLost, walletDesc, locLibrary, detailLibrary))
	createItem(t, wang, lostWalletBody(walletTitleLost, walletDesc, locLibrary, detailLibrary))
	createItem(t, wang, lostWalletBody(walletTitleLost, walletDesc, locLibrary, detailLibrary))
	// 赵：地点不同（Tier 1 的硬筛就是 location_id，他被筛掉，压根不进候选池）
	createItem(t, zhao, lostWalletBody("丢了一个蓝色卡包", "蓝色卡包，里面有一张公交卡", locOther, "二楼栏杆"))

	resp := harness.Post(t, "/api/items",
		foundWalletBody(walletTitleFound, walletDesc, locLibrary, detailLibrary), st.finder.Token)
	RequireOK(t, resp, "拾主发拾物帖")
	st.found = createResultOf(t, resp)

	// 前提核对：三行配对、两个不同的作者。少了任何一行，下面那些
	// 「1 条提示」就变成了在测「零条提示」，而这两种红看起来一模一样。
	if got := harness.Count(t,
		`SELECT count(*) FROM match_pairs WHERE found_item_id = $1`, st.found.ID); got != 3 {
		t.Fatalf("夹具期望台账里有 3 行配对，实际 %d —— 去重那一条就没得测了", got)
	}
	if got := harness.Count(t,
		`SELECT count(DISTINCT i.user_id) FROM match_pairs mp
		   JOIN items i ON i.id = mp.lost_item_id WHERE mp.found_item_id = $1`,
		st.found.ID); got != 2 {
		t.Fatalf("夹具期望台账里有 2 个不同的 lost 作者，实际 %d —— 反查测试的前提没了", got)
	}
	if got := notificationsFor(t, zhao.UserID); got != 0 {
		t.Fatalf("夹具期望赵没有任何 new_match，实际 %d 条 —— 那条失物帖其实配上对了", got)
	}
	return st, wang, zhao
}

// TestM5ConfirmWithoutLedgerStillSucceeds 是上一条的反面：
// 一条从没和任何失物帖配过对的拾物帖，confirm 照样成功，只是少一条提示。
//
// 「hints 为空」不能只是「循环恰好零次」：它必须证明这件事不会让整条链失败，
// 也不会让分数或流水缺一半。现实中大多数拾物帖根本没人配过对。
func TestM5ConfirmWithoutLedgerStillSucceeds(t *testing.T) {
	st := setupM5(t)

	// 另开一条**配不上对**的拾物帖：地点不同（Tier 1 的硬筛把它筛掉，
	// 于是这条帖子在台账里一行都没有 —— 见 setupM5MultiAuthor 里同一句理由）
	solo := createItem(t, st.finder,
		foundWalletBody("在体育馆捡到一串钥匙", "体育馆二楼栏杆上捡到一串钥匙，挂在失物处",
			locOther, "二楼栏杆"))
	if got := harness.Count(t,
		`SELECT count(*) FROM match_pairs WHERE found_item_id = $1`, solo.ID); got != 0 {
		t.Fatalf("夹具期望这条拾物帖没有台账，实际 %d 行 —— 它就不再是「零个收件人」那个场景了", got)
	}

	submitted := requireSubmitted(t, solo.ID, st.li)
	r := confirmReturn(t, submitted.ID, "是我的笔", st.finder.Token)
	RequireOK(t, r, "确认一条没有配对记录的归还")

	// 四件事照做，只有提示没有
	if got := itemStatusOf(t, solo.ID); got != model.ItemStatusClosed {
		t.Errorf("items.status=%q，仍然应当 closed", got)
	}
	if got := creditOf(t, st.finder.UserID); got != 110 {
		t.Errorf("拾主分数=%d，应当照样 +10", got)
	}
	if got := harness.Count(t, `SELECT count(*) FROM credit_logs`); got != 2 {
		t.Errorf("流水期望 2 行，实际 %d", got)
	}
	li := notificationsOf(t, st.li.UserID)
	if n := findByTypeCount(t, li, model.NotificationItemReturnedHint); n != 0 {
		t.Errorf("台账是空的，却发出了 %d 条 item_returned_hint", n)
	}
	if n := findByTypeCount(t, li, model.NotificationReturnConfirmed); n != 1 {
		t.Errorf("return_confirmed 期望恰好 1 条，实际 %d", n)
	}
}

// ---------- 关帖那一句 WHERE：不复活 ----------

// TestM5ConfirmDoesNotResurrectADeletedItem 是定位原则 5 的直接检验。
//
// 场景：admin 先下架了那条拾物帖，发帖人后来才点确认。
// 关帖那句 UPDATE 带 `AND status='open'`，所以那条帖子必须**仍是 deleted**。
// 如果它回到 closed（进而出现在「我的发布」里）就说明「admin 能销毁内容」
// 这条边界被一个普通用户的正常操作撤销了 —— 那正是 §3.1 反复要避免的形状。
//
// 而其余四件事必须**照常发生**：发帖人确认了归还这件事是真的，
// 它不取决于那条帖子现在挂什么状态（repo 注释第 ② 点）。
func TestM5ConfirmDoesNotResurrectADeletedItem(t *testing.T) {
	st := setupM5(t)
	submitted := requireSubmitted(t, st.found.ID, st.li)

	// admin 下架（M6 才有 #44 端点，这里按 M1 那几条夹具的做法直接改库）
	harness.SetItemStatus(t, st.found.ID, model.ItemStatusDeleted)

	r := confirmReturn(t, submitted.ID, "", st.finder.Token)
	RequireOK(t, r, "对一条已被下架的帖子确认归还")

	if got := itemStatusOf(t, st.found.ID); got != model.ItemStatusDeleted {
		t.Errorf("confirm 把下架的帖子复活成了 %q —— 必须是 deleted", got)
	}
	row := returnRow(t, submitted.ID)
	if row["status"] != "confirmed" || row["review_kind"] != model.ReviewKindOwner {
		t.Errorf("记录本身没被推进：%v / %v", row["status"], row["review_kind"])
	}
	if got := creditOf(t, st.finder.UserID); got != 110 {
		t.Errorf("拾主分数=%d，加分不该因为帖子被下架就跳过", got)
	}
	if got := creditOf(t, st.li.UserID); got != 102 {
		t.Errorf("提交人分数=%d", got)
	}
	if got := harness.Count(t, `SELECT count(*) FROM credit_logs`); got != 2 {
		t.Errorf("流水期望 2 行，实际 %d", got)
	}
	if n := findByTypeCount(t, notificationsOf(t, st.li.UserID), model.NotificationReturnConfirmed); n != 1 {
		t.Errorf("return_confirmed 期望 1 条，实际 %d", n)
	}
	if n := findByTypeCount(t, notificationsOf(t, st.li.UserID), model.NotificationItemReturnedHint); n != 1 {
		t.Errorf("item_returned_hint 期望 1 条（他被 new_match 叫醒过，就该收到收尾），实际 %d", n)
	}

	// #24 仍然能看见这条记录，而它嵌套的帖子摘要诚实显示 deleted
	view := requireDetail(t, submitted.ID, st.li.Token)
	if view.Item.Status != model.ItemStatusDeleted {
		t.Errorf("#24 里的帖子摘要显示 %q，应当诚实是 deleted", view.Item.Status)
	}
}

// TestM5ConfirmOnAlreadyClosedItem 覆盖另一格：帖子已经是 closed（发帖人自己点的
// 「已找到」），那条更早的 pending 后来被确认。
//
// 关帖那一句这次确实一行都没改，而其余四件事照常发生。
// 这一格值得单独测是因为它是**最常见**的真实顺序：
// 失主找到了东西，先把帖子关掉，再顺手确认那条归还。
func TestM5ConfirmOnAlreadyClosedItem(t *testing.T) {
	st := setupM5(t)
	submitted := requireSubmitted(t, st.found.ID, st.li)

	// 发帖人用 #18 自己关帖
	closeRes := harness.Do(t, http.MethodPatch, "/api/items/"+itoa(st.found.ID)+"/status",
		map[string]any{"status": model.ItemStatusClosed}, st.finder.Token)
	RequireOK(t, closeRes, "发帖人自己把拾物帖标成已归还")

	// 已经 closed 的帖子不再收新的归还确认（这条边界是 #23 的⑤）
	r := submitReturn(t, st.found.ID, m5Body(t, st.stranger, m5Message), st.stranger.Token)
	RequireCode(t, r, apperr.CodeItemClosed)

	RequireOK(t, confirmReturn(t, submitted.ID, "对，已经还给我了", st.finder.Token), "确认那条 pending")

	if got := itemStatusOf(t, st.found.ID); got != model.ItemStatusClosed {
		t.Errorf("items.status=%q", got)
	}
	if got := creditOf(t, st.finder.UserID); got != 110 {
		t.Errorf("拾主分数=%d，期望 110（帖子状态不影响加分）", got)
	}
	if got := harness.Count(t, `SELECT count(*) FROM credit_logs`); got != 2 {
		t.Errorf("流水期望 2 行，实际 %d", got)
	}
}

// ---------- 分数上限：夹住的是实际增量 ----------

// TestM5ConfirmClampsCreditAtTheCeiling 把「流水记实际生效值而不是名义值」钉住。
//
// §3.6 那条设计里 credit_logs 唯一可 debug 的性质是「Σ流水 + 100 == credit_score」。
// 分数已经 200 的人再确认一次归还，如果流水记了名义上的 +10，
// 那条对账就永久失效 —— 而这条对账是用户问「我为什么是 200」时唯一的依据。
// 所以：夹到顶时流水必须记 0，而**那一行照插**。
func TestM5ConfirmClampsCreditAtTheCeiling(t *testing.T) {
	st := setupM5(t)
	submitted := requireSubmitted(t, st.found.ID, st.li)

	_, ceil := model.CreditScoreBounds()
	topUpTo(t, st.finder.UserID, ceil)

	r := confirmReturn(t, submitted.ID, "", st.finder.Token)
	RequireOK(t, r, "拾主已在上限时确认")
	var confirmed confirmReturnView
	r.DataInto(t, &confirmed)

	if confirmed.CreditDelta != 0 {
		t.Errorf("响应 credit_delta=%d，已经是上限时应当返回**实际生效**的 0", confirmed.CreditDelta)
	}
	if got := creditOf(t, st.finder.UserID); got != ceil {
		t.Errorf("分数越过上限：%d", got)
	}

	// 那一行流水照插，delta 是 0
	logs := creditLogsInDB(t)
	owner := pickReturnLog(t, logs, st.finder.UserID)
	if owner.delta != 0 {
		t.Errorf("流水记的是 %d，必须记实际生效的 0（否则对账失效）", owner.delta)
	}
	// 提交人那一条不受影响（他不在上限）
	sub := pickReturnLog(t, logs, st.li.UserID)
	if sub.delta != model.CreditDeltaReturnSubmitter {
		t.Errorf("提交人那条 delta=%d", sub.delta)
	}

	// 这就是那条对账本身：每个人「Σ流水 + 100」都等于他的 credit_score
	reconcileAllCredits(t)
}

// topUpTo 把一个人的分数直接推到某个值，并且**同时补一条能对上账的流水**。
//
// 为什么不能只 UPDATE users：这个文件最后要跑 Σ流水 + 100 == credit_score 那条对账，
// 而夹具自己改分数不记流水的话，对账就永远红在夹具上 ——
// 那时候测试测到的是「脚手架破了」而不是「夹住上限的实现破了」，
// 这两种红在错误信息里长得一模一样。
//
// reason 用 "smoke_topup"：它不是 model 里的任何一条业务原因，
// 所以一眼就能在流水里认出这是夹具而不是真实加分；ref_type 留 NULL，
// 「没有来源单据的流水」正是 #33 要能诚实渲染的那种行。
func topUpTo(t *testing.T, userID int64, want int) {
	t.Helper()
	ctx := context.Background()

	var current int
	if err := harness.Pool.QueryRow(ctx, `SELECT credit_score FROM users WHERE id = $1`, userID).Scan(&current); err != nil {
		t.Fatalf("topUpTo 读分数失败 (user=%d): %v", userID, err)
	}
	if current == want {
		return
	}

	tx, err := harness.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("topUpTo 开事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx,
		`UPDATE users SET credit_score = $2, updated_at = now() WHERE id = $1`, userID, want); err != nil {
		t.Fatalf("topUpTo 改分数失败 (user=%d): %v", userID, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO credit_logs (user_id, delta, reason, ref_type, ref_id)
		VALUES ($1, $2, 'smoke_topup', NULL, NULL)`, userID, want-current); err != nil {
		t.Fatalf("topUpTo 补流水失败 (user=%d): %v", userID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("topUpTo 提交失败: %v", err)
	}
}

// reconcileAllCredits 跑 §3.6 那条唯一的对账性质：
//
//	Σ(这个人所有 credit_logs.delta) + 100 == users.credit_score
//
// 它对每个用户都成立（包括没被加分过的那些 —— 求和为 0）。
// 这一条写成通用函数而不是一个用例，是因为它是**账本的一致性**，
// 每一次 confirm 之后都应当再跑一遍，而不是只在「夹到顶」那一格检查。
func reconcileAllCredits(t *testing.T) {
	t.Helper()
	rows, err := harness.Pool.Query(context.Background(), `
		SELECT u.id, u.credit_score, COALESCE(SUM(l.delta), 0)::int AS logged
		  FROM users u
		  LEFT JOIN credit_logs l ON l.user_id = u.id
		 GROUP BY u.id, u.credit_score
		 ORDER BY u.id`)
	if err != nil {
		t.Fatalf("对账查询失败: %v", err)
	}
	defer rows.Close()

	var problems []string
	for rows.Next() {
		var id int64
		var score, logged int
		if err := rows.Scan(&id, &score, &logged); err != nil {
			t.Fatalf("对账扫描失败: %v", err)
		}
		if score != 100+logged {
			problems = append(problems, fmt.Sprintf("user %d: credit_score=%d 而 100+Σ流水=%d",
				id, score, 100+logged))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("对账读取失败: %v", err)
	}
	if len(problems) > 0 {
		t.Errorf("流水和分数对不上账：\n  %s", strings.Join(problems, "\n  "))
	}
}

// ---------- #25 / #26 的响应形状 ----------

// TestM5DecideResponseKeys 钉住两个决定的回执差别：
// #25 有 credit_delta，#26 **没有**这个键。
//
// 「拒绝不产生分数变化」如果实现成返回 credit_delta: 0，读起来像
// 「本来要加分但没加」。所以这里断言的是键的缺失，而不是值。
// 这条差别只能用 HTTP 层测（第①层测的是响应体形状，那里同样是键集合），
// 而它是 §4 第 25/26 行两列的直接对照。
func TestM5DecideResponseKeys(t *testing.T) {
	st := setupM5(t)

	// ⚠ 顺序是「先拒后 confirm」而不是两条 pending 各走一个动作：
	// confirm 会把帖子关掉，而关掉之后那条帖子不再收新的归还确认（#23 的⑤），
	// 所以「同一条帖子上的第二条 pending」只可能出现在 reject 之后 ——
	// 拒绝不关帖（§13 第 6 步）在这里不只是一条业务规则，它还是这条测试的前提。
	rejected := rejectReturn(t, requireSubmitted(t, st.found.ID, st.li).ID,
		"东西不在你那儿", st.finder.Token)
	RequireOK(t, rejected, "拒绝一条")
	keys := dataKeys(t, rejected)
	assertSameSet(t, "#26 data", []string{"id", "status", "reviewed_at"}, keys)
	if _, present := keys["credit_delta"]; present {
		t.Errorf("#26 返回了 credit_delta —— 拒绝不产生分数变化，连一个 0 都不该返回")
	}

	confirmable := requireSubmitted(t, st.found.ID, st.stranger)
	r := confirmReturn(t, confirmable.ID, "", st.finder.Token)
	RequireOK(t, r, "确认另一条")
	assertSameSet(t, "#25 data", []string{"id", "status", "reviewed_at", "credit_delta"}, dataKeys(t, r))
}

// TestM5ConfirmRequiresAuthAndIsPostOnly 补齐 #25 的两条通用边界。
//
// 「GET 能打 confirm 吗」这个问题在这里值得单独测：状态推进是写操作，
// 而它一旦被浏览器预取或爬虫命中就会**真的关帖、真的加分**。
// 计划里这一组全部用 POST 而不是 PUT/PATCH 就是这个原因（M1 的 #21 同样）。
func TestM5ConfirmRequiresAuthAndIsPostOnly(t *testing.T) {
	st := setupM5(t)
	submitted := requireSubmitted(t, st.found.ID, st.li)

	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		r := harness.Do(t, m, decideURL("confirm", submitted.ID), nil, st.finder.Token)
		t.Run(m+" 不允许", func(t *testing.T) {
			RequireCode(t, r, apperr.CodeMethodNotAllowed)
			if row := returnRow(t, submitted.ID); row["status"] != "pending" {
				t.Errorf("%s 把记录推进成了 %v", m, row["status"])
			}
		})
	}

	// 而正确的 POST 仍然成功（防止「405 是通过把 POST 也挡掉实现的」）
	RequireOK(t, confirmReturn(t, submitted.ID, "", st.finder.Token), "POST 才是合法方法")
}

// ---------- 读库的小工具 ----------

type creditLogRow struct {
	userID  int64
	delta   int
	reason  string
	refType string
	refID   int64
}

// creditLogsInDB 读全部流水。同样刻意在 SQL 里 ::text，
// 避免 map[string]any 里的类型惊喜（见 onlyLedgerRow 那条理由）。
//
// ref_type 那一列是 **nullable**（没有来源单据的流水就是 NULL，
// 夹具的 smoke_topup 行正是这种），所以 COALESCE 成空串再扫进 string。
// 不这么写的话 pgx 会报「cannot scan NULL into *string」——
// 一个和归还毫无关系的读取失败，却足以让整条测试变成 build 前的红。
func creditLogsInDB(t *testing.T) []creditLogRow {
	t.Helper()
	rows, err := harness.Pool.Query(context.Background(), `
		SELECT user_id, delta, reason, COALESCE(ref_type::text, ''), COALESCE(ref_id, 0)
		  FROM credit_logs ORDER BY id`)
	if err != nil {
		t.Fatalf("读流水失败: %v", err)
	}
	defer rows.Close()

	var out []creditLogRow
	for rows.Next() {
		var l creditLogRow
		if err := rows.Scan(&l.userID, &l.delta, &l.reason, &l.refType, &l.refID); err != nil {
			t.Fatalf("扫描流水失败: %v", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读取流水失败: %v", err)
	}
	return out
}

// pickReturnLog 取出某个人「因归还确认而产生」的那一行流水。
//
// 按 ref_type 筛而不是「取这个人的最后一行」：夹具的 topUpTo 也会写一行流水，
// 一个只看 user_id 的 helper 会在有夹具行的测试里 Fatalf 成「期望恰好 1 行」，
// 而那恰恰不是被测事实。被测事实是「这次 confirm 记了哪一行」。
func pickReturnLog(t *testing.T, logs []creditLogRow, userID int64) creditLogRow {
	t.Helper()
	var hits []creditLogRow
	for _, l := range logs {
		if l.userID == userID && l.refType == model.CreditRefTypeReturn {
			hits = append(hits, l)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("用户 %d 期望恰好 1 行归还流水，实际 %d 行", userID, len(hits))
	}
	return hits[0]
}

func findByTypeCount(t *testing.T, ns []notifRow, typ string) int {
	t.Helper()
	var n int
	for _, x := range ns {
		if x.Type == typ {
			n++
		}
	}
	return n
}

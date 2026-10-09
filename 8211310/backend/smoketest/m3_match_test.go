package smoketest

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"lostfound/internal/apperr"
	"lostfound/internal/repo"
)

// 本文件是计划 §10 的第②层里 M3 的那一段，逐条对照 §12 的 M3 冒烟判据：
//
//	①「黑色钱包@图书馆」lost + 相似 found → GET matches 返回 tier:1、score≥0.75、
//	  **breakdown 恰好三个信号且权重是 0.45/0.40/0.15**
//	②建 found 帖 → **notified_count:1**、match_pairs 新增一行、
//	  **lost 作者收到 new_match、found 作者的通知数为 0**
//	③重复同一对 → **ON CONFLICT 生效**、match_pairs 仍一行、notified_count:0（去重）
//	④建 lost 帖 → 响应带 **matches_preview 数组**、**match_pairs 与 notifications 都 0 行**
//	⑤地点「其他」的 lost → **tier:2 + 四个信号 + notice 文案 + 同样不产生通知**
//
// 再加 #20 的鉴权（401 / 403 / admin）、参数校验，和 #16 改帖重匹配。
//
// ---------- 关于判据③的一处偏差和它的修法（2026-10-07，已与用户确认）----------
//
// match_pairs 的唯一约束是 `UNIQUE(lost_item_id, found_item_id)`，也就是**一对帖子**一条。
// 而 found 帖每次创建都会拿到一个新的 item id，所以判据③那句「重复建同样的 found 帖 →
// 仍一行」**按字面实现是错的**：走 HTTP 两遍发帖永远造不出冲突，第二次是一条新事实
// （另一个人也捡到了类似的东西），该发第二条通知 —— 和定位原则③「认领非排他」同一个逻辑。
//
// 冲突真正发生在**同一对第二次参与匹配**的时候，而唯一能触发它的动作是改帖
// （§3.7 用途① 举的例子本来就是「同一条 found 帖被编辑后重新触发匹配」）。
// 问题是 §5.8 的触发源表里根本没有「编辑」这一行，代码里 Update() 也确实没跑匹配 ——
// 计划自己前后不一致，于是那条 SQL 无处可达，同时还留下一个功能洞：
// 小王把「捡到钱包/其他」改成「黑色长款钱包/图书馆」之后，系统不会重算，
// 小李永远等不到那条通知。
//
// 所以判据③最后由三条测试分担，一条比一条更接近它想守的东西：
//   - TestEditFoundPostRematchesAndDedups：**改帖 → 再改帖**，同一对第二次插入被 ON CONFLICT
//     挡掉，台账和通知都不增行。这是判据③真正可执行的形式，而且是走 HTTP 的。
//   - TestLedgerOnConflictDedup：直接对同一个 (lost, found) 调两次写入，验那条 SQL 本身
//     （含混合批次：旧对跳过、新对照发）。
//   - TestSameLostMatchedByTwoFoundPosts：同一 lost 被两条不同的 found 命中 → 2 行、2 条通知。
//     这条是把「不去重才是对的」那半边钉住，防止有人把去重键改成 lost_item_id。

// ---------- 字典里的已知行（复用 m2_items_test.go 的那几个常量）----------
//
// catWallet=36 衣物箱包→钱包（level 2）、locLibrary=70 教学区→场馆→图书馆（叶子）、
// locOther=3 「其他」（level 1 叶子，is_freeform=true，parent_id NULL）。

// ---------- 响应形状（在测试包里重新声明一遍，理由见 m2_items_test.go）----------

// signal 取 weight / score，外加一个 matched_by。
//
// 前两个够验「权重是 0.45/0.40/0.15 还是 0.35/0.30/0.20/0.15」；matched_by 只有在
// Tier 2 才有意义，而判据⑤要证明的是地点那一档**兜到了自由文本**
// （「其他」的 parent/top 都是 0，任何一档 id 比较都不该命中）。
// 其余键（same_leaf / title_dice）是 matcher 的分解细节，已在第①层钉死，这里不重复。
type signal struct {
	Weight    float64 `json:"weight"`
	Score     float64 `json:"score"`
	MatchedBy string  `json:"matched_by"`
}

type breakdownView struct {
	Score   float64           `json:"score"`
	Tier    int               `json:"tier"`
	Signals map[string]signal `json:"signals"`
}

type matchHitView struct {
	Item      itemSummary   `json:"item"`
	Score     float64       `json:"score"`
	Breakdown breakdownView `json:"breakdown"`
}

type matchesView struct {
	Tier   int            `json:"tier"`
	Notice string         `json:"notice"`
	List   []matchHitView `json:"list"`
}

// dataKeys 复用 m1_auth_test.go 里那一个（同一个包里两份定义会打架，
// 而两份的话其中一份改了另一份不会跟着改）。

func decodeInto(t *testing.T, raw json.RawMessage, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("解析 %s 失败: %v\n原始: %s", string(raw), err, truncate(string(raw)))
	}
}

// ---------- 请求体夹具 ----------

// 这一对是 §2.3 / §2.6 那条主线：小李在图书馆丢了黑色长款钱包，小王在同一个地方捡到。
//
// 分数是能手算的，这也是 M3 判据里「breakdown 作为调试器」的意思：
//
//	分类 同小类 → 1.0 × 0.45 = 0.45
//	文本 标题 dice(「黑色钱包」,「捡到黑色长款钱包」) = 2×2/(3+7) = 0.40（§5.6 的已知值），
//	     描述一字不差 → 1.0 → S_text = 0.5×0.40 + 0.5×1.0 = 0.70 → × 0.40 = 0.28
//	时间 found_at 10:00 落在 [last_seen 09:00, lost_at 12:30] 里 → 1.0 × 0.15 = 0.15
//	─────────────────────────────────────
//	合计 0.88 —— 高于通知线 0.75，也远高于展示线 0.55
const (
	walletTitleLost  = "黑色钱包"
	walletTitleFound = "捡到黑色长款钱包"
	walletDesc       = "黑色长款钱包，夹层里有校园卡"
	detailLibrary    = "三楼自习室B区"

	walletScore = 0.88
)

func lostWalletBody(title, desc string, locationID int64, detail string) map[string]any {
	return map[string]any{
		"item_type":       "lost",
		"title":           title,
		"description":     desc,
		"category_id":     catWallet,
		"location_id":     locationID,
		"location_detail": detail,
		"last_seen_at":    "2026-10-01T09:00:00+08:00",
		"lost_at":         "2026-10-01T12:30:00+08:00",
		"contact":         "13800000000",
	}
}

func foundWalletBody(title, desc string, locationID int64, detail string) map[string]any {
	return map[string]any{
		"item_type":       "found",
		"title":           title,
		"description":     desc,
		"category_id":     catWallet,
		"location_id":     locationID,
		"location_detail": detail,
		"found_at":        "2026-10-01T10:00:00+08:00",
		"contact":         "wx_xiaowang",
	}
}

// ---------- ① + ② + ④：主线 ----------

// TestWalletStoryMatchAndNotify 按 §2.3 / §2.6 的顺序走一遍。
//
// 为什么是一条链而不是三个独立测试：方向不对称这件事只有在「先发 lost 再发 found」
// 这个顺序里才看得见 —— 单独测 found 创建时，库里没有 lost 帖，通知数天然是 0，
// 那种 0 测不出任何东西。
func TestWalletStoryMatchAndNotify(t *testing.T) {
	harness.TruncateAll(t)

	const password = "correct-horse-battery"
	li := harness.RegisterAndLogin(t, "xiaoli", password)   // 失主，发 lost
	xw := harness.RegisterAndLogin(t, "xiaowang", password) // 拾主，发 found

	// ④ 小李发 lost 帖：库里还没有任何 found 帖
	//    → matches_preview 必须出现并且是 []，notified_count 必须不出现，
	//      match_pairs 和 notifications 必须都是 0 行
	rLi := harness.Post(t, "/api/items",
		lostWalletBody(walletTitleLost, walletDesc, locLibrary, detailLibrary), li.Token)
	RequireOK(t, rLi, "小李发失物帖")

	keys := dataKeys(t, rLi)
	if _, ok := keys["matches_preview"]; !ok {
		t.Fatalf("lost 帖的响应里没有 matches_preview 键（判据④要求空命中也出现，是 []）：%s",
			truncate(string(rLi.Data)))
	}
	var preview []matchHitView
	decodeInto(t, keys["matches_preview"], &preview)
	if len(preview) != 0 {
		t.Errorf("库里还没有 found 帖，matches_preview 却是 %d 条", len(preview))
	}
	if _, ok := keys["notified_count"]; ok {
		t.Errorf("lost 帖的响应里出现了 notified_count（%s）—— §4：两个字段互斥，lost 方向谁都不通知",
			string(keys["notified_count"]))
	}
	assertCount(t, "match_pairs", 0)
	assertCount(t, "notifications", 0)

	lost := createResultOf(t, rLi)

	// ① 小李此时查匹配：严格模式的前提成立，但库里一条候选都没有 →
	//    auto 空手而归，降级到 Tier 2 再捞一次，仍然空。
	//    ⚠ 这里期望 tier=2 而不是 1：auto 的定义就是「Tier 1 捞不到就放宽」（§5.5），
	//    降级本身不是错，降级**也不写台账**才是要钉死的那件事。
	rEmpty := harness.Get(t, "/api/items/"+itoa(lost.ID)+"/matches", li.Token)
	RequireOK(t, rEmpty, "小李查自己的匹配（库里还没候选）")
	var empty matchesView
	rEmpty.DataInto(t, &empty)
	if empty.Tier != 2 {
		t.Errorf("Tier 1 空手而归后 auto 应该降到 2，实际 tier=%d", empty.Tier)
	}
	if len(empty.List) != 0 {
		t.Errorf("库里没有 found 帖，匹配列表却有 %d 条", len(empty.List))
	}
	assertCount(t, "match_pairs", 0)

	// ② 小王发 found 帖：同一分类、同一地点、时间落在丢失窗口里 → 0.88
	rXw := harness.Post(t, "/api/items",
		foundWalletBody(walletTitleFound, walletDesc, locLibrary, detailLibrary), xw.Token)
	RequireOK(t, rXw, "小王发拾物帖")

	keys = dataKeys(t, rXw)
	if _, ok := keys["matches_preview"]; ok {
		t.Errorf("found 帖的响应里出现了 matches_preview —— §4：平台不会通知 found 任何东西")
	}
	if _, ok := keys["notified_count"]; !ok {
		t.Fatalf("found 帖的响应里没有 notified_count 键（判据②要求 0 也要出现，这里期望 1）：%s",
			truncate(string(rXw.Data)))
	}
	var notified int
	decodeInto(t, keys["notified_count"], &notified)
	if notified != 1 {
		t.Errorf("notified_count 期望 1（推给了小李一个人），实际 %d", notified)
	}
	found := createResultOf(t, rXw)

	// 台账一行，分数就是那个能手算出来的 0.88
	if got := harness.Count(t, `SELECT count(*) FROM match_pairs`); got != 1 {
		t.Errorf("match_pairs 期望 1 行，实际 %d", got)
	}
	lostID, foundID, score, bd := onlyLedgerRow(t)
	if lostID != lost.ID {
		t.Errorf("台账里的 lost_item_id 应该是 %d（小李那条），实际 %d", lost.ID, lostID)
	}
	if foundID != found.ID {
		t.Errorf("台账里的 found_item_id 应该是 %d（小王那条），实际 %d", found.ID, foundID)
	}
	if diff := score - walletScore; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("落库的 score %v 和手算的 %v 不一致（判据②要求 ≥0.75）—— 要么公式改了要么夹具变了，"+
			"这种不一致正是 breakdown 作为调试器最该抓到的东西", score, walletScore)
	}
	// 落库的 breakdown 必须能解、必须是 Tier 1 的三个信号 —— JSONB 列里躺着的才是真相。
	// outer 传 score **列**：breakdown.score 和 match_pairs.score 必须是同一个数
	// （service 在落库前 round4，所以「响应里看到的」和「台账里记的」不会分叉）。
	var rowBd breakdownView
	decodeInto(t, json.RawMessage(bd), &rowBd)
	assertSignals(t, rowBd, score, map[string]float64{"category": 0.45, "text": 0.40, "time": 0.15})
	if rowBd.Tier != 1 {
		t.Errorf("台账里的 breakdown.tier 应该是 1（Tier 2 的结果一律不写），实际 %d", rowBd.Tier)
	}

	// 判据②的后半句：**found 作者的通知数为 0**
	if got := harness.Count(t, `SELECT count(*) FROM notifications WHERE user_id = $1`, xw.UserID); got != 0 {
		t.Errorf("小王是 found 帖作者，收到 %d 条通知 —— §2.3 明令禁止（他是被动方，发完帖义务就完成了）", got)
	}
	if got := harness.Count(t, `SELECT count(*) FROM notifications WHERE user_id = $1 AND type = 'new_match'`, li.UserID); got != 1 {
		t.Errorf("小李应该收到 1 条 new_match，实际 %d", got)
	}
	notifItemID, notifTitle, notifBody := onlyNotification(t)
	if notifItemID != found.ID {
		t.Errorf("通知的 item_id 应该指向那条 found 帖 %d（点进去就能联系对方），实际 %d", found.ID, notifItemID)
	}
	// 措辞是定位原则 1 的落点，所以它属于契约而不只是文案：不能出现「这就是你的」
	if notifTitle == "" || notifBody == "" {
		t.Errorf("通知标题/正文有空的：title=%q content=%q", notifTitle, notifBody)
	}

	// ① 现在匹配列表里有东西了：tier 1、分数≥0.75、**恰好三个信号且权重是 0.45/0.40/0.15**
	rMatches := harness.Get(t, "/api/items/"+itoa(lost.ID)+"/matches", li.Token)
	RequireOK(t, rMatches, "小李查匹配")
	var mv matchesView
	rMatches.DataInto(t, &mv)

	if mv.Tier != 1 {
		t.Errorf("tier 期望 1，实际 %d", mv.Tier)
	}
	if mv.Notice != "" {
		t.Errorf("Tier 1 不该带放宽筛选的横幅文案：%q", mv.Notice)
	}
	if len(mv.List) != 1 {
		t.Fatalf("匹配列表期望 1 条，实际 %d：%+v", len(mv.List), mv.List)
	}
	hit := mv.List[0]
	if hit.Item.ID != found.ID {
		t.Errorf("匹配到的帖子应该是 %d，实际 %d", found.ID, hit.Item.ID)
	}
	if hit.Score < 0.75 {
		t.Errorf("score 期望 ≥0.75，实际 %v", hit.Score)
	}
	if hit.Breakdown.Tier != 1 {
		t.Errorf("breakdown.tier 期望 1，实际 %d", hit.Breakdown.Tier)
	}
	assertSignals(t, hit.Breakdown, hit.Score,
		map[string]float64{"category": 0.45, "text": 0.40, "time": 0.15})

	// contact 规则（§4 #14 同一条）：匹配列表里的 found 候选必须锁着
	if hit.Item.Contact != nil {
		t.Errorf("found 候选的 contact 出现在匹配列表里（%q）—— 解锁只能走 M4 的 #21，"+
			"否则 contact_views 这份认领名单就不再完整", *hit.Item.Contact)
	}

	// 反向：小王查自己那条 found 帖的匹配，候选是小李的 lost 帖，contact 是公开的
	rBack := harness.Get(t, "/api/items/"+itoa(found.ID)+"/matches", xw.Token)
	RequireOK(t, rBack, "小王查自己的匹配")
	var back matchesView
	rBack.DataInto(t, &back)
	if len(back.List) != 1 {
		t.Fatalf("反向期望 1 条，实际 %d", len(back.List))
	}
	if back.List[0].Item.Contact == nil {
		t.Error("lost 候选的 contact 应该公开（丢东西的人巴不得被联系上）")
	}
	// 查了三次匹配（含空手而归那次），一行台账都不该多出来（§3.7「GET 不写」）
	if got := harness.Count(t, `SELECT count(*) FROM match_pairs`); got != 1 {
		t.Errorf("查了几次 GET matches 之后 match_pairs 变成 %d 行了 —— 查询接口不该写库", got)
	}
}

// TestMatchesEndpointAuthz 是 #20 的鉴权矩阵（HTTP 层，含 401）。
func TestMatchesEndpointAuthz(t *testing.T) {
	harness.TruncateAll(t)

	const password = "correct-horse-battery"
	owner := harness.RegisterAndLogin(t, "matchowner", password)
	stranger := harness.RegisterAndLogin(t, "matchstranger", password)
	admin := harness.MakeAdmin(t, harness.RegisterAndLogin(t, "matchadmin", password))

	lost := createItem(t, owner, lostWalletBody(walletTitleLost, walletDesc, locLibrary, detailLibrary))
	path := "/api/items/" + itoa(lost.ID) + "/matches"

	cases := []struct {
		name  string
		token string
		want  string
	}{
		{"没有 token", "", apperr.CodeUnauthorized},
		{"发帖人本人", owner.Token, apperr.CodeOK},
		{"admin", admin.Token, apperr.CodeOK},
		{"无关用户", stranger.Token, apperr.CodeForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			RequireCode(t, harness.Get(t, path, c.token), c.want)
		})
	}

	// 参数校验：handler 一个都不解析，所以这些都必须变成带字段名的 VALIDATION
	for _, q := range []string{"?top=0", "?top=99999", "?top=abc", "?min_score=2", "?min_score=x", "?tier=3"} {
		t.Run("非法参数 "+q, func(t *testing.T) {
			RequireCode(t, harness.Get(t, path+q, owner.Token), apperr.CodeValidation)
		})
	}

	// 已软删的帖子对外不存在：本人还能查（他自己得知道被下架了什么），别人 404
	RequireOK(t, harness.Do(t, http.MethodDelete, "/api/items/"+itoa(lost.ID), nil, owner.Token), "本人删帖")
	RequireCode(t, harness.Get(t, path, owner.Token), apperr.CodeOK)
	RequireCode(t, harness.Get(t, path, stranger.Token), apperr.CodeNotFound)
}

// TestTier2FreeformLocation 是判据⑤。
//
// 刻意让这一对的分数**高于通知线**：地点选「其他」时走 Tier 2，分数构成是
//
//	0.35 × 1.0(同小类) + 0.30 × 0.70(文本同主线那对) + 0.20 × 1.0(时间在窗口内)
//	+ 0.15 × 0.4(叶子/父/top 全对不上，兜到 location_detail 的 dice=1.0 再打四折)
//	= 0.82
//
// 就算 0.82 也不写台账、不发通知 —— 这才是「Tier 2 置信度不够，不打扰人」这条规则
// 最强的形式（低分不写可能只是巧合，高分不写才说明规则生效了）。
func TestTier2FreeformLocation(t *testing.T) {
	harness.TruncateAll(t)

	const password = "correct-horse-battery"
	xw := harness.RegisterAndLogin(t, "wang", password)
	zhao := harness.RegisterAndLogin(t, "xiaozhao", password)

	// 小王在图书馆捡到（正常地点）
	createItem(t, xw, foundWalletBody(walletTitleFound, walletDesc, locLibrary, detailLibrary))

	// 小赵丢了同一个东西，但地点选「其他」，把「图书馆」写进了自由文本
	rLost := harness.Post(t, "/api/items", map[string]any{
		"item_type":       "lost",
		"title":           walletTitleLost,
		"description":     walletDesc,
		"category_id":     catWallet,
		"location_id":     locOther,
		"location_detail": detailLibrary,
		"last_seen_at":    "2026-10-01T09:00:00+08:00",
		"lost_at":         "2026-10-01T12:30:00+08:00",
		"contact":         "13900000000",
	}, zhao.Token)
	RequireOK(t, rLost, "小赵发失物帖（地点：其他）")

	// 发帖当下：#13 只跑 Tier 1，而 Tier 1 的前提不成立 → 空预览，但键必须在
	keys := dataKeys(t, rLost)
	if _, ok := keys["matches_preview"]; !ok {
		t.Fatal("地点是「其他」的 lost 帖响应里 matches_preview 键消失了")
	}
	var preview []matchHitView
	decodeInto(t, keys["matches_preview"], &preview)
	if len(preview) != 0 {
		t.Errorf("发帖时不该跑 Tier 2（§5.8：#13 只跑 Tier 1），实际给了 %d 条预览", len(preview))
	}
	// 谁都不通知：库里已经有那条 found 帖了，但 lost 方向一行都不写
	assertCount(t, "match_pairs", 0)
	assertCount(t, "notifications", 0)

	lost := createResultOf(t, rLost)

	// ⑤ 查匹配 → auto 直接落到 Tier 2：四个信号 + 横幅文案
	r := harness.Get(t, "/api/items/"+itoa(lost.ID)+"/matches", zhao.Token)
	RequireOK(t, r, "小赵查匹配")
	var mv matchesView
	r.DataInto(t, &mv)

	if mv.Tier != 2 {
		t.Fatalf("地点是「其他」时 tier 期望 2，实际 %d", mv.Tier)
	}
	if mv.Notice == "" {
		t.Error("Tier 2 必须带提示文案（§5.5「API 响应必须带 tier 字段和提示文案」，前端靠它显示黄色横幅）")
	}
	if len(mv.List) != 1 {
		t.Fatalf("Tier 2 放宽了地点，应该能捞到那条图书馆的 found 帖，实际 %d 条", len(mv.List))
	}
	hit := mv.List[0]
	assertSignals(t, hit.Breakdown, hit.Score,
		map[string]float64{"category": 0.35, "text": 0.30, "time": 0.20, "location": 0.15})
	if hit.Score < 0.75 {
		t.Errorf("这一对的分数应该高于通知线才能真正验证「Tier 2 不写」，实际 %v（期望 ≈0.82）", hit.Score)
	}
	// 地点那一档必须是从自由文本来的：「其他」没有父节点也没有 top，
	// 任何一档 id 比较命中都说明祖先列填错了（0 被当成了「同一级」）。
	loc := hit.Breakdown.Signals["location"]
	if loc.MatchedBy != "detail_text" {
		t.Errorf("matched_by 期望 detail_text，实际 %q —— 而 score=%v", loc.MatchedBy, loc.Score)
	}
	if diff := loc.Score - 0.4; diff > 1e-6 || diff < -1e-6 {
		t.Errorf("两段完全相同的自由文本，dice=1.0 再打四折应该是 0.4，实际 %v", loc.Score)
	}

	// 高分也不写：这是判据⑤的后半句
	assertCount(t, "match_pairs", 0)
	assertCount(t, "notifications", 0)

	// 显式 ?tier=1 时不偷偷放宽：给空结果而不是一个「看起来匹配上了」的假象
	strict := harness.Get(t, "/api/items/"+itoa(lost.ID)+"/matches?tier=1", zhao.Token)
	RequireOK(t, strict, "小赵显式要 Tier 1")
	var sv matchesView
	strict.DataInto(t, &sv)
	if sv.Tier != 1 || len(sv.List) != 0 {
		t.Errorf("显式 tier=1 却返回了 tier=%d、%d 条（前提不成立时应该是 1 档空结果）", sv.Tier, len(sv.List))
	}
	if sv.Notice != "" {
		t.Errorf("Tier 1 的空结果不该带放宽筛选的横幅：%q", sv.Notice)
	}
	// 显式放宽（不是降级）也要能拿到 4 个信号
	loose := harness.Get(t, "/api/items/"+itoa(lost.ID)+"/matches?tier=2", zhao.Token)
	RequireOK(t, loose, "小赵显式要 Tier 2")
	var lv matchesView
	loose.DataInto(t, &lv)
	if lv.Tier != 2 || len(lv.List) != 1 {
		t.Errorf("显式 tier=2 期望 1 条，实际 tier=%d、%d 条", lv.Tier, len(lv.List))
	}
	assertCount(t, "match_pairs", 0)
}

// TestSameLostMatchedByTwoFoundPosts 说明「重复建同样的 found 帖」在真实数据下**不会**去重，
// 而且这是对的（理由见文件顶部关于判据③的那段）。
func TestSameLostMatchedByTwoFoundPosts(t *testing.T) {
	harness.TruncateAll(t)

	const password = "correct-horse-battery"
	li := harness.RegisterAndLogin(t, "li2", password)
	a := harness.RegisterAndLogin(t, "finderA", password)
	b := harness.RegisterAndLogin(t, "finderB", password)

	lost := createItem(t, li, lostWalletBody(walletTitleLost, walletDesc, locLibrary, detailLibrary))

	rA := harness.Post(t, "/api/items", foundWalletBody(walletTitleFound, walletDesc, locLibrary, detailLibrary), a.Token)
	RequireOK(t, rA, "A 发拾物帖")
	if n := readNotifiedCount(t, rA); n != 1 {
		t.Errorf("A 那条应该通知 1 个人，实际 notified_count=%v", n)
	}

	rB := harness.Post(t, "/api/items", foundWalletBody("捡到一个黑色钱包", walletDesc, locLibrary, detailLibrary), b.Token)
	RequireOK(t, rB, "B 发拾物帖")
	if n := readNotifiedCount(t, rB); n != 1 {
		t.Errorf("B 那条也应该通知小李 1 个人（这是**另一对**，不是重复），实际 notified_count=%v", n)
	}

	// 台账两行、通知两条 —— 每一行对应一个不同的 found 帖
	if got := harness.Count(t, `SELECT count(*) FROM match_pairs WHERE lost_item_id = $1`, lost.ID); got != 2 {
		t.Errorf("同一 lost 被两条 found 命中，match_pairs 期望 2 行，实际 %d", got)
	}
	if got := harness.Count(t, `SELECT count(*) FROM notifications WHERE user_id = $1`, li.UserID); got != 2 {
		t.Errorf("小李应该收到 2 条（两个不同的拾主各一条），实际 %d", got)
	}
	// 反过来，如果哪天有人把去重键改成 lost_item_id，这条断言会红：
	// 「同一个人被第二个拾主通知」会被吞掉，那是真丢东西的人最不该错过的信息。
}

// TestEditFoundPostRematchesAndDedups 是判据③现在**真正可达**的那条路径：改帖。
//
// 判据原文写的是「重复建同样的 found 帖 → ON CONFLICT 生效」，但走 HTTP 发帖永远拿不到
// 同一个 found_item_id，所以那条 SQL 在发帖路径上不可达（见文件顶部）。
// 真正会让「同一对帖子第二次参与匹配」的动作是**改帖**：§3.7 用途① 举的例子
// 本来就是「同一条 found 帖被编辑后重新触发匹配」。
//
// 而且这条路径不只是为了让判据可测，它补的是一个真实的功能洞：
// 小王随手发了「钱包」、地点选「其他」，没匹配上；后来他把描述改准了。
// 没有这一步的话系统不会重算，小李永远等不到通知 —— 只能天天自己刷广场。
//
// 五步链，每步的台账/通知增量都是精确值而不是「≥」：
//
//	① 弱文本 found 帖（0.70，在展示区、通知线以下）→ 台账 0、通知 0
//	② 改成强文本（0.88）                                  → 台账 +1、小李 +1 条、小王 +0 条
//	③ 再改一次（同一对第二次插入 → ON CONFLICT）          → 台账 +0、通知 +0  ← 判据③
//	④ 改的是 lost 帖（不对称在改帖上同样成立）            → 台账 +0、通知 +0
//	⑤ 关掉那条 found 帖再改（closed 不该打扰已结束寻找的人）→ 台账 +0、通知 +0
func TestEditFoundPostRematchesAndDedups(t *testing.T) {
	harness.TruncateAll(t)

	const password = "correct-horse-battery"
	li := harness.RegisterAndLogin(t, "li4", password)
	xw := harness.RegisterAndLogin(t, "wang4", password)

	lost := createItem(t, li, lostWalletBody(walletTitleLost, walletDesc, locLibrary, detailLibrary))

	ledger := func() int {
		return harness.Count(t, `SELECT count(*) FROM match_pairs WHERE lost_item_id = $1`, lost.ID)
	}
	liNotices := func() int {
		return harness.Count(t, `SELECT count(*) FROM notifications WHERE user_id = $1`, li.UserID)
	}
	xwNotices := func() int {
		return harness.Count(t, `SELECT count(*) FROM notifications WHERE user_id = $1`, xw.UserID)
	}

	// ① 「钱包」+ 空描述：0.45（同小类）+ 0.40×0.25（标题 dice=0.5、描述 0，各占一半）
	//    + 0.15（拾获时间落在丢失窗口）= 0.70。
	//    高于展示线 0.55（小李在 #20 里看得见它）、低于通知线 0.75（不该发通知）。
	//    这一步的作用是让 ② 有一个「从 0 变成 1」的对照 —— 一开始就满分的话，
	//    「改帖触发了一次写入」和「写入其实是发帖时留下的」分不出来。
	sloppy := createItem(t, xw, foundWalletBody("钱包", "", locLibrary, detailLibrary))
	if n := ledger(); n != 0 {
		t.Fatalf("① 0.70 低于通知线，不该写台账，实际 %d 行", n)
	}
	assertCount(t, "notifications", 0)

	// ② 改准了 → 同一个 found_item_id 第一次真正够线。
	rPut := harness.Do(t, http.MethodPut, "/api/items/"+itoa(sloppy.ID),
		foundWalletBody(walletTitleFound, walletDesc, locLibrary, detailLibrary), xw.Token)
	RequireOK(t, rPut, "② 小王把拾物帖改准")
	if n := ledger(); n != 1 {
		t.Errorf("② 改帖后应该写一行台账，实际 %d 行", n)
	}
	if n := liNotices(); n != 1 {
		t.Errorf("② 小李应该因为这次改帖收到 1 条 new_match，实际 %d 条", n)
	}
	if n := xwNotices(); n != 0 {
		t.Errorf("② 小王是拾物方，平台不通知 found 作者任何东西，实际收到 %d 条", n)
	}
	// 通知正文用的是**改过之后**的标题。这一条顺带证明了匹配读的是更新后的行
	// 而不是请求体里那份半成品 —— 如果 OnUpdated 拿的是改之前的数据，
	// 分数根本够不到 0.75，台账也不会有行，但日志文案错这件事只有查内容才会发现。
	nItemID, _, nContent := onlyNotification(t)
	if nItemID != sloppy.ID {
		t.Errorf("② 通知挂的 item 应该是那条 found 帖 %d，实际 %d", sloppy.ID, nItemID)
	}
	if !strings.Contains(nContent, walletTitleFound) {
		t.Errorf("② 通知正文里应该是改后的标题「%s」，实际：%s", walletTitleFound, truncate(nContent))
	}

	// #16 的响应形状没变：匹配结果不进改帖响应（想看得用 #20）。
	// 这条断言防的是「顺手把 notified_count 塞进所有写接口」——那会让 #16 的契约
	// 随帖子类型变化，而前端表单是同一个。
	if _, ok := dataKeys(t, rPut)["notified_count"]; ok {
		t.Errorf("② 改帖响应里出现了 notified_count，#16 的契约不该带它：%s", truncate(string(rPut.Data)))
	}

	// ③ **判据③本体**：同一对第二次参与匹配。
	//    只把 location_detail 动一下（够让 UPDATE 真的执行、updated_at 变掉），
	//    分数仍然是 0.88，仍然 ≥ 通知线，于是 RecordMatches 又被调用一次 ——
	//    这次 ON CONFLICT 吃掉它，台账不增行、通知不发。
	//    没有这条约束的话，小王每改一次帖子（改错别字、换电话）都会把小李通知一遍。
	twice := foundWalletBody(walletTitleFound, walletDesc, locLibrary, "三楼自习室C区")
	RequireOK(t, harness.Do(t, http.MethodPut, "/api/items/"+itoa(sloppy.ID), twice, xw.Token),
		"③ 第二次改同一条拾物帖")
	if n := ledger(); n != 1 {
		t.Errorf("③ 同一对第二次插入应该被 ON CONFLICT 挡掉，台账期望仍 1 行，实际 %d 行", n)
	}
	if n := liNotices(); n != 1 {
		t.Errorf("③ 不该重复通知同一个人，小李的 new_match 期望仍 1 条，实际 %d 条", n)
	}

	// ④ 改 lost 帖不写台账也不发通知：不对称是「方向」的规则，不是「发帖」的规则。
	//    这里数的是**全表**而不是 lost.ID 那一行：假如有人在 OnUpdated 里漏了类型判断，
	//    改一条 lost 帖会拿 found 帖当候选往台账里写，那一行的 lost_item_id 是**候选的 id**，
	//    按 lost.ID 数出来还是 1，测试照样绿。数全表才挡得住方向写反。
	RequireOK(t, harness.Do(t, http.MethodPut, "/api/items/"+itoa(lost.ID),
		lostWalletBody(walletTitleLost, walletDesc+"（还有一把钥匙）", locLibrary, detailLibrary), li.Token),
		"④ 小李改自己的失物帖")
	assertCount(t, "match_pairs", 1)
	assertCount(t, "notifications", 1)

	// ⑤ 东西已经还回来了（closed），之后又冒出第二条相似的失物帖，
	//    小王这时改他那条 closed 的拾物帖（§8 允许改 closed 帖，比如改错了的电话）。
	//    ⚠ 这一对是**全新的**，台账里没有：所以这一步真正测的是「closed 不跑匹配」这道门槛。
	//    如果 OnUpdated 只看类型不看状态，这里会多写一行台账、多给小赵发一条通知 ——
	//    而那条通知说的是「你丢的东西可能有匹配」，对方捡到并已经归还的东西根本不可能还给他。
	li2 := harness.RegisterAndLogin(t, "li5", password)
	late := createItem(t, li2, lostWalletBody(walletTitleLost, walletDesc, locLibrary, detailLibrary))
	if n := harness.Count(t, `SELECT count(*) FROM match_pairs WHERE lost_item_id = $1`, late.ID); n != 0 {
		t.Fatalf("⑤ 发 lost 帖不该写台账（不对称），实际 %d 行", n)
	}

	harness.SetItemStatus(t, sloppy.ID, "closed")
	RequireOK(t, harness.Do(t, http.MethodPut, "/api/items/"+itoa(sloppy.ID),
		foundWalletBody(walletTitleFound, walletDesc, locLibrary, "一楼大厅服务台"), xw.Token),
		"⑤ 改一条 closed 的拾物帖必须成功")
	assertCount(t, "match_pairs", 1)
	assertCount(t, "notifications", 1)
	if n := harness.Count(t, `SELECT count(*) FROM notifications WHERE user_id = $1`, li2.UserID); n != 0 {
		t.Errorf("⑤ 小赵不该收到任何匹配通知（那条拾物帖已经归还了），实际 %d 条", n)
	}
}

// TestLedgerOnConflictDedup 直接在 repo 层验 ON CONFLICT —— 判据③说的「去重」本身。
//
// 和上面那条改帖链的分工：HTTP 那条测的是「业务代码真的会走到这条 SQL」，
// 这里测的是这条 SQL 本身在真 PostgreSQL 上的行为 —— 第一次 1 行、第二次 0 行、
// 通知只跟着受影响行数走。$4::jsonb 的转型、score 列的 NUMERIC(5,4)、
// 以及 notifications 那两条 INSERT 的列名，都是纯单测和 fake store 永远碰不到的东西。
//
// 这里还要多测一件 HTTP 测不到的事：**rows 为空时不开事务**（repo 里那条提前 return），
// 以及「同一批里一半是新对一半是旧对」的混合情况。
func TestLedgerOnConflictDedup(t *testing.T) {
	harness.TruncateAll(t)

	const password = "correct-horse-battery"
	li := harness.RegisterAndLogin(t, "li3", password)
	xw := harness.RegisterAndLogin(t, "wang3", password)

	lost := createItem(t, li, lostWalletBody(walletTitleLost, walletDesc, locLibrary, detailLibrary))
	found := createItem(t, xw, foundWalletBody(walletTitleFound, walletDesc, locLibrary, detailLibrary))

	// 上面那条 found 帖发帖时自己就写了一行台账（那正是判据②，已经在主线测过了）。
	// 这条测试要从「空表」数起，所以只清这两张表、留着帖子 ——
	// 外键 id 必须是真的，否则 match_pairs 那两条 INSERT 会被外键挡下来，
	// 报的是约束冲突而不是「去重生效」。
	clearLedger(t)
	if got := harness.Count(t, `SELECT count(*) FROM match_pairs`); got != 0 {
		t.Fatalf("清场没生效，后面数不出增量：%d 行", got)
	}

	m := repo.NewMatch(harness.Pool)
	ctx := context.Background()
	rows := []repo.LedgerRow{{
		LostItemID:  lost.ID,
		FoundItemID: found.ID,
		Score:       walletScore,
		Breakdown:   `{"score":0.88,"tier":1,"signals":{"category":{"weight":0.45,"score":1.0}}}`,
		NotifyTo:    li.UserID,
		NotifyTitle: "你丢失的「黑色钱包」可能有匹配",
		NotifyBody:  "请自行核对物品特征后再联系对方。",
	}}

	first, err := m.RecordMatches(ctx, rows)
	if err != nil {
		t.Fatalf("第一次写台账失败（JSONB 转型或 NUMERIC 精度对不上？这是真 SQL 才会暴露的问题）: %v", err)
	}
	if first != 1 {
		t.Errorf("第一次写入期望返回 1 条通知，实际 %d", first)
	}

	second, err := m.RecordMatches(ctx, rows)
	if err != nil {
		t.Fatalf("第二次写台账失败（ON CONFLICT 之后事务状态没回收好？）: %v", err)
	}
	if second != 0 {
		t.Errorf("同一对重复写入期望返回 0 条（去重生效），实际 %d", second)
	}

	if got := harness.Count(t, `SELECT count(*) FROM match_pairs`); got != 1 {
		t.Errorf("ON CONFLICT 之后 match_pairs 期望仍 1 行，实际 %d", got)
	}
	if got := harness.Count(t, `SELECT count(*) FROM notifications`); got != 1 {
		t.Errorf("去重命中时不该多插通知，notifications 期望 1 行，实际 %d", got)
	}

	// 混合批次：同一批里一条是旧对、一条是新对 → 只发新对那条的通知，返回 1。
	// 这条测的是 RecordMatches 循环里那个 continue：把「唯一冲突」当成错误往上抛的话，
	// 一整个帖子的通知会因为其中一条早就记过而全部消失，而发帖人那边已经是 200 了。
	// 用另一个失主的一条 lost 帖当新对（发 lost 帖自己不写台账，所以行数仍然只涨这一条）。
	other := createItem(t, li, lostWalletBody("黑色雨伞", "黑色长柄雨伞，伞尖有缺口", locLibrary, detailLibrary))
	mixed, err := m.RecordMatches(ctx, []repo.LedgerRow{
		{LostItemID: lost.ID, FoundItemID: found.ID, Score: walletScore,
			Breakdown: rows[0].Breakdown, NotifyTo: li.UserID,
			NotifyTitle: "旧对，不该再发", NotifyBody: "不该出现"},
		{LostItemID: other.ID, FoundItemID: found.ID, Score: walletScore,
			Breakdown: rows[0].Breakdown, NotifyTo: li.UserID,
			NotifyTitle: "新对，应该发", NotifyBody: "应该出现"},
	})
	if err != nil {
		t.Fatalf("混合批次写入失败: %v", err)
	}
	if mixed != 1 {
		t.Errorf("混合批次期望只发 1 条通知（旧对那条被 ON CONFLICT 挡掉），实际 %d", mixed)
	}
	if got := harness.Count(t, `SELECT count(*) FROM match_pairs`); got != 2 {
		t.Errorf("混合批次之后 match_pairs 期望 2 行，实际 %d", got)
	}
	if got := harness.Count(t, `SELECT count(*) FROM notifications WHERE title = $1`, "旧对，不该再发"); got != 0 {
		t.Errorf("旧对那条居然也发了通知，notifications 里 title=「旧对，不该再发」有 %d 行", got)
	}

	// 空批次不该开事务（§9 的日志噪音问题），也不该报错
	n, err := m.RecordMatches(ctx, nil)
	if err != nil || n != 0 {
		t.Errorf("rows 为空时应该直接返回 (0, nil)，实际 (%d, %v)", n, err)
	}
}

// ---------- 辅助 ----------

// assertSignals 验 breakdown 里**恰好**是这几个信号、权重一个不差、且自洽。
//
// 键的个数用 map 的长度来判（而不是数 struct 的字段），因为这是线上字节：
// matcher 的 Signals struct 用 `json:"location,omitempty"` 指针来实现
// 「Tier 1 三个、Tier 2 四个」，只有解成 map 才看得出那个键到底在不在。
// 这也是 §5.4「S_attr 随 color/brand 一起删了」的机器验证 —— 多一个 attr 键就红。
//
// outer 是外层那个 score（hit.Score 或台账的 score 列）：breakdown.score 必须和它
// 是同一个数，否则「响应里看到的分数」和「台账里记的分数」会分叉。传 0 表示不比这一项。
func assertSignals(t *testing.T, bd breakdownView, outer float64, want map[string]float64) {
	t.Helper()

	if len(bd.Signals) != len(want) {
		t.Fatalf("信号个数期望 %d（Tier 1 三个 / Tier 2 四个），实际 %d：%v",
			len(want), len(bd.Signals), keysOf(bd.Signals))
	}
	var sum, hand float64
	for name, w := range want {
		s, ok := bd.Signals[name]
		if !ok {
			t.Errorf("缺少 %q 信号（实际键：%v）", name, keysOf(bd.Signals))
			continue
		}
		if diff := s.Weight - w; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s 信号权重期望 %v，实际 %v", name, w, s.Weight)
		}
		if s.Score < 0 || s.Score > 1 {
			t.Errorf("%s 信号分数应该在 [0,1]，实际 %v", name, s.Score)
		}
		sum += s.Weight
		hand += s.Weight * s.Score
	}
	// 权重加起来必须是 1.0：分数才天然落在 [0,1]，前端才可能手算验算（§5.7）
	if diff := sum - 1.0; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("这一档的权重之和是 %v，应该是 1.0", sum)
	}
	// 总分必须等于「权重 × 分量」的手算结果 —— breakdown 作为调试器的全部价值就是这一行
	if diff := hand - bd.Score; diff > 1e-4 || diff < -1e-4 {
		t.Errorf("breakdown 自相矛盾：Σ(weight×score)=%v 但 score=%v —— 前端按 §5.7 手算会对不上", hand, bd.Score)
	}
	if outer != 0 {
		if diff := bd.Score - outer; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("breakdown.score (%v) 和外层 score (%v) 不是同一个数", bd.Score, outer)
		}
	}
}

func assertCount(t *testing.T, table string, want int) {
	t.Helper()
	if got := harness.Count(t, `SELECT count(*) FROM `+table); got != want {
		t.Errorf("%s 期望 %d 行，实际 %d", table, want, got)
	}
}

// clearLedger 只清 match_pairs 和 notifications 两张表，帖子和用户留着。
func clearLedger(t *testing.T) {
	t.Helper()
	if _, err := harness.Pool.Exec(context.Background(),
		`TRUNCATE match_pairs, notifications RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("清空台账失败: %v", err)
	}
}

// onlyLedgerRow 读出表里**唯一**那行台账。
//
// 为什么不用 harness.QueryRow 拿 map：match_pairs.score 是 NUMERIC，pgx 默认解成
// pgtype.Numeric 而不是 float64，那个断言会写成 `row["score"].(float64)` 然后 panic，
// 而 panic 的报错信息完全看不出「只是类型选错了」。这里在 SQL 里 ::float8、::text
// 转好，Scan 进有类型的变量，编译期就把错挡住了。
func onlyLedgerRow(t *testing.T) (lostID, foundID int64, score float64, breakdown string) {
	t.Helper()
	ctx := context.Background()
	var bd []byte
	err := harness.Pool.QueryRow(ctx,
		`SELECT lost_item_id, found_item_id, score::float8, breakdown::text FROM match_pairs`).
		Scan(&lostID, &foundID, &score, &bd)
	if err != nil {
		t.Fatalf("读台账一行失败（表里是不是不止一行？）: %v", err)
	}
	return lostID, foundID, score, string(bd)
}

// onlyNotification 读出表里**唯一**那条通知的 item_id / title / content。
func onlyNotification(t *testing.T) (itemID int64, title, content string) {
	t.Helper()
	var nullable int64
	err := harness.Pool.QueryRow(context.Background(),
		`SELECT COALESCE(item_id, 0), title, content FROM notifications`).
		Scan(&nullable, &title, &content)
	if err != nil {
		t.Fatalf("读通知一行失败（表里是不是不止一行？）: %v", err)
	}
	return nullable, title, content
}

func readNotifiedCount(t *testing.T, r Response) int {
	t.Helper()
	keys := dataKeys(t, r)
	raw, ok := keys["notified_count"]
	if !ok {
		t.Fatalf("found 帖的响应里没有 notified_count 键：%s", truncate(string(r.Data)))
	}
	var n int
	decodeInto(t, raw, &n)
	return n
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

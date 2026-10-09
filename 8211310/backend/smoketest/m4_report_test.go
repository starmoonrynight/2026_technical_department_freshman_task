package smoketest

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
)

// 本文件是 M4 第②层的第三段：#41 用户举报。对照 §12 的 M4 判据后半段：
//
//	① 举报一条帖 → reports 新增一行、status='open'
//	② **被举报人没收到任何通知、帖子 status 没变、广场排序没变**
//	   （验证「只记录不仲裁」这条契约，是本里程碑最重要的一条断言）
//	③ 同一人重复举报同一帖 → REPORT_DUPLICATE
//	④ 换一个用户举报同一帖 → 成功（多人举报是优先处理信号）
//	⑤ reason_code 传表外的值 → VALIDATION
//
// 判据②为什么是重点：举报是最容易被做成武器的功能（§14-13）。两个人互相看不顺眼，
// 只要「举报会让对方下沉」，灌掉对方的曝光就是零成本的攻击。
// 所以这里要证的不是「举报能用」，而是**举报什么都改变不了** ——
// 帖子还在原来的位置、状态没变、被举报人毫不知情、举报人自己也不多什么。
// 这类「没有发生什么」的断言在其它层没法测，只能对着数据库和一次完整的广场列表比。

// ---------- 夹具 ----------

func reportPassword() string { return "correct-horse-battery" }

func reportItem(t *testing.T, itemID int64, body any, token string) Response {
	t.Helper()
	return harness.Post(t, "/api/items/"+itoa(itemID)+"/report", body, token)
}

func spamBody(detail string) map[string]any {
	return map[string]any{"reason_code": model.ReportReasonSpam, "detail": detail}
}

// reportsRow 读回那条举报，用于「HTTP 响应好看但库里是错的」这一类失败。
func reportsRow(t *testing.T, reportID int64) map[string]any {
	t.Helper()
	return harness.QueryRow(t, `SELECT item_id, reporter_id, reason_code, detail, status
	                           FROM reports WHERE id = $1`, reportID)
}

func reportsCountFor(t *testing.T, itemID int64) int {
	t.Helper()
	return harness.Count(t, `SELECT count(*) FROM reports WHERE item_id = $1`, itemID)
}

func notificationsFor(t *testing.T, userID int64) int {
	t.Helper()
	return harness.Count(t, `SELECT count(*) FROM notifications WHERE user_id = $1`, userID)
}

func creditOf(t *testing.T, userID int64) int {
	t.Helper()
	return harness.Count(t, `SELECT credit_score FROM users WHERE id = $1`, userID)
}

func itemStatusOf(t *testing.T, itemID int64) string {
	t.Helper()
	row := harness.QueryRow(t, `SELECT status FROM items WHERE id = $1`, itemID)
	s, ok := row["status"].(string)
	if !ok {
		t.Fatalf("items.status 读出来不是字符串：%#v", row["status"])
	}
	return s
}

// squareIDs 是广场默认那一页的 id 顺序（顺序本身就是被测对象）。
func squareIDs(t *testing.T) []int64 {
	t.Helper()
	return idsOf(fetchSquare(t, "page_size=100", ""))
}

// ---------- 判据链 ----------

// TestM4ReportChain 按 §12 那五步的顺序走一遍，中间不重新建数据 ——
// 「② 什么都没变」只有拿①前后两次同样的观测来比才成立。
func TestM4ReportChain(t *testing.T) {
	harness.TruncateAll(t)
	password := reportPassword()

	owner := harness.RegisterAndLogin(t, "m4rpowner", password)
	reporter := harness.RegisterAndLogin(t, "m4rpfirst", password)
	second := harness.RegisterAndLogin(t, "m4rpsecond", password)

	// 广场上放三条帖子，让「排序没变」这件事有可比的内容。
	// 建帖顺序是有讲究的：found 帖先建（此刻库里还没有 lost 帖，匹配写不出通知），
	// 两条 lost 帖后建（lost 侧那一跳按设计不落通知）。见 §4 的不对称。
	target := createItem(t, owner, foundBody("被举报的拾物帖", "13800001234"))
	before := createItem(t, reporter, lostBody("举报之前就在的帖子", "13800005678"))
	createItem(t, second, lostBody("别人发的无关帖", "13800009999"))
	orderBefore := squareIDs(t)
	if len(orderBefore) != 3 {
		t.Fatalf("广场期望 3 条帖子，实际 %v（建帖夹具没生效？）", orderBefore)
	}
	statusBefore := itemStatusOf(t, target.ID)
	ownerCredit, reporterCredit := creditOf(t, owner.UserID), creditOf(t, reporter.UserID)

	// ① 举报 → 一行、status='open'
	r := reportItem(t, target.ID, spamBody("连续发了五条一模一样的广告"), reporter.Token)
	RequireOK(t, r, "第一次举报")

	var res struct {
		ID        int64  `json:"id"`
		Status    string `json:"status"`
		CreatedAt string `json:"created_at"`
	}
	r.DataInto(t, &res)
	if res.ID == 0 || res.Status != "open" || res.CreatedAt == "" {
		t.Fatalf("#41 的 data 形状应该是 {id,status:open,created_at}，实际 %+v", res)
	}
	if got := reportsCountFor(t, target.ID); got != 1 {
		t.Errorf("reports 期望新增 1 行，实际 %d", got)
	}
	row := reportsRow(t, res.ID)
	if asInt64(t, row["item_id"]) != target.ID {
		t.Errorf("举报挂错了帖子：%v，期望 %d", row["item_id"], target.ID)
	}
	if asInt64(t, row["reporter_id"]) != reporter.UserID {
		t.Errorf("reporter_id 应该来自 token（%d），实际 %v —— 这是能不能伪造举报人的分水岭",
			reporter.UserID, row["reporter_id"])
	}
	if row["reason_code"] != model.ReportReasonSpam {
		t.Errorf("reason_code 落库成了 %v", row["reason_code"])
	}
	// ⚠ 库里那一行的状态必须是 open。#41 响应里的 status 是自己拼的常量，
	// 只有读回库里这一行才能证明「用户提交的那一刻，平台没有替 admin 做任何表态」。
	if row["status"] != "open" {
		t.Errorf("库里 status 是 %v —— M4 只能产生 open，resolved/dismissed 是 M6 里 admin 的决定", row["status"])
	}

	// ② 举报零自动后果（本里程碑最重要的一条）
	if got := notificationsFor(t, owner.UserID); got != 0 {
		t.Errorf("被举报人收到了 %d 条通知 —— 举报立刻变成骚扰工具：你举报我一次我就知道是你，"+
			"下次我举报回去（§3.7「有一条通知绝对不发」）", got)
	}
	if got := itemStatusOf(t, target.ID); got != statusBefore {
		t.Errorf("举报把帖子状态从 %q 改成了 %q —— 平台只记录事实，不裁决（定位原则 1）", statusBefore, got)
	}
	if got := squareIDs(t); !equalIDs(got, orderBefore) {
		t.Errorf("举报改变了广场排序：%v → %v —— 举报数不是排序信号，否则刷举报就能压掉别人", orderBefore, got)
	}
	if got := creditOf(t, owner.UserID); got != ownerCredit {
		t.Errorf("被举报人信用分被举报改动了：%d → %d", ownerCredit, got)
	}
	if got := creditOf(t, reporter.UserID); got != reporterCredit {
		t.Errorf("举报人自己也被扣分/加分了：%d → %d —— 举报不是积分行为", reporterCredit, got)
	}
	// 被举报的人连「谁举报了我」都查不到：M4 没有任何读 reports 的端点
	RequireCode(t, harness.Get(t, "/api/items/"+itoa(target.ID)+"/reports", owner.Token), apperr.CodeNotFound)

	// ③ 同一人重复举报同一帖 → REPORT_DUPLICATE，而且不增行
	dup := reportItem(t, target.ID, spamBody("再举报一次"), reporter.Token)
	RequireCode(t, dup, apperr.CodeReportDuplicate)
	if got := reportsCountFor(t, target.ID); got != 1 {
		t.Errorf("重复举报之后 reports 有 %d 行（部分唯一索引 uq_reports_open 没起作用？）", got)
	}
	// 换一个 reason_code 也算重复：挡的是「同人对同帖的待处理举报」，不是「同一条理由」
	RequireCode(t, reportItem(t, target.ID,
		map[string]any{"reason_code": model.ReportReasonFraud, "detail": "换个理由再举报"}, reporter.Token),
		apperr.CodeReportDuplicate)

	// ④ 换一个用户举报同一帖 → 成功，两行（多人举报是优先处理信号）
	r2 := reportItem(t, target.ID, spamBody("我也觉得是广告"), second.Token)
	RequireOK(t, r2, "第二个人举报同一帖")
	if got := reportsCountFor(t, target.ID); got != 2 {
		t.Errorf("两个人各举报一次，期望 2 行，实际 %d —— 排他就是把举报当成了认领", got)
	}
	var res2 struct {
		ID int64 `json:"id"`
	}
	r2.DataInto(t, &res2)
	if asInt64(t, reportsRow(t, res2.ID)["reporter_id"]) != second.UserID {
		t.Error("第二行举报人记错了")
	}

	// ⑤ 表外的 reason_code → VALIDATION
	RequireCode(t, reportItem(t, target.ID,
		map[string]any{"reason_code": "inappropriate", "detail": ""}, second.Token), apperr.CodeValidation)

	// 整条链结束：帖子还在原地
	if got := itemStatusOf(t, target.ID); got != statusBefore {
		t.Errorf("整条举报链跑完，帖子状态变成了 %q", got)
	}
	if got := squareIDs(t); !equalIDs(got, orderBefore) {
		t.Errorf("整条链跑完广场排序变了：%v → %v", orderBefore, got)
	}

	// 举报也一条都没让别人的帖子消失 —— 它不是下架工具
	if got := itemStatusOf(t, before.ID); got != "open" {
		t.Errorf("无关帖子的状态变成了 %q", got)
	}
}

// asInt64 把 QueryRow 返回的整数列取出来。pgx 对 int/bigint 的返回类型不完全稳定
// （取决于列的类型和驱动版本），所以这里一次转换、失败就 fatal，
// 而不是在每个断言里写一遍类型判断。
func asInt64(t *testing.T, v any) int64 {
	t.Helper()
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	default:
		t.Fatalf("期望一个整数，实际是 %#v（%T）", v, v)
		return 0
	}
}

// ---------- 白名单与长度 ----------

// TestM4ReportAcceptsEveryReason 逐个提交六个 reason_code。
//
// 这里的每一个都必须真的插进库：迁移里 reports_reason_code_check 是那条 CHECK 的真相，
// 而 model.ReportReasonCodes 只是它的镜像。镜像里多一个字母（或者 CHECK 少一个），
// 单元测试发现不了 —— 只有插一次才知道。
func TestM4ReportAcceptsEveryReason(t *testing.T) {
	harness.TruncateAll(t)
	password := reportPassword()

	owner := harness.RegisterAndLogin(t, "m4codes_owner", password)
	target := createItem(t, owner, foundBody("六个理由都能提交的帖子", "13800002345"))

	for i, code := range model.ReportReasonCodes {
		who := harness.RegisterAndLogin(t, "m4code"+itoa(int64(i)), password)
		r := reportItem(t, target.ID, map[string]any{"reason_code": code, "detail": "第 " + itoa(int64(i)) + " 个人"}, who.Token)
		RequireOK(t, r, "合法 reason_code "+code)

		// 落库的必须是同一个 code（不是被规范化掉的别的值）
		var stored string
		if err := harness.Pool.QueryRow(context.Background(),
			`SELECT reason_code FROM reports WHERE reporter_id = $1`, who.UserID).Scan(&stored); err != nil {
			t.Fatalf("读回 %s 那行失败: %v", code, err)
		}
		if stored != code {
			t.Errorf("reason_code %q 落库成了 %q", code, stored)
		}
	}
}

// TestM4ReportRejectsBadReason 钉住 #41 的 reason_code 白名单在 HTTP 这一层同样生效，
// 顺便把「trim 之后才是进库的那一份」在真库上验一次。
func TestM4ReportRejectsBadReason(t *testing.T) {
	harness.TruncateAll(t)
	password := reportPassword()

	owner := harness.RegisterAndLogin(t, "m4badreason", password)
	who := harness.RegisterAndLogin(t, "m4badreasonr", password)
	target := createItem(t, owner, foundBody("理由不合法的举报帖", "13800003456"))

	// 注意列表里没有 "spam "：白名单比的是 trim 之后的值，带空格的合法码是**合法**的，
	// 下面单独验它落库时的那个形状。
	for _, code := range []string{"", "   ", "Spam", "未知", "垃圾广告", "other_", "spam,fraud"} {
		t.Run("reason_code="+code, func(t *testing.T) {
			RequireCode(t, reportItem(t, target.ID,
				map[string]any{"reason_code": code, "detail": "x"}, who.Token), apperr.CodeValidation)
		})
	}
	if got := reportsCountFor(t, target.ID); got != 0 {
		t.Errorf("七次被拒的举报写出了 %d 行", got)
	}

	// 完全不带 reason_code
	RequireCode(t, reportItem(t, target.ID, map[string]any{"detail": "忘了填理由"}, who.Token), apperr.CodeValidation)
	// 请求体不是 JSON
	RequireCode(t, reportItem(t, target.ID, nil, who.Token), apperr.CodeValidation)
	if got := reportsCountFor(t, target.ID); got != 0 {
		t.Errorf("缺字段 / 非 JSON 的请求写出了 %d 行", got)
	}

	// reason_code 与 detail 两侧的空白都会被削掉，而削完的那份才是进库的那份。
	// 第①层用 fake store 钉过「交给 repo 的字符串是 trim 过的」，这一条钉的是它在真库上成立：
	// 少了这一步，那个 trim 只存在于单元测试的想象里，真实结果是
	// reports_reason_code_check 上的一条 23514 —— 用户看到的会是 INTERNAL。
	RequireOK(t, reportItem(t, target.ID,
		map[string]any{"reason_code": " spam ", "detail": "  带空格的举报  "}, who.Token), "两侧带空格的合法举报")
	row := harness.QueryRow(t, `SELECT reason_code, detail FROM reports WHERE item_id = $1`, target.ID)
	if row["reason_code"] != model.ReportReasonSpam {
		t.Errorf("reason_code 落库成了 %q —— 带空格直接撞 CHECK 了", row["reason_code"])
	}
	if row["detail"] != "带空格的举报" {
		t.Errorf("detail 落库成了 %q，期望两侧空白已被削掉", row["detail"])
	}
}

// TestM4ReportDetailIsCountedInRunes 用真实字符数而不是字节数验 detail 上限。
//
// 500 个汉字是 1500 字节，PostgreSQL 的 VARCHAR(500) 数的是**字符**，
// 所以那一条必须成功。如果哪天有人把校验改成 len(detail)，
// 中文用户会在写了 167 个字时被拒，而报错说的是「太长了」—— 这条测试会立刻红。
func TestM4ReportDetailIsCountedInRunes(t *testing.T) {
	harness.TruncateAll(t)
	password := reportPassword()

	owner := harness.RegisterAndLogin(t, "m4lenowner", password)
	target := createItem(t, owner, foundBody("长度上限用的帖子", "13800004567"))

	cases := []struct {
		name    string
		detail  string
		wantErr string
		who     string
	}{
		{"500 个汉字", strings.Repeat("汉", 500), "", "m4len1"},
		{"501 个汉字", strings.Repeat("汉", 501), apperr.CodeValidation, "m4len2"},
		{"500 个字母", strings.Repeat("a", 500), "", "m4len3"},
		{"501 个字母", strings.Repeat("a", 501), apperr.CodeValidation, "m4len4"},
		{"空 detail（选填）", "", "", "m4len5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			who := harness.RegisterAndLogin(t, c.who, password)
			r := reportItem(t, target.ID, spamBody(c.detail), who.Token)
			if c.wantErr != "" {
				RequireCode(t, r, c.wantErr)
				return
			}
			RequireOK(t, r, "detail 合法长度")
		})
	}
}

// ---------- 帖子状态与类型 ----------

// TestM4ReportTargets 决定谁能被举报：所有**存在的**帖子，两种类型、open 和 closed 都行。
//
// deleted 是唯一被排除的（对外已经不存在，举报它在数据上没有归宿）。
// closed 仍然可举报：「东西还回去了」不等于「这条帖子没问题」，
// 一条靠虚假信息成功归还的帖子照样需要让 admin 看见。
func TestM4ReportTargets(t *testing.T) {
	harness.TruncateAll(t)
	password := reportPassword()

	owner := harness.RegisterAndLogin(t, "m4tgtowner", password)
	who := harness.RegisterAndLogin(t, "m4tgtreader", password)

	open := createItem(t, owner, foundBody("open 的帖子可以举报", "13800005678"))
	lost := createItem(t, owner, lostBody("lost 的帖子也可以举报", "13800006789"))
	closed := createItem(t, owner, foundBody("已归还但仍有问题的帖子", "13800007890"))
	RequireOK(t, harness.Do(t, http.MethodPatch, "/api/items/"+itoa(closed.ID)+"/status",
		map[string]any{"status": "closed"}, owner.Token), "关闭一条帖子")
	deleted := createItem(t, owner, foundBody("已经下架的帖子", "13800008901"))
	harness.SetItemStatus(t, deleted.ID, "deleted")

	RequireOK(t, reportItem(t, open.ID, spamBody(""), who.Token), "举报 open")
	RequireOK(t, reportItem(t, lost.ID, spamBody(""), who.Token), "举报 lost")
	RequireOK(t, reportItem(t, closed.ID, spamBody(""), who.Token), "举报 closed")
	RequireCode(t, reportItem(t, deleted.ID, spamBody(""), who.Token), apperr.CodeNotFound)

	// 一条不存在的 id 同样是 NOT_FOUND，两者对外不可区分
	RequireCode(t, reportItem(t, 999999, spamBody(""), who.Token), apperr.CodeNotFound)
	for _, bad := range []string{"abc", "0", "-3"} {
		RequireCode(t, harness.Post(t, "/api/items/"+bad+"/report", spamBody(""), who.Token), apperr.CodeNotFound)
	}
}

// TestM4ReportRequiresAuth 是 #41 的鉴权面：举报也要登录。
//
// 匿名的「举报」不是一种反馈，是一种投放 —— 没有任何一条路径能追到人，
// 那 admin 的待处理队列就成了垃圾场。
func TestM4ReportRequiresAuth(t *testing.T) {
	harness.TruncateAll(t)
	password := reportPassword()

	owner := harness.RegisterAndLogin(t, "m4authown", password)
	target := createItem(t, owner, foundBody("鉴权用的被举报帖", "13800009012"))

	RequireCode(t, reportItem(t, target.ID, spamBody(""), ""), apperr.CodeUnauthorized)
	RequireCode(t, reportItem(t, target.ID, spamBody(""), "not-a-real-token"), apperr.CodeUnauthorized)
	RequireCode(t, reportItem(t, target.ID, spamBody(""), forgeTokenWithAnotherSecret(t, owner.UserID)),
		apperr.CodeUnauthorized)

	banned := harness.RegisterAndLogin(t, "m4authbanned", password)
	harness.Ban(t, banned.UserID)
	RequireCode(t, reportItem(t, target.ID, spamBody(""), banned.Token), apperr.CodeUserBanned)

	// 作者举报自己的帖子也成功 —— 举报不是对抗性工具，也是一条自助通道
	RequireOK(t, reportItem(t, target.ID,
		map[string]any{"reason_code": model.ReportReasonOther, "detail": "我自己发错了，想请管理员看一眼"},
		owner.Token), "作者举报自己的帖子")

	if got := reportsCountFor(t, target.ID); got != 1 {
		t.Errorf("上面那些被拒的请求写出了 %d 行，期望只有作者那一行", got)
	}
}

// ---------- 部分唯一索引的另一半 ----------

// TestM4ReportAfterResolutionIsAllowed 验 uq_reports_open 里那句 WHERE status='open'。
//
// 去重的范围是「待处理的举报」，不是「这条帖子被举报过的历史」。
// 如果索引漏了那个 WHERE，同一个人就**永远**不能再举报同一条帖子 ——
// 一条被 admin 判了 dismissed 的帖子继续作恶，而唯一能看到它的人再也提交不了。
//
// 把那一行改成 resolved 用的是 SQL：处置端点 #49 在 M6，M4 没有合法的 HTTP 路径能改状态。
func TestM4ReportAfterResolutionIsAllowed(t *testing.T) {
	harness.TruncateAll(t)
	password := reportPassword()

	owner := harness.RegisterAndLogin(t, "m4resowner", password)
	who := harness.RegisterAndLogin(t, "m4resreader", password)
	admin := harness.MakeAdmin(t, harness.RegisterAndLogin(t, "m4resadmin", password))
	target := createItem(t, owner, foundBody("处置之后还能再举报", "13800001010"))

	first := reportItem(t, target.ID, spamBody("第一次举报"), who.Token)
	RequireOK(t, first, "第一次举报")
	var res struct {
		ID int64 `json:"id"`
	}
	first.DataInto(t, &res)

	RequireCode(t, reportItem(t, target.ID, spamBody("待处理期间重复举报"), who.Token),
		apperr.CodeReportDuplicate)

	// 模拟 M6 的处置结果：这条举报已经被处理过了。
	// 没有 updated_at = now() 这一句 —— reports 表压根没有这一列（见迁移第 12 节），
	// 一条已经处理完的举报不需要「最后修改时间」，它需要的是一般不会改的事实。
	if _, err := harness.Pool.Exec(context.Background(),
		`UPDATE reports SET status = 'resolved', resolved_by = $2, resolved_at = now()
		 WHERE id = $1`, res.ID, admin.UserID); err != nil {
		t.Fatalf("把举报 %d 标记为已处理失败: %v", res.ID, err)
	}

	r := reportItem(t, target.ID, spamBody("处理完之后发现它又犯了"), who.Token)
	RequireOK(t, r, "处置之后同一人再次举报")
	if got := reportsCountFor(t, target.ID); got != 2 {
		t.Errorf("期望 2 行（一条已处理的旧事实 + 一条新的 open），实际 %d", got)
	}

	// 新的那一行仍然是 open，旧的那一行不许被改回来（历史是历史）
	var again struct {
		ID int64 `json:"id"`
	}
	r.DataInto(t, &again)
	if reportsRow(t, again.ID)["status"] != "open" {
		t.Error("新写的举报行状态不是 open")
	}
	if reportsRow(t, res.ID)["status"] != "resolved" {
		t.Error("旧的已处理举报被新举报覆盖了状态")
	}
}

// TestM4ReportsHaveNoReadEndpoint 钉住 M4 的举报是**只进不出**的：
// 用户提交完就结束，处置住在 M6 的 admin 端（#47–#49）。
//
// 这条测试看起来多余（路由集合测试已经数过 25 条），但它守的是一件具体的事：
// 如果有人在 M5/M6 之前先加一个 GET /items/:id/reports 给前端「看看举报进度」，
// 那被举报人就能从帖子的公开页面上看到「谁举报了我」——
// 而 §3.7 说这条信息连被举报人本人都不能知道。
func TestM4ReportsHaveNoReadEndpoint(t *testing.T) {
	harness.TruncateAll(t)
	password := reportPassword()

	owner := harness.RegisterAndLogin(t, "m4noread", password)
	admin := harness.MakeAdmin(t, harness.RegisterAndLogin(t, "m4noreadadmin", password))
	who := harness.RegisterAndLogin(t, "m4noreadr", password)

	target := createItem(t, owner, foundBody("没有读接口的举报帖", "13800002020"))
	RequireOK(t, reportItem(t, target.ID, spamBody(""), who.Token), "提交举报")

	for _, probe := range []struct {
		path string
		code string
	}{
		// /api/items/:id/report 这条**路径**是存在的（#41 的 POST），只是没有读的方法，
		// gin 的 HandleMethodNotAllowed 回 405 —— 这是 m0 的 TestWrongMethodReturns405
		// 钉过的全站行为，这里要承认它而不是误判成 bug。
		{"/api/items/" + itoa(target.ID) + "/report", apperr.CodeMethodNotAllowed},
		// 另外三条压根没注册 → NOT_FOUND（不是 401，那会泄漏「它存在但你看不到」）
		{"/api/items/" + itoa(target.ID) + "/reports", apperr.CodeNotFound},
		{"/api/reports", apperr.CodeNotFound},
		{"/api/my/reports", apperr.CodeNotFound},
	} {
		for _, token := range []string{owner.Token, who.Token, admin.Token, ""} {
			RequireCode(t, harness.Get(t, probe.path, token), probe.code)
		}
	}

	// 唯一合法的那条 POST 路径不会被这些 GET 影响
	if got := reportsCountFor(t, target.ID); got != 1 {
		t.Errorf("四条只读探测写出了 %d 行举报", got)
	}
}

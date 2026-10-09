package smoketest

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
)

// 本文件是 M5 第②层的第一段：#23 提交归还确认，以及 §12 那条 M5 判据链。
//
// 链的顺序就是 §12 那一行的顺序（它同时也是 §13 第 4 步的顺序）：
//
//	从没解锁过的用户直接提交 → 成功（无前置门槛）
//	→ 缺图片 → VALIDATION
//	→ 重复提交 → RETURN_DUPLICATE
//	→ 对自己帖子提交 → RETURN_SELF
//	→ admin token 去 confirm → FORBIDDEN（全系统最重要的一条负向规则）
//	→ 发帖人 confirm → 帖子 closed、review_kind='owner'、reviewer_id=发帖人
//	→ 拾主 110、失主 102、credit_logs 两条
//	→ 提交人收到 return_confirmed，且作为 lost 作者另收 item_returned_hint
//	→ 对已 confirmed 的记录再 reject → RETURN_ILLEGAL_TRANSITION
//
// reject 那一条（§12 最后半句、§13 第 6 步）单独住在 m5_return_reject_test.go，
// 因为它的测试名 TestRejectDoesNotCloseItem 已经被 repo/item_return.go 和
// service/item_return.go 的注释引用了两次 —— 那个名字不能改，文件也不能改。
//
// 这个文件另外还定义了 M5 四个测试文件共用的夹具（setupM5 / submitReturn / 各种读库函数）：
// M5 的每一条断言都要先有一个 pending 的归还确认，而建它的那三步（注册、发帖、传图）
// 太长，不能在每个测试里各写一遍。

// ---------- 响应形状 ----------

// 同样刻意在测试包里重新声明，不 import service 的结果 struct（见 m2_items_test.go 那条理由）。

type submitReturnView struct {
	ID          int64  `json:"id"`
	ItemID      int64  `json:"item_id"`
	Status      string `json:"status"`
	SubmittedAt string `json:"submitted_at"`
}

type confirmReturnView struct {
	ID          int64  `json:"id"`
	Status      string `json:"status"`
	ReviewedAt  string `json:"reviewed_at"`
	CreditDelta int    `json:"credit_delta"`
}

type decideReturnView struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	ReviewedAt string `json:"reviewed_at"`
}

type cancelReturnView struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

type returnSubmitterView struct {
	ID          int64  `json:"id"`
	Nickname    string `json:"nickname"`
	CreditScore int    `json:"credit_score"`
}

type returnDetailView struct {
	ID            int64               `json:"id"`
	Item          itemSummary         `json:"item"`
	Submitter     returnSubmitterView `json:"submitter"`
	Message       string              `json:"message"`
	ProofImageURL string              `json:"proof_image_url"`
	Status        string              `json:"status"`
	OwnerNote     string              `json:"owner_note"`
	ReviewerID    *int64              `json:"reviewer_id"`
	ReviewKind    *string             `json:"review_kind"`
	SubmittedAt   string              `json:"submitted_at"`
	ReviewedAt    string              `json:"reviewed_at"`
}

type returnEntryView struct {
	ID          int64       `json:"id"`
	Item        itemSummary `json:"item"`
	Status      string      `json:"status"`
	OwnerNote   string      `json:"owner_note"`
	SubmittedAt string      `json:"submitted_at"`
	ReviewedAt  string      `json:"reviewed_at"`
}

type returnPageView struct {
	List     []returnEntryView `json:"list"`
	Total    int               `json:"total"`
	Page     int               `json:"page"`
	PageSize int               `json:"page_size"`
}

type creditLogView struct {
	ID        int64   `json:"id"`
	Delta     int     `json:"delta"`
	Reason    string  `json:"reason"`
	RefType   *string `json:"ref_type"`
	RefID     *int64  `json:"ref_id"`
	CreatedAt string  `json:"created_at"`
}

type creditHistoryView struct {
	CreditScore int             `json:"credit_score"`
	List        []creditLogView `json:"list"`
	Total       int             `json:"total"`
	Page        int             `json:"page"`
	PageSize    int             `json:"page_size"`
}

// ---------- 夹具 ----------

const m5Password = "correct-horse-battery"

// m5Message 是一条**保证合法**的归还说明（5–1000 字之间）。
//
// 它必须一开始就合法：归还确认的校验顺序是「字段先于查库」（service 里那六步），
// 夹具如果不合格，那么所有「帖子状态不对」的用例都会撞上 VALIDATION，
// 而那正是 §12 要区分开的两件事。
const m5Message = "周五下午在图书馆三楼自习室门口把黑色长款钱包交给您了，这是当时的照片"

// m5Stage 是 M5 的完整舞台：三个人 + 一对配过对的帖子。
//
// 为什么夹具要把 M3 那条匹配链路也走一遍，而不是只发一条 found 帖：
// confirm 有六个连带后果，其中 item_returned_hint 的收件人来自
// match_pairs 反查（LedgerLostAuthors）。台账是空的话，
// 「lost 帖作者收到提示」这一条就只能断言「没收到」——
// 那是最容易假绿的一类断言：实现整个没写，测试照样过。
// 所以夹具自带一行台账和一个 new_match，让那条提示有明确的收件人可查。
type m5Stage struct {
	finder   Session // 拾主，found 帖的发帖人 —— 全系统唯一能 confirm / reject 的人
	li       Session // 小李：lost 作者，也是提交归还确认的人
	stranger Session // 小张：没发过帖、没解锁过、没提交过的第三方

	lost  itemView
	found itemView
}

func setupM5(t *testing.T) m5Stage {
	t.Helper()
	harness.TruncateAll(t)

	st := m5Stage{
		finder:   harness.RegisterAndLogin(t, "m5finder", m5Password),
		li:       harness.RegisterAndLogin(t, "m5li", m5Password),
		stranger: harness.RegisterAndLogin(t, "m5stranger", m5Password),
	}

	// 顺序是 M3 定下来的那一条：lost 先、found 后。反过来建就没有台账也没有通知。
	st.lost = createItem(t, st.li,
		lostWalletBody(walletTitleLost, walletDesc, locLibrary, detailLibrary))
	resp := harness.Post(t, "/api/items",
		foundWalletBody(walletTitleFound, walletDesc, locLibrary, detailLibrary), st.finder.Token)
	RequireOK(t, resp, "拾主发拾物帖")
	st.found = createResultOf(t, resp)

	// 夹具自证：下面那些「通知 +N」「流水恰好 2 行」的断言全部依赖这三行基线。
	// 基线塌了却不自查的话，几十条测试会一起变红，而红的原因看起来像 M5 写错了。
	if got := harness.Count(t,
		`SELECT count(*) FROM match_pairs WHERE found_item_id = $1`, st.found.ID); got != 1 {
		t.Fatalf("夹具的台账期望 1 行，实际 %d —— item_returned_hint 反查不到人，M5 的链就断了", got)
	}
	if got := notificationsFor(t, st.li.UserID); got != 1 {
		t.Fatalf("夹具期望小李有 1 条 new_match，实际 %d", got)
	}
	if got := notificationsFor(t, st.finder.UserID); got != 0 {
		t.Fatalf("夹具期望拾主的收件箱是空的（found 作者不该被匹配通知），实际 %d", got)
	}
	return st
}

// m5Proof 走真实的 #6 上传拿回一个 path。
//
// 不写死一个形状正确的常量是因为：#23 对 proof_image_path 用的白名单和 #13
// 校验图片路径用的是同一个 IsUploadPath，而 #6 是唯一的生产方。
// 上传接口哪天改了命名规则，写死的夹具会跟着过期，测试就会在一个与归还无关的地方变红。
func m5Proof(t *testing.T, s Session) string {
	t.Helper()
	return upload(t, s, 0).Path
}

func m5Body(t *testing.T, s Session, message string) map[string]any {
	t.Helper()
	return map[string]any{"message": message, "proof_image_path": m5Proof(t, s)}
}

func submitReturn(t *testing.T, itemID int64, body any, token string) Response {
	t.Helper()
	return harness.Post(t, "/api/items/"+itoa(itemID)+"/returns", body, token)
}

// requireSubmitted 提交一条归还确认并返回 #23 的 data。失败直接 fatal —— 它是夹具不是被测对象。
func requireSubmitted(t *testing.T, itemID int64, s Session) submitReturnView {
	t.Helper()
	r := submitReturn(t, itemID, m5Body(t, s, m5Message), s.Token)
	RequireOK(t, r, "小李提交归还确认")
	var v submitReturnView
	r.DataInto(t, &v)
	if v.ID == 0 {
		t.Fatalf("#23 成功返回了但 id 是 0：%s", truncate(string(r.Data)))
	}
	return v
}

// returnRow 读回 item_returns 那一行。很多断言必须落到库上：
// 响应里的 status 是 service 自己拼的字符串，而那一列才是事实。
//
// 两个时间列在这里 ::text：pgx 默认把 timestamptz 解成 time.Time，而 QueryRow
// 返回的是 map[string]any —— 断言写成 row["reviewed_at"].(string) 会 panic，
// 而 panic 的报错完全看不出「只是类型选错了」（同 onlyLedgerRow 那条教训）。
// NULL 经过 ::text 仍然是 NULL，所以「没审过就是 nil」这个断言不受影响。
func returnRow(t *testing.T, id int64) map[string]any {
	t.Helper()
	return harness.QueryRow(t, `SELECT id, item_id, submitter_id, message, proof_image_path, status,
	                              owner_note, reviewer_id, review_kind,
	                              submitted_at::text AS submitted_at, reviewed_at::text AS reviewed_at
	                       FROM item_returns WHERE id = $1`, id)
}

// unreviewedOutsideService 是 §13 第 10 步那条自检 SQL。
//
// 任何一行命中都说明这条归还确认是在 service 层之外被推进的
// （reviewer_id / review_kind 只有 service 的那两条路径会写）。
// M5 把它做成一个函数而不是「写在某个测试里的一次 SQL」，
// 因为四个测试文件都该在收尾时跑它一次。
func unreviewedOutsideService(t *testing.T) []int64 {
	t.Helper()
	rows, err := harness.Pool.Query(context.Background(),
		`SELECT id FROM item_returns
		 WHERE status IN ('confirmed','rejected') AND review_kind IS NULL`)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("扫描失败: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	return ids
}

// notifRow 是 notifications 的一行（带类型、收件人和那两个跳转用的外键）。
type notifRow struct {
	ID       int64
	UserID   int64
	Type     string
	Title    string
	Content  string
	ItemID   *int64
	ReturnID *int64
}

func notificationsOf(t *testing.T, userID int64) []notifRow {
	t.Helper()
	rows, err := harness.Pool.Query(context.Background(),
		`SELECT id, user_id, type, title, content, item_id, return_id
		 FROM notifications WHERE user_id = $1 ORDER BY id`, userID)
	if err != nil {
		t.Fatalf("查询通知失败: %v", err)
	}
	defer rows.Close()

	var out []notifRow
	for rows.Next() {
		var n notifRow
		if err := rows.Scan(&n.ID, &n.UserID, &n.Type, &n.Title, &n.Content, &n.ItemID, &n.ReturnID); err != nil {
			t.Fatalf("扫描通知失败: %v", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读取通知失败: %v", err)
	}
	return out
}

// typesOf 把一串通知压成 "type" 列表，方便整条链一次比完。
func typesOf(ns []notifRow) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Type)
	}
	return out
}

func findByType(t *testing.T, ns []notifRow, typ string) notifRow {
	t.Helper()
	var found []notifRow
	for _, n := range ns {
		if n.Type == typ {
			found = append(found, n)
		}
	}
	if len(found) != 1 {
		t.Fatalf("期望恰好 1 条 %s，实际 %d 条（这一页是 %v）", typ, len(found), typesOf(ns))
	}
	return found[0]
}

func creditLogsOf(t *testing.T, userID int64) []creditLogView {
	t.Helper()
	r := harness.Get(t, "/api/my/credit-logs?page_size=100", tokenOf(t, userID))
	RequireOK(t, r, "GET /api/my/credit-logs")
	var page creditHistoryView
	r.DataInto(t, &page)
	return page.List
}

// tokenOf 给一个已经在库里的 user id 铸 token。
//
// 为什么不复用 Session.Token：M5 的很多断言要看「别人」的账本，
// 而那些别人是夹具里顺手注册的（比如 admin），把它们都塞进 m5Stage 会让夹具
// 变成谁都要改的地方。铸一个比塞进去便宜。
func tokenOf(t *testing.T, userID int64) string {
	t.Helper()
	return harness.TokenFor(t, userID)
}

// ---------- 判据链 ----------

// TestM5CriterionChain 是 §12 里 M5 那一整行判据的可执行形式（除 reject 分支）。
func TestM5CriterionChain(t *testing.T) {
	st := setupM5(t)

	// ① 从没解锁过联系方式的人直接提交 → 成功。
	//    §12 把这条放在最前面是有原因的：这是最容易「顺手加一道门槛」的地方，
	//    而 §16 已经明确把 RETURN_NOT_UNLOCKED 整个删掉了。
	if got := contactViewRowCount(t, st.found.ID); got != 0 {
		t.Fatalf("夹具里小李不该解锁过任何帖子，contact_views 却有 %d 行 —— 这一条判据的前提没了", got)
	}
	submitted := requireSubmitted(t, st.found.ID, st.li)
	if submitted.Status != model.ReturnStatusPending {
		t.Errorf("#23 返回的 status 应该是 %q，实际 %q", model.ReturnStatusPending, submitted.Status)
	}
	if submitted.ItemID != st.found.ID {
		t.Errorf("#23 返回的 item_id=%d，和请求的帖子 %d 不一致", submitted.ItemID, st.found.ID)
	}
	if submitted.SubmittedAt == "" {
		t.Errorf("#23 少了 submitted_at，前端没法在列表里显示时间")
	}
	row := returnRow(t, submitted.ID)
	if row["status"] != "pending" {
		t.Errorf("库里那一行是 %v，期望 pending", row["status"])
	}
	if row["reviewer_id"] != nil || row["review_kind"] != nil || row["reviewed_at"] != nil {
		t.Errorf("一条刚提交的记录不该有任何审核痕迹：%#v", row)
	}

	// ② 缺凭证图 → VALIDATION，而且一行都不写
	before := harness.Count(t, `SELECT count(*) FROM item_returns`)
	r := submitReturn(t, st.found.ID, map[string]any{"message": m5Message}, st.stranger.Token)
	RequireCode(t, r, apperr.CodeValidation)
	requireField(t, r, "proof_image_path")
	if got := harness.Count(t, `SELECT count(*) FROM item_returns`); got != before {
		t.Errorf("被拒的提交写了 %d 行，之前 %d 行", got, before)
	}

	// ③ 重复提交 → RETURN_DUPLICATE（部分唯一索引只管 pending 那一个状态）
	dup := submitReturn(t, st.found.ID, m5Body(t, st.stranger, m5Message), st.li.Token)
	RequireCode(t, dup, apperr.CodeReturnDuplicate)

	// ④ 对自己的帖子提交 → RETURN_SELF
	self := submitReturn(t, st.found.ID, m5Body(t, st.finder, m5Message), st.finder.Token)
	RequireCode(t, self, apperr.CodeReturnSelf)

	// ⑤ 发帖人收到唯一那条 return_submitted
	ownerInbox := notificationsOf(t, st.finder.UserID)
	if len(ownerInbox) != 1 {
		t.Fatalf("拾主期望 1 条通知，实际 %v", typesOf(ownerInbox))
	}
	first := findByType(t, ownerInbox, model.NotificationReturnSubmitted)
	if first.ReturnID == nil || *first.ReturnID != submitted.ID {
		t.Errorf("return_submitted 的 return_id=%v，应指向那条 pending 记录 %d", first.ReturnID, submitted.ID)
	}
	if first.ItemID == nil || *first.ItemID != st.found.ID {
		t.Errorf("return_submitted 的 item_id=%v，应指向那条拾物帖 %d", first.ItemID, st.found.ID)
	}
	if !strings.Contains(first.Content, st.li.Username) {
		t.Errorf("通知里没有提交人昵称，发帖人无法据此判断：%q", first.Content)
	}

	// ⑥ admin 去 confirm → FORBIDDEN。全系统最要紧的一条负向规则：
	//    管理员能销毁内容和账号，但不能制造归属（定位原则 5）。
	admin := harness.MakeAdmin(t, st.stranger)
	// 顺手把 pending 那条给小张也留一条，让 admin 拿它去试（路径里的 id 必须是真实存在的）
	asStranger := requireSubmitted(t, st.found.ID, st.stranger)
	blocked := harness.Post(t, "/api/returns/"+itoa(asStranger.ID)+"/confirm", map[string]any{}, admin.Token)
	RequireCode(t, blocked, apperr.CodeForbidden)
	if row := returnRow(t, asStranger.ID); row["status"] != "pending" {
		t.Errorf("被拒的 admin confirm 把记录推进到了 %v", row["status"])
	}
	if got := itemStatusOf(t, st.found.ID); got != model.ItemStatusOpen {
		t.Errorf("被拒的 admin confirm 之后帖子变成 %q 了", got)
	}

	// ⑦ 发帖人自己 confirm → 六个连带后果
	note := "对上了，谢谢您"
	cr := harness.Post(t, "/api/returns/"+itoa(submitted.ID)+"/confirm",
		map[string]any{"owner_note": note}, st.finder.Token)
	RequireOK(t, cr, "发帖人确认归还")
	var confirmed confirmReturnView
	cr.DataInto(t, &confirmed)
	if confirmed.Status != model.ReturnStatusConfirmed {
		t.Errorf("期望 status=confirmed，实际 %q", confirmed.Status)
	}
	if confirmed.CreditDelta != model.CreditDeltaReturnOwner {
		t.Errorf("拾主的 credit_delta 期望 %d，实际 %d",
			model.CreditDeltaReturnOwner, confirmed.CreditDelta)
	}
	if confirmed.ReviewedAt == "" {
		t.Errorf("缺少 reviewed_at")
	}

	// ⑧ 帖子 closed + 归属两列（§12 原话：review_kind='owner' 且 reviewer_id = 发帖人 id）
	if got := itemStatusOf(t, st.found.ID); got != model.ItemStatusClosed {
		t.Errorf("confirm 之后 items.status 期望 %q，实际 %q", model.ItemStatusClosed, got)
	}
	row = returnRow(t, submitted.ID)
	if row["status"] != "confirmed" {
		t.Errorf("item_returns.status 期望 confirmed，实际 %v", row["status"])
	}
	if got := asInt64(t, row["reviewer_id"]); got != st.finder.UserID {
		t.Errorf("reviewer_id 期望 %d（发帖人），实际 %d", st.finder.UserID, got)
	}
	if row["review_kind"] != model.ReviewKindOwner {
		t.Errorf("review_kind 期望 %q，实际 %#v", model.ReviewKindOwner, row["review_kind"])
	}
	if s, ok := row["owner_note"].(string); !ok || s != note {
		t.Errorf("owner_note 没存下来：%#v", row["owner_note"])
	}

	// ⑨ 分数与流水
	if got := creditOf(t, st.finder.UserID); got != 100+model.CreditDeltaReturnOwner {
		t.Errorf("拾主 credit_score 期望 %d，实际 %d", 100+model.CreditDeltaReturnOwner, got)
	}
	if got := creditOf(t, st.li.UserID); got != 100+model.CreditDeltaReturnSubmitter {
		t.Errorf("提交人 credit_score 期望 %d，实际 %d", 100+model.CreditDeltaReturnSubmitter, got)
	}
	if got := harness.Count(t, `SELECT count(*) FROM credit_logs`); got != 2 {
		t.Errorf("credit_logs 期望 2 行（拾主 +10、提交人 +2），实际 %d", got)
	}

	// ⑩ 提交人自己应当是双份：return_confirmed + item_returned_hint（§13 第 4 步最后半句）
	liInbox := notificationsOf(t, st.li.UserID)
	if len(liInbox) != 3 {
		t.Fatalf("小李期望 3 条（new_match + return_confirmed + item_returned_hint），实际 %v", typesOf(liInbox))
	}
	hint := findByType(t, liInbox, model.NotificationItemReturnedHint)
	if hint.ItemID == nil || *hint.ItemID != st.found.ID {
		t.Errorf("item_returned_hint 的 item_id=%v，应指向那条拾物帖 %d", hint.ItemID, st.found.ID)
	}
	done := findByType(t, liInbox, model.NotificationReturnConfirmed)
	if done.ReturnID == nil || *done.ReturnID != submitted.ID {
		t.Errorf("return_confirmed 的 return_id=%v，应指向那条记录 %d", done.ReturnID, submitted.ID)
	}

	// ⑪ 终态没有出边：对已经 confirmed 的记录再 reject → 409
	//    这一条和「再 confirm」是同一件事的两面，但只测 reject 那一面就够了：
	//    两个动作共用 checkTransition 那一张表（service 里只有三个键，pending 是唯一有出边的）。
	late := harness.Post(t, "/api/returns/"+itoa(submitted.ID)+"/reject",
		map[string]any{"owner_note": "想反悔"}, st.finder.Token)
	RequireCode(t, late, apperr.CodeReturnIllegalTransition)
	if row := returnRow(t, submitted.ID); row["status"] != "confirmed" {
		t.Errorf("非法转换把记录改成了 %v，它必须停在 confirmed", row["status"])
	}
	if got := creditOf(t, st.finder.UserID); got != 100+model.CreditDeltaReturnOwner {
		t.Errorf("非法的 reject 动了分数（%d）—— 它应该在写任何东西之前就被挡住", got)
	}

	if ids := unreviewedOutsideService(t); len(ids) != 0 {
		t.Errorf("这几条归还确认是在 service 之外被推进的：%v", ids)
	}
}

// ---------- 无前置门槛（§12 的第一条，单独钉一遍） ----------

// TestM5SubmitHasNoPrerequisite 把「提交归还确认不需要先解锁联系方式」
// 做成一条独立命名的测试，而不是只当链上的第①步。
//
// 理由写在 service/item_return.go 顶部第 1 条：这是一条**减法**规则，
// 而减法规则最容易被后来人以「防止乱提交」的名义加回去。
// 链上的那一步失败时报的是「夹具里 contact_views 不为 0」，
// 看不出真正被破坏的是哪一条规则；这一条直接报「解锁和提交之间没有任何依赖」。
func TestM5SubmitHasNoPrerequisite(t *testing.T) {
	st := setupM5(t)

	// 小李连那条拾物帖的联系方式都没看过（他没走 #21），就直接提交。
	if got := contactViewRowCount(t, st.found.ID); got != 0 {
		t.Fatalf("前提不成立：contact_views 已经有 %d 行", got)
	}
	submitted := requireSubmitted(t, st.found.ID, st.li)

	// 反面同样要钉住：**提交也不会顺手写 contact_views**。
	// 如果哪天有人把「提交归还确认」实现成「先解锁再提交」，
	// 这一行会变成 1，而那条审计名单（§3.2 的唯一一处真实姓名出口）就被污染了。
	if got := contactViewRowCount(t, st.found.ID); got != 0 {
		t.Errorf("提交归还确认写了 %d 行 contact_views —— 它不是解锁的替代品，两件事互不相关", got)
	}

	// 和这条帖子匹配过与否也不影响：换一个从没配过对的提交人照样能提。
	other := harness.RegisterAndLogin(t, "m5never", m5Password)
	r := submitReturn(t, st.found.ID, m5Body(t, other, m5Message), other.Token)
	RequireOK(t, r, "一个从没和这条帖子匹配过的用户提交归还确认")
	if harness.Count(t, `SELECT count(*) FROM item_returns WHERE submitter_id = $1`, other.UserID) != 1 {
		t.Errorf("提交人 %d 的记录没落库（已经提过一条：%d）", other.UserID, submitted.ID)
	}
}

// ---------- #23 的请求体校验 ----------

// TestM5SubmitFieldValidation 表驱动钉住两个字段的规则。
//
// 每个用例都额外断言「item_returns 一行都没写」：#23 的校验顺序是字段排在查库之前，
// 所以这些请求连 SELECT 都不该发出。少断这一句的话，
// 「先插了行再回头报错」这种实现（它会留下一个永远没人审的 pending）也能让 code 断言通过。
func TestM5SubmitFieldValidation(t *testing.T) {
	st := setupM5(t)
	const goodProof = "2026/10/0123456789abcdef0123456789abcdef.png"

	cases := []struct {
		name  string
		body  map[string]any
		field string
	}{
		{"没有 message 这个键", map[string]any{"proof_image_path": goodProof}, "message"},
		{"message 为空串", map[string]any{"message": "", "proof_image_path": goodProof}, "message"},
		{"message 纯空白", map[string]any{"message": "   \t ", "proof_image_path": goodProof}, "message"},
		{"message 只有 4 个字", map[string]any{"message": "交给您了", "proof_image_path": goodProof}, "message"},
		{"message 1001 个字", map[string]any{"message": strings.Repeat("钱", 1001), "proof_image_path": goodProof}, "message"},
		{"没有 proof_image_path 这个键", map[string]any{"message": m5Message}, "proof_image_path"},
		{"proof 为空串", map[string]any{"message": m5Message, "proof_image_path": ""}, "proof_image_path"},
		{"proof 纯空白", map[string]any{"message": m5Message, "proof_image_path": "  "}, "proof_image_path"},
		// 下面几条全是「形状像但不来自 #6」的输入。它们的后果不是报错难看，
		// 而是 #24 会把这个值拼成 proof_image_url 返回给发帖人 —— 不校验就等于
		// 允许提交人往别人的审核页面上塞任意 URL（钓鱼链接、绝对磁盘路径、../ 遍历）。
		{"proof 是完整 URL", map[string]any{"message": m5Message, "proof_image_path": "https://evil.example/" + goodProof}, "proof_image_path"},
		{"proof 是站内绝对路径", map[string]any{"message": m5Message, "proof_image_path": "/uploads/" + goodProof}, "proof_image_path"},
		{"proof 带目录遍历", map[string]any{"message": m5Message, "proof_image_path": "2026/10/../" + goodProof}, "proof_image_path"},
		{"proof 是磁盘路径", map[string]any{"message": m5Message, "proof_image_path": `C:\Windows\temp\a.jpg`}, "proof_image_path"},
		{"proof 扩展名不是图片", map[string]any{"message": m5Message, "proof_image_path": "2026/10/0123456789abcdef0123456789abcdef.txt"}, "proof_image_path"},
		{"proof 文件名不是 32 位十六进制", map[string]any{"message": m5Message, "proof_image_path": "2026/10/not-a-32-hex-string.jpg"}, "proof_image_path"},
		{"proof 日期段不对", map[string]any{"message": m5Message, "proof_image_path": "26/10/0123456789abcdef0123456789abcdef.jpg"}, "proof_image_path"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := harness.Count(t, `SELECT count(*) FROM item_returns`)
			r := submitReturn(t, st.found.ID, c.body, st.li.Token)
			RequireCode(t, r, apperr.CodeValidation)
			requireField(t, r, c.field)
			if got := harness.Count(t, `SELECT count(*) FROM item_returns`); got != before {
				t.Errorf("字段不合格却被写进库了：%d 行 → %d 行", before, got)
			}
			if got := harness.Count(t, `SELECT count(*) FROM notifications`); got != 1 {
				t.Errorf("字段不合格的提交发了通知（基线 1 条 new_match，实际总共 %d 条）", got)
			}
		})
	}

	// 边界值本身要放行：1000 个汉字的说明必须能提交成功。
	// CHECK 是 char_length BETWEEN 5 AND 1000，PostgreSQL 数的是字符；
	// service 侧如果用 len() 就会在 334 个汉字上报 VALIDATION —— 而这一条
	// 在第①层用 fake 测过，这里从 HTTP 再证一次，因为 CHECK 只有真库能验。
	big := submitReturn(t, st.found.ID,
		map[string]any{"message": strings.Repeat("归", model.ReturnMessageMaxChars), "proof_image_path": m5Proof(t, st.li)},
		st.li.Token)
	RequireOK(t, big, "1000 个汉字的归还说明")
}

// TestM5SubmitTargetMatrix 覆盖「字段全对，但对象不对」的六种帖子。
//
// 顺序是 service 里那六步的顺序，两个「两个条件同时不满足」的用例是这里的重点：
// lost + 自己的帖子 → VALIDATION（不是 RETURN_SELF），
// 自己的 + closed → RETURN_SELF（不是 ITEM_CLOSED）。
// 顺序反了的症状很具体：小王对着自己的失物帖点归还确认，
// 会看到「不能对自己的帖子提交归还确认」，然后困惑地去发一条拾物帖试试。
func TestM5SubmitTargetMatrix(t *testing.T) {
	st := setupM5(t)

	// 每次都要一张新的凭证图？不需要 —— 凭证图只在「落库那一条」用一次，
	// 这些用例全部在落库之前就被拦下。

	t.Run("别人的 lost 帖 → VALIDATION", func(t *testing.T) {
		r := submitReturn(t, st.lost.ID, m5Body(t, st.stranger, m5Message), st.stranger.Token)
		RequireCode(t, r, apperr.CodeValidation)
		requireField(t, r, "id")
	})

	t.Run("自己的 lost 帖 → VALIDATION 而不是 RETURN_SELF", func(t *testing.T) {
		r := submitReturn(t, st.lost.ID, m5Body(t, st.li, m5Message), st.li.Token)
		RequireCode(t, r, apperr.CodeValidation)
	})

	t.Run("别人的 closed 帖 → ITEM_CLOSED", func(t *testing.T) {
		post := createItem(t, st.finder, foundBody("在二楼捡到一串钥匙", "13800000001"))
		harness.SetItemStatus(t, post.ID, model.ItemStatusClosed)
		r := submitReturn(t, post.ID, m5Body(t, st.stranger, m5Message), st.stranger.Token)
		RequireCode(t, r, apperr.CodeItemClosed)
	})

	t.Run("自己的 closed 帖 → RETURN_SELF 而不是 ITEM_CLOSED", func(t *testing.T) {
		post := createItem(t, st.finder, foundBody("在二楼捡到一张校园卡", "13800000002"))
		harness.SetItemStatus(t, post.ID, model.ItemStatusClosed)
		r := submitReturn(t, post.ID, m5Body(t, st.finder, m5Message), st.finder.Token)
		RequireCode(t, r, apperr.CodeReturnSelf)
	})

	t.Run("deleted 帖 → NOT_FOUND", func(t *testing.T) {
		post := createItem(t, st.finder, foundBody("在二楼捡到一只水杯", "13800000003"))
		harness.SetItemStatus(t, post.ID, model.ItemStatusDeleted)
		r := submitReturn(t, post.ID, m5Body(t, st.stranger, m5Message), st.stranger.Token)
		RequireCode(t, r, apperr.CodeNotFound)
	})

	t.Run("不存在的帖子 → NOT_FOUND", func(t *testing.T) {
		r := submitReturn(t, 999_999, m5Body(t, st.stranger, m5Message), st.stranger.Token)
		RequireCode(t, r, apperr.CodeNotFound)
	})

	t.Run("路径里的 id 不是数字 → NOT_FOUND", func(t *testing.T) {
		// 这里是 NOT_FOUND 而不是 VALIDATION，而且不是偶然：handler/params.go 的 pathID
		// 把这条约定一次性定给全项目所有以 :id 为路径的端点（§4 给 #15–#18/#42
		// 列的专属码里也没有 VALIDATION）。「/api/items/abc」指的是一条不存在的帖子，
		// 而 VALIDATION 暗示「换个格式就能查」—— 没有任何格式能让 abc 变成一条帖子。
		for _, raw := range []string{"abc", "0", "-1", "99999999999999999999"} {
			r := harness.Post(t, "/api/items/"+raw+"/returns",
				m5Body(t, st.stranger, m5Message), st.stranger.Token)
			RequireCode(t, r, apperr.CodeNotFound)
		}
	})

	// 上面这些负向用例不能留下任何痕迹。
	if got := harness.Count(t, `SELECT count(*) FROM item_returns`); got != 0 {
		t.Errorf("整组负向用例写了 %d 行 item_returns", got)
	}
	if ids := unreviewedOutsideService(t); len(ids) != 0 {
		t.Errorf("出现了 service 之外推进的归还确认：%v", ids)
	}
}

// ---------- 认领不是排他锁 ----------

// TestM5SubmitsAreNonExclusive 是定位原则 3 在归还确认这一侧的形式：
// 「同一条帖子可以同时有好几条待处理的归还确认，每个人一条」。
//
// 这条测试存在的理由和 M4 那条「第一个人解锁之后别人照样能解锁」一样：
// item_returns 上没有任何唯一约束阻止第二条 pending 存在
// （那条索引是 (item_id, submitter_id) WHERE status='pending'，带了 submitter_id），
// 而实现很容易被改成「一条帖子只能有一条待处理确认」—— 那等于把平台
// 变成了「第一个声称的人占住这条帖子」，也就是排他锁。
func TestM5SubmitsAreNonExclusive(t *testing.T) {
	st := setupM5(t)

	a := requireSubmitted(t, st.found.ID, st.li)
	b := requireSubmitted(t, st.found.ID, st.stranger)
	if a.ID == b.ID {
		t.Fatalf("两个人提交拿到了同一个 id")
	}
	if got := harness.Count(t,
		`SELECT count(*) FROM item_returns WHERE item_id = $1 AND status = 'pending'`, st.found.ID); got != 2 {
		t.Errorf("同一条帖子上的两条 pending 期望 2 行，实际 %d", got)
	}

	// 第三个人也能提（没有名额上限）
	c := requireSubmitted(t, st.found.ID, harness.RegisterAndLogin(t, "m5third", m5Password))
	if got := harness.Count(t, `SELECT count(*) FROM item_returns`); got != 3 {
		t.Errorf("期望 3 条独立的归还确认，实际 %d（第三条 id=%d）", got, c.ID)
	}

	// 但同一个人不能刷第二条 —— 那是那条部分唯一索引唯一的用途
	dup := submitReturn(t, st.found.ID, m5Body(t, st.li, m5Message), st.li.Token)
	RequireCode(t, dup, apperr.CodeReturnDuplicate)
	if got := harness.Count(t,
		`SELECT count(*) FROM item_returns WHERE item_id = $1 AND submitter_id = $2`,
		st.found.ID, st.li.UserID); got != 1 {
		t.Errorf("重复提交之后小李那条变成 %d 行了", got)
	}
}

// TestM5DuplicateTouchesNothing 把 RETURN_DUPLICATE 的后果钉成「零」：
// 不加行、不加通知、也不改动那条原本就在的 pending。
//
// 断言「原记录没被动过」不是凑数：撞唯一索引的实现有一种很自然的错误写法 ——
// 把 INSERT ... ON CONFLICT DO UPDATE 写成更新 message。
// 那种写法下提交人可以反复改写自己那条已被人盯上的说明，而列表里那条的时间戳也会跟着变。
func TestM5DuplicateTouchesNothing(t *testing.T) {
	st := setupM5(t)
	first := requireSubmitted(t, st.found.ID, st.li)

	ownerBefore := harness.Count(t, `SELECT count(*) FROM notifications WHERE user_id = $1`, st.finder.UserID)
	oldRow := returnRow(t, first.ID)

	retry := submitReturn(t, st.found.ID,
		map[string]any{"message": "我换一个说法再试一次，这条长一些", "proof_image_path": m5Proof(t, st.li)},
		st.li.Token)
	RequireCode(t, retry, apperr.CodeReturnDuplicate)

	if got := harness.Count(t, `SELECT count(*) FROM item_returns`); got != 1 {
		t.Errorf("重复提交之后库里期望 1 行，实际 %d", got)
	}
	if got := harness.Count(t, `SELECT count(*) FROM notifications WHERE user_id = $1`, st.finder.UserID); got != ownerBefore {
		t.Errorf("重复提交给发帖人多发了 %d 条通知", got-ownerBefore)
	}
	now := returnRow(t, first.ID)
	for _, col := range []string{"message", "submitted_at", "status", "proof_image_path"} {
		if fmt.Sprint(now[col]) != fmt.Sprint(oldRow[col]) {
			t.Errorf("撞唯一索引把 %s 从 %v 改成了 %v —— 应该一行都不动", col, oldRow[col], now[col])
		}
	}
}

// TestM5SubmitNotifiesOnlyTheOwner 单独锁住「提交这一步的通知收件人恰好一个」。
//
// M5 有四种通知，其中 return_submitted 是唯一一条由**提交人**触发、
// 却**不给提交人**发的。这个不对称很容易被写成「两边都发」，
// 而两边都发的后果是提交人收到一条自己写的話（§3.7 那三条禁令之一的同类错误）。
func TestM5SubmitNotifiesOnlyTheOwner(t *testing.T) {
	st := setupM5(t)
	submitted := requireSubmitted(t, st.found.ID, st.li)

	owner := notificationsOf(t, st.finder.UserID)
	if len(owner) != 1 || owner[0].Type != model.NotificationReturnSubmitted {
		t.Errorf("拾主的通知期望恰好 1 条 return_submitted，实际 %v", typesOf(owner))
	}
	if owner[0].Title != "有人提交了归还确认" {
		t.Errorf("标题不对： %q", owner[0].Title)
	}
	if !strings.Contains(owner[0].Content, walletTitleFound) {
		t.Errorf("通知里没有帖子标题，发帖人分不清是哪一条帖子：%q", owner[0].Content)
	}

	// 提交人自己：基线那条 new_match 之后不该再多。
	li := notificationsOf(t, st.li.UserID)
	if len(li) != 1 || li[0].Type != model.NotificationNewMatch {
		t.Errorf("提交人不该因为自己的提交收到通知，实际 %v", typesOf(li))
	}
	// 第三方：一个人都不该被牵连。
	if got := notificationsFor(t, st.stranger.UserID); got != 0 {
		t.Errorf("路人在这次提交里收到了 %d 条通知", got)
	}

	// 通知必须能跳到那条记录：return_id 是 #24 的唯一入口。
	if owner[0].ReturnID == nil || *owner[0].ReturnID != submitted.ID {
		t.Errorf("return_id=%v，前端无法打开那条归还确认", owner[0].ReturnID)
	}
}

// TestM5SubmitRequiresAuth 覆盖这一组全部八个端点的「没带 token」分支。
//
// 放在这个文件是因为 #23 是这一组的入口：一条测试跑遍八个路径，
// 比八个各写一小段更容易看出「少了一个」。
// 少一个的后果不是报错难看，而是「未登录也能撤销别人的归还确认」——
// 前提得是 handler 真的忘了挂中间件，而那正是路由表测试看不出来的一类 bug。
//
// 最后一条断言（签名正确但用户不存在）是 M1 那个「role/status 不进 JWT」
// 设计的直接后果：token 里只有一个 user id，而那个 id 随时可能不再存在。
func TestM5SubmitRequiresAuth(t *testing.T) {
	st := setupM5(t)
	submitted := requireSubmitted(t, st.found.ID, st.li)

	cases := []struct{ method, path string }{
		{http.MethodPost, "/api/items/" + itoa(st.found.ID) + "/returns"},
		{http.MethodGet, "/api/returns/" + itoa(submitted.ID)},
		{http.MethodPost, "/api/returns/" + itoa(submitted.ID) + "/confirm"},
		{http.MethodPost, "/api/returns/" + itoa(submitted.ID) + "/reject"},
		{http.MethodPost, "/api/returns/" + itoa(submitted.ID) + "/cancel"},
		{http.MethodGet, "/api/my/returns/submitted"},
		{http.MethodGet, "/api/my/returns/received"},
		{http.MethodGet, "/api/my/credit-logs"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			r := harness.Do(t, c.method, c.path, map[string]any{}, "")
			RequireCode(t, r, apperr.CodeUnauthorized)
			if r.HTTPStatus != http.StatusUnauthorized {
				t.Errorf("HTTP 状态期望 401，实际 %d", r.HTTPStatus)
			}
		})
	}

	// 带一个签名正确但用户已不存在的 token（撤销账号那种场景的近似）也得是 401，
	// 而不是 500：中间件在 users 表查不到人时必须走 apperr.Unauthorized。
	ghost := harness.TokenFor(t, 987_654)
	r := harness.Do(t, http.MethodGet, "/api/my/credit-logs", nil, ghost)
	RequireCode(t, r, apperr.CodeUnauthorized)
}

// TestM5SubmitKeysMatchPlan 断言 #23 的 data 恰好是那四个键。
//
// 多一个键（比如顺手把 submitter 塞回去）在这里就会被点名。
func TestM5SubmitKeysMatchPlan(t *testing.T) {
	st := setupM5(t)
	r := submitReturn(t, st.found.ID, m5Body(t, st.li, m5Message), st.li.Token)
	RequireOK(t, r, "提交归还确认")

	got := dataKeys(t, r)
	assertSameSet(t, "#23 data", []string{"id", "item_id", "status", "submitted_at"}, got)

	// submitted_at 必须是 RFC3339 的 UTC 绝对时间：列表和详情都靠它排序，
	// 而相对时间（"3 分钟前"）是前端的展示逻辑，后端一旦开始算就有两份定义。
	var v submitReturnView
	decodeInto(t, got["submitted_at"], &v.SubmittedAt)
	parsed, err := time.Parse(time.RFC3339, v.SubmittedAt)
	if err != nil {
		t.Errorf("submitted_at=%q 不是 RFC3339 时间：%v", v.SubmittedAt, err)
	}
	if !strings.HasSuffix(v.SubmittedAt, "Z") {
		t.Errorf("submitted_at=%q 不是 UTC（model 那边统一 FormatUTC，带本地偏移就是漏了一处）", v.SubmittedAt)
	}
	if time.Since(parsed) > time.Minute {
		t.Errorf("submitted_at=%q 距现在 %v —— 时间戳不是这一秒写出来的", v.SubmittedAt, time.Since(parsed))
	}
}

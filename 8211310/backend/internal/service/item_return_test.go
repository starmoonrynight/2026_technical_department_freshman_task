package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
	"lostfound/internal/repo"
)

// 这个文件是 M5 的第①层（§10：不起数据库）。
//
// M5 有四条**否定式**规则，它们是这整个里程碑最容易在重构中被弄丢的东西：
//
//	① 提交归还确认没有前置门槛（不需要先解锁联系方式）
//	② admin 不能 confirm / reject（平台不能替人裁决归属）
//	③ reject 不产生任何后果（不关帖、不扣分）
//	④ cancel 零副作用（连通知都不发）
//
// 这四条在集成测试里只表现为「最终状态对」，但状态对有时是巧合。
// 只有在这一层能断言「那个方法一次都没被调用」和「那个依赖在类型上根本不存在」——
// 而这两件事才是这些规则唯一的硬保证。
//
// 所以本文件的重心不是「功能能用」，而是四件事：
//   - 状态机 4×3 全矩阵（§12 判据明确要求表驱动覆盖 from×action）
//   - 判断**顺序**（权限在状态之前、状态在字段之前 —— 三处都对调不得）
//   - 依赖形状（反射查方法名，把「那行代码写不出来」写成一条会红的测试）
//   - 通知文案的字符预算（撞 VARCHAR(100)/VARCHAR(500) 会让整条 confirm 事务回滚成 500）

// ---------- 夹具 ----------

// 四个 id 覆盖 M5 全部身份组合。⚠ adminID 在这里只是一个「和谁都不相等的 id」：
// authorizeReturnDecision 从不读 role，所以 admin 被挡住这件事在这张表里
// 和 stranger 被挡住是同一行代码、同一个原因。
const (
	retOwnerID     int64 = 2 // 拾物帖的发帖人：唯一能 confirm / reject 的人
	retSubmitterID int64 = 3 // 归还确认的提交人：唯一能 cancel 的人
	retStrangerID  int64 = 4 // 与他们无关的普通登录用户
	retAdminID     int64 = 9 // 管理员
	retItemID      int64 = 11
	retReturnID    int64 = 5
)

// retProofPath 是一个**形状合法**的上传路径（年/月/32 位十六进制.扩展名，见 uploadPathRe）。
const retProofPath = "2026/10/0123456789abcdef0123456789abcdef.jpg"

var retStamp = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// retFound 返回一条 open 的拾物帖，作者是 retOwnerID。
func retFound() *model.ItemDetail {
	d := &model.ItemDetail{}
	d.ID = retItemID
	d.ItemType = model.ItemTypeFound
	d.UserID = retOwnerID
	d.Title = "黑色钱包"
	d.Status = model.ItemStatusOpen
	d.Contact = "wx_owner_123"
	d.CategoryName = "证件卡类"
	d.LocationName = "图书馆"
	d.CoverPath = retProofPath
	d.CreatedAt = retStamp
	return d
}

// retItem 克隆一份帖子并改掉状态/类型/作者，避免各测试为了一点差异重抄全部字段。
func retItem(mut func(*model.ItemDetail)) *model.ItemDetail {
	d := *retFound()
	if mut != nil {
		mut(&d)
	}
	return &d
}

// retRecord 返回一条归还确认记录，默认 pending、提交人是 retSubmitterID。
func retRecord(status string) *model.ItemReturn {
	r := &model.ItemReturn{
		ID:             retReturnID,
		ItemID:         retItemID,
		SubmitterID:    retSubmitterID,
		Message:        "在图书馆三楼捡到的，已经还给他了",
		ProofImagePath: retProofPath,
		Status:         status,
		SubmittedAt:    retStamp,
	}
	if status == model.ReturnStatusConfirmed || status == model.ReturnStatusRejected {
		reviewer := retOwnerID
		kind := model.ReviewKindOwner
		at := retStamp.Add(time.Hour)
		r.ReviewerID = &reviewer
		r.ReviewKind = &kind
		r.ReviewedAt = &at
	}
	return r
}

func retUser(id int64, admin bool, credit int) *model.User {
	role := model.RoleUser
	if admin {
		role = model.RoleAdmin
	}
	return &model.User{
		ID: id, Nickname: "小李", Username: strptr("u" + strconv.FormatInt(id, 10)),
		AuthSource: model.AuthSourceLocal, Role: role,
		Status: model.UserStatusActive, CreditScore: credit,
	}
}

// ---------- fake ----------

// fakeReturnStore 数每一次调用，并把收到的参数原样留下。
//
// 「断言没被调用」是本文件用得最多的一个能力：Submit 的字段校验应当一次库都不发，
// confirm 的权限判断应当在读任何状态之前发生，cancel 不该去读那条帖子。
// 数据库只能看到最终状态，看不到中间有没有写；这一层的 fake 看得到。
type fakeReturnStore struct {
	submitCalls int
	submitRows  []repo.SubmitRow
	submitID    int64
	submitAt    time.Time
	submitErr   error

	getCalls int
	byID     map[int64]*model.ItemReturn
	getErr   error

	confirmCalls int
	confirmRows  []repo.DecideRow
	confirmHints [][]repo.HintRow
	confirmRet   *repo.ConfirmResult
	confirmErr   error

	rejectCalls int
	rejectRows  []repo.DecideRow
	rejectRet   *repo.DecideResult
	rejectErr   error

	cancelCalls int
	cancelArgs  [][2]int64
	cancelRet   *repo.DecideResult
	cancelErr   error

	listCalls   int
	listFilters []repo.ReturnListFilter
	listRows    []model.ItemReturnRow
	listTotal   int
	listErr     error
}

func newFakeReturnStore() *fakeReturnStore {
	return &fakeReturnStore{
		byID:     map[int64]*model.ItemReturn{},
		submitID: 77,
		submitAt: retStamp,
	}
}

func (f *fakeReturnStore) Submit(_ context.Context, p repo.SubmitRow) (int64, time.Time, error) {
	f.submitCalls++
	f.submitRows = append(f.submitRows, p)
	if f.submitErr != nil {
		return 0, time.Time{}, f.submitErr
	}
	return f.submitID, f.submitAt, nil
}

func (f *fakeReturnStore) GetByID(_ context.Context, id int64) (*model.ItemReturn, error) {
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	r, ok := f.byID[id]
	if !ok {
		return nil, apperr.NotFound("归还确认")
	}
	return r, nil
}

func (f *fakeReturnStore) Confirm(_ context.Context, d repo.DecideRow, hints []repo.HintRow) (*repo.ConfirmResult, error) {
	f.confirmCalls++
	f.confirmRows = append(f.confirmRows, d)
	f.confirmHints = append(f.confirmHints, hints)
	if f.confirmErr != nil {
		return nil, f.confirmErr
	}
	if f.confirmRet != nil {
		return f.confirmRet, nil
	}
	return &repo.ConfirmResult{
		ItemID: retItemID, SubmitterID: retSubmitterID, ReviewedAt: retStamp,
		ItemClosed: true, OwnerScore: 110, SubmitterScore: 102,
		OwnerDelta: model.CreditDeltaReturnOwner, SubmitterDelta: model.CreditDeltaReturnSubmitter,
	}, nil
}

func (f *fakeReturnStore) Reject(_ context.Context, d repo.DecideRow) (*repo.DecideResult, error) {
	f.rejectCalls++
	f.rejectRows = append(f.rejectRows, d)
	if f.rejectErr != nil {
		return nil, f.rejectErr
	}
	if f.rejectRet != nil {
		return f.rejectRet, nil
	}
	return &repo.DecideResult{ItemID: retItemID, SubmitterID: retSubmitterID, ReviewedAt: retStamp}, nil
}

func (f *fakeReturnStore) Cancel(_ context.Context, id, submitterID int64) (*repo.DecideResult, error) {
	f.cancelCalls++
	f.cancelArgs = append(f.cancelArgs, [2]int64{id, submitterID})
	if f.cancelErr != nil {
		return nil, f.cancelErr
	}
	if f.cancelRet != nil {
		return f.cancelRet, nil
	}
	return &repo.DecideResult{ItemID: retItemID, SubmitterID: submitterID, ReviewedAt: retStamp}, nil
}

func (f *fakeReturnStore) ListReturns(_ context.Context, fl repo.ReturnListFilter) ([]model.ItemReturnRow, int, error) {
	f.listCalls++
	f.listFilters = append(f.listFilters, fl)
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.listRows, f.listTotal, nil
}

// fakeItemReader 是 ItemLookup 的带计数器版本。
//
// 计数器存在的意义：cancel 那一步刻意不读帖子，而这条纪律只有「calls == 0」
// 这一种写法能钉住 —— 帖子照样能取消，读与不读从响应上完全看不出来。
type fakeItemReader struct {
	byID   map[int64]*model.ItemDetail
	calls  int
	lastID int64
	err    error
}

func newFakeItemReader(items ...*model.ItemDetail) *fakeItemReader {
	m := map[int64]*model.ItemDetail{}
	for _, d := range items {
		m[d.ID] = d
	}
	return &fakeItemReader{byID: m}
}

func (f *fakeItemReader) GetByID(_ context.Context, id int64) (*model.ItemDetail, error) {
	f.calls++
	f.lastID = id
	if f.err != nil {
		return nil, f.err
	}
	d, ok := f.byID[id]
	if !ok {
		return nil, apperr.NotFound("帖子")
	}
	return d, nil
}

type fakeLedgerReader struct {
	authors []int64
	err     error
	calls   int
	gotID   int64
}

func (f *fakeLedgerReader) LedgerLostAuthors(_ context.Context, foundItemID int64) ([]int64, error) {
	f.calls++
	f.gotID = foundItemID
	if f.err != nil {
		return nil, f.err
	}
	return f.authors, nil
}

type fakeReturnUsers struct {
	byID  map[int64]*model.User
	calls int
	err   error
}

func (f *fakeReturnUsers) GetByID(_ context.Context, id int64) (*model.User, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	u, ok := f.byID[id]
	if !ok {
		return nil, apperr.NotFound("用户")
	}
	return u, nil
}

// newReturnSvc 装配一个被测服务。uploads 用真 *Upload 而不是 fake：
// URL() 是纯字符串拼接，用真的顺带把「proof_image_url 长得和 #15 的封面一样」
// 这件事也测了，而 fake 会让这条断言变成自证。
func newReturnSvc(t *testing.T, store *fakeReturnStore, items ItemLookup,
	ledger *fakeLedgerReader, users *fakeReturnUsers) *ItemReturn {
	t.Helper()
	uploads, err := NewUpload(t.TempDir(), "/uploads", nil)
	if err != nil {
		t.Fatalf("建 Upload 失败: %v", err)
	}
	return NewItemReturn(store, items, ledger, users, uploads, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// ---------- 状态机：4 个状态 × 3 个动作，全矩阵 ----------

// TestCheckTransitionFullMatrix 是 §12 判据里那句
// 「状态机表驱动单测覆盖全部 from×action 组合」的直接落地。
//
// 12 个格子逐条列出，**包括那 9 个应当被拒的**：
// 只测合法路径的话，把 legalTransitions 写成「任何状态都能确认」这条测试照样绿 ——
// 因为合法路径的断言全都成立。红的那一半才是这条测试的价值。
func TestCheckTransitionFullMatrix(t *testing.T) {
	froms := []string{
		model.ReturnStatusPending,
		model.ReturnStatusConfirmed,
		model.ReturnStatusRejected,
		model.ReturnStatusCancelled,
	}
	actions := []string{actionConfirm, actionReject, actionCancel}

	type want struct {
		ok bool
		to string
	}
	cases := map[[2]string]want{
		{model.ReturnStatusPending, actionConfirm}:   {true, model.ReturnStatusConfirmed},
		{model.ReturnStatusPending, actionReject}:    {true, model.ReturnStatusRejected},
		{model.ReturnStatusPending, actionCancel}:    {true, model.ReturnStatusCancelled},
		{model.ReturnStatusConfirmed, actionConfirm}: {false, ""},
		{model.ReturnStatusConfirmed, actionReject}:  {false, ""},
		{model.ReturnStatusConfirmed, actionCancel}:  {false, ""},
		{model.ReturnStatusRejected, actionConfirm}:  {false, ""},
		{model.ReturnStatusRejected, actionReject}:   {false, ""},
		{model.ReturnStatusRejected, actionCancel}:   {false, ""},
		{model.ReturnStatusCancelled, actionConfirm}: {false, ""},
		{model.ReturnStatusCancelled, actionReject}:  {false, ""},
		{model.ReturnStatusCancelled, actionCancel}:  {false, ""},
	}

	if len(cases) != len(froms)*len(actions) {
		t.Fatalf("表里 %d 格，矩阵是 %d×%d=%d —— 加状态或加动作时忘了补用例",
			len(cases), len(froms), len(actions), len(froms)*len(actions))
	}

	for _, from := range froms {
		for _, action := range actions {
			t.Run(from+"/"+action, func(t *testing.T) {
				w := cases[[2]string{from, action}]
				to, ok := checkTransition(from, action)
				if ok != w.ok {
					t.Fatalf("checkTransition(%q, %q) ok=%v，期望 %v", from, action, ok, w.ok)
				}
				if to != w.to {
					t.Errorf("checkTransition(%q, %q) 目标状态=%q，期望 %q", from, action, to, w.to)
				}
			})
		}
	}
}

// TestTerminalStatesHaveNoOutgoingEdge 单独钉住「三个终态在表里连键都没有」。
//
// 为什么在矩阵之外还要这一条：矩阵里三个终态的 9 个格子全过，
// 也可能只是因为内层 map 存在但是空 —— 那和「不在这个状态机上」是两回事：
// 空内层 map 意味着将来有人填一个键就有了出边，而表设计上终态**不该有出边**。
// （写这条测试时 legalTransitions 只有一个键；第二个键出现的那天它会提醒读 §3.4。）
func TestTerminalStatesHaveNoOutgoingEdge(t *testing.T) {
	for _, terminal := range []string{
		model.ReturnStatusConfirmed, model.ReturnStatusRejected, model.ReturnStatusCancelled,
	} {
		if _, ok := legalTransitions[terminal]; ok {
			t.Errorf("%q 是终态，legalTransitions 里不该有它这一键（有出边等于允许回退）", terminal)
		}
	}
	if len(legalTransitions) != 1 {
		t.Errorf("legalTransitions 有 %d 个键，期望 1 个（只有 pending 有出边）—— 加了新状态？先读 §3.4 那张图",
			len(legalTransitions))
	}
}

// ---------- 权限：admin 为什么被挡住 ----------

// TestAuthorizeReturnDecisionIsIDEqualityNotRole 是「admin 也不行」的可执行定义。
//
// ⚠ 这个函数没有 role 参数。它**看不出**调用方是不是管理员，
// 所以「admin 被拒绝」不是一条特判，而是不相等的那个必然结果。
// 反射检查签名（下面那条测试）+ 这张矩阵 = 计划 §4 第 25 行那句
// 「发帖人（Admin 也不行）」唯一可靠的落地形式。
func TestAuthorizeReturnDecisionIsIDEqualityNotRole(t *testing.T) {
	cases := []struct {
		name                    string
		action                  string
		actor, owner, submitter int64
		want                    string // "" 表示放行
	}{
		{"帖主本人确认", actionConfirm, retOwnerID, retOwnerID, retSubmitterID, ""},
		{"提交人不能确认自己的", actionConfirm, retSubmitterID, retOwnerID, retSubmitterID, apperr.CodeForbidden},
		{"路人不能确认", actionConfirm, retStrangerID, retOwnerID, retSubmitterID, apperr.CodeForbidden},
		{"admin 不能确认（只是一个不相等的 id）", actionConfirm, retAdminID, retOwnerID, retSubmitterID, apperr.CodeForbidden},
		{"帖主本人拒绝", actionReject, retOwnerID, retOwnerID, retSubmitterID, ""},
		{"admin 不能拒绝", actionReject, retAdminID, retOwnerID, retSubmitterID, apperr.CodeForbidden},
		{"提交人不能替帖主拒绝", actionReject, retSubmitterID, retOwnerID, retSubmitterID, apperr.CodeForbidden},
		{"提交人自己撤销", actionCancel, retSubmitterID, retOwnerID, retSubmitterID, ""},
		{"帖主不能替提交人撤销", actionCancel, retOwnerID, retOwnerID, retSubmitterID, apperr.CodeForbidden},
		{"admin 不能撤销", actionCancel, retAdminID, retOwnerID, retSubmitterID, apperr.CodeForbidden},
		// 帖主同时是提交人的话（Submit 挡住了，但这道防线独立存在）：
		// cancel 看的是 submitter，confirm 看的是 owner，两个分支互不串用。
		{"同人双身份时两个分支各看各的参数", actionCancel, retOwnerID, retOwnerID, retOwnerID, ""},
		{"未知动作是编程错误不是用户输入", "delete", retOwnerID, retOwnerID, retSubmitterID, apperr.CodeInternal},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := authorizeReturnDecision(c.action, c.actor, c.owner, c.submitter)
			if c.want == "" {
				if err != nil {
					t.Fatalf("期望放行，实际 %v", err)
				}
				return
			}
			if got := codeOf(t, err); got != c.want {
				t.Fatalf("期望 %s，实际 %s", c.want, got)
			}
		})
	}
}

// TestAuthorizeReturnDecisionSignatureHasNoRole 用反射钉住那个纯函数的参数表。
//
// 「admin 也不行」这句话最脆的实现方式是 `if actor.IsAdmin() { 放行 }` 的反面 ——
// 一次「方便管理员」的顺手改动就能把它加回来，而加回来之后行为仍然符合所有
// 正向测试（帖主能确认、路人不能），只有专门测 admin 的那几条会红。
// 把签名钉死之后，加那个分支得先加一个参数，而那会立刻红在这里，
// 并且红的时候**必须**来改这段注释。
func TestAuthorizeReturnDecisionSignatureHasNoRole(t *testing.T) {
	typ := reflect.TypeOf(authorizeReturnDecision)
	if typ.NumIn() != 4 {
		t.Fatalf("authorizeReturnDecision 期望 4 个参数，实际 %d", typ.NumIn())
	}
	for i := range typ.NumIn() {
		in := typ.In(i)
		if i == 0 {
			if in.Kind() != reflect.String {
				t.Errorf("第 0 个参数应该是动作名（string），实际是 %v", in)
			}
			continue
		}
		if in.Kind() != reflect.Int64 {
			t.Errorf("第 %d 个参数应该是 id（int64），实际是 %v —— 出现 model.User / role 字符串了？"+
				"停下：读 §3.5 和计划 §4 第 25 行那句「Admin 也不行」", i, in)
		}
	}
}

// ---------- 依赖形状：那四行禁令的机器可查形态 ----------

// TestM5DependenciesCannotDoWhatTheyMustNot 是本文件的核心。
//
// 四条禁令都是否定式的（「不许有前置门槛」「不许直接改分」「不许关帖」「不许发通知」）。
// 否定式规则用行为断言来测是不可靠的：一个从没用过的方法本来就不会出现在测试里。
// 方法名是这份约束唯一能被机器查证的形态 —— 遍历服务的全部依赖，
// 只要出现下面任何一个方法名，就说明那条禁令被打通了。
func TestM5DependenciesCannotDoWhatTheyMustNot(t *testing.T) {
	// 每个名字都对应一条具体的禁令，注释里写清楚是哪一条。
	banned := map[string]string{
		"Viewed":       "§16 删掉的 RETURN_NOT_UNLOCKED：归还服务不该能读 contact_views",
		"Unlock":       "解锁联系方式是 #21 的事，提交归还确认不要求先解锁",
		"Update":       "关帖/改帖住在 repo.Confirm 的事务里，不在这个服务上",
		"ChangeStatus": "#18 的「已找到」是帖主自己的动作，confirm 不能替他调",
		"SetStatus":    "状态推进只有 Submit/Confirm/Reject/Cancel 四条路",
		"SetCredit":    "§3.6：积分只能被赚，不能被改",
		"AddCredit":    "加分唯一的路径是 repo.Confirm 内部的 applyCredit",
		"Insert":       "通知必须住在产生它的那个动作里（repo/credit.go 顶部同一条纪律）",
		"Push":         "同上",
		"Create":       "同上：这里没有第二条插通知的路",
	}

	svc := newReturnSvc(t, newFakeReturnStore(), newFakeItemReader(), &fakeLedgerReader{}, &fakeReturnUsers{byID: map[int64]*model.User{}})
	typ := reflect.TypeOf(*svc)
	for i := range typ.NumField() {
		field := typ.Field(i)
		if field.Name == "logger" {
			continue // 日志器不是数据依赖
		}
		ft := field.Type
		for m := range ft.NumMethod() {
			name := ft.Method(m).Name
			if why, hit := banned[name]; hit {
				t.Errorf("依赖 %s (%v) 有方法 %s —— %s", field.Name, ft, name, why)
			}
		}
	}
}

// TestM5SeamMethodCounts 同 M4 那条 NumMethod 测试：把「接缝有几个方法」钉成数字。
func TestM5SeamMethodCounts(t *testing.T) {
	cases := []struct {
		name    string
		iface   any
		want    int
		noteWhy string
	}{
		{"ReturnStore", (*ReturnStore)(nil), 6, "四个动作 + 两个读，多一个就是多一条绕过状态机的路"},
		{"LedgerLookup", (*LedgerLookup)(nil), 1, "只读台账；能写 match_pairs 的依赖不该出现在归还服务里"},
		{"UserLookup", (*UserLookup)(nil), 1, "只读一行 users"},
		{"CreditStore", (*CreditStore)(nil), 1, "§3.6：流水只能和改分同事务产生，所以这个接口连 Insert 都没有"},
	}
	for _, c := range cases {
		typ := reflect.TypeOf(c.iface).Elem()
		if got := typ.NumMethod(); got != c.want {
			t.Errorf("%s 期望 %d 个方法，实际 %d —— %s", typ.Name(), c.want, got, c.noteWhy)
		}
	}
}

// ---------- #23 提交：字段校验排在一切查库之前 ----------

// TestSubmitBadFieldsNeverTouchTheDB 钉住 Submit 判断顺序的第①步。
//
// 除了错误码，更重要的是 submitCalls==0 **和** items.calls==0：
// 一个连说明都没写对的请求不应该去查那条帖子，更不该往 item_returns 插行。
// 后者在集成测试里也能测（数表），但那时已经付出了一次数据库往返。
func TestSubmitBadFieldsNeverTouchTheDB(t *testing.T) {
	cases := []struct {
		name  string
		in    SubmitReturnInput
		field string
	}{
		{"说明太短（4 个字）", SubmitReturnInput{Message: "还给你", ProofImagePath: retProofPath}, "message"},
		{"说明为空", SubmitReturnInput{Message: "   ", ProofImagePath: retProofPath}, "message"},
		{"说明太长", SubmitReturnInput{Message: strings.Repeat("字", model.ReturnMessageMaxChars+1), ProofImagePath: retProofPath}, "message"},
		{"没带凭证图", SubmitReturnInput{Message: "在图书馆三楼捡到的", ProofImagePath: ""}, "proof_image_path"},
		{"凭证图是磁盘路径", SubmitReturnInput{Message: "在图书馆三楼捡到的", ProofImagePath: "/etc/passwd"}, "proof_image_path"},
		{"凭证图是 ../", SubmitReturnInput{Message: "在图书馆三楼捡到的", ProofImagePath: "../../.env"}, "proof_image_path"},
		{"凭证图是别人的域名", SubmitReturnInput{Message: "在图书馆三楼捡到的", ProofImagePath: "https://evil.example/a.jpg"}, "proof_image_path"},
		{"凭证图扩展名不在白名单", SubmitReturnInput{Message: "在图书馆三楼捡到的", ProofImagePath: "2026/10/0123456789abcdef0123456789abcdef.svg"}, "proof_image_path"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newFakeReturnStore()
			items := newFakeItemReader(retFound())
			svc := newReturnSvc(t, store, items, &fakeLedgerReader{}, &fakeReturnUsers{byID: map[int64]*model.User{}})

			_, err := svc.Submit(context.Background(), retUser(retSubmitterID, false, 100), retItemID, c.in)
			if got := codeOf(t, err); got != apperr.CodeValidation {
				t.Fatalf("期望 VALIDATION，实际 %s（%v）", got, err)
			}
			var ae *apperr.Error
			if !errors.As(err, &ae) || !slices.ContainsFunc(ae.Fields, func(f apperr.FieldError) bool { return f.Field == c.field }) {
				t.Errorf("VALIDATION 里应该带字段 %s 的说明，实际 %#v", c.field, ae)
			}
			if store.submitCalls != 0 {
				t.Errorf("字段不合法时不该插记录，却调了 Submit %d 次", store.submitCalls)
			}
			if items.calls != 0 {
				t.Errorf("字段不合法时连帖子都不该读，却查了 %d 次", items.calls)
			}
		})
	}
}

// TestSubmitCountsRunesNotBytes 是 §12 那条「中文用户写满 333 个字就被拒」的防回归测试。
//
// 判别式必须精心构造：300 个汉字的 utf8 字节长度是 900（< 1000），
// 用 len() 和用 utf8.RuneCountInString() 都放行，测不出差别。
// 所以这里用的是 400 个汉字 = 1200 字节：
//   - 按字节算 → 1200 > 1000 → VALIDATION（错的实现）
//   - 按字符算 → 400 ≤ 1000 → 放行（对的实现）
//
// 另一半用 4 个汉字（=12 字节）：按字节它会放行（12 ≥ 5），按字符它该拒 ——
// 两个方向各一个用例，才真正锁住「数的是什么单位」。
func TestSubmitCountsRunesNotBytes(t *testing.T) {
	ctx := context.Background()

	t.Run("400 个汉字（1200 字节）必须放行", func(t *testing.T) {
		store := newFakeReturnStore()
		svc := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{},
			&fakeReturnUsers{byID: map[int64]*model.User{}})
		msg := strings.Repeat("汉", 400)
		if utf8.RuneCountInString(msg) > model.ReturnMessageMaxChars {
			t.Fatalf("夹具本身就越过上限了，用例失去意义")
		}
		if len(msg) <= model.ReturnMessageMaxChars {
			t.Fatalf("夹具的**字节**长度没超过上限，这个用例区分不出字节/字符")
		}
		if _, err := svc.Submit(ctx, retUser(retSubmitterID, false, 100), retItemID,
			SubmitReturnInput{Message: msg, ProofImagePath: retProofPath}); err != nil {
			t.Fatalf("按字符数它只有 400，不该被拒：%v", err)
		}
	})

	t.Run("4 个汉字（12 字节）必须被拒", func(t *testing.T) {
		store := newFakeReturnStore()
		svc := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{},
			&fakeReturnUsers{byID: map[int64]*model.User{}})
		_, err := svc.Submit(ctx, retUser(retSubmitterID, false, 100), retItemID,
			SubmitReturnInput{Message: "还给你了", ProofImagePath: retProofPath})
		if got := codeOf(t, err); got != apperr.CodeValidation {
			t.Fatalf("4 个字该报 VALIDATION（按字节算的话 12≥5 会被放行），实际 %s", got)
		}
		if store.submitCalls != 0 {
			t.Errorf("被拒的请求插了 %d 条记录", store.submitCalls)
		}
	})
}

// TestSubmitPrerequisiteMatrix 钉住 Submit 的第②–⑤步，以及它们之间的**顺序**。
//
// 顺序在这里不是洁癖而是文案的可行动性：
//   - ②deleted → NOT_FOUND 排在最前：软删的帖子对广场等于不存在，
//     对它报错「帖子已关闭」等于承认它还在（同 §4 #49「不通知被举报人」那条纪律）
//   - ③lost → VALIDATION 排在 ④ 前面：小王对自己那条 lost 帖点归还确认，
//     如果先撞上 RETURN_SELF，他会困惑地去发一条 found 帖试试 —— 那才是真正的泄漏
//   - ④RETURN_SELF 排在 ⑤ITEM_CLOSED 前面：自己的 closed 帖，
//     「不能对自己的帖子提交」比「这条帖子已关闭」更准（同 #21 的 ③④ 完全同构）
func TestSubmitPrerequisiteMatrix(t *testing.T) {
	cases := []struct {
		name   string
		item   *model.ItemDetail
		actor  int64
		want   string
		reason string
	}{
		{
			name:   "软删的帖子 → NOT_FOUND（且不承认它已关闭）",
			item:   retItem(func(d *model.ItemDetail) { d.Status = model.ItemStatusDeleted }),
			actor:  retSubmitterID,
			want:   apperr.CodeNotFound,
			reason: "②：deleted 一律当不存在",
		},
		{
			name:   "自己的失物帖 → VALIDATION 而不是 RETURN_SELF",
			item:   retItem(func(d *model.ItemDetail) { d.ItemType = model.ItemTypeLost; d.UserID = retSubmitterID }),
			actor:  retSubmitterID,
			want:   apperr.CodeValidation,
			reason: "③ 必须排在 ④ 前面",
		},
		{
			name:   "别人的失物帖 → VALIDATION",
			item:   retItem(func(d *model.ItemDetail) { d.ItemType = model.ItemTypeLost }),
			actor:  retSubmitterID,
			want:   apperr.CodeValidation,
			reason: "item_returns 只服务 found 帖",
		},
		{
			name:   "自己的拾物帖 → RETURN_SELF",
			item:   retItem(func(d *model.ItemDetail) { d.UserID = retSubmitterID }),
			actor:  retSubmitterID,
			want:   apperr.CodeReturnSelf,
			reason: "④",
		},
		{
			name:   "自己的、已关闭的拾物帖 → 仍然是 RETURN_SELF",
			item:   retItem(func(d *model.ItemDetail) { d.UserID = retSubmitterID; d.Status = model.ItemStatusClosed }),
			actor:  retSubmitterID,
			want:   apperr.CodeReturnSelf,
			reason: "④ 排在 ⑤ 前面",
		},
		{
			name:   "别人的 closed 帖 → ITEM_CLOSED",
			item:   retItem(func(d *model.ItemDetail) { d.Status = model.ItemStatusClosed }),
			actor:  retSubmitterID,
			want:   apperr.CodeItemClosed,
			reason: "⑤：closed 的帖子不收新的归还确认",
		},
		{
			name:   "别人的 deleted 帖 → NOT_FOUND 而不是 ITEM_CLOSED",
			item:   retItem(func(d *model.ItemDetail) { d.Status = model.ItemStatusDeleted }),
			actor:  retSubmitterID,
			want:   apperr.CodeNotFound,
			reason: "② 排在 ⑤ 前面",
		},
		{
			name:   "admin 提交别人的帖子的归还确认 → 一律 RETURN_SELF",
			item:   retItem(func(d *model.ItemDetail) { d.UserID = retAdminID }),
			actor:  retAdminID,
			want:   apperr.CodeReturnSelf,
			reason: "定位原则 5：admin 也是普通提交者，平台不给他开「替别人声称归还」的口子",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newFakeReturnStore()
			svc := newReturnSvc(t, store, newFakeItemReader(c.item), &fakeLedgerReader{},
				&fakeReturnUsers{byID: map[int64]*model.User{}})

			_, err := svc.Submit(context.Background(), retUser(c.actor, false, 100), retItemID,
				SubmitReturnInput{Message: "在图书馆三楼捡到的，已当面归还", ProofImagePath: retProofPath})
			if got := codeOf(t, err); got != c.want {
				t.Fatalf("%s：期望 %s，实际 %s（%v）", c.reason, c.want, got, err)
			}
			if store.submitCalls != 0 {
				t.Errorf("被前置条件挡下时不该插记录，却插了 %d 次", store.submitCalls)
			}
		})
	}
}

// TestSubmitUnknownItemPropagatesNotFound 帖子读不到时错误原样上抛，
// 不许在这里被美化成 VALIDATION —— 那是实现细节而不是业务规则。
func TestSubmitUnknownItemPropagatesNotFound(t *testing.T) {
	store := newFakeReturnStore()
	svc := newReturnSvc(t, store, newFakeItemReader(), &fakeLedgerReader{},
		&fakeReturnUsers{byID: map[int64]*model.User{}})
	_, err := svc.Submit(context.Background(), retUser(retSubmitterID, false, 100), 999,
		SubmitReturnInput{Message: "在图书馆三楼捡到的", ProofImagePath: retProofPath})
	if got := codeOf(t, err); got != apperr.CodeNotFound {
		t.Fatalf("期望 NOT_FOUND，实际 %s", got)
	}
}

// TestSubmitHappyPathRowIsWhatOwnerNeeds 断言交给 repo 的那一行**内容**。
//
// 这是 §14-5「判断真假的权力全在发帖人手里」的落地检查点：
// 发帖人在列表里只看到一条通知，所以通知必须带得上手的原料 ——
// 收件人必须是帖主（不是提交人）、文案非空、说明被 trim 过、凭证路径原样传递。
// 「NotifyTo 写错成提交人」这种 bug 从 HTTP 响应完全看不出来（响应只回 id），
// 只有在这里断言才拦得住。
func TestSubmitHappyPathRowIsWhatOwnerNeeds(t *testing.T) {
	store := newFakeReturnStore()
	items := newFakeItemReader(retFound())
	svc := newReturnSvc(t, store, items, &fakeLedgerReader{},
		&fakeReturnUsers{byID: map[int64]*model.User{}})

	res, err := svc.Submit(context.Background(), retUser(retSubmitterID, false, 100), retItemID,
		SubmitReturnInput{Message: "  在图书馆三楼捡到的，已当面归还  ", ProofImagePath: retProofPath})
	if err != nil {
		t.Fatalf("Submit 失败: %v", err)
	}

	if store.submitCalls != 1 {
		t.Fatalf("Submit 调了 %d 次，期望 1 次", store.submitCalls)
	}
	row := store.submitRows[0]
	if row.NotifyTo != retOwnerID {
		t.Errorf("return_submitted 的收件人应该是帖主 %d，实际 %d", retOwnerID, row.NotifyTo)
	}
	if row.ItemID != retItemID || row.SubmitterID != retSubmitterID {
		t.Errorf("行里的 (item, submitter) = (%d,%d)，期望 (%d,%d)",
			row.ItemID, row.SubmitterID, retItemID, retSubmitterID)
	}
	if row.Message != "在图书馆三楼捡到的，已当面归还" {
		t.Errorf("说明没被 trim 或改坏了：%q", row.Message)
	}
	if row.ProofPath != retProofPath {
		t.Errorf("凭证路径应该原样传下去（不改写成 URL），实际 %q", row.ProofPath)
	}
	if row.NotifyTitle == "" || row.NotifyBody == "" {
		t.Errorf("通知文案不能为空，否则发帖人点开一条空的：%q / %q", row.NotifyTitle, row.NotifyBody)
	}
	if !strings.Contains(row.NotifyBody, "小李") {
		t.Errorf("给帖主的通知里应该带提交人昵称（那是他判断的原料），实际 %q", row.NotifyBody)
	}

	if res.Status != model.ReturnStatusPending {
		t.Errorf("data.status 应该是 pending，实际 %q", res.Status)
	}
	if res.ID != 77 || res.ItemID != retItemID {
		t.Errorf("data.id/item = (%d,%d)，期望 (77,%d)", res.ID, res.ItemID, retItemID)
	}
	if _, err := time.Parse(time.RFC3339, res.SubmittedAt); err != nil {
		t.Errorf("submitted_at 不是 RFC3339: %q", res.SubmittedAt)
	}
}

// TestSubmitPropagatesDuplicate 撞部分唯一索引时那个 409 必须原样上抛。
//
// 它是 repo 那一层翻译出来的（RETURN_DUPLICATE），service 不许把它重新包装成
// INTERNAL 或 CONFLICT —— 前端要靠这个码区分「你已经提过一条了」和「出故障了」。
func TestSubmitPropagatesDuplicate(t *testing.T) {
	store := newFakeReturnStore()
	store.submitErr = apperr.New(apperr.CodeReturnDuplicate)
	svc := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{},
		&fakeReturnUsers{byID: map[int64]*model.User{}})

	_, err := svc.Submit(context.Background(), retUser(retSubmitterID, false, 100), retItemID,
		SubmitReturnInput{Message: "在图书馆三楼捡到的", ProofImagePath: retProofPath})
	if got := codeOf(t, err); got != apperr.CodeReturnDuplicate {
		t.Fatalf("期望 RETURN_DUPLICATE 原样上抛，实际 %s", got)
	}
}

// ---------- #25/#26 判断顺序：权限 → 状态 → 字段 ----------

// TestDecisionOrderPermissionThenStateThenField 是本文件最重要的一条测试。
//
// 三段顺序各有一个用例，而且每个用例都故意让「后面的那一环」也失败：
//   - 路人 + 已 confirmed：状态也非法，但必须回 FORBIDDEN
//   - 帖主 + 已 confirmed：这才轮到 RETURN_ILLEGAL_TRANSITION
//   - 路人 + 501 字的备注：字段也非法，但必须回 FORBIDDEN
//
// 为什么值得写这么绕的用例：「先读状态再判权」的代码在所有**正向**测试里
// 表现完全一样（帖主对 pending 操作，两段的判断都成立），
// 只有让两段同时失败才分得出先后。而分错先后的后果是泄漏：
// 一句「当前状态不允许此操作」向一个路人确认了「这条记录存在，而且它现在长什么样」，
// 那是他本来不该知道的信息（同 service/item_validate.go 的 authorizeItemWrite 第 3 条）。
func TestDecisionOrderPermissionThenStateThenField(t *testing.T) {
	ctx := context.Background()

	type setup struct {
		name   string
		status string
		actor  *model.User
		note   string
		want   string
	}

	cases := []setup{
		{"路人确认已确认的记录 → FORBIDDEN（不是 409）", model.ReturnStatusConfirmed,
			retUser(retStrangerID, false, 100), "", apperr.CodeReturnIllegalTransition},
	}

	// 上面那条断言的期望值先按实现写死为 409 是**错的**，所以这一组用表分开写，
	// 让读者一眼看到「同一条已 confirmed 的记录，两个人问出两个答案」。
	t.Run("同一条 confirmed 记录，路人和帖主得到的答案不同", func(t *testing.T) {
		_ = cases // 见下面两组显式断言

		strangerStore := newFakeReturnStore()
		strangerStore.byID[retReturnID] = retRecord(model.ReturnStatusConfirmed)
		svc := newReturnSvc(t, strangerStore, newFakeItemReader(retFound()), &fakeLedgerReader{},
			&fakeReturnUsers{byID: map[int64]*model.User{}})
		_, err := svc.Confirm(ctx, retUser(retStrangerID, false, 100), retReturnID, "")
		if got := codeOf(t, err); got != apperr.CodeForbidden {
			t.Fatalf("路人应当只听到「没权限」，实际 %s —— 他刚从这句回复里读出了这条记录的状态", got)
		}
		if strangerStore.confirmCalls != 0 {
			t.Errorf("被挡下时不该写库，却调了 Confirm %d 次", strangerStore.confirmCalls)
		}

		ownerStore := newFakeReturnStore()
		ownerStore.byID[retReturnID] = retRecord(model.ReturnStatusConfirmed)
		ownerSvc := newReturnSvc(t, ownerStore, newFakeItemReader(retFound()), &fakeLedgerReader{},
			&fakeReturnUsers{byID: map[int64]*model.User{}})
		_, err = ownerSvc.Confirm(ctx, retUser(retOwnerID, false, 100), retReturnID, "")
		if got := codeOf(t, err); got != apperr.CodeReturnIllegalTransition {
			t.Fatalf("帖主自己重复点确认应该听到 409，实际 %s", got)
		}
	})

	t.Run("admin 确认与拒绝都 FORBIDDEN，且一次写库都没有", func(t *testing.T) {
		for _, action := range []string{"confirm", "reject"} {
			store := newFakeReturnStore()
			store.byID[retReturnID] = retRecord(model.ReturnStatusPending)
			svc := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{},
				&fakeReturnUsers{byID: map[int64]*model.User{}})

			var err error
			if action == "confirm" {
				_, err = svc.Confirm(ctx, retUser(retAdminID, true, 100), retReturnID, "我替他确认")
			} else {
				_, err = svc.Reject(ctx, retUser(retAdminID, true, 100), retReturnID, "我替他拒绝")
			}
			if got := codeOf(t, err); got != apperr.CodeForbidden {
				t.Errorf("admin %s 应该 FORBIDDEN，实际 %s —— 定位原则 5：admin 能销毁内容，制造不出归属", action, got)
			}
			if store.confirmCalls+store.rejectCalls != 0 {
				t.Errorf("admin %s 被拒之后仍然写了库：confirm=%d reject=%d",
					action, store.confirmCalls, store.rejectCalls)
			}
		}
	})

	t.Run("权限先于字段：路人 + 超长备注 → FORBIDDEN 而不是 VALIDATION", func(t *testing.T) {
		store := newFakeReturnStore()
		store.byID[retReturnID] = retRecord(model.ReturnStatusPending)
		svc := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{},
			&fakeReturnUsers{byID: map[int64]*model.User{}})

		long := strings.Repeat("太", model.ReturnOwnerNoteMaxChars+1)
		_, err := svc.Reject(ctx, retUser(retStrangerID, false, 100), retReturnID, long)
		if got := codeOf(t, err); got != apperr.CodeForbidden {
			t.Fatalf("期望 FORBIDDEN，实际 %s —— 「把备注改短你就能审这条记录」是在向外描述一条通道", got)
		}
	})

	t.Run("帖主 + 超长备注 → VALIDATION 且不写库", func(t *testing.T) {
		store := newFakeReturnStore()
		store.byID[retReturnID] = retRecord(model.ReturnStatusPending)
		svc := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{},
			&fakeReturnUsers{byID: map[int64]*model.User{}})

		long := strings.Repeat("太", model.ReturnOwnerNoteMaxChars+1)
		_, err := svc.Confirm(ctx, retUser(retOwnerID, false, 100), retReturnID, long)
		if got := codeOf(t, err); got != apperr.CodeValidation {
			t.Fatalf("期望 VALIDATION，实际 %s", got)
		}
		if store.confirmCalls != 0 {
			t.Errorf("字段不合法时不该写库，却调了 Confirm %d 次", store.confirmCalls)
		}
	})

	t.Run("记录不存在时连帖子都不读", func(t *testing.T) {
		store := newFakeReturnStore()
		items := newFakeItemReader(retFound())
		svc := newReturnSvc(t, store, items, &fakeLedgerReader{},
			&fakeReturnUsers{byID: map[int64]*model.User{}})
		_, err := svc.Confirm(ctx, retUser(retOwnerID, false, 100), 999, "")
		if got := codeOf(t, err); got != apperr.CodeNotFound {
			t.Fatalf("期望 NOT_FOUND，实际 %s", got)
		}
		if items.calls != 0 {
			t.Errorf("归还记录都不存在，读帖子没有任何用处却查了 %d 次（这条记录指向哪条帖都不知道）", items.calls)
		}
	})
}

// TestOwnerNoteLimitCountsRunesNotBytes 同 Submit 那条：备注上限 500 按字符数。
// 300 个汉字的备注（900 字节）必须放行；VARCHAR(500) 数的是字符，
// 用 len() 的话中文拾主写一句「非常感谢」级别的长备注就被拒。
func TestOwnerNoteLimitCountsRunesNotBytes(t *testing.T) {
	ctx := context.Background()
	store := newFakeReturnStore()
	store.byID[retReturnID] = retRecord(model.ReturnStatusPending)
	svc := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{},
		&fakeReturnUsers{byID: map[int64]*model.User{}})

	note := strings.Repeat("谢", 400) // 1200 字节，400 字符
	if _, err := svc.Confirm(ctx, retUser(retOwnerID, false, 100), retReturnID, note); err != nil {
		t.Fatalf("400 个汉字的备注应该放行：%v", err)
	}
	if utf8.RuneCountInString(store.confirmRows[0].OwnerNote) != 400 {
		t.Errorf("备注被改坏了，长度 %d", utf8.RuneCountInString(store.confirmRows[0].OwnerNote))
	}

	_, err := svc.Confirm(ctx, retUser(retOwnerID, false, 100), retReturnID, strings.Repeat("谢", 501))
	if got := codeOf(t, err); got != apperr.CodeValidation {
		t.Fatalf("501 个字符该 VALIDATION（501 汉字=1503 字节，按字节的话 500 汉字也会被拒）")
	}
}

// TestRejectRequiresNoteButConfirmDoesNot 是 #25/#26 那个**刻意的不对称**。
//
// 计划 §4 只在第 26 行给 reject 列了 VALIDATION：
// 拒绝是一个坏消息，而发帖人的那句留言是提交人唯一能得到的解释；
// 确认不需要理由，所以那里连空字符串都合法。
// 两个用例一起写才看得出这是「一个必填一个选填」而不是两处各写了一遍。
func TestRejectRequiresNoteButConfirmDoesNot(t *testing.T) {
	ctx := context.Background()

	newSvc := func(t *testing.T) (*ItemReturn, *fakeReturnStore) {
		store := newFakeReturnStore()
		store.byID[retReturnID] = retRecord(model.ReturnStatusPending)
		return newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{},
			&fakeReturnUsers{byID: map[int64]*model.User{}}), store
	}

	for _, blank := range []string{"", "   ", "\t\n"} {
		svc, store := newSvc(t)
		_, err := svc.Reject(ctx, retUser(retOwnerID, false, 100), retReturnID, blank)
		if got := codeOf(t, err); got != apperr.CodeValidation {
			t.Fatalf("拒绝时空理由（%q）必须 VALIDATION，实际 %s —— 那是提交人唯一能得到的解释", blank, got)
		}
		if store.rejectCalls != 0 {
			t.Errorf("理由不合法时不该写库，却调了 Reject %d 次", store.rejectCalls)
		}
	}

	svc, store := newSvc(t)
	if _, err := svc.Confirm(ctx, retUser(retOwnerID, false, 100), retReturnID, ""); err != nil {
		t.Fatalf("确认不写备注必须放行：%v", err)
	}
	if store.confirmRows[0].OwnerNote != "" {
		t.Errorf("confirm 的备注应该是空串，实际 %q", store.confirmRows[0].OwnerNote)
	}

	// 空白理由对 confirm 来说应当被 trim 成空串（而不是留下三个空格写进库）
	svc2, store2 := newSvc(t)
	if _, err := svc2.Confirm(ctx, retUser(retOwnerID, false, 100), retReturnID, "  "); err != nil {
		t.Fatalf("空白备注也应该放行：%v", err)
	}
	if store2.confirmRows[0].OwnerNote != "" {
		t.Errorf("备注应该被 trim 成空串，实际 %q", store2.confirmRows[0].OwnerNote)
	}
}

// ---------- confirm 的连带 ----------

// TestConfirmHandsRepoEverythingTheOneTransactionNeeds 断言 confirm 交给 repo 的三样东西。
//
// confirm 是全项目副作用最大的一个用户动作（改记录 + 关帖 + 两处加分 + 两条流水 + 1..N 条通知），
// 而 repo 那一个事务只认 service 传进去的这几个值：
//   - OwnerID 必须是操作者本人（那一列 reviewer_id 是「谁同意了」的存证）
//   - NotifyTitle/Body 非空（return_confirmed 是提交人唯一的回执）
//   - hints 一个作者一条，且带去的是那条拾物帖的 id
//
// 「reviewer_id 传成帖主的 id」这类 bug 在 HTTP 响应里看不见（响应只有 status），
// 所以断言必须落在参数上。
func TestConfirmHandsRepoEverythingTheOneTransactionNeeds(t *testing.T) {
	store := newFakeReturnStore()
	store.byID[retReturnID] = retRecord(model.ReturnStatusPending)
	items := newFakeItemReader(retFound())
	ledger := &fakeLedgerReader{authors: []int64{retSubmitterID, 21, 22}}
	svc := newReturnSvc(t, store, items, ledger, &fakeReturnUsers{byID: map[int64]*model.User{}})

	res, err := svc.Confirm(context.Background(), retUser(retOwnerID, false, 100), retReturnID, "东西收到了，谢谢")
	if err != nil {
		t.Fatalf("Confirm 失败: %v", err)
	}

	if ledger.calls != 1 {
		t.Fatalf("台账应该恰好读一次，实际 %d 次", ledger.calls)
	}
	if ledger.gotID != retItemID {
		t.Errorf("台账反查用的应该是那条**拾物帖**的 id，实际用了 %d", ledger.gotID)
	}

	if store.confirmCalls != 1 {
		t.Fatalf("Confirm 调了 %d 次", store.confirmCalls)
	}
	row := store.confirmRows[0]
	if row.ID != retReturnID || row.OwnerID != retOwnerID {
		t.Errorf("DecideRow = (%d,%d)，期望 (%d,%d)", row.ID, row.OwnerID, retReturnID, retOwnerID)
	}
	if row.NotifyTitle == "" || row.NotifyBody == "" {
		t.Errorf("给提交人的回执文案不能为空：%q / %q", row.NotifyTitle, row.NotifyBody)
	}

	hints := store.confirmHints[0]
	if len(hints) != 3 {
		t.Fatalf("三个 lost 作者应该收到 3 条 hint，实际 %d 条", len(hints))
	}
	for i, h := range hints {
		if h.UserID != ledger.authors[i] {
			t.Errorf("第 %d 条 hint 的收件人是 %d，期望 %d", i, h.UserID, ledger.authors[i])
		}
		if h.Title == "" || h.Body == "" {
			t.Errorf("hint 文案不能为空：%+v", h)
		}
	}
	// 提交人自己也在台账里 —— §13 步 4 明确说他收到两条（return_confirmed + item_returned_hint）。
	// 这里断言他**没有**被去重掉，因为那是设计而不是遗漏（见 repo.LedgerLostAuthors 注释）。
	if hints[0].UserID != retSubmitterID {
		t.Errorf("台账第一个作者就是提交人，hint 列表里他必须还在（不去重）：%+v", hints)
	}

	if res.Status != model.ReturnStatusConfirmed {
		t.Errorf("data.status = %q，期望 confirmed", res.Status)
	}
	if res.CreditDelta != model.CreditDeltaReturnOwner {
		t.Errorf("data.credit_delta 应该是**拾主**那一份（+%d），实际 %d", model.CreditDeltaReturnOwner, res.CreditDelta)
	}
}

// TestConfirmClampsCreditDeltaToWhatActuallyApplied 钉住 §3.6 那条对账性质。
//
// 分数已经 200 的拾主再确认一次，repo 夹住之后实际增量是 0，
// 而响应里的 credit_delta 必须是那个 0 —— 因为 #33 的流水里躺着的也是 0。
// 如果这里回一句「+10」，用户会在自己的流水里看到一条 0，
// 于是「平台说我加了 10 分却没加」变成一个查不出原因的投诉。
func TestConfirmClampsCreditDeltaToWhatActuallyApplied(t *testing.T) {
	store := newFakeReturnStore()
	store.byID[retReturnID] = retRecord(model.ReturnStatusPending)
	_, ceil := model.CreditScoreBounds()
	store.confirmRet = &repo.ConfirmResult{
		ItemID: retItemID, SubmitterID: retSubmitterID, ReviewedAt: retStamp,
		ItemClosed: true, OwnerScore: ceil, SubmitterScore: ceil,
		OwnerDelta: 0, SubmitterDelta: 0,
	}
	svc := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{},
		&fakeReturnUsers{byID: map[int64]*model.User{}})

	res, err := svc.Confirm(context.Background(), retUser(retOwnerID, false, ceil), retReturnID, "")
	if err != nil {
		t.Fatalf("Confirm 失败: %v", err)
	}
	if res.CreditDelta != 0 {
		t.Fatalf("夹到顶的时候 credit_delta 必须是实际生效的 0，实际 %d", res.CreditDelta)
	}
}

// TestConfirmAbortsWhenLedgerReadFails 钉住「读台账失败 → 整件事失败」。
//
// 这条纪律很容易被「顺手降级」掉：给 confirm 加一个 try/catch 式分支，
// 台账读不到就只关帖、不发提示 —— 代码能跑、用户看到 200、测试的前半段全绿。
// 留下的是「拾主这边确认完了、当初被通知过的失主永远等不到收尾」这种不对称，
// 而它在数据库里看不出任何异常，只有当事人知道。那是最难 debug 的一类 bug，
// 所以这里断言两件事：返回错误，**且 repo.Confirm 一次都没被调**。
func TestConfirmAbortsWhenLedgerReadFails(t *testing.T) {
	store := newFakeReturnStore()
	store.byID[retReturnID] = retRecord(model.ReturnStatusPending)
	ledger := &fakeLedgerReader{err: errors.New("台账读不到")}
	svc := newReturnSvc(t, store, newFakeItemReader(retFound()), ledger,
		&fakeReturnUsers{byID: map[int64]*model.User{}})

	_, err := svc.Confirm(context.Background(), retUser(retOwnerID, false, 100), retReturnID, "")
	if err == nil {
		t.Fatalf("台账读失败时 confirm 必须整件事失败，不许降级成「只关帖不发提示」")
	}
	if store.confirmCalls != 0 {
		t.Errorf("已经决定失败了却还写了库（%d 次）—— 那会留下半个确认", store.confirmCalls)
	}
}

// TestConfirmWithEmptyLedgerStillSucceeds 是上一条的另一半：
// 「台账是空的」不是失败，而是一条完全正常的 confirm（从没匹配过的帖子被归还了）。
// 如果实现里把 len(authors)==0 也当成错误，§12 那条「从没解锁过的用户直接提交→成功」
// 的冒烟就会在 confirm 这一步挂掉。
func TestConfirmWithEmptyLedgerStillSucceeds(t *testing.T) {
	store := newFakeReturnStore()
	store.byID[retReturnID] = retRecord(model.ReturnStatusPending)
	svc := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{},
		&fakeReturnUsers{byID: map[int64]*model.User{}})

	if _, err := svc.Confirm(context.Background(), retUser(retOwnerID, false, 100), retReturnID, ""); err != nil {
		t.Fatalf("没有匹配台账的帖子照样能被确认：%v", err)
	}
	if hints := store.confirmHints[0]; len(hints) != 0 {
		t.Errorf("台账为空时不该有 hint，实际 %d 条", len(hints))
	}
	if hints := store.confirmHints[0]; hints == nil {
		t.Errorf("hints 应该是空切片而不是 nil —— repo 那边 range 它，但 nil 会让「一条都没发」和「没考虑过」在日志里长得一样")
	}
}

// TestConfirmPropagatesIllegalTransitionFromStore 并发双点：service 读的时候还是 pending，
// 写的时候已经被别人推进了。repo 那条 UPDATE 的 WHERE 会挡下并返回 409，
// service 必须原样上抛而不是包装成 500。
func TestConfirmPropagatesIllegalTransitionFromStore(t *testing.T) {
	store := newFakeReturnStore()
	store.byID[retReturnID] = retRecord(model.ReturnStatusPending)
	store.confirmErr = apperr.New(apperr.CodeReturnIllegalTransition)
	svc := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{},
		&fakeReturnUsers{byID: map[int64]*model.User{}})

	_, err := svc.Confirm(context.Background(), retUser(retOwnerID, false, 100), retReturnID, "")
	if got := codeOf(t, err); got != apperr.CodeReturnIllegalTransition {
		t.Fatalf("期望 RETURN_ILLEGAL_TRANSITION，实际 %s —— 那是并发，不是故障", got)
	}
}

// ---------- reject：唯一后果是一条通知 ----------

// TestRejectWritesOnlyTheRecordAndOneNotice 是 §3.4 那句
// 「全系统最容易误解的一点」在第①层的落地（第②层的对应断言是 TestRejectDoesNotCloseItem）。
//
// 这里能断言的是**参数面**：Reject 交给 repo 的只有 DecideRow 一个参数，
// 没有 hints、没有分数、没有帖子状态 —— 也就是说这个服务在类型层面就递不出
// 「顺便关个帖」的东西。而 repo.Reject 的签名里也没有 items/users/credit_logs，
// 两侧合起来才是「拒绝不产生后果」这句话的完整证明。
func TestRejectWritesOnlyTheRecordAndOneNotice(t *testing.T) {
	store := newFakeReturnStore()
	store.byID[retReturnID] = retRecord(model.ReturnStatusPending)
	items := newFakeItemReader(retFound())
	ledger := &fakeLedgerReader{authors: []int64{21}}
	svc := newReturnSvc(t, store, items, ledger, &fakeReturnUsers{byID: map[int64]*model.User{}})

	res, err := svc.Reject(context.Background(), retUser(retOwnerID, false, 100), retReturnID, "东西不是我的，特征对不上")
	if err != nil {
		t.Fatalf("Reject 失败: %v", err)
	}

	if store.rejectCalls != 1 {
		t.Fatalf("Reject 调了 %d 次", store.rejectCalls)
	}
	if store.confirmCalls != 0 {
		t.Errorf("拒绝却走了确认那条链：%d 次", store.confirmCalls)
	}
	// ⚠ 拒绝**不读台账**：那是 confirm 独有的连带。
	// 这条断言的意义在于「将来有人说被拒的人也该收到提示」时会先撞上这里。
	if ledger.calls != 0 {
		t.Errorf("reject 不该读台账，却读了 %d 次", ledger.calls)
	}
	row := store.rejectRows[0]
	if row.OwnerID != retOwnerID || row.OwnerNote == "" {
		t.Errorf("DecideRow 应该带上帖主 id 和他那句理由：%+v", row)
	}
	if !strings.Contains(row.NotifyBody, "特征对不上") {
		t.Errorf("给提交人的通知里必须带那句理由原文（那是他唯一的解释），实际 %q", row.NotifyBody)
	}

	if res.Status != model.ReturnStatusRejected {
		t.Errorf("data.status = %q，期望 rejected", res.Status)
	}
	// data 里**没有** credit_delta：返回一个 0 会让人以为「本来要加分但没加」。
	if keys := retTopKeys(t, res); slices.Contains(keys, "credit_delta") {
		t.Errorf("拒绝的响应里出现了 credit_delta 键：%v", keys)
	}
}

// ---------- cancel：零副作用 ----------

// TestCancelIsTheCheapestAction 把 #27 的「轻」写成断言。
//
// 三条不显眼但都是刻意的：
//   - **不读帖子**：撤销一条确认和那条帖子是什么状态无关。帖子被下架了，
//     提交人仍然有权撤掉自己那条还没被处理的记录（反过来才奇怪：
//     平台的治理动作把用户锁在了他自己的记录外面）
//   - **不读台账**、不写任何 notifications 之外的东西
//   - 响应里**没有 reviewed_at**：那个词承诺的是「有人审过了」，而没有人审过
func TestCancelIsTheCheapestAction(t *testing.T) {
	ctx := context.Background()
	store := newFakeReturnStore()
	store.byID[retReturnID] = retRecord(model.ReturnStatusPending)
	items := newFakeItemReader(retFound())
	ledger := &fakeLedgerReader{authors: []int64{21}}
	svc := newReturnSvc(t, store, items, ledger, &fakeReturnUsers{byID: map[int64]*model.User{}})

	res, err := svc.Cancel(ctx, retUser(retSubmitterID, false, 100), retReturnID)
	if err != nil {
		t.Fatalf("Cancel 失败: %v", err)
	}

	if store.cancelCalls != 1 {
		t.Fatalf("Cancel 调了 %d 次", store.cancelCalls)
	}
	if got := store.cancelArgs[0]; got[0] != retReturnID || got[1] != retSubmitterID {
		t.Errorf("repo.Cancel 应该收到 (记录 id, 提交人 id)，实际 %v", got)
	}
	if items.calls != 0 {
		t.Errorf("cancel 不该读帖子（撤销和帖子状态无关），却查了 %d 次 —— 一旦读了，帖子被删就会让撤销变成 404", items.calls)
	}
	if ledger.calls != 0 {
		t.Errorf("cancel 不该读台账，却读了 %d 次", ledger.calls)
	}
	if store.confirmCalls != 0 || store.rejectCalls != 0 {
		t.Errorf("cancel 串到别的动作上了：confirm=%d reject=%d", store.confirmCalls, store.rejectCalls)
	}

	if res.Status != model.ReturnStatusCancelled {
		t.Errorf("data.status = %q", res.Status)
	}
	if keys := retTopKeys(t, res); !slices.Equal(keys, []string{"id", "status"}) {
		t.Errorf("cancel 的 data 应该恰好是 {id, status}（计划 §4 第 27 行），实际 %v —— "+
			"多一个 reviewed_at 就是在暗示「有人审过了」", keys)
	}
}

// TestCancelPermissionAndState 覆盖 #27 的三种拒绝。
//
// 注意帖主那一格：**帖主也不能替提交人撤销**。
// 这条看起来反直觉（「我的帖子上挂着一条我没同意的确认，我却不能删掉它？」），
// 但答案是不能：那条记录是提交人的陈述，平台不替任何人收回他说过的话（原则 1）。
// 帖主该走的是 reject —— 那才是他作为接收方的动作，而且会留下 review 存证。
func TestCancelPermissionAndState(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		status string
		actor  *model.User
		want   string
	}{
		{"提交人撤销 pending", model.ReturnStatusPending, retUser(retSubmitterID, false, 100), ""},
		{"帖主不能替提交人撤销（该走 reject）", model.ReturnStatusPending, retUser(retOwnerID, false, 100), apperr.CodeForbidden},
		{"admin 不能撤销", model.ReturnStatusPending, retUser(retAdminID, true, 100), apperr.CodeForbidden},
		{"路人不能撤销", model.ReturnStatusPending, retUser(retStrangerID, false, 100), apperr.CodeForbidden},
		{"已确认的不能再撤销", model.ReturnStatusConfirmed, retUser(retSubmitterID, false, 100), apperr.CodeReturnIllegalTransition},
		{"已拒绝的不能再撤销", model.ReturnStatusRejected, retUser(retSubmitterID, false, 100), apperr.CodeReturnIllegalTransition},
		{"已撤销的不能撤销两次", model.ReturnStatusCancelled, retUser(retSubmitterID, false, 100), apperr.CodeReturnIllegalTransition},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newFakeReturnStore()
			store.byID[retReturnID] = retRecord(c.status)
			svc := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{},
				&fakeReturnUsers{byID: map[int64]*model.User{}})

			res, err := svc.Cancel(ctx, c.actor, retReturnID)
			if c.want == "" {
				if err != nil {
					t.Fatalf("期望放行，实际 %v", err)
				}
				if store.cancelCalls != 1 {
					t.Errorf("放行却没写库：%d 次", store.cancelCalls)
				}
				return
			}
			if got := codeOf(t, err); got != c.want {
				t.Fatalf("期望 %s，实际 %s（%v）", c.want, got, err)
			}
			if store.cancelCalls != 0 {
				t.Errorf("被拒的操作写了库：%d 次", store.cancelCalls)
			}
			if res != nil {
				t.Errorf("被拒时返回值应该是 nil，实际 %+v", res)
			}
		})
	}
}

// ---------- #24 详情鉴权 ----------

// TestDetailWhoMaySee 是计划 §4 第 24 行那一列（提交人 / 发帖人 / Admin）的完整覆盖。
//
// 第四格（路人）得到的是 FORBIDDEN 而不是 NOT_FOUND，这个取舍和 #22 名单一样，
// 理由在 service 的注释里：藏起来的代价是提交人拼错一个 id 会收到
// 「记录不存在」而明明是没权限，他会去翻自己到底提没提过。
// 这里把两种人各测一遍，是因为「用 404 藏权限」是这类端点最常见的实现方式
// —— 它看起来更安全，实际上把唯一合法的那三个读者也一起藏了。
func TestDetailWhoMaySee(t *testing.T) {
	ctx := context.Background()
	users := &fakeReturnUsers{byID: map[int64]*model.User{
		retSubmitterID: retUser(retSubmitterID, false, 108),
	}}

	cases := []struct {
		name  string
		actor *model.User
		want  string
	}{
		{"提交人看得见", retUser(retSubmitterID, false, 100), ""},
		{"发帖人看得见", retUser(retOwnerID, false, 100), ""},
		{"admin 看得见", retUser(retAdminID, true, 100), ""},
		{"路人看不见", retUser(retStrangerID, false, 100), apperr.CodeForbidden},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newFakeReturnStore()
			store.byID[retReturnID] = retRecord(model.ReturnStatusPending)
			svc := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{}, users)

			view, err := svc.Detail(ctx, c.actor, retReturnID)
			if c.want == "" {
				if err != nil {
					t.Fatalf("期望放行，实际 %v", err)
				}
				if view == nil {
					t.Fatalf("放行了却没有返回值")
				}
				return
			}
			if got := codeOf(t, err); got != c.want {
				t.Fatalf("期望 %s，实际 %s", got, err)
			}
		})
	}

	t.Run("记录不存在 → NOT_FOUND 且不读提交人", func(t *testing.T) {
		store := newFakeReturnStore()
		users := &fakeReturnUsers{byID: map[int64]*model.User{retSubmitterID: retUser(retSubmitterID, false, 100)}}
		svc := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{}, users)
		_, err := svc.Detail(ctx, retUser(retSubmitterID, false, 100), 999)
		if got := codeOf(t, err); got != apperr.CodeNotFound {
			t.Fatalf("期望 NOT_FOUND，实际 %s", got)
		}
		if users.calls != 0 {
			t.Errorf("记录都不存在，读提交人毫无用处却读了 %d 次", users.calls)
		}
	})

	t.Run("帖子被软删之后这条记录仍然返回（记录是事实）", func(t *testing.T) {
		store := newFakeReturnStore()
		store.byID[retReturnID] = retRecord(model.ReturnStatusPending)
		svc := newReturnSvc(t, store,
			newFakeItemReader(retItem(func(d *model.ItemDetail) { d.Status = model.ItemStatusDeleted })),
			&fakeLedgerReader{}, users)

		view, err := svc.Detail(ctx, retUser(retSubmitterID, false, 100), retReturnID)
		if err != nil {
			t.Fatalf("admin 下架那条帖子不该让提交人的记录凭空蒸发：%v", err)
		}
		if view.Item.Status != model.ItemStatusDeleted {
			t.Errorf("摘要里应如实带着 deleted（它不做判断，只记录事实），实际 %q", view.Item.Status)
		}
	})
}

// TestDetailViewKeysAndContactAlwaysLocked 断言 #24 的 data 形状。
//
// 两件事各有一个具体后果：
//   - reviewer_id / review_kind 的键必须在（值为 null 也要在）：
//     那两个 null 就是「提交人自己撤的、没有人审过」这句话的数据形式
//   - 嵌套的 item 摘要里 contact **一律 null**。这条在 M5 格外要紧：
//     #24 的读者里包括 admin，如果摘要能带 contact，
//     「admin 随手一读就看到别人的手机号」就绕过了 M4 为 #15 写下的那四条规则
func TestDetailViewKeysAndContactAlwaysLocked(t *testing.T) {
	store := newFakeReturnStore()
	r := retRecord(model.ReturnStatusCancelled)
	store.byID[retReturnID] = r // cancelled：reviewer_id / review_kind 都是 NULL
	svc := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{},
		&fakeReturnUsers{byID: map[int64]*model.User{retSubmitterID: retUser(retSubmitterID, false, 108)}})

	view, err := svc.Detail(context.Background(), retUser(retSubmitterID, false, 100), retReturnID)
	if err != nil {
		t.Fatalf("Detail 失败: %v", err)
	}

	keys := retTopKeys(t, view)
	want := []string{"id", "item", "message", "owner_note", "proof_image_url", "review_kind",
		"reviewer_id", "status", "submitted_at", "reviewed_at", "submitter"}
	for _, k := range want {
		if !slices.Contains(keys, k) {
			t.Errorf("data 里少了计划 §4 第 24 行要求的键 %q：%v", k, keys)
		}
	}
	if view.ReviewerID != nil || view.ReviewKind != nil {
		t.Errorf("cancelled 的记录里 reviewer_id / review_kind 必须是 null，实际 %v / %v",
			view.ReviewerID, view.ReviewKind)
	}

	raw, err := json.Marshal(view.Item)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "wx_owner_123") {
		t.Errorf("item 摘要泄漏了帖主的联系方式（解锁只有 #21 一条路）：%s", raw)
	}

	if !strings.HasPrefix(view.ProofImageURL, "/uploads/") {
		t.Errorf("proof_image_url 应该和 #15 的封面一样是站内 URL，实际 %q", view.ProofImageURL)
	}
	if view.Submitter.CreditScore != 108 {
		t.Errorf("提交人信用分应该带上（那是帖主判断的原料），实际 %d", view.Submitter.CreditScore)
	}
	if view.Submitter.Nickname == "" {
		t.Errorf("提交人昵称不能为空")
	}
}

// ---------- #28/#29 两个列表 ----------

// TestListsDifferOnlyByWhoseIDIsFiltered 是这个里程碑上最容易写反的一处：
// #28 筛 submitter_id，#29 筛帖子的作者（JOIN 出来的 items.user_id）。
//
// 两个条件各填错一次都不会报错：查询照样执行、形状照样合法，
// 只是「我收到的」那一页会变成「我提交的」（因为两个字段在 filter 里都是 int64，
// 传错格子在类型上完全成立）。所以这里断言的是**另一格必须是 0**。
//
// ⚠ 两个 id 都来自 JWT，测试里没有任何一条路径能让请求参数决定它们。
func TestListsDifferOnlyByWhoseIDIsFiltered(t *testing.T) {
	ctx := context.Background()
	actor := retUser(retOwnerID, false, 100)

	cases := []struct {
		name          string
		call          func(*ItemReturn) error
		wantSubmitter int64
		wantOwner     int64
	}{
		{
			name: "#28 我提交的",
			call: func(s *ItemReturn) error {
				_, err := s.ListSubmitted(ctx, actor, ReturnListQuery{})
				return err
			},
			wantSubmitter: retOwnerID, wantOwner: 0,
		},
		{
			name: "#29 我收到的",
			call: func(s *ItemReturn) error {
				_, err := s.ListReceived(ctx, actor, ReturnListQuery{})
				return err
			},
			wantSubmitter: 0, wantOwner: retOwnerID,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := newFakeReturnStore()
			svc := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{},
				&fakeReturnUsers{byID: map[int64]*model.User{}})
			if err := c.call(svc); err != nil {
				t.Fatalf("列表查询失败: %v", err)
			}
			if store.listCalls != 1 {
				t.Fatalf("调了 %d 次 ListReturns", store.listCalls)
			}
			f := store.listFilters[0]
			if f.SubmitterID != c.wantSubmitter {
				t.Errorf("SubmitterID = %d，期望 %d", f.SubmitterID, c.wantSubmitter)
			}
			if f.OwnerID != c.wantOwner {
				t.Errorf("OwnerID = %d，期望 %d", f.OwnerID, c.wantOwner)
			}
			if f.Page != 1 || f.PageSize != defaultPageSize {
				t.Errorf("分页缺省应该是 1/%d，实际 %d/%d", defaultPageSize, f.Page, f.PageSize)
			}
		})
	}
}

// TestListRejectsBadStatusBeforeQuerying 和 #14 那条同一条纪律：
// 非法 status 报 VALIDATION 并列出合法取值，而不是安静地返回空列表。
//
// 后者是这里最坏的行为：前端把一个 ?status=Confirmed 拼错成大写，
// 请求成功、响应合法、列表为空 —— 它看起来「worksWith」，能调试一整天。
func TestListRejectsBadStatusBeforeQuerying(t *testing.T) {
	ctx := context.Background()
	svc := newReturnSvc(t, newFakeReturnStore(), newFakeItemReader(retFound()), &fakeLedgerReader{},
		&fakeReturnUsers{byID: map[int64]*model.User{}})

	for _, bad := range []string{"Confirmed", "PENDING", "done", "all", " pending", "pending ", "打开"} {
		_, err := svc.ListSubmitted(ctx, retUser(retSubmitterID, false, 100), ReturnListQuery{Status: bad})
		if got := codeOf(t, err); got != apperr.CodeValidation {
			t.Errorf("?status=%q 期望 VALIDATION，实际 %s", bad, got)
		}
	}

	store := newFakeReturnStore()
	svc2 := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{},
		&fakeReturnUsers{byID: map[int64]*model.User{}})
	for _, good := range model.ReturnStatuses {
		if _, err := svc2.ListSubmitted(ctx, retUser(retSubmitterID, false, 100), ReturnListQuery{Status: good}); err != nil {
			t.Errorf("?status=%q 是白名单里的值却被拒：%v", good, err)
		}
	}
	if store.listFilters[len(store.listFilters)-1].Status == nil {
		t.Errorf("显式传了 status 却传了 nil 给 repo（等于不过滤）")
	}
	if store.listCalls != len(model.ReturnStatuses) {
		t.Errorf("白名单 %d 个值，查库 %d 次", len(model.ReturnStatuses), store.listCalls)
	}

	// 空串 = 不过滤：那一格必须是 nil，repo 那边才不会拼 WHERE r.status = ''
	if _, err := svc2.ListSubmitted(ctx, retUser(retSubmitterID, false, 100), ReturnListQuery{Status: ""}); err != nil {
		t.Fatal(err)
	}
	if store.listFilters[store.listCalls-1].Status != nil {
		t.Errorf("?status= 留空应该是 nil（不过滤），实际 %+v", *store.listFilters[store.listCalls-1].Status)
	}
}

// TestListEntryShapeHasNoJudgementMaterial 断言列表元素的键恰好是计划里那六个。
//
// 少了 message / proof_image / submitter 不是疏忽：列表是索引，详情才是判断现场。
// 这条约束在 M5 有安全含义 —— #29 的读者是帖主，他在这里看到的是「有条新的归还确认」，
// 而「所以我要点同意」那个判断必须在点进去之后才发生。
// 如果列表顺手带上凭证图和说明，前端就有理由在列表页放一个「确认」按钮，
// 于是判断发生在没看全信息的时候。
func TestListEntryShapeHasNoJudgementMaterial(t *testing.T) {
	store := newFakeReturnStore()
	store.listRows = []model.ItemReturnRow{{
		ID: retReturnID, Status: model.ReturnStatusPending, OwnerNote: "",
		SubmittedAt: retStamp,
		ItemID:      retItemID, ItemType: model.ItemTypeFound, Title: "黑色钱包",
		ItemStatus: model.ItemStatusOpen,
		CategoryID: 3, CategoryName: "证件卡类", LocationID: 5, LocationName: "图书馆",
		Contact: "wx_owner_123", CoverPath: retProofPath,
		AuthorID: retOwnerID, AuthorName: "小王", ItemCreatedAt: retStamp,
	}}
	svc := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{},
		&fakeReturnUsers{byID: map[int64]*model.User{}})

	page, err := svc.ListReceived(context.Background(), retUser(retOwnerID, false, 100), ReturnListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.List) != 1 {
		t.Fatalf("列表应有 1 条，实际 %d", len(page.List))
	}

	keys := retTopKeys(t, page.List[0])
	want := []string{"id", "item", "owner_note", "reviewed_at", "status", "submitted_at"}
	if !slices.Equal(keys, want) {
		t.Errorf("列表元素的键应该是 %v，实际 %v —— 多出来的那一个就是在给「在列表页直接点确认」开门", want, keys)
	}

	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "wx_owner_123") {
		t.Errorf("列表里泄漏了联系方式：%s", truncateForName(string(raw)))
	}
}

// TestListEmptyIsArrayNotNull 同 M2/M4 那条纪律：空结果是 []，不是 null。
func TestListEmptyIsArrayNotNull(t *testing.T) {
	svc := newReturnSvc(t, newFakeReturnStore(), newFakeItemReader(retFound()), &fakeLedgerReader{},
		&fakeReturnUsers{byID: map[int64]*model.User{}})

	page, err := svc.ListSubmitted(context.Background(), retUser(retSubmitterID, false, 100), ReturnListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"list":null`) {
		t.Errorf("空列表序列化成了 null，前端的 list.map 会当场炸：%s", raw)
	}
}

// TestListPropagatesStoreError 数据库故障原样上抛，不许伪装成空列表。
//
// 这条看起来像废话，但它挡的是一个真实存在的诱惑：给列表加一个
// `if err != nil { return 空 }` 的降级，会让一次宕机在 UI 上表现成
// 「最近没人提交归还确认」，而运维从监控上看到的是 200。
func TestListPropagatesStoreError(t *testing.T) {
	store := newFakeReturnStore()
	store.listErr = apperr.New(apperr.CodeInternal)
	svc := newReturnSvc(t, store, newFakeItemReader(retFound()), &fakeLedgerReader{},
		&fakeReturnUsers{byID: map[int64]*model.User{}})

	_, err := svc.ListReceived(context.Background(), retUser(retOwnerID, false, 100), ReturnListQuery{})
	if got := codeOf(t, err); got != apperr.CodeInternal {
		t.Fatalf("期望 INTERNAL 原样上抛，实际 %s", got)
	}
}

// ---------- 通知文案：长度与措辞 ----------

// TestNoticeCopyRespectsColumnLimits 是 §12 那条「通知文案不能撞 VARCHAR 上限」的落地。
//
// 撞上去的后果不是一条被截短的通知，而是**整条 confirm 事务回滚**：
// PostgreSQL 在 INSERT 时报 23514，而那一句话的失败会表现为
// 「拾主点了确认，得到一次莫名其妙的 500」。所以这是硬约束，
// 而唯一的测法是把每个入口都灌进**最长可能值**（标题 100、说明 1000、备注 500）。
func TestNoticeCopyRespectsColumnLimits(t *testing.T) {
	longTitle := strings.Repeat("钱", titleMaxChars)
	longMessage := strings.Repeat("归", model.ReturnMessageMaxChars)
	longNote := strings.Repeat("特", model.ReturnOwnerNoteMaxChars)
	longNick := strings.Repeat("李", 100)

	// 四种文案各配一个「灌进最长可能值」的构造函数。
	// 表格存构造函数而不是直接存结果，是因为 Go 不允许在多字段结构体里
	// 用一次多值调用填其中两格 —— 这纯粹是可编译性问题，不是取舍。
	cases := []struct {
		name  string
		build func() (string, string)
	}{
		{"return_submitted", func() (string, string) {
			return returnSubmittedNotice(retItem(func(d *model.ItemDetail) { d.Title = longTitle }), longNick, longMessage)
		}},
		{"return_confirmed", func() (string, string) {
			return returnConfirmedNotice(retItem(func(d *model.ItemDetail) { d.Title = longTitle }))
		}},
		{"return_rejected", func() (string, string) {
			return returnRejectedNotice(retItem(func(d *model.ItemDetail) { d.Title = longTitle }), longNote)
		}},
		{"item_returned_hint", func() (string, string) {
			return returnedHintNotice(retItem(func(d *model.ItemDetail) { d.Title = longTitle }))
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			title, body := c.build()
			if n := utf8.RuneCountInString(title); n > 100 {
				t.Errorf("notifications.title 是 VARCHAR(100)（数的是字符），这里 %d 个字符：%q", n, title)
			}
			if n := utf8.RuneCountInString(body); n > 500 {
				t.Errorf("notifications.content 是 VARCHAR(500)，这里 %d 个字符，撞上去整条事务回滚：%q", n, body)
			}
			if title == "" || body == "" {
				t.Errorf("文案不能为空")
			}
		})
	}
}

// TestNoticeCopyMakesNoPlatformPromise 钉住定位原则 1 那条措辞纪律。
//
// 通知只说「某人做某事」这个事实，不替平台承诺后果。三个被禁的措辞各有一个具体的坑：
//   - 「帖子已关闭」：关帖那句 SQL 带 `AND status='open'`，本来就是 closed/deleted 的
//     帖子不会被再关一次，所以那不是必然发生的事实
//   - 「仍然展示」：那条帖子可能已经被下架，而「仍然展示」是一个平台保证
//   - 「这就是你的东西」/「归还成功」：那是归属判断，平台不做（原则 1）
//
// 按字符串匹配测文案确实脆，但这里匹配的是**禁止出现**的那几个词，
// 而不是整句 —— 改措辞不会让它红，只有开始替平台许诺才会。
func TestNoticeCopyMakesNoPlatformPromise(t *testing.T) {
	d := retFound()
	note := "东西不是我的"

	all := []string{
		retJoin(returnSubmittedNotice(d, "小李", "在图书馆三楼捡到的")),
		retJoin(returnConfirmedNotice(d)),
		retJoin(returnRejectedNotice(d, note)),
		retJoin(returnedHintNotice(d)),
	}

	banned := []string{"已关闭", "仍然展示", "这就是", "归还成功", "已归还给你", "判定"}
	for _, text := range all {
		for _, word := range banned {
			if strings.Contains(text, word) {
				t.Errorf("通知文案里出现了 %q，那是在替平台许诺或裁决归属：%q", word, text)
			}
		}
	}

	// 反过来，必须出现的是「谁做了什么」这个事实 + 一句让当事人自己去核对的话。
	confirmed := retJoin(returnConfirmedNotice(d))
	if !strings.Contains(confirmed, "发帖人") {
		t.Errorf("return_confirmed 应该说清是**发帖人**确认的（不是平台确认的）：%q", confirmed)
	}
	hint := retJoin(returnedHintNotice(d))
	if !strings.Contains(hint, "已找到") {
		t.Errorf("item_returned_hint 的职责就是把失主叫去点他自己那个「已找到」（§14-4），实际：%q", hint)
	}
}

// TestReturnedHintIsSameCopyForEveryAuthor 解释为什么 hint 文案不带收件人姓名：
// 一次 confirm 要给 N 个作者发通知，而那条拾物帖在这里只有一张脸。
// 如果文案里混入「你发布的失物」这种指向收件人的词，
// 有两条失物帖的作者会收到两条措辞相同、指向不同自己帖子的通知 —— 那正是 §16.2
// 要避免的「不知道该点哪一条」。所以 hint 只描述那条 found 帖。
func TestReturnedHintIsSameCopyForEveryAuthor(t *testing.T) {
	d := retFound()
	title, body := returnedHintNotice(d)
	for i := range 3 {
		gotTitle, gotBody := returnedHintNotice(d)
		if gotTitle != title || gotBody != body {
			t.Fatalf("第 %d 个作者的文案和第 0 个不同，说明它把收件人拼进去了", i)
		}
	}
	if strings.Contains(body, "小李") || strings.Contains(body, "你发布的") {
		t.Errorf("hint 文案里出现了收件人相关的词：%q", body)
	}
}

// ---------- 辅助 ----------

// retTopKeys 返回一个值序列化成 JSON 之后的顶层键名，按字典序。
// 用「键集合」而不是逐字段反射，是因为计划里写的就是键名，
// 而键名是前端会直接依赖的契约（多一个键、少一个键都是契约变更）。
func retTopKeys(t *testing.T, v any) []string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("data 不是一个 JSON 对象: %v\n%s", err, raw)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func retJoin(a, b string) string { return a + "|" + b }

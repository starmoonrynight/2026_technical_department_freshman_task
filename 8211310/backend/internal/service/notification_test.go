package service

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
	"lostfound/internal/repo"
)

// 这个文件接着 contact_test.go 做 M4 的第①层（不起数据库），管 #30–#32 这一块。
//
// 收件箱三件事里只有 #32 有业务规则，而它的规则全是**否定式**的：
// 形状不对不许查库、里面有别人的 id 就不许改、all 那条分支不该做归属检查。
// 「没发生」在集成测试里几乎看不出来（表最终状态是一样的），
// 所以这个文件的核心手法是：fake 记下每一次调用，测试断言次数为 0。

// fakeNoteStore 实现 NotificationStore 的四个方法，并把每次调用的参数留下来。
type fakeNoteStore struct {
	listCalls int
	listF     repo.NotificationFilter
	listRows  []model.Notification
	listTotal int
	listErr   error

	unreadCalls int
	unreadUser  int64
	unreadRet   int
	unreadErr   error

	foreignCalls int
	foreignIDs   []int64
	foreignUser  int64
	foreignRet   int
	foreignErr   error

	markCalls int
	markUser  int64
	// markIDs 直接存传进来的切片：**nil 和空切片是两条不同的 SQL**
	// （nil 走「这个人全部未读」，空切片会命中 service 层的 VALIDATION），
	// 所以这里必须原样留着，让测试能区分。
	markIDs []int64
	markRet int
	markErr error
}

func (f *fakeNoteStore) List(_ context.Context, fl repo.NotificationFilter) ([]model.Notification, int, error) {
	f.listCalls++
	f.listF = fl
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.listRows, f.listTotal, nil
}

func (f *fakeNoteStore) UnreadCount(_ context.Context, userID int64) (int, error) {
	f.unreadCalls++
	f.unreadUser = userID
	if f.unreadErr != nil {
		return 0, f.unreadErr
	}
	return f.unreadRet, nil
}

func (f *fakeNoteStore) CountForeign(_ context.Context, userID int64, ids []int64) (int, error) {
	f.foreignCalls++
	f.foreignUser = userID
	f.foreignIDs = ids
	if f.foreignErr != nil {
		return 0, f.foreignErr
	}
	return f.foreignRet, nil
}

func (f *fakeNoteStore) MarkRead(_ context.Context, userID int64, ids []int64) (int, error) {
	f.markCalls++
	f.markUser = userID
	f.markIDs = ids
	if f.markErr != nil {
		return 0, f.markErr
	}
	return f.markRet, nil
}

// 编译期证据：NotificationStore 只有这四个方法。
// 想给收件箱加第五个能力（尤其是写通知）得先动这个 var，
// 而撞上它的人就会读到 contact_test.go 里那条反射测试。
var _ NotificationStore = (*fakeNoteStore)(nil)

func noteFixture(id int64, read bool, itemID *int64) model.Notification {
	return model.Notification{
		ID:        id,
		UserID:    3,
		Type:      model.NotificationNewMatch,
		Title:     "有人捡到了相似的东西",
		ItemID:    itemID,
		IsRead:    read,
		CreatedAt: time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC),
	}
}

// ptr 把一个字面量变成指针。MarkReadInput 的两个字段都是指针，
// 而「nil」和「指向零值」的语义完全不同（见 notification.go 里那段注释），
// 所以测试里必须能写出「指向 false」这种请求。
func ptr[T any](v T) *T { return &v }

// ---------- #30 收件箱 ----------

// TestParseIsReadParam 钉住「只认 true/false 两种写法」。
//
// 1/0/yes 这些宽容写法在这里是**有害的**而不是无害的：
// 一个被静默当成「不过滤」的 is_read=1，用户看到的是「我勾了只看未读，
// 结果列表一条没少」，而响应是 200，日志里什么都没有。
func TestParseIsReadParam(t *testing.T) {
	tc := []struct {
		raw     string
		wantNil bool
		wantVal bool
		wantErr bool
	}{
		{raw: "", wantNil: true},
		{raw: "   ", wantNil: true},
		{raw: "true", wantVal: true},
		{raw: "TRUE", wantVal: true},
		{raw: " true ", wantVal: true},
		{raw: "false", wantVal: false},
		{raw: "False", wantVal: false},
		{raw: "1", wantErr: true},
		{raw: "0", wantErr: true},
		{raw: "yes", wantErr: true},
		{raw: "unread", wantErr: true},
	}

	for _, c := range tc {
		got, err := parseIsReadParam(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("is_read=%q 该报错，结果放行了（会被当成「不过滤」）", c.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("is_read=%q 不该报错：%v", c.raw, err)
			continue
		}
		if c.wantNil {
			if got != nil {
				t.Errorf("is_read=%q 期望 nil（不过滤），实际 %v", c.raw, *got)
			}
			continue
		}
		if got == nil {
			t.Fatalf("is_read=%q 期望指向 %v，实际 nil（三态里的两态被合并了）", c.raw, c.wantVal)
		}
		if *got != c.wantVal {
			t.Errorf("is_read=%q 期望 %v，实际 %v", c.raw, c.wantVal, *got)
		}
	}
}

// TestListMineFilter 是 #30 的全部规则：userID 来自调用方而不是参数、
// is_read 三态原样透传、分页走统一默认值、坏参数在查库之前就拒掉。
func TestListMineFilter(t *testing.T) {
	unread := false
	now := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	id42 := int64(42)

	tc := []struct {
		name    string
		q       InboxQuery
		wantErr string
		filter  *repo.NotificationFilter
	}{
		{
			name: "什么都不带 → 不按已读筛、第 1 页 20 条",
			q:    InboxQuery{},
			filter: &repo.NotificationFilter{
				UserID: 3, IsRead: nil, Page: defaultPage, PageSize: defaultPageSize,
			},
		},
		{
			name: "is_read=false → 只看未读（指针必须是非 nil 的 false）",
			q:    InboxQuery{IsRead: "false"},
			filter: &repo.NotificationFilter{
				UserID: 3, IsRead: &unread, Page: defaultPage, PageSize: defaultPageSize,
			},
		},
		{
			name: "is_read=true → 只看已读",
			q:    InboxQuery{IsRead: "true"},
			filter: &repo.NotificationFilter{
				UserID: 3, IsRead: func() *bool { b := true; return &b }(),
				Page: defaultPage, PageSize: defaultPageSize,
			},
		},
		{
			name: "分页参数原样透传",
			q:    InboxQuery{Page: "3", PageSize: "5"},
			filter: &repo.NotificationFilter{
				UserID: 3, IsRead: nil, Page: 3, PageSize: 5,
			},
		},
		{
			name:    "page_size 超过全站上限 → VALIDATION",
			q:       InboxQuery{PageSize: "99999"},
			wantErr: apperr.CodeValidation,
		},
		{
			name:    "is_read 写了 1 → VALIDATION 而不是当成不过滤",
			q:       InboxQuery{IsRead: "1"},
			wantErr: apperr.CodeValidation,
		},
		{
			name:    "page 是字母 → VALIDATION",
			q:       InboxQuery{Page: "abc"},
			wantErr: apperr.CodeValidation,
		},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeNoteStore{listRows: []model.Notification{
				noteFixture(1, false, &id42),
				noteFixture(2, true, nil),
			}, listTotal: 47}
			svc := NewNotification(store, testLogger)

			got, err := svc.ListMine(context.Background(), 3, c.q)
			if c.wantErr != "" {
				if codeOf(t, err) != c.wantErr {
					t.Fatalf("期望 %s，实际 %s（%v）", c.wantErr, codeOf(t, err), err)
				}
				// ⚠ 参数不合格时一次库都不该查。这条断言的意义是顺序：
				// 先解析再查，反过来的话一个 ?page_size=99999 就能让收件箱把全表捞出来。
				if store.listCalls != 0 {
					t.Errorf("参数不合法却查了库：%d 次", store.listCalls)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v", err)
			}

			f := store.listF
			if f.UserID != c.filter.UserID {
				t.Errorf("user_id 期望从调用方拿（%d），实际 %d", c.filter.UserID, f.UserID)
			}
			if (f.IsRead == nil) != (c.filter.IsRead == nil) {
				t.Fatalf("is_read 的三态被压平了：期望 %v，实际 %v", c.filter.IsRead, f.IsRead)
			}
			if f.IsRead != nil && *f.IsRead != *c.filter.IsRead {
				t.Errorf("is_read 期望 %v，实际 %v", *c.filter.IsRead, *f.IsRead)
			}
			if f.Page != c.filter.Page || f.PageSize != c.filter.PageSize {
				t.Errorf("分页期望 (%d,%d)，实际 (%d,%d)",
					c.filter.Page, c.filter.PageSize, f.Page, f.PageSize)
			}

			// 响应里带的是请求解析后的分页值和 total，前端靠这两个算总页数。
			if got.Total != 47 || got.Page != c.filter.Page || got.PageSize != c.filter.PageSize {
				t.Errorf("分页回显不对：%+v", got)
			}
			if len(got.List) != 2 {
				t.Fatalf("两行通知必须都转成视图，实际 %d 行", len(got.List))
			}
			// created_at 必须是带时区偏移的 RFC3339（全站同一个时间形状，M2 定的）。
			if got.List[0].CreatedAt != now.Format(time.RFC3339) {
				t.Errorf("created_at 形状不对：%q", got.List[0].CreatedAt)
			}
		})
	}
}

// TestListMineEmptyIsArrayNotNull 断言没有通知时 list 是 []（键在、空数组），
// 而不是 null。前端写 list.map(...) 遇到 null 会直接崩，
// 而「一个新用户收件箱是空的」是这条路径每天最常见的一次调用。
func TestListMineEmptyIsArrayNotNull(t *testing.T) {
	store := &fakeNoteStore{listRows: []model.Notification{}}
	svc := NewNotification(store, testLogger)

	got, err := svc.ListMine(context.Background(), 3, InboxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if got.List == nil {
		t.Fatal("空收件箱的 List 是 nil 切片，marshal 出来是 null")
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	l, ok := m["list"].([]any)
	if !ok {
		t.Fatalf("list 不是数组：%s", raw)
	}
	if len(l) != 0 {
		t.Errorf("期望空数组，实际 %v", m["list"])
	}
}

// TestListMineKeepsNoRecipientField 是 #30 的隐私边界：
// 收件箱的每一行不该带着「这是发给谁的」这个 user_id 出去。
// 它现在结构上就不可能（NotificationView 没这个字段），
// 但这条测试的作用是：将来谁把它加回去（多半是为了前端方便），这里会红。
func TestListMineKeepsNoRecipientField(t *testing.T) {
	store := &fakeNoteStore{listRows: []model.Notification{noteFixture(1, false, nil)}, listTotal: 1}
	svc := NewNotification(store, testLogger)

	got, err := svc.ListMine(context.Background(), 3, InboxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got.List[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["user_id"]; ok {
		t.Errorf("响应里出现了 user_id（收件人不该暴露给前端）：%s", raw)
	}
}

// TestListMinePropagatesStoreError 保证 repo 的错误码不会被这层改写。
func TestListMinePropagatesStoreError(t *testing.T) {
	boom := apperr.New(apperr.CodeInternal)
	svc := NewNotification(&fakeNoteStore{listErr: boom}, testLogger)

	if _, err := svc.ListMine(context.Background(), 3, InboxQuery{}); !errors.Is(err, boom) {
		t.Fatalf("期望透传 repo 的错误，实际 %v", err)
	}
}

// ---------- #31 未读数 ----------

func TestUnreadCountReturnsNumber(t *testing.T) {
	for _, n := range []int{0, 7} {
		store := &fakeNoteStore{unreadRet: n}
		svc := NewNotification(store, testLogger)

		got, err := svc.UnreadCount(context.Background(), 3)
		if err != nil {
			t.Fatal(err)
		}
		if got.Count != n {
			t.Errorf("count 期望 %d，实际 %d", n, got.Count)
		}
		if store.unreadUser != 3 {
			t.Errorf("未读数必须按当前用户算，实际查了 user=%d", store.unreadUser)
		}

		// 0 必须是 0 而不是键消失（铃铛上要显示「没有未读」）。
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != `{"count":`+strconv.Itoa(n)+`}` {
			t.Errorf("#31 的 data 形状只能是 {count}，实际 %s", raw)
		}
	}
}

func TestUnreadCountPropagatesError(t *testing.T) {
	boom := errors.New("connection reset")
	svc := NewNotification(&fakeNoteStore{unreadErr: boom}, testLogger)

	if _, err := svc.UnreadCount(context.Background(), 3); !errors.Is(err, boom) {
		t.Fatalf("实际 %v", err)
	}
}

// ---------- #32 标记已读 ----------

// TestMarkReadRejectsShapeWithoutTouchingStore 是 §4「ids 和 all 二选一」的完整反面清单。
//
// 每种被拒的形状都同时断言「一次库都没查」：#32 是一个写接口，
// 一个形状就不对的请求如果在拒绝之前先查了归属，它就有了探测能力
// （用响应耗时区分「这些 id 是不是别人的」）。
func TestMarkReadRejectsShapeWithoutTouchingStore(t *testing.T) {
	longIDs := make([]int64, maxMarkReadIDs+1)
	for i := range longIDs {
		longIDs[i] = int64(i + 1)
	}
	three := []int64{1, 2, 3}
	noIDs := []int64{}
	f := false
	tr := true

	tc := []struct {
		name string
		in   MarkReadInput
	}{
		{
			// 都带：两种解释（先 ids 再 all / all 覆盖 ids）后端猜不出来，
			// 而猜错的后果是多标了用户没打算读的通知 —— 已读不可逆。
			name: "ids 和 all 都带",
			in:   MarkReadInput{IDs: &three, All: &tr},
		},
		{
			// 空请求体不是「什么都不做」：当成 no-op 会返回 200 + updated_count:0，
			// 用户以为已经标记过了。
			name: "两个都不带（空请求体）",
			in:   MarkReadInput{},
		},
		{
			// all:false 既不是「标记 0 条」，更不是「全部标成未读」——系统里没这个操作。
			name: "all 是 false",
			in:   MarkReadInput{All: &f},
		},
		{
			// ids:[] 和「没带 ids」的区别正是那两个指针字段存在的理由。
			name: "ids 是空数组",
			in:   MarkReadInput{IDs: &noIDs},
		},
		{
			name: "ids 超过 " + strconv.Itoa(maxMarkReadIDs) + " 个",
			in:   MarkReadInput{IDs: &longIDs},
		},
		{
			name: "ids 里有 0",
			in:   MarkReadInput{IDs: ptr([]int64{1, 0})},
		},
		{
			name: "ids 里有负数",
			in:   MarkReadInput{IDs: ptr([]int64{-5})},
		},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeNoteStore{}
			svc := NewNotification(store, testLogger)

			got, err := svc.MarkRead(context.Background(), 3, c.in)
			if codeOf(t, err) != apperr.CodeValidation {
				t.Fatalf("期望 VALIDATION，实际 %s（%v）", codeOf(t, err), err)
			}
			if got != nil {
				t.Errorf("被拒的请求不该返回结果：%+v", got)
			}
			if store.foreignCalls != 0 || store.markCalls != 0 {
				t.Errorf("形状就不对时不该碰数据库：CountForeign %d 次、MarkRead %d 次",
					store.foreignCalls, store.markCalls)
			}
		})
	}
}

// TestMarkReadBlocksForeignBeforeWriting 是 #32 的越权防线（第一层）。
func TestMarkReadBlocksForeignBeforeWriting(t *testing.T) {
	store := &fakeNoteStore{foreignRet: 1}
	svc := NewNotification(store, testLogger)

	got, err := svc.MarkRead(context.Background(), 3, MarkReadInput{IDs: ptr([]int64{1, 2})})
	if codeOf(t, err) != apperr.CodeForbidden {
		t.Fatalf("期望 FORBIDDEN，实际 %s（%v）", codeOf(t, err), err)
	}
	if got != nil {
		t.Errorf("越权请求不该有任何写入结果：%+v", got)
	}
	// ⚠ 这条是本测试的重点：归属检查失败之后 UPDATE 一次都没发。
	// 只靠 UPDATE 的 WHERE user_id 兜底的话，越权那部分会被静默跳过，
	// 用户看到 updated_count 比预期小却不知道原因。
	if store.markCalls != 0 {
		t.Errorf("CountForeign 说有别人的通知，却还是执行了 UPDATE：%d 次", store.markCalls)
	}
	if store.foreignUser != 3 {
		t.Errorf("归属检查算错了人：user=%d", store.foreignUser)
	}
	if len(store.foreignIDs) != 2 || store.foreignIDs[0] != 1 {
		t.Errorf("归属检查用的 id 不是请求里那批：%v", store.foreignIDs)
	}
}

// TestMarkReadAllSkipsOwnershipCheck 断言 all=true 那条分支**不发** CountForeign。
//
// 看起来像漏测，其实是必要的反向断言：全部标记的作用范围本来就是
// 「这个人自己的所有未读」（repo 的 WHERE 里带着 user_id），
// 拿一批 id 去查归属这件事在这条分支上没有对象。
// 如果哪天有人「统一一下」把 all 翻译成「先查出所有 id 再逐条标」，
// 那就既多两次查询、又会漏掉翻页之间新到达的通知 —— 这条测试会红。
func TestMarkReadAllSkipsOwnershipCheck(t *testing.T) {
	store := &fakeNoteStore{markRet: 5}
	svc := NewNotification(store, testLogger)

	got, err := svc.MarkRead(context.Background(), 3, MarkReadInput{All: ptr(true)})
	if err != nil {
		t.Fatal(err)
	}
	if store.foreignCalls != 0 {
		t.Errorf("all=true 没有 id 可查归属，却查了 %d 次", store.foreignCalls)
	}
	// ⚠ 传给 repo 的必须是 nil（那是「全部未读」那条 SQL 的开关），
	// 换成空切片就变成 `id = ANY('{}')` —— 一条什么都不改的 UPDATE，返回 0。
	if store.markIDs != nil {
		t.Errorf("all=true 必须把 nil 交给 repo，实际 %v", store.markIDs)
	}
	if store.markUser != 3 {
		t.Errorf("全部标记也必须只作用于当前用户，实际 user=%d", store.markUser)
	}
	if got.UpdatedCount != 5 {
		t.Errorf("updated_count 该原样透传真正改动的行数，期望 5，实际 %d", got.UpdatedCount)
	}
}

// TestMarkReadUpdatedCountIsTrueRowsAffected 钉住 updated_count 的语义：
// 它是「真的从未读变成已读的行数」，不是「你提交了几个 id」。
// 同一批 id 连着标两次，第二次必须是 0 —— 那个 0 是幂等的证据。
func TestMarkReadUpdatedCountIsTrueRowsAffected(t *testing.T) {
	store := &fakeNoteStore{}
	svc := NewNotification(store, testLogger)
	ids := ptr([]int64{1, 2, 3})

	store.markRet = 3
	first, err := svc.MarkRead(context.Background(), 3, MarkReadInput{IDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	if first.UpdatedCount != 3 {
		t.Errorf("第一次期望 3，实际 %d", first.UpdatedCount)
	}

	store.markRet = 0
	second, err := svc.MarkRead(context.Background(), 3, MarkReadInput{IDs: ids})
	if err != nil {
		t.Fatalf("重复标记不是错误，必须 200：%v", err)
	}
	if second.UpdatedCount != 0 {
		t.Errorf("第二次期望 0（幂等的证据），实际 %d", second.UpdatedCount)
	}
	if store.markCalls != 2 {
		t.Errorf("两次都该执行 UPDATE：%d 次", store.markCalls)
	}
}

func TestMarkReadPropagatesStoreErrors(t *testing.T) {
	t.Run("CountForeign 报错", func(t *testing.T) {
		boom := errors.New("connection reset")
		store := &fakeNoteStore{foreignErr: boom}
		svc := NewNotification(store, testLogger)

		if _, err := svc.MarkRead(context.Background(), 3, MarkReadInput{IDs: ptr([]int64{1})}); !errors.Is(err, boom) {
			t.Fatalf("实际 %v", err)
		}
		// 归属检查失败时绝不猜测「大概都是自己的」就往下改。
		if store.markCalls != 0 {
			t.Errorf("归属没查成却改了库：%d 次", store.markCalls)
		}
	})

	t.Run("MarkRead 报错", func(t *testing.T) {
		boom := apperr.New(apperr.CodeInternal)
		store := &fakeNoteStore{markErr: boom}
		svc := NewNotification(store, testLogger)

		if _, err := svc.MarkRead(context.Background(), 3, MarkReadInput{All: ptr(true)}); !errors.Is(err, boom) {
			t.Fatalf("实际 %v", err)
		}
	})
}

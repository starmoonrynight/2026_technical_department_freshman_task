package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
	"lostfound/internal/repo"
)

// testLogger 是本文件所有被测服务的日志出口。
//
// #21 解锁成功会打一条 Info（那是全站唯一能带 request_id 回答
// 「谁在什么时候解锁了谁的联系方式」的地方），测试里不接掉就会在每次跑单测时
// 往终端刷一行，把真正要看的失败信息冲掉。
var testLogger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))

// 这个文件是 M4 的第①层测试（§10：不起数据库）。
//
// M4 最该被钉死的三件事恰好都不需要数据库：
//  1. #21 的**判断顺序**（lost 帖该得 VALIDATION 而不是 ITEM_CLOSED；作者本人不该产生一行解锁记录）
//  2. #15 的**fail closed**（查 contact_views 失败时必须锁着，而不是放行）
//  3. #22 的**鉴权**（只有发帖人和 admin 能看到真实姓名名单）
//
// 三件事在集成测试里也测得到，但只有在这里才能同时断言「**没调过**那个方法」——
// 数据库只能看到最终状态，看不到中间有没有写。

// ---------- 夹具 ----------

type fakeContactStore struct {
	unlockCalls int
	unlockArgs  [][2]int64 // 每次调用的 (itemID, userID)
	unlockAt    time.Time
	unlockNew   bool // repo 返回的 created
	unlockErr   error

	listCalls int
	listPage  int
	listSize  int
	listRows  []model.ContactView
	listTotal int
	listErr   error
}

func (f *fakeContactStore) Unlock(_ context.Context, itemID, userID int64) (time.Time, bool, error) {
	f.unlockCalls++
	f.unlockArgs = append(f.unlockArgs, [2]int64{itemID, userID})
	if f.unlockErr != nil {
		return time.Time{}, false, f.unlockErr
	}
	at := f.unlockAt
	if at.IsZero() {
		at = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	}
	return at, f.unlockNew, nil
}

func (f *fakeContactStore) List(_ context.Context, _ int64, page, pageSize int) ([]model.ContactView, int, error) {
	f.listCalls++
	f.listPage, f.listSize = page, pageSize
	if f.listErr != nil {
		return nil, 0, f.listErr
	}
	return f.listRows, f.listTotal, nil
}

// fakeViewed 是 #15 那一次点查的接缝。calls 记次数，为的是断言
// 「lost 帖 / 匿名 / 作者本人这三条路径一次查询都不发」——
// 那不只是省一点开销：那条查询发不发，决定了 #15 这个公开接口的形状。
type fakeViewed struct {
	viewed bool
	err    error
	calls  int
	gotID  [2]int64
}

func (f *fakeViewed) Viewed(_ context.Context, itemID, userID int64) (bool, error) {
	f.calls++
	f.gotID = [2]int64{itemID, userID}
	if f.err != nil {
		return false, f.err
	}
	return f.viewed, nil
}

func itemFixture(ownerID int64, itemType, status string) *model.ItemDetail {
	return &model.ItemDetail{
		Item: model.Item{
			ID:       7,
			UserID:   ownerID,
			ItemType: itemType,
			Status:   status,
			Contact:  "wx_owner_123",
			Title:    "黑色钱包",
		},
	}
}

func userFixture(id int64, admin bool) *model.User {
	role := model.RoleUser
	if admin {
		role = model.RoleAdmin
	}
	return &model.User{ID: id, Nickname: "小王", Role: role, Status: model.UserStatusActive}
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatalf("期望一个错误，结果 err == nil")
	}
	var e *apperr.Error
	if !errors.As(err, &e) {
		t.Fatalf("错误不是 apperr： %#v", err)
	}
	return e.Code
}

// ---------- #21 解锁 ----------

// TestUnlockDecisionOrder 钉住 §4 那四行规则在 #21 上的落地顺序。
//
// 每条 case 都同时断言两件事：**错误码**，和**有没有往 contact_views 写行**。
// 后者比前者更重要：把 lost 帖的错误码搞对但顺手写了一行，
// 集成测试要数表才看得出来，而这里一句 unlockCalls==0 就够了。
func TestUnlockDecisionOrder(t *testing.T) {
	owner := userFixture(3, false)
	stranger := userFixture(4, false)

	tc := []struct {
		name    string
		item    *model.ItemDetail
		viewer  *model.User
		wantErr string // "" 表示不报错
		writes  int    // 期望的 Unlock 调用次数
	}{
		{
			name:   "open 的 found 帖、别人来看 → 放行并写一行",
			item:   itemFixture(3, model.ItemTypeFound, model.ItemStatusOpen),
			viewer: stranger, writes: 1,
		},
		{
			name:    "closed 的 found 帖 → ITEM_CLOSED，且不写行",
			item:    itemFixture(3, model.ItemTypeFound, model.ItemStatusClosed),
			viewer:  stranger,
			wantErr: apperr.CodeItemClosed, writes: 0,
		},
		{
			// ⚠ 这条是「顺序」测试：deleted 必须排在类型判断前面。
			// 排错了的话，一条被下架的 lost 帖会得到 VALIDATION，
			// 而 VALIDATION 的语义是「你请求得不对，换个请求再来」——
			// 那等于对外确认了这条帖子存在过（和 M1 登录不区分用户名/密码错误同一条理由）。
			name:    "deleted 的 found 帖 → NOT_FOUND（哪怕它是 found 且别人在看）",
			item:    itemFixture(3, model.ItemTypeFound, model.ItemStatusDeleted),
			viewer:  stranger,
			wantErr: apperr.CodeNotFound, writes: 0,
		},
		{
			// ⚠ 这条钉住「② 排在 ④ 前面」：closed 的 lost 帖要得 VALIDATION。
			// 得到 ITEM_CLOSED 的话，用户会以为是帖子的问题，
			// 于是去重新开帖、再点解锁、还是失败 —— 一个死循环。
			name:    "closed 的 lost 帖 → VALIDATION 而不是 ITEM_CLOSED",
			item:    itemFixture(3, model.ItemTypeLost, model.ItemStatusClosed),
			viewer:  stranger,
			wantErr: apperr.CodeValidation, writes: 0,
		},
		{
			name:    "open 的 lost 帖 → VALIDATION（联系方式本来就公开，不需要解锁）",
			item:    itemFixture(3, model.ItemTypeLost, model.ItemStatusOpen),
			viewer:  stranger,
			wantErr: apperr.CodeValidation, writes: 0,
		},
		{
			// 作者看自己的帖子：不报错、给联系方式、**不写行**。
			// 这条的 writes:0 是本文件最重要的一次断言，理由写在 Unlock 的 ③ 那段注释里：
			// 写进去就是「作者解锁了自己的帖子」这条假事实，而 #22 那份名单会因此混进作者。
			name:   "作者本人看自己的 found 帖 → 放行但不写解锁记录",
			item:   itemFixture(3, model.ItemTypeFound, model.ItemStatusOpen),
			viewer: owner, writes: 0,
		},
		{
			name:   "作者本人看自己一条 closed 的 found 帖 → 仍然放行",
			item:   itemFixture(3, model.ItemTypeFound, model.ItemStatusClosed),
			viewer: owner, writes: 0,
		},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeContactStore{}
			svc := NewContact(store, fakeItemGetter{byID: map[int64]*model.ItemDetail{7: c.item}}, testLogger)

			got, err := svc.Unlock(context.Background(), c.viewer, 7)
			if c.wantErr != "" {
				if codeOf(t, err) != c.wantErr {
					t.Fatalf("错误码期望 %s，实际 %s（%v）", c.wantErr, codeOf(t, err), err)
				}
				if got != nil {
					t.Errorf("报错时不该返回结果：%+v", got)
				}
			} else if err != nil {
				t.Fatalf("不该报错：%v", err)
			}

			if store.unlockCalls != c.writes {
				t.Errorf("contact_views 写入次数期望 %d，实际 %d", c.writes, store.unlockCalls)
			}
		})
	}
}

// TestUnlockUnknownItemPropagatesNotFound 断言「帖子不存在」这件事
// 是从 repo 透传上来的 NOT_FOUND，而不是被 #21 改成别的码。
func TestUnlockUnknownItemPropagatesNotFound(t *testing.T) {
	store := &fakeContactStore{}
	svc := NewContact(store, fakeItemGetter{}, testLogger)

	if _, err := svc.Unlock(context.Background(), userFixture(4, false), 999); codeOf(t, err) != apperr.CodeNotFound {
		t.Fatalf("期望 NOT_FOUND，实际 %v", err)
	}
	if store.unlockCalls != 0 {
		t.Errorf("帖子都没查到，不该往 contact_views 写：%d 次", store.unlockCalls)
	}
}

// TestUnlockResponseShape 把 #21 响应的三种情况分开测：
// 第一次解锁、重复解锁、作者本人。三者的 contact 都一样，
// 但 unlocked_at / already_unlocked 必须能让人看出是哪一种。
func TestUnlockResponseShape(t *testing.T) {
	first := time.Date(2026, 10, 7, 9, 30, 0, 0, time.UTC)

	t.Run("第一次解锁", func(t *testing.T) {
		store := &fakeContactStore{unlockAt: first, unlockNew: true}
		svc := NewContact(store, fakeItemGetter{byID: map[int64]*model.ItemDetail{
			7: itemFixture(3, model.ItemTypeFound, model.ItemStatusOpen),
		}}, testLogger)

		got, err := svc.Unlock(context.Background(), userFixture(4, false), 7)
		if err != nil {
			t.Fatalf("%v", err)
		}
		if got.Contact != "wx_owner_123" {
			t.Errorf("contact 期望给出来，实际 %q", got.Contact)
		}
		if got.AlreadyUnlocked {
			t.Error("第一次解锁不该 already_unlocked=true")
		}
		if got.UnlockedAt != first.Format(time.RFC3339) {
			t.Errorf("unlocked_at 期望 %s，实际 %q", first.Format(time.RFC3339), got.UnlockedAt)
		}
		// 方向也在这里查：写的是 (这条帖子, 我)，反过来集成测试也是绿的。
		if store.unlockArgs[0] != [2]int64{7, 4} {
			t.Errorf("解锁记录的对象写反了：%+v", store.unlockArgs[0])
		}
	})

	t.Run("重复解锁返回同一个时刻", func(t *testing.T) {
		// repo 那边冲突时回读的是**第一次**那一行的 created_at，
		// 所以 created=false、at=first。这里验的是 service 把 created 翻译成了
		// already_unlocked，而不是拿 time.Now() 去填 unlocked_at ——
		// 后者会让那份审计日志看不出「这个人到底哪天开始盯上这条帖子的」。
		store := &fakeContactStore{unlockAt: first, unlockNew: false}
		svc := NewContact(store, fakeItemGetter{byID: map[int64]*model.ItemDetail{
			7: itemFixture(3, model.ItemTypeFound, model.ItemStatusOpen),
		}}, testLogger)

		got, err := svc.Unlock(context.Background(), userFixture(4, false), 7)
		if err != nil {
			t.Fatalf("%v", err)
		}
		if !got.AlreadyUnlocked {
			t.Error("repo 说没新建行，service 必须报 already_unlocked=true")
		}
		if got.UnlockedAt != first.Format(time.RFC3339) {
			t.Errorf("unlocked_at 必须是第一次解锁的时刻，实际 %q", got.UnlockedAt)
		}
	})

	t.Run("作者本人没有解锁时刻", func(t *testing.T) {
		store := &fakeContactStore{unlockAt: first, unlockNew: true}
		svc := NewContact(store, fakeItemGetter{byID: map[int64]*model.ItemDetail{
			7: itemFixture(3, model.ItemTypeFound, model.ItemStatusOpen),
		}}, testLogger)

		got, err := svc.Unlock(context.Background(), userFixture(3, false), 7)
		if err != nil {
			t.Fatalf("%v", err)
		}
		if got.UnlockedAt != "" {
			t.Errorf("作者名下没有 contact_views 行，unlocked_at 必须是空串而不是编造一个时刻：%q", got.UnlockedAt)
		}
		if !got.AlreadyUnlocked {
			t.Error("作者从来不需要解锁，already_unlocked 应为 true")
		}
	})

	t.Run("store 报错就原样往上抛", func(t *testing.T) {
		boom := apperr.New(apperr.CodeInternal)
		store := &fakeContactStore{unlockErr: boom}
		svc := NewContact(store, fakeItemGetter{byID: map[int64]*model.ItemDetail{
			7: itemFixture(3, model.ItemTypeFound, model.ItemStatusOpen),
		}}, testLogger)

		if _, err := svc.Unlock(context.Background(), userFixture(4, false), 7); !errors.Is(err, boom) {
			t.Fatalf("期望透传 store 的错误，实际 %v", err)
		}
	})
}

// ---------- #15 的 contact 可见性 ----------

// TestContactLockedAfterLookup 是 §4 那四行伪代码的完整落地测试，
// 包括 M4 新加的第三条（已解锁过 → 放行）。
//
// lookups 那一列是本文件第二重要的断言：三行不需要查库的路径必须真的不发查询。
// #15 是公开接口、广场每一次点进详情都会走它 ——
// 如果在 lost 帖或作者本人那种显然不需要查的场合也发查询，
// 高并发下这个公开接口就被自己拖慢了，而且日志里全是没意义的 SQL。
func TestContactLockedAfterLookup(t *testing.T) {
	found := itemFixture(3, model.ItemTypeFound, model.ItemStatusOpen)
	lost := itemFixture(3, model.ItemTypeLost, model.ItemStatusOpen)
	anonymous := (*model.User)(nil)
	stranger := userFixture(4, false)
	admin := userFixture(5, true)

	tc := []struct {
		name      string
		item      *model.ItemDetail
		viewer    *model.User
		viewed    bool
		lookupErr error
		force     bool
		locked    bool
		lookups   int
	}{
		{name: "lost 帖一律公开，不查库", item: lost, viewer: stranger, locked: false, lookups: 0},
		{name: "lost 帖匿名也不查库", item: lost, viewer: anonymous, locked: false, lookups: 0},
		{name: "found 帖匿名 → 锁着，没有 id 可查所以不查库", item: found, viewer: anonymous, locked: true, lookups: 0},
		{name: "found 帖作者本人 → 放行，不查库", item: found, viewer: userFixture(3, false), locked: false, lookups: 0},
		{name: "found 帖别人、没解锁过 → 锁着", item: found, viewer: stranger, viewed: false, locked: true, lookups: 1},
		{name: "found 帖别人、解锁过 → 放行（M4 新加的那一条）", item: found, viewer: stranger, viewed: true, locked: false, lookups: 1},
		{name: "admin 没有额外的联系方式可见性", item: found, viewer: admin, viewed: false, locked: true, lookups: 1},
		{
			// ⚠ fail closed。这一条是 §10 说的「只有测了才成立」的性质里最贵的那种：
			// 查库失败时如果这里写成 return false，contact 就泄漏了，
			// 而系统里没有任何一条其它测试会发现它 —— 因为其它测试走的都是正常路径。
			name: "查库失败 → 按锁着处理，且不把错误抛给调用方", item: found, viewer: stranger,
			lookupErr: errors.New("connection reset"), locked: true, lookups: 1,
		},
		{name: "forceContact（刚提交过 contact 的 #13/#16）→ 放行，不查库", item: found, viewer: stranger, force: true, locked: false, lookups: 0},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			v := &fakeViewed{viewed: c.viewed, err: c.lookupErr}
			s := &Item{contacts: v, logger: testLogger}

			if got := s.contactLockedAfterLookup(context.Background(), c.item, c.viewer, c.force); got != c.locked {
				t.Errorf("locked 期望 %v，实际 %v", c.locked, got)
			}
			if v.calls != c.lookups {
				t.Errorf("查库次数期望 %d，实际 %d", c.lookups, v.calls)
			}
			if c.lookups == 1 && v.gotID != [2]int64{c.item.ID, c.viewer.ID} {
				t.Errorf("查的是错的键对：%+v，期望 (%d, %d)", v.gotID, c.item.ID, c.viewer.ID)
			}
		})
	}
}

// ---------- #22 解锁名单 ----------

func TestViewsAuthorization(t *testing.T) {
	owner := userFixture(3, false)
	stranger := userFixture(4, false)
	admin := userFixture(5, true)

	tc := []struct {
		name    string
		status  string
		viewer  *model.User
		wantErr string
		calls   int
	}{
		{name: "发帖人看自己的名单 → 放行", status: model.ItemStatusOpen, viewer: owner, calls: 1},
		{name: "admin 看别人的名单 → 放行（§4 的只读那一类）", status: model.ItemStatusOpen, viewer: admin, calls: 1},
		{
			name:   "其他人 → FORBIDDEN，且不查库",
			status: model.ItemStatusOpen, viewer: stranger,
			wantErr: apperr.CodeForbidden, calls: 0,
		},
		{
			// 作者被下架之后仍然有权看名单 —— 这正是治理场景里最需要的证据（§3.4）。
			name:   "发帖人看自己一条已下架帖子的名单 → 放行",
			status: model.ItemStatusDeleted, viewer: owner, calls: 1,
		},
		{
			name:   "别人看已下架帖子的名单 → NOT_FOUND（藏起来，和 #15 同一条纪律）",
			status: model.ItemStatusDeleted, viewer: stranger,
			wantErr: apperr.CodeNotFound, calls: 0,
		},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeContactStore{listRows: []model.ContactView{{
				ID: 1, ItemID: 7, UserID: 4, Nickname: "小李", RealName: "李某某",
				CreatedAt: time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC),
			}}, listTotal: 1}
			svc := NewContact(store, fakeItemGetter{byID: map[int64]*model.ItemDetail{
				7: itemFixture(3, model.ItemTypeFound, c.status),
			}}, testLogger)

			got, err := svc.Views(context.Background(), c.viewer, 7, PageQuery{})
			if c.wantErr != "" {
				if codeOf(t, err) != c.wantErr {
					t.Fatalf("期望 %s，实际 %s（%v）", c.wantErr, codeOf(t, err), err)
				}
			} else if err != nil {
				t.Fatalf("%v", err)
			}
			if store.listCalls != c.calls {
				t.Errorf("查名单次数期望 %d，实际 %d", c.calls, store.listCalls)
			}
			if c.calls == 1 {
				// 参数没传时走 §4 的统一分页默认值。
				if store.listPage != defaultPage || store.listSize != defaultPageSize {
					t.Errorf("分页默认值期望 (%d,%d)，实际 (%d,%d)",
						defaultPage, defaultPageSize, store.listPage, store.listSize)
				}
				if len(got.List) != 1 || got.List[0].User.RealName != "李某某" {
					t.Fatalf("名单没带出真实姓名，#22 就没有意义了：%+v", got.List)
				}
			}
		})
	}
}

// TestViewsRejectsBadPageBeforeQuerying 断言参数校验发生在查库之前：
// 一个 page_size=99999 的请求不该先去把名单捞出来再报错。
func TestViewsRejectsBadPageBeforeQuerying(t *testing.T) {
	store := &fakeContactStore{}
	svc := NewContact(store, fakeItemGetter{byID: map[int64]*model.ItemDetail{
		7: itemFixture(3, model.ItemTypeFound, model.ItemStatusOpen),
	}}, testLogger)

	_, err := svc.Views(context.Background(), userFixture(3, false), 7, PageQuery{PageSize: "99999"})
	if codeOf(t, err) != apperr.CodeValidation {
		t.Fatalf("期望 VALIDATION，实际 %v", err)
	}
	if store.listCalls != 0 {
		t.Errorf("参数不合格时不该查库：%d 次", store.listCalls)
	}
}

// ---------- 「这个服务写不了通知」的形状证明 ----------

// TestM4ServicesCannotWriteNotifications 用反射检查三个接口的**方法个数**。
//
// 为什么值得写这么一条测试：§3.7 的「有一条通知绝对不发」和 §16 的
// 「contact_unlocked 已经删掉」是两条**否定式**规则，
// 而否定式规则用断言「没有发生什么」来测是不可靠的 ——
// 一个从没用过的字段本来就不会出现在测试里。
// 接口的方法个数是这份约束唯一能被机器查证的形态：
// NotificationStore 有 4 个方法（全是读），ContactStore 2 个，ReportStore 1 个，ItemLookup 1 个。
// 将来谁往它们中间塞一个 Insert/Push/Create，这条测试立刻红，
// 而红的时候他就得来改这段注释 —— 那正是「停下来想清楚这是在哪个里程碑加通知」的时机。
func TestM4ServicesCannotWriteNotifications(t *testing.T) {
	tc := []struct {
		name  string
		iface any
		want  int
	}{
		{"ContactStore", (*ContactStore)(nil), 2},           // Unlock + List
		{"ContactViewLookup", (*ContactViewLookup)(nil), 1}, // Viewed
		{"ReportStore", (*ReportStore)(nil), 1},             // Create
		{"ItemLookup", (*ItemLookup)(nil), 1},               // GetByID
		{"NotificationStore", (*NotificationStore)(nil), 4}, // 全是读
	}

	for _, c := range tc {
		typ := reflect.TypeOf(c.iface).Elem()
		if got := typ.NumMethod(); got != c.want {
			t.Errorf("%s 期望 %d 个方法，实际 %d —— 加方法之前先读 §3.7 那三行禁令",
				typ.Name(), c.want, got)
		}
	}
}

// ---------- 通知的对外形状 ----------

// TestNotificationViewKeepsNullItemID 断言 item_id / return_id 为 NULL 时
// **键仍然存在并且是 null**，而不是整个消失。
//
// 这是 M3 那条 *int / *[]MatchHit 纪律的镜像：那时要保的是「0 也要出现」，
// 这里要保的是「没有关联帖子也要出现」。批量下架那种通知 item_id 本来就是 NULL
// （§3.7 的合并规则），前端要靠「键在但是 null」显示一条不带跳转的通知；
// 键消失了它只能猜是没数据还是后端漏了。
func TestNotificationViewKeepsNullItemID(t *testing.T) {
	raw, err := json.Marshal((&model.Notification{ID: 1, UserID: 3, Type: model.NotificationNewMatch,
		Title: "有匹配"}).View())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}

	for _, k := range []string{"item_id", "return_id"} {
		v, ok := m[k]
		if !ok {
			t.Errorf("%s 这个键消失了：%s", k, raw)
			continue
		}
		if v != nil {
			t.Errorf("%s 期望是 null，实际 %v", k, v)
		}
	}
	// is_read 反过来必须是 false 而不是消失（Go 的 omitempty 会把 false 也算空）。
	if v, ok := m["is_read"]; !ok || v != false {
		t.Errorf("is_read 期望出现且为 false：%s", raw)
	}
}

// ---------- 让上面这些测试能引用 repo 的类型（防漂移） ----------

var (
	_ ContactStore      = (*repo.Contact)(nil)
	_ ContactViewLookup = (*repo.Contact)(nil)
	_ NotificationStore = (*repo.Notification)(nil)
	_ ReportStore       = (*repo.Report)(nil)
	_ ItemLookup        = (*repo.Item)(nil)
)

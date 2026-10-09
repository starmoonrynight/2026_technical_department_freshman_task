package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
)

// 这个文件接着 notification_test.go 做 M4 的第①层，管 #41 举报。
//
// service/report.go 的注释里写了三条禁令（不写 items、不扣分、不给被举报人发通知），
// 它们靠类型层面保证，编译不过去就写不出来。所以这一层的单测测的是**剩下那件事**：
// 一次 INSERT 之前的四步判断顺序，尤其是「字段错了就不要去查库」和
// 「帖子软删了对外就是不存在」—— 这两条都是从用户可见的错误码反推不出来的。

// fakeReportStore 只有一行 INSERT 的能力，和 ReportStore 接口一样宽。
type fakeReportStore struct {
	calls  int
	args   []reportArgs
	retID  int64
	retAt  time.Time
	retErr error
}

type reportArgs struct {
	itemID, reporterID int64
	reason, detail     string
}

func (f *fakeReportStore) Create(_ context.Context, itemID, reporterID int64, reasonCode, detail string) (int64, time.Time, error) {
	f.calls++
	f.args = append(f.args, reportArgs{itemID, reporterID, reasonCode, detail})
	if f.retErr != nil {
		return 0, time.Time{}, f.retErr
	}
	id := f.retID
	if id == 0 {
		id = 9001
	}
	at := f.retAt
	if at.IsZero() {
		at = time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)
	}
	return id, at, nil
}

// spyItems 在 fakeItemGetter 之上多一个次数计数器。
//
// 为什么需要计数：#41 的第一条规则是「reason_code 不在白名单时不许查库」。
// 光看错误码看不出来 —— 查了库再报 VALIDATION 也是同样的 400。
// 只有「GetByID 被调了几次」能证明顺序是对的。
type spyItems struct {
	fakeItemGetter
	calls int
}

func (s *spyItems) GetByID(ctx context.Context, id int64) (*model.ItemDetail, error) {
	s.calls++
	return s.fakeItemGetter.GetByID(ctx, id)
}

func newReportSvc(item *model.ItemDetail, store *fakeReportStore) (*Report, *spyItems) {
	getter := &spyItems{fakeItemGetter: fakeItemGetter{}}
	if item != nil {
		getter.byID = map[int64]*model.ItemDetail{item.ID: item}
	}
	return NewReport(store, getter, testLogger), getter
}

// ---------- ① reason_code 白名单 ----------

// TestReportAcceptsEveryWhitelistedReason 遍历 model.ReportReasonCodes 逐个提交。
//
// 这个循环不嫌多余：那份列表是迁移里 reports_reason_code_check 的镜像，
// 而镜像最容易出的错是「列表里有、CHECK 里没有」——
// 那样 #41 会在 service 放行、在数据库撞 23514，用户看到的是一条内部错误。
// 集成测试（第②层）负责反过来钉一次：这六个值都能真的插进去。
func TestReportAcceptsEveryWhitelistedReason(t *testing.T) {
	for _, code := range model.ReportReasonCodes {
		t.Run(code, func(t *testing.T) {
			store := &fakeReportStore{}
			svc, items := newReportSvc(itemFixture(3, model.ItemTypeFound, model.ItemStatusOpen), store)

			got, err := svc.Create(context.Background(), userFixture(4, false), 7, code, "看着像广告")
			if err != nil {
				t.Fatalf("合法 reason_code %q 被拒了：%v", code, err)
			}
			if got.Status != model.ReportStatusOpen {
				t.Errorf("M4 只能产生 open，实际 %q", got.Status)
			}
			if store.calls != 1 || items.calls != 1 {
				t.Errorf("查库/插库次数期望 (1,1)，实际 (%d,%d)", items.calls, store.calls)
			}
		})
	}
}

// TestReportRejectsUnknownReason 是白名单的反面：表外值一律 VALIDATION，
// 而且**一次都不查库**。
func TestReportRejectsUnknownReason(t *testing.T) {
	tc := []struct {
		name string
		code string
	}{
		{"空", ""},
		{"只有空格", "   "},
		{"大小写不对（CHECK 是区分大小写的）", "Spam"},
		{"自己造一个", "inappropriate"},
		{"看起来像但多了个点", "spam."},
		{"中文（前端下拉必须传 code 而不是显示文案）", "垃圾广告"},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeReportStore{}
			svc, items := newReportSvc(itemFixture(3, model.ItemTypeFound, model.ItemStatusOpen), store)

			got, err := svc.Create(context.Background(), userFixture(4, false), 7, c.code, "x")
			if codeOf(t, err) != apperr.CodeValidation {
				t.Fatalf("期望 VALIDATION，实际 %s（%v）", codeOf(t, err), err)
			}
			if got != nil {
				t.Errorf("被拒的请求不该有结果：%+v", got)
			}
			// ⚠ 顺序断言：一个乱填 reason 的请求不该有能力探库。
			if items.calls != 0 {
				t.Errorf("reason_code 不合法却查了帖子：%d 次", items.calls)
			}
			if store.calls != 0 {
				t.Errorf("reason_code 不合法却插了举报：%d 次", store.calls)
			}
		})
	}
}

// TestReportTrimsBeforeInsert 断言写进库的是 trim 过的值。
//
// reason_code 带一个前导空格的话，service 的白名单（它自己 trim 过）会放行，
// 而数据库那条 CHECK 会拒绝 —— 那正是「代码里没错、报错在库里」最难查的一类。
func TestReportTrimsBeforeInsert(t *testing.T) {
	store := &fakeReportStore{}
	svc, _ := newReportSvc(itemFixture(3, model.ItemTypeFound, model.ItemStatusOpen), store)

	if _, err := svc.Create(context.Background(), userFixture(4, false), 7, "  spam  ", "  重复发同一个帖子  "); err != nil {
		t.Fatal(err)
	}
	if store.args[0].reason != "spam" {
		t.Errorf("reason_code 带着空格进了库，会在 CHECK 上撞 23514：%q", store.args[0].reason)
	}
	if store.args[0].detail != "重复发同一个帖子" {
		t.Errorf("detail 没 trim：%q", store.args[0].detail)
	}
}

// ---------- ② detail 长度 ----------

// TestReportDetailLimitCountsRunes 钉住「500 是字符数而不是字节数」。
//
// reports.detail 是 VARCHAR(500)，PostgreSQL 按**字符**算这个长度，
// 一个汉字算 1。用 len() 的话中文用户提交 167 个字就被拒了，
// 而他看到的是「说明太长了」——一个他完全看不出哪里长的错。
func TestReportDetailLimitCountsRunes(t *testing.T) {
	tc := []struct {
		name    string
		detail  string
		wantErr bool
	}{
		{"正好 500 个汉字 → 放行", strings.Repeat("汉", 500), false},
		{"501 个汉字 → 拒", strings.Repeat("汉", 501), true},
		{"500 个字母 → 放行", strings.Repeat("a", 500), false},
		{"501 个字母 → 拒", strings.Repeat("a", 501), true},
		{"空 detail → 放行（举报说明是可选的）", "", false},
		{"500 个汉字再加两侧空格 → 放行（长度算的是 trim 之后）", "  " + strings.Repeat("汉", 500) + "  ", false},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeReportStore{}
			svc, _ := newReportSvc(itemFixture(3, model.ItemTypeFound, model.ItemStatusOpen), store)

			_, err := svc.Create(context.Background(), userFixture(4, false), 7, model.ReportReasonOther, c.detail)
			if c.wantErr {
				if codeOf(t, err) != apperr.CodeValidation {
					t.Fatalf("期望 VALIDATION，实际 %s（%v）", codeOf(t, err), err)
				}
				if store.calls != 0 {
					t.Errorf("超长 detail 却插了库：%d 次", store.calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("%v", err)
			}
		})
	}
}

// ---------- ③ 帖子存在且没被软删 ----------

// TestReportItemStatusGate 决定谁能被举报。
//
// 只有 deleted 被挡（对外不存在），open/closed 都能举报：
// closed 只表示「东西还回去了」，不表示它没问题 —— 一条虚假帖子归还成功了
// 依然值得举报，而 admin 需要看到它（§3.4 的治理场景）。
func TestReportItemStatusGate(t *testing.T) {
	tc := []struct {
		name    string
		status  string
		wantErr string
		writes  int
	}{
		{"open → 放行", model.ItemStatusOpen, "", 1},
		{"closed → 放行", model.ItemStatusClosed, "", 1},
		{"deleted → NOT_FOUND 且不写行", model.ItemStatusDeleted, apperr.CodeNotFound, 0},
	}

	for _, c := range tc {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeReportStore{}
			svc, _ := newReportSvc(itemFixture(3, model.ItemTypeFound, c.status), store)

			_, err := svc.Create(context.Background(), userFixture(4, false), 7, model.ReportReasonSpam, "")
			if c.wantErr != "" {
				if codeOf(t, err) != c.wantErr {
					t.Fatalf("期望 %s，实际 %s（%v）", c.wantErr, codeOf(t, err), err)
				}
			} else if err != nil {
				t.Fatalf("%v", err)
			}
			if store.calls != c.writes {
				t.Errorf("reports 写入次数期望 %d，实际 %d", c.writes, store.calls)
			}
		})
	}
}

// TestReportOwnPostIsAllowed 是原则 1 的一个边角：举报不设「不能举报自己的帖子」。
//
// 看起来多余（谁会举报自己），但它挡掉了一个错误实现：
// 有人会把这里写成「owner 不能举报」，于是用户发现自己的帖子有问题想请 admin 看一眼时
// 得到 FORBIDDEN —— 那是把举报当成了对抗性工具，而它其实是自助通道。
func TestReportOwnPostIsAllowed(t *testing.T) {
	store := &fakeReportStore{}
	svc, _ := newReportSvc(itemFixture(3, model.ItemTypeLost, model.ItemStatusOpen), store)

	if _, err := svc.Create(context.Background(), userFixture(3, false), 7, model.ReportReasonOther, "我自己这条发错了"); err != nil {
		t.Fatalf("举报自己的帖子被拒了：%v", err)
	}
	if store.calls != 1 {
		t.Errorf("期望插一行，实际 %d 次", store.calls)
	}
	// lost 帖同样可以举报（#21 才区分类型，#41 不区分）。
	if store.args[0].itemID != 7 || store.args[0].reporterID != 3 {
		t.Errorf("写进去的键对不对：item=%d reporter=%d", store.args[0].itemID, store.args[0].reporterID)
	}
}

// TestReportUnknownItemPropagatesNotFound 断言「查不到这一行」的错误码
// 从 repo 原样透传（fakeItemGetter 抛的就是 NOT_FOUND）。
func TestReportUnknownItemPropagatesNotFound(t *testing.T) {
	store := &fakeReportStore{}
	svc, _ := newReportSvc(nil, store)

	_, err := svc.Create(context.Background(), userFixture(4, false), 999, model.ReportReasonSpam, "")
	if codeOf(t, err) != apperr.CodeNotFound {
		t.Fatalf("实际 %v", err)
	}
	if store.calls != 0 {
		t.Errorf("帖子都没查到，不该有举报行：%d 次", store.calls)
	}
}

// ---------- ④ 插一行，以及它的后果边界 ----------

// TestReportResultShape 是 #41 的响应契约。
func TestReportResultShape(t *testing.T) {
	at := time.Date(2026, 10, 7, 1, 30, 0, 0, time.UTC)
	store := &fakeReportStore{retID: 512, retAt: at}
	svc, _ := newReportSvc(itemFixture(3, model.ItemTypeFound, model.ItemStatusOpen), store)

	got, err := svc.Create(context.Background(), userFixture(4, false), 7, model.ReportReasonPrivacy, "有手机号")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 512 {
		t.Errorf("id 该透传 repo 的：%d", got.ID)
	}
	// Status 恒为 open：出现 resolved/dismissed 就等于平台在替 admin 表态（原则 1）。
	if got.Status != "open" {
		t.Errorf("status 期望 open，实际 %q", got.Status)
	}
	if got.CreatedAt != "2026-10-07T01:30:00Z" {
		t.Errorf("created_at 期望 UTC RFC3339，实际 %q", got.CreatedAt)
	}
}

// TestReportIsTheOnlyWrite 是那条「举报零自动后果」在单元层的影子。
//
// 集成测试里那条更狠（被举报人通知数为 0、帖子 status 不变、广场排序不变），
// 这里能证的是更前面的事：这个服务**只有一次 INSERT 可用**，
// 而且 reporter_id 用的是当前登录用户、不是请求里带来的某个 id。
func TestReportIsTheOnlyWrite(t *testing.T) {
	store := &fakeReportStore{}
	svc, _ := newReportSvc(itemFixture(3, model.ItemTypeFound, model.ItemStatusOpen), store)

	if _, err := svc.Create(context.Background(), userFixture(4, false), 7, model.ReportReasonFraud, ""); err != nil {
		t.Fatal(err)
	}
	if store.calls != 1 {
		t.Errorf("一次举报只该有一行，实际 %d 行", store.calls)
	}
	if store.args[0].reporterID != 4 {
		t.Errorf("reporter_id 必须来自 JWT（4），实际 %d —— 是不是被人伪造了", store.args[0].reporterID)
	}
	if store.args[0].itemID != 7 {
		t.Errorf("举报的对象搞错了：%d", store.args[0].itemID)
	}
}

// TestReportPropagatesDuplicate 断言 repo 的 REPORT_DUPLICATE 不会被这层改写成 500。
//
// 「你已经举报过了」对用户是一条该显示出来的消息，不是故障。
func TestReportPropagatesDuplicate(t *testing.T) {
	dup := apperr.WrapMsg(errors.New("23505 uq_reports_open"), apperr.CodeReportDuplicate, "你已经举报过这条帖子了，管理员的处理中")
	store := &fakeReportStore{retErr: dup}
	svc, _ := newReportSvc(itemFixture(3, model.ItemTypeFound, model.ItemStatusOpen), store)

	got, err := svc.Create(context.Background(), userFixture(4, false), 7, model.ReportReasonSpam, "")
	if codeOf(t, err) != apperr.CodeReportDuplicate {
		t.Fatalf("期望 REPORT_DUPLICATE，实际 %s（%v）", codeOf(t, err), err)
	}
	if got != nil {
		t.Errorf("重复举报不该返回一个假 id：%+v", got)
	}
}

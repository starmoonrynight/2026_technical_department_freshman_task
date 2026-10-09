package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"lostfound/internal/apperr"
	"lostfound/internal/config"
	"lostfound/internal/matcher"
	"lostfound/internal/model"
	"lostfound/internal/repo"
)

// 这个文件是计划 §10 的第①层：不连数据库、不起 HTTP server，把 M3 那几条
// 「谁都不通知 / 只有 found 帖写库 / Tier 2 不写 / 谁能看匹配」的规则钉死。
//
// 为什么这几条必须在第①层而不是靠集成测试：
//   - 「lost 帖创建一行都不写」用真库测的话，你断言的是「表里没多行」，
//     而**没多行**和**根本没调用写入函数**在结果上一模一样。用记录调用次数的 fake，
//     断言的才是那个真正的论断（§4 的实现检查点：matches_preview 分支不允许出现任何 INSERT）。
//   - 「通知发给 lost 作者而不是 found 作者」这种方向性错误，用真库测要建两个用户、
//     发两条帖、查 notifications 的 user_id —— 六步里任何一步写错都会让测试通过或失败得莫名其妙。
//     在这里它是一次函数调用加一行断言。
//   - 权限分支（本人 / Admin / 他人 / 已软删）需要四个用户和四种 token，
//     而这些规则本身是纯逻辑，不起数据库完全能测（同 ItemStore 那个接口的理由）。

// testMatchCfg 是这份文件里所有测试共用的阈值，和 .env.example 的默认值一致。
//
// 刻意不用 config.Config{} 的零值：零值意味着 NotifyThreshold=0、Tolerance=0，
// 于是**任何**候选都会越过通知线，「只写高分那几条」这条规则会被静默跳过。
// 这是集成测试 harness 也踩过的坑（见 smoketest/setup_test.go 里的 cfg.Match）。
var testMatchCfg = config.MatchConfig{
	TimeToleranceHours: 24,
	DecayDays:          14,
	NotifyThreshold:    0.75,
	ShowThreshold:      0.55,
}

// ---------- fixture ----------

func mustTS(t *testing.T, s string) *time.Time {
	t.Helper()
	if s == "" {
		return nil
	}
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("fixture 时间 %q 解析失败: %v", s, err)
	}
	return &v
}

// 三个固定的时间锚点，所有 fixture 共用。
// 丢失窗口 = [2026-05-11 08:00, 2026-05-12 15:00]，窗口内捡到 → S_time = 1.0。
const (
	seenRFC  = "2026-05-11T08:00:00Z"
	lostRFC  = "2026-05-12T15:00:00Z"
	foundRFC = "2026-05-12T10:00:00Z"
)

// 字典 id 也固定：分类 12（钱包）挂在 3（衣物箱包）下；地点 57（图书馆）挂在 25 → 2 下。
// 换成别的数字不影响任何断言，但一旦不一致，Tier 2 的三档地点分就会变，
// 所以「同小类同叶子」这件事在 fixture 里钉死而不是散在各个测试里各写一遍。
const (
	catWallet, catWalletParent = int64(12), int64(3)
	catOther, catOtherParent   = int64(21), int64(9)
	locLibrary                 = int64(57)
	locLibraryParent           = int64(25)
	locLibraryTop              = int64(2)
	locDorm, locDormParent     = int64(60), int64(30)
)

type postSpec struct {
	id, userID int64
	itemType   string
	title      string
	desc       string

	cat, catParent int64
	loc            int64
	locParent      int64
	locTop         int64
	freeform       bool
	locName        string

	seen, lost, found string
}

func mk(t *testing.T, sp postSpec) *model.ItemDetail {
	t.Helper()
	if sp.itemType != model.ItemTypeLost && sp.itemType != model.ItemTypeFound {
		t.Fatalf("fixture 类型 %q 既不是 lost 也不是 found", sp.itemType)
	}
	d := &model.ItemDetail{
		Item: model.Item{
			ID:             sp.id,
			ItemType:       sp.itemType,
			UserID:         sp.userID,
			Title:          sp.title,
			Description:    sp.desc,
			CategoryID:     sp.cat,
			LocationID:     sp.loc,
			LocationDetail: "3楼自习室B区",
			LastSeenAt:     mustTS(t, sp.seen),
			LostAt:         mustTS(t, sp.lost),
			FoundAt:        mustTS(t, sp.found),
			Contact:        fmt.Sprintf("wx_%d", sp.id),
			Status:         model.ItemStatusOpen,
		},
		CategoryName:       "钱包",
		LocationName:       sp.locName,
		AuthorNickname:     fmt.Sprintf("同学%d", sp.userID),
		CategoryParentID:   sp.catParent,
		LocationParentID:   sp.locParent,
		LocationTopID:      sp.locTop,
		LocationIsFreeform: sp.freeform,
	}
	if d.LocationName == "" {
		d.LocationName = "图书馆"
	}
	return d
}

// lostWallet 是一条标准的 lost 帖：图书馆、钱包、丢失窗口正常。
func lostWallet(t *testing.T, id, userID int64, title, desc string) *model.ItemDetail {
	t.Helper()
	return mk(t, postSpec{
		id: id, userID: userID, itemType: model.ItemTypeLost,
		title: title, desc: desc,
		cat: catWallet, catParent: catWalletParent,
		loc: locLibrary, locParent: locLibraryParent, locTop: locLibraryTop,
		seen: seenRFC, lost: lostRFC,
	})
}

// foundWallet 同上但类型是 found，拾获时间落在上面那个窗口里。
func foundWallet(t *testing.T, id, userID int64, title, desc string) *model.ItemDetail {
	t.Helper()
	return mk(t, postSpec{
		id: id, userID: userID, itemType: model.ItemTypeFound,
		title: title, desc: desc,
		cat: catWallet, catParent: catWalletParent,
		loc: locLibrary, locParent: locLibraryParent, locTop: locLibraryTop,
		found: foundRFC,
	})
}

// junkFound 是一条**绝不该被通知**的候选：不同大类、不同地点区、文本无关。
// 它的分数落在 0.2 以下（§13 的分布测试里充电宝 vs 钱包 = 0.15）。
func junkFound(t *testing.T, id, userID int64) *model.ItemDetail {
	t.Helper()
	return mk(t, postSpec{
		id: id, userID: userID, itemType: model.ItemTypeFound,
		title: "U盘", desc: "银色u盘32g",
		cat: catOther, catParent: catOtherParent,
		loc: locDorm, locParent: locDormParent, locTop: locDormParent,
		locName: "三号楼宿舍",
		found:   foundRFC,
	})
}

// ---------- fake ----------

// fakeMatchStore 记录**被调用了几次**和**收到了什么参数**。
//
// 这两件事就是 M3 全部关键断言的载体：
// 「lost 方向不写台账」= recordCalls==0，「只写高分那几条」= len(rows)，
// 「方向没搞反」= rows[i].LostItemID 是候选而不是 target。
// 用真数据库只能看到最终状态，看不到中间有没有调过。
type fakeMatchStore struct {
	cands     []model.ItemDetail
	candErr   error
	candCalls []repo.CandidateFilter

	recordCalls int
	recordRows  []repo.LedgerRow
	recordRet   int
	recordErr   error
}

func (f *fakeMatchStore) Candidates(_ context.Context, f2 repo.CandidateFilter) ([]model.ItemDetail, error) {
	f.candCalls = append(f.candCalls, f2)
	if f.candErr != nil {
		return nil, f.candErr
	}
	return f.cands, nil
}

func (f *fakeMatchStore) RecordMatches(_ context.Context, rows []repo.LedgerRow) (int, error) {
	f.recordCalls++
	f.recordRows = rows
	if f.recordErr != nil {
		return 0, f.recordErr
	}
	return f.recordRet, nil
}

type fakeItemGetter struct {
	byID map[int64]*model.ItemDetail
	err  error
}

func (f fakeItemGetter) GetByID(_ context.Context, id int64) (*model.ItemDetail, error) {
	if f.err != nil {
		return nil, f.err
	}
	d, ok := f.byID[id]
	if !ok {
		return nil, apperr.NotFound("帖子")
	}
	return d, nil
}

func newMatchSvc(t *testing.T, store *fakeMatchStore, getter ItemLookup) *Match {
	t.Helper()
	uploads, err := NewUpload(t.TempDir(), "/uploads", nil)
	if err != nil {
		t.Fatalf("建 Upload 失败: %v", err)
	}
	return NewMatch(store, getter, uploads, testMatchCfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// ---------- #13 found/lost 不对称 ----------

// TestOnCreatedLostComputesButNeverWrites 是 §4 那条实现检查点的直接版本。
func TestOnCreatedLostComputesButNeverWrites(t *testing.T) {
	target := lostWallet(t, 1, 7, "黑色钱包", "黑色钱包，夹层里有校园卡")
	perfect := foundWallet(t, 2, 8, "黑色钱包", "黑色钱包，夹层里有校园卡") // 一字不差 → 1.0
	// 三条候选代表 §5.8 的三个分数带，每条都能手算验：
	//   perfect  同小类 + 一字不差 + 落在丢失窗口 = 0.45 + 0.40 + 0.15 = 1.0        → 通知线以上
	//   mid      同小类 + 标题 dice(「钱包」,「黑色钱包」)=2*1/(1+3)=0.5、描述空
	//            → S_text=0.5*0.5+0.5*0=0.25 → 0.45 + 0.10 + 0.15 = 0.70           → 展示区内、通知线下
	//   junk     跨大类 + 文本无关 + 时间合理 → 0 + 0 + 0.15 = 0.15                  → 默认不返回
	mid := foundWallet(t, 3, 9, "钱包", "")
	junk := junkFound(t, 4, 10)

	store := &fakeMatchStore{cands: []model.ItemDetail{*mid, *perfect, *junk}}
	svc := newMatchSvc(t, store, fakeItemGetter{})

	preview, notified := svc.OnCreated(context.Background(), target)

	if store.recordCalls != 0 {
		t.Errorf("lost 帖创建居然调了 RecordMatches %d 次 —— 这是 §4 明令禁止的：matches_preview 分支不允许出现任何 INSERT", store.recordCalls)
	}
	if notified != nil {
		t.Errorf("lost 帖不该有 notified_count，实际拿到了 %d", *notified)
	}
	if preview == nil {
		t.Fatal("lost 帖的 matches_preview 指针是 nil —— 键会从响应里消失，前端无法区分「跑过了没匹配上」和「没跑」")
	}

	// 展示线 0.55：perfect(1.0) 和 mid(0.60) 进来，junk 被筛掉。
	// 顺序必须是分数降序。
	got := *preview
	if len(got) != 2 {
		t.Fatalf("matches_preview 期望 2 条（junk 该被展示线筛掉），实际 %d 条：%s", len(got), dumpHits(got))
	}
	if got[0].Item.ID != 2 || got[1].Item.ID != 3 {
		t.Errorf("排序不对，期望 [2,3]，实际 [%d,%d]", got[0].Item.ID, got[1].Item.ID)
	}
	if got[0].Score != 1.0 {
		t.Errorf("一字不差的一对分数应该是 1.0，实际 %v", got[0].Score)
	}
}

// TestOnCreatedFoundWritesOnlyAboveNotifyLine 验唯一的写入路径：只有 ≥0.75 的进入台账。
func TestOnCreatedFoundWritesOnlyAboveNotifyLine(t *testing.T) {
	target := foundWallet(t, 2, 8, "黑色钱包", "黑色钱包，夹层里有校园卡")
	perfect := lostWallet(t, 1, 7, "黑色钱包", "黑色钱包，夹层里有校园卡") // 1.0
	mid := lostWallet(t, 5, 11, "钱包", "")                  // 0.70：展示区内、通知线下（算法同上一个测试）
	junk := mk(t, postSpec{
		id: 6, userID: 12, itemType: model.ItemTypeLost, title: "学生证", desc: "姓名张三",
		cat: catOther, catParent: catOtherParent,
		loc: locDorm, locParent: locDormParent, locTop: locDormParent,
		seen: seenRFC, lost: lostRFC,
	})

	store := &fakeMatchStore{
		cands:     []model.ItemDetail{*mid, *perfect, *junk},
		recordRet: 1, // 假装其中一条是重复对，被 ON CONFLICT 吃掉
	}
	svc := newMatchSvc(t, store, fakeItemGetter{})

	preview, notified := svc.OnCreated(context.Background(), target)

	if preview != nil {
		t.Errorf("found 帖不该有 matches_preview（§4：两个字段互斥），实际 %s", dumpHits(*preview))
	}
	if notified == nil {
		t.Fatal("found 帖的 notified_count 指针是 nil —— 「一条都没通知出去」必须显示成 0，不是键消失")
	}
	if *notified != 1 {
		t.Errorf("notified_count 应该原样透传 repo 的返回值（去重后的真实条数），期望 1，实际 %d", *notified)
	}
	if store.recordCalls != 1 {
		t.Fatalf("RecordMatches 调用次数期望 1，实际 %d", store.recordCalls)
	}
	if len(store.recordRows) != 1 {
		t.Fatalf("台账行数期望 1（只有 ≥0.75 的 perfect 那条），实际 %d：%s",
			len(store.recordRows), dumpRows(store.recordRows))
	}

	row := store.recordRows[0]
	// ⚠ 这四行是 M3 最容易写错、后果最重的地方：方向反了不会报错，
	// 只会让通知发给捡到东西的人（§2.6.2 明确禁止），并把台账记成反的一对。
	if row.LostItemID != 1 {
		t.Errorf("LostItemID 应该是那条 lost 候选 1，实际 %d", row.LostItemID)
	}
	if row.FoundItemID != 2 {
		t.Errorf("FoundItemID 应该是刚发出来的 found 帖 2，实际 %d", row.FoundItemID)
	}
	if row.NotifyTo != 7 {
		t.Errorf("通知应该发给 lost 帖的作者 7，实际 %d（%d 是 found 帖作者，他不该收到任何东西）", row.NotifyTo, 8)
	}
	if row.Score < testMatchCfg.NotifyThreshold {
		t.Errorf("写进台账的分数 %v 低于通知线 %v", row.Score, testMatchCfg.NotifyThreshold)
	}

	// breakdown 是要落 JSONB 的字符串，必须是合法 JSON 且形状和响应里那个一致。
	var bd matcher.Breakdown
	if err := json.Unmarshal([]byte(row.Breakdown), &bd); err != nil {
		t.Fatalf("台账的 breakdown 不是合法 JSON（JSONB 列会直接拒绝这一行）: %v\n原文: %s", err, row.Breakdown)
	}
	if bd.Tier != matcher.Tier1 {
		t.Errorf("台账里的 tier 应该是 1，实际 %d", bd.Tier)
	}
	if bd.Signals.Location != nil {
		t.Error("Tier 1 的 breakdown 里出现了 location 信号 —— 它应该是三个信号")
	}
	if bd.Score != row.Score {
		t.Errorf("breakdown.score (%v) 和 score 列 (%v) 不是同一个数", bd.Score, row.Score)
	}
}

// TestOnCreatedFoundTier2DoesNotWrite 验 §3.7「Tier 2 的结果不写、不发通知」。
//
// 触发方式是地点选「其他」（is_freeform）：严格模式的前提不成立，
// 而发帖路径不允许降级（§5.8 那张表写的是 Tier 1），所以这里连候选都不该捞。
func TestOnCreatedFoundTier2DoesNotWrite(t *testing.T) {
	target := foundWallet(t, 2, 8, "黑色钱包", "黑色长款钱包")
	target.LocationIsFreeform = true
	perfect := lostWallet(t, 1, 7, "黑色钱包", "黑色长款钱包")

	store := &fakeMatchStore{cands: []model.ItemDetail{*perfect}}
	svc := newMatchSvc(t, store, fakeItemGetter{})

	_, notified := svc.OnCreated(context.Background(), target)

	if len(store.candCalls) != 0 {
		t.Errorf("地点是「其他」的 found 帖还去捞了 Tier 1 候选（%d 次）—— location_id=「其他」筛出来的全是无关帖子，比空结果更误导人", len(store.candCalls))
	}
	if store.recordCalls != 0 {
		t.Errorf("一条都没匹配上却调了 RecordMatches %d 次", store.recordCalls)
	}
	if notified == nil || *notified != 0 {
		t.Errorf("found 帖的 notified_count 应该是 0（指针非空），实际 %v", notified)
	}
}

// ---------- #16 改帖：OnUpdated ----------

// TestOnUpdatedFoundWritesTheSameRowAsOnCreated 是「两条路径共用一套写入规则」
// 的可执行版本 —— 也是这次补 OnUpdated 唯一值得测的理由。
//
// 分成两份代码的话，「改帖时通知线算不算」「Tier 2 写不写」「方向会不会反」
// 每一项都要各自重新推导一遍，而它们的答案必须和发帖时完全一致。
// 所以这里不逐条重复断言，而是把两次调用的产出**整体比相等**：
// 谁将来给其中一条路径单独加逻辑，这个测试就会红，而且红得看得见差异。
func TestOnUpdatedFoundWritesTheSameRowAsOnCreated(t *testing.T) {
	target := foundWallet(t, 2, 8, "黑色长款钱包", "黑色长款钱包，夹层里有校园卡")
	perfect := lostWallet(t, 1, 7, "黑色钱包", "黑色钱包，夹层里有校园卡")

	run := func(f func(svc *Match)) []repo.LedgerRow {
		store := &fakeMatchStore{cands: []model.ItemDetail{*perfect}, recordRet: 1}
		f(newMatchSvc(t, store, fakeItemGetter{}))
		if store.recordCalls != 1 {
			t.Fatalf("RecordMatches 调用次数期望 1，实际 %d", store.recordCalls)
		}
		return store.recordRows
	}

	created := run(func(svc *Match) {
		_, notified := svc.OnCreated(context.Background(), target)
		if notified == nil || *notified != 1 {
			t.Fatalf("OnCreated 的 notified_count 期望 1，实际 %v", notified)
		}
	})
	updated := run(func(svc *Match) { svc.OnUpdated(context.Background(), target) })

	if !reflect.DeepEqual(created, updated) {
		t.Errorf("改帖写进去的台账行和发帖写的不一致（两条路径必须共用同一套规则）：\ncreated=%s\nupdated=%s",
			dumpRows(created), dumpRows(updated))
	}
	// 上面那句相等如果成立，方向就不可能反（created 的方向由
	// TestOnCreatedFoundWritesOnlyAboveNotifyLine 逐字段钉过）。这里只额外确认
	// 改帖这条路径确实也在给**失主**发通知，而不是只留了个日志。
	if len(updated) != 1 || updated[0].LostItemID != 1 || updated[0].FoundItemID != 2 || updated[0].NotifyTo != 7 {
		t.Errorf("改帖的通知方向不对：%s", dumpRows(updated))
	}
	if updated[0].NotifyTitle == "" || updated[0].NotifyBody == "" {
		t.Error("改帖这条路径写出来的通知没有标题或正文 —— 发出去会是一条空消息")
	}
}

// TestOnUpdatedSkipsLostAndClosed 验 OnUpdated 的两道门槛：不是 found 就不跑，
// found 但不是 open 也不跑。
//
// 「不跑」而不是「跑了不写」是这里要断言的东西，所以连候选 SQL 的调用次数一起查：
// lost 帖改一次就去捞一遍全表候选，除了浪费一次查询什么都换不来（§5.8：它永远算完就丢）；
// closed 的拾物帖已经还回去了，再给失主推「可能有匹配」是在骗一个已经结束寻找的人，
// 而 §8 允许改 closed 帖（改错了的电话必须能改），所以这个判断不能省。
func TestOnUpdatedSkipsLostAndClosed(t *testing.T) {
	perfect := foundWallet(t, 2, 8, "黑色钱包", "黑色钱包")

	tests := []struct {
		name   string
		target *model.ItemDetail
		reason string
	}{
		{"lost 帖", lostWallet(t, 1, 7, "黑色钱包", "黑色钱包"), "lost 方向的匹配任何时候都只算不写"},
		{"closed 的 found 帖", func() *model.ItemDetail {
			d := foundWallet(t, 3, 9, "黑色钱包", "黑色钱包")
			d.Status = model.ItemStatusClosed
			return d
		}(), "东西已经还回去了，不该再通知失主"},
		{"deleted 的 found 帖", func() *model.ItemDetail {
			d := foundWallet(t, 4, 10, "黑色钱包", "黑色钱包")
			d.Status = model.ItemStatusDeleted
			return d
		}(), "对外已经不存在的帖子不该出现在任何人的通知里"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &fakeMatchStore{cands: []model.ItemDetail{*perfect}}
			svc := newMatchSvc(t, store, fakeItemGetter{})
			svc.OnUpdated(context.Background(), tt.target)

			if len(store.candCalls) != 0 {
				t.Errorf("不该跑匹配却捞了 %d 次候选（%s）：%s", len(store.candCalls), tt.name, tt.reason)
			}
			if store.recordCalls != 0 {
				t.Errorf("不该跑匹配却写了 %d 次台账：%s", store.recordCalls, tt.reason)
			}
		})
	}
}

// TestOnUpdatedSwallowsStoreFailure 确认改帖这条路径也一样传不出错误。
//
// OnUpdated 没有返回值也没有 error，所以这个测试只能断言「不 panic、不改状态」——
// 而这正是重点：签名里没有一个能让失败溜到 Update() 的出口。
// 匹配挂了最坏是「帖子改好了、通知没发出去」，绝不能变成「改帖 500，用户再改一次」。
func TestOnUpdatedSwallowsStoreFailure(t *testing.T) {
	boom := errors.New("测试注入：数据库抽风")
	perfect := lostWallet(t, 1, 7, "黑色钱包", "黑色钱包，夹层里有校园卡")

	t.Run("候选查不到", func(t *testing.T) {
		store := &fakeMatchStore{candErr: boom}
		svc := newMatchSvc(t, store, fakeItemGetter{})
		svc.OnUpdated(context.Background(), foundWallet(t, 2, 8, "黑色钱包", "黑色钱包"))
		if store.recordCalls != 0 {
			t.Errorf("候选都没捞到却写了台账 %d 次", store.recordCalls)
		}
	})

	t.Run("台账写不进", func(t *testing.T) {
		store := &fakeMatchStore{cands: []model.ItemDetail{*perfect}, recordErr: boom}
		svc := newMatchSvc(t, store, fakeItemGetter{})
		svc.OnUpdated(context.Background(), foundWallet(t, 2, 8, "黑色钱包", "黑色钱包"))
		if store.recordCalls != 1 {
			t.Fatalf("台账应该被尝试写一次，实际 %d 次", store.recordCalls)
		}
		// 到这里测试就结束了：写失败只变成一条 error 日志，函数正常返回。
		// 如果哪天有人把 error 往上抛，panic 或编译失败都会先把这件事暴露出来。
	})
}

// TestOnCreatedSwallowsStoreFailure 是「匹配失败绝不影响发帖成功」的可执行版本。
//
// OnCreated 的签名里没有 error，所以这个测试**只能**断言返回值形状 ——
// 而这正是重点：类型层面就传不出错误，实现想 error 都没地方 return。
func TestOnCreatedSwallowsStoreFailure(t *testing.T) {
	boom := errors.New("测试注入：数据库抽风")

	t.Run("lost", func(t *testing.T) {
		store := &fakeMatchStore{candErr: boom}
		svc := newMatchSvc(t, store, fakeItemGetter{})
		preview, notified := svc.OnCreated(context.Background(), lostWallet(t, 1, 7, "黑色钱包", "黑色钱包"))
		if notified != nil {
			t.Errorf("lost 方向不该有 notified_count")
		}
		if preview == nil || len(*preview) != 0 {
			t.Errorf("查候选失败时 matches_preview 应该是空数组而不是消失，实际 %v", preview)
		}
	})

	t.Run("found", func(t *testing.T) {
		store := &fakeMatchStore{candErr: boom}
		svc := newMatchSvc(t, store, fakeItemGetter{})
		preview, notified := svc.OnCreated(context.Background(), foundWallet(t, 2, 8, "黑色钱包", "黑色钱包"))
		if preview != nil {
			t.Errorf("found 方向不该有 matches_preview")
		}
		if notified == nil || *notified != 0 {
			t.Errorf("查候选失败时 notified_count 应该是 0（键要出现），实际 %v", notified)
		}
	})

	t.Run("台账写失败", func(t *testing.T) {
		target := foundWallet(t, 2, 8, "黑色钱包", "黑色钱包，夹层里有校园卡")
		perfect := lostWallet(t, 1, 7, "黑色钱包", "黑色钱包，夹层里有校园卡")
		store := &fakeMatchStore{cands: []model.ItemDetail{*perfect}, recordErr: boom}
		svc := newMatchSvc(t, store, fakeItemGetter{})

		_, notified := svc.OnCreated(context.Background(), target)
		if store.recordCalls != 1 {
			t.Fatalf("该调一次 RecordMatches，实际 %d 次", store.recordCalls)
		}
		if notified == nil || *notified != 0 {
			// 台账没写进去就当没通知出去。报一个「通知了 1 条」但其实回滚了，
			// 是比 0 更糟的谎 —— 用户会去检查那个人的收件箱。
			t.Errorf("台账写入失败时 notified_count 必须是 0，实际 %v", notified)
		}
	})
}

// TestCandidateFilterDirection 验两个方向的候选条件（§5.2 + §5.5）。
//
// 这是 repo/match.go 那份 SQL 的唯一「编译期之外」的守门测试：
// 锚点列选错（found 帖拿 last_seen_at 当锚点，那是 NULL）或类型反了，
// 症状都是「匹配永远为空」，而空在 Tier 1 还会被 auto 档降级掩盖掉。
func TestCandidateFilterDirection(t *testing.T) {
	t.Run("目标lost找found候选", func(t *testing.T) {
		store := &fakeMatchStore{}
		svc := newMatchSvc(t, store, fakeItemGetter{})
		svc.OnCreated(context.Background(), lostWallet(t, 1, 7, "黑色钱包", ""))

		if len(store.candCalls) != 1 {
			t.Fatalf("期望捞一次候选，实际 %d 次", len(store.candCalls))
		}
		f := store.candCalls[0]
		if f.OppositeType != model.ItemTypeFound {
			t.Errorf("OppositeType 期望 found，实际 %q", f.OppositeType)
		}
		if f.TargetID != 1 {
			t.Errorf("TargetID 应该是目标自己（排除自身用），实际 %d", f.TargetID)
		}
		if f.LocationID != locLibrary {
			t.Errorf("Tier 1 必须带地点硬筛选，LocationID 期望 %d，实际 %d", locLibrary, f.LocationID)
		}
		want := mustTS(t, seenRFC)
		if f.AnchorAt != *want {
			t.Errorf("lost 目标的锚点应该是 last_seen_at (%v)，实际 %v", *want, f.AnchorAt)
		}
		if f.Tolerance != 24*time.Hour {
			t.Errorf("容差应该是 cfg.TimeToleranceHours=24 小时，实际 %v", f.Tolerance)
		}
	})

	t.Run("目标found找lost候选", func(t *testing.T) {
		store := &fakeMatchStore{}
		svc := newMatchSvc(t, store, fakeItemGetter{})
		svc.OnCreated(context.Background(), foundWallet(t, 2, 8, "黑色钱包", ""))

		f := store.candCalls[0]
		if f.OppositeType != model.ItemTypeLost {
			t.Errorf("OppositeType 期望 lost，实际 %q", f.OppositeType)
		}
		want := mustTS(t, foundRFC)
		if f.AnchorAt != *want {
			t.Errorf("found 目标的锚点应该是 found_at (%v)，实际 %v —— 拿错列会筛掉所有候选", *want, f.AnchorAt)
		}
	})
}

// ---------- #20 ----------

// TestListPermissionMatrix 是 §4 第 20 行「JWT（本人或 Admin）」的四种组合。
func TestListPermissionMatrix(t *testing.T) {
	owner := mk(t, postSpec{
		id: 1, userID: 7, itemType: model.ItemTypeLost, title: "黑色钱包",
		cat: catWallet, catParent: catWalletParent,
		loc: locLibrary, locParent: locLibraryParent, locTop: locLibraryTop,
		seen: seenRFC, lost: lostRFC,
	})
	deleted := mk(t, postSpec{
		id: 2, userID: 7, itemType: model.ItemTypeLost, title: "黑色钱包",
		cat: catWallet, catParent: catWalletParent,
		loc: locLibrary, locParent: locLibraryParent, locTop: locLibraryTop,
		seen: seenRFC, lost: lostRFC,
	})
	deleted.Status = model.ItemStatusDeleted

	getter := fakeItemGetter{byID: map[int64]*model.ItemDetail{1: owner, 2: deleted}}
	store := &fakeMatchStore{}
	svc := newMatchSvc(t, store, getter)

	user := &model.User{ID: 7, Role: model.RoleUser}
	admin := &model.User{ID: 99, Role: model.RoleAdmin}
	other := &model.User{ID: 123, Role: model.RoleUser}

	cases := []struct {
		name   string
		viewer *model.User
		itemID int64
		want   string // "" 表示期望成功
	}{
		{"本人看自己的", user, 1, ""},
		{"admin 看别人的", admin, 1, ""},
		{"无关用户看别人的", other, 1, apperr.CodeForbidden},
		{"无关用户看已软删的", other, 2, apperr.CodeNotFound},
		{"本人看自己已软删的", user, 2, ""},
		{"admin 看已软删的", admin, 2, ""},
		{"帖子不存在", other, 404, apperr.CodeNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.List(context.Background(), c.viewer, c.itemID, MatchesQuery{})
			if c.want == "" {
				if err != nil {
					t.Fatalf("不该失败: %v", err)
				}
				return
			}
			if !apperr.IsCode(err, c.want) {
				t.Fatalf("期望错误码 %s，实际 %v", c.want, err)
			}
		})
	}
}

// TestListIsAlwaysReadOnly 单独钉一次「刷新页面不该灌台账」。
func TestListIsAlwaysReadOnly(t *testing.T) {
	target := lostWallet(t, 1, 7, "黑色钱包", "黑色钱包，夹层里有校园卡")
	perfect := foundWallet(t, 2, 8, "黑色钱包", "黑色钱包，夹层里有校园卡")
	store := &fakeMatchStore{cands: []model.ItemDetail{*perfect}}
	svc := newMatchSvc(t, store, fakeItemGetter{byID: map[int64]*model.ItemDetail{1: target}})
	viewer := &model.User{ID: 7, Role: model.RoleUser}

	for i := 0; i < 3; i++ {
		res, err := svc.List(context.Background(), viewer, 1, MatchesQuery{})
		if err != nil {
			t.Fatalf("第 %d 次查询失败: %v", i+1, err)
		}
		if len(res.List) != 1 {
			t.Fatalf("第 %d 次结果应该是 1 条，实际 %d —— 每次都现算，不能因为上一次算过就返回空", i+1, len(res.List))
		}
	}
	if len(store.candCalls) != 3 {
		t.Errorf("三次查询应该各捞一次候选（永远现算，§5.8），实际 %d 次", len(store.candCalls))
	}
	if store.recordCalls != 0 {
		t.Errorf("GET matches 刷了 3 次就写了几次台账：%d 次 —— §3.7 禁止，否则「谁被通知过」变成「谁刷过页面」", store.recordCalls)
	}
}

// TestListTierAndThreshold 覆盖 ?tier= 三档和 min_score / top 的效果。
func TestListTierAndThreshold(t *testing.T) {
	normal := lostWallet(t, 1, 7, "黑色钱包", "黑色钱包，夹层里有校园卡")
	freeform := lostWallet(t, 3, 7, "黑色钱包", "黑色钱包")
	freeform.LocationIsFreeform = true
	missingAnchor := lostWallet(t, 4, 7, "黑色钱包", "")
	missingAnchor.LastSeenAt = nil

	perfect := foundWallet(t, 11, 8, "黑色钱包", "黑色钱包，夹层里有校园卡")
	mid := foundWallet(t, 12, 9, "钱包", "") // 0.70，见上面的三带说明
	junk := junkFound(t, 13, 10)

	allCands := []model.ItemDetail{*mid, *perfect, *junk}

	cases := []struct {
		name     string
		target   *model.ItemDetail
		q        MatchesQuery
		cands    []model.ItemDetail
		wantTier int
		wantIDs  []int64
		wantNote bool
		// 降级时捞候选的那几次 LocationID，按顺序
		wantLocs []int64
	}{
		{
			name: "auto 且 Tier1 有候选 → 三信号、无横幅",
			// 注意：fake 的 Candidates 不区分 LocationID，所以这里的候选
			// 既可能被 Tier 1 的地点筛也可能被 Tier 2 的捞到，判档看的是第一次调用。
			target:   normal,
			q:        MatchesQuery{},
			cands:    allCands,
			wantTier: matcher.Tier1,
			wantIDs:  []int64{11, 12},
			wantNote: false,
			wantLocs: []int64{locLibrary},
		},
		{
			name:     "显式 tier=2 → 四信号、带横幅",
			target:   normal,
			q:        MatchesQuery{Tier: "2"},
			cands:    allCands,
			wantTier: matcher.Tier2,
			wantIDs:  []int64{11, 12},
			wantNote: true,
			wantLocs: []int64{0},
		},
		{
			name:     "Tier1 空手而归 → auto 降级，fell_back 之后带横幅",
			target:   normal,
			q:        MatchesQuery{},
			cands:    nil,
			wantTier: matcher.Tier2,
			wantIDs:  nil,
			wantNote: true,
			wantLocs: []int64{locLibrary, 0},
		},
		{
			name:     "地点是「其他」→ auto 直接 Tier2，一次都不捞严格候选",
			target:   freeform,
			q:        MatchesQuery{},
			cands:    allCands,
			wantTier: matcher.Tier2,
			wantIDs:  []int64{11, 12},
			wantNote: true,
			wantLocs: []int64{0},
		},
		{
			name:     "显式 tier=1 但前提不成立 → 留在 1 档给空结果，不偷偷放宽",
			target:   freeform,
			q:        MatchesQuery{Tier: "1"},
			cands:    allCands,
			wantTier: matcher.Tier1,
			wantIDs:  nil,
			wantNote: false,
			wantLocs: nil,
		},
		{
			name:     "min_score=0 放宽 → junk 也出来",
			target:   normal,
			q:        MatchesQuery{MinScore: "0"},
			cands:    allCands,
			wantTier: matcher.Tier1,
			wantIDs:  []int64{11, 12, 13},
			wantNote: false,
			wantLocs: []int64{locLibrary},
		},
		{
			name:     "top=1 截断",
			target:   normal,
			q:        MatchesQuery{Top: "1"},
			cands:    allCands,
			wantTier: matcher.Tier1,
			wantIDs:  []int64{11},
			wantNote: false,
			wantLocs: []int64{locLibrary},
		},
		{
			name:     "时间锚点缺失（脏数据）→ 空结果而不是 panic",
			target:   missingAnchor,
			q:        MatchesQuery{Tier: "2"},
			cands:    allCands,
			wantTier: matcher.Tier2,
			wantIDs:  nil,
			wantNote: true,
			wantLocs: nil,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store := &fakeMatchStore{cands: c.cands}
			getter := fakeItemGetter{byID: map[int64]*model.ItemDetail{c.target.ID: c.target}}
			svc := newMatchSvc(t, store, getter)
			viewer := &model.User{ID: c.target.UserID, Role: model.RoleUser}

			res, err := svc.List(context.Background(), viewer, c.target.ID, c.q)
			if err != nil {
				t.Fatalf("查询失败: %v", err)
			}
			if res.Tier != c.wantTier {
				t.Errorf("tier 期望 %d，实际 %d", c.wantTier, res.Tier)
			}
			if (res.Notice != "") != c.wantNote {
				t.Errorf("notice 该不该有：期望 %v，实际 %q", c.wantNote, res.Notice)
			}
			if res.List == nil {
				t.Fatal("list 是 nil —— 序列化成 null，前端得为它写判空；应该是空数组")
			}
			if len(res.List) != len(c.wantIDs) {
				t.Fatalf("结果条数期望 %d，实际 %d：%s", len(c.wantIDs), len(res.List), dumpHits(res.List))
			}
			for i, id := range c.wantIDs {
				if res.List[i].Item.ID != id {
					t.Errorf("第 %d 条期望 id=%d，实际 %d：%s", i, id, res.List[i].Item.ID, dumpHits(res.List))
				}
			}

			// 每次捞候选的地点条件必须和档位对上（Tier 2 传 0 = 不筛地点）
			if len(c.wantLocs) != len(store.candCalls) {
				t.Fatalf("捞候选次数期望 %d（各次地点条件 %v），实际 %d：%v",
					len(c.wantLocs), c.wantLocs, len(store.candCalls), dumpCalls(store.candCalls))
			}
			for i, want := range c.wantLocs {
				if got := store.candCalls[i].LocationID; got != want {
					t.Errorf("第 %d 次捞候选的 LocationID 期望 %d，实际 %d", i, want, got)
				}
			}

			// 信号数量是档位的可观察形状：M3 判据「Tier1 恰好三个 / Tier2 四个」
			for _, h := range res.List {
				hasLoc := h.Breakdown.Signals.Location != nil
				if (h.Breakdown.Tier == matcher.Tier2) != hasLoc {
					t.Errorf("帖子 %d 的 breakdown 档位和信号数对不上：tier=%d，location 信号 %v",
						h.Item.ID, h.Breakdown.Tier, hasLoc)
				}
			}
		})
	}
}

// TestListRejectsBadQueryParams 验参数错误的形状（VALIDATION + 字段名）。
func TestListRejectsBadQueryParams(t *testing.T) {
	target := lostWallet(t, 1, 7, "黑色钱包", "")
	getter := fakeItemGetter{byID: map[int64]*model.ItemDetail{1: target}}
	svc := newMatchSvc(t, &fakeMatchStore{}, getter)
	viewer := &model.User{ID: 7, Role: model.RoleUser}

	cases := []struct {
		name string
		q    MatchesQuery
	}{
		{"top 不是数字", MatchesQuery{Top: "abc"}},
		{"top=0", MatchesQuery{Top: "0"}},
		{"top 超上限", MatchesQuery{Top: "51"}},
		{"min_score 越界", MatchesQuery{MinScore: "1.5"}},
		{"min_score 不是数字", MatchesQuery{MinScore: "高"}},
		{"tier 不是 auto/1/2", MatchesQuery{Tier: "3"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.List(context.Background(), viewer, 1, c.q)
			if !apperr.IsCode(err, apperr.CodeValidation) {
				t.Fatalf("期望 VALIDATION，实际 %v", err)
			}
			// 参数错一个候选都不该捞：解析放在最前面，
			// 否则每个非法请求都能白跑一次 200 行的 SQL。
		})
	}

	// 合法但非默认的 auto 写法（大小写不敏感由前端保证，这里只认小写）
	if _, err := svc.List(context.Background(), viewer, 1, MatchesQuery{Tier: "auto"}); err != nil {
		t.Errorf("tier=auto 应该被接受: %v", err)
	}
}

// ---------- 形状与投影 ----------

// TestToMatcherItemProjection 验 model→matcher 的祖先列没接错。
//
// 这条测试看起来很机械，但它守的是 Tier 2 的一个静默错误：
// 把 LocationParentID 接到 LocationTop 上，「同二级子类 0.6」会变成「同一级区 0.3」，
// 分数照样在 [0,1] 里、排序照样像样子，只有真实数据会露馅 ——
// 而露馅的时候你已经分不清是算法还是接线了。
func TestToMatcherItemProjection(t *testing.T) {
	d := mk(t, postSpec{
		id: 1, userID: 7, itemType: model.ItemTypeLost, title: "T",
		cat: catWallet, catParent: catWalletParent,
		loc: locLibrary, locParent: locLibraryParent, locTop: locLibraryTop,
		freeform: true, seen: seenRFC, lost: lostRFC,
	})
	d.CategoryName = "不该出现在 matcher 里"

	it := toMatcherItem(d)
	if it.CategoryParent != catWalletParent {
		t.Errorf("CategoryParent 接错: %d", it.CategoryParent)
	}
	if it.LocationParent != locLibraryParent || it.LocationTop != locLibraryTop {
		t.Errorf("地点祖先接错: parent=%d top=%d", it.LocationParent, it.LocationTop)
	}
	if !it.LocationIsFreeform {
		t.Error("LocationIsFreeform 没投影过去，「其他」地点会误走 Tier 1")
	}
	if it.ID != 1 || it.ItemType != model.ItemTypeLost {
		t.Errorf("基本字段接错: %+v", it)
	}
	if it.LastSeenAt == nil || it.LostAt == nil || it.FoundAt != nil {
		t.Errorf("时间列接错: seen=%v lost=%v found=%v", it.LastSeenAt, it.LostAt, it.FoundAt)
	}

	// CanTier1 的两个前提在投影结果上必须分别成立/不成立：
	// 漏投影任何一个字段（尤其是 LocationIsFreeform）都会让分档判错。
	if matcher.CanTier1(it) {
		t.Error("地点是「其他」的帖子 CanTier1 应该是 false")
	}
	standard := toMatcherItem(mk(t, postSpec{
		id: 1, itemType: model.ItemTypeLost, cat: catWallet,
		loc: locLibrary, seen: seenRFC, lost: lostRFC,
	}))
	if !matcher.CanTier1(standard) {
		t.Error("标准 lost 帖的 CanTier1 应该是 true —— 多半是漏投影了 LastSeenAt，或者 freeform 标记串了")
	}
}

// TestHitContactFollowsSquareRule：匹配列表不是解锁的替代品。
func TestHitContactFollowsSquareRule(t *testing.T) {
	target := foundWallet(t, 2, 8, "黑色钱包", "黑色钱包，夹层里有校园卡")
	lostCand := lostWallet(t, 1, 7, "黑色钱包", "黑色钱包，夹层里有校园卡")
	store := &fakeMatchStore{cands: []model.ItemDetail{*lostCand}}
	getter := fakeItemGetter{byID: map[int64]*model.ItemDetail{2: target}}
	svc := newMatchSvc(t, store, getter)

	// viewer 用的是 user id，不是 item id —— 这两份 fixture 里数字都是 1/2，抄串了
	// 就会得到一个 FORBIDDEN，而症状看起来像「权限写错了」。
	res, err := svc.List(context.Background(), &model.User{ID: 8, Role: model.RoleUser}, 2, MatchesQuery{})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(res.List) != 1 {
		t.Fatalf("期望 1 条，实际 %d", len(res.List))
	}
	// 候选是 lost 帖 → contact 公开（和 #14 广场同一个规则）
	if res.List[0].Item.Contact == nil {
		t.Error("lost 候选的 contact 应该是公开的（§4：广场 lost 带、found 一律 null）")
	}

	// 反过来：给 lost 帖找到的 found 候选，contact 必须锁着
	store2 := &fakeMatchStore{cands: []model.ItemDetail{*foundWallet(t, 3, 9, "黑色钱包", "黑色钱包，夹层里有校园卡")}}
	getter2 := fakeItemGetter{byID: map[int64]*model.ItemDetail{1: lostCand}}
	svc2 := newMatchSvc(t, store2, getter2)
	res2, err := svc2.List(context.Background(), &model.User{ID: 7, Role: model.RoleUser}, 1, MatchesQuery{})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if res2.List[0].Item.Contact != nil {
		t.Errorf("found 候选的 contact 泄进了匹配列表（%q）—— M4 的 #21 才是解锁入口，"+
			"从匹配列表绕过它，contact_views 这份认领名单就不再完整了", *res2.List[0].Item.Contact)
	}
}

// TestCreateResultJSONKeys 验那两个互斥键在序列化之后真的互斥。
//
// 单测里指针判断对了不等于 JSON 对了：omitempty 对切片和指针的行为不一样，
// 而前端只看键在不在。这条测试直接验字节。
func TestCreateResultJSONKeys(t *testing.T) {
	target := lostWallet(t, 1, 7, "黑色钱包", "黑色钱包，夹层里有校园卡")
	perfect := foundWallet(t, 2, 8, "黑色钱包", "黑色钱包，夹层里有校园卡")

	t.Run("lost 有 preview 无 notified", func(t *testing.T) {
		store := &fakeMatchStore{cands: []model.ItemDetail{*perfect}}
		svc := newMatchSvc(t, store, fakeItemGetter{})
		preview, notified := svc.OnCreated(context.Background(), target)
		raw := encodeCreateData(t, preview, notified)
		if !strings.Contains(raw, `"matches_preview"`) {
			t.Errorf("lost 帖的响应里没有 matches_preview 键: %s", raw)
		}
		if strings.Contains(raw, `"notified_count"`) {
			t.Errorf("lost 帖的响应里出现了 notified_count: %s", raw)
		}
	})

	t.Run("found 一条没中也有 notified_count:0", func(t *testing.T) {
		store := &fakeMatchStore{cands: []model.ItemDetail{*junkFound(t, 3, 9)}}
		svc := newMatchSvc(t, store, fakeItemGetter{})
		preview, notified := svc.OnCreated(context.Background(), foundWallet(t, 2, 8, "黑色钱包", "黑色钱包"))
		raw := encodeCreateData(t, preview, notified)
		if strings.Contains(raw, `"matches_preview"`) {
			t.Errorf("found 帖的响应里出现了 matches_preview: %s", raw)
		}
		if !strings.Contains(raw, `"notified_count":0`) {
			t.Errorf("found 帖一条都没通知出去时 notified_count 必须显式为 0（判据③），实际: %s", raw)
		}
	})

	t.Run("lost 空命中时键还在并且是 []", func(t *testing.T) {
		store := &fakeMatchStore{cands: []model.ItemDetail{*junkFound(t, 3, 9)}}
		svc := newMatchSvc(t, store, fakeItemGetter{})
		preview, _ := svc.OnCreated(context.Background(), target)
		raw := encodeCreateData(t, preview, nil)
		if !strings.Contains(raw, `"matches_preview":[]`) {
			t.Errorf("空命中应该是 []，实际: %s", raw)
		}
	})
}

func encodeCreateData(t *testing.T, preview *[]MatchHit, notified *int) string {
	t.Helper()
	raw, err := json.Marshal(CreateResult{MatchesPreview: preview, NotifiedCount: notified})
	if err != nil {
		t.Fatalf("序列化 CreateResult 失败: %v", err)
	}
	return string(raw)
}

// ---------- 文案与数据库列宽 ----------

// TestNewMatchNoticeRespectsColumnLimits 验通知文案不会撑爆 VARCHAR。
//
// 这不是洁癖：notifications.title 是 VARCHAR(100)、content 是 VARCHAR(500)，
// 超了会报 22001，而那个错误在 RecordMatches 的**同一个事务**里 ——
// 一条超长文案会把这一轮所有正常通知一起回滚掉，症状是「通知一条都没发出去」。
func TestNewMatchNoticeRespectsColumnLimits(t *testing.T) {
	long := strings.Repeat("黑", 100) // items.title 的上限就是 100 字符，所以这是合法的最坏输入
	target := foundWallet(t, 2, 8, long, long)
	lostCand := lostWallet(t, 1, 7, long, "")

	title, body := newMatchNotice(target, MatchHit{
		Item:  lostCand.Summary(nil, ""),
		Score: 0.87,
	})
	if n := utf8.RuneCountInString(title); n > 100 {
		t.Errorf("title %d 个字符，超过 VARCHAR(100)：%s", n, title)
	}
	if n := utf8.RuneCountInString(body); n > 500 {
		t.Errorf("content %d 个字符，超过 VARCHAR(500)", n)
	}
	if !strings.Contains(body, "自行核对") {
		t.Errorf("通知文案没写「请自行核对」，读起来像平台判定归属：%s", body)
	}
	if strings.Contains(title, long) || strings.Contains(body, long) {
		t.Error("标题没有被截断")
	}
}

func TestClipRunes(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"黑色钱包", 10, "黑色钱包"},
		{"黑色钱包", 4, "黑色钱包"}, // 正好等于上限不补省略号
		{"黑色钱包", 3, "黑色…"},  // 按字符不按字节：一个汉字 3 字节
		{"", 5, ""},
		{"abcdefghij", 4, "abc…"},
	}
	for _, c := range cases {
		if got := clipRunes(c.in, c.n); got != c.want {
			t.Errorf("clipRunes(%q, %d) = %q，期望 %q", c.in, c.n, got, c.want)
		}
	}
}

// ---------- 参数解析 ----------

func TestParseMatchesQuery(t *testing.T) {
	const def = 0.55
	cases := []struct {
		name        string
		q           MatchesQuery
		wantTop     int
		wantMin     float64
		wantTier    int
		wantErrCode string
	}{
		{"全空走默认", MatchesQuery{}, matchesDefaultTop, def, 0, ""},
		{"auto 显式", MatchesQuery{Tier: "auto"}, matchesDefaultTop, def, 0, ""},
		{"tier=1", MatchesQuery{Tier: "1"}, matchesDefaultTop, def, 1, ""},
		{"tier=2", MatchesQuery{Tier: "2"}, matchesDefaultTop, def, 2, ""},
		{"top 上限", MatchesQuery{Top: "50"}, 50, def, 0, ""},
		{"min_score 放宽到 0", MatchesQuery{MinScore: "0"}, matchesDefaultTop, 0, 0, ""},
		{"min_score=1 边界合法", MatchesQuery{MinScore: "1"}, matchesDefaultTop, 1, 0, ""},
		{"top=0 非法", MatchesQuery{Top: "0"}, 0, 0, 0, apperr.CodeValidation},
		{"top 负数", MatchesQuery{Top: "-1"}, 0, 0, 0, apperr.CodeValidation},
		{"top 不是整数", MatchesQuery{Top: "1.5"}, 0, 0, 0, apperr.CodeValidation},
		{"min_score 越界", MatchesQuery{MinScore: "2"}, 0, 0, 0, apperr.CodeValidation},
		{"min_score 负数", MatchesQuery{MinScore: "-0.1"}, 0, 0, 0, apperr.CodeValidation},
		{"tier=3", MatchesQuery{Tier: "3"}, 0, 0, 0, apperr.CodeValidation},
		{"tier 大写 AUTO 不认", MatchesQuery{Tier: "AUTO"}, 0, 0, 0, apperr.CodeValidation},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			top, minScore, tier, err := parseMatchesQuery(c.q, def)
			if c.wantErrCode != "" {
				if !apperr.IsCode(err, c.wantErrCode) {
					t.Fatalf("期望 %s，实际 %v", c.wantErrCode, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if top != c.wantTop || minScore != c.wantMin || tier != c.wantTier {
				t.Errorf("解析结果 (top=%d, min=%v, tier=%d)，期望 (top=%d, min=%v, tier=%d)",
					top, minScore, tier, c.wantTop, c.wantMin, c.wantTier)
			}
		})
	}
}

// ---------- 辅助：失败时把上下文打出来 ----------

func dumpHits(hits []MatchHit) string {
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "\n  id=%d type=%s title=%q score=%.4f tier=%d",
			h.Item.ID, h.Item.ItemType, h.Item.Title, h.Score, h.Breakdown.Tier)
	}
	return b.String()
}

func dumpRows(rows []repo.LedgerRow) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "\n  lost=%d found=%d score=%.4f notify_to=%d",
			r.LostItemID, r.FoundItemID, r.Score, r.NotifyTo)
	}
	return b.String()
}

func dumpCalls(calls []repo.CandidateFilter) string {
	var b strings.Builder
	for _, c := range calls {
		fmt.Fprintf(&b, "\n  opposite=%s location=%d anchor=%v", c.OppositeType, c.LocationID, c.AnchorAt)
	}
	return b.String()
}

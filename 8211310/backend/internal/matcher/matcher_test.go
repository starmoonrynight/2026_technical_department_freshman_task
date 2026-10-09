package matcher

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---------- 构造测试数据的几个小工具 ----------
//
// 时间基准用 §2.3/§13 那个贯穿全文的例子：
// 小李周一 08:00 最后拿着钱包，周二 15:00 才发现不见了（丢失窗口 [周一8, 周二15]），
// 小王周一 12:00 在图书馆捡到 → 落在窗口内，S_time 满分。
var (
	lastSeenAt = "2026-05-11T08:00:00Z" // 周一 08:00
	lostAt     = "2026-05-12T15:00:00Z" // 周二 15:00
	inWindow   = "2026-05-11T12:00:00Z" // 周一 12:00 捡到 → 1.0
	oneDayLate = "2026-05-13T15:00:00Z" // 周二 15:00 + 24h → 1 - 1/14
	beforeWin  = "2026-05-11T07:00:00Z" // 周一 07:00，早于最后确认拥有 → 0.3
	exactDecay = "2026-05-26T15:00:00Z" // lost_at + 正好 14 天 → 0
	pastDecay  = "2026-05-27T15:00:00Z" // 14 天以外 → 依然是 0，不能变负数
)

func tp(rfc string) *time.Time {
	if rfc == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		panic(err)
	}
	return &t
}

func lost(title, desc string, cat, catParent int64, seen, at string) Item {
	return Item{
		ID: 1, ItemType: TypeLost, Title: title, Description: desc,
		CategoryID: cat, CategoryParent: catParent,
		LocationID: 57, LocationParent: 25, LocationTop: 2,
		LastSeenAt: tp(seen), LostAt: tp(at),
	}
}

func found(title, desc string, cat, catParent int64, at string) Item {
	return Item{
		ID: 2, ItemType: TypeFound, Title: title, Description: desc,
		CategoryID: cat, CategoryParent: catParent,
		LocationID: 57, LocationParent: 25, LocationTop: 2,
		FoundAt: tp(at),
	}
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---------- 打分：判据点名的那些「已知值」 ----------

// TestScorePairKnownScores 是 M3 判据的正文。
//
// 每一行的期望分数都在注释里手算过。权重是 0.45/0.40/0.15，
// 所以「同小类 + 文本全同 + 窗口内」必然是精确的 1.0，而「跨大类 + 文本全同 + 窗口内」
// 必然是精确的 0.55 —— 这两个数一个是上界一个是封顶，它们一起定义了整套阈值的形状。
func TestScorePairKnownScores(t *testing.T) {
	cases := []struct {
		name              string
		lost, found       Item
		wantCat, wantText float64
		wantTime          float64
		wantDays          int
		wantScore         float64
	}{
		{
			// ★ 判据「同小类≈1.0」：三个信号全满 → 0.45+0.40+0.15 = 1.0
			name:      "同小类 + 文本相同 + 落在丢失窗口内 = 满分",
			lost:      lost("黑色钱包", "里有一些证件", 31, 5, lastSeenAt, lostAt),
			found:     found("黑色钱包", "里有一些证件", 31, 5, inWindow),
			wantCat:   1.0,
			wantText:  1.0,
			wantTime:  1.0,
			wantScore: 1.0,
		},
		{
			// ★ 判据「同大类半分」：耳机音响 vs 智能穿戴，同属数码电子大类 → S_cat 0.5
			// 0.45*0.5 + 0.40*1 + 0.15*1 = 0.225 + 0.55 = 0.775
			name:      "同大类不同小类（耳机音响 vs 智能穿戴）半分",
			lost:      lost("蓝牙耳机", "白色", 4, 1, lastSeenAt, lostAt),
			found:     found("蓝牙耳机", "白色", 7, 1, inWindow),
			wantCat:   0.5,
			wantText:  1.0,
			wantTime:  1.0,
			wantScore: 0.775,
		},
		{
			// ★ 判据「跨大类封顶」：分类全错时，就算文本一字不差、时间完美落在窗口内，
			// 也只有 0.40 + 0.15 = 0.55 —— 永远够不到 0.75 的通知线。
			// 这一行是「分类只做排序、但排序权重足够大」的量化证据。
			name:      "跨大类封顶：文本与时间全满也只有 0.55",
			lost:      lost("黑色钱包", "里有一些证件", 31, 5, lastSeenAt, lostAt),
			found:     found("黑色钱包", "里有一些证件", 8, 1, inWindow),
			wantCat:   0.0,
			wantText:  1.0,
			wantTime:  1.0,
			wantScore: 0.55,
		},
		{
			// ★ 判据「时间三档」第一档
			name:      "时间档一：落在丢失窗口内 → 1.0",
			lost:      lost("黑色钱包", "里有一些证件", 31, 5, lastSeenAt, lostAt),
			found:     found("黑色钱包", "里有一些证件", 31, 5, inWindow),
			wantCat:   1.0,
			wantText:  1.0,
			wantTime:  1.0,
			wantDays:  0,
			wantScore: 1.0,
		},
		{
			// ★ 判据「时间三档」第二档：found_at = lost_at + 正好 24h
			// 1 - 24h/336h = 0.9286（四舍五入到 4 位）；总分 0.45+0.40+0.15*0.9286 = 0.9893
			//
			// 这一行同时是 lost_at 那个设计的验收：如果起点错用 last_seen_at，
			// 这里会差 2 天（0.8571），总分 0.9798 —— 少 0.0095 看着不多，
			// 但它会在阈值线上把一个真匹配挤到通知线以下。
			name:      "时间档二：lost_at 之后 1 天 → 1-1/14",
			lost:      lost("黑色钱包", "里有一些证件", 31, 5, lastSeenAt, lostAt),
			found:     found("黑色钱包", "里有一些证件", 31, 5, oneDayLate),
			wantCat:   1.0,
			wantText:  1.0,
			wantTime:  0.9286,
			wantDays:  1,
			wantScore: 0.9893,
		},
		{
			// ★ 判据「时间三档」第三档：重罚但不清零
			name:      "时间档三：早于最后确认拥有 → 0.3，不清零",
			lost:      lost("黑色钱包", "里有一些证件", 31, 5, lastSeenAt, lostAt),
			found:     found("黑色钱包", "里有一些证件", 31, 5, beforeWin),
			wantCat:   1.0,
			wantText:  1.0,
			wantTime:  TimeBeforeWindow,
			wantScore: 0.895,
		},
		{
			// ★ 判据「14 天外为 0」：正好 14 天 → 边界值 0（含边界，不是 <）
			name:      "衰减边界：正好 14 天 → S_time = 0",
			lost:      lost("黑色钱包", "里有一些证件", 31, 5, lastSeenAt, lostAt),
			found:     found("黑色钱包", "里有一些证件", 31, 5, exactDecay),
			wantCat:   1.0,
			wantText:  1.0,
			wantTime:  0,
			wantDays:  14,
			wantScore: 0.85,
		},
		{
			// ★ 判据「14 天外为 0」的另一半：超出去不能变成负数
			name:      "14 天以外仍是 0，不会变负",
			lost:      lost("黑色钱包", "里有一些证件", 31, 5, lastSeenAt, lostAt),
			found:     found("黑色钱包", "里有一些证件", 31, 5, pastDecay),
			wantCat:   1.0,
			wantText:  1.0,
			wantTime:  0,
			wantDays:  15,
			wantScore: 0.85,
		},
		{
			// ★ 判据「title/desc 各占一半的加权」：标题一模一样、描述毫不相干 → 0.5
			name:      "文本加权：标题全同、描述全无重合 → S_text = 0.5",
			lost:      lost("黑色钱包", "牛皮的里面有几张卡", 31, 5, lastSeenAt, lostAt),
			found:     found("黑色钱包", "捡到的是充电宝", 31, 5, inWindow),
			wantCat:   1.0,
			wantText:  0.5,
			wantTime:  1.0,
			wantScore: 0.8,
		},
		{
			// ★ 判据「单字符 fallback」
			name:      "单字符标题：两个都是「锁」→ 1.0",
			lost:      lost("锁", "", 31, 5, lastSeenAt, lostAt),
			found:     found("锁", "", 31, 5, inWindow),
			wantCat:   1.0,
			wantText:  0.5, // 标题 1.0、描述两边都空 → 0.5*1+0.5*0
			wantTime:  1.0,
			wantScore: 0.8,
		},
		{
			// 两边描述都空 → desc dice 必须是 0 而不是 1。
			// 「同样空白」不是相似，是没信息。见 text.go 的 bigramSet 注释。
			name:      "两边描述都是空串 → 描述那半分不给",
			lost:      lost("黑色钱包", "", 31, 5, lastSeenAt, lostAt),
			found:     found("黑色钱包", "", 31, 5, inWindow),
			wantCat:   1.0,
			wantText:  0.5,
			wantTime:  1.0,
			wantScore: 0.8,
		},
		{
			// ★ 判据「『黑色钱包』vs『捡到黑色长款钱包』的 dice 已知值」在打分链路里的落点。
			// title_dice = 0.4（「黑色」「钱包」两个词都被抓到），描述都空 →
			// S_text = 0.5*0.4 + 0.5*0 = 0.2 → 0.45+0.08+0.15 = 0.68，落在展示档。
			name:      "黑色钱包 vs 捡到黑色长款钱包：0.68（不通知，但会展示）",
			lost:      lost("黑色钱包", "", 31, 5, lastSeenAt, lostAt),
			found:     found("捡到黑色长款钱包", "", 31, 5, inWindow),
			wantCat:   1.0,
			wantText:  0.2,
			wantTime:  1.0,
			wantScore: 0.68,
		},
		{
			// 祖先 id 为 0 是「没有这一级」，不是「同一级」。
			// 两个都没挂大类的脏数据如果算同大类，就会凭空拿 0.5 挤掉真匹配。
			name:      "两个大类 id 都是 0 → 不给半分，算完全不同",
			lost:      lost("黑色钱包", "里有一些证件", 31, 0, lastSeenAt, lostAt),
			found:     found("黑色钱包", "里有一些证件", 32, 0, inWindow),
			wantCat:   0.0,
			wantText:  1.0,
			wantTime:  1.0,
			wantScore: 0.55,
		},
		{
			// 时间锚点缺失（found_at 为 NULL）→ S_time 0，而不是 panic。
			// 库里 items_time_semantics CHECK 保证不会发生，这里守的是纯函数本身的健壮性。
			name:      "found_at 缺失 → S_time = 0，不 panic",
			lost:      lost("黑色钱包", "里有一些证件", 31, 5, lastSeenAt, lostAt),
			found:     found("黑色钱包", "里有一些证件", 31, 5, ""),
			wantCat:   1.0,
			wantText:  1.0,
			wantTime:  0,
			wantScore: 0.85,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := ScorePair(c.lost, c.found, Tier1, Options{})
			b := r.Breakdown

			if !almostEqual(b.Signals.Category.Score, c.wantCat) {
				t.Errorf("S_cat = %v，期望 %v", b.Signals.Category.Score, c.wantCat)
			}
			if !almostEqual(b.Signals.Text.Score, c.wantText) {
				t.Errorf("S_text = %v，期望 %v（title_dice=%v desc_dice=%v）",
					b.Signals.Text.Score, c.wantText, b.Signals.Text.TitleDice, b.Signals.Text.DescDice)
			}
			if !almostEqual(b.Signals.Time.Score, c.wantTime) {
				t.Errorf("S_time = %v，期望 %v", b.Signals.Time.Score, c.wantTime)
			}
			// in_loss_window 期望值直接从 wantTime 推：三档里只有「落在窗口内」给满分 1.0，
			// 衰减档和早于窗口档都给不出 1.0。所以这一行断言的其实是
			// **「满分只可能来自落在窗口内」这条档位设计本身**，比逐行手填更值得测。
			// （理论上衰减档在 elapsed < 约 1 分钟时也会被 round4 舍成 1.0，本表没有这种行。）
			if wantIn := almostEqual(c.wantTime, 1.0); b.Signals.Time.InLossWindow != wantIn {
				t.Errorf("in_loss_window = %v，期望 %v（S_time=%v）",
					b.Signals.Time.InLossWindow, wantIn, c.wantTime)
			}
			if b.Signals.Time.DaysAfterLostAt != c.wantDays {
				t.Errorf("days_after_lost_at = %d，期望 %d", b.Signals.Time.DaysAfterLostAt, c.wantDays)
			}
			if !almostEqual(b.Score, c.wantScore) {
				t.Errorf("总分 = %v，期望 %v\n分解=%#v", b.Score, c.wantScore, b.Signals)
			}
			if b.Tier != Tier1 {
				t.Errorf("tier = %d，期望 %d", b.Tier, Tier1)
			}
			// 分数必须落在 [0,1]：match_pairs.score 那一列有 CHECK 守着，
			// 越界的分数会在写台账时报 23514，而那时已经离原因很远了。
			if b.Score < 0 || b.Score > 1 {
				t.Errorf("总分 %v 越界 [0,1]", b.Score)
			}
		})
	}
}

// TestBreakdownHandCheck 验证「前端可以直接验算」（§5.7）这件事是真的：
// 把 breakdown 里给出的权重和分量手工加权求和，必须等于给出的总分。
func TestBreakdownHandCheck(t *testing.T) {
	pairs := []struct{ lost, found Item }{
		{lost("黑色钱包", "里有一些证件", 31, 5, lastSeenAt, lostAt), found("捡到黑色长款钱包", "黑色钱包里面有一些证件", 31, 5, oneDayLate)},
		{lost("学生证", "名字小李", 11, 2, lastSeenAt, lostAt), found("捡到一张学生证", "小李的同学", 11, 2, beforeWin)},
	}
	for tier, weights := range map[int][3]float64{
		Tier1: {WeightCategoryTier1, WeightTextTier1, WeightTimeTier1},
		Tier2: {WeightCategoryTier2, WeightTextTier2, WeightTimeTier2},
	} {
		for _, p := range pairs {
			b := ScorePair(p.lost, p.found, tier, Options{}).Breakdown
			sum := weights[0]*b.Signals.Category.Score + weights[1]*b.Signals.Text.Score + weights[2]*b.Signals.Time.Score
			if b.Signals.Location != nil {
				sum += WeightLocationTier2 * b.Signals.Location.Score
			}
			if math.Abs(round4(sum)-b.Score) > 1e-9 {
				t.Errorf("tier %d 手算 %v 和 breakdown 里的 %v 对不上", tier, sum, b.Score)
			}
		}
	}
}

// TestTierWeightsSumToOne 是那条「只剩三个信号」的算术保证。
//
// 权重之和不为 1 的话，[0,1] 的值域承诺就是假的，而 §5.8 那条
// 「0.45*1.0 + 0.40*0.71 + 0.15*0.93 = 0.87」的示例还算对得上，只有全满分时才露馅。
func TestTierWeightsSumToOne(t *testing.T) {
	t1 := WeightCategoryTier1 + WeightTextTier1 + WeightTimeTier1
	if math.Abs(t1-1) > 1e-12 {
		t.Errorf("Tier 1 权重和 = %v，应为 1", t1)
	}
	t2 := WeightCategoryTier2 + WeightTextTier2 + WeightTimeTier2 + WeightLocationTier2
	if math.Abs(t2-1) > 1e-12 {
		t.Errorf("Tier 2 权重和 = %v，应为 1", t2)
	}
	// §5.5 的两条明文：时间从 0.15 涨到 0.20，分类和文本都比 Tier 1 低
	if !(WeightTimeTier2 > WeightTimeTier1) {
		t.Error("Tier 2 的时间权重应当高于 Tier 1（地点放宽后时间成了唯一还能缩窄范围的硬条件）")
	}
	if !(WeightCategoryTier2 < WeightCategoryTier1 && WeightTextTier2 < WeightTextTier1) {
		t.Error("Tier 2 的分类和文本权重应当都低于 Tier 1（要给 S_loc 腾出权重）")
	}
	// §5.2 的优先级：先分类，再分词
	if !(WeightCategoryTier1 > WeightTextTier1) {
		t.Error("分类权重必须严格高于文本权重")
	}
}

// ---------- Tier 2 的地点四档 ----------

func TestTier2LocationSignal(t *testing.T) {
	cases := []struct {
		name             string
		locF, parF, topF int64
		detailF          string
		want             float64
		wantMatch        string
	}{
		{name: "同一叶子（都是图书馆）", locF: 57, parF: 25, topF: 2, want: LocationSameLeaf, wantMatch: LocationSameLeafName},
		{name: "同二级子类", locF: 58, parF: 25, topF: 2, want: LocationSameSecond, wantMatch: LocationSameSecondName},
		{name: "同一级区", locF: 59, parF: 26, topF: 2, want: LocationSameTop, wantMatch: LocationSameTopName},
		// 完全不同的区：兜到 location_detail 的文本 dice，再打四折。
		// 「三楼自习室b区」vs「三楼 自习室 B 区」归一化后一模一样 → dice 1 → 0.4
		{name: "全不命中，用自由文本兜底", locF: 60, parF: 27, topF: 3, detailF: "三楼 自习室 B 区", want: LocationTextDiscount, wantMatch: LocationTextName},
		// 祖先为 0 不算同一级（同分类那条规矩）
		{name: "二级祖先都是 0 → 落到文本兜底", locF: 58, parF: 0, topF: 0, detailF: "完全不同的地方", want: 0, wantMatch: LocationTextName},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l := lost("黑色钱包", "里有一些证件", 31, 5, lastSeenAt, lostAt)
			l.LocationDetail = "三楼自习室b区"
			f := found("黑色钱包", "里有一些证件", 31, 5, inWindow)
			f.LocationID, f.LocationParent, f.LocationTop = c.locF, c.parF, c.topF
			f.LocationDetail = c.detailF

			b := ScorePair(l, f, Tier2, Options{}).Breakdown
			if b.Signals.Location == nil {
				t.Fatal("Tier 2 必须有第四个信号 location")
			}
			if !almostEqual(b.Signals.Location.Score, c.want) {
				t.Errorf("S_loc = %v，期望 %v（detail L=%q F=%q）",
					b.Signals.Location.Score, c.want, l.LocationDetail, f.LocationDetail)
			}
			if b.Signals.Location.MatchedBy != c.wantMatch {
				t.Errorf("matched_by = %q，期望 %q", b.Signals.Location.MatchedBy, c.wantMatch)
			}
			if b.Signals.Location.Weight != WeightLocationTier2 {
				t.Errorf("地点权重 = %v，期望 %v", b.Signals.Location.Weight, WeightLocationTier2)
			}
		})
	}
}

// ---------- breakdown 的 JSON 形状（M3 判据在数字面上的那半条） ----------

// TestBreakdownJSONShape 断言的是三件事，全都是判据/§5.4 明文要求的：
//  1. Tier 1 恰好三个信号，键名 category/text/time
//  2. Tier 2 四个，多出来的是 location
//  3. **两个档位里都不许出现 attr 键** —— 它随 color/brand 一起删了，
//     留在 JSON 里会让前端和单测误以为那个信号还存在
func TestBreakdownJSONShape(t *testing.T) {
	l := lost("黑色钱包", "里有一些证件", 31, 5, lastSeenAt, lostAt)
	f := found("捡到黑色长款钱包", "黑色钱包里面有一些证件", 31, 5, inWindow)

	for _, c := range []struct {
		tier int
		want []string
	}{
		{Tier1, []string{"category", "text", "time"}},
		{Tier2, []string{"category", "location", "text", "time"}},
	} {
		raw, err := json.Marshal(ScorePair(l, f, c.tier, Options{}).Breakdown)
		if err != nil {
			t.Fatalf("序列化 breakdown: %v", err)
		}
		s := string(raw)
		if strings.Contains(s, "attr") {
			t.Errorf("tier %d 的 breakdown 里出现了 attr 键（§5.4 明令禁止）：%s", c.tier, s)
		}
		if strings.Contains(s, "color") || strings.Contains(s, "brand") {
			t.Errorf("tier %d 的 breakdown 里出现了已删除的属性列：%s", c.tier, s)
		}

		var parsed struct {
			Score   float64        `json:"score"`
			Tier    int            `json:"tier"`
			Signals map[string]any `json:"signals"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("反序列化: %v", err)
		}
		if len(parsed.Signals) != len(c.want) {
			t.Errorf("tier %d 有 %d 个信号，期望 %d 个：%s", c.tier, len(parsed.Signals), len(c.want), s)
		}
		for _, k := range c.want {
			if _, ok := parsed.Signals[k]; !ok {
				t.Errorf("tier %d 缺信号键 %q：%s", c.tier, k, s)
			}
		}
		if parsed.Tier != c.tier {
			t.Errorf("tier 字段 = %d，期望 %d", parsed.Tier, c.tier)
		}
		// §5.7 的形状：顶层就是 score / tier / signals 三个键
		var top map[string]any
		_ = json.Unmarshal(raw, &top)
		if len(top) != 3 {
			t.Errorf("breakdown 顶层应有 3 个键（score/tier/signals），实际 %d 个：%s", len(top), s)
		}
	}
}

// TestTier1ExcludesLocationIsNotJustZero 补一刀：Tier 1 的地点不是「算了但得 0」，
// 是**根本没参与**。如果实现是「算了但权重给 0」，JSON 里会有 location 键，
// 上面那个测试能抓到；这里再直接断一次指针为 nil，让错误信息更明确。
func TestTier1ExcludesLocationIsNotJustZero(t *testing.T) {
	b := ScorePair(
		lost("黑色钱包", "", 31, 5, lastSeenAt, lostAt),
		found("黑色钱包", "", 31, 5, inWindow),
		Tier1, Options{},
	).Breakdown
	if b.Signals.Location != nil {
		t.Errorf("Tier 1 不该有地点信号（SQL 已经硬筛过 location_id），实际是 %#v", *b.Signals.Location)
	}
}

// ---------- Tier 1 资格 ----------

func TestCanTier1(t *testing.T) {
	cases := []struct {
		name string
		in   Item
		want bool
	}{
		{"lost + 正常地点 + 有 last_seen_at", lost("钱包", "", 31, 5, lastSeenAt, lostAt), true},
		{"found + 正常地点 + 有 found_at", found("钱包", "", 31, 5, inWindow), true},
		{"lost 地点是「其他」（freeform）", withFreeform(lost("钱包", "", 31, 5, lastSeenAt, lostAt)), false},
		{"found 地点是「其他」", withFreeform(found("钱包", "", 31, 5, inWindow)), false},
		{"lost 但没有 last_seen_at（时间硬筛选写不出来）", withLostTimes(lost("钱包", "", 31, 5, lastSeenAt, lostAt), nil, tp(lostAt)), false},
		{"found 但没有 found_at", withFoundTime(found("钱包", "", 31, 5, inWindow), nil), false},
		{"类型不认识", Item{ItemType: "something"}, false},
	}
	for _, c := range cases {
		if got := CanTier1(c.in); got != c.want {
			t.Errorf("CanTier1(%s) = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func withFreeform(in Item) Item { in.LocationIsFreeform = true; return in }

func withLostTimes(in Item, seen, at *time.Time) Item {
	in.LastSeenAt, in.LostAt = seen, at
	return in
}

func withFoundTime(in Item, at *time.Time) Item { in.FoundAt = at; return in }

// ---------- BestMatches：排序、截断、方向 ----------

func TestBestMatches(t *testing.T) {
	target := lost("黑色钱包", "里有一些证件", 31, 5, lastSeenAt, lostAt)

	// 三个候选：一个同小类、一个同大类、一个跨大类。文本都一样，
	// 所以顺序完全由分类决定 —— 这正是「分类是最强粗筛信号」的断言。
	sameLeaf := found("黑色钱包", "里有一些证件", 31, 5, inWindow)
	sameLeaf.ID = 10
	sameParent := found("黑色钱包", "里有一些证件", 32, 5, inWindow)
	sameParent.ID = 11
	otherTop := found("黑色钱包", "里有一些证件", 8, 1, inWindow)
	otherTop.ID = 12

	got := BestMatches(target, []Item{otherTop, sameParent, sameLeaf}, Tier1, 10, Options{})
	if len(got) != 3 {
		t.Fatalf("返回 %d 条，期望 3 条", len(got))
	}
	wantIDs := []int64{10, 11, 12}
	wantScores := []float64{1.0, 0.775, 0.55}
	for i := range got {
		if got[i].ItemID != wantIDs[i] {
			t.Errorf("第 %d 名是 item %d，期望 %d", i+1, got[i].ItemID, wantIDs[i])
		}
		if !almostEqual(got[i].Breakdown.Score, wantScores[i]) {
			t.Errorf("第 %d 名分数 %v，期望 %v", i+1, got[i].Breakdown.Score, wantScores[i])
		}
	}

	// topN 截断
	if len(BestMatches(target, []Item{sameLeaf, sameParent, otherTop}, Tier1, 2, Options{})) != 2 {
		t.Error("topN=2 应该只返回 2 条")
	}
	// topN<=0 = 不截断（不是返回 0 条）
	if len(BestMatches(target, []Item{sameLeaf, sameParent}, Tier1, 0, Options{})) != 2 {
		t.Error("topN=0 应当返回全部，而不是空")
	}
	// 空候选集
	if len(BestMatches(target, nil, Tier1, 5, Options{})) != 0 {
		t.Error("空候选集应返回空列表")
	}
}

// TestBestMatchesTieBreakIsStable：同分候选按 id 升序。
// PG 不保证等值行的顺序，不加这个兜底的话同一页刷新两次顺序会变，
// 用户看到的是「排序坏了」，而代码看起来完全正确。
func TestBestMatchesTieBreakIsStable(t *testing.T) {
	target := lost("黑色钱包", "里有一些证件", 31, 5, lastSeenAt, lostAt)
	a := found("黑色钱包", "里有一些证件", 8, 1, inWindow)
	a.ID = 99
	b := found("黑色钱包", "里有一些证件", 9, 1, inWindow)
	b.ID = 3
	if got := BestMatches(target, []Item{a, b}, Tier1, 0, Options{}); got[0].ItemID != 3 {
		t.Errorf("同分时应按 id 升序，实际第一名是 %d", got[0].ItemID)
	}
	if got := BestMatches(target, []Item{b, a}, Tier1, 0, Options{}); got[0].ItemID != 3 {
		t.Errorf("候选顺序不影响结果，实际第一名是 %d", got[0].ItemID)
	}
}

// TestBestMatchesHandlesFoundTarget 验证方向被摆正了：
// 目标是 found 时，候选是 lost，打分仍然必须是 (lost, found) 的顺序。
// 这条测试存在的意义：如果 BestMatches 忘了摆正，S_time 会拿 found 的窗口
// 去比 lost 的时间点，得到一个「看起来合理」的错分数 —— 只有这一条能发现它。
func TestBestMatchesHandlesFoundTarget(t *testing.T) {
	// 早于窗口的 found：直接 ScorePair 得到 0.3 那一档
	direct := ScorePair(
		lost("黑色钱包", "里有一些证件", 31, 5, lastSeenAt, lostAt),
		found("黑色钱包", "里有一些证件", 31, 5, beforeWin),
		Tier1, Options{},
	)
	if !almostEqual(direct.Breakdown.Signals.Time.Score, TimeBeforeWindow) {
		t.Fatalf("前置条件坏了：直接打分应得 %v，实得 %v", TimeBeforeWindow, direct.Breakdown.Signals.Time.Score)
	}

	// 同一对，但这次是 found 当目标、lost 当候选（#13 发 found 帖走的就是这个方向）
	fTarget := found("黑色钱包", "里有一些证件", 31, 5, beforeWin)
	fTarget.ID = 2
	cand := lost("黑色钱包", "里有一些证件", 31, 5, lastSeenAt, lostAt)
	cand.ID = 1
	viaTarget := BestMatches(fTarget, []Item{cand}, Tier1, 5, Options{})
	if len(viaTarget) != 1 {
		t.Fatalf("返回 %d 条", len(viaTarget))
	}
	if viaTarget[0].Breakdown != direct.Breakdown {
		t.Errorf("两个方向算出的 breakdown 不一致：\n found 为目标=%#v\n 直接打分=%#v",
			viaTarget[0].Breakdown, direct.Breakdown)
	}
	if viaTarget[0].ItemID != cand.ID {
		t.Errorf("ItemID 应指向候选 %d，实得 %d", cand.ID, viaTarget[0].ItemID)
	}
}

// TestOptionsDecayIsHonored：MATCH_DECAY_DAYS 是真的进了公式的。
// 把衰减改成 1 天，lost_at + 24h 之后就该归零。
func TestOptionsDecayIsHonored(t *testing.T) {
	l := lost("黑色钱包", "里有一些证件", 31, 5, lastSeenAt, lostAt)
	f := found("黑色钱包", "里有一些证件", 31, 5, oneDayLate)

	def := ScorePair(l, f, Tier1, Options{}).Breakdown.Signals.Time.Score
	if !almostEqual(def, 0.9286) {
		t.Errorf("默认 14 天衰减下应为 0.9286，实得 %v", def)
	}
	short := ScorePair(l, f, Tier1, Options{Decay: 24 * time.Hour}).Breakdown.Signals.Time.Score
	if short != 0 {
		t.Errorf("衰减设成 1 天时应为 0，实得 %v", short)
	}
	// Options{} 的零值必须走默认值，而不是除零得到 NaN/Inf
	if math.IsNaN(def) || math.IsInf(def, 0) {
		t.Error("零值 Options 产生了 NaN/Inf")
	}
}

// TestScoresAreInRange 把整个分数空间扫一遍。
//
// match_pairs.score 是 NUMERIC(5,4) 且有 CHECK (score >= 0 AND score <= 1)，
// 越界会在写台账时报 23514 —— 那时错误信息里只有 SQLSTATE，没有「哪个信号算错了」。
// 所以在纯函数这一层用暴力扫描钉住值域，比在集成测试里撞上一次约束违规便宜得多。
func TestScoresAreInRange(t *testing.T) {
	fixed := []Item{
		lost("黑色钱包", "里有一些证件", 31, 5, lastSeenAt, lostAt),
		lost("锁", "", 0, 0, "", ""),
		found("捡到黑色长款钱包", "黑色钱包里面有一些证件", 31, 5, pastDecay),
		found("", "", 32, 5, ""),
	}
	for _, tier := range []int{Tier1, Tier2} {
		for _, l := range fixed {
			for _, f := range fixed {
				b := ScorePair(l, f, tier, Options{}).Breakdown
				if math.IsNaN(b.Score) || math.IsInf(b.Score, 0) {
					t.Fatalf("tier %d 出现 NaN/Inf: %#v", tier, b)
				}
				if b.Score < 0 || b.Score > 1 {
					t.Errorf("tier %d 分数越界 %v（lost=%#v found=%#v）", tier, b.Score, l, f)
				}
			}
		}
	}
}

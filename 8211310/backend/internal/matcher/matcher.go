// Package matcher 是匹配算法的打分内核：纯函数，零 DB、零 HTTP 依赖。
//
// 「零依赖」不是洁癖，是整个系统里最容易测的部分能成立的前提（§10 第①层）：
// 「同小类≈1.0」「14 天外为 0」「中文 bigram 已知值」这些判据，
// 起一次真库要跑完整迁移、要插种子数据、失败时还分不清是分数错了还是数据脏了。
// 在 matcher 里它们只是一个 table-driven test 的一行，毫秒级跑完。
//
// 所以这个包**不 import 项目内任何包**：候选从哪来、算完写哪张表、给谁发通知，
// 全是 service/match.go 的事。
package matcher

import (
	"math"
	"sort"
	"time"
)

// Item 是打分需要的全部字段 —— 它是 model.ItemDetail 的**投影**，不是副本。
//
// 只列打分用得到的，意味着两件事：
//   - 读代码的人一眼看出「匹配只看这些信息」。status、view_count、图片那些确实不参与打分。
//   - model 加字段时 matcher 不会跟着变（绝大多数新字段和匹配无关）。
//
// 四个祖先 id（CategoryParent / LocationParent / LocationTop）是必须带上来的：
// 半分（同大类 0.5）和三档地点（同二级 0.6 / 同一级区 0.3）都要比祖先，
// 而 items 表里只有叶子 id。谁填谁 0 分。用 0 而不是 -1 表示「没有祖先」，
// 因为 0 在数据库里本来就不可能是任何一行的 id，比较时一句 != 0 就挡掉了。
type Item struct {
	ID             int64
	ItemType       string
	Title          string
	Description    string
	LocationDetail string

	CategoryID     int64
	CategoryParent int64

	LocationID         int64
	LocationParent     int64
	LocationTop        int64
	LocationIsFreeform bool

	// 三个时间列都可空，用 *time.Time 和 model 保持一致：
	// 「这一列不该有值」和「1 年 1 月 1 日」必须能区分开。
	LastSeenAt *time.Time
	LostAt     *time.Time
	FoundAt    *time.Time
}

// ---------- 对外结构（也是 match_pairs.breakdown 的 JSONB 形状）----------

// CategorySignal 是分类信号的分解。
type CategorySignal struct {
	Weight   float64 `json:"weight"`
	Score    float64 `json:"score"`
	SameLeaf bool    `json:"same_leaf"`
}

// TextSignal 是文本信号的分解。两个 dice 单独摊开是这套 breakdown 作为
// 「调试器」的核心价值：分数低的时候，一眼能看出是标题不像还是描述没写。
type TextSignal struct {
	Weight    float64 `json:"weight"`
	Score     float64 `json:"score"`
	TitleDice float64 `json:"title_dice"`
	DescDice  float64 `json:"desc_dice"`
}

// TimeSignal 是时间信号的分解。
//
// 两个布尔/整数字段**恒定出现**（不适用时是 false 和 0），不是 omitempty：
// 前端要按固定形状渲染，缺键会让它显示成 undefined。
// days_after_lost_at 只在衰减档有意义，落在窗口内时为 0。
type TimeSignal struct {
	Weight          float64 `json:"weight"`
	Score           float64 `json:"score"`
	InLossWindow    bool    `json:"in_loss_window"`
	DaysAfterLostAt int     `json:"days_after_lost_at"`
}

// LocationSignal 是地点信号的分解。matched_by 说清是哪一档命中的：
// same_leaf / same_second / same_top / detail_text。
type LocationSignal struct {
	Weight    float64 `json:"weight"`
	Score     float64 `json:"score"`
	MatchedBy string  `json:"matched_by"`
}

// Signals 是分解表。
//
// **没有 Attr 字段、没有 attr 键**（§5.4）：S_attr 随 color/brand 两列一起删了，
// 这里留一个空壳会让前端和单测误以为那个信号还存在。
//
// Location 是指针而不是值：Tier 1 只有三个信号，硬塞一个 weight 0.15 的地点
// 会让 breakdown 的加权和对不上总分。omitempty 之后 Tier 1 的 JSON 里
// 这个键根本不存在，前端数键数就能判断档位。
type Signals struct {
	Category CategorySignal  `json:"category"`
	Text     TextSignal      `json:"text"`
	Time     TimeSignal      `json:"time"`
	Location *LocationSignal `json:"location,omitempty"`
}

// Breakdown 是 §5.7 那个对象。ScorePair 算出来的东西和落库的东西是同一个，
// 所以「响应里看到的分数」和「台账里记的分数」不可能对不上。
type Breakdown struct {
	Score   float64 `json:"score"`
	Tier    int     `json:"tier"`
	Signals Signals `json:"signals"`
}

// Result 是一条候选的打分结果。
//
// ItemID 是**对侧那条帖子**的 id（给 lost 目标打分时是 found 候选的 id，反之亦然）。
// service 拿它去 map 里找回完整的 model.ItemDetail 拼响应摘要。
type Result struct {
	ItemID    int64     `json:"-"`
	Breakdown Breakdown `json:"breakdown"`
}

// ---------- 对外函数 ----------

// CanTier1 报告这个目标帖能不能走严格模式。
//
// 两个前提（§5.2）：地点不是 is_freeform=true 的「其他」（没有叶子节点可比），
// 且自己的时间锚点在（lost 看 last_seen_at，found 看 found_at）——
// 时间硬筛选的 SQL 条件写得出来才叫严格模式。
//
// 它是 auto 分档、显式 ?tier=1 的合法性检查、以及 #13 发帖后要不要跑匹配的共同依据。
// 三处各自判断一遍迟早会写出三个版本，所以只留这一个函数。
func CanTier1(target Item) bool {
	if target.LocationIsFreeform {
		return false
	}
	switch target.ItemType {
	case TypeLost:
		return target.LastSeenAt != nil
	case TypeFound:
		return target.FoundAt != nil
	}
	return false
}

// ScorePair 给一对 (lost, found) 打分，返回带完整分解的结果。
//
// ⚠ 参数顺序固定：lost 在前。调用方负责按 ItemType 摆正。
// 因为 S_time 的公式是**非对称**的 —— 丢失窗口 [last_seen_at, lost_at] 属于 lost 帖，
// 拾获时间点 found_at 属于 found 帖。把两个参数对调不会报错，
// 只会算出一个「时间看起来完全合理」的错分数，这类错没有编译期保护。
func ScorePair(lost, found Item, tier int, opt Options) Result {
	sCat, sameLeaf := categoryScore(lost, found)
	sText, titleDice, descDice := textScore(lost, found)
	sTime, inWindow, daysAfter := timeScore(lost, found, opt)

	// 先各自四舍五入再加权，总分也从**舍入后的分量**算（见 round4 的注释）。
	wCat, wText, wTime := WeightCategoryTier1, WeightTextTier1, WeightTimeTier1
	if tier == Tier2 {
		wCat, wText, wTime = WeightCategoryTier2, WeightTextTier2, WeightTimeTier2
	}
	sCat, sText, sTime = round4(sCat), round4(sText), round4(sTime)

	signals := Signals{
		Category: CategorySignal{Weight: wCat, Score: sCat, SameLeaf: sameLeaf},
		Text:     TextSignal{Weight: wText, Score: sText, TitleDice: titleDice, DescDice: descDice},
		Time:     TimeSignal{Weight: wTime, Score: sTime, InLossWindow: inWindow, DaysAfterLostAt: daysAfter},
	}

	score := wCat*sCat + wText*sText + wTime*sTime

	// Tier 1 的 breakdown 里**恰好三个信号**（M3 判据就是数字面上的「没有 attr 键」）。
	// 权重 0.45/0.40/0.15 三条加起来正好是 1.0，所以分数天然落在 [0,1]，
	// 不需要最后再 clamp 一次。
	if tier == Tier2 {
		sLoc, matchedBy := locationScore(lost, found)
		sLoc = round4(sLoc)
		signals.Location = &LocationSignal{Weight: WeightLocationTier2, Score: sLoc, MatchedBy: matchedBy}
		score += WeightLocationTier2 * sLoc
	}

	return Result{Breakdown: Breakdown{Score: round4(score), Tier: tier, Signals: signals}}
}

// BestMatches 给整个候选集打分、按分数降序排、截断到 topN。
//
// 三件事按这个顺序做，缺一不可：
//   - **全部打分，不中途丢弃低分候选**（§5.1 第三步：分数只决定顺序，不决定去留）。
//     低于展示线的候选是在 service 按 min_score 筛的，那是运营阈值不是算法；
//     在算法里筛就会把「分类选错但确实是同一个东西」的帖子彻底变隐形。
//   - 排序对分数完全相同的候选按 id 升序兜底。PG 不保证等值行的返回顺序，
//     不加这一条，同一个请求刷新两次顺序可能不同，前端看着就像「排序坏了」。
//   - topN <= 0 表示不截断（返回全部），不是返回 0 条。
func BestMatches(target Item, candidates []Item, tier int, topN int, opt Options) []Result {
	out := make([]Result, 0, len(candidates))
	for _, c := range candidates {
		// 摆正方向。候选集由 SQL 的 `item_type = 对侧` 保证一定和目标异型，
		// 所以这里没有「同型怎么办」的分支要写。
		var lost, found Item
		if target.ItemType == TypeLost {
			lost, found = target, c
		} else {
			lost, found = c, target
		}
		r := ScorePair(lost, found, tier, opt)
		r.ItemID = c.ID
		out = append(out, r)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Breakdown.Score != out[j].Breakdown.Score {
			return out[i].Breakdown.Score > out[j].Breakdown.Score
		}
		return out[i].ItemID < out[j].ItemID
	})

	if topN > 0 && len(out) > topN {
		out = out[:topN]
	}
	return out
}

// ---------- 四个信号 ----------

// categoryScore 返回分数和「是不是同小类」。
//
// 同小类 → 1.0；同大类不同小类 → 0.5；不同大类 → 0.0。
// != 0 那个判断不是防御性代码，是业务规则：**两个都没有祖先的条目不算同大类**。
// 大类 id 为 0 意味着这条帖子的分类连一级都没挂上（脏数据或种子以外的分类），
// 这时候给 0.5 等于凭空送出半分，真匹配会被这种假半分挤下去。
func categoryScore(lost, found Item) (float64, bool) {
	if lost.CategoryID == found.CategoryID {
		return CategorySameLeaf, true
	}
	if lost.CategoryParent != 0 && lost.CategoryParent == found.CategoryParent {
		return CategorySameParent, false
	}
	return CategoryDifferent, false
}

// textScore = 0.5*dice(title) + 0.5*dice(description)。
//
// **不含 location_detail**（§5.2）：那段文本在 Tier 1 是硬筛选的补充说明，
// 在 Tier 2 由 S_loc 单独负责。算进文本里会让「地点写得像」占两次便宜。
//
// 各占一半是不打算调的经验值，但它在 breakdown 里是可验证的：
// title_dice 和 desc_dice 分开透出，任何人手工乘一遍就能对上 S_text。
func textScore(lost, found Item) (float64, float64, float64) {
	td := round4(textSimilarity(lost.Title, found.Title))
	dd := round4(textSimilarity(lost.Description, found.Description))
	return 0.5*td + 0.5*dd, td, dd
}

// timeScore 用满两个时间字段（§5.3），返回分数、是否落在丢失窗口内、以及
// 捡到时间距 lost_at 的整天数。
//
// 三档：
//
//	found_at ∈ [last_seen_at, lost_at]        → 1.0  最佳：在你意识到丢之前就被捡到了
//	found_at > lost_at                        → max(0, 1 - (found_at - lost_at) / 14天)
//	found_at < last_seen_at（容差内才可能到）→ 0.3  重罚但不清零
//
// lost_at 的全部作用是把衰减的**起点**从 last_seen_at 推到 lost_at：
// 周一 8 点还拿着、周二 15 点才发现丢，周三捡到 —— 从 lost_at 起算只差 1 天（≈0.93），
// 从 last_seen_at 起算是差 2 天（≈0.86）。同一个事实，后一种算法无谓地压低了真匹配。
func timeScore(lost, found Item, opt Options) (float64, bool, int) {
	if found.FoundAt == nil || lost.LastSeenAt == nil {
		return 0, false, 0
	}
	// 窗口右端用 lost_at；它为空时退回 last_seen_at。
	// 库里 items_time_semantics CHECK 保证 lost 帖两列都有值，所以这个回落
	// 只为纯函数在单测的残缺输入下也不 panic 而存在，不是业务分支。
	right := *lost.LastSeenAt
	if lost.LostAt != nil {
		right = *lost.LostAt
	}

	foundAt := *found.FoundAt
	switch {
	case foundAt.Before(*lost.LastSeenAt):
		return TimeBeforeWindow, false, 0
	case !foundAt.After(right):
		return 1.0, true, 0
	}

	elapsed := foundAt.Sub(right)
	// 两个 Duration 相除在 Go 里得到的是**整数 Duration（截断）**，不是比值：
	// 直接写 elapsed/decay 会得到「14 天的窗口相除恒为 0」，于是分数永远是 1，
	// 而代码看起来完全正确。必须先转 float64。
	score := 1 - float64(elapsed)/float64(opt.decay())
	if score < 0 {
		score = 0
	}
	// days 只进 breakdown 给人看，不进公式：公式用的是时长本身，
	// 用整天数算会把 23 小时和 1 小时判成同一个分。
	return score, false, int(elapsed.Hours() / 24)
}

// locationScore 是 Tier 2 独有的地点信号，四档递降，最后兜到自由文本。
//
// 前三档全靠字典 id 比较（定位原则：字典把自由文本变成可比的整数）：
// 同一叶子 → 1.0；同二级子类（「图书馆」vs「行政楼」都在教学区场馆）→ 0.6；
// 同一级区 → 0.3。全对不上才去比 location_detail 的 dice 并打四折。
//
// 中间两档同样有 != 0 的门槛：祖先 id 为 0 是「没有这一级」，不是「同一级」。
func locationScore(lost, found Item) (float64, string) {
	switch {
	case lost.LocationID == found.LocationID:
		return LocationSameLeaf, LocationSameLeafName
	case lost.LocationParent != 0 && lost.LocationParent == found.LocationParent:
		return LocationSameSecond, LocationSameSecondName
	case lost.LocationTop != 0 && lost.LocationTop == found.LocationTop:
		return LocationSameTop, LocationSameTopName
	}
	return LocationTextDiscount * textSimilarity(lost.LocationDetail, found.LocationDetail), LocationTextName
}

// round4 四舍五入到四位小数。
//
// 两个原因：
//  1. match_pairs.score 列是 NUMERIC(5,4)，PG 会把第六位起抹掉。
//     如果在落库前自己舍入一次，breakdown 里的 score 和表里的 score 就是同一个数；
//     不舍入的话调试时会看到「台账里 0.8735，响应里 0.8734999999999999」。
//  2. breakdown 是给人验算的（§5.7 举的例子就是手算 0.45+0.284+0.140）。
//     分量先舍、总分再从舍过的分量算，所以「权重 × 分量」手加的结果和总分对得上。
//     代价是总分可能有万分之一的误差 —— 换成用未舍入的分量算，人就算不出来了，
//     而 breakdown 存在的意义恰恰是人能算出来。
func round4(f float64) float64 {
	return math.Round(f*10000) / 10000
}

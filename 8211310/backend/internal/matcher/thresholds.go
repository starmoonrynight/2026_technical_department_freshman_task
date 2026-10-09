package matcher

import "time"

// 这里集中了匹配算法的**全部权重和档位常量**（§5.8、§15）。
// 调参只动这一个文件：公式在 matcher.go，数字在这里，改哪个都不会误伤另一个。
//
// 唯一的例外是那四个走 .env 的数字（MATCH_NOTIFY_THRESHOLD / MATCH_SHOW_THRESHOLD /
// MATCH_TIME_TOLERANCE_HOURS / MATCH_DECAY_DAYS，见 config.MatchConfig）。
// 它们不在这里，是因为线上出现「通知太吵」时应该改一行环境变量重启就好，
// 不该要求人重新编译一次 Go。所以：
//   - 权重（信号之间怎么分配）→ 这个文件，改它等于改算法
//   - 阈值（算出来之后怎么用）→ .env，改它等于改运营策略
const (
	Tier1 = 1
	Tier2 = 2

	// 帖子的两种类型。数据库用 CHECK 钉死了这两个字面量（000001 迁移），
	// model 包里有同名常量；这里重复一次是因为 matcher 不 import 任何项目内包
	// —— 它必须能脱离整个工程单独编译、单独测。
	TypeLost  = "lost"
	TypeFound = "found"
)

// Tier 1 权重：0.45 / 0.40 / 0.15，只有三个信号。
//
// 地点在 Tier 1 **不参与打分**，因为 SQL 已经把它当硬条件筛过了
// （候选的 location_id 全等于目标的），一堆相同的值没有区分能力。
//
// 第 4 版这里还有第四个信号 S_attr（颜色+品牌，权重 0.15），随 color/brand 两列
// 一起删了（§5.4）。它原来的权重这样分掉：+0.10 给分类、+0.05 给文本、时间不动。
// 分类涨最多，因为删掉属性列之后它是唯一剩下的**结构化**信号（离散、无歧义、
// 字典表保证「同一个东西就是同一个 id」）；文本涨一点，因为颜色和品牌的词
// 现在只活在标题和描述里，S_text 事实上兼了 S_attr 的一部分活。
// 「分类严格高于文本」保住了用户定的优先级：先分类，再分词。
const (
	WeightCategoryTier1 = 0.45
	WeightTextTier1     = 0.40
	WeightTimeTier1     = 0.15
)

// Tier 2 权重：0.35 / 0.30 / 0.20 / 0.15，四个信号。
//
// 和 Tier 1 的两处差别都不是随手调的：
//   - 分类和文本各降一点，是为了给 S_loc 腾出 0.15 —— 地点在 Tier 2 从硬条件
//     降级成排序信号，不参与打分就等于假装它没被放宽过。
//   - 时间反而从 0.15 涨到 0.20：地点不再缩小范围之后，时间是唯一还能大幅
//     缩窄候选的东西，区分价值上升。
//
// 于是「分类 → 文本 → 时间 → 地点」严格递减，和排序优先级完全对应。
const (
	WeightCategoryTier2 = 0.35
	WeightTextTier2     = 0.30
	WeightTimeTier2     = 0.20
	WeightLocationTier2 = 0.15
)

// 分类信号：两级树 + 半分机制（§3.6）。
//
// 半分是「选错分类」的代价上限：一个人选「手机」、另一个人选「智能穿戴」，
// 同属数码电子大类 → 0.5，东西不会被排除，只是排后面。
// 选成完全不同大类 → 0.0，也还是 0.0 而已 —— 分类永远只做排序，不做筛选。
const (
	CategorySameLeaf   = 1.0
	CategorySameParent = 0.5
	CategoryDifferent  = 0.0
)

// 地点信号（只在 Tier 2 用）：三级树的三档命中 + 全不命中时的文本兜底。
//
// LocationTextDiscount 单独说明：两个人连「同一级区」都对不上（或者其中一个选了
// 「其他」），此时唯一还能比的是 location_detail 那段自由文本
// （「3楼自习室B区」vs「三楼自习室b区靠窗」）。它是用户手打的字，可信度低于
// 任何一级字典匹配，所以 dice 之后还要再打四折。
const (
	LocationSameLeaf       = 1.0
	LocationSameSecond     = 0.6
	LocationSameTop        = 0.3
	LocationTextDiscount   = 0.4
	LocationSameLeafName   = "same_leaf"
	LocationSameSecondName = "same_second"
	LocationSameTopName    = "same_top"
	LocationTextName       = "detail_text"
)

// TimeBeforeWindow：捡到时间**早于**「最后确认拥有」时给的分。
//
// 重罚但不清零（0.3 而不是 0）。理由是人会记错：周一 8 点还拿着钱包、
// 周二才发现丢了，是「周一 8 点」这个记忆本身可能偏了。
// 清零会让一对真匹配因为一个填表误差彻底消失，那是筛选而不是排序 —— 越界了。
//
// 能走到这一档的候选已经被 SQL 的时间硬筛选限制在容差之内（默认 1 天），
// 所以打分这里不需要再检查容差：超出容差的根本没进候选集。
const TimeBeforeWindow = 0.3

// PreviewTopN：#13 发 lost 帖时响应里 matches_preview 的条数（§4）。
const PreviewTopN = 5

// Options 是打分唯一的时间参数。
//
// 零值可用（Decay 缺省用 DefaultTimeDecay），这样 matcher 的表驱动单测
// 不需要为了打一个分去构造 config.Config。service 那边从 .env 读出来再传进来。
//
// 容差（MATCH_TIME_TOLERANCE_HOURS）**不在这里**：它只作用于 SQL 的候选筛选，
// 不参与任何打分公式 —— 放进 Options 会让人以为改它能影响分数，而它不能。
type Options struct {
	Decay time.Duration // found_at 晚于 lost_at 之后的衰减窗口
}

// DefaultTimeDecay 是 14 天：三周内捡到的还算合理，再晚就一路衰减到 0。
const DefaultTimeDecay = 14 * 24 * time.Hour

func (o Options) decay() time.Duration {
	if o.Decay > 0 {
		return o.Decay
	}
	return DefaultTimeDecay
}

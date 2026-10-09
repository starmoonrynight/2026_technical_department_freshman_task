package matcher

import (
	"testing"
)

// TestScoreDistribution 是 §12 M3 判据的后半句：
// 「再打印一批已知真/假匹配的分数分布，确认 0.75 和 0.55 两条线划得合理（§5.8）」。
//
// 为什么要有这个测试而不是只写断言：§5.8 明说了删掉 S_attr 之后**分数分布整体上移**
// （第 4 版里颜色填不一致能把一对真匹配从 0.874 压到 0.738，现在压不下去了），
// 副作用是通知量会比第 4 版多。这是一个只能靠看分布来判断的事，
// 光断言「每条都 > 0.75」不会告诉你「离阈值线还剩多少余量」。
//
// 所以这里做两件事：
//  1. 打印一批人造但真实的样本（含每对的分解），`go test -run TestScoreDistribution -v` 就能看
//  2. 按 §5.8 的三个分数带分别断言：**真匹配不低于展示线**（不能凭空消失）、
//     **明显不相干的对不超过展示线**（不该出现在用户面前）、
//     **同大类但不相干的对不超过通知线**（不该有人被推送）。
//     通知线不用来卡所有假匹配 —— 0.55–0.75 这个带子存在的意义就是
//     「有点可能但不确定，摆出来给人自己判断」。
//
// 阈值 0.75/0.55 在这里写成局部常量：线上真实值走 .env 的 MATCH_NOTIFY_THRESHOLD /
// MATCH_SHOW_THRESHOLD，默认值和这里一致（config.MatchConfig）。如果调参调高了，
// 这个测试的打印会立刻告诉你现在的样本分数分布够不够得着。
func TestScoreDistribution(t *testing.T) {
	const (
		notifyLine = 0.75
		showLine   = 0.55
	)

	samples := []struct {
		name string
		// kind 决定这一行受哪条阈值约束（三档，正好对应 §5.8 的三个分数带）：
		//   "真匹配"    → 必须 ≥ 展示线，否则它会从用户面前彻底消失
		//   "不该通知"  → 必须 < 通知线，否则就是打扰
		//   "绝不该出现" → 必须 < 展示线（跨大类且文本不重合的那类明显错误）
		//   "已知边界"  → 不断言，只打印，留给 §14-2 的观察指标
		kind        string
		lost, found Item
	}{
		{
			// §13 端到端场景的原样：小李周一 8 点最后拿着、周二 15 点发现丢，
			// 小王周一 12 点在图书馆捡到。描述互相能对上但不是一句话。
			name:  "★§13 主线：黑色钱包 @图书馆（周一捡到，落在丢失窗口内）",
			kind:  "真匹配",
			lost:  lost("黑色钱包", "钱包里有一些证件", 31, 5, lastSeenAt, lostAt),
			found: found("捡到黑色长款钱包", "黑色钱包里面有一些证件", 31, 5, inWindow),
		},
		{
			name:  "标题一字不差、描述一字不差（最理想的一对）",
			kind:  "真匹配",
			lost:  lost("黑色钱包", "牛皮长款，里面有几张卡", 31, 5, lastSeenAt, lostAt),
			found: found("黑色钱包", "牛皮长款，里面有几张卡", 31, 5, inWindow),
		},
		{
			name:  "同大类不同小类（钱包 vs 手提袋）+ 文本部分重合",
			kind:  "真匹配",
			lost:  lost("黑色钱包", "长款的", 31, 5, lastSeenAt, lostAt),
			found: found("黑色手提袋", "长款的", 32, 5, inWindow),
		},
		{
			// 这就是「真匹配但写得很省」的样子：只有标题、描述全空。
			// 它落在展示档而不是通知档，是对的 —— 一个字的信息量不该换来一次推送。
			name:  "真匹配但描述都没写（只有标题的 0.4 dice）",
			kind:  "真匹配",
			lost:  lost("黑色钱包", "", 31, 5, lastSeenAt, lostAt),
			found: found("捡到黑色长款钱包", "", 31, 5, inWindow),
		},
		{
			name:  "证件类：学生证 vs 捡到一张学生证（同小类，时间差 3 天）",
			kind:  "真匹配",
			lost:  lost("学生证", "名字是李某某", 11, 2, lastSeenAt, lostAt),
			found: found("捡到一张学生证", "李某某的学生证", 11, 2, "2026-05-15T15:00:00Z"),
		},

		{
			name:  "✗ 跨大类：充电宝 vs 黑色钱包（同地点同时段）",
			kind:  "绝不该出现",
			lost:  lost("充电宝", "白色的带一根线", 8, 1, lastSeenAt, lostAt),
			found: found("捡到黑色长款钱包", "黑色钱包里面有一些证件", 31, 5, inWindow),
		},
		{
			// 同大类（都是数码电子）救回了 0.225，但两个东西根本不相干。
			// 它必须在通知线以下 —— 给一个丢耳机的人推一条 U 盘是打扰。
			name:  "✗ 同大类不同小类且东西完全不同：耳机 vs U盘",
			kind:  "不该通知",
			lost:  lost("耳机", "白色有线耳机", 4, 1, lastSeenAt, lostAt),
			found: found("U盘", "金色u盘 32g", 8, 1, inWindow),
		},
		{
			name:  "✗ 完全无关的两件东西：雨伞 vs 保温杯",
			kind:  "绝不该出现",
			lost:  lost("雨伞", "黑色长柄", 26, 6, lastSeenAt, lostAt),
			found: found("保温杯", "银色 500ml", 28, 6, inWindow),
		},
		{
			name:  "✗ 同大类、15 天前丢的、文本几乎不重合：宿舍钥匙 vs 车钥匙",
			kind:  "绝不该出现",
			lost:  lost("宿舍钥匙", "一串三把", 16, 3, lastSeenAt, lostAt),
			found: found("车钥匙", "红色遥控钥匙", 18, 3, pastDecay),
		},
		{
			// 同小类 + 时间完美，但一个是一串钥匙、一个是钥匙扣，描述毫无重合。
			// 0.70：展示档内、通知档外 —— 这正是 §5.8 中间那个带子该装的东西。
			name:  "✗ 同小类但文本无关：一串钥匙 vs 一个钥匙扣",
			kind:  "不该通知",
			lost:  lost("钥匙串", "三把钥匙加一个公交卡", 19, 3, lastSeenAt, lostAt),
			found: found("钥匙扣", "金属环", 19, 3, inWindow),
		},

		{
			// ★ 已知边界，不是 bug，但必须被看见：
			// 同小类 + 描述一字不差 + 拾获时间在 14 天衰减窗口之外 → S_time=0，
			// 总分仍然有 0.85（0.45 分类 + 0.40 文本），照样过通知线。
			//
			// 这是 §5.2 那句「时间只做精细排序，边际价值低」的直接后果：
			// Tier 1 的 SQL 只筛下界（found_at >= last_seen_at - 1 天），**没有上界**，
			// 所以一条三个月后捡到的、描述一模一样的帖子会一路走到打分，然后仅靠
			// 分类和文本拿到通知档。
			// 这是刻意接受的（同一地点、同一小类、描述全同，三个月后捡到确实值得一看），
			// 但如果 §14-2 观察到一个学期后还在发通知，方案是给候选 SQL 加一个
			// found_at 的上界（用 MATCH_DECAY_DAYS），而不是动权重。
			name:  "△ 已知边界：同小类 + 文本全同 + 时间差 15 天 → 仍然 0.85",
			kind:  "已知边界",
			lost:  lost("宿舍钥匙", "一串三把，带个蓝色挂件", 16, 3, lastSeenAt, lostAt),
			found: found("宿舍钥匙", "一串三把，带个蓝色挂件", 16, 3, pastDecay),
		},
	}

	band := func(s float64) string {
		switch {
		case s >= notifyLine:
			return "通知(≥0.75)"
		case s >= showLine:
			return "展示(0.55~0.75)"
		default:
			return "隐藏(<0.55)"
		}
	}

	t.Logf("")
	t.Logf("=========== 分数分布（tier1 = 严格模式，tier2 = 放宽地点）===========")
	for _, s := range samples {
		b1 := ScorePair(s.lost, s.found, Tier1, Options{}).Breakdown
		b2 := ScorePair(s.lost, s.found, Tier2, Options{}).Breakdown
		t.Logf("%-6s %-52s tier1=%.4f %-14s (cat=%.2f text=%.4f time=%.4f)",
			s.kind, s.name, b1.Score, band(b1.Score),
			b1.Signals.Category.Score, b1.Signals.Text.Score, b1.Signals.Time.Score)
		t.Logf("%-6s %-52s tier2=%.4f %-14s (cat=%.2f text=%.4f time=%.4f loc=%.4f %s)",
			"", s.name, b2.Score, band(b2.Score),
			b2.Signals.Category.Score, b2.Signals.Text.Score, b2.Signals.Time.Score,
			b2.Signals.Location.Score, b2.Signals.Location.MatchedBy)

		switch s.kind {
		case "真匹配":
			if b1.Score < showLine {
				t.Errorf("%q 是真匹配，但 tier1 只有 %v，低于展示线 %v —— 它会从结果里彻底消失",
					s.name, b1.Score, showLine)
			}
		case "不该通知":
			if b1.Score >= notifyLine {
				t.Errorf("%q 不该通知，但 tier1 有 %v，够到了通知线 %v —— 会有人收到一条无关的推送",
					s.name, b1.Score, notifyLine)
			}
		case "绝不该出现":
			if b1.Score >= showLine {
				t.Errorf("%q 是明显不相干的一对，但 tier1 有 %v，达到展示线 %v —— 它会出现在用户面前",
					s.name, b1.Score, showLine)
			}
		}
	}

	// §13 那条主线必须够得着通知线：它是「found 帖创建 → 写台账 + 发 new_match」
	// 这条唯一写入路径的冒烟靶子，M3 判据的 ①② 两条都靠它。
	core := samples[0]
	if got := ScorePair(core.lost, core.found, Tier1, Options{}).Breakdown.Score; got < notifyLine {
		t.Errorf("§13 主线场景 tier1 只有 %v，够不到通知线 %v，M3 的冒烟第①②条会假红",
			got, notifyLine)
	}

	// 分布整体上移这件事本身也要打印出来，给 §14-2 的观察指标留个基线。
	var above, trueCount, edgeCount int
	for _, s := range samples {
		switch s.kind {
		case "真匹配":
			trueCount++
		case "已知边界":
			edgeCount++
		}
		if ScorePair(s.lost, s.found, Tier1, Options{}).Breakdown.Score >= notifyLine {
			above++
		}
	}
	t.Logf("样本 %d 对（真匹配 %d / 不该出现或不该通知 %d / 已知边界 %d），其中 tier1 达到通知线的 %d 对",
		len(samples), trueCount, len(samples)-trueCount-edgeCount, edgeCount, above)
	t.Logf("=====================================================================")
	t.Logf("提醒：§5.8 说过删掉 S_attr 会让通知量变多。如果这里看到假匹配贴着 0.75，")
	t.Logf("      先只改 .env 的 MATCH_NOTIFY_THRESHOLD（例如提到 0.80），不要改权重或公式。")
}

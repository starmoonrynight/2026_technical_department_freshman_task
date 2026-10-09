package matcher

import (
	"strings"
	"unicode"
)

// textSimilarity 是文本相似度的**唯一入口**。
//
// 计划 §5.6 把整条换分词库的路都押在这一个函数上：现在它等于 dice（字符 bigram），
// 将来若要换成 go-ego/gse 真分词，只改这个函数体 + 重跑单测，四处调用点一行不动。
// 这也是 bigram 方案「先做起来，效果不行再换」能成立的前提 —— 如果 dice 散落在
// 打分公式里，反悔的成本就从改一个文件变成改整个包。
func textSimilarity(a, b string) float64 {
	return dice(a, b)
}

// dice 算两段文本的字符 bigram Dice 系数：2*|A∩B| / (|A|+|B|)，值域 [0,1]。
//
// 用集合而不是多重集合（重复的 bigram 只数一次）：公式要的是「出现了哪些双字词」，
// 同一个人把「钱包」写三遍不会让两段文字更像。
//
// 已知缺点（§5.6 明写）：「色钱」这种跨词边界的无意义片段也会计分。
// 这是 bigram 换掉分词库的代价，不是 bug，别在单测里把它当例外处理。
func dice(a, b string) float64 {
	setA := bigramSet(a)
	setB := bigramSet(b)
	// 任一边为空 → 0。分母为 0 时公式无定义，计划把它的值定为 0。
	// 空集只可能来自「归一化之后一个字符都不剩」（空描述、纯标点描述），
	// 见 bigramSet 的注释 —— 那正是分母为 0 的那种情况。
	if len(setA) == 0 || len(setB) == 0 {
		return 0
	}
	// 遍历小的那个集合：交集大小和遍历方向无关，少几轮是纯赚。
	if len(setA) > len(setB) {
		setA, setB = setB, setA
	}
	var inter int
	for k := range setA {
		if _, ok := setB[k]; ok {
			inter++
		}
	}
	return 2 * float64(inter) / float64(len(setA)+len(setB))
}

// bigramSet 归一化后切出相邻双字集合。
//
// ⚠ 必须按 rune 切，不能按字节。Go 的 string 是字节串，中文一个字占 3 字节，
// 用 s[i:i+2] 切出来的是「半个字 + 半个字」的乱码，两段中文的集合永远不相交，
// 症状是「所有中文帖的文本分数都是 0」，而这看起来完全像是「描述写得太不一样」。
func bigramSet(s string) map[string]struct{} {
	r := []rune(normalize(s))
	switch len(r) {
	// 归一化后什么都不剩 → **空集**，交给 dice 返回 0。
	//
	// 这里是计划 §5.6「长度 < 2 → 集合 = {s} 本身」的一个刻意偏离：
	// 那条规则是为「单个字」设计的（{s} 让「锁」和「锁包」也能对上），
	// 但照字面套到空串上会得到 {""}，于是「两个都没写描述的帖子」
	// 会因为「同样空白」而拿满分。空白不是相似，是没信息 —— 必须给 0。
	// 描述可空是这个系统里的常态（#13 只强制 title 和 contact）。
	case 0:
		return nil
	case 1:
		return map[string]struct{}{string(r): {}}
	}
	out := make(map[string]struct{}, len(r)-1)
	for i := 0; i+1 < len(r); i++ {
		out[string(r[i:i+2])] = struct{}{}
	}
	return out
}

// normalize 转小写，并且只留汉字/字母/数字。
//
// 「删掉其余一切」这一刀同时处理了空白、标点、emoji 和全角符号，
// 因为 unicode.IsLetter/IsDigit 对汉字返回 true、对话框和「。」返回 false。
// 不做这件事的话，「黑色钱包」和「黑色 钱包！！」会被切成不同的 bigram，
// 而它们明明是同一个人写的同一句话。
func normalize(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

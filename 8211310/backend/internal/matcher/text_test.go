package matcher

import (
	"math"
	"testing"
)

// almostEqual 用于比较 dice 这类除法结果。
//
// 不直接 == 是因为 2*3/7 在二进制浮点里没有精确表示；
// 用 1e-9 而不是 1e-6 是因为这些值都在 [0,1]，误差本身就是最后一位的量级。
func almostEqual(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}

// TestDiceKnownValues 是 §12 M3 判据里那张「中文 bigram 已知值」表。
//
// 每条期望值都是**手算**出来的，写在注释里，因为让代码去算期望值就等于什么都没测。
// 数一数：下面 16 行，覆盖判据点名的四项（已知值 / 单字符 fallback /
// 「黑色钱包」对「捡到黑色长款钱包」/ 归一化的各个分支）。
func TestDiceKnownValues(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		want float64
		// work 是手算过程，出错时打印出来，省得再去数 bigram
		work string
	}{
		{
			// ★ 判据专门点名的这一条：颜色列删掉之后，文本信号必须还能抓住「黑色」。
			// A={黑色,色钱,钱包} 3 个；B={捡到,到黑,黑色,色长,长款,款钱,钱包} 7 个；
			// 交集 {黑色,钱包} 2 个 → 2*2/(3+7) = 0.4
			name: "黑色钱包 vs 捡到黑色长款钱包（颜色词被 bigram 抓到）",
			a:    "黑色钱包", b: "捡到黑色长款钱包",
			want: 0.4,
			work: "2*2/(3+7)=0.4  交集={黑色,钱包}",
		},
		{name: "完全相同", a: "钱包", b: "钱包", want: 1, work: "2*1/(1+1)=1"},
		{name: "两段都是空的描述", a: "", b: "", want: 0, work: "空集 → 分母为 0，定义为 0"},
		{name: "一段空一段有", a: "", b: "黑色钱包", want: 0, work: "任一边空集就是 0"},
		{name: "纯标点和空白被归一化掉后等于空", a: "！！！  ", b: "黑色钱包", want: 0, work: "归一化后 a 是空串"},
		{
			// 单字符 fallback：{s} 本身。两个都是「锁」时给满分，
			// 而「锁」和「钥匙」还是 0 —— fallback 只救「同一个字」，不救「字在里面」。
			name: "单字符 fallback：锁 vs 锁",
			a:    "锁", b: "锁", want: 1, work: "|A|=|B|=1，交集 1 → 2*1/2=1",
		},
		{name: "单字符不是子串匹配：锁 vs 钥匙", a: "锁", b: "钥匙", want: 0, work: "{锁} ∩ {钥匙} = ∅"},
		{name: "单字符 vs 双字：锁 vs 锁包", a: "锁", b: "锁包", want: 0, work: "{锁} ∩ {锁包} = ∅"},
		{name: "大小写与空格归一化", a: "iPhone 15", b: "iphone15", want: 1, work: "两边都归一成 iphone15"},
		{name: "标点不参与", a: "黑色 钱包！！", b: "黑色钱包", want: 1, work: "去掉空格和 ! 之后两边相同"},
		{name: "emoji 被删掉", a: "钱包🎒", b: "钱包", want: 1, work: "🎒 不是 letter/digit，删掉后等于 {钱包}"},
		{
			// 重复不会加分（集合语义），但会把分母撑大 → 分数被稀释。
			// 这是刻意选择：一个人把「钱包」写三遍不该比写一遍更像。
			name: "重复文本被集合去重",
			a:    "黑色钱包", b: "黑色钱包黑色钱包",
			want: 6.0 / 7,
			work: "A 3 个，B {黑色,色钱,钱包,包黑} 4 个，交集 3 → 2*3/(3+4)=0.857",
		},
		{
			// §5.6 明写的已知缺点：「色钱」是跨词边界的无意义片段，也会计分。
			// 这条不是断言它好，是**钉住这个行为**，将来换分词库时它会变，那时要 knowingly 改。
			name: "跨词边界的无意义片段也计分（已知缺点）",
			a:    "色钱包", b: "黑色钱包",
			want: 0.8,
			work: "A={色钱,钱包} 2，B={黑色,色钱,钱包} 3，交集 2 → 4/5",
		},
		{
			name: "校园卡 vs 学生证校园卡",
			a:    "校园卡", b: "学生证校园卡",
			want: 4.0 / 7,
			work: "A={校园,园卡} 2，B={学生,生证,证校,校园,园卡} 5，交集 2 → 2*2/(2+5)=0.571",
		},
		{name: "毫不相干的两件东西", a: "雨伞", b: "充电宝", want: 0, work: "{雨伞} ∩ {充电,电宝} = ∅"},
		{
			name: "一个字的差别",
			a:    "黑色钱包", b: "粉色钱包",
			want: 4.0 / 6,
			work: "A={黑色,色钱,钱包}，B={粉色,色钱,钱包}，交集 {色钱,钱包} 2 → 2*2/(3+3)=0.667",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := dice(c.a, c.b)
			if !almostEqual(got, c.want) {
				t.Errorf("dice(%q, %q) = %v，期望 %v\n手算: %s\nA集=%v\nB集=%v",
					c.a, c.b, got, c.want, c.work, sortedKeys(bigramSet(c.a)), sortedKeys(bigramSet(c.b)))
			}
		})
	}
}

// TestBlackWalletColorWordIsCaptured 单独把判据点名的那条挑出来再断言一次，
// 并且断言它**大于**一个跨大类的对照 —— 这样「颜色列删了会不会匹配不到」这个问题
// 有一个可执行的答案，而不是文档里的一句承诺。
func TestBlackWalletColorWordIsCaptured(t *testing.T) {
	colorWordScore := dice("黑色钱包", "捡到黑色长款钱包")
	if !almostEqual(colorWordScore, 0.4) {
		t.Fatalf("黑色钱包/捡到黑色长款钱包 的 dice 应为 0.4，实测 %v", colorWordScore)
	}
	// 对照：一个完全不同的物品，字面也不重合
	unrelated := dice("黑色钱包", "捡到充电宝三线")
	if colorWordScore <= unrelated {
		t.Errorf("颜色词没被捕捉到：钱包对钱包只有 %v，竟然不比钱包对充电宝（%v）更像", colorWordScore, unrelated)
	}
}

func TestBigramSetShape(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"黑色钱包", []string{"黑色", "色钱", "钱包"}},
		{"锁", []string{"锁"}}, // 单字符 fallback：集合就是它自己
		{"", nil},            // 空 → 空集（不是 {""}，见 text.go）
		{"。，！", nil},         // 全标点 → 空集
		{"ab", []string{"ab"}},
		{"abc", []string{"ab", "bc"}},
		{"aabb", []string{"aa", "ab", "bb"}}, // 重复 bigram 只留一个（集合语义）
	}
	// 比的是**集合内容**，不比顺序：bigramSet 返回 map，而 sortedKeys 按码点排
	// （色 U+8272 < 钱 U+94B1 < 黑 U+9ED1），按书写顺序写期望值会让测试因为无关原因红。
	for _, c := range cases {
		got := bigramSet(c.in)
		if len(got) != len(c.want) {
			t.Errorf("bigramSet(%q) 有 %d 个元素 %v，期望 %v", c.in, len(got), sortedKeys(got), c.want)
			continue
		}
		for _, w := range c.want {
			if _, ok := got[w]; !ok {
				t.Errorf("bigramSet(%q) 缺 %q，实际 %v", c.in, w, sortedKeys(got))
			}
		}
	}
}

// TestNormalizeKeepsOnlyLettersAndDigits 逐类验证归一化保留了什么。
func TestNormalizeKeepsOnlyLettersAndDigits(t *testing.T) {
	cases := []struct{ in, want string }{
		{"黑色钱包", "黑色钱包"},
		{"  黑色\t钱包\r\n", "黑色钱包"},
		{"黑色。钱包、", "黑色钱包"},
		{"iPhone 15 Pro", "iphone15pro"},
		{"钱包🎒✨", "钱包"},
		{"ＡＢ１２", "ａｂ１２"}, // 全角字母数字是 letter/digit，保留（小写成全角形式）
		{"卡号: 6217 0012", "卡号62170012"},
	}
	for _, c := range cases {
		if got := normalize(c.in); got != c.want {
			t.Errorf("normalize(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestTextSimilarityIsTheOnlySeam 锁住 §5.6 那个「换成 gse 只改一个函数」的前提：
// textSimilarity 现在必须等价于 dice。将来真换了分词库，这个测试就该被改掉，
// 而它被改掉的那一刻就是提醒你去重跑那批分数分布的时候。
func TestTextSimilarityIsTheOnlySeam(t *testing.T) {
	for _, p := range [][2]string{
		{"黑色钱包", "捡到黑色长款钱包"},
		{"校园卡", "学生证校园卡"},
		{"", "空描述"},
	} {
		if got, want := textSimilarity(p[0], p[1]), dice(p[0], p[1]); got != want {
			t.Errorf("textSimilarity(%q,%q)=%v 和 dice=%v 不一致", p[0], p[1], got, want)
		}
	}
}

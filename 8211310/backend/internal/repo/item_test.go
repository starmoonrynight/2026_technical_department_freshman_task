package repo

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// 本文件测的是「动态 SQL 拼装」这一处 —— 整个后端唯一把字符串拼进查询的地方。
//
// 它不需要数据库：buildItemWhere 是纯函数，返回一段 WHERE 文本和一份参数。
// 正因为不需要数据库，它才值得单独测：SQL 注入和占位符编号错位都是
// 「集成测试里恰好没触发」的典型 —— 前者要有人真的去搜一个引号，
// 后者要有人真的同时用上全部筛选条件。

// ---------- escapeLike ----------

func TestEscapeLike(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"钱包", "钱包"},                         // 普通中文原样
		{"abc", "abc"},                       // 普通 ASCII 原样
		{"", ""},                             // 空串原样
		{"100%", `100\%`},                    // % 是通配符，必须转义
		{"a_b", `a\_b`},                      // _ 是单字符通配符，必须转义
		{`a\b`, `a\\b`},                      // 反斜杠自己也要转义
		{`\%_`, `\\\%\_`},                    // 三个一起来
		{"%", `\%`},                          // 只有一个 %
		{"%%", `\%\%`},                       // 连续两个
		{`%\`, `\%\\`},                       // 百分号后面跟着反斜杠：顺序敏感的那一类
		{"50%_off\\deal", `50\%\_off\\deal`}, // 混在一起
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.in), func(t *testing.T) {
			if got := escapeLike(tc.in); got != tc.want {
				t.Fatalf("escapeLike(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEscapeLikeBackslashFirstIsNotAnIssue 钉住「替换顺序」这个坑。
//
// 如果实现是「先 Replace("%", `\%`) 再 Replace(`\`, `\\`)」，
// 那么第一步产出的 `\%` 里的反斜杠会在第二步被再转一次变成 `\\%`，
// 数据库按 ESCAPE '\' 解读时，`\\` 是一个字面反斜杠、后面的 % 又变成了通配符 ——
// 匹配的东西完全不是用户输入的那个。
//
// strings.NewReplacer 一次扫描同时替换、不会把替换结果再拿去匹配，所以天然没这个问题。
// 但这条性质是**实现细节**，将来有人改成两次 Replace 就会静默出错，
// 所以用输入 `\%`（一个反斜杠加一个百分号）把它钉住：正确的结果是 `\\\%`。
func TestEscapeLikeBackslashFirstIsNotAnIssue(t *testing.T) {
	const in = `\%`
	const want = `\\\%` // 反斜杠 → \\，百分号 → \%
	if got := escapeLike(in); got != want {
		t.Fatalf("escapeLike(%q) = %q，期望 %q。"+
			"如果是两次 Replace 实现的，第二次会把第一次产出的反斜杠再转一遍", in, got, want)
	}
}

// ---------- buildItemWhere ----------

// placeholderRe 抓 WHERE 文本里出现的全部 $n。
var placeholderRe = regexp.MustCompile(`\$(\d+)`)

// checkPlaceholders 断言「WHERE 里用到的占位符编号」恰好是 1..len(args) 的一个排列。
//
// 这条不变量比任何具体断言都重要：pgx 在编号对不上时报的是
// "bind message supplies 3 parameters, but prepared statement requires 5"，
// 那是 INTERNAL 500，用户看到的是「服务器出错了」，而我们连日志都看不出是哪个条件拼错了。
func checkPlaceholders(t *testing.T, where string, args []any) {
	t.Helper()
	seen := map[int]bool{}
	for _, m := range placeholderRe.FindAllStringSubmatch(where, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("WHERE 里有解析不了的占位符 %q：%s", m[0], where)
		}
		if n < 1 || n > len(args) {
			t.Fatalf("WHERE 里的 %s 超出了参数个数 %d：%s", m[0], len(args), where)
		}
		seen[n] = true
	}
	if len(seen) != len(args) {
		t.Fatalf("参数有 %d 个，但 WHERE 里只用到了 %d 个不同的编号（多出来的参数会让 pgx 直接报错）\n%s",
			len(args), len(seen), where)
	}
	for i := 1; i <= len(args); i++ {
		if !seen[i] {
			t.Fatalf("编号 $%d 没有出现在 WHERE 里，但 args 有 %d 个：%s", i, len(args), where)
		}
	}
}

func int64ptr(v int64) *int64 { return &v }

func TestBuildItemWhereEmpty(t *testing.T) {
	where, args := buildItemWhere(ListFilter{})
	if where != "" {
		t.Errorf("空条件应该产出空字符串（不加 WHERE 关键字），实际 %q", where)
	}
	if len(args) != 0 {
		t.Errorf("空条件不该有参数，实际 %v", args)
	}
}

func TestBuildItemWhereSingleConditions(t *testing.T) {
	cases := []struct {
		name      string
		filter    ListFilter
		wantWhere string
		wantArgs  []any
	}{
		{
			"只按类型",
			ListFilter{ItemType: "lost"},
			" WHERE i.item_type = $1",
			[]any{"lost"},
		},
		{
			// keyword 占**两个**参数（title 和 description 各一个），
			// 而且两个都是「% + 转义后的关键词 + %」。
			"只按关键词",
			ListFilter{Keyword: "钱包"},
			` WHERE (i.title ILIKE $1 ESCAPE '\' OR i.description ILIKE $2 ESCAPE '\')`,
			[]any{"%钱包%", "%钱包%"},
		},
		{
			"只按分类",
			ListFilter{CategoryID: int64ptr(36)},
			" WHERE i.category_id = $1",
			[]any{int64(36)},
		},
		{
			"只按地点",
			ListFilter{LocationID: int64ptr(50)},
			" WHERE i.location_id = $1",
			[]any{int64(50)},
		},
		{
			// = ANY($n) 而不是 IN ($1,$2)：数组只占一个占位符，
			// 不用为「状态可能有几个」动态生成编号。
			"只按状态",
			ListFilter{Statuses: []string{"open", "closed"}},
			" WHERE i.status = ANY($1)",
			[]any{[]string{"open", "closed"}},
		},
		{
			"只按用户",
			ListFilter{UserID: int64ptr(7)},
			" WHERE i.user_id = $1",
			[]any{int64(7)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			where, args := buildItemWhere(tc.filter)
			if where != tc.wantWhere {
				t.Errorf("WHERE = %q\n期望   %q", where, tc.wantWhere)
			}
			checkPlaceholders(t, where, args)
			if fmt.Sprint(args) != fmt.Sprint(tc.wantArgs) {
				t.Errorf("args = %#v\n期望   %#v", args, tc.wantArgs)
			}
		})
	}
}

// TestBuildItemWhereAllConditions 用上全部六个条件。
//
// 单独列一条用例是因为编号错位的 bug **只在多条件同时出现时**才暴露：
// 单条件时 $1 永远是 $1。而「同时用上全部筛选条件」在手工测试里几乎不会发生 ——
// 用户要么搜关键词，要么点分类，很少两件事一起做。
func TestBuildItemWhereAllConditions(t *testing.T) {
	where, args := buildItemWhere(ListFilter{
		ItemType:   "found",
		Keyword:    "钱包",
		CategoryID: int64ptr(36),
		LocationID: int64ptr(50),
		Statuses:   []string{"open"},
		UserID:     int64ptr(7),
	})

	want := ` WHERE i.item_type = $1` +
		` AND (i.title ILIKE $2 ESCAPE '\' OR i.description ILIKE $3 ESCAPE '\')` +
		` AND i.category_id = $4` +
		` AND i.location_id = $5` +
		` AND i.status = ANY($6)` +
		` AND i.user_id = $7`
	if where != want {
		t.Errorf("WHERE =\n%s\n期望 =\n%s", where, want)
	}
	checkPlaceholders(t, where, args)
	if len(args) != 7 {
		t.Fatalf("应该有 7 个参数，实际 %d 个：%#v", len(args), args)
	}
}

// TestBuildItemWhereNeverInlinesUserInput 是 SQL 注入的回归测试。
//
// 断言的方式很硬：不管 payload 是什么，产出的 WHERE 文本都必须**逐字节等于**
// 同一个固定模板。只要有一处把值拼进了字符串而不是走占位符，
// 换了 payload 之后这个等式立刻不成立。
func TestBuildItemWhereNeverInlinesUserInput(t *testing.T) {
	// ItemType + Keyword + Statuses 三个条件全用同一个 payload 填。
	const wantWhere = ` WHERE i.item_type = $1` +
		` AND (i.title ILIKE $2 ESCAPE '\' OR i.description ILIKE $3 ESCAPE '\')` +
		` AND i.status = ANY($4)`

	payloads := []string{
		`'; DROP TABLE items; --`,
		`1 OR 1=1`,
		`%`,
		`_`,
		`\`,
		`$1`, // 试图伪造占位符编号
		`$99`,
		`钱包' AND '1'='1`,
		`<script>alert(1)</script>`,
		"'\n' UNION SELECT password_hash FROM users --",
		`' ESCAPE ''`, // 试图改掉 ILIKE 的转义符
		strings.Repeat("a", 500),
	}

	for _, p := range payloads {
		t.Run(strings.ReplaceAll(p, "\n", `\n`), func(t *testing.T) {
			where, args := buildItemWhere(ListFilter{
				ItemType: p,
				Keyword:  p,
				Statuses: []string{p},
			})

			if where != wantWhere {
				t.Fatalf("WHERE 文本随用户输入变化了 —— 说明有值被拼进了 SQL：\n got: %s\nwant: %s",
					where, wantWhere)
			}
			checkPlaceholders(t, where, args)

			// 反过来，payload 必须**完整地出现在参数里**，否则就是筛选条件被静默丢掉了。
			// 静默丢弃比注入更难发现：用户搜了个东西，得到一份没筛过的列表，还以为搜对了。
			if len(args) != 4 {
				t.Fatalf("应该有 4 个参数，实际 %d 个：%#v", len(args), args)
			}
			if args[0] != p {
				t.Errorf("item_type 参数 = %#v，期望原样的 payload", args[0])
			}
			wantLike := "%" + escapeLike(p) + "%"
			if args[1] != wantLike || args[2] != wantLike {
				t.Errorf("keyword 参数 = %#v / %#v，期望 %q", args[1], args[2], wantLike)
			}
			statuses, ok := args[3].([]string)
			if !ok || len(statuses) != 1 || statuses[0] != p {
				t.Errorf("statuses 参数 = %#v，期望原样的 payload", args[3])
			}
		})
	}
}

// TestBuildItemWhereKeywordEscapesWildcards 是「搜 100% 结果全是帖子」这个 bug 的回归测试。
//
// 不转义的话 `%` 变成通配符，`%100%%` 会匹配几乎所有行。
// 这既是正确性问题也是可用性问题，而且它不会被任何别的测试发现 ——
// 除非有人真的去搜一个百分号。
func TestBuildItemWhereKeywordEscapesWildcards(t *testing.T) {
	cases := []struct {
		keyword  string
		wantLike string // 交给数据库的那个模式串
	}{
		{"钱包", "%钱包%"},
		{"100%", `%100\%%`},
		{"a_b", `%a\_b%`},
		{`\`, `%\\%`},
		{"%", `%\%%`},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%q", tc.keyword), func(t *testing.T) {
			_, args := buildItemWhere(ListFilter{Keyword: tc.keyword})
			if len(args) != 2 {
				t.Fatalf("keyword 应该产出 2 个参数（title 和 description 各一个），实际 %d 个", len(args))
			}
			for i, a := range args {
				if a != tc.wantLike {
					t.Errorf("第 %d 个参数 = %q，期望 %q", i, a, tc.wantLike)
				}
			}
		})
	}
}

// TestItemSortColumnsCoversWhitelist 把 repo 的排序白名单和 service 的那张对在一起验。
//
// 两张白名单**故意不共用**（service 那张决定「用户能请求什么」，这张决定「什么能被
// 拼进 SQL」）。不共用的代价就是它们可能走偏：service 放行了一个 repo 不认识的 sort，
// 而 List 里的兜底会把它悄悄换成 created_at —— 用户点了「按丢失时间排序」，
// 拿到的却是按创建时间排的结果，**没有任何报错**。
func TestItemSortColumnsCoversWhitelist(t *testing.T) {
	allowed := []string{"created_at", "lost_at", "found_at"}
	for _, s := range allowed {
		col, ok := itemSortColumns[s]
		if !ok {
			t.Errorf("service 允许 sort=%s，但 itemSortColumns 里没有它 —— 会被静默换成 created_at", s)
			continue
		}
		// 列表达式会被拼进 ORDER BY，所以它必须是「i.列名」这种一眼能验的形状
		if !strings.HasPrefix(col, "i.") || strings.ContainsAny(col, " ;'\"()") {
			t.Errorf("itemSortColumns[%q] = %q，不像是安全的列引用", s, col)
		}
	}
	if len(itemSortColumns) != len(allowed) {
		t.Errorf("itemSortColumns 有 %d 项，service 的白名单有 %d 项；多出来的那些永远请求不到",
			len(itemSortColumns), len(allowed))
	}
}

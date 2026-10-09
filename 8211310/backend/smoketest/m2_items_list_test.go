package smoketest

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"lostfound/internal/apperr"
)

// 本文件覆盖 #14 GET /api/items（广场，公开）和 #19 GET /api/my/items（我的发布，JWT）。
//
// 两个端点用的是同一个 repo.List 和同一套查询参数解析，差别只有三处，
// 而这三处正是本文件要钉住的全部东西：
//
//	① #19 强制加了一个 user_id = 当前用户 的条件
//	② 默认状态集不同：#14 只看 open，#19 看 open + closed
//	③ 允许显式请求的状态不同：#14 不许 deleted，#19 许
//	   （自己的帖子被 admin 下架之后应该看得见，否则用户会以为帖子凭空消失了）
//
// contact 的填充规则也不同，而且是**两条独立的规则**（计划 §4）：
//
//	#14：lost 帖带 contact，found 帖一律 null —— **不看是谁在查**。
//	     自己的 found 帖在广场上也是锁着的。这样 #14 完全不需要身份，
//	     公开、可缓存，也不用为「这一行是不是我的」多发一次查询。
//	#19：一律带 contact（都是自己的）。
//	     想看自己帖子的联系方式就来这里，不是去广场。

// ---------- 夹具 ----------

// locNorthGate 是「生活区（北侧）→ 生活区校门 → 生活区北门」，一个 level-3 叶子。
// 和 locLibrary（教学区那一边）分属不同的一级区，这样 location_id 筛选才有区分度。
const locNorthGate = int64(12)

// squareSeed 是一个已知形状的广场：6 条帖子，覆盖两种类型、两个分类、两个地点、三种状态。
//
// 这张表是下面十几条断言的唯一依据，改任何一格都要重新算一遍期望值：
//
//	名字  作者   类型   分类        地点          状态      图   contact
//	a    owner  lost   手机(10)    图书馆(70)    open      有   13800000001
//	b    owner  lost   钱包(36)    北门(12)      closed    无   13800000002
//	c    owner  found  钱包(36)    图书馆(70)    open      有   问图书馆前台
//	d    owner  found  钱包(36)    北门(12)      open      无   13800000004
//	e    owner  lost   手机(10)    图书馆(70)    deleted   无   13800000005
//	f    other  lost   钱包(36)    其他(3)       open      无   13800000006
//
// 描述没列进表里，但 keyword 筛选**同时搜标题和描述**，所以它一样影响期望值。
// 默认值是 lostBody「…里面有校园卡」/ foundBody「…已经交到前台」，
// 只有 a 的描述被单独改写过。算 keyword 的期望值时必须把默认描述一起算进去。
//
// f 是**别人**发的 open 帖：#14 不分作者，所以它出现在广场的默认结果里，
// 上面几乎每一条期望值都包含它 —— 这正是它存在的意义（见 TestM2SquareFilters 最后一条）。
//
// ids 是「测试里用的名字 → 帖子 id」，这样每条断言都能指名道姓地说
// 「应该只出现 a 和 c」，而不是「应该出现前两条」—— 后者在有人调整种子顺序时
// 会静默地变成另一件事的测试。
type squareSeed struct {
	owner Session
	other Session
	ids   map[string]int64
}

func seedSquare(t *testing.T) squareSeed {
	t.Helper()
	harness.TruncateAll(t)

	owner := harness.RegisterAndLogin(t, "squareowner", "correct-horse-battery")
	other := harness.RegisterAndLogin(t, "squareother", "correct-horse-battery")

	// a 和 c 共用同一张图（各自的帖子有一行 item_images，指向同一个 path）。
	// 这是允许的：path 的去重只在**一条帖子内部**做（validateImagePaths），
	// 跨帖子共用一张图不是错误 —— 用户完全可能把同一张照片挂到两条帖子上。
	pic := upload(t, owner, 1024)

	ids := map[string]int64{}
	add := func(key string, body map[string]any, paths []string) {
		if paths != nil {
			body["image_paths"] = paths
		}
		ids[key] = createItem(t, owner, body).ID
	}

	// a: lost · 手机 · 图书馆 · open · 有图 · 标题里带「钱包」这个词（用来验 keyword 搜的是标题）
	a := lostBody("黑色钱包丢在图书馆", "13800000001")
	a["description"] = "里面有校园卡和一百块钱"
	add("a", a, []string{pic.Path})

	// b: lost · 钱包 · 生活区北门 · closed · 无图
	b := lostBody("丢了一个卡包", "13800000002")
	b["category_id"] = catWallet
	b["location_id"] = locNorthGate
	b["last_seen_at"] = "2026-09-20T08:00:00+08:00"
	b["lost_at"] = "2026-09-21T08:00:00+08:00"
	add("b", b, nil)
	harness.SetItemStatus(t, ids["b"], "closed")

	// c: found · 钱包（foundBody 的默认分类）· 图书馆 · open · 有图
	c := foundBody("在图书馆捡到一台手机", "问图书馆前台")
	add("c", c, []string{pic.Path})

	// d: found · 钱包 · 生活区北门 · open · 无图
	d := foundBody("捡到一个棕色钱包", "13800000004")
	d["location_id"] = locNorthGate
	d["found_at"] = "2026-09-25T10:00:00+08:00"
	add("d", d, nil)

	// e: lost · 手机 · 图书馆 · deleted（广场上看不到，本人的 #19 能看到）
	e := lostBody("已经被删掉的一条", "13800000005")
	add("e", e, nil)
	harness.SetItemStatus(t, ids["e"], "deleted")

	// f: 别人发的一条 open lost，用来验 #19 只返回自己的
	f := lostBody("别人丢的雨伞", "13800000006")
	f["category_id"] = catWallet
	f["location_id"] = locOther
	f["location_detail"] = "食堂门口"
	ids["f"] = createItem(t, other, f).ID

	return squareSeed{owner: owner, other: other, ids: ids}
}

// ---------- #14 广场 ----------

func TestM2SquareDefaultShowsOnlyOpen(t *testing.T) {
	s := seedSquare(t)

	page := fetchSquare(t, "", "")
	// f 是别人发的 open 帖，**也在广场上** —— #14 不分作者。
	if got := idsOf(page); !sameIDs(got, []int64{s.ids["a"], s.ids["c"], s.ids["d"], s.ids["f"]}) {
		t.Errorf("广场默认应该是 a/c/d/f（所有 open 的帖子），实际是 %v\n全部种子：%v", got, s.ids)
	}
	if page.Total != 4 {
		t.Errorf("total 应该是 4，实际 %d", page.Total)
	}
	if page.Page != 1 || page.PageSize != 20 {
		t.Errorf("默认分页应该是 page=1&page_size=20，实际 %+v", page)
	}

	// closed 不是「不存在」，是「默认不显示」：显式请求 status=closed 必须能查到。
	// 找已归还的历史记录时用得上，而「查不到」和「被删了」对用户是两回事。
	closed := fetchSquare(t, "status=closed", "")
	if got := idsOf(closed); !sameIDs(got, []int64{s.ids["b"]}) {
		t.Errorf("status=closed 应该只有 b，实际 %v", got)
	}
}

// TestM2SquareIsPublic 钉住 #14 不需要身份。
//
// 匿名浏览广场是 §2.4 的三条途径之一（另两条是搜索和匹配推荐）。
// 而且它**刻意**不知道「你是谁」：found 帖的 contact 对所有人都锁着，
// 包括作者本人 —— 这样一个公开列表接口就是纯函数，可以缓存，
// 也不用为每一行多查一次「这是不是当前用户的帖子」。
func TestM2SquareIsPublic(t *testing.T) {
	s := seedSquare(t)

	anon := fetchSquare(t, "", "")
	authed := fetchSquare(t, "", s.owner.Token)
	if !sameIDs(idsOf(anon), idsOf(authed)) {
		t.Errorf("登录和未登录看到的广场应该完全一样：匿名 %v vs 登录 %v",
			idsOf(anon), idsOf(authed))
	}

	// 无效 token 也当匿名处理（和 #15 同一条纪律：公开接口不该因为身份有问题而拒绝服务）
	r := harness.Get(t, "/api/items", "not-a-real-token")
	RequireOK(t, r, "带无效 token 的 GET /api/items")
}

// TestM2SquareContactRules 钉住 #14 的 contact 填充规则。
//
// lost 帖带，found 帖一律 null —— **包括自己的 found 帖**。
// 判据没有明写这一条，但 §4 第 14 行写了，而且它是整个 M2 里
// 最容易被「顺手优化」破坏的一处：加一个「如果是自己的就把 contact 填上」
// 看起来是无害的便利，代价是 #14 从此需要身份、需要为每行查一次归属，
// 而那份便利 #19 已经提供了。
func TestM2SquareContactRules(t *testing.T) {
	s := seedSquare(t)

	for _, token := range []string{"", s.owner.Token, s.other.Token} {
		who := "匿名"
		switch token {
		case s.owner.Token:
			who = "作者本人"
		case s.other.Token:
			who = "其他登录用户"
		}

		page := fetchSquare(t, "status=open", token)
		if len(page.List) != 4 {
			t.Fatalf("%s 看到的广场应该有 4 条（a/c/d/f），实际 %d 条：%v", who, len(page.List), idsOf(page))
		}
		for _, sum := range page.List {
			switch sum.ItemType {
			case "lost":
				if sum.Contact == nil || strings.TrimSpace(*sum.Contact) == "" {
					t.Errorf("%s 看 lost 帖 %d 应该带 contact，实际 %+v", who, sum.ID, sum.Contact)
				}
			case "found":
				if sum.Contact != nil {
					t.Errorf("%s 看 found 帖 %d 的 contact 必须是 null，实际 %q",
						who, sum.ID, *sum.Contact)
				}
			default:
				t.Errorf("出现了未知的 item_type %q", sum.ItemType)
			}
		}
	}

	// 作者本人看**自己的 found 帖**在广场上也是锁着的。单独拎出来断言，
	// 因为它是上面那个循环里最容易被误判成 bug 的一格。
	own := fetchSquare(t, "status=open", s.owner.Token)
	for _, sum := range own.List {
		if sum.ID == s.ids["c"] && sum.Contact != nil {
			t.Errorf("自己的 found 帖在广场上必须锁着，实际 contact=%q（想看自己的联系方式去 #19）",
				*sum.Contact)
		}
	}

	// 锁着的时候，found 帖的 contact 字符串在整个列表响应里都不能出现。
	r := harness.Get(t, "/api/items?status=open", "")
	if strings.Contains(string(r.Data), "问图书馆前台") {
		t.Errorf("广场上出现了 found 帖锁着的联系方式：\n%s", truncate(string(r.Data)))
	}
}

// TestM2MineContactAlwaysVisible 钉住 #19 的 contact 规则：一律带上。
//
// 都是自己的帖子，锁着没有任何意义 —— 而「我发的帖子上写着我的电话，
// 我在『我的发布』里却看不到」会让他以为帖子发坏了。
func TestM2MineContactAlwaysVisible(t *testing.T) {
	s := seedSquare(t)

	page := fetchMine(t, "", s.owner)
	// 默认集是 open + closed：a（open）、b（closed）、c（open）、d（open）。
	// e 是 deleted，不在默认集里；f 是别人的。
	if len(page.List) != 4 {
		t.Fatalf("#19 默认应该有 4 条（a/b/c/d），实际 %d 条：%v", len(page.List), idsOf(page))
	}
	want := map[int64]string{
		s.ids["a"]: "13800000001",
		s.ids["b"]: "13800000002",
		s.ids["c"]: "问图书馆前台",
		s.ids["d"]: "13800000004",
	}
	for _, sum := range page.List {
		w, ok := want[sum.ID]
		if !ok {
			t.Errorf("#19 里出现了不该有的帖子 %d", sum.ID)
			continue
		}
		if sum.Contact == nil || *sum.Contact != w {
			t.Errorf("帖子 %d 的 contact 应该是 %q，实际 %+v", sum.ID, w, sum.Contact)
		}
	}
}

// TestM2MineIsScopedToTheCaller 钉住 #19 的「我的」两个字。
//
// user_id 是 service 从 JWT 里取的，**不是查询参数**。
// 如果它来自查询参数，那么 ?user_id=别人 就能读到别人的全部帖子（含 deleted 的），
// 而联系方式一并泄漏 —— 这是整个 M2 里后果最严重的一个可能错误。
func TestM2MineIsScopedToTheCaller(t *testing.T) {
	s := seedSquare(t)

	page := fetchMine(t, "", s.other)
	if got := idsOf(page); !sameIDs(got, []int64{s.ids["f"]}) {
		t.Errorf("other 的 #19 应该只有 f，实际 %v（全部种子：%v）", got, s.ids)
	}

	// 试着用查询参数越权。这些参数**不该被读取**；就算被读取了，
	// 也必须在 service 里被 JWT 里的 user_id 覆盖掉。
	for _, q := range []string{
		"user_id=" + itoa(s.owner.UserID),
		"uid=" + itoa(s.owner.UserID),
		"author_id=" + itoa(s.owner.UserID),
		"mine=false",
		"all=true",
	} {
		t.Run(q, func(t *testing.T) {
			p := fetchMine(t, q, s.other)
			if got := idsOf(p); !sameIDs(got, []int64{s.ids["f"]}) {
				t.Errorf("?%s 让 other 看到了别人的帖子：%v", q, got)
			}
		})
	}

	// #19 必须登录
	RequireCode(t, harness.Get(t, "/api/my/items", ""), apperr.CodeUnauthorized)
}

// TestM2MineShowsOwnDeleted 钉住 #19 允许显式查 deleted，而 #14 不允许。
//
// 自己的帖子被 admin 下架之后应该看得见 —— 否则用户会以为帖子凭空消失了。
// M6 会额外给他发一条 admin_action 通知，但通知会过期，列表不会。
//
// 反过来，广场上**任何人**（包括发帖人自己）都不该通过 #14 看到被删的帖子：
// 那等于把治理动作公开化了。
func TestM2MineShowsOwnDeleted(t *testing.T) {
	s := seedSquare(t)

	mine := fetchMine(t, "status=deleted", s.owner)
	if got := idsOf(mine); !sameIDs(got, []int64{s.ids["e"]}) {
		t.Errorf("#19?status=deleted 应该只有 e，实际 %v", got)
	}
	for _, sum := range mine.List {
		if sum.Status != "deleted" {
			t.Errorf("status 应该是 deleted，实际 %q", sum.Status)
		}
	}

	// 默认集是 open + closed：e 不在里面，f 也不是他的
	def := fetchMine(t, "", s.owner)
	if got := idsOf(def); !sameIDs(got, []int64{s.ids["a"], s.ids["b"], s.ids["c"], s.ids["d"]}) {
		t.Errorf("#19 默认应该是 a/b/c/d，实际 %v", got)
	}
	if def.Total != 4 {
		t.Errorf("total 应该是 4，实际 %d", def.Total)
	}

	// #14 不许 deleted
	requireField(t, harness.Get(t, "/api/items?status=deleted", s.owner.Token), "status")
	requireField(t, harness.Get(t, "/api/items?status=deleted", ""), "status")
}

// ---------- 筛选 ----------

func TestM2SquareFilters(t *testing.T) {
	s := seedSquare(t)

	cases := []struct {
		name  string
		query string
		want  []string // 种子名字
	}{
		{"item_type=lost", "item_type=lost", []string{"a", "f"}},
		{"item_type=found", "item_type=found", []string{"c", "d"}},
		{"category_id=手机", "category_id=" + itoa(catPhone), []string{"a"}},
		{"category_id=钱包", "category_id=" + itoa(catWallet), []string{"c", "d", "f"}},
		{"location_id=图书馆", "location_id=" + itoa(locLibrary), []string{"a", "c"}},
		{"location_id=生活区北门", "location_id=" + itoa(locNorthGate), []string{"d"}},
		{"keyword 命中标题", "keyword=钱包", []string{"a", "d"}},
		// lostBody 的默认描述是「…里面有校园卡」，所以 f 也命中；
		// a 的描述被单独改成了「里面有校园卡和一百块钱」，同样命中。
		// b（closed）和 e（deleted）虽然也含这个词，但不在默认状态集里。
		{"keyword 命中描述", "keyword=校园卡", []string{"a", "f"}},
		// foundBody 的默认描述是「在图书馆三楼捡到的，已经交到前台」，c 和 d 都用它。
		// 两个标题里都没有「交到前台」，所以这一条专门验「描述也在搜索范围内」。
		{"keyword 只命中描述（标题里没有）", "keyword=交到前台", []string{"c", "d"}},
		{"keyword 没有命中", "keyword=根本不存在的东西", nil},
		{"两个筛选同时用", "item_type=found&category_id=" + itoa(catWallet), []string{"c", "d"}},
		{"三个筛选同时用", "item_type=lost&category_id=" + itoa(catPhone) +
			"&location_id=" + itoa(locLibrary), []string{"a"}},
		{"互斥的筛选", "item_type=found&category_id=" + itoa(catPhone) +
			"&location_id=" + itoa(locNorthGate), nil},
		{"status=closed 加上分类", "status=closed&category_id=" + itoa(catWallet), []string{"b"}},
		{"别人的帖子也在广场上（#14 不分作者）", "location_id=" + itoa(locOther), []string{"f"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page := fetchSquare(t, tc.query, "")
			got := idsOf(page)
			want := make([]int64, 0, len(tc.want))
			for _, name := range tc.want {
				want = append(want, s.ids[name])
			}
			if !sameIDs(got, want) {
				t.Errorf("?%s\n期望 %v（%v）\n实际 %v", tc.query, want, tc.want, got)
			}
			if page.Total != len(want) {
				t.Errorf("?%s 的 total 应该是 %d，实际 %d", tc.query, len(want), page.Total)
			}
		})
	}
}

// TestM2KeywordEscapesWildcards 钉住 LIKE 的转义。
//
// 用户搜 "100%" 时那个 % 是**字面量**。不转义它就变成通配符，
// 搜索结果会是「几乎所有帖子」—— 这既是正确性问题也是可用性问题，
// 而且它不会被任何测试发现，除非有人真的去搜一个百分号。
//
// 顺带验 `_`（单字符通配符）和 `\`（我们自己的转义符）。
func TestM2KeywordEscapesWildcards(t *testing.T) {
	s := seedSquare(t)
	me := s.owner

	// 一条标题里真的带 % 的帖子，和一条标题里带 _ 的帖子。
	pct := createItem(t, me, lostBody("钱包里有100%纯棉的手帕", "13800000000"))
	und := createItem(t, me, lostBody("一把 a_b 牌子的伞", "13800000000"))

	cases := []struct {
		keyword string
		want    []int64
		why     string
	}{
		{"100%", []int64{pct.ID}, "字面量百分号应该只命中带百分号的那条"},
		{"%", []int64{pct.ID}, "单独一个 % 也是字面量：只有 pct 的标题真的含 %，" +
			"如果转义被去掉它就会变成通配符、命中全部帖子"},
		{"a_b", []int64{und.ID}, "字面量下划线应该只命中带下划线的那条"},
		{"_", []int64{und.ID}, "单独一个 _ 也是字面量下划线：只有 und 含它，" +
			"不转义的话 _ 会匹配任意单字符、命中几乎所有帖子"},
		{`100\%`, nil, "反斜杠也是字面量，标题里没有「100\\%」这个串"},
		{"纯棉", []int64{pct.ID}, "普通中文关键词照常工作"},
	}
	for _, tc := range cases {
		t.Run("keyword="+tc.keyword, func(t *testing.T) {
			page := fetchSquare(t, "keyword="+urlQueryEscape(tc.keyword), "")
			if got := idsOf(page); !sameIDs(got, tc.want) {
				t.Errorf("%s\n期望 %v，实际 %v", tc.why, tc.want, got)
			}
		})
	}

	// 反向兜底：上面那几条「% → 只命中 pct」的断言，只有在「广场上确实有很多条帖子」
	// 的前提下才有意义。如果种子只剩一条，那么「转义坏了、% 匹配全部」和「转义正常」
	// 的结果是一样的，测试会静默地失去鉴别力。
	all := fetchSquare(t, "", "")
	if all.Total < 5 {
		t.Fatalf("种子数据不对：广场上一共只有 %d 条，不足以说明「%% 没有匹配全部」", all.Total)
	}
}

// TestM2KeywordIsCaseInsensitive 钉住 ILIKE 的 I。
//
// 校园里的物品名混着中英文，「AirPods」和「airpods」在用户看来是同一个词。
// 用 LIKE 的话搜小写会一条都搜不到 —— 而这种 bug 在测试里很难被发现，
// 因为测试种子往往全是中文（中文没有大小写）。所以这里必须专门造英文种子。
func TestM2KeywordIsCaseInsensitive(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "caser", "correct-horse-battery")

	// 标题里是大写开头，描述里是全小写：两边都要能被任意大小写的关键词命中。
	body := lostBody("丢了一副 AirPods Pro", "13800000000")
	body["description"] = "airpods 的充电盒上贴了一张皮卡丘贴纸"
	item := createItem(t, me, body)

	for _, kw := range []string{"airpods", "AirPods", "AIRPODS", "AiRpOdS", "AirPods Pro", "皮卡丘"} {
		t.Run("keyword="+kw, func(t *testing.T) {
			got := idsOf(fetchSquare(t, "keyword="+urlQueryEscape(kw), ""))
			if !sameIDs(got, []int64{item.ID}) {
				t.Errorf("关键词 %q 应该命中帖子 %d，实际 %v", kw, item.ID, got)
			}
		})
	}
}

// ---------- 分页与排序 ----------

func TestM2Paging(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "pager", "correct-horse-battery")

	const n = 25
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, createItem(t, me, lostBody(paddedTitle(i), "13800000000")).ID)
	}

	t.Run("默认 page_size=20", func(t *testing.T) {
		p := fetchSquare(t, "", "")
		if len(p.List) != 20 || p.Total != n || p.Page != 1 || p.PageSize != 20 {
			t.Errorf("默认分页不对：len=%d total=%d page=%d page_size=%d",
				len(p.List), p.Total, p.Page, p.PageSize)
		}
	})

	t.Run("第二页拿到剩下的 5 条", func(t *testing.T) {
		p := fetchSquare(t, "page=2", "")
		if len(p.List) != 5 || p.Total != n || p.Page != 2 {
			t.Errorf("第二页不对：len=%d total=%d page=%d", len(p.List), p.Total, p.Page)
		}
	})

	t.Run("两页不重叠", func(t *testing.T) {
		one := idsOf(fetchSquare(t, "page=1&page_size=10", ""))
		two := idsOf(fetchSquare(t, "page=2&page_size=10", ""))
		three := idsOf(fetchSquare(t, "page=3&page_size=10", ""))
		if len(one) != 10 || len(two) != 10 || len(three) != 5 {
			t.Errorf("分页大小不对：%d / %d / %d", len(one), len(two), len(three))
		}
		seen := map[int64]bool{}
		for _, group := range [][]int64{one, two, three} {
			for _, id := range group {
				if seen[id] {
					t.Errorf("帖子 %d 出现了两次 —— 分页在翻页时漏了或者重了", id)
				}
				seen[id] = true
			}
		}
		if len(seen) != n {
			t.Errorf("三页加起来应该有 %d 条不同的帖子，实际 %d 条", n, len(seen))
		}
	})

	t.Run("超出最后一页是空列表而不是错误", func(t *testing.T) {
		p := fetchSquare(t, "page=99", "")
		if len(p.List) != 0 {
			t.Errorf("应该返回空列表，实际 %d 条", len(p.List))
		}
		if p.Total != n {
			t.Errorf("total 应该仍然是 %d（它是筛选后的总数，不是本页条数），实际 %d", n, p.Total)
		}
		if p.List == nil {
			t.Error("list 应该是 [] 而不是 null（前端直接 .map）")
		}
	})

	t.Run("page_size=100 是上限", func(t *testing.T) {
		p := fetchSquare(t, "page_size=100", "")
		if p.PageSize != 100 {
			t.Errorf("page_size 应该是 100，实际 %d", p.PageSize)
		}
	})
}

func TestM2PagingRejectsBadParams(t *testing.T) {
	harness.TruncateAll(t)

	// #14 是公开接口，所以这些参数校验必须能在**未登录**的情况下给出有用的错误。
	for _, q := range []string{
		"page=0", "page=-1", "page=abc", "page=1.5",
		"page_size=0", "page_size=-1", "page_size=101", "page_size=abc",
		"item_type=claim", "item_type=LOST",
		"category_id=abc", "category_id=0", "category_id=-1",
		"location_id=abc", "location_id=0",
		"status=archived", "status=OPEN",
		"sort=id", "sort=title", "sort=status",
		// 下面两条是注入尝试。sort 会走两张白名单（service.sortWhitelist 决定
		// 「用户能请求什么」，repo.itemSortColumns 决定「什么能被拼进 SQL」），
		// 任何一张写漏了都不会变成注入，只会变成 VALIDATION 或者 500 ——
		// 而 500 也是错的，所以这里必须断言是 VALIDATION。
		//
		// ⚠ 分号和空格**必须百分号编码**，这是 Go 客户端的两个坑：
		//   - url.ParseQuery 从 Go 1.17 起拒绝把 `;` 当分隔符，遇到它整个查询串解析失败，
		//     c.Query("sort") 返回 ""，于是请求会**成功**（走默认排序），测试变绿但什么也没测到。
		//   - http.NewRequest 遇到 URL 里的裸空格直接返回 error。
		"sort=created_at%3BDROP%20TABLE%20items",
		"sort=(SELECT%201)",
	} {
		t.Run(q, func(t *testing.T) {
			r := harness.Get(t, "/api/items?"+q, "")
			RequireCode(t, r, apperr.CodeValidation)
			if r.HTTPStatus != http.StatusBadRequest {
				t.Errorf("期望 HTTP 400，实际 %d", r.HTTPStatus)
			}
		})
	}

	// 反向：白名单里的值必须放行。少了这一半，「把所有参数都拒掉」也能让上面全绿。
	for _, q := range []string{
		"page=1", "page=999", "page_size=1", "page_size=20", "page_size=100",
		"item_type=lost", "item_type=found",
		"category_id=" + itoa(catPhone), "location_id=" + itoa(locLibrary),
		"status=open", "status=closed",
		"sort=created_at", "sort=lost_at", "sort=found_at",
		"keyword=", "sort=", "status=",
	} {
		t.Run("放行 "+q, func(t *testing.T) {
			RequireOK(t, harness.Get(t, "/api/items?"+q, ""), "GET /api/items?"+q)
		})
	}

	// #19 用同一套解析，但 deleted 是允许的
	me := harness.RegisterAndLogin(t, "mineparams", "correct-horse-battery")
	RequireOK(t, harness.Get(t, "/api/my/items?status=deleted", me.Token), "GET /api/my/items?status=deleted")
	requireField(t, harness.Get(t, "/api/my/items?page=0", me.Token), "page")
}

// TestM2Sorting 钉住 sort 参数和默认排序。
//
// ORDER BY 是 `<列> DESC NULLS LAST, i.id DESC`。两个细节都值得测：
//
//	NULLS LAST：sort=lost_at 时，found 帖的 lost_at 全是 NULL。
//	  PG 的 DESC 默认把 NULL 排在**最前面**，那样广场上「按丢失时间排序」
//	  的头几条会全是没有丢失时间的 found 帖 —— 而用户点这个排序按钮
//	  想看的恰恰是「最近丢的东西」。
//	  （注意：found 帖在 sort=lost_at 时仍然会出现，因为排序不是筛选。
//	  想把它们去掉要显式加 item_type=lost。）
//	i.id DESC：次键。同一秒内发的两条帖子（在测试里是常态）必须有一个确定的顺序，
//	  否则翻页时同一条帖子可能出现在两页上，或者两页都不出现。
func TestM2Sorting(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "sorter", "correct-horse-battery")

	// 三条 lost，丢失时间递增；两条 found，没有 lost_at。
	l1 := createItem(t, me, lostItemAt("丢了第一样东西", "2026-09-01T10:00:00+08:00"))
	l2 := createItem(t, me, lostItemAt("丢了第二样东西", "2026-09-02T10:00:00+08:00"))
	l3 := createItem(t, me, lostItemAt("丢了第三样东西", "2026-09-03T10:00:00+08:00"))
	f1 := createItem(t, me, foundBody("捡到第一样东西", "13800000000"))
	f2 := createItem(t, me, foundBody("捡到第二样东西", "13800000000"))

	t.Run("默认按 created_at 倒序", func(t *testing.T) {
		// 创建顺序是 l1 l2 l3 f1 f2，所以倒序是 f2 f1 l3 l2 l1。
		want := []int64{f2.ID, f1.ID, l3.ID, l2.ID, l1.ID}
		if got := idsOf(fetchSquare(t, "", "")); !equalIDs(got, want) {
			t.Errorf("默认排序应该是 %v，实际 %v", want, got)
		}
	})

	t.Run("sort=lost_at：found 帖的 NULL 排在最后", func(t *testing.T) {
		want := []int64{l3.ID, l2.ID, l1.ID, f2.ID, f1.ID}
		if got := idsOf(fetchSquare(t, "sort=lost_at", "")); !equalIDs(got, want) {
			t.Errorf("sort=lost_at 应该是 %v（三条 lost 按丢失时间倒序，两条 found 因为 NULL 排最后），实际 %v",
				want, got)
		}
	})

	t.Run("sort=found_at：lost 帖的 NULL 排在最后", func(t *testing.T) {
		got := idsOf(fetchSquare(t, "sort=found_at", ""))
		want := []int64{f2.ID, f1.ID, l3.ID, l2.ID, l1.ID}
		if !equalIDs(got, want) {
			t.Errorf("sort=found_at 应该是 %v，实际 %v", want, got)
		}
	})

	t.Run("sort=created_at 和默认一致", func(t *testing.T) {
		if a, b := idsOf(fetchSquare(t, "", "")), idsOf(fetchSquare(t, "sort=created_at", "")); !equalIDs(a, b) {
			t.Errorf("显式 sort=created_at 应该和默认一样：%v vs %v", a, b)
		}
	})

	t.Run("排序和分页一起用时不重不漏", func(t *testing.T) {
		one := idsOf(fetchSquare(t, "sort=lost_at&page=1&page_size=2", ""))
		two := idsOf(fetchSquare(t, "sort=lost_at&page=2&page_size=2", ""))
		if !equalIDs(one, []int64{l3.ID, l2.ID}) {
			t.Errorf("第一页应该是 [l3 l2]，实际 %v", one)
		}
		if !equalIDs(two, []int64{l1.ID, f2.ID}) {
			t.Errorf("第二页应该是 [l1 f2]，实际 %v", two)
		}
	})
}

// ---------- 摘要形状 ----------

// TestM2SummaryShape 钉住 ItemSummary 和 ItemView 的区别。
//
// 摘要**没有** description、没有 images 数组（只有 cover_image 一张）、没有 updated_at。
// 少传这些字段对 20 行的列表来说是实打实的带宽差别，更重要的是
// 「前端在列表页拿不到 description」这件事本身就是一种约束 ——
// 它逼着详情页和列表页各司其职，而不是把详情逻辑悄悄搬到列表里。
//
// 查的是原始 JSON 的键集合，不是解码后的结构体：结构体只会读到声明过的字段，
// 多出来的字段它一声不响地丢掉，而「列表悄悄开始返回 description」正是这里要防的。
func TestM2SummaryShape(t *testing.T) {
	s := seedSquare(t)

	// 筛到只剩 a 一条：既要验形状又要验具体字段值，只有一条时期望值才不含歧义。
	// （只用 item_type=lost 会同时命中 f，而 List[0] 是按 created_at 倒序的第一条 = f，
	// 那样下面的 category/location 断言就会去校验另一条帖子。）
	q := "item_type=lost&category_id=" + itoa(catPhone)

	page := fetchSquare(t, q, "")
	if len(page.List) != 1 || page.List[0].ID != s.ids["a"] {
		t.Fatalf("?%s 应该只返回 a，实际 %v", q, idsOf(page))
	}

	r := harness.Get(t, "/api/items?"+q, "")
	RequireOK(t, r, "GET /api/items")
	var raw struct {
		List []map[string]json.RawMessage `json:"list"`
	}
	r.DataInto(t, &raw)
	if len(raw.List) != 1 {
		t.Fatalf("原始 JSON 里应该有 1 条，实际 %d 条", len(raw.List))
	}

	want := map[string]bool{
		"id": true, "item_type": true, "title": true, "status": true,
		"category_id": true, "category_name": true,
		"location_id": true, "location_name": true,
		"lost_at": true, "found_at": true, "contact": true,
		"cover_image": true, "author_id": true, "author_name": true, "created_at": true,
	}
	for key := range raw.List[0] {
		if !want[key] {
			t.Errorf("摘要里多了一个字段 %q —— 列表接口不该返回它", key)
		}
		delete(want, key)
	}
	for key := range want {
		t.Errorf("摘要里少了字段 %q", key)
	}

	// 摘要里不该有 contact 之外的隐私字段
	for _, forbidden := range []string{"description", "images", "updated_at", "location_detail",
		"contact_locked", "author", "view_count", "real_name"} {
		if _, ok := raw.List[0][forbidden]; ok {
			t.Errorf("摘要里出现了 %q", forbidden)
		}
	}

	// 分类和地点在摘要里是**平铺**的两个字段，不是嵌套对象。
	// 和 #15 的嵌套形状不同是刻意的：详情页要把「分类」当成一个东西塞进
	// 级联选择器的 value，列表页只是要显示两个名字。
	sum := page.List[0]
	if sum.CategoryID != catPhone || sum.CategoryName != "手机" {
		t.Errorf("category 应该是 {10, 手机}，实际 {%d, %q}", sum.CategoryID, sum.CategoryName)
	}
	if sum.LocationID != locLibrary || sum.LocationName != "图书馆" {
		t.Errorf("location 应该是 {70, 图书馆}，实际 {%d, %q}", sum.LocationID, sum.LocationName)
	}
	if sum.AuthorID != s.owner.UserID || sum.AuthorName == "" {
		t.Errorf("author 应该是 {%d, 昵称}，实际 {%d, %q}", s.owner.UserID, sum.AuthorID, sum.AuthorName)
	}
	if sum.LostAt == "" || sum.FoundAt != "" {
		t.Errorf("lost 帖的时间不对：lost_at=%q found_at=%q", sum.LostAt, sum.FoundAt)
	}
}

func TestM2CoverImage(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "coverer", "correct-horse-battery")

	first := upload(t, me, 1024)
	second := upload(t, me, 1024)

	withTwo := lostBody("有两张图", "13800000000")
	withTwo["image_paths"] = []string{first.Path, second.Path}
	a := createItem(t, me, withTwo)

	withNone := lostBody("没有图", "13800000000")
	b := createItem(t, me, withNone)

	page := fetchSquare(t, "", "")
	for _, sum := range page.List {
		switch sum.ID {
		case a.ID:
			// 封面是 sort_order 最小的那张，也就是请求里 image_paths 的第一张。
			if sum.CoverImage != first.URL {
				t.Errorf("封面应该是第一张图 %s，实际 %q", first.URL, sum.CoverImage)
			}
		case b.ID:
			// 没有图时是**空串**，不是 null，也不是一个只有前缀的 "/uploads"。
			// 后者会在前端渲染成一次注定 404 的请求。
			if sum.CoverImage != "" {
				t.Errorf("没有图的帖子 cover_image 应该是空串，实际 %q", sum.CoverImage)
			}
		}
	}

	// 原始 JSON 里也必须是 "" 而不是 null：前端 `src={cover || placeholder}`
	// 对 null 和 "" 的处理一样，但 TS 的类型是 string 还是 string|null 不一样，
	// 而列表里这两种值混着出现的话，类型就只能取那个更宽的。
	r := harness.Get(t, "/api/items", "")
	RequireOK(t, r, "GET /api/items")
	if strings.Contains(string(r.Data), `"cover_image":null`) {
		t.Errorf("cover_image 出现了 null，应该一律是字符串\n%s", truncate(string(r.Data)))
	}
}

// TestM2ListEmptyIsNotNull 钉住空结果的 JSON 形状。
//
// 一个刚建好的空库、或者一次没有命中的搜索，返回的都必须是
// {"list":[],"total":0,...} 而不是 {"list":null,...}。
// 前端的 data.list.map(...) 在 null 上会直接崩，
// 而「搜一个不存在的关键词」是用户天天会做的事。
func TestM2ListEmptyIsNotNull(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "emptylist", "correct-horse-battery")

	for _, path := range []string{"/api/items", "/api/my/items"} {
		r := harness.Get(t, path+"?keyword=什么都没有", me.Token)
		RequireOK(t, r, "GET "+path)
		if strings.Contains(string(r.Data), `"list":null`) {
			t.Errorf("%s 的空结果里 list 是 null，应该是 []\n%s", path, truncate(string(r.Data)))
		}
		var p itemPage
		r.DataInto(t, &p)
		if p.List == nil {
			t.Errorf("%s 解出来的 List 是 nil", path)
		}
		if p.Total != 0 {
			t.Errorf("%s 的 total 应该是 0，实际 %d", path, p.Total)
		}
	}
}

// ---------- 辅助 ----------

// fetchSquare / fetchMine 是两个列表端点的快捷方式。
func fetchSquare(t *testing.T, query, token string) itemPage {
	t.Helper()
	path := "/api/items"
	if query != "" {
		path += "?" + query
	}
	r := harness.Get(t, path, token)
	RequireOK(t, r, "GET "+path)
	var p itemPage
	r.DataInto(t, &p)
	return p
}

func fetchMine(t *testing.T, query string, s Session) itemPage {
	t.Helper()
	path := "/api/my/items"
	if query != "" {
		path += "?" + query
	}
	r := harness.Get(t, path, s.Token)
	RequireOK(t, r, "GET "+path)
	var p itemPage
	r.DataInto(t, &p)
	return p
}

func idsOf(p itemPage) []int64 {
	out := make([]int64, 0, len(p.List))
	for _, s := range p.List {
		out = append(out, s.ID)
	}
	return out
}

// equalIDs 按顺序比；sameIDs 忽略顺序比。
//
// 两个都要：排序测试必须按顺序比（顺序就是被测对象），
// 而筛选测试不该关心顺序（那是排序测试的职责）—— 用同一个函数
// 会让筛选测试在有人改默认排序时莫名其妙地变红。
func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[int64]int, len(a))
	for _, id := range a {
		set[id]++
	}
	for _, id := range b {
		set[id]--
		if set[id] < 0 {
			return false
		}
	}
	return true
}

// lostItemAt 造一条指定 lost_at 的合法 lost 帖（last_seen_at 固定在它前一天）。
func lostItemAt(title, lostAt string) map[string]any {
	b := lostBody(title, "13800000000")
	b["last_seen_at"] = "2026-08-31T10:00:00+08:00"
	b["lost_at"] = lostAt
	return b
}

// paddedTitle 造一个能按字符串排序看出先后的标题（00、01、…、24）。
func paddedTitle(i int) string {
	return "批量种子帖 " + string(rune('0'+i/10)) + string(rune('0'+i%10))
}

// urlQueryEscape 只转义 keyword 里那几个会把查询串切开或者让请求发不出去的字符。
//
// 不用 url.QueryEscape：它会把中文也转成 %XX，那样测试里打印出来的
// 查询串就没法读了，而失败信息可读性在这个包里是刻意维护的东西。
//
// 空格必须转义：裸空格会让 http.NewRequest 造出一个非法的请求行，
// 服务端直接回一句纯文本的「400 Bad Request」—— 连信封都不是，
// 于是测试挂在一个和被测行为毫无关系的地方。
func urlQueryEscape(s string) string {
	r := strings.NewReplacer(
		"%", "%25", "&", "%26", "=", "%3D", "+", "%2B", "#", "%23", `\`, "%5C", " ", "%20")
	return r.Replace(s)
}

package smoketest

import (
	"strings"
	"testing"
)

// 本文件覆盖 #7 GET /api/categories 和 #8 GET /api/locations。
//
// 两个接口都是公开只读，返回的是**整棵树**（不分页）。所以这里的断言分两类：
//   1. 结构性质：几层、顺序、children 永远是数组、is_freeform 只在一处为真
//   2. 和数据库自洽：树里的节点总数必须等于表里 is_active 的行数
//
// 第 2 类刻意不写死 55 / 91：数字是迁移种下去的，将来 admin 加一行分类（#9）
// 或者从 Adminer 里停用一行，写死的数字就会红 —— 而那时候错的不是代码。
// 拿「接口返回的节点数」和「同一张表 count(*)」比，才是真正在测「建树没有漏行」。

// ---------- 响应形状 ----------

type categoryNode struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Level     int             `json:"level"`
	SortOrder int             `json:"sort_order"`
	Children  []*categoryNode `json:"children"`
}

type locationNode struct {
	ID         int64           `json:"id"`
	Name       string          `json:"name"`
	Level      int             `json:"level"`
	IsFreeform bool            `json:"is_freeform"`
	SortOrder  int             `json:"sort_order"`
	Children   []*locationNode `json:"children"`
}

// ---------- #7 分类树 ----------

func TestM2CategoryTree(t *testing.T) {
	harness.TruncateAll(t)

	r := harness.Get(t, "/api/categories", "")
	RequireOK(t, r, "GET /api/categories")

	var tree []*categoryNode
	r.DataInto(t, &tree)

	dbRows := harness.Count(t, `SELECT count(*) FROM categories WHERE is_active`)
	if got := countCategoryNodes(tree); got != dbRows {
		t.Fatalf("树里有 %d 个节点，但 categories 表里 is_active 的有 %d 行 —— 建树漏了行或者多算了",
			got, dbRows)
	}

	// 计划 §3.1：分类只有两级，9 个大类 + 46 个小类。
	if len(tree) != 9 {
		t.Errorf("应该有 9 个一级大类，实际 %d 个", len(tree))
	}
	leaves := 0
	for _, root := range tree {
		if root.Level != 1 {
			t.Errorf("根节点 %q 的 level 是 %d，应该是 1", root.Name, root.Level)
		}
		for _, child := range root.Children {
			if child.Level != 2 {
				t.Errorf("%q 下的 %q level 是 %d，应该是 2", root.Name, child.Name, child.Level)
			}
			if len(child.Children) != 0 {
				t.Errorf("小类 %q 不该有子节点，实际有 %d 个（分类只有两级）",
					child.Name, len(child.Children))
			}
			leaves++
		}
	}
	if leaves != 46 {
		t.Errorf("应该有 46 个二级小类，实际 %d 个", leaves)
	}
	if leaves+9 != dbRows {
		t.Errorf("9 + %d 不等于表里的 %d 行", leaves, dbRows)
	}
}

// TestM2CategoryTreeKnownBranch 验一条**具体的**已知分支。
//
// 只验数量的话，「树建对了但挂错了父亲」这种 bug 看不出来 ——
// 节点总数一样，前端渲染出来的下拉框却完全是另一回事。
// 所以挑一条计划 §2.3 里用作例子的路径钉死：衣物箱包 → 钱包。
func TestM2CategoryTreeKnownBranch(t *testing.T) {
	harness.TruncateAll(t)

	var tree []*categoryNode
	r := harness.Get(t, "/api/categories", "")
	RequireOK(t, r, "GET /api/categories")
	r.DataInto(t, &tree)

	bags := findCategory(tree, "衣物箱包")
	if bags == nil {
		t.Fatal("树里找不到「衣物箱包」这个大类")
	}
	if bags.ID != 5 {
		t.Errorf("「衣物箱包」的 id 应该是 5（迁移里的显式 id），实际 %d", bags.ID)
	}

	wallet := findCategory(bags.Children, "钱包")
	if wallet == nil {
		names := make([]string, 0, len(bags.Children))
		for _, c := range bags.Children {
			names = append(names, c.Name)
		}
		t.Fatalf("「衣物箱包」下找不到「钱包」，实际有 %v", names)
	}
	if wallet.ID != 36 {
		t.Errorf("「钱包」的 id 应该是 36，实际 %d", wallet.ID)
	}

	// 「数码电子 → 手机」是 §5 匹配算法示例里用的另一条路径，也钉一下。
	digital := findCategory(tree, "数码电子")
	if digital == nil {
		t.Fatal("树里找不到「数码电子」")
	}
	if phone := findCategory(digital.Children, "手机"); phone == nil || phone.ID != 10 {
		t.Errorf("「数码电子 → 手机」不对：%+v", phone)
	}
}

// TestM2CategoryTreeOrderingIsStable 钉住「输入顺序即输出顺序」。
//
// repo 按 (level, sort_order, id) 排，建树时不再重排。如果哪天有人给建树加了
// 一次 sort（或者去掉了 repo 的 ORDER BY），前端的下拉框顺序就会变 ——
// 那是用户天天看的东西，顺序一乱就找不到熟悉的条目。
//
// 一级大类的顺序直接写死成迁移里的那九行：这九个名字是产品语义
// （计划 §3.5），不是可以从数据库推出来的东西，写死才有意义。
func TestM2CategoryTreeOrderingIsStable(t *testing.T) {
	harness.TruncateAll(t)

	var tree []*categoryNode
	r := harness.Get(t, "/api/categories", "")
	RequireOK(t, r, "GET /api/categories")
	r.DataInto(t, &tree)

	want := []string{"数码电子", "证件卡片", "钥匙", "书籍文具", "衣物箱包", "生活用品", "运动器材", "首饰配饰", "其他"}
	got := make([]string, 0, len(tree))
	for _, n := range tree {
		got = append(got, n.Name)
	}
	if len(got) != len(want) {
		t.Fatalf("一级大类应该是 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个大类应该是 %q，实际 %q（完整顺序：%v）", i+1, want[i], got[i], got)
		}
	}

	keys := make([]orderKey, 0, len(tree))
	for _, n := range tree {
		keys = append(keys, orderKey{name: n.Name, sortOrder: n.SortOrder, id: n.ID})
	}
	assertOrdered(t, "分类树根节点", keys)

	for _, root := range tree {
		kids := make([]orderKey, 0, len(root.Children))
		for _, c := range root.Children {
			kids = append(kids, orderKey{name: c.Name, sortOrder: c.SortOrder, id: c.ID})
		}
		assertOrdered(t, "「"+root.Name+"」下的小类", kids)
	}
}

func TestM2CategoryTreeIsPublic(t *testing.T) {
	harness.TruncateAll(t)
	// 未登录也要能用：拿不到字典树，前端那两个级联下拉框根本渲染不出来，
	// 而广场本身是允许匿名浏览的（§2.4 途径三）。
	RequireOK(t, harness.Get(t, "/api/categories", ""), "匿名 GET /api/categories")
	RequireOK(t, harness.Get(t, "/api/locations", ""), "匿名 GET /api/locations")
}

// ---------- #8 地点树 ----------

func TestM2LocationTree(t *testing.T) {
	harness.TruncateAll(t)

	r := harness.Get(t, "/api/locations", "")
	RequireOK(t, r, "GET /api/locations")

	var tree []*locationNode
	r.DataInto(t, &tree)

	dbRows := harness.Count(t, `SELECT count(*) FROM locations WHERE is_active`)
	if got := countLocationNodes(tree); got != dbRows {
		t.Fatalf("树里有 %d 个节点，但 locations 表里 is_active 的有 %d 行", got, dbRows)
	}

	// 计划 §3.6：3 个一级区 + 8 个二级子类 + 80 个三级具体地点。
	if len(tree) != 3 {
		t.Errorf("应该有 3 个一级区，实际 %d 个", len(tree))
	}

	level2, level3, freeform := 0, 0, 0
	var walk func(nodes []*locationNode, depth int)
	walk = func(nodes []*locationNode, depth int) {
		for _, n := range nodes {
			if n.Level != depth {
				t.Errorf("节点 %q 在树的第 %d 层，但它自己声称 level=%d", n.Name, depth, n.Level)
			}
			if n.IsFreeform {
				freeform++
			}
			switch depth {
			case 2:
				level2++
			case 3:
				level3++
			}
			if depth > 3 {
				t.Fatalf("地点树最多三级，%q 出现在了第 %d 层", n.Name, depth)
			}
			walk(n.Children, depth+1)
		}
	}
	walk(tree, 1)

	if level2 != 8 {
		t.Errorf("应该有 8 个二级子类，实际 %d 个", level2)
	}
	if level3 != 80 {
		t.Errorf("应该有 80 个三级地点，实际 %d 个", level3)
	}
	// 全库只有「其他」一行 is_freeform=true。多了就意味着前端会在别的地方也弹出
	// 「请填写最近的建筑」的红色警告，而那里根本没有这个语义。
	if freeform != 1 {
		t.Errorf("is_freeform=true 的节点应该恰好 1 个，实际 %d 个", freeform)
	}
	if n := harness.Count(t, `SELECT count(*) FROM locations WHERE is_active AND is_freeform`); n != 1 {
		t.Errorf("数据库里 is_freeform 的行数也应该是 1，实际 %d", n)
	}
}

// TestM2LocationTreeOtherIsASelectableLeaf 钉住「其他」这个特例。
//
// 它是 level=1 且 is_freeform=true 的**一级叶子** —— 没有子节点，但必须能被选中。
// 这条如果坏了，用户在任何字典里找不到对应地点时就彻底发不了帖，
// 而发帖时选不到「其他」这件事，从 API 响应上完全看不出来（树看起来一切正常）。
func TestM2LocationTreeOtherIsASelectableLeaf(t *testing.T) {
	harness.TruncateAll(t)

	var tree []*locationNode
	r := harness.Get(t, "/api/locations", "")
	RequireOK(t, r, "GET /api/locations")
	r.DataInto(t, &tree)

	var other *locationNode
	for _, n := range tree {
		if n.IsFreeform {
			other = n
		}
	}
	if other == nil {
		t.Fatal("树里没有任何 is_freeform=true 的节点")
	}
	if other.Name != "其他" {
		t.Errorf("is_freeform 的节点应该叫「其他」，实际叫 %q", other.Name)
	}
	if other.Level != 1 {
		t.Errorf("「其他」是 level=1 的一级叶子，实际 level=%d", other.Level)
	}
	if len(other.Children) != 0 {
		t.Errorf("「其他」不该有子节点，实际有 %d 个", len(other.Children))
	}
	if other.ID != 3 {
		t.Errorf("「其他」的 id 应该是 3（迁移里的显式 id），实际 %d", other.ID)
	}
}

// TestM2LocationTreeKnownBranch 验计划 §2.3 用过的那条三级路径：
// 教学区（南侧）→ 场馆与公共建筑 → 图书馆。
func TestM2LocationTreeKnownBranch(t *testing.T) {
	harness.TruncateAll(t)

	var tree []*locationNode
	r := harness.Get(t, "/api/locations", "")
	RequireOK(t, r, "GET /api/locations")
	r.DataInto(t, &tree)

	teaching := findLocation(tree, "教学区（南侧）")
	if teaching == nil {
		t.Fatal("找不到一级区「教学区（南侧）」（注意是全角括号）")
	}
	if teaching.ID != 2 {
		t.Errorf("「教学区（南侧）」的 id 应该是 2，实际 %d", teaching.ID)
	}

	building := findLocation(teaching.Children, "场馆与公共建筑")
	if building == nil {
		t.Fatal("「教学区（南侧）」下找不到「场馆与公共建筑」")
	}
	if building.ID != 9 {
		t.Errorf("「场馆与公共建筑」的 id 应该是 9，实际 %d", building.ID)
	}

	library := findLocation(building.Children, "图书馆")
	if library == nil {
		t.Fatal("「场馆与公共建筑」下找不到「图书馆」")
	}
	if library.ID != 70 {
		t.Errorf("「图书馆」的 id 应该是 70，实际 %d", library.ID)
	}
	if library.Level != 3 {
		t.Errorf("「图书馆」应该是 level=3，实际 %d", library.Level)
	}
}

// TestM2ChildrenSerializesAsEmptyArray 钉住 JSON 形状：叶子节点的 children 是 [] 不是 null。
//
// 对前端来说 `[]` 和 `null` 是两种类型（Array<T> vs Array<T> | null）。
// 返回 null 的话，每个递归渲染 children 的组件都得先判空 ——
// 而漏判的那一处只在「某个大类恰好没有小类」时才崩，开发库里几乎不会遇到。
//
// 顺带钉住「响应里不出现 parent_id」：父指针已经蕴含在嵌套结构里了，
// 再给一遍就会出现「children 说 A 是 B 的父亲、parent_id 说不是」这种自相矛盾的可能。
func TestM2ChildrenSerializesAsEmptyArray(t *testing.T) {
	harness.TruncateAll(t)

	for _, path := range []string{"/api/categories", "/api/locations"} {
		r := harness.Get(t, path, "")
		RequireOK(t, r, "GET "+path)
		raw := string(r.Data)
		if containsNullChildren(raw) {
			t.Errorf("%s 的响应里有 \"children\":null，应该是 []\n%s", path, truncate(raw))
		}
		if strings.Contains(raw, `"parent_id"`) {
			t.Errorf("%s 的响应里出现了 parent_id，树形响应不该再暴露父指针\n%s", path, truncate(raw))
		}
		if raw == "null" || raw == "[]" {
			t.Errorf("%s 返回了空的 data，字典表应该是迁移种满的", path)
		}
	}
}

// TestM2LocationTreeOrderingIsStable 同分类那条，只是对象换成地点树。
//
// 地点树是三级的，所以要把每一层都走一遍 —— 只查根节点的顺序等于没查：
// 「教学区（南侧）→ 场馆与公共建筑」下面挂了 20 多个三级地点，
// 那才是用户在级联选择器第三格里真正会滚动的列表。
func TestM2LocationTreeOrderingIsStable(t *testing.T) {
	harness.TruncateAll(t)

	var tree []*locationNode
	r := harness.Get(t, "/api/locations", "")
	RequireOK(t, r, "GET /api/locations")
	r.DataInto(t, &tree)

	want := []string{"生活区（北侧）", "教学区（南侧）", "其他"}
	if len(tree) != len(want) {
		t.Fatalf("一级区应该是 %v，实际有 %d 个", want, len(tree))
	}
	for i, n := range tree {
		if n.Name != want[i] {
			t.Errorf("第 %d 个一级区应该是 %q，实际 %q", i+1, want[i], n.Name)
		}
	}

	var walk func(nodes []*locationNode, path string)
	walk = func(nodes []*locationNode, path string) {
		keys := make([]orderKey, 0, len(nodes))
		for _, n := range nodes {
			keys = append(keys, orderKey{name: n.Name, sortOrder: n.SortOrder, id: n.ID})
		}
		assertOrdered(t, path, keys)
		for _, n := range nodes {
			walk(n.Children, path+" → "+n.Name)
		}
	}
	walk(tree, "地点树根节点")
}

// ---------- 辅助 ----------

func countCategoryNodes(nodes []*categoryNode) int {
	total := 0
	for _, n := range nodes {
		total += 1 + countCategoryNodes(n.Children)
	}
	return total
}

func countLocationNodes(nodes []*locationNode) int {
	total := 0
	for _, n := range nodes {
		total += 1 + countLocationNodes(n.Children)
	}
	return total
}

func findCategory(nodes []*categoryNode, name string) *categoryNode {
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	return nil
}

func findLocation(nodes []*locationNode, name string) *locationNode {
	for _, n := range nodes {
		if n.Name == name {
			return n
		}
	}
	return nil
}

// orderKey / assertOrdered 检查一层节点的顺序。
//
// repo 的 ORDER BY 是 (level, sort_order, id)，所以 sort_order 相同时必须按 id 升序 ——
// 迁移里同一个父亲下的 sort_order 是有重复的（比如分类的 level-2 每个大类都从 1 开始，
// 但地点的三级里有几行共用一个 sort_order），少了 id 这个次键，
// 同一层的顺序就退化成「数据库当时怎么读出来就怎么排」，重启一次可能就变。
type orderKey struct {
	name      string
	sortOrder int
	id        int64
}

func assertOrdered(t *testing.T, what string, keys []orderKey) {
	t.Helper()
	for i := 1; i < len(keys); i++ {
		prev, cur := keys[i-1], keys[i]
		switch {
		case cur.sortOrder < prev.sortOrder:
			t.Errorf("%s 的顺序乱了：%q(sort=%d,id=%d) 排在 %q(sort=%d,id=%d) 前面",
				what, prev.name, prev.sortOrder, prev.id, cur.name, cur.sortOrder, cur.id)
		case cur.sortOrder == prev.sortOrder && cur.id < prev.id:
			t.Errorf("%s 里 sort_order 相同（都是 %d）时应该按 id 升序：%q(id=%d) 排在 %q(id=%d) 前面",
				what, cur.sortOrder, prev.name, prev.id, cur.name, cur.id)
		}
	}
}

// containsNullChildren 报告一段 JSON 里有没有出现 "children":null。
//
// 不用 json.Unmarshal 再遍历是因为那样只能查到**结构体里声明过的**字段；
// 直接搜原始文本连「多出来一个 null 字段」都能一起发现。
func containsNullChildren(raw string) bool {
	return strings.Contains(raw, `"children":null`)
}

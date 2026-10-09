package model

import (
	"encoding/json"
	"testing"
)

// 这两个函数是 #7 / #8 的全部逻辑：把扁平的行组装成树。
//
// 它们没有 DB、没有 HTTP，所以属于计划 §10 的第①层 —— 毫秒级、无外部依赖。
// 「孤儿节点怎么办」「兄弟顺序保不保」这类问题必须在这一层被钉死，
// 因为一旦漏到集成测试，失败信息里会混进一堆和建树无关的东西（迁移、连接池、信封）。

func idptr(v int64) *int64 { return &v }

// catIDs 把一棵树的每一层展开成 id 序列，方便断言。
func catIDs(nodes []*CategoryNode) []int64 {
	out := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.ID)
	}
	return out
}

func eqInt64s(t *testing.T, what string, got, want []int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：长度 %d，期望 %d\n got=%v\nwant=%v", what, len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s：第 %d 个是 %d，期望 %d\n got=%v\nwant=%v", what, i, got[i], want[i], got, want)
		}
	}
}

func TestBuildCategoryTree(t *testing.T) {
	// 行的顺序刻意按 repo 真实的排序 (level, sort_order, id)：
	// 所有 level=1 先来，然后所有 level=2。这意味着**子节点永远在父节点之后**出现，
	// 所以「父节点还没建出来子节点就来挂载」这个错误顺序不会发生 —— 但如果有人
	// 改了 repo 的 ORDER BY，这条测试的输入仍然是那个顺序，树就会散架。这正是我们要的。
	rows := []Category{
		{ID: 1, ParentID: nil, Name: "数码电子", Level: 1, SortOrder: 1},
		{ID: 2, ParentID: nil, Name: "证件卡片", Level: 1, SortOrder: 2},
		{ID: 9, ParentID: nil, Name: "其他", Level: 1, SortOrder: 9},
		{ID: 10, ParentID: idptr(1), Name: "手机", Level: 2, SortOrder: 1},
		{ID: 11, ParentID: idptr(1), Name: "笔记本电脑", Level: 2, SortOrder: 2},
		{ID: 12, ParentID: idptr(1), Name: "平板", Level: 2, SortOrder: 3},
		{ID: 19, ParentID: idptr(2), Name: "身份证", Level: 2, SortOrder: 1},
	}

	tree := BuildCategoryTree(rows)

	eqInt64s(t, "根节点", catIDs(tree), []int64{1, 2, 9})

	// 「大类下只有一个小类」是 Children 存值（而不是存指针）时的典型症状：
	// append 触发拷贝，之后挂上去的兄弟被隔离在拷贝之外。三个小类都在才算过。
	eqInt64s(t, "数码电子的子节点", catIDs(tree[0].Children), []int64{10, 11, 12})
	eqInt64s(t, "证件卡片的子节点", catIDs(tree[1].Children), []int64{19})

	if tree[2].Name != "其他" {
		t.Errorf("第三个根节点应该是「其他」，实际是 %q", tree[2].Name)
	}
	if tree[0].Children[0].Name != "手机" {
		t.Errorf("节点内容没有带过来：期望「手机」，实际 %q", tree[0].Children[0].Name)
	}
	if tree[0].Level != 1 || tree[0].Children[0].Level != 2 {
		t.Errorf("level 没有带过来：根=%d 子=%d", tree[0].Level, tree[0].Children[0].Level)
	}
	if tree[0].SortOrder != 1 || tree[0].Children[1].SortOrder != 2 {
		t.Errorf("sort_order 没有带过来：根=%d 子=%d", tree[0].SortOrder, tree[0].Children[1].SortOrder)
	}
}

// TestBuildCategoryTreeOrphanPromotedToRoot 钉住「孤儿提升为根而不是丢弃」。
//
// 孤儿只有一种成因：父行 is_active=false 被 repo 的查询滤掉了，而子行还是 active。
// 丢弃的后果是「一批仍然可选的小类凭空消失」—— 用户发帖时选不到它们，
// 而且没有任何报错，因为程序的行为完全正常，只是少了几行。
func TestBuildCategoryTreeOrphanPromotedToRoot(t *testing.T) {
	rows := []Category{
		{ID: 1, ParentID: nil, Name: "数码电子", Level: 1, SortOrder: 1},
		{ID: 10, ParentID: idptr(999), Name: "手机", Level: 2, SortOrder: 1},
	}

	tree := BuildCategoryTree(rows)

	eqInt64s(t, "根节点", catIDs(tree), []int64{1, 10})
	if len(tree[0].Children) != 0 {
		t.Errorf("孤儿不该被挂到任何一个存在的父节点上，实际挂了 %d 个", len(tree[0].Children))
	}
}

// TestBuildCategoryTreeLeafChildrenIsNotNil 钉住 JSON 形状。
//
// `[]` 和 `null` 对前端是两种类型（Array<T> vs Array<T> | null）。
// 叶子节点返回 null 的话，前端每个用到 children 的地方都得先判空，
// 而漏判的那一处只会在「某个大类恰好没有小类」时才崩 —— 生产数据里几乎不会遇到。
func TestBuildCategoryTreeLeafChildrenIsNotNil(t *testing.T) {
	tree := BuildCategoryTree([]Category{{ID: 9, Name: "其他", Level: 1, SortOrder: 9}})

	if tree[0].Children == nil {
		t.Fatal("叶子节点的 Children 是 nil，应该是空切片")
	}
	raw, err := json.Marshal(tree)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	const want = `[{"id":9,"name":"其他","level":1,"sort_order":9,"children":[]}]`
	if string(raw) != want {
		t.Fatalf("JSON 形状不对\n got: %s\nwant: %s", raw, want)
	}
}

func TestBuildCategoryTreeEmpty(t *testing.T) {
	tree := BuildCategoryTree(nil)
	if tree == nil {
		t.Fatal("空输入应该返回空切片而不是 nil（理由同 TestBuildCategoryTreeLeafChildrenIsNotNil）")
	}
	raw, _ := json.Marshal(tree)
	if string(raw) != "[]" {
		t.Fatalf("空树应该序列化成 []，实际是 %s", raw)
	}
}

func TestBuildLocationTree(t *testing.T) {
	rows := []Location{
		{ID: 1, ParentID: nil, Name: "教学区", Level: 1, SortOrder: 1},
		{ID: 9, ParentID: nil, Name: "其他", Level: 1, SortOrder: 9, IsFreeform: true},
		{ID: 20, ParentID: idptr(1), Name: "场馆与公共建筑", Level: 2, SortOrder: 1},
		{ID: 50, ParentID: idptr(20), Name: "图书馆", Level: 3, SortOrder: 1},
	}

	tree := BuildLocationTree(rows)

	if len(tree) != 2 {
		t.Fatalf("应该有 2 个根节点，实际 %d", len(tree))
	}
	if len(tree[0].Children) != 1 || len(tree[0].Children[0].Children) != 1 {
		t.Fatalf("三级结构没有建出来：%+v", tree[0])
	}
	leaf := tree[0].Children[0].Children[0]
	if leaf.ID != 50 || leaf.Name != "图书馆" || leaf.Level != 3 {
		t.Errorf("第三层节点不对：%+v", leaf)
	}

	// is_freeform 是地点树独有的字段，也是「其他」这一行唯一的特殊之处：
	// 前端看到它要强制填「最近的建筑」，匹配算法看到它要跳过 Tier 1（计划 §3.6）。
	// 建树时漏带这个字段的话，两端都会静默地按普通节点处理 —— 不会报错，只会算错。
	if !tree[1].IsFreeform {
		t.Error("「其他」的 is_freeform 丢了")
	}
	if tree[0].IsFreeform || leaf.IsFreeform {
		t.Error("普通节点不该带 is_freeform=true")
	}

	raw, err := json.Marshal(tree[1])
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	const want = `{"id":9,"name":"其他","level":1,"is_freeform":true,"sort_order":9,"children":[]}`
	if string(raw) != want {
		t.Fatalf("JSON 形状不对（字段名或顺序和计划 §4 第 8 行不一致？）\n got: %s\nwant: %s", raw, want)
	}
}

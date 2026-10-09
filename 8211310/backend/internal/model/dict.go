package model

// 字典表（categories / locations）的行与对外树形节点。
//
// 两张表都是**自引用树**：parent_id 指向同一张表。分类两级、地点三级（计划 §3.1、§3.6）。
// 用自引用而不是「大类表 + 小类表」：层级数一变就要改表结构，而自引用只是多一行数据。

// Category 对应 categories 表的一行。
type Category struct {
	ID        int64
	ParentID  *int64 // level=1 时为 NULL（迁移里的 categories_parent_level CHECK 钉死了这层对应关系）
	Name      string
	Level     int // 1=大类，2=小类
	SortOrder int
	IsActive  bool
}

// Location 对应 locations 表的一行。
type Location struct {
	ID        int64
	ParentID  *int64
	Name      string
	Level     int // 1/2/3
	SortOrder int
	IsActive  bool
	// IsFreeform 标记「其他」这类**没有子节点、要用户自己输入文本**的条目。
	// 全库只有「其他」这一行是 true。前端看到它就要强制填「最近的建筑」并弹红色警告（§3.6），
	// 匹配算法看到它就直接跳过 Tier 1 进 Tier 2（§5.5）。
	IsFreeform bool
}

// CategoryNode 是 #7 GET /api/categories 的 data 元素，字段照计划 §4 第 7 行原样落地。
//
// Children 是**指针**切片，两个理由：
//  1. 建树时父节点先入 map、子节点后 append 进去。如果存值，append 的那一次拷贝会
//     把「父节点后续再收到的兄弟节点」永远隔离在拷贝之外 —— 这类 bug 的表现是
//     「大类下只有一个小类」，而且只在数据行数变多时才出现，极难往指针语义上想。
//  2. encoding/json 序列化 *T 和 T 的结果完全一样，前端看不出区别。
//
// Children 永远是空切片而不是 nil：JSON 里 `[]` 和 `null` 对前端是两种类型
// （`Array<T>` vs `Array<T> | null`），叶子节点返回 null 会逼前端到处判空。
type CategoryNode struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Level     int             `json:"level"`
	SortOrder int             `json:"sort_order"`
	Children  []*CategoryNode `json:"children"`
}

// LocationNode 是 #8 GET /api/locations 的 data 元素，比分类多一个 is_freeform。
type LocationNode struct {
	ID         int64           `json:"id"`
	Name       string          `json:"name"`
	Level      int             `json:"level"`
	IsFreeform bool            `json:"is_freeform"`
	SortOrder  int             `json:"sort_order"`
	Children   []*LocationNode `json:"children"`
}

// BuildCategoryTree 把扁平的分类行组装成树。
//
// 这是纯函数：没有 DB、没有 HTTP，所以它能被 §10 第①层的表驱动单测直接覆盖
// —— 「孤儿节点怎么办」「顺序保不保」这类问题不该等到起了数据库才能验证。
//
// 输入顺序即输出顺序：repo 那边按 (level, sort_order, id) 排好序再传进来，
// 这里只做挂载，不重新排序。
func BuildCategoryTree(rows []Category) []*CategoryNode {
	nodes := make(map[int64]*CategoryNode, len(rows))
	for i := range rows {
		nodes[rows[i].ID] = &CategoryNode{
			ID:        rows[i].ID,
			Name:      rows[i].Name,
			Level:     rows[i].Level,
			SortOrder: rows[i].SortOrder,
			Children:  []*CategoryNode{},
		}
	}

	roots := []*CategoryNode{}
	for i := range rows {
		n := nodes[rows[i].ID]
		// 父节点不在结果集里 = 孤儿。成因只有一种：父行 is_active=false 被查询滤掉了，
		// 而子行还是 active（M6 的删除端点有 CATEGORY_IN_USE 挡着，走不到这一步，
		// 但从 Adminer 里改 is_active 是可以的）。
		// 处理方式是**提升为根**而不是丢弃：丢弃等于让一批仍然可选的小类凭空消失，
		// 用户发帖时选不到它们，而且没有任何报错能提示发生了什么。
		if rows[i].ParentID == nil {
			roots = append(roots, n)
			continue
		}
		if p, ok := nodes[*rows[i].ParentID]; ok {
			p.Children = append(p.Children, n)
			continue
		}
		roots = append(roots, n)
	}
	return roots
}

// BuildLocationTree 同 BuildCategoryTree，多带一个 is_freeform。
func BuildLocationTree(rows []Location) []*LocationNode {
	nodes := make(map[int64]*LocationNode, len(rows))
	for i := range rows {
		nodes[rows[i].ID] = &LocationNode{
			ID:         rows[i].ID,
			Name:       rows[i].Name,
			Level:      rows[i].Level,
			IsFreeform: rows[i].IsFreeform,
			SortOrder:  rows[i].SortOrder,
			Children:   []*LocationNode{},
		}
	}

	roots := []*LocationNode{}
	for i := range rows {
		n := nodes[rows[i].ID]
		if rows[i].ParentID == nil {
			roots = append(roots, n)
			continue
		}
		if p, ok := nodes[*rows[i].ParentID]; ok {
			p.Children = append(p.Children, n)
			continue
		}
		roots = append(roots, n)
	}
	return roots
}

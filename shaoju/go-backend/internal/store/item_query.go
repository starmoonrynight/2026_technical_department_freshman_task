package store

import (
	"fmt"
	"strings"
)

// itemSortColumns 是可排序字段的白名单：接口参数值 -> 真实列名。
//
// 排序字段和排序方向没法写成 ? 占位符（占位符只能表示「值」，不能表示列名或关键字），
// 所以它们必须被拼进 SQL 文本。唯一安全的做法就是先过这张白名单表：
// 只有命中的 key 才会参与拼接，用户传进来的字符串本身永远不会进入 SQL。
var itemSortColumns = map[string]string{
	"id":         "i.id",
	"createdAt":  "i.created_at",
	"updatedAt":  "i.updated_at",
	"happenedAt": "i.happened_at",
	"title":      "i.title",
	"category":   "i.category",
	"status":     "i.status",
	"type":       "i.type",
}

// ItemSortKeys 传给接口层做参数校验（顺序固定，便于生成错误文案）。
var ItemSortKeys = []string{
	"createdAt", "updatedAt", "happenedAt", "title", "category", "status", "type", "id",
}

// ItemSortOrders 排序方向。
var ItemSortOrders = []string{"desc", "asc"}

// DefaultItemSort 缺省排序。
const DefaultItemSort = "createdAt"

// buildItemWhere 把过滤条件编译成 WHERE 子句与对应的参数列表。
//
// 三条原则：
//  1. SQL 文本里的每一段都是代码里的常量；
//  2. 用户输入一律只作为 ? 的参数传入，绝不做字符串拼接；
//  3. 条件按固定顺序追加，参数顺序与占位符顺序严格一致。
//
// 返回的 clause 形如 ` WHERE i.audit_status = 'approved' AND i.type = ?`，
// 没有任何条件时返回空串（调用方直接拼接即可）。
func buildItemWhere(o ListOptions) (string, []any) {
	conditions := make([]string, 0, 6)
	args := make([]any, 0, 10)

	// 前台列表只看审核通过的；后台可以按审核状态筛选
	if o.PublicOnly {
		conditions = append(conditions, "i.audit_status = 'approved'")
	} else if o.AuditStatus != nil {
		conditions = append(conditions, "i.audit_status = ?")
		args = append(args, *o.AuditStatus)
	}

	if o.Type != nil {
		conditions = append(conditions, "i.type = ?")
		args = append(args, *o.Type)
	}
	if o.Category != nil {
		conditions = append(conditions, "i.category = ?")
		args = append(args, *o.Category)
	}
	if o.Status != nil {
		conditions = append(conditions, "i.status = ?")
		args = append(args, *o.Status)
	}
	if o.UserID != nil {
		conditions = append(conditions, "i.user_id = ?")
		args = append(args, *o.UserID)
	}

	// 关键词搜索：四个字段做 LIKE 模糊匹配，任意一个命中即可。
	// 通配符 % 由后端拼在参数值两端，所以用户输入里的 % 和 _ 也会被当成普通字符之外的
	// 通配符（这是 LIKE 的语义）；重点是整个值仍然是参数，不会改变 SQL 结构。
	if o.Keyword != nil {
		conditions = append(conditions,
			"(i.title LIKE ? OR i.description LIKE ? OR i.location LIKE ? OR i.category LIKE ?)")
		like := "%" + *o.Keyword + "%"
		args = append(args, like, like, like, like)
	}

	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}

// buildItemOrderBy 把排序参数编译成 ORDER BY 子句。
//
//	o.Sort   排序字段，必须命中 itemSortColumns，否则退回 createdAt
//	o.Order  asc / desc，否则退回 desc
//
// 末尾总会再追加一个 i.id 作为「第二排序键」：
// 同一时刻创建的两条记录如果顺序不确定，LIMIT/OFFSET 翻页时就会出现
// 同一条记录在第 1 页和第 2 页各出现一次、另一条却完全看不到的情况。
func buildItemOrderBy(o ListOptions) string {
	column, ok := itemSortColumns[o.Sort]
	if !ok {
		column = itemSortColumns[DefaultItemSort]
	}

	direction := "DESC"
	if strings.EqualFold(o.Order, "asc") {
		direction = "ASC"
	}

	return fmt.Sprintf(" ORDER BY %s %s, i.id %s", column, direction, direction)
}

// normalizePaging 修正分页参数，保证 LIMIT / OFFSET 不会是负数。
func normalizePaging(page, pageSize int) (int, int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 10
	}
	return page, pageSize, (page - 1) * pageSize
}

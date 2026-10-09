// Package models 定义返回给前端的结构。
//
// 用户相关结构同时输出两套字段名：
//   - studentId / name 是正式字段（学号、姓名）
//   - username / nickname 是兼容字段，取值分别等于上面两个
//
// 保留兼容字段是为了让 Node 版后端与现有前端页面在切换期间继续工作，
// 前端改用学号之后可以整体删除。
package models

// User 用户公开信息（不含口令散列）。
type User struct {
	ID        int64  `json:"id"`
	StudentID string `json:"studentId"` // 学号，登录凭据
	Name      string `json:"name"`      // 姓名
	Contact   string `json:"contact"`   // 联系方式
	Role      string `json:"role"`      // user | admin
	Status    string `json:"status"`    // active | disabled
	CreatedAt string `json:"createdAt"`
	// ItemCount 只在后台用户列表里出现，其它接口不返回该字段。
	ItemCount *int64 `json:"itemCount,omitempty"`

	Username string `json:"username"` // 兼容字段 = StudentID
	Nickname string `json:"nickname"` // 兼容字段 = Name
}

// Owner 条目所属用户的精简信息。
type Owner struct {
	ID        int64  `json:"id"`
	StudentID string `json:"studentId"`
	Name      string `json:"name"`

	Username string `json:"username"` // 兼容字段 = StudentID
	Nickname string `json:"nickname"` // 兼容字段 = Name
}

// Item 失物 / 招领信息。
type Item struct {
	ID          int64  `json:"id"`
	Type        string `json:"type"` // lost = 寻物启事，found = 失物招领
	Title       string `json:"title"`
	Category    string `json:"category"`
	Description string `json:"description"`
	Location    string `json:"location"`
	// StoragePlace 寄放处：捡到物品后存放在哪里，选填，主要用于「失物招领」。
	StoragePlace string `json:"storagePlace"`
	HappenedAt   string `json:"happenedAt"`
	Contact      string `json:"contact"`
	ImageURL     string `json:"imageUrl"`
	Status       string `json:"status"` // open = 进行中，closed = 已解决
	AuditStatus  string `json:"auditStatus"`
	AuditRemark  string `json:"auditRemark"`
	UserID       int64  `json:"userId"` // 发布者，对应 users.id
	Owner        Owner  `json:"owner"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
}

// ItemDetail 详情页在条目上追加 canEdit 标记。
type ItemDetail struct {
	Item
	CanEdit bool `json:"canEdit"`
}

// Page 分页结果。
type Page struct {
	Items      []Item `json:"items"`
	Page       int    `json:"page"`
	PageSize   int    `json:"pageSize"`
	Total      int64  `json:"total"`
	TotalPages int    `json:"totalPages"`
}

// CategoryCount 分类统计项。
type CategoryCount struct {
	Category string `json:"category"`
	Count    int64  `json:"count"`
}

// StatusCount 进度状态统计项。
type StatusCount struct {
	Status string `json:"status"` // open / found / closed
	Count  int64  `json:"count"`
}

// Stats 后台数据概览。
//
// 注意 Lost / Found 统计的是「信息类型」（寻物启事 / 失物招领），
// 进度状态的数量看 ByStatus，两者含义不同不要混淆。
type Stats struct {
	Total        int64           `json:"total"`
	Lost         int64           `json:"lost"`
	Found        int64           `json:"found"`
	Open         int64           `json:"open"`
	Closed       int64           `json:"closed"`
	Pending      int64           `json:"pending"`
	ResolvedRate int             `json:"resolvedRate"`
	ByCategory   []CategoryCount `json:"byCategory"`
	ByStatus     []StatusCount   `json:"byStatus"`
}

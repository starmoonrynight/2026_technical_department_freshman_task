package model

import "time"

// ContactView 对应 contact_views 表的一行，外加 #22 要显示的两个用户字段。
//
// ⚠ 这张表**没有任何状态列**（见 000001 迁移第 6 节）。它只表达一件事：
// 「某个用户在某个时刻点开过一次这条 found 帖的联系方式」。
// 它不是锁、不是认领、不是审批 —— 定位原则 3 的原话是
// 「不需要设一个人认领其他人不允许查看的规则」。
// 所以这里不能出现 `granted` / `approved` / `claimed_at` 之类的字段，
// 一旦加了，非排他这个设计就被从数据层悄悄推翻了。
//
// Nickname / RealName 是 repo 连 users 表带出来的，只为 #22 服务：
// 发帖人要看的是一份「谁来找过」的名单，只有 id 的话这份名单没法读。
// 这是全项目**唯一**把 real_name 放进对外响应的地方（AuthorView 刻意没有它），
// 因为这是计划里那种「你确实需要知道对方是谁」的场合，且读取者限定为发帖人或 admin。
type ContactView struct {
	ID        int64
	ItemID    int64
	UserID    int64
	Nickname  string
	RealName  string
	CreatedAt time.Time
}

// UnlockerView 是 #22 名单里「一个人」的形状。
type UnlockerView struct {
	ID       int64  `json:"id"`
	Nickname string `json:"nickname"`
	RealName string `json:"real_name"`
}

// ContactViewEntry 是 #22 GET /api/items/:id/contact-views 的 list 元素。
//
// 外层那个 id 是 contact_views 自己的行 id，不是用户 id —— 计划 §4 第 22 行
// 写的形状就是 `{id, user:{...}, created_at}`。留着它不是为了跳转，是为了让这份名单
// 在治理场景里可引用（「第 37 行那次解锁」比「第三个人」精确）。
type ContactViewEntry struct {
	ID        int64        `json:"id"`
	User      UnlockerView `json:"user"`
	CreatedAt string       `json:"created_at"`
}

// View 把一行 contact_views 转成 #22 的对外形状。
func (c *ContactView) View() ContactViewEntry {
	return ContactViewEntry{
		ID:        c.ID,
		User:      UnlockerView{ID: c.UserID, Nickname: c.Nickname, RealName: c.RealName},
		CreatedAt: formatTimeValue(c.CreatedAt),
	}
}

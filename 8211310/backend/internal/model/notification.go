package model

import "time"

// 通知的七种 type。**这七个字面量不是我们自己定的**，它是 000001 迁移里
// notifications_type_check 那条 CHECK 约束的内容，写错一个字母会得到 23514
// 而不是一个能看出问题的报错。所以这里一次列全七个，当作那条约束的镜像。
//
// M3 只有 NotificationNewMatch 有写入方（found 帖创建时给 lost 作者发一条，§3.7）。
// 其余六个在 M5（归还状态机）和 M6（治理动作回执）里才有触发点。
//
// ⚠ M4 **不新增任何写入方**，它只负责把已存在的通知读出来（#30–#32）。
// 而且 M4 的两条硬规则恰恰是「不写通知」：解锁联系方式不发通知
// （`contact_unlocked` 这个 type 已经在第 4 版被删掉，见 §16），
// 举报永远不通知被举报人（§3.7）。这两条各有冒烟测试守着。
//
// ⚠ 方向是这一整张表最容易搞错的地方，所以逐条写清「发给谁」：
//   - new_match：**仅那条 lost 帖的作者**。found 帖作者永远收不到匹配通知
//     （§2.3、§5.8 的不对称：他是被动方，发完帖义务就完成了）
//   - item_returned_hint：反查台账后告诉 lost 方「你联系过的那条帖子已被确认归还」
//   - admin_action / report_resolved：治理动作的**回执**，不发通知就等于没治理
const (
	NotificationNewMatch         = "new_match"
	NotificationReturnSubmitted  = "return_submitted"
	NotificationReturnConfirmed  = "return_confirmed"
	NotificationReturnRejected   = "return_rejected"
	NotificationItemReturnedHint = "item_returned_hint"
	NotificationAdminAction      = "admin_action"
	NotificationReportResolved   = "report_resolved"
)

// Notification 对应 notifications 表的一行。
//
// ItemID / ReturnID 是**跳转用的**，不是外键（迁移里这两列没有 REFERENCES）：
// 通知说的是「某件事发生了」这个事实，而那条帖子可能已经被删了 ——
// 加了外键就得决定删帖时要不要连带删通知，那是个没有好答案的问题。
// 现在的语义是：id 留着，前端点进去如果 404 就显示「该内容已不存在」。
type Notification struct {
	ID        int64
	UserID    int64
	Type      string
	Title     string
	Content   string
	ItemID    *int64
	ReturnID  *int64
	IsRead    bool
	CreatedAt time.Time
}

// NotificationView 是 #30 GET /api/my/notifications 的 list 元素。
//
// ItemID / ReturnID 是指针而不是 `json:",omitempty"`，理由和 M3 的
// notified_count 是同一条家族但方向相反：这里要保住的是「**没有**关联帖子时
// 这个键仍然出现并且是 null」。用 omitempty 的话键会整个消失，
// 前端就分不清「这条通知本来不挂帖子」（admin_action 批量下架那种）
// 和「后端忘了返回这个字段」。空串更不行 —— 那是一个合法的 id 位置。
//
// IsRead 是值类型 bool：它永远有值，false 也是要出现的。
type NotificationView struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	ItemID    *int64 `json:"item_id"`
	ReturnID  *int64 `json:"return_id"`
	IsRead    bool   `json:"is_read"`
	CreatedAt string `json:"created_at"`
}

// View 把一行 notifications 转成 #30 的对外形状。user_id 刻意不在里面：
// 这个端点只会返回当前用户自己的通知，收件人是「你」，
// 把 user_id 暴露出去只是多给前端一个可以拿去猜别人 id 的字段。
func (n *Notification) View() NotificationView {
	return NotificationView{
		ID:        n.ID,
		Type:      n.Type,
		Title:     n.Title,
		Content:   n.Content,
		ItemID:    n.ItemID,
		ReturnID:  n.ReturnID,
		IsRead:    n.IsRead,
		CreatedAt: formatTimeValue(n.CreatedAt),
	}
}

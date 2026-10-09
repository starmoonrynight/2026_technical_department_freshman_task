package model

import "time"

// 归还确认的四个状态。字面量是 000001 迁移第 7 节 item_returns_status_check 的镜像，
// 写错一个字母得到的是 23514 而不是一个能看出问题的报错。
//
// ⚠ 只有 pending 有出边（见 service/item_return.go 的 legalTransitions）：
// confirmed / rejected / cancelled 都是**终态**，任何回退和终态互转都非法。
const (
	ReturnStatusPending   = "pending"
	ReturnStatusConfirmed = "confirmed"
	ReturnStatusRejected  = "rejected"
	ReturnStatusCancelled = "cancelled"
)

// ReturnStatuses 是 #28/#29 的 `?status=` 白名单（service 用它挡非法值，
// repo 只允许把 map 查出来的表达式拼进 SQL —— 两张白名单同 sort 那条纪律）。
var ReturnStatuses = []string{
	ReturnStatusPending,
	ReturnStatusConfirmed,
	ReturnStatusRejected,
	ReturnStatusCancelled,
}

// review_kind 的两个值。这一列是定位原则 5 的落地处：
//
//   - owner        = 发帖人本人通过 API 点的，是一次**有社区含义的确认**（关帖、加分、发通知）
//   - admin_data_fix = 管理员绕过 service 层直接改的数据修正，**没有社区含义**，不加分不通知
//
// 列是可空的，而 service 层是唯一会写 'owner' 的地方（因为只有发帖人过得了身份校验）。
// 所以「管理员改的行」不需要他主动承认就会露馅：
//
//	SELECT id FROM item_returns WHERE status IN ('confirmed','rejected') AND review_kind IS NULL
//
// 任何一行命中，都证明这条归还确认是在 service 层之外被推进的。
// M5 的测试专门断言 cancel 之后这条查询仍然为空（cancelled 是提交人自己撤的，
// 不在那条查询的 status 列表里，所以它天然不会被误判成数据修正）。
const (
	ReviewKindOwner        = "owner"
	ReviewKindAdminDataFix = "admin_data_fix"
)

// 积分规则：**只有两条**，且都由 confirm 触发（§3.6）。
//
// rejected / cancelled 一律不扣分 —— 判断错了不罚人，罚了就等于逼发帖人不敢点拒绝，
// 而「发帖人自由地拒绝假确认」正是这套设计的承重墙。
//
// ⚠ 这两个数字同时出现在两处：这里的常量，和 credit_logs 的 delta 列。
// credit_score 纯展示、不参与任何排序（参与排序就有刷分动机），
// 所以改这两个数不需要迁移、不需要回归任何列表顺序。
const (
	CreditReasonReturnOwner     = "return_confirmed_as_owner"
	CreditReasonReturnSubmitter = "return_confirmed_as_submitter"

	CreditDeltaReturnOwner     = 10
	CreditDeltaReturnSubmitter = 2

	// credit_logs.ref_type 的取值：这一行流水是被哪张表的事触发的。
	// VARCHAR(16) 装得下，且和 notifications 那边一样只用作前端跳转。
	CreditRefTypeReturn = "item_return"

	// creditScoreMin/Max 是 users.credit_score 的 CHECK 边界（0..200）。
	// repo 用 LEAST/GREATEST 夹住，这里留着是为了让「夹到顶」这个分支能被测试引用。
	creditScoreFloor = 0
	creditScoreCeil  = 200
)

// CreditScoreBounds 返回信用分的上下界。repo 拼 SQL 时用它而不是抄数字，
// 这样 CHECK 边界只有一份定义。
func CreditScoreBounds() (int, int) { return creditScoreFloor, creditScoreCeil }

// 归还确认的文本长度上限，和迁移对齐。
//
// message 那条 CHECK 是 `char_length(message) BETWEEN 5 AND 1000`，
// 而 PostgreSQL 的 char_length 数的是**字符**不是字节 —— 所以 service 侧
// 必须用 utf8.RuneCountInString，用 len() 的话中文用户写满 333 个字就被拒，
// 报错还说「太长了」（和举报 detail 同一条教训，见 service/report.go）。
const (
	ReturnMessageMinChars = 5
	ReturnMessageMaxChars = 1000

	// owner_note 是 VARCHAR(500) NOT NULL DEFAULT ''，没有下限 CHECK：
	// 下限是业务规则而不是数据库规则 —— confirm 可以不写备注（默认空串），
	// reject **必须**写（那是提交人唯一能得到的解释）。
	ReturnOwnerNoteMaxChars = 500
)

// ItemReturn 对应 item_returns 表的一行。
//
// ReviewerID / ReviewKind / ReviewedAt 都是指针：它们在「还没人审」和
// 「提交人自己撤的」两种情况下真的是 NULL，而 NULL 和「空串 / 0」在对外形状上
// 必须是能区分的（同 NotificationView.ItemID 那条理由：键要出现，值要诚实）。
type ItemReturn struct {
	ID             int64
	ItemID         int64
	SubmitterID    int64
	Message        string
	ProofImagePath string
	Status         string
	OwnerNote      string
	ReviewerID     *int64
	ReviewKind     *string
	SubmittedAt    time.Time
	ReviewedAt     *time.Time
}

// SubmitterView 是 #24 里「提交人」的形状。
//
// ⚠ 这里是全项目第二个带 credit_score 的对外形状（第一个是 UserView）。
// 带它的理由写在计划 §2.5：发帖人判断真伪的唯一依据就是
// 「文字 + 凭证图 + 提交人昵称和信用分」，缺了这一格 #24 就没法独立完成判断。
// 但**没有** real_name —— 那是 #22 解锁名单专属的字段（那里是「你确实需要知道对方是谁」，
// 而归还确认页面上发帖人和提交人本来就没见过面，公示真实姓名是另一种泄漏）。
type SubmitterView struct {
	ID          int64  `json:"id"`
	Nickname    string `json:"nickname"`
	CreditScore int    `json:"credit_score"`
}

// ReturnDetailView 是 #24 GET /api/returns/:id 的 data（计划 §4 第 24 行）。
type ReturnDetailView struct {
	ID            int64         `json:"id"`
	Item          ItemSummary   `json:"item"`
	Submitter     SubmitterView `json:"submitter"`
	Message       string        `json:"message"`
	ProofImageURL string        `json:"proof_image_url"`
	Status        string        `json:"status"`
	OwnerNote     string        `json:"owner_note"`
	ReviewerID    *int64        `json:"reviewer_id"`
	ReviewKind    *string       `json:"review_kind"`
	SubmittedAt   string        `json:"submitted_at"`
	ReviewedAt    string        `json:"reviewed_at"`
}

// View 把一行归还确认转成 #24 的形状。
//
// item 和 submitter 都不在这一行里，由 service 拼（它才有 posts/users 的读取能力）：
// model 只负责把已经查好的值放进正确的字段，这一层没有 if。
func (r *ItemReturn) View(item ItemSummary, submitter SubmitterView, proofURL string) ReturnDetailView {
	return ReturnDetailView{
		ID:            r.ID,
		Item:          item,
		Submitter:     submitter,
		Message:       r.Message,
		ProofImageURL: proofURL,
		Status:        r.Status,
		OwnerNote:     r.OwnerNote,
		ReviewerID:    r.ReviewerID,
		ReviewKind:    r.ReviewKind,
		SubmittedAt:   formatTimeValue(r.SubmittedAt),
		ReviewedAt:    formatTime(r.ReviewedAt),
	}
}

// ReturnEntry 是 #28/#29 列表的 list 元素。
//
// 和 #24 的区别是刻意的：没有 message、没有凭证图、没有 submitter。
// 列表是索引，详情才是判断现场 —— 发帖人在列表里看到「有条新的归还确认」，
// 点进去才需要读那 200 字和那张图。少传这些字段不只是省带宽，
// 它让「列表页不该有确认/拒绝按钮」这件事在前端没有别的选择。
type ReturnEntry struct {
	ID          int64       `json:"id"`
	Item        ItemSummary `json:"item"`
	Status      string      `json:"status"`
	OwnerNote   string      `json:"owner_note"`
	SubmittedAt string      `json:"submitted_at"`
	ReviewedAt  string      `json:"reviewed_at"`
}

// ItemReturnRow 是 #28/#29 一次连表查出来的行：归还记录 + 帖子摘要所需的字段。
//
// 为什么不复用 ItemDetail（repo 那边已经有一套 itemDetailCols/scanItemDetailRows）：
// ItemDetail 有 24 列，其中四个祖先 id 和一个 is_freeform 是**只为匹配算法存在**的，
// 还有 description / view_count / updated_at 是列表根本不返回的。
// 借用它就得为这 15 个字段各写一次 NULL 或零值填充，而「填了的」和「没填的」
// 混在一个结构体里，下一个读代码的人分不清哪个能信 —— 这是 §3 里
// 「半填的形状」那个反复出现的坑（和 #42 删完图留一行指向不存在文件的记录同一种）。
// 显式列 15 个字段长一点，但每一列都有值、每一列都被用。
type ItemReturnRow struct {
	ID          int64
	Status      string
	OwnerNote   string
	SubmittedAt time.Time
	ReviewedAt  *time.Time

	ItemID        int64
	ItemType      string
	Title         string
	ItemStatus    string
	CategoryID    int64
	CategoryName  string
	LocationID    int64
	LocationName  string
	LostAt        *time.Time
	FoundAt       *time.Time
	Contact       string
	CoverPath     string
	AuthorID      int64
	AuthorName    string
	ItemCreatedAt time.Time
}

// Summary 把这一行里「帖子」那一半转成对外摘要。contact 由调用方决定给不给
// （规则和其他摘要端点完全一致：found 帖一律 nil，见 service/item_return.go 的 toEntry）。
func (r *ItemReturnRow) Summary(contact *string, coverURL string) ItemSummary {
	return ItemSummary{
		ID:           r.ItemID,
		ItemType:     r.ItemType,
		Title:        r.Title,
		Status:       r.ItemStatus,
		CategoryID:   r.CategoryID,
		CategoryName: r.CategoryName,
		LocationID:   r.LocationID,
		LocationName: r.LocationName,
		LostAt:       formatTime(r.LostAt),
		FoundAt:      formatTime(r.FoundAt),
		Contact:      contact,
		CoverImage:   coverURL,
		AuthorID:     r.AuthorID,
		AuthorName:   r.AuthorName,
		CreatedAt:    formatTimeValue(r.ItemCreatedAt),
	}
}

// Entry 把整行转成 #28/#29 的列表元素。
func (r *ItemReturnRow) Entry(contact *string, coverURL string) ReturnEntry {
	return ReturnEntry{
		ID:          r.ID,
		Item:        r.Summary(contact, coverURL),
		Status:      r.Status,
		OwnerNote:   r.OwnerNote,
		SubmittedAt: formatTimeValue(r.SubmittedAt),
		ReviewedAt:  formatTime(r.ReviewedAt),
	}
}

// CreditLog 对应 credit_logs 表的一行。
//
// 这张表存在的全部理由是「让分数可解释」：credit_score 是一个被夹过的累计值，
// 单看它永远看不出为什么。有这一行流水，用户问「我为什么是 110」就有答案。
//
// ⚠ Delta 记的是**实际生效**的增量，不是规则名义值。
// 分数已经 200 的人再确认一次归还，这里记的是 0 而不是 10 ——
// 因为「流水之和 + 100 == credit_score」是这张表唯一可 debug 的性质，
// 记名义值会让它对不上账。夹到顶这件事本身仍然是一次真实发生过的确认，
// 所以这一行**照插**（0 也在），删掉它的话用户会看到「我确认了但没有流水」。
type CreditLog struct {
	ID        int64
	UserID    int64
	Delta     int
	Reason    string
	RefType   *string
	RefID     *int64
	CreatedAt time.Time
}

// CreditLogView 是 #33 GET /api/my/credit-logs 的 list 元素。
//
// RefType / RefID 是指针：注册送的那 0 条流水之外，将来若有不挂具体对象的加分
// （比如 admin 手工补分），这两列就是 NULL。键必须出现、值必须诚实（同上面几条理由）。
type CreditLogView struct {
	ID        int64   `json:"id"`
	Delta     int     `json:"delta"`
	Reason    string  `json:"reason"`
	RefType   *string `json:"ref_type"`
	RefID     *int64  `json:"ref_id"`
	CreatedAt string  `json:"created_at"`
}

// View 把一行流水转成 #33 的形状。user_id 刻意不在里面：
// 这个端点只会返回当前用户自己的流水，收件人是「你」
// （和 NotificationView 排除 user_id 同一条理由：不给前端一个可以拿去猜别人 id 的字段）。
func (c *CreditLog) View() CreditLogView {
	return CreditLogView{
		ID:        c.ID,
		Delta:     c.Delta,
		Reason:    c.Reason,
		RefType:   c.RefType,
		RefID:     c.RefID,
		CreatedAt: formatTimeValue(c.CreatedAt),
	}
}

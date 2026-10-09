package model

import "time"

// items 表的枚举值。和 user.go 里那组常量同一个理由：写成常量，
// 拼错的 'colsed' 会在编译期就红，而不是在运行时表现成「帖子关不掉」。
// 数据库侧还有 CHECK 兜底（000001 迁移里 item_type / status 各一条）。
const (
	ItemTypeLost  = "lost"
	ItemTypeFound = "found"

	ItemStatusOpen    = "open"
	ItemStatusClosed  = "closed"
	ItemStatusDeleted = "deleted"
)

// Item 对应 items 表的一行。
//
// lost 和 found 共用这一张表，靠 ItemType 区分（计划 §3.2）。
// 三个时间列都可空，「哪种类型填哪列」由数据库的 items_time_semantics CHECK 钉死，
// 所以这里用 *time.Time 而不是零值 time.Time —— 零值和 NULL 在语义上完全不同：
// 「1 年 1 月 1 日丢的」和「这一列不该有值」必须能区分开。
type Item struct {
	ID             int64
	ItemType       string
	UserID         int64
	Title          string
	Description    string
	CategoryID     int64
	LocationID     int64
	LocationDetail string
	LastSeenAt     *time.Time // 仅 lost：最后一次确认还拥有它的时间
	LostAt         *time.Time // 仅 lost：发现它不见了的时间
	FoundAt        *time.Time // 仅 found：实际拾获时间（不是上传时间）
	Contact        string
	Status         string
	// ViewCount 建了但这一轮没有任何代码读写它。
	// 刻意**不**出现在下面的 View/Summary 里：返回一个恒为 0 的字段，
	// 只会让前端以为「以后会有」而写出等它的代码（和 UserView 排除 student_id 同一条理由）。
	// 将来真要做浏览量，加一次 UPDATE 再把它加进 View 就行，不影响现在的任何契约。
	ViewCount int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ItemImage 对应 item_images 表的一行。
//
// Path 是相对于 UPLOAD_DIR 的路径（形如 2026/10/ab12....jpg），**数据库里不存图片二进制**。
// 对外一律给 URL（UploadBaseURL + "/" + Path），前端永远看不到磁盘路径 ——
// 磁盘布局是实现细节，暴露出去之后想改目录结构就变成了破坏性变更。
type ItemImage struct {
	ID        int64
	ItemID    int64
	Path      string
	SortOrder int
	CreatedAt time.Time
}

// ItemDetail 是 repo 连表查出来的一行：帖子本体 + 展示所需的字典名与作者名。
//
// 为什么要连表：广场列表和详情页都要显示「衣物箱包 / 钱包」「图书馆」「小王」，
// 而 items 表里只有三个 id。不连表就得每行再发三次查询（N+1），
// 20 行的列表会变成 61 次往返 —— 慢是次要的，主要是日志里会刷出 61 条 SQL，
// 把真正想看的那条埋掉。
type ItemDetail struct {
	Item
	CategoryName   string
	LocationName   string
	AuthorNickname string
	// CoverPath 是排序最前的那张图的相对路径，没有图时为空串。
	// 由 SQL 里的 LEFT JOIN LATERAL 一次取回（见 repo/item.go）。
	CoverPath string

	// ---------- 下面四个字段只为匹配算法服务（§5.2、§5.5），一律不出现在任何对外 JSON 里 ----------
	//
	// 打分要比较祖先，而 items 表里只有叶子 id：
	//   - 分类「同大类不同小类给 0.5」要比 CategoryParentID
	//   - 地点「同二级子类 0.6 / 同一级区 0.3」要比 LocationParentID / LocationTopID
	// 所以连表的时候顺路把 l 的父和祖父一起 JOIN 出来（见 repo/item.go 的 lp / lt），
	// 否则每个候选都要再发两次字典点查，N 个候选就是 2N 次往返。
	//
	// 三个 id 都用 0 表示「没有这一级」（SQL 里 COALESCE 过），而不是 -1 或指针：
	// 0 不可能是任何一行的 id，比较时一句 != 0 就能挡住「两个空祖先被判成同一个」。
	// 典型例子就是地点树里的「其他」—— 它是 level=1 的叶子，本来就没有父节点。
	CategoryParentID int64
	LocationParentID int64
	LocationTopID    int64
	// LocationIsFreeform 来自 l.is_freeform：true 就是选了「其他」，
	// 没有叶子节点可比，这条帖子直接跳过 Tier 1 进 Tier 2（§5.5、§3.6）。
	LocationIsFreeform bool
}

// RefView 是「一个字典条目的最小对外形状」。
//
// 详情响应用嵌套对象（category: {id, name}）而不是平铺两个字段
// （category_id + category_name），是因为前端要把「分类」当成一个东西传给
// 级联选择器和筛选器，嵌套形状能直接塞进组件的 value，不用再拼一次。
type RefView struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// AuthorView 是帖子作者的对外形状。
//
// ⚠ 只有 id 和 nickname，**没有 real_name、没有 credit_score**。
// #15 是公开接口，未登录也能访问，而 real_name 是 SSO 带来的真实姓名 ——
// 把它挂在一个人人可读的页面上，等于替全体用户做了一次实名公示。
// 计划里 real_name 只出现在 #22（解锁名单，仅发帖人和 admin 可见）那种
// 「你确实需要知道对方是谁」的场合，这里不属于那种场合。
type AuthorView struct {
	ID       int64  `json:"id"`
	Nickname string `json:"nickname"`
}

// ImageView 是一张图片的对外形状。id 必须在：#42 帖主自删单张图片走的就是它。
type ImageView struct {
	ID        int64  `json:"id"`
	URL       string `json:"url"`
	SortOrder int    `json:"sort_order"`
}

// ItemView 是 #15 GET /api/items/:id 的 data 形状，也是 #13/#16 响应里 item 字段的形状。
//
// Contact 是**指针**，这是整个 M2 最关键的一处类型选择：
// 计划 §4「#15 的 contact 可见性规则」要求锁着的时候返回 `contact: null`，
// 而 found 帖的联系方式在解锁前必须一个字都不出现在响应体里。
// 用空串表示「锁着」是错的 —— 前端分不清「锁着」和「这人填了个空串」，
// 而 §3.2 明确禁止空串入库，所以空串在业务上根本不该存在。
// 用 *string 之后，nil 就是 null，语义只有一种解释。
//
// ContactLocked 始终存在（不管锁没锁），前端只靠它一个字段决定那块 UI 长什么样。
type ItemView struct {
	ID             int64       `json:"id"`
	ItemType       string      `json:"item_type"`
	Title          string      `json:"title"`
	Description    string      `json:"description"`
	Status         string      `json:"status"`
	Category       RefView     `json:"category"`
	Location       RefView     `json:"location"`
	LocationDetail string      `json:"location_detail"`
	LastSeenAt     string      `json:"last_seen_at"`
	LostAt         string      `json:"lost_at"`
	FoundAt        string      `json:"found_at"`
	Contact        *string     `json:"contact"`
	ContactLocked  bool        `json:"contact_locked"`
	Images         []ImageView `json:"images"`
	Author         AuthorView  `json:"author"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at"`
}

// ItemSummary 是列表类端点（#14 广场、#19 我的发布，以及 M3 之后的 #20 匹配结果、
// M5 之后的 #24/#28/#29）共用的「帖子摘要」形状。
//
// 和 ItemView 的区别：没有 description（列表里显示不下，点进去才看）、
// 没有 images 数组（只有 cover_image 一张，理由同上）、没有 updated_at。
// 少传这些字段对 20 行的列表来说是实打实的带宽差别，更重要的是
// 「前端在列表页拿不到 description」这件事本身就是一种约束 ——
// 它逼着详情页和列表页各司其职，而不是把详情逻辑悄悄搬到列表里。
//
// Contact 的可空性和 ItemView 一样，但填充规则不同（计划 §4）：
//   - #14 广场：lost 帖带 contact，found 帖一律 null（**不看是谁在查**，
//     自己的 found 帖在广场上也是锁着的 —— 这样 #14 完全不需要身份，
//     公开、可缓存、也不用为「是不是自己」多发一次查询）
//   - #19 我的发布：一律带 contact（都是自己的）
type ItemSummary struct {
	ID           int64   `json:"id"`
	ItemType     string  `json:"item_type"`
	Title        string  `json:"title"`
	Status       string  `json:"status"`
	CategoryID   int64   `json:"category_id"`
	CategoryName string  `json:"category_name"`
	LocationID   int64   `json:"location_id"`
	LocationName string  `json:"location_name"`
	LostAt       string  `json:"lost_at"`
	FoundAt      string  `json:"found_at"`
	Contact      *string `json:"contact"`
	CoverImage   string  `json:"cover_image"`
	AuthorID     int64   `json:"author_id"`
	AuthorName   string  `json:"author_name"`
	CreatedAt    string  `json:"created_at"`
}

// View 把一行 ItemDetail 转成详情形状。
//
// contact 的可见性判断**不在这里**，在 service（那是要读 contact_views 表的业务规则）。
// model 只负责把已经决定好的值放进正确的字段 —— 这一层没有任何 if，
// 所以它不需要测试「规则对不对」，只需要保证「形状对不对」。
func (d *ItemDetail) View(contact *string, locked bool, images []ImageView) ItemView {
	if images == nil {
		images = []ImageView{}
	}
	return ItemView{
		ID:       d.ID,
		ItemType: d.ItemType,
		Title:    d.Title,
		// description 原样返回，**不在展示时二次加工**。
		// 用户写描述时的换行和段首缩进是有意义的排版，替他「清理」掉等于改了用户的内容。
		// 首尾空白在**入库前**就已经 trim 过了（service.normalizeItemFields），
		// 那是校验规则；这里不再动它，是因为「存进去什么就返回什么」这条规则
		// 一旦破例，用户就会看到「我明明写了三个换行，页面只剩两个」。
		Description:    d.Description,
		Status:         d.Status,
		Category:       RefView{ID: d.CategoryID, Name: d.CategoryName},
		Location:       RefView{ID: d.LocationID, Name: d.LocationName},
		LocationDetail: d.LocationDetail,
		LastSeenAt:     formatTime(d.LastSeenAt),
		LostAt:         formatTime(d.LostAt),
		FoundAt:        formatTime(d.FoundAt),
		Contact:        contact,
		ContactLocked:  locked,
		Images:         images,
		Author:         AuthorView{ID: d.UserID, Nickname: d.AuthorNickname},
		CreatedAt:      formatTimeValue(d.CreatedAt),
		UpdatedAt:      formatTimeValue(d.UpdatedAt),
	}
}

// Summary 把一行 ItemDetail 转成摘要形状。coverURL 由调用方拼好（它知道 UploadBaseURL），
// 没有封面图时传空串。
func (d *ItemDetail) Summary(contact *string, coverURL string) ItemSummary {
	return ItemSummary{
		ID:           d.ID,
		ItemType:     d.ItemType,
		Title:        d.Title,
		Status:       d.Status,
		CategoryID:   d.CategoryID,
		CategoryName: d.CategoryName,
		LocationID:   d.LocationID,
		LocationName: d.LocationName,
		LostAt:       formatTime(d.LostAt),
		FoundAt:      formatTime(d.FoundAt),
		Contact:      contact,
		CoverImage:   coverURL,
		AuthorID:     d.UserID,
		AuthorName:   d.AuthorNickname,
		CreatedAt:    formatTimeValue(d.CreatedAt),
	}
}

// formatTime 把可空时间列格式化成 RFC3339 字符串，NULL 折成空串。
//
// 空串而不是 JSON null：前端这些字段全部是「有就显示、没有就不显示」，
// 而 lost 帖的 found_at 恒为空、found 帖的 lost_at 恒为空 ——
// 让类型统一成 string，前端就不用为四个时间字段各写一遍判空（和 UserView 同一条理由）。
func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return formatTimeValue(*t)
}

// formatTimeValue 先转 UTC 再格式化。
//
// 不转的话，序列化结果里会带上数据库会话的时区偏移（本机是 +08:00），
// 前端拿到之后还得猜这个偏移是服务器的还是自己的。§3.1 定了「一律存 UTC」，
// 出口这里也统一成 UTC，两端就都不用猜了。
func formatTimeValue(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

package service

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
	"lostfound/internal/repo"
)

// 本文件是 items 的**纯校验规则**：没有 DB、没有 HTTP、没有指针接收者，
// 全部是包级函数，所以它们能被 §10 第①层的表驱动单测直接覆盖。
//
// 把校验从 service 方法里拆出来的动机很具体：M2 的验收判据里有一大半是
// 「某种输入必须得 VALIDATION」，如果这些规则写在 Create() 方法里，
// 每验一条就得连数据库建一个用户、造一个分类、起一次事务 —— 慢、脆、
// 而且失败信息里混着一堆和「校验对不对」无关的东西。

// 字段长度上限，和 000001 迁移里的 VARCHAR 一一对应。
//
// ⚠ 单位是**字符数**不是字节数：PG 的 VARCHAR(n) 和 char_length() 数的都是字符，
// 一个汉字算 1。所以这里必须走 utf8.RuneCountInString（validateLength 里已经是了），
// 用 len() 的话「标题最长 100」会变成「最长 33 个汉字」，和数据库的实际行为不一致。
const (
	titleMaxChars          = 100 // items.title VARCHAR(100)
	descriptionMaxChars    = 5000
	locationDetailMaxChars = 200 // items.location_detail VARCHAR(200)
	contactMaxChars        = 100 // items.contact VARCHAR(100)

	// maxItemImages 是一个帖子最多几张图。
	//
	// 数据库没有这个限制，所以它是**我们自己加的**，理由有两条：
	//  1. 不限的话一个请求就能插十万行 item_images，而 #14 的 LEFT JOIN LATERAL
	//     会对每一行帖子都去排一次序。
	//  2. 9 张正好是前端 3×3 的缩略图格子，再多也显示不下，用户不会真的传。
	//
	// 计划没有规定这个数字，所以它写在常量里而不是藏在代码中间 —— 要改就改这一处。
	maxItemImages = 9
)

// 分页参数的默认值与上限。
const (
	defaultPage     = 1
	defaultPageSize = 20 // 计划 §4：「分页统一 ?page=1&page_size=20」
	maxPageSize     = 100
)

// timeLayout 是接口接受的时间格式。
//
// 用 RFC3339（形如 2026-10-07T15:04:05+08:00）而不是自定义格式：它是 JSON 生态的
// 事实标准，Go 的 time.Time 默认就序列化成它，前端 new Date(...).toISOString()
// 产出的也是它，两端不用为格式协商一次。
//
// ⚠ M7 的 <input type="datetime-local"> 给出的是 "2026-10-07T15:04"，
// **那不是合法的 RFC3339**（缺秒和时区偏移），前端必须先 new Date(v).toISOString()。
// 这里刻意不做「宽容解析」：接受多种格式意味着同一个时刻有多种写法，
// 而写错时区的那一种会静默地把时间挪 8 小时，匹配算法随后给出完全错误的结果。
const timeLayout = time.RFC3339

// ItemFields 是发帖/改帖共用的那 9 个字段（计划 §4 #13 的请求体去掉 item_type 和 image_paths）。
//
// 时间字段是**字符串**而不是 time.Time：解析失败要能报出「哪个字段、什么格式才对」，
// 而交给 encoding/json 自动解析的话，用户会收到一句
// "parsing time \"2026-13-01\" as ...: cannot parse" 的英文技术细节。
type ItemFields struct {
	Title          string
	Description    string
	CategoryID     int64
	LocationID     int64
	LocationDetail string
	LastSeenAt     string
	LostAt         string
	FoundAt        string
	Contact        string
}

// normalizedItem 是校验并规范化之后的字段值，可以直接交给 repo。
//
// 和 ItemFields 的区别：文本都 trim 过了、时间都解析成 *time.Time 了、
// 长度和跨字段规则都过了。repo 拿到它之后不需要再判断任何业务规则。
type normalizedItem struct {
	Title          string
	Description    string
	LocationDetail string
	Contact        string
	LastSeenAt     *time.Time
	LostAt         *time.Time
	FoundAt        *time.Time
}

// normalizeItemFields 校验并规范化发帖/改帖的输入。
//
// itemType 必须是 "lost" 或 "found"（调用方保证；#13 在 Create 里查，
// #16 从库里已有的那一行取，因为「改帖不能改类型」）。
func normalizeItemFields(itemType string, f ItemFields) (normalizedItem, error) {
	var out normalizedItem

	title := strings.TrimSpace(f.Title)
	if title == "" {
		return out, apperr.Validation("标题不能为空",
			apperr.FieldError{Field: "title", Msg: "写清楚是什么东西，例如「黑色长款钱包」"})
	}
	if err := validateLength("title", title, titleMaxChars); err != nil {
		return out, err
	}

	desc := strings.TrimSpace(f.Description)
	if err := validateLength("description", desc, descriptionMaxChars); err != nil {
		return out, err
	}

	detail := strings.TrimSpace(f.LocationDetail)
	if err := validateLength("location_detail", detail, locationDetailMaxChars); err != nil {
		return out, err
	}

	// ---- contact：只校验非空和长度，**内容一律不校验** ----
	//
	// 这是计划 §3.2 里写得最重的一条契约，也是本系统最容易被「顺手加个校验」破坏的地方。
	// 明确**不做**的事：
	//   - 不做格式校验（不强制手机号 11 位、不校验邮箱、不校验微信号规则）
	//   - 不做占位值黑名单（填「无」「问宿舍阿姨」「图书馆前台」全部接受）
	//   - 不做真实性校验、不做敏感词过滤
	//
	// 理由（定位原则 1）：平台一旦开始校验，就要为校验结果背书 ——
	// 「你通过了我的校验，所以这个联系方式是真的」。而 lost+found 本质是靠自觉维持的，
	// 填错的后果由填错的人自己承担：没人联系得上他，他的东西就还不了。
	// 这个反馈闭环不需要平台插手，平台插手反而会让人以为平台担保过。
	//
	// 唯一的约束是「去首尾空白后非空」，因为 items.contact 是 NOT NULL 且有一条
	// items_contact_not_blank 的 CHECK，绕不过去。
	contact := strings.TrimSpace(f.Contact)
	if contact == "" {
		return out, apperr.Validation("联系方式不能为空",
			apperr.FieldError{Field: "contact", Msg: "平台不提供站内私信，这是对方联系到你的唯一方式"})
	}
	if err := validateLength("contact", contact, contactMaxChars); err != nil {
		return out, err
	}

	if f.CategoryID <= 0 {
		return out, apperr.Validation("必须选择一个分类",
			apperr.FieldError{Field: "category_id", Msg: "从 GET /api/categories 里选一个小类"})
	}
	if f.LocationID <= 0 {
		return out, apperr.Validation("必须选择一个地点",
			apperr.FieldError{Field: "location_id", Msg: "从 GET /api/locations 里选一个具体地点"})
	}

	lastSeen, err := parseItemTime("last_seen_at", f.LastSeenAt)
	if err != nil {
		return out, err
	}
	lost, err := parseItemTime("lost_at", f.LostAt)
	if err != nil {
		return out, err
	}
	found, err := parseItemTime("found_at", f.FoundAt)
	if err != nil {
		return out, err
	}

	// ---- 时间语义：三个具名列，哪种类型填哪列是钉死的 ----
	//
	// 数据库有一条 items_time_semantics CHECK 做兜底，但**必须在这里先查一遍**：
	// CHECK 被撞到时的报错是英文技术细节，而且用户看不出该改哪个字段。
	// 这里的错误信息是能直接显示在表单上的。
	switch itemType {
	case model.ItemTypeLost:
		if lastSeen == nil || lost == nil {
			return out, apperr.Validation("失物帖必须同时填写两个时间",
				apperr.FieldError{Field: "last_seen_at", Msg: "最后一次确认还拥有它的时间"},
				apperr.FieldError{Field: "lost_at", Msg: "发现它不见了的时间"})
		}
		if found != nil {
			return out, apperr.Validation("失物帖不能填拾获时间",
				apperr.FieldError{Field: "found_at", Msg: "found_at 只有拾物帖才用"})
		}
		// last_seen_at <= lost_at：「还拿着」不可能晚于「发现没了」。
		// 填反了不是笔误就是理解错了字段含义，两种都需要明确告诉他。
		if lastSeen.After(*lost) {
			return out, apperr.Validation("最后一次见到的时间不能晚于丢失时间",
				apperr.FieldError{Field: "last_seen_at", Msg: "它必须不晚于 lost_at"},
				apperr.FieldError{Field: "lost_at", Msg: "它必须不早于 last_seen_at"})
		}
	case model.ItemTypeFound:
		if found == nil {
			return out, apperr.Validation("拾物帖必须填写拾获时间",
				apperr.FieldError{Field: "found_at", Msg: "你**实际捡到**的时间，不是现在上传的时间"})
		}
		if lastSeen != nil || lost != nil {
			return out, apperr.Validation("拾物帖不能填丢失时间",
				apperr.FieldError{Field: "last_seen_at", Msg: "这两个字段只有失物帖才用"},
				apperr.FieldError{Field: "lost_at", Msg: "这两个字段只有失物帖才用"})
		}
	default:
		return out, apperr.Validation("帖子类型不对",
			apperr.FieldError{Field: "item_type", Msg: "只能是 lost 或 found"})
	}

	out.Title = title
	out.Description = desc
	out.LocationDetail = detail
	out.Contact = contact
	out.LastSeenAt = lastSeen
	out.LostAt = lost
	out.FoundAt = found
	return out, nil
}

// parseItemTime 把一个可选的时间字符串解析成 *time.Time。空串 → nil（表示这一列写 NULL）。
func parseItemTime(field, raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(timeLayout, raw)
	if err != nil {
		return nil, apperr.Validation(
			fmt.Sprintf("%s 的格式不对", field),
			apperr.FieldError{Field: field, Msg: "必须是 RFC3339，例如 2026-10-07T15:04:05+08:00"})
	}
	// 存 UTC（§3.1「时间一律存 UTC」）。转不转其实不影响数据库里的值
	// （timestamptz 存的就是一个绝对时刻），但转了之后日志和 SQL tracer 打出来的
	// 参数都是 UTC，和库里的东西一致，排查时不用在脑子里换算时区。
	utc := t.UTC()
	return &utc, nil
}

// validateImagePaths 校验发帖请求里的图片路径数组。
//
// 三件事：数量上限、形状白名单、去重。
// 形状白名单的安全含义见 service.IsUploadPath 的注释（那是本文件最重要的一条防线）。
//
// 去重不是洁癖：如果同一个 path 被写进两行 item_images，那么 #42 删掉其中一行时
// 会连带删掉磁盘文件，另一行就变成一个指向不存在文件的死链 —— 前端渲染出一张碎图，
// 而数据库里那行看起来完全正常。这种「数据自相矛盾」的状态一旦产生就没人会去清。
func validateImagePaths(paths []string) error {
	if len(paths) > maxItemImages {
		return apperr.Validation(
			fmt.Sprintf("一个帖子最多 %d 张图片，当前 %d 张", maxItemImages, len(paths)),
			apperr.FieldError{Field: "image_paths", Msg: fmt.Sprintf("最多 %d 张", maxItemImages)})
	}

	seen := make(map[string]struct{}, len(paths))
	for i, p := range paths {
		if !IsUploadPath(p) {
			return apperr.Validation("图片路径不合法",
				apperr.FieldError{Field: fmt.Sprintf("image_paths[%d]", i),
					Msg: "必须是 POST /api/uploads 返回的 path 原样传回来"})
		}
		if _, dup := seen[p]; dup {
			return apperr.Validation("图片路径重复了",
				apperr.FieldError{Field: fmt.Sprintf("image_paths[%d]", i), Msg: "同一张图不能传两次"})
		}
		seen[p] = struct{}{}
	}
	return nil
}

// contactLocked 是计划 §4「#15 的 contact 可见性规则」那四行伪代码里
// **不查库的前两行 + 最后一行**。
//
// 完整规则（照抄计划）：
//
//	if item.item_type == 'lost':       不锁     # lost 帖联系方式公开
//	elif 当前用户是发帖人:              不锁     # 自己的帖子当然能看
//	elif 已登录且 contact_views 有记录:  不锁     # 已解锁过   ← 这一条在 Item 服务里查库
//	else:                              锁
//
// 所以这个函数返回 true 的确切含义是「**光看身份还不能放行**」，而不是
// 「最终就是锁着的」：登录后非作者看 found 帖时，还要经过
// (*Item).contactLockedAfterLookup 那一次 EXISTS 查询才能定案。
// 之所以不把这个函数改成返回 error 直接查库：它是全系统最容易写错的一段逻辑
// （写错的后果是把别人的联系方式公开出去），而一个纯函数能被表驱动单测
// 一遍扫尽（§10 第①层），加上 error 就必须在测试里造 fake store 才跑得动。
// 拆成「纯身份判断 + 一次查库」两段，各自都能被各自的方式测透。
func contactLocked(it *model.Item, viewer *model.User) bool {
	if it.ItemType == model.ItemTypeLost {
		return false
	}
	if viewer != nil && viewer.ID == it.UserID {
		return false
	}
	return true
}

// authorizeItemWrite 是 #16 改帖 / #17 删帖共用的权限判断。
//
// 返回 asAdmin 表示「这次操作是管理员在动别人的数据行」。调用方要拿它决定
// 日志级别，M6 还要拿它决定要不要写 admin_actions（计划 §4：#16/#17 的 admin
// 分支必须补写）。
//
// 三条规则，顺序有讲究：
//  1. 本人 → 直接放行，**不要求理由**。自己的帖子自己改，不需要向任何人解释。
//  2. 不是本人也不是 admin → FORBIDDEN。
//  3. 是 admin 但不是本人 → 必须有 admin_reason，否则 VALIDATION。
//
// 第 3 条为什么是「先判身份再判理由」而不是反过来：如果先校验理由，
// 一个普通用户随便带上一个 admin_reason 就能收到「理由不合格」而不是「你没权限」，
// 那等于向他确认了「这个端点存在一条 admin 通道」。权限判断永远走在字段校验前面。
func authorizeItemWrite(actor *model.User, ownerID int64, adminReason string) (asAdmin bool, err error) {
	if actor.ID == ownerID {
		return false, nil
	}
	if !actor.IsAdmin() {
		return false, apperr.Forbidden("只能操作自己发布的帖子")
	}
	if strings.TrimSpace(adminReason) == "" {
		return false, apperr.Validation("管理员操作别人的帖子必须填写理由",
			apperr.FieldError{Field: "admin_reason",
				Msg: "这个理由会写进 admin_actions，让其他管理员看到你为什么动了别人的帖子"})
	}
	return true, nil
}

// ---------- 列表查询参数 ----------

// ListQuery 是 #14 / #19 的原始查询参数，全部是字符串。
//
// 刻意不在 handler 里做 strconv.Atoi：那样「参数不合法」的错误信息就会长在 handler 里，
// 而 handler 的职责是 HTTP 翻译，不该包含「page 必须大于 0」这种规则。
// 全传字符串进来，解析和校验集中在这里，出错时能给出带字段名的 VALIDATION。
type ListQuery struct {
	ItemType   string
	Keyword    string
	CategoryID string
	LocationID string
	Status     string
	Sort       string
	Page       string
	PageSize   string
}

// statusPolicy 决定一个列表端点「默认显示哪些状态」和「允许显式请求哪些状态」。
type statusPolicy struct {
	Default []string
	Allowed []string
}

// #14 广场（公开）：默认只看 open。
//
// closed（已归还）不默认显示，因为它对「正在找东西的人」是噪声 ——
// 但允许显式 status=closed 查，找已归还的历史记录时用得上。
// deleted **一律不允许**：软删的帖子对广场来说就是不存在，
// 让任何人（包括发帖人自己）通过广场列表看到「这里有一条被删的帖子」，
// 等于把治理动作公开化了，而计划 §4 #49 明确写了「dismiss 也不通知被举报人」。
var publicListStatus = statusPolicy{
	Default: []string{model.ItemStatusOpen},
	Allowed: []string{model.ItemStatusOpen, model.ItemStatusClosed},
}

// #19 我的发布（JWT）：默认 open + closed，且允许显式查 deleted。
//
// 自己的帖子被 admin 下架之后应该看得见 —— 否则用户会以为帖子凭空消失了。
// M6 会额外给他发一条 admin_action 通知，但通知会过期，列表不会。
var mineListStatus = statusPolicy{
	Default: []string{model.ItemStatusOpen, model.ItemStatusClosed},
	Allowed: []string{model.ItemStatusOpen, model.ItemStatusClosed, model.ItemStatusDeleted},
}

// sortWhitelist 是 #14 的 sort 参数允许的值。
//
// 和 repo.itemSortColumns 是两张独立的白名单，**故意不共用**：
// 这一张决定「用户能请求什么」，那一张决定「什么能被拼进 SQL」。
// 两张都在，任何一张写漏了都不会变成注入或者 500。
var sortWhitelist = []string{"created_at", "lost_at", "found_at"}

// parseListQuery 把原始查询参数解析成一个可以直接交给 repo 的 ListFilter。
func parseListQuery(q ListQuery, st statusPolicy) (repo.ListFilter, error) {
	var f repo.ListFilter

	switch q.ItemType {
	case "":
		f.ItemType = ""
	case model.ItemTypeLost, model.ItemTypeFound:
		f.ItemType = q.ItemType
	default:
		return f, apperr.Validation("item_type 不对",
			apperr.FieldError{Field: "item_type", Msg: "只能是 lost 或 found"})
	}

	f.Keyword = strings.TrimSpace(q.Keyword)

	catID, err := parseIDParam("category_id", q.CategoryID)
	if err != nil {
		return f, err
	}
	f.CategoryID = catID

	locID, err := parseIDParam("location_id", q.LocationID)
	if err != nil {
		return f, err
	}
	f.LocationID = locID

	status := strings.TrimSpace(q.Status)
	if status == "" {
		f.Statuses = slices.Clone(st.Default)
	} else {
		if !slices.Contains(st.Allowed, status) {
			return f, apperr.Validation("status 不对",
				apperr.FieldError{Field: "status",
					Msg: "只能是 " + strings.Join(st.Allowed, " / ")})
		}
		f.Statuses = []string{status}
	}

	sort := strings.TrimSpace(q.Sort)
	switch {
	case sort == "":
		f.Sort = "created_at"
	case slices.Contains(sortWhitelist, sort):
		f.Sort = sort
	default:
		return f, apperr.Validation("sort 不对",
			apperr.FieldError{Field: "sort", Msg: "只能是 " + strings.Join(sortWhitelist, " / ")})
	}

	page, err := parseIntParam("page", q.Page, defaultPage, 1, 1<<30)
	if err != nil {
		return f, err
	}
	f.Page = page

	size, err := parseIntParam("page_size", q.PageSize, defaultPageSize, 1, maxPageSize)
	if err != nil {
		return f, err
	}
	f.PageSize = size

	return f, nil
}

// parseIDParam 解析一个可选的正整数 id 参数。空串 → nil（表示「不加这个筛选条件」）。
func parseIDParam(field, raw string) (*int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return nil, apperr.Validation(field+" 必须是一个正整数",
			apperr.FieldError{Field: field, Msg: "当前是 " + raw})
	}
	return &n, nil
}

// parseIntParam 解析一个带默认值和上下界的整数参数。
func parseIntParam(field, raw string, def, min, max int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < min || n > max {
		return 0, apperr.Validation(
			fmt.Sprintf("%s 必须是 %d 到 %d 之间的整数", field, min, max),
			apperr.FieldError{Field: field, Msg: "当前是 " + raw})
	}
	return n, nil
}

// Page 是计划 §4 约定的统一分页形状：{list, total, page, page_size}。
//
// List 永远是非 nil 切片：空结果序列化成 `[]` 而不是 `null`，
// 前端的 `data.list.map(...)` 就不用先判空。
type Page[T any] struct {
	List     []T `json:"list"`
	Total    int `json:"total"`
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
}

// PageQuery 是「只有分页参数」的那类端点用的输入（#22 解锁名单、#30 收件箱）。
//
// 不复用 ListQuery：那个有八个字段，为它写的 fake 或调用点都得先想清楚
// 「其余八个字段在这里是什么意思」。分页是这两个端点唯一的查询参数，
// 就用一个只有两个字段的结构体。
//
// 同样是**原始字符串**而不是 int：解析失败要能报出字段名的中文 VALIDATION。
type PageQuery struct {
	Page     string
	PageSize string
}

// parsePageQuery 解析 page / page_size，缺省走 §4 的统一约定 ?page=1&page_size=20。
//
// 上限沿用 maxPageSize：这三个字（page_size）在所有分页端点上的值域必须是同一个，
// 否则前端一个通用的分页组件就得按端点配不同的 max，
// 而配错的那一个表现为「翻到第 200 页就空了」这种没人会当成 bug 报的行为。
func parsePageQuery(q PageQuery) (page, pageSize int, err error) {
	page, err = parseIntParam("page", q.Page, defaultPage, 1, 1<<30)
	if err != nil {
		return 0, 0, err
	}
	pageSize, err = parseIntParam("page_size", q.PageSize, defaultPageSize, 1, maxPageSize)
	if err != nil {
		return 0, 0, err
	}
	return page, pageSize, nil
}

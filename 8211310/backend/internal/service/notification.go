package service

import (
	"context"
	"log/slog"
	"strconv"
	"strings"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
	"lostfound/internal/repo"
)

// 这个文件是 M4 的第二块：**通知只读，不改任何业务**。
//
// M3 已经往 notifications 里写 new_match 了，但没有一个接口能把它读出来 ——
// 也就是说「失主被通知了」这件事在前端是不可见的。#30–#32 补的就是这一段。
//
// ⚠ 这里**没有任何方法能写通知**。写通知的路径只有两条，都不在这个文件：
//   - repo.Match.RecordMatches（和 match_pairs 同一事务，M3）
//   - M5 的归还状态机、M6 的治理动作
//
// 把它们收在一处做成一个通用 Push 是错的：那样 #32 这种「读」的服务就能发通知，
// 而通知是**后果**，后果必须住在产生它的那件事旁边。

// maxMarkReadIDs 是一次能标记多少条通知的上限。
//
// 数据库没有这个限制，所以它是**我们自己加的**（和 M2 的 maxItemImages 同一类决定）：
//   - 收件箱一页最多 maxPageSize=100 条，「全部已读」走的是 all=true 那条分支，
//     所以 200 这个数没有任何正常 UI 能碰到；
//   - 碰得到它的只有脚本：一个 1MB 的请求体（middleware.MaxBodySize）能塞进
//     几万个 id，而 `id = ANY($1)` 会把那个数组逐个比对 ——
//     这条 SQL 的代价随数组长度线性涨，且没有任何业务上限。
//
// 报 VALIDATION 而不是静默截断：截断的话用户会以为「点过了但没生效」。
const maxMarkReadIDs = 200

// NotificationStore 是收件箱需要的持久化能力。
//
// ⚠ 只有四条，且**没有 Insert / Push / Create**。这个接口形状就是本文件
// 「只读通知」这件事的类型层面证据：想在这里发一条通知，得先改接口，
// 而改接口会撞上所有 fake 和这个文件开头的注释。
type NotificationStore interface {
	List(ctx context.Context, f repo.NotificationFilter) ([]model.Notification, int, error)
	UnreadCount(ctx context.Context, userID int64) (int, error)
	CountForeign(ctx context.Context, userID int64, ids []int64) (int, error)
	MarkRead(ctx context.Context, userID int64, ids []int64) (int, error)
}

// Notification 是收件箱的业务规则。
type Notification struct {
	notes  NotificationStore
	logger *slog.Logger
}

func NewNotification(notes NotificationStore, logger *slog.Logger) *Notification {
	if logger == nil {
		logger = slog.Default()
	}
	return &Notification{notes: notes, logger: logger}
}

// ---------- 请求/响应形状 ----------

// InboxQuery 是 #30 的原始查询参数（一律还没解析，解析在下面的方法里）。
type InboxQuery struct {
	IsRead   string
	Page     string
	PageSize string
}

// UnreadResult 是 #31 的 data：{count}。
type UnreadResult struct {
	Count int `json:"count"`
}

// MarkReadInput 是 #32 的请求体，两个字段**二选一**。
//
// 两个都是指针，为的是区分「字段没带」和「带了但是空/false」：
//   - ids 没带 → nil；`{"ids":[]}` → 指向空切片
//   - all 没带 → nil；`{"all":false}` → 指向 false
//
// 这个区分不是洁癖。用值类型的话 `{"all":false}` 会被当成「没带 all」，
// 于是那条请求的语义从「我不想标记任何东西」（该 VALIDATION）
// 悄悄变成「把全部标成已读」（真的改了库）—— 一个字段缺失和一个字段为 false
// 撞进同一个分支，是这类接口最经典的越权写法。
type MarkReadInput struct {
	IDs *[]int64
	All *bool
}

// MarkReadResult 是 #32 的 data：{updated_count}。
//
// UpdatedCount 数的是「这次真的从未读变成已读的行数」，不是「你提交了几个 id」。
// 所以同一批 id 连着标记两次，第二次是 0 —— 那个 0 是幂等的证据，
// M4 的集成测试专门断言它（换成「提交几条就返回几」的话，这条断言就永远看不出问题）。
type MarkReadResult struct {
	UpdatedCount int `json:"updated_count"`
}

// ---------- #30 收件箱 ----------

// ListMine 分页返回当前用户的通知。
//
// userID 来自 JWT，不是查询参数 —— 和 #19「我的发布」同一条纪律：
// 「谁的收件箱」这件事永远不该由客户端说了算。
func (s *Notification) ListMine(ctx context.Context, userID int64, q InboxQuery) (*Page[model.NotificationView], error) {
	isRead, err := parseIsReadParam(q.IsRead)
	if err != nil {
		return nil, err
	}
	page, pageSize, err := parsePageQuery(PageQuery{Page: q.Page, PageSize: q.PageSize})
	if err != nil {
		return nil, err
	}

	rows, total, err := s.notes.List(ctx, repo.NotificationFilter{
		UserID:   userID,
		IsRead:   isRead,
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		return nil, err
	}

	list := make([]model.NotificationView, 0, len(rows))
	for _, n := range rows {
		list = append(list, n.View())
	}
	return &Page[model.NotificationView]{List: list, Total: total, Page: page, PageSize: pageSize}, nil
}

// parseIsReadParam 把 ?is_read= 解析成三态：nil 不过滤，否则只看某一类。
//
// 只认 true / false 两种写法（大小写不敏感），**不认 1/0/yes/no**。
// 理由和 M2 拒绝宽容解析时间格式是同一条：一个含义有多种写法，
// 前端就得记住哪几种能用，而写错的那种会被静默当成「不过滤」——
// 用户勾了「只看未读」却看到全量列表，这是最难报也最难查的 bug。
// 现在它会报 VALIDATION 并列出合法取值。
func parseIsReadParam(raw string) (*bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	switch strings.ToLower(raw) {
	case "true":
		t := true
		return &t, nil
	case "false":
		f := false
		return &f, nil
	default:
		return nil, apperr.Validation("is_read 不对",
			apperr.FieldError{Field: "is_read", Msg: "只能留空（全部）、true（只看已读）或 false（只看未读），当前是 " + raw})
	}
}

// ---------- #31 未读数 ----------

// UnreadCount 是铃铛上那个数字。
//
// 单独一条端点而不是让前端从 #30 第一页数：未读数要被轮询/在登录后立刻取，
// 而收件箱列表是 20 条连表的数据 —— 为了拿一个数字去拉一页通知，
// 是把 §10 说的「往返次数」问题用在了最不该用的地方。
func (s *Notification) UnreadCount(ctx context.Context, userID int64) (*UnreadResult, error) {
	count, err := s.notes.UnreadCount(ctx, userID)
	if err != nil {
		return nil, err
	}
	return &UnreadResult{Count: count}, nil
}

// ---------- #32 标记已读 ----------

// MarkRead 把指定通知（或全部）标成已读。
//
// 计划 §4 第 32 行把 ids 和 all 定成**二选一**，这里严格执行：
// 都带 → VALIDATION，都不带 → VALIDATION。
// 都带时不选一个「更宽松的」解释是有原因的：`{ids:[1,2], all:true}` 的两种可能含义
// （「先按 ids 标，再标全部」vs「all 覆盖 ids」）在后端永远猜不出来，
// 猜错的后果是多把用户没打算读的通知标成已读 —— 已读是不可逆的（系统里没有
// 「标回未读」的端点），所以这里宁可拒绝。
//
// 判断顺序也是刻意的：
//
//	① 形状（二选一、非空、上限、正整数）→ VALIDATION
//	② 归属（这批 id 里有没有别人的）→ FORBIDDEN
//	③ 执行
//
// ② 排在 ③ 之前，且**不合并进 UPDATE 的 WHERE**：如果只靠 WHERE user_id=$1 兜底，
// 越权的那部分行会被静默跳过，用户看到 updated_count 比预期小却不知道原因。
// 先数一次 foreign，才能给出那句「你不拥有这些通知」。（并发上这确实不是原子的，
// 但通知的归属在它被创建的那一刻就定了，没有任何接口会改 user_id ——
// 真正不可少的防线在 repo.MarkRead 的 WHERE 里，两层各管各的失败模式。）
func (s *Notification) MarkRead(ctx context.Context, userID int64, in MarkReadInput) (*MarkReadResult, error) {
	ids, markAll, err := resolveMarkReadTarget(in)
	if err != nil {
		return nil, err
	}

	if ids != nil {
		foreign, err := s.notes.CountForeign(ctx, userID, ids)
		if err != nil {
			return nil, err
		}
		if foreign > 0 {
			return nil, apperr.Forbidden("有不属于当前用户的通知")
		}
	}

	// markAll 时 ids 必然是 nil（resolveMarkReadTarget 的两个分支就是这么返回的），
	// 于是 repo 那边收到的就是「这个人所有未读」那条 SQL。
	// 这里刻意不把「全部」翻译成「先把 id 查出来再逐条标」：
	// 那会把一条 UPDATE 变成两次查询 + 一条带超长数组的 UPDATE，
	// 而且翻页之间新到达的通知会被漏掉。
	updated, err := s.notes.MarkRead(ctx, userID, ids)
	if err != nil {
		return nil, err
	}

	s.logger.InfoContext(ctx, "notification.mark_read",
		slog.Int64("user_id", userID),
		slog.Bool("all", markAll),
		slog.Int("requested", len(ids)),
		slog.Int("updated_count", updated),
	)
	return &MarkReadResult{UpdatedCount: updated}, nil
}

// resolveMarkReadTarget 把「二选一」的两种形状解析成 (ids, all)。
// 返回的 ids 为 nil 表示「不是按 id 走」。
func resolveMarkReadTarget(in MarkReadInput) (ids []int64, all bool, err error) {
	hasIDs, hasAll := in.IDs != nil, in.All != nil

	switch {
	case hasIDs && hasAll:
		return nil, false, apperr.Validation("ids 和 all 只能二选一",
			apperr.FieldError{Field: "all", Msg: "按 id 标记时不要带 all，标记全部时不要带 ids"})
	case !hasIDs && !hasAll:
		// 空请求体在这里**不**当成「什么都不做」：那会让一个忘了带 body 的请求
		// 返回 200 + updated_count:0，用户以为「已经标记过了」。
		return nil, false, apperr.Validation("要标记的通知没指定",
			apperr.FieldError{Field: "ids", Msg: "传 ids 数组（按条标记）或 all: true（全部标记）"})
	case hasAll && !*in.All:
		// {"all": false} 不是「标记 0 条」，那是个没有意义的请求；
		// 也不是「把全部标成未读」—— 系统里没有这个操作，绝不能在这里暗示它。
		return nil, false, apperr.Validation("all 只能是 true",
			apperr.FieldError{Field: "all", Msg: "要按条标记请传 ids；把全部标成已读请传 all: true"})
	case hasAll:
		return nil, true, nil
	}

	// 剩下只有 hasIDs 这一种情况。
	list := *in.IDs
	if len(list) == 0 {
		return nil, false, apperr.Validation("ids 不能为空",
			apperr.FieldError{Field: "ids", Msg: "至少要有一个 id；要标记全部请传 all: true"})
	}
	if len(list) > maxMarkReadIDs {
		return nil, false, apperr.Validation("一次标记的条数太多了",
			apperr.FieldError{Field: "ids", Msg: "最多 " + strconv.Itoa(maxMarkReadIDs) + " 个，全部标记请传 all: true"})
	}
	for _, id := range list {
		if id <= 0 {
			return nil, false, apperr.Validation("ids 里有不合法的 id",
				apperr.FieldError{Field: "ids", Msg: "id 必须是正整数，当前有 " + strconv.FormatInt(id, 10)})
		}
	}
	return list, false, nil
}

package service

import (
	"context"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
)

// 这个文件是 M4 的第三块，也是全项目**最短的一个 service**：一次 INSERT，没有第二次。
//
// 短是设计的目标而不是巧合。§14-13 说举报会被当成武器用（两个人互相看不顺眼
// 就能把待办队列灌满），挡掉的三层里第 ③ 层是「举报零自动后果」——
// 刷再多也不会让对方的帖子下沉或消失，攻击收益是 0。
// 这句话在这个文件里的落地方式非常具体：**这里能做的事只有插一行**。
//
// ⚠ 三条禁令（M4 的冒烟测试逐条对着断言，改动本文件前先读那三条测试）：
//  1. 不许写 items —— 举报不自动下架、不自动关闭、不改任何帖子状态。
//     实现上的保证是类型层面的：这个服务只依赖 ItemLookup（只有 GetByID，没有 UPDATE），
//     所以「顺手把帖子删了」这行代码在这里根本编译不出来。
//  2. 不许写 users.credit_score —— 举报不扣分，也不因为被举报而扣分。
//     本文件没有任何一处出现 users 表的写操作。
//  3. 不许给被举报人发通知 —— 否则举报立刻变成骚扰工具：
//     你举报我一次我就知道是你，下次我举报回去。§3.7 把这条写成了一句
//     「有一条通知绝对不发」。NotificationStore 接口在这里连出现都没出现。
//
// 处置（#48/#49）住在 M6 的 moderation.go，由 admin 决定。
// 也就是说：**用户只负责让 admin 看见，admin 负责决定，决定才有后果。**

// reportDetailMaxChars 是 reports.detail 的长度上限（VARCHAR(500)，和迁移对齐）。
const reportDetailMaxChars = 500

// ReportStore 是举报需要的持久化能力：只有一行 INSERT。
//
// 同上，接口里没有 Resolve、没有 SetStatus、没有 ListByItem ——
// M4 的举报服务能做的就这么多，剩下的是 M6 的事。
type ReportStore interface {
	Create(ctx context.Context, itemID, reporterID int64, reasonCode, detail string) (int64, time.Time, error)
}

// Report 是举报的业务规则。
type Report struct {
	reports ReportStore
	items   ItemLookup
	logger  *slog.Logger
}

func NewReport(reports ReportStore, items ItemLookup, logger *slog.Logger) *Report {
	if logger == nil {
		logger = slog.Default()
	}
	return &Report{reports: reports, items: items, logger: logger}
}

// ReportResult 是 #41 POST /api/items/:id/report 的 data（计划 §4 第 41 行）。
//
// Status 恒为 "open" —— 它是「这条举报现在等着 admin 看」这个事实，
// 不是给用户看的处置进度。把它返回而不是让用户自己猜，是为了让前端能显示
// 「已提交，管理员会看到」这种明确的回执；如果是 resolved/dismissed 出现在这里，
// 那就是平台在替 admin 表态（原则 1）。
type ReportResult struct {
	ID        int64  `json:"id"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
}

// Create 提交一条举报。
//
// 任何登录用户都能举报任何一条**存在的**帖子，这是刻意的低门槛：
// 举报是治理的眼睛（§1.1 原则 4 那段讨论），门槛一高就没人举报了。
// 防滥用不靠这里加条件，靠的是 §14-13 那三层：
// 同人对同帖只有一条待处理（uq_reports_open，撞了报 REPORT_DUPLICATE）、
// 被举报人永远不知道（不发通知）、且举报本身不产生任何自动后果。
//
// 判断顺序：
//
//	① reason_code 白名单 → VALIDATION
//	② detail 长度 → VALIDATION
//	③ 帖子存在且没被软删 → NOT_FOUND
//	④ 插一行（撞部分唯一索引 → REPORT_DUPLICATE）
//
// ①② 排在 ③ 前面是有取舍的：字段错了应该先说字段错了，
// 因为那是用户当下能改的东西；而「帖子不存在」他改不了。
// 顺带的好处是：一个乱填 reason_code 的请求不会去查库。
func (s *Report) Create(ctx context.Context, reporter *model.User, itemID int64, reasonCode, detail string) (*ReportResult, error) {
	code := strings.TrimSpace(reasonCode)
	if !slices.Contains(model.ReportReasonCodes, code) {
		return nil, apperr.Validation("举报理由不对",
			apperr.FieldError{Field: "reason_code",
				Msg: "只能是 " + strings.Join(model.ReportReasonCodes, " / ")})
	}

	detail = strings.TrimSpace(detail)
	if utf8.RuneCountInString(detail) > reportDetailMaxChars {
		return nil, apperr.Validation("举报说明太长了",
			apperr.FieldError{Field: "detail",
				Msg: "最多 " + strconv.Itoa(reportDetailMaxChars) + " 个字（一个汉字算 1 个）"})
	}

	d, err := s.items.GetByID(ctx, itemID)
	if err != nil {
		return nil, err
	}
	if d.Status == model.ItemStatusDeleted {
		// 软删的帖子对外就是不存在（#15、#22 同一条纪律）。
		// 而且被下架的帖子已经不在广场上了，举报它没有意义 —— admin 手里有那条帖子。
		return nil, apperr.NotFound("帖子")
	}

	id, createdAt, err := s.reports.Create(ctx, itemID, reporter.ID, code, detail)
	if err != nil {
		return nil, err
	}

	s.logger.InfoContext(ctx, "report.submitted",
		slog.Int64("report_id", id),
		slog.Int64("item_id", itemID),
		slog.Int64("reporter_id", reporter.ID),
		slog.Int64("owner_id", d.UserID),
		slog.String("reason_code", code),
	)

	return &ReportResult{
		ID:        id,
		Status:    model.ReportStatusOpen,
		CreatedAt: createdAt.UTC().Format(time.RFC3339),
	}, nil
}

package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lostfound/internal/apperr"
)

// Report 管 reports 表。M4 只需要一件事：**插一行**。
//
// 读的那一半（#48 待办列表、#49 处置）属于 admin，在 M6。
// 这里刻意不预先写一个 List 或 Resolve 方法 —— 那些方法一旦存在，
// M4 的 service 就能「顺手」把举报状态改掉，而 §3.7 的整个设计是
// **用户只能提交举报，不能推进它**。少一个方法就少一种越权写法。
type Report struct {
	pool *pgxpool.Pool
}

func NewReport(pool *pgxpool.Pool) *Report { return &Report{pool: pool} }

// uqReportsOpen 是 000001 迁移第 12 节里那条**部分唯一索引**的名字：
//
//	CREATE UNIQUE INDEX uq_reports_open ON reports(item_id, reporter_id) WHERE status = 'open'
//
// 它是「防刷举报」的全部实现（§14-13 的第 ① 层）：同一个人对同一条帖子
// 只能有一条待处理的举报，反复点不产生第二行。
// 注意是「部分」唯一 —— 处置完（status 变成 resolved/dismissed）之后
// 同一个人可以再举报同一条帖子，这是对的：那条帖子又出问题了。
const uqReportsOpen = "uq_reports_open"

// Create 插入一条 open 举报，返回新行的 id 和 created_at。
//
// status 不在参数里：这一列只有一个合法值（open），由 service 决定而不是由调用方传 ——
// 传了 status 就等于给了任何调用方「直接把举报标成已处置」的能力。
//
// ⚠ 撞 uq_reports_open 时返回 **REPORT_DUPLICATE**，而且这个判断必须排在
// TranslateConstraint 之前：那个函数把所有 23505 一律翻成 CONFLICT，
// 而 CONFLICT 是「内容和已有记录冲突」这种通用文案，用户看了不知道该怎么办。
// REPORT_DUPLICATE 是 §8 里给举报专门留的码，前端据此显示「你已经举报过了」。
//
// reason_code 的 CHECK（23514）也在这里兜底，但那是兜底不是主检查 ——
// service 会先按 model.ReportReasonCodes 校验一遍，好给出带字段名的中文提示。
func (r *Report) Create(ctx context.Context, itemID, reporterID int64, reasonCode, detail string) (int64, time.Time, error) {
	var (
		id        int64
		createdAt time.Time
	)
	err := r.pool.QueryRow(ctx, `
		INSERT INTO reports (item_id, reporter_id, reason_code, detail)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at`,
		itemID, reporterID, reasonCode, detail).Scan(&id, &createdAt)
	if err != nil {
		var pge *pgconn.PgError
		if errors.As(err, &pge) && pge.Code == pgUniqueViolation && pge.ConstraintName == uqReportsOpen {
			return 0, time.Time{}, apperr.WrapMsg(err, apperr.CodeReportDuplicate,
				"你已经举报过这条帖子了，管理员的处理中")
		}
		if ae := TranslateConstraint(err); ae != nil {
			return 0, time.Time{}, ae
		}
		return 0, time.Time{}, fmt.Errorf("repo.Report.Create (item=%d, reporter=%d): %w", itemID, reporterID, err)
	}
	return id, createdAt, nil
}

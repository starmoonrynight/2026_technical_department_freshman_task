package repo

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"lostfound/internal/model"
)

// Credit 管 credit_logs 的**读**。
//
// ⚠ 这个类型里没有任何写方法，而且这是刻意的：
// 流水必须和那次改分在同一个事务里，而那个事务住在 ItemReturn.Confirm
// （applyCredit 那三步：锁读旧值 → 夹边界写新值 → 记实际增量）。
// 如果这里再放一个 Insert，就有第二条「记了流水却没改分」和
// 第三条「改了分却没记流水」的路径，而 §3.6 说这张表存在的全部理由就是
// 「Σdelta + 100 == credit_score」这条对账性质。少一个方法就少一种写错的方式。
type Credit struct {
	pool *pgxpool.Pool
}

func NewCredit(pool *pgxpool.Pool) *Credit { return &Credit{pool: pool} }

// List 分页取某个人的积分流水，最新在前。
//
// WHERE 里的 user_id 是必加条件而不是可选筛选（同 Notification.List 那条纪律）：
// 它来自 JWT，永远不来自查询参数。少这一句就是「任何人可读全站所有人的加分历史」，
// 而那份历史能直接反推出「谁在什么时候确认归还过什么东西」。
//
// ORDER BY created_at DESC, id DESC：id 作第二排序键是为了让同一秒内插入的多条流水
// （一次 confirm 会给两个人各记一条，时间戳完全相同）在翻页时顺序稳定。
// 走的是 idx_credit_logs_user (user_id, created_at DESC)。
//
// 一个从没确认过归还的用户返回**空切片而不是 nil**：JSON 里那是 `[]`，
// 前端可以直接 map；如果是 null，它得先判空，而「判空」在 JS 里经常被写成
// `if (list)` 那种把空数组也当假的错法（同 list 在所有分页端点上的纪律）。
func (c *Credit) List(ctx context.Context, userID int64, page, pageSize int) ([]model.CreditLog, int, error) {
	var total int
	if err := c.pool.QueryRow(ctx,
		`SELECT count(*) FROM credit_logs WHERE user_id = $1`, userID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("repo.Credit.List 计数 (user=%d): %w", userID, err)
	}

	rows, err := c.pool.Query(ctx, `
		SELECT id, user_id, delta, reason, ref_type, ref_id, created_at
		  FROM credit_logs
		 WHERE user_id = $1
		 ORDER BY created_at DESC, id DESC
		 LIMIT $2 OFFSET $3`,
		userID, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, fmt.Errorf("repo.Credit.List (user=%d): %w", userID, err)
	}
	defer rows.Close()

	out := []model.CreditLog{}
	for rows.Next() {
		var v model.CreditLog
		if err := rows.Scan(&v.ID, &v.UserID, &v.Delta, &v.Reason,
			&v.RefType, &v.RefID, &v.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("repo.Credit.List 扫描一行: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("repo.Credit.List 迭代: %w", err)
	}
	return out, total, nil
}

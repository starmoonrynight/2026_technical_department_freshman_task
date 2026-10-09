package repo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lostfound/internal/model"
)

// Contact 管 contact_views 这一张表：解锁（写）、判断是否已解锁（点查）、名单（分页读）。
//
// 它和 repo.Item 分开，是因为这条链上的规则完全不同：Item 管「一条帖子的增删改查」，
// 而这里的三条读写的**幂等性**要求不一样 —— Unlock 是全站唯一一个「同一个调用者
// 反复调用必须永远成功、且永远只留一行」的写接口。这个性质靠
// UNIQUE(item_id, user_id) + ON CONFLICT DO NOTHING 实现，全部代码在这一个文件里，
// M4 的集成测试直连它测幂等，不用绕一圈 HTTP。
type Contact struct {
	pool *pgxpool.Pool
}

func NewContact(pool *pgxpool.Pool) *Contact { return &Contact{pool: pool} }

// Unlock 幂等地记下「这个用户看过这条帖子的联系方式」。
//
// 返回值里的 at 是**第一次**解锁的时刻，不是本次调用的时刻 —— 这一点必须由测试钉住：
// 如果重复调用把 created_at 刷成 now()，那份审计日志就再也看不出
// 「这个人到底是哪天开始盯上这条帖子的」，而 §3.4 说这张表存在的唯一理由就是审计。
// 实现上靠的是「冲突时不再 UPDATE，而是把已有那一行读回来」。
//
// created=false 就是「这一行本来就存在」。service 把它原样透出成 already_unlocked，
// 前端据此决定要不要弹「已解锁过，这是你 X 月 X 日看过的」提示。
//
// ⚠ 这里**不往 notifications 写任何东西**。§16 已经定过：`contact_unlocked`
// 这个通知 type 被删掉了，理由是「有人看了你的联系方式」不直接帮双方联系上，
// 而一条热门 found 帖能产生十几条这种通知，纯噪音。
// M4 的冒烟测试专门断言解锁前后 notifications 行数不变 —— 少删一句，
// 将来谁「顺手」加个通知，测试会红。
func (c *Contact) Unlock(ctx context.Context, itemID, userID int64) (at time.Time, created bool, err error) {
	var id int64
	err = c.pool.QueryRow(ctx, `
		INSERT INTO contact_views (item_id, user_id)
		VALUES ($1, $2)
		ON CONFLICT (item_id, user_id) DO NOTHING
		RETURNING id, created_at`,
		itemID, userID).Scan(&id, &at)

	if errors.Is(err, pgx.ErrNoRows) {
		// Scan 拿不到行只有这一种情况：ON CONFLICT 命中了，INSERT 什么也没插。
		// 于是把已有那一行的时间读回来，让幂等的两次调用返回同一个 at。
		if err := c.pool.QueryRow(ctx, `
			SELECT created_at FROM contact_views WHERE item_id = $1 AND user_id = $2`,
			itemID, userID).Scan(&at); err != nil {
			return time.Time{}, false, fmt.Errorf("repo.Contact.Unlock 回读已有解锁记录 (item=%d, user=%d): %w",
				itemID, userID, err)
		}
		return at, false, nil
	}
	if err != nil {
		if ae := TranslateConstraint(err); ae != nil {
			return time.Time{}, false, ae
		}
		return time.Time{}, false, fmt.Errorf("repo.Contact.Unlock 插入 (item=%d, user=%d): %w", itemID, userID, err)
	}

	return at, true, nil
}

// Viewed 回答「这位读者解锁过这条帖子没有」。#15 GET /api/items/:id 用它决定
// contact 给不给（计划 §4「#15 的 contact 可见性规则」的第三条分支）。
//
// 只 SELECT 1 而不是把整行捞出来：这里要的是一个布尔值。
// 走的是 uq_contact_views 那条唯一索引，(item_id, user_id) 两个等值条件就是一次点查。
//
// 返回 error 时调用方**必须按「锁着」处理**而不是放行 —— 这是联系方式，
// 少给一次用户顶多点一下重试，多给一次就是一次隐私泄漏。
// 那条 fail-closed 的规则住在 service 层（service.Item.buildView），它才是不管怎么
// 都不能写错的那一层，所以写在那边并在那边测。
func (c *Contact) Viewed(ctx context.Context, itemID, userID int64) (bool, error) {
	var one int
	err := c.pool.QueryRow(ctx, `
		SELECT 1 FROM contact_views WHERE item_id = $1 AND user_id = $2`,
		itemID, userID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("repo.Contact.Viewed (item=%d, user=%d): %w", itemID, userID, err)
	}
	return true, nil
}

// List 分页取一条帖子的解锁名单，按解锁时间倒序。
//
// 倒序的理由是「发帖人最关心刚来看过的人」，而且 000001 迁移里那条
// idx_contact_views_item 就是 (item_id, created_at DESC) —— ORDER BY 和索引同向，
// 这个排序不需要额外开销。id 做第二排序键，因为 created_at 有默认值 now()，
// 同一事务里的两行会拿到同一个时间戳，没有第二键的话翻页会重复或漏行。
//
// JOIN users 而不是让 service 拿到 user_id 再逐个查：一份 20 行的名单点查 20 次，
// 是 §10 里说的那种「日志里刷出 20 条 SQL，把真正想看的那条埋掉」的 N+1。
// nickname 是 NOT NULL 直接取，real_name 可空（SSO 用户也可能没有），
// 所以 COALESCE 成空串 —— 对外形状里 real_name 是 string 而不是 *string。
func (c *Contact) List(ctx context.Context, itemID int64, page, pageSize int) ([]model.ContactView, int, error) {
	var total int
	if err := c.pool.QueryRow(ctx, `SELECT count(*) FROM contact_views WHERE item_id = $1`, itemID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("repo.Contact.List 计数 (item=%d): %w", itemID, err)
	}

	rows, err := c.pool.Query(ctx, `
		SELECT cv.id, cv.item_id, cv.user_id, u.nickname, COALESCE(u.real_name, ''), cv.created_at
		FROM contact_views cv
		JOIN users u ON u.id = cv.user_id
		WHERE cv.item_id = $1
		ORDER BY cv.created_at DESC, cv.id DESC
		LIMIT $2 OFFSET $3`,
		itemID, pageSize, (page-1)*pageSize)
	if err != nil {
		return nil, 0, fmt.Errorf("repo.Contact.List (item=%d): %w", itemID, err)
	}
	defer rows.Close()

	out := []model.ContactView{}
	for rows.Next() {
		var v model.ContactView
		if err := rows.Scan(&v.ID, &v.ItemID, &v.UserID, &v.Nickname, &v.RealName, &v.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("repo.Contact.List 扫描一行: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("repo.Contact.List 迭代: %w", err)
	}
	return out, total, nil
}

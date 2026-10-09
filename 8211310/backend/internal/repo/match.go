package repo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"lostfound/internal/model"
)

// Match 是匹配功能的两项持久化能力：**捞候选**和**写通知台账**。
//
// 它和 repo.Item 的分工是：Item 管「一条帖子的增删改查」，Match 管
// 「一堆帖子的相似性检索 + 一个跨 items/match_pairs/notifications 的写入」。
// 合并进 Item 会让那个文件翻倍，而且两类操作的失败语义完全不同
// （帖子建不出来必须报错；台账写不进去只记日志）。
type Match struct {
	pool *pgxpool.Pool
}

func NewMatch(pool *pgxpool.Pool) *Match { return &Match{pool: pool} }

// CandidateLimit 是候选集的硬上限。
//
// 为什么要有上限而不是一路捞到底：同一地点、时间合理的对侧帖子在校园里不会是几百条，
// 而万一有人拿脚本刷出一个「图书馆」下挂着 5000 条 found 帖的场景，
// 一次 GET matches 就要在 Go 里算 5000 次 dice。200 条上限把最坏情况钉在 <10ms，
// 超出部分本来就排在后面，截断不改变前 10 名的顺序。
// §9 的 match.run 里 candidate_count 就是这一列的真实值，撞到 200 就该看日志了。
const CandidateLimit = 200

// CandidateFilter 是捞候选的条件。
//
// ⚠ 方向是这里唯一需要小心的地方，所以字段名故意写成「对侧类型 + 锚点」而不是
// 「last_seen_at / found_at」两列名：同一份代码要服务两个方向
// （lost 找 found、found 找 lost），列名写死在参数名里会让人以为这个函数只服务一个方向。
//
// 两个方向的语义（§5.2 的 SQL + §5.5「时间硬筛选保留」）：
//   - 目标 lost，候选 found：found_at >= 目标.last_seen_at - 容差
//     （周一 8 点还拿着，周一 7 点捡到的不可能是你的；留 1 天容差因为记忆和填表都会错）
//   - 目标 found，候选 lost：候选.last_seen_at <= 目标.found_at + 容差
//     （完全同一个不等式移项后的样子，不是第二套规则）
type CandidateFilter struct {
	OppositeType string    // model.ItemTypeLost | ItemTypeFound，**候选**的类型
	TargetID     int64     // 排除自身
	LocationID   int64     // 0 = 不做地点硬筛选（Tier 2）
	AnchorAt     time.Time // 目标 lost 时传它的 last_seen_at；目标 found 时传它的 found_at
	Tolerance    time.Duration
	Limit        int // 0 = CandidateLimit
}

// Candidates 捞出要给 matcher 打分的候选帖子。
//
// 返回的是完整 ItemDetail（连表带分类名/地点名/作者名/封面/祖先 id），不是裸 items 行 ——
// 因为候选既要参与打分（要祖先 id），又要直接出现在响应里（要摘要形状），
// 还要写进台账的 breakdown 之外什么都不留。分两次查再拼形状只会多一倍往返。
//
// 这里**只筛不排**：分数永远在 Go 里算（§5.1 第三步），SQL 的 ORDER BY created_at DESC
// 只服务于「候选超过上限时保留最近的那些」这一个目的。
// 别在 SQL 里加 ORDER BY score —— 分数在 SQL 里根本不存在。
func (r *Match) Candidates(ctx context.Context, f CandidateFilter) ([]model.ItemDetail, error) {
	if f.OppositeType != model.ItemTypeLost && f.OppositeType != model.ItemTypeFound {
		// 这是调用方的编程错误，不是用户输入：对侧类型只可能由 service 算出来。
		// 不查的话一个拼错的类型会安静地捞出 0 行，症状是「永远匹配不上」，
		// 而 0 行在 Tier 1 会触发降级到 Tier 2，看起来像是「兜底生效了」。
		return nil, fmt.Errorf("repo.Match.Candidates: 非法的候选类型 %q", f.OppositeType)
	}

	limit := f.Limit
	if limit <= 0 {
		limit = CandidateLimit
	}

	// 所有值一律走占位符，没有一处拼接用户输入（纪律同 buildItemWhere）。
	// make_interval 的两个参数也都走占位符：容差是 .env 里读出来的，
	// 但「从环境变量拼进 SQL」仍然是拼 —— 这里不给它这个机会。
	var (
		conds = []string{"i.item_type = $1", "i.status = $2", "i.id <> $3"}
		args  = []any{f.OppositeType, model.ItemStatusOpen, f.TargetID}
	)
	addCond := func(sql string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(sql, len(args)))
	}

	if f.LocationID != 0 {
		addCond("i.location_id = $%d", f.LocationID)
	}

	// 容差用 make_interval 而不是 Go 里先减好时间：
	// 时间算术放在 SQL 里，一次查询一个参数，改了容差不用改代码；
	// 放在 Go 里则多一个「这个 time.Time 是锚点还是已经减过的」这种容易传错的中间量。
	//
	// ⚠ 参数名必须是 secs，不能写 hours。make_interval 的 hours 是 **integer**，
	// 只有 secs 是 double precision；把 float8 传给 hours，PostgreSQL 不会帮你转，
	// 它报的是 `function make_interval(hours => double precision) does not exist
	// (SQLSTATE 42883)` —— 看起来像函数不存在，实际是签名对不上。
	// 用 secs 还顺带免掉一次截断：容差将来配成 12.5 小时也不会悄悄变成 12。
	secs := f.Tolerance.Seconds()
	if secs < 0 {
		secs = 0
	}
	anchorIdx := len(args) + 1
	args = append(args, f.AnchorAt, secs)

	// 两个方向是同一个不等式的两种写法，不是两套规则（见 CandidateFilter 的注释）。
	// $ 编号要接着前面已经用掉的参数往后排，所以先记下锚点的位置再拼。
	//
	// ⚠ 锚点必须写 $%d::timestamptz。参数不带类型时 PostgreSQL 会「猜」：
	// `? + interval` 里它先把未知的 ? 往 interval 上靠（存在 interval + interval 这个运算符），
	// 于是整条比较变成 `timestamptz <= interval`，报 42883 operator does not exist。
	// 转型只加在参数上，列名保持裸的 —— 条件左边一旦包上函数，索引就用不上了。
	timeCond := "i.last_seen_at <= $%d::timestamptz + make_interval(secs => $%d::double precision)"
	if f.OppositeType == model.ItemTypeFound {
		timeCond = "i.found_at >= $%d::timestamptz - make_interval(secs => $%d::double precision)"
	}
	conds = append(conds, fmt.Sprintf(timeCond, anchorIdx, anchorIdx+1))

	args = append(args, limit)
	q := fmt.Sprintf("SELECT %s %s WHERE %s ORDER BY i.created_at DESC, i.id DESC LIMIT $%d",
		itemDetailCols, itemDetailFrom, strings.Join(conds, " AND "), len(args))

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("repo.Match.Candidates: %w", err)
	}
	defer rows.Close()

	out := []model.ItemDetail{}
	for rows.Next() {
		var d model.ItemDetail
		if err := scanItemDetailRows(rows, &d); err != nil {
			return nil, fmt.Errorf("repo.Match.Candidates 扫描一行: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo.Match.Candidates 迭代: %w", err)
	}
	return out, nil
}

// LedgerRow 是「记一对，并因此给 lost 作者发一条通知」所需的全部信息。
//
// 七个字段里有三个（Notify*）看起来不该由调用方传 —— 为什么不在 repo 里拼文案？
// 因为文案是业务规则（要不要带地点名、要不要写相似度、要不要承诺「可能是你的」），
// 放进 repo 就等于把它钉死在 SQL 旁边，而 §2.6 那些文案是要照着定位原则 1 逐字审的。
// repo 在这里只负责一件事：**保证台账和通知同生同死**。
type LedgerRow struct {
	LostItemID  int64
	FoundItemID int64
	Score       float64
	Breakdown   string // matcher.Breakdown 序列化出来的 JSON，列类型是 JSONB
	NotifyTo    int64  // 那条 lost 帖的作者
	NotifyTitle string
	NotifyBody  string
}

// LedgerLostAuthors 取出「台账里和这条 found 帖配过对的那些 lost 帖，它们的作者分别是谁」。
//
// 这是 M5 的 confirm 需要的唯一一次台账反查：东西确认还回去了，
// 当初被 new_match 叫醒的那些人应该收到一个结束（item_returned_hint）。
// 读放在 Match 而不是 ItemReturn，是因为 **match_pairs 这张表只有这一个主人** ——
// 谁都能写它的读路径，「台账和通知是不是原子」那套推理就会开始模糊。
//
// SELECT DISTINCT 的是**作者**而不是配对行：一条 found 帖可能和同一个人的两条失物帖
// 都配过对（他丢了两个相似的东西），那是同一个人在等同一件事，
// 而且这条通知的 item_id 指向的就是这条 found 帖 —— 发两条，点进去是同一个页面。
//
// ⚠ 不按失物帖的状态筛（closed / deleted 的作者照样在名单里）。
// 要筛就得先定义「什么状态的失主不配知道东西已经还回去了」，
// 而那是裁决而不是记录（定位原则 1）。他被 new_match 叫醒过，就该收到收尾。
//
// 提交人自己如果也是 lost 作者，会同时拿到 return_confirmed 和 item_returned_hint 两条 ——
// 这是计划 §13 第 4 步明确写的形状，不是重复发送的 bug。
func (r *Match) LedgerLostAuthors(ctx context.Context, foundItemID int64) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT DISTINCT i.user_id
		  FROM match_pairs mp
		  JOIN items i ON i.id = mp.lost_item_id
		 WHERE mp.found_item_id = $1
		 ORDER BY 1`, foundItemID)
	if err != nil {
		return nil, fmt.Errorf("repo.Match.LedgerLostAuthors (found=%d): %w", foundItemID, err)
	}
	defer rows.Close()

	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("repo.Match.LedgerLostAuthors 扫描一行: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo.Match.LedgerLostAuthors 迭代: %w", err)
	}
	return out, nil
}

// RecordMatches 在一个事务里写台账 + 发通知，返回**真正新发出去**的通知条数。
//
// 返回值不等于 len(rows)，这是这个函数存在的全部意义：
//
//	INSERT ... ON CONFLICT (lost_item_id, found_item_id) DO NOTHING
//	受影响行数 == 1  → 这一对是新的  → 插通知
//	受影响行数 == 0  → 以前记过     → 跳过，**不重复打扰**
//
// 「写了一行台账但没发通知」和「发了通知但没写台账」都是不可接受的中间态：
// 前者是用户永远等不到的推送，后者是下一次重复通知同一个人。
// 所以这两件事必须在同一个事务里，且顺序是「先台账后通知」——
// 台账那一行的 RowsAffected 就是去重的判据，它必须先于通知存在。
//
// 为什么不用一个 notified 布尔列：那一列和「这一对记过没记过」是同一件事的两种写法，
// 而两种写法就需要一个「它们不一致」的修复脚本。§3.7 明说去重机制就是这条唯一约束。
//
// rows 为空时直接返回 0，不开事务：一次 found 帖没匹配上是最常见的情况，
// 为它跑 BEGIN/COMMIT 会在 debug 日志里留两条毫无意义的 SQL，
// 而看日志的人正在找那条真正的匹配记录。
func (r *Match) RecordMatches(ctx context.Context, rows []LedgerRow) (int, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("repo.Match.RecordMatches 开事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	notified := 0
	for _, row := range rows {
		tag, err := tx.Exec(ctx, `
			INSERT INTO match_pairs (lost_item_id, found_item_id, score, breakdown)
			VALUES ($1, $2, $3, $4::jsonb)
			ON CONFLICT (lost_item_id, found_item_id) DO NOTHING`,
			row.LostItemID, row.FoundItemID, row.Score, row.Breakdown)
		if err != nil {
			if ae := TranslateConstraint(err); ae != nil {
				return 0, ae
			}
			return 0, fmt.Errorf("repo.Match.RecordMatches 写台账 (lost=%d, found=%d): %w",
				row.LostItemID, row.FoundItemID, err)
		}
		if tag.RowsAffected() == 0 {
			continue // 这一对以前通知过，跳过
		}

		// score 列有 CHECK (score >= 0 AND score <= 1)，越界会在这里变成 23514。
		// matcher 的单测已经把值域钉在 [0,1]（TestScoresAreInRange），
		// 真撞上了说明有人在打分和落库之间改了分数。
		if _, err := tx.Exec(ctx, `
			INSERT INTO notifications (user_id, type, title, content, item_id)
			VALUES ($1, $2, $3, $4, $5)`,
			row.NotifyTo, model.NotificationNewMatch, row.NotifyTitle, row.NotifyBody, row.FoundItemID); err != nil {
			if ae := TranslateConstraint(err); ae != nil {
				return 0, ae
			}
			return 0, fmt.Errorf("repo.Match.RecordMatches 写通知 (lost=%d 的作者 %d): %w",
				row.LostItemID, row.NotifyTo, err)
		}
		notified++
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("repo.Match.RecordMatches 提交事务: %w", err)
	}
	return notified, nil
}

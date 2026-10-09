package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
)

// ItemReturn 管 item_returns 这一张表，以及 confirm 那一次连带写到的
// items / users / credit_logs / notifications。
//
// 它是本仓库**唯一**允许一次事务里跨五张表写的方法集合，理由是 §3.4 那句话：
// 「confirm 的业务含义（关帖 + 加分 + 发通知 + 记录发帖人同意了）全部住在这一条链上」。
// 拆成五个方法让 service 逐个调，任何一步之间崩掉都会留下半件事：
//   - 只写了 item_returns 没关帖 → 帖子继续在广场上等人来捡，而东西已经还了
//   - 只关了帖没加分 → 拾主做了好事却查无此事，§3.6 那条「分数可解释」直接失效
//   - 加了分没发通知 → 提交人永远不知道自己被确认了
//
// 所以这里的接口形状是**三个动作**（Confirm / Reject / Cancel），不是七个 setter。
//
// ⚠ Reject 和 Cancel 都**不碰 items / users / credit_logs**，一行 SQL 都没有。
// 那是 §3.4 的语义边界：拒绝的是这条归还确认记录，不是这条帖子。
// 改动这个文件之前先读 service/item_return.go 顶部那三条禁令和
// smoketest/m5_return_reject_test.go 里的 TestRejectDoesNotCloseItem。
type ItemReturn struct {
	pool *pgxpool.Pool
}

func NewItemReturn(pool *pgxpool.Pool) *ItemReturn { return &ItemReturn{pool: pool} }

// uqItemReturnsPending 是 000001 迁移第 7 节那条**部分**唯一索引的名字：
//
//	CREATE UNIQUE INDEX uq_item_returns_pending ON item_returns(item_id, submitter_id) WHERE status = 'pending'
//
// 「同一个人对同一条帖子只能有一条待处理的确认」。注意是部分唯一 ——
// 被拒（rejected）或撤销（cancelled）之后同一个人可以再提一条，这是设计而不是漏洞：
// 补充了新证据就该能再试一次（§13 第 6 步最后一条核对项）。
const uqItemReturnsPending = "uq_item_returns_pending"

// returnCols 是单行读取（GetByID）需要的全部列，顺序和 scanItemReturn 严格对应。
const returnCols = `id, item_id, submitter_id, message, proof_image_path, status,
	owner_note, reviewer_id, review_kind, submitted_at, reviewed_at`

func scanItemReturn(row pgx.Row, r *model.ItemReturn) error {
	return row.Scan(
		&r.ID, &r.ItemID, &r.SubmitterID, &r.Message, &r.ProofImagePath, &r.Status,
		&r.OwnerNote, &r.ReviewerID, &r.ReviewKind, &r.SubmittedAt, &r.ReviewedAt,
	)
}

// GetByID 取一条归还确认记录本身（不带帖子、不带提交人，那两样由 service 各查一次）。
//
// 查不到 → NOT_FOUND。这里不藏「它属于别人」：调用方拿到行之后由 service 判权，
// 判不过给 FORBIDDEN（计划 §4 第 24 行同时列了这两个码，
// 而 M4 的 #22 已经定过一次同样的取舍）。
func (r *ItemReturn) GetByID(ctx context.Context, id int64) (*model.ItemReturn, error) {
	var v model.ItemReturn
	err := scanItemReturn(r.pool.QueryRow(ctx,
		`SELECT `+returnCols+` FROM item_returns WHERE id = $1`, id), &v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.NotFound("归还确认")
	}
	if err != nil {
		return nil, fmt.Errorf("repo.ItemReturn.GetByID (id=%d): %w", id, err)
	}
	return &v, nil
}

// SubmitRow 是「插一条 pending 归还确认，并给发帖人发 return_submitted」所需的信息。
//
// 通知的收件人和文案都由调用方传进来，理由和 M3 的 LedgerRow 一模一样：
// 文案是业务规则（要不要带上提交人昵称、要不要摘出原话），它住在 service；
// repo 在这里只保证一件事 —— **确认记录和它的通知同生同死**。
//
// 为什么必须是同一个事务：通知是这条链上唯一的推送路径，没有轮询补偿。
// 行插了而通知没发出去 = 发帖人永远不知道有人提交了确认，那条 pending 会挂到天荒地老；
// 反过来则会出现「发帖人被通知了，点进去说记录不存在」。两种都比整件事失败更难解释。
type SubmitRow struct {
	ItemID      int64
	SubmitterID int64
	Message     string
	ProofPath   string

	NotifyTo    int64
	NotifyTitle string
	NotifyBody  string
}

// Submit 插入一条 pending 记录 + 一条通知，返回新行的 id 和 submitted_at。
//
// ⚠ 撞 uq_item_returns_pending 必须返回 **RETURN_DUPLICATE**（409），
// 而且这个判断要排在 TranslateConstraint 前面 —— 那个函数把所有 23505 一律翻成
// CONFLICT 这种通用文案，用户看了不知道该怎么办（同 repo.Report.Create 的教训）。
//
// status 不在参数里：这一列在这里只有一个合法值 pending。
// owner_note / reviewer_id / review_kind / reviewed_at 同理全部不给调用方传的机会 ——
// 那是「提交人自己把自己的确认标成已确认」这条攻击路径，接口里没有那一格，
// 就写不出那一行代码（定位原则 1：能不能做的事由接口形状决定，不由自觉决定）。
func (r *ItemReturn) Submit(ctx context.Context, p SubmitRow) (int64, time.Time, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("repo.ItemReturn.Submit 开事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		id          int64
		submittedAt time.Time
	)
	err = tx.QueryRow(ctx, `
		INSERT INTO item_returns (item_id, submitter_id, message, proof_image_path)
		VALUES ($1, $2, $3, $4)
		RETURNING id, submitted_at`,
		p.ItemID, p.SubmitterID, p.Message, p.ProofPath).Scan(&id, &submittedAt)
	if err != nil {
		var pge *pgconn.PgError
		if errors.As(err, &pge) && pge.Code == pgUniqueViolation && pge.ConstraintName == uqItemReturnsPending {
			return 0, time.Time{}, apperr.WrapMsg(err, apperr.CodeReturnDuplicate,
				"你已经提交过一条待处理的归还确认，等发帖人处理完才能再提")
		}
		if ae := TranslateConstraint(err); ae != nil {
			return 0, time.Time{}, ae
		}
		return 0, time.Time{}, fmt.Errorf("repo.ItemReturn.Submit 插记录 (item=%d, submitter=%d): %w",
			p.ItemID, p.SubmitterID, err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO notifications (user_id, type, title, content, item_id, return_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		p.NotifyTo, model.NotificationReturnSubmitted, p.NotifyTitle, p.NotifyBody, p.ItemID, id); err != nil {
		if ae := TranslateConstraint(err); ae != nil {
			return 0, time.Time{}, ae
		}
		return 0, time.Time{}, fmt.Errorf("repo.ItemReturn.Submit 写通知 (item=%d, 发帖人=%d): %w",
			p.ItemID, p.NotifyTo, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, time.Time{}, fmt.Errorf("repo.ItemReturn.Submit 提交事务: %w", err)
	}
	return id, submittedAt, nil
}

// DecideRow 是 confirm / reject 两个动作共用的参数。
//
// OwnerID 由 service 传（它已经把 actor 和 item.user_id 比过了），repo 这里同时把它
// **写成 reviewer_id** 并且**当 WHERE 条件用**（那句 EXISTS）。第二重身份是数据层防线：
// 万一 service 漏了某条鉴权分支，一个非发帖人的请求会撞成「0 行受影响」，
// 表现成 RETURN_ILLEGAL_TRANSITION 而不是悄悄改掉别人的记录。
// 报错的类别不算精确，但**数据不会被写错**，这是这种双层检查的取舍。
//
// ⚠ 那句 EXISTS 读 items 但不写 items —— 「读作者的 id 来验证操作者就是作者」
// 和「把帖子状态改掉」是两件完全不同的事，前者是检查，后者才是 §3.4 禁的那一样。
type DecideRow struct {
	ID        int64
	OwnerID   int64
	OwnerNote string

	// 给提交人的那条通知（return_confirmed / return_rejected）
	NotifyTitle string
	NotifyBody  string
}

// HintRow 是一条 item_returned_hint 的收件人。
//
// 只有 user_id 和文案，没有「哪条 lost 帖」：这条通知说的是「**这条 found 帖**已被确认归还」，
// 点进去应该看到那条已经物归原主的拾物帖，所以 notifications.item_id 一律填 found 帖 id。
// 一个作者即使有两条 lost 帖都和这条 found 帖配过对，也只会收到一条（service 那边按作者去重）。
type HintRow struct {
	UserID int64
	Title  string
	Body   string
}

// ConfirmResult 是 confirm 这一次事务实际发生的事，字段全部来自数据库而不是入参。
//
// ItemID / SubmitterID 来自 UPDATE ... RETURNING：它们是**这一行真实记录的**归属，
// 不是 service 猜的。并发场景下（发帖人连点两次确认）第二次会撞 0 行直接报错，
// 所以永远不会有「用第一次的 id 去加第二次的分」这种错位。
type ConfirmResult struct {
	ItemID         int64
	SubmitterID    int64
	ReviewedAt     time.Time
	ItemClosed     bool // 这次真的把 open 变成 closed 了吗（已经是 closed/deleted 时为 false）
	OwnerScore     int  // 拾主确认后的信用分
	SubmitterScore int  // 提交人确认后的信用分
	// 两条流水各自的**实际生效**增量：分数夹到 200 顶时可能是 0 而不是 10。
	OwnerDelta     int
	SubmitterDelta int
}

// Confirm 推进 pending → confirmed，并在同一个事务里做完四件连带的事：
// 关帖、两边加分（含两条流水）、给提交人发 return_confirmed、给台账里的 lost 作者发 item_returned_hint。
//
// hints 由 service 从匹配台账读出来再传进来（repo 不去猜谁配过对），
// 这个分工和 M3 的 RecordMatches 完全一致：**service 决定发给谁、说什么，
// repo 只保证这一整批写下去要么全成要么全无**。
//
// 顺序是刻意排的：
//
//	① 带 status='pending' 的 UPDATE —— 这一步就是原子状态检查，撞 0 行 = 已被别人处理过
//	② 关帖（只关 open 的那一条，见下面那条 WHERE）
//	③ 加分 + 流水
//	④ 通知
//
// ①放在最前是因为后面三步全都依赖「这一行确实从 pending 变成了 confirmed」这个事实。
// 反过来说：**一旦①成功，后面任何一步失败都会整批回滚**，不会出现
// 「状态改了但分没加」这种对不上账的中间态。
//
// ⚠ ②的 WHERE 带 `AND status = 'open'`。这不是幂等优化，是**不复活**：
// admin 已经软删（status='deleted'）的帖子，绝不能因为发帖人后来点了一次确认就回到广场上。
// 「admin 能销毁内容」（定位原则 5）如果会被一个普通用户的正常操作撤销，
// 那条边界就成了建议。closed 的帖子同样不需要再关一次，所以那个 WHERE 一并挡掉。
// 无论②改没改到行，①③④ 都照常发生 —— 「发帖人确认了归还」这件事是真的，
// 它不取决于那条帖子现在挂什么状态。
func (r *ItemReturn) Confirm(ctx context.Context, d DecideRow, hints []HintRow) (*ConfirmResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("repo.ItemReturn.Confirm 开事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		itemID      int64
		submitterID int64
		reviewedAt  time.Time
	)
	err = tx.QueryRow(ctx, `
		UPDATE item_returns
		   SET status = 'confirmed', owner_note = $2, reviewer_id = $3,
		       review_kind = 'owner', reviewed_at = now()
		 WHERE id = $1 AND status = 'pending'
		   AND EXISTS (SELECT 1 FROM items i WHERE i.id = item_returns.item_id AND i.user_id = $3)
		RETURNING item_id, submitter_id, reviewed_at`,
		d.ID, d.OwnerNote, d.OwnerID).Scan(&itemID, &submitterID, &reviewedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, illegalTransition()
	}
	if err != nil {
		if ae := TranslateConstraint(err); ae != nil {
			return nil, ae
		}
		return nil, fmt.Errorf("repo.ItemReturn.Confirm 推进状态 (id=%d): %w", d.ID, err)
	}

	res := &ConfirmResult{ItemID: itemID, SubmitterID: submitterID, ReviewedAt: reviewedAt}

	tag, err := tx.Exec(ctx, `UPDATE items SET status = 'closed', updated_at = now()
		WHERE id = $1 AND status = 'open'`, itemID)
	if err != nil {
		if ae := TranslateConstraint(err); ae != nil {
			return nil, ae
		}
		return nil, fmt.Errorf("repo.ItemReturn.Confirm 关帖 (item=%d): %w", itemID, err)
	}
	res.ItemClosed = tag.RowsAffected() > 0

	// 拾主 +10、提交人 +2。两次调用之间不并发写同一行（两个 id 必然不同：
	// RETURN_SELF 早就挡住了「对自己的帖子提交确认」）。
	res.OwnerScore, res.OwnerDelta, err = applyCredit(ctx, tx, d.OwnerID,
		model.CreditDeltaReturnOwner, model.CreditReasonReturnOwner, d.ID)
	if err != nil {
		return nil, err
	}
	res.SubmitterScore, res.SubmitterDelta, err = applyCredit(ctx, tx, submitterID,
		model.CreditDeltaReturnSubmitter, model.CreditReasonReturnSubmitter, d.ID)
	if err != nil {
		return nil, err
	}

	// 给提交人的那条。收件人取 RETURNING 里的 submitter_id 而不是入参：
	// 那是这一行真实记录的提交人，而「确认通过了」这件事只对当事人有意义。
	if _, err := tx.Exec(ctx, `
		INSERT INTO notifications (user_id, type, title, content, item_id, return_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		submitterID, model.NotificationReturnConfirmed, d.NotifyTitle, d.NotifyBody, itemID, d.ID); err != nil {
		if ae := TranslateConstraint(err); ae != nil {
			return nil, ae
		}
		return nil, fmt.Errorf("repo.ItemReturn.Confirm 写 return_confirmed (提交人=%d): %w", submitterID, err)
	}

	// 台账里的那些 lost 作者：他们当初被 new_match 叫醒，现在给他们一个结束。
	// 这一批和上面那条在同一个事务里，所以不存在「拾主这边已经确认、
	// 失主那边还挂着一条 active 匹配」的中间态 —— 这就是 item_returned_hint
	// 必须住在 confirm 事务里而不是一个异步任务里的全部理由。
	for _, h := range hints {
		if _, err := tx.Exec(ctx, `
			INSERT INTO notifications (user_id, type, title, content, item_id, return_id)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			h.UserID, model.NotificationItemReturnedHint, h.Title, h.Body, itemID, d.ID); err != nil {
			if ae := TranslateConstraint(err); ae != nil {
				return nil, ae
			}
			return nil, fmt.Errorf("repo.ItemReturn.Confirm 写 item_returned_hint (lost 作者=%d): %w", h.UserID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("repo.ItemReturn.Confirm 提交事务: %w", err)
	}
	return res, nil
}

// applyCredit 给一个人加（减）分并插一条流水，返回新分数和**实际生效**的增量。
//
// 三步而不是「一条 UPDATE 解决」的原因在流水那一行：
// credit_logs.delta 必须记实际生效的值，否则「Σ流水 + 100 == credit_score」这条
// 唯一的对账性质会在分数夹到 200 顶时失效（名义 +10、实际 +0）。
// 要拿到实际增量就必须先知道旧值，而旧值只能在同一个事务里带锁读出来 ——
// SELECT ... FOR UPDATE 顺带把两次并发确认串起来，
// 不然两个 confirm 同时读 195 会双双写回 200 却各记一条 +10。
//
// UPDATE 那句是计划 §3.6 的原文（LEAST/GREATEST 夹住 0..200），一个字没改。
// 边界值从 model.CreditScoreBounds() 取而不是抄数字：CHECK 约束只有一处定义。
func applyCredit(ctx context.Context, tx pgx.Tx, userID int64, delta int, reason string, refID int64) (int, int, error) {
	minScore, maxScore := model.CreditScoreBounds()

	var old int
	if err := tx.QueryRow(ctx, `SELECT credit_score FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&old); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, apperr.Internal(fmt.Errorf("repo.applyCredit: 用户 %d 不存在，无法加分 (reason=%s)", userID, reason))
		}
		return 0, 0, fmt.Errorf("repo.applyCredit 加锁读分数 (user=%d): %w", userID, err)
	}

	var next int
	if err := tx.QueryRow(ctx, `
		UPDATE users SET credit_score = LEAST($2, GREATEST($3, credit_score + $1)) WHERE id = $4
		RETURNING credit_score`, delta, maxScore, minScore, userID).Scan(&next); err != nil {
		if ae := TranslateConstraint(err); ae != nil {
			return 0, 0, ae
		}
		return 0, 0, fmt.Errorf("repo.applyCredit 写分数 (user=%d, delta=%d): %w", userID, delta, err)
	}

	effective := next - old
	if _, err := tx.Exec(ctx, `
		INSERT INTO credit_logs (user_id, delta, reason, ref_type, ref_id)
		VALUES ($1, $2, $3, $4, $5)`,
		userID, effective, reason, model.CreditRefTypeReturn, refID); err != nil {
		if ae := TranslateConstraint(err); ae != nil {
			return 0, 0, ae
		}
		return 0, 0, fmt.Errorf("repo.applyCredit 写流水 (user=%d, reason=%s): %w", userID, reason, err)
	}
	return next, effective, nil
}

// Reject 推进 pending → rejected。
//
// ### ⚠ 这个方法里只允许有两条 SQL，改动前先读完这两条
//
//	Update item_returns  —— 改这一行的状态、发帖人的留言、归属两列
//	INSERT notifications —— 给提交人一条 return_rejected
//
// 它**没有** UPDATE items、**没有** UPDATE users、**没有** INSERT credit_logs。
// （UPDATE 的 WHERE 里那句 EXISTS 是**读** items 来验证操作者就是作者，
// 一次读、零次写 —— 读不违反任何禁令，写才违反。）
// 计划 §3.4 那句话是这一整个里程碑最容易实现错的一处：
// 「rejected 拒绝的是这条归还确认记录，绝对不是这条 found 帖」。
// 拒绝之后帖子仍然 open、继续展示、继续参与匹配、继续接受别人提交新的确认，
// 而且**不扣分**（罚了就等于逼发帖人不敢拒绝，而拒绝假提交是这套设计的承重墙）。
//
// M5 有三道锁守住这件事：service 层的表驱动单测（fake 会记下每一次方法调用，
// 多调一次 UPDATE items 就红）、集成测试 TestRejectDoesNotCloseItem
// （reject 完立刻 SELECT status FROM items）、以及 smoke.sh 里那条 psql 前后差值断言。
func (r *ItemReturn) Reject(ctx context.Context, d DecideRow) (*DecideResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("repo.ItemReturn.Reject 开事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		itemID      int64
		submitterID int64
		reviewedAt  time.Time
	)
	err = tx.QueryRow(ctx, `
		UPDATE item_returns
		   SET status = 'rejected', owner_note = $2, reviewer_id = $3,
		       review_kind = 'owner', reviewed_at = now()
		 WHERE id = $1 AND status = 'pending'
		   AND EXISTS (SELECT 1 FROM items i WHERE i.id = item_returns.item_id AND i.user_id = $3)
		RETURNING item_id, submitter_id, reviewed_at`,
		d.ID, d.OwnerNote, d.OwnerID).Scan(&itemID, &submitterID, &reviewedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, illegalTransition()
	}
	if err != nil {
		if ae := TranslateConstraint(err); ae != nil {
			return nil, ae
		}
		return nil, fmt.Errorf("repo.ItemReturn.Reject 推进状态 (id=%d): %w", d.ID, err)
	}

	// 收件人取 RETURNING 里的 submitter_id，不取入参：这条通知只能给提交人，
	// 而从库里读出来的那个提交人才是事实本身。
	if _, err := tx.Exec(ctx, `
		INSERT INTO notifications (user_id, type, title, content, item_id, return_id)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		submitterID, model.NotificationReturnRejected, d.NotifyTitle, d.NotifyBody, itemID, d.ID); err != nil {
		if ae := TranslateConstraint(err); ae != nil {
			return nil, ae
		}
		return nil, fmt.Errorf("repo.ItemReturn.Reject 写通知 (提交人=%d): %w", submitterID, err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("repo.ItemReturn.Reject 提交事务: %w", err)
	}
	return &DecideResult{ItemID: itemID, SubmitterID: submitterID, ReviewedAt: reviewedAt}, nil
}

// DecideResult 是 reject / cancel 的结果。
//
// 它比 ConfirmResult 小得多，而这个「小」正是重点：rejected 和 cancelled 是**纯记录状态**，
// 除了这一行自己的变化以外，什么都不产生（ConfirmResult 有五个字段，那里有六个副作用）。
type DecideResult struct {
	ItemID      int64
	SubmitterID int64
	ReviewedAt  time.Time
}

// Cancel 由提交人自己撤销：一条 UPDATE，零副作用，**不发通知**。
//
// 为什么不发通知：那条 pending 本来只挂在发帖人的待办里，他还没处理，
// 现在撤销就等于「当我没提过」。给他发一条「某人撤销了一条你从没看过的确认」
// 是纯噪音（和 §16 删掉 contact_unlocked 是同一条判断标准：
// 不直接帮双方联系上的通知就不发）。
//
// ⚠ 没有 reviewer_id / review_kind：那是「谁审的」两列，而 cancelled 不是被审的，
// 是自己撤的（计划 §3.3 明写 cancelled 时看 submitter_id）。
// 但 reviewed_at 会填 —— 它的实际含义是「这条记录退出 pending 的时刻」，
// 而「谁做的、有没有社区含义」那两问仍然由 review_kind 为 NULL 来回答。
// 于是那条自检 SQL 依旧成立：
//
//	SELECT id FROM item_returns WHERE status IN ('confirmed','rejected') AND review_kind IS NULL
//
// cancel 不在 IN 列表里，所以它不会被误判成「管理员绕过 service 改的行」。
func (r *ItemReturn) Cancel(ctx context.Context, id, submitterID int64) (*DecideResult, error) {
	var (
		itemID     int64
		reviewedAt time.Time
	)
	err := r.pool.QueryRow(ctx, `
		UPDATE item_returns
		   SET status = 'cancelled', reviewed_at = now()
		 WHERE id = $1 AND status = 'pending' AND submitter_id = $2
		RETURNING item_id, reviewed_at`,
		id, submitterID).Scan(&itemID, &reviewedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, illegalTransition()
	}
	if err != nil {
		if ae := TranslateConstraint(err); ae != nil {
			return nil, ae
		}
		return nil, fmt.Errorf("repo.ItemReturn.Cancel (id=%d, submitter=%d): %w", id, submitterID, err)
	}
	return &DecideResult{ItemID: itemID, SubmitterID: submitterID, ReviewedAt: reviewedAt}, nil
}

// illegalTransition 是「0 行受影响」的统一解释。
//
// WHERE 里那三个条件（id / status='pending' / 归属）任何一个不成立都会走到这里，
// 而 service 已经先读过一次行、判过权限和状态，所以到这一步只剩两种可能：
// 要么状态刚被别人改过（并发双点），要么归属不成立。两种都是「当前状态不允许这个操作」，
// 不需要更细的码 —— 用户重新拉一次列表就会看到真实状态。
// 这里刻意**不**返回 500：那是并发，不是故障。
func illegalTransition() error {
	return apperr.New(apperr.CodeReturnIllegalTransition)
}

// ReturnListFilter 是 #28/#29 的查询条件。
//
// 两个列表**共用一套 WHERE 拼接**，只有列名不同：SubmitterID 非 0 就筛提交人，
// OwnerID 非 0 就筛「我发的帖子收到的确认」。把它们做成一个 filter 结构而不是
// 两个方法的两套 SQL，是为了让「分页、状态筛选、排序」这三件事只有一份实现 ——
// 列表行为不一致是很容易漏测的那类 bug。
type ReturnListFilter struct {
	SubmitterID int64
	OwnerID     int64
	Status      *string
	Page        int
	PageSize    int
}

// returnListCols / returnListFrom 是 #28/#29 的连表清单。
//
// 列顺序必须和 scanReturnListRow 严格对应（同 itemDetailCols 那条纪律）。
// 别名 i / c / l / u / cover 故意和 itemDetailFrom 一致：那套 JOIN 的每一个理由
// （三个 INNER 安全、封面图用 LATERAL 免掉 N+1）在这里同样成立，重读一遍没有意义。
//
// 和 itemDetailCols 的区别是**少了** description、四个祖先 id、is_freeform、
// view_count、updated_at —— 列表不返回它们（ItemSummary 的形状决定的），
// 而祖先三列只为匹配算法存在。宁可多列 15 个字段，也不要交出一个半填的 ItemDetail。
const returnListCols = `r.id, r.status, r.owner_note, r.submitted_at, r.reviewed_at,
	i.id, i.item_type, i.user_id, i.title, i.status,
	i.category_id, c.name, i.location_id, l.name,
	i.lost_at, i.found_at, i.contact, COALESCE(cover.path, ''),
	u.nickname, i.created_at`

const returnListFrom = ` FROM item_returns r
	 JOIN items i      ON i.id = r.item_id
	 JOIN categories c ON c.id = i.category_id
	 JOIN locations  l ON l.id = i.location_id
	 JOIN users      u ON u.id = i.user_id
	 LEFT JOIN LATERAL (
	     SELECT path FROM item_images im
	      WHERE im.item_id = i.id
	      ORDER BY im.sort_order, im.id
	      LIMIT 1
	 ) cover ON true`

func scanReturnListRow(row pgx.Row, v *model.ItemReturnRow) error {
	return row.Scan(
		&v.ID, &v.Status, &v.OwnerNote, &v.SubmittedAt, &v.ReviewedAt,
		&v.ItemID, &v.ItemType, &v.AuthorID, &v.Title, &v.ItemStatus,
		&v.CategoryID, &v.CategoryName, &v.LocationID, &v.LocationName,
		&v.LostAt, &v.FoundAt, &v.Contact, &v.CoverPath,
		&v.AuthorName, &v.ItemCreatedAt,
	)
}

// ListReturns 按条件分页取归还确认。#28（我提交的）和 #29（我收到的）都走这一个方法。
//
// 排序是 submitted_at DESC：**提交时间**而不是 review 时间。
// 理由有两个 —— 索引（idx_item_returns_submitter / idx_item_returns_item 都是行级索引，
// 排序键用哪个都一样要 sort，但 submitted_at 对两张表都成立）；
// 更重要的是「最新有人提交了确认」是这两个列表唯一想置顶的事件，
// 而 reviewed_at 对 pending 行是 NULL，PG 的 DESC 会把 NULL 排在最前，
// 于是「还没处理的」会全部糊在第一屏 —— 看起来像排序坏了，代码却完全正确。
//
// ⚠ WHERE 里的归属条件（r.submitter_id / i.user_id）是必加的，不是可选筛选。
// 它永远来自 JWT 而不是请求参数，少一句就是「任何人可读全站所有人的归还确认」。
func (r *ItemReturn) ListReturns(ctx context.Context, f ReturnListFilter) ([]model.ItemReturnRow, int, error) {
	var (
		conds = []string{}
		args  = []any{}
	)
	if f.SubmitterID != 0 {
		args = append(args, f.SubmitterID)
		conds = append(conds, fmt.Sprintf(`r.submitter_id = $%d`, len(args)))
	}
	if f.OwnerID != 0 {
		args = append(args, f.OwnerID)
		conds = append(conds, fmt.Sprintf(`i.user_id = $%d`, len(args)))
	}
	if f.Status != nil {
		args = append(args, *f.Status)
		conds = append(conds, fmt.Sprintf(`r.status = $%d`, len(args)))
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	var total int
	if err := r.pool.QueryRow(ctx,
		`SELECT count(*)`+returnListFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("repo.ItemReturn.ListReturns 计数 (submitter=%d, owner=%d): %w",
			f.SubmitterID, f.OwnerID, err)
	}

	limitIdx, offsetIdx := len(args)+1, len(args)+2
	args = append(args, f.PageSize, (f.Page-1)*f.PageSize)

	rows, err := r.pool.Query(ctx, fmt.Sprintf(
		`SELECT %s%s ORDER BY r.submitted_at DESC, r.id DESC LIMIT $%d OFFSET $%d`,
		returnListCols, returnListFrom+where, limitIdx, offsetIdx), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("repo.ItemReturn.ListReturns (submitter=%d, owner=%d): %w",
			f.SubmitterID, f.OwnerID, err)
	}
	defer rows.Close()

	out := []model.ItemReturnRow{}
	for rows.Next() {
		var v model.ItemReturnRow
		if err := scanReturnListRow(rows, &v); err != nil {
			return nil, 0, fmt.Errorf("repo.ItemReturn.ListReturns 扫描一行: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("repo.ItemReturn.ListReturns 迭代: %w", err)
	}
	return out, total, nil
}

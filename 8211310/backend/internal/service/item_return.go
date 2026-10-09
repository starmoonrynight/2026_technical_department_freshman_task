package service

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
	"lostfound/internal/repo"
)

// 这个文件是 M5 的全部业务规则：**归还确认的状态机，和它唯一的一次连带后果**。
//
// 它是全项目最短的一条正向链路（双方线下归还 → 事后记一笔），
// 也是最容易「顺手多加一道」的一条。所以先写清楚这里**没有**什么：
//
//  1. **没有前置门槛**。提交归还确认不需要先解锁联系方式、不需要和这条帖子匹配过、
//     不需要任何历史记录。§16 把 `RETURN_NOT_UNLOCKED` 整个删掉了而不是放宽：
//     它是平台强加的流程门槛，和「全凭自觉」矛盾，而且会卡住线下先认识、
//     从没在平台上解锁过的双方。所以这个 struct 的依赖里**没有 ContactViewLookup**——
//     想加那道校验，先得给它接上一个能读 contact_views 的口子，
//     而那一步会在这个文件里留下一个显眼的、需要解释的新依赖。
//     提交的前置条件只有三条，全在下面：帖子存在（且没被软删）+ open + 不是自己的帖子。
//
//  2. **平台不裁决**。谁提交都不拦，但判断真假的权力 100% 在发帖人手里：
//     只有他能 confirm / reject，而且 admin 也不行（看下面 authorizeReturnDecision）。
//     平台为此做的唯一一件事是让发帖人有足够信息自己判断——
//     文字、凭证图、提交人昵称和信用分，全都排在 #24 那一页里。
//
//  3. **拒绝不产生后果**。rejected 只改那一行记录 + 给提交人一条通知，
//     帖子仍然 open、不扣分、不阻止别人重提。这是 §3.4 那句
//     「全系统最容易误解的一点」，三道测试锁着它。
//
// 反过来，confirm 是**全项目副作用最大的一个用户动作**：一次调用连带
// 关帖、两处加分、两条流水、一到多条通知。所以那一整批写在同一个事务里
// （见 repo/item_return.go 的 Confirm），这里只负责决定「给谁、说什么」。

// ---------- 接缝 ----------

// ReturnStore 是归还确认需要的全部持久化能力：六个动作，没有一个是通用 setter。
//
// 接口里没有 SetStatus、没有 CloseItem、没有 AddCredit ——
// 「推进状态」这四条路（submit / confirm / reject / cancel）之外的写法在这里根本不存在，
// 所以「提交人自己把自己的确认标成已确认」这种代码在类型层面就写不出来
// （那些连带后果全都住在 repo.Confirm 内部，而它只认 pending）。
type ReturnStore interface {
	Submit(ctx context.Context, p repo.SubmitRow) (int64, time.Time, error)
	GetByID(ctx context.Context, id int64) (*model.ItemReturn, error)
	Confirm(ctx context.Context, d repo.DecideRow, hints []repo.HintRow) (*repo.ConfirmResult, error)
	Reject(ctx context.Context, d repo.DecideRow) (*repo.DecideResult, error)
	Cancel(ctx context.Context, id, submitterID int64) (*repo.DecideResult, error)
	ListReturns(ctx context.Context, f repo.ReturnListFilter) ([]model.ItemReturnRow, int, error)
}

// LedgerLookup 是 confirm 那一步需要的台账反查（只读）。
//
// 单独一个小接口而不是把 *repo.Match 塞进来：Match 有 Candidates / RecordMatches
// 两个写读混合的能力，而归还确认只需要「当初谁被通知过」这一问。
// 一个能写台账的依赖出现在归还服务里，是一种它本不该有的可能性（同 ContactViewLookup 那条理由）。
type LedgerLookup interface {
	LedgerLostAuthors(ctx context.Context, foundItemID int64) ([]int64, error)
}

// UserLookup 是 #24 取「提交人昵称 + 信用分」需要的能力。
//
// ⚠ 只有读，没有任何写分的方法。加分唯一的路径是 repo.Confirm 那个事务内部的
// applyCredit（见 repo/credit.go 顶部：这里为什么连一个 Insert 都没有）。
type UserLookup interface {
	GetByID(ctx context.Context, id int64) (*model.User, error)
}

// ItemReturn 是归还确认的业务规则。
type ItemReturn struct {
	returns ReturnStore
	items   ItemLookup
	ledger  LedgerLookup
	users   UserLookup
	uploads *Upload
	logger  *slog.Logger
}

func NewItemReturn(returns ReturnStore, items ItemLookup, ledger LedgerLookup,
	users UserLookup, uploads *Upload, logger *slog.Logger) *ItemReturn {
	if logger == nil {
		logger = slog.Default()
	}
	return &ItemReturn{
		returns: returns, items: items, ledger: ledger, users: users,
		uploads: uploads, logger: logger,
	}
}

// ---------- 状态机（纯函数，第①层表驱动单测覆盖全部 from×action 组合）----------

// 三个动作的内部名字。它们只在这个包里出现，**不出现在任何 JSON 或 SQL 里**：
// 对外的写法是 HTTP 路径（/confirm、/reject、/cancel），对内的这一层只是查表的键。
const (
	actionConfirm = "confirm"
	actionReject  = "reject"
	actionCancel  = "cancel"
)

// legalTransitions 是计划 §3.4 那张状态机图的代码形式：
//
//	pending ──发帖人本人──▶ confirmed
//	pending ──发帖人本人──▶ rejected
//	pending ──提交人本人──▶ cancelled
//
// 仅此三条。三个终态在表里**连键都没有**（不是空 map，是整个不出现），
// 所以任何「从 confirmed 回到 pending」和「confirmed 转 rejected」都查不到 → 非法。
//
// 为什么用查表而不是三个 if：
//   - §12 的判据要求「状态机表驱动单测覆盖全部 from×to 组合」，
//     一张表让那 4×3=12 个组合变成 12 行表数据，加一个状态就多一行；
//   - 更要紧的是，将来谁加第五个状态时**必须显式决定它有没有出边**，
//     而不是在某个 if 链里漏掉一条分支，漏掉的分支表现成「这个操作偶尔 409」。
//
// ⚠ 表里的目标状态和 repo 那三条 SQL 里硬写的 'confirmed'/'rejected'/'cancelled'
// 是同一件事的两处写法。这不冗余而是分工：表决定**让不让做**，SQL 决定**落成什么**。
// 它们不一致由集成测试兜住（每个动作之后都会回读 item_returns.status）。
var legalTransitions = map[string]map[string]string{
	model.ReturnStatusPending: {
		actionConfirm: model.ReturnStatusConfirmed,
		actionReject:  model.ReturnStatusRejected,
		actionCancel:  model.ReturnStatusCancelled,
	},
}

// checkTransition 查「当前状态 + 这个动作」合不合法，合法时给出目标状态。
//
// 读一个不存在的 from 键会得到 nil 内层 map，索引它返回零值和 false —— Go 的
// 「对 nil map 取值不 panic」在这里正好是想要的行为：三个终态和任何脏值一样走到非法分支。
func checkTransition(from, action string) (to string, ok bool) {
	return legalTransitions[from][action], legalTransitions[from][action] != ""
}

// authorizeReturnDecision 判「这个人能不能做这个动作」。
//
// 两条规则，**只看 id 相等，一次都没有读 role**：
//   - confirm / reject → 必须是那条帖子的发帖人（itemOwnerID）
//   - cancel           → 必须是那条确认的提交人（submitterID）
//
// 「admin 也不行」不是这条代码里的一个特判，而是这条规则的自然结果：
// admin 的 user_id 不等于发帖人的 user_id。这个区别很重要 ——
// 代码里不存在 `if actor.IsAdmin()` 这样的分支，所以将来谁想「给管理员开个方便」，
// 必须改动这个纯函数并让它多一个参数，而那会撞上全部第①层测试和第②层那两条
// admin 遍历路由得 FORBIDDEN 的集成测试，而不是顺手在这里加一个 `||`。
//
// 这是定位原则 5 的代码体现：admin 能销毁内容和账号，但**制造不出归属关系**，
// 因为「发帖人同意了」这件事只能由发帖人本人通过这个函数。
func authorizeReturnDecision(action string, actorID, itemOwnerID, submitterID int64) error {
	switch action {
	case actionConfirm, actionReject:
		if actorID != itemOwnerID {
			return apperr.Forbidden("只有那条拾物帖的发帖人能处理归还确认")
		}
		return nil
	case actionCancel:
		if actorID != submitterID {
			return apperr.Forbidden("只有提交人能撤销自己提交的归还确认")
		}
		return nil
	default:
		// 走到这里只有一个可能：本文件调用时写错了一个 action 常量。
		// 那是编程错误而不是用户输入，所以它是 INTERNAL 而不是 VALIDATION
		// （用户改不了任何东西，而这条路径正常情况永远走不到）。
		return apperr.Internal(fmt.Errorf("service.ItemReturn: 未知的归还确认动作 %q", action))
	}
}

// ---------- #23 提交归还确认 ----------

// SubmitReturnInput 是 #23 POST /api/items/:id/returns 的输入。
type SubmitReturnInput struct {
	Message        string
	ProofImagePath string
}

// SubmitReturnResult 是 #23 的 data（计划 §4 第 23 行：{id, item_id, status, submitted_at}）。
//
// Status 恒为 "pending" —— 它是「这条记录刚被插进待处理队列」这个事实。
// 返回它不是为了让用户读出一个未知值，而是让前端能直接把它插进 #28 的列表里
// 而不用重新拉一次列表。
type SubmitReturnResult struct {
	ID          int64  `json:"id"`
	ItemID      int64  `json:"item_id"`
	Status      string `json:"status"`
	SubmittedAt string `json:"submitted_at"`
}

// Submit 提交一条归还确认。
//
// 判断顺序（每一步都有理由，改动前读完这段）：
//
//	① message 长度、proof 路径形状 → VALIDATION
//	② 帖子不存在 / 已软删          → NOT_FOUND
//	③ lost 帖                     → VALIDATION
//	④ 自己的帖子                   → RETURN_SELF
//	⑤ 不是 open                   → ITEM_CLOSED
//	⑥ 插一行 pending + 一条通知（同一事务）
//
// ①排在所有查库之前，理由和 #41 举报完全一样：**字段错了应该先说字段错了**，
// 那是用户当下能改的东西，而「帖子不存在」他改不了。顺带的好处是一个
// 连 message 都没填对的请求根本不会去查库。
//
// ③排在④前面：对 lost 帖提交归还确认这件事**根本不成立**（item_returns 只服务 found 帖），
// 比「这是你自己的帖子所以不行」更接近问题的本质。
// 判断顺序错了的症状很具体：小王对自己那条 lost 帖点归还确认，
// 会看到「不能对自己的帖子提交归还确认」，然后困惑地去发一条 found 帖试试 ——
// 那才是真正的泄漏。
//
// ④排在⑤前面：一个 closed 的**自己的**帖子，RETURN_SELF 比 ITEM_CLOSED 更准。
// 这条顺序和 #21 解锁的 ③④ 完全同构（那边是「作者本人直接给」排在「closed 拦」前面）。
//
// ⑤是最后一道：**closed 的帖子不收新的归还确认**。
// 东西已经还回去了（或者发帖人自己点了已找到），再来一条 pending 只会挂在
// 一个没有人会去看列表的人身上。撞 ITEM_CLOSED 的文案「这条帖子已关闭」是看得懂的。
//
// ⚠ 这里**没有**「必须先解锁过联系方式」这一条。§16 已经把那个校验整个删掉了，
// 而且 §12 的 M5 判据里第一条冒烟就是「从没解锁过的用户直接提交归还确认 → 成功」。
// 少这一句注释，将来一定有人说「乱提交怎么办」并把那道门槛加回来。
// 防乱提交靠的是三条弱得多的机制（部分唯一索引、凭证图必填、rejected 不加分），
// 而真正的防线是发帖人自己的判断 —— 平台不裁决，所以平台也不负责挡住假提交。
func (s *ItemReturn) Submit(ctx context.Context, submitter *model.User, itemID int64,
	in SubmitReturnInput) (*SubmitReturnResult, error) {

	message := strings.TrimSpace(in.Message)
	if n := utf8.RuneCountInString(message); n < model.ReturnMessageMinChars || n > model.ReturnMessageMaxChars {
		return nil, apperr.Validation(
			fmt.Sprintf("归还说明长度必须是 %d–%d 个字，当前 %d 个（一个汉字算 1 个字）",
				model.ReturnMessageMinChars, model.ReturnMessageMaxChars, n),
			apperr.FieldError{Field: "message",
				Msg: fmt.Sprintf("%d–%d 个字", model.ReturnMessageMinChars, model.ReturnMessageMaxChars)})
	}

	// 凭证图必填（§16.2 那条「不表态就按必填实现」就是必填）。
	// 它是唯一能把线下那次交接和平台这条帖子对上号的客观证据，
	// 而整个 credit_score 体系就挂在这一步上 —— 这里松了，系统就没有任何硬东西了。
	//
	// ⚠ 必须过 IsUploadPath：这个值来自请求体，而它将来会被拼成 #24 的 proof_image_url。
	// 不校验形状就等于允许客户端塞进任意字符串（绝对磁盘路径、别人的 URL、
	// 带 ../ 的相对路径），而 #13 的图片路径校验早就是这么写的 —— 复用同一个函数，
	// 就只有一份白名单要维护。
	proof := strings.TrimSpace(in.ProofImagePath)
	if proof == "" {
		return nil, apperr.Validation("必须上传一张归还凭证图",
			apperr.FieldError{Field: "proof_image_path",
				Msg: "请把 POST /api/uploads 返回的 path 原样传回来"})
	}
	if !IsUploadPath(proof) {
		return nil, apperr.Validation("凭证图路径不合法",
			apperr.FieldError{Field: "proof_image_path",
				Msg: "必须是 POST /api/uploads 返回的 path 原样传回来"})
	}

	d, err := s.items.GetByID(ctx, itemID)
	if err != nil {
		return nil, err
	}
	if d.Status == model.ItemStatusDeleted {
		return nil, apperr.NotFound("帖子")
	}
	if d.ItemType == model.ItemTypeLost {
		return nil, apperr.Validation("失物帖不需要归还确认",
			apperr.FieldError{Field: "id",
				Msg: "归还确认只在别人捡到你东西的那条拾物帖下进行；失物帖找回之后请到「我的发布」点「已找到」"})
	}
	if submitter.ID == d.UserID {
		return nil, apperr.New(apperr.CodeReturnSelf)
	}
	if d.Status != model.ItemStatusOpen {
		return nil, apperr.New(apperr.CodeItemClosed)
	}

	title, body := returnSubmittedNotice(d, submitter.Nickname, message)
	id, submittedAt, err := s.returns.Submit(ctx, repo.SubmitRow{
		ItemID:      d.ID,
		SubmitterID: submitter.ID,
		Message:     message,
		ProofPath:   proof,
		NotifyTo:    d.UserID,
		NotifyTitle: title,
		NotifyBody:  body,
	})
	if err != nil {
		return nil, err
	}

	// 日志里带上帖主 id：这条是事后追查「谁在我的帖子上刷过归还确认」的唯一索引，
	// 而 notifications 表按设计不会给被举报/被骚扰的人发任何东西。
	s.logger.InfoContext(ctx, "return.submitted",
		slog.Int64("return_id", id),
		slog.Int64("item_id", d.ID),
		slog.Int64("submitter_id", submitter.ID),
		slog.Int64("owner_id", d.UserID),
	)

	return &SubmitReturnResult{
		ID:          id,
		ItemID:      d.ID,
		Status:      model.ReturnStatusPending,
		SubmittedAt: submittedAt.UTC().Format(time.RFC3339),
	}, nil
}

// ---------- #24 归还确认详情 ----------

// Detail 取一条归还确认，只有三方看得见：提交人、发帖人、admin（计划 §4 第 24 行的鉴权列）。
//
// 判断顺序：① 记录不存在 → NOT_FOUND；② 三方之外 → FORBIDDEN。
// FORBIDDEN 而不是 NOT_FOUND 的取舍和 #22 名单一样：藏起来的代价是
// 提交人拼错一个 id 会收到「记录不存在」而明明是没权限，
// 他会去翻自己到底提没提过，而真正的原因一眼就能告诉他。
//
// ⚠ 这条记录指向的帖子**即使已被软删也照样返回**（摘要里 status 就是 "deleted"）。
// 理由：#24 说的是「这条归还确认记录」，而记录是事实（定位原则 1）。
// admin 下架那条帖子不会让「某人某天提交过一条归还确认、发帖人后来拒绝了」这件事消失；
// 如果这里返回 404，提交人会看到自己的记录凭空蒸发，
// 那恰好是治理场景里最难解释的一种「什么都没发生」。
func (s *ItemReturn) Detail(ctx context.Context, actor *model.User, returnID int64) (*model.ReturnDetailView, error) {
	r, err := s.returns.GetByID(ctx, returnID)
	if err != nil {
		return nil, err
	}

	d, err := s.items.GetByID(ctx, r.ItemID)
	if err != nil {
		return nil, err
	}

	if actor.ID != r.SubmitterID && actor.ID != d.UserID && !actor.IsAdmin() {
		return nil, apperr.Forbidden("只有提交人、发帖人和管理员可以查看这条归还确认")
	}

	submitter, err := s.users.GetByID(ctx, r.SubmitterID)
	if err != nil {
		return nil, err
	}

	view := r.View(
		summaryOfFound(d, s.uploads.URL(d.CoverPath)),
		model.SubmitterView{ID: submitter.ID, Nickname: submitter.Nickname, CreditScore: submitter.CreditScore},
		s.uploads.URL(r.ProofImagePath),
	)
	return &view, nil
}

// ---------- #25 / #26 发帖人的两个决定 ----------

// ConfirmReturnResult 是 #25 的 data（计划 §4 第 25 行）。
//
// CreditDelta 是**给操作者本人（拾主）实际生效**的增量，不是名义值：
// 分数已经 200 的人这里返回 0，而他自己的 #33 流水里那条也是 0 —— 两边必须对得上。
// 提交人那 +2 不在这个响应里（这个响应是给拾主看的回执，提交人的分数在他自己的收件箱和流水里）。
type ConfirmReturnResult struct {
	ID          int64  `json:"id"`
	Status      string `json:"status"`
	ReviewedAt  string `json:"reviewed_at"`
	CreditDelta int    `json:"credit_delta"`
}

// DecideReturnResult 是 #26 的 data（{id, status, reviewed_at}，没有 credit_delta：
// 拒绝不产生任何分数变化，连一个 0 都不该返回 —— 返回 0 会让人以为「本来要加分但没加」）。
type DecideReturnResult struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	ReviewedAt string `json:"reviewed_at"`
}

// Confirm 发帖人确认「东西确实还给我说的这位了」。
//
// 这是全项目唯一一次一个用户动作连带六件事：改记录、关帖、两处加分、两条流水、一到多条通知。
// 所以这里的顺序是「先把要判断的全判断完，最后一次性交给 repo 写」：
//
//	① 读记录（不存在 → NOT_FOUND）
//	② 读帖子（拿到 item.user_id 才能判权）
//	③ 权限：actor 必须就是发帖人 → FORBIDDEN（**admin 也在这里被挡掉**）
//	④ 状态：pending → confirmed 合法吗 → RETURN_ILLEGAL_TRANSITION
//	⑤ 可选的 owner_note：trim + 500 字上限 → VALIDATION
//	⑥ 读台账，拼出该收 item_returned_hint 的人和他们的通知文案
//	⑦ repo.Confirm（一个事务）
//
// ③排在④前面是本文件最重要的一条顺序，理由和 #16 的「权限判断永远走在字段校验前面」一样：
// 状态是**任何人**都能改变的吗？不是，只有发帖人能改。所以一个路人对着一条已经
// confirmed 的记录点确认，应当听到「你没权限」而不是「当前状态不允许此操作」——
// 后者向他确认了「这条记录存在、而且它现在长什么样」，那是他不需要知道的信息。
// 反过来说，发帖人自己重复点确认才会拿到 RETURN_ILLEGAL_TRANSITION，
// 而那个 409 对他是有用的（页面上那条已经处理过了，刷新一下）。
//
// ⑥的读失败会**整件事失败**（不降级成「只关帖不发提示」）：
// confirm 是一个人一辈子点不了几次的动作，宁可让他重试一次，
// 也不要留下一个「拾主这边确认完了、失主那边永远等不到收尾」的不对称状态 ——
// 而那种状态在数据库里看不出任何异常，只有当事人知道，这正是最难 debug 的一类 bug。
func (s *ItemReturn) Confirm(ctx context.Context, actor *model.User, returnID int64,
	ownerNote string) (*ConfirmReturnResult, error) {

	r, d, to, err := s.prepareDecision(ctx, actor, returnID, actionConfirm)
	if err != nil {
		return nil, err
	}

	note, err := s.normalizeOwnerNote(ownerNote, false)
	if err != nil {
		return nil, err
	}

	authorIDs, err := s.ledger.LedgerLostAuthors(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	hintTitle, hintBody := returnedHintNotice(d)
	hints := make([]repo.HintRow, 0, len(authorIDs))
	for _, uid := range authorIDs {
		hints = append(hints, repo.HintRow{UserID: uid, Title: hintTitle, Body: hintBody})
	}
	confirmedTitle, confirmedBody := returnConfirmedNotice(d)

	res, err := s.returns.Confirm(ctx, repo.DecideRow{
		ID:          r.ID,
		OwnerID:     actor.ID,
		OwnerNote:   note,
		NotifyTitle: confirmedTitle,
		NotifyBody:  confirmedBody,
	}, hints)
	if err != nil {
		return nil, err
	}

	s.logger.InfoContext(ctx, "return.confirmed",
		slog.Int64("return_id", r.ID),
		slog.Int64("item_id", res.ItemID),
		slog.Int64("owner_id", actor.ID),
		slog.Int64("submitter_id", res.SubmitterID),
		slog.Bool("item_closed", res.ItemClosed),
		slog.Int("owner_credit_delta", res.OwnerDelta),
		slog.Int("submitter_credit_delta", res.SubmitterDelta),
		slog.Int("hint_count", len(hints)),
	)

	return &ConfirmReturnResult{
		ID:          r.ID,
		Status:      to,
		ReviewedAt:  res.ReviewedAt.UTC().Format(time.RFC3339),
		CreditDelta: res.OwnerDelta,
	}, nil
}

// Reject 发帖人拒绝这条归还确认。
//
// 判断顺序和 Confirm 一模一样（①记录 ②帖子 ③权限 ④状态 ⑤理由 ⑥写），
// 差别只在**后果**：这个方法往下走的 repo.Reject 只有两条 SQL，
// 没有关帖、没有加分、没有流水、只有一条给提交人的通知。
//
// owner_note 在这里是**必填**的（#25 可选、#26 必填，这个不对称是刻意的）：
// 拒绝是一个坏消息，而提交人唯一能得到的解释就是这一句。
// 没有它，被拒绝的人只知道「发帖人不同意」，只能靠猜，
// 而猜不出来的人要么反复重提要么去问 —— 两种都是这套设计要避免的。
// 计划 §4 第 26 行给这个端点列了 VALIDATION，那条就是给「没写理由」留的。
func (s *ItemReturn) Reject(ctx context.Context, actor *model.User, returnID int64,
	ownerNote string) (*DecideReturnResult, error) {

	r, d, to, err := s.prepareDecision(ctx, actor, returnID, actionReject)
	if err != nil {
		return nil, err
	}

	note, err := s.normalizeOwnerNote(ownerNote, true)
	if err != nil {
		return nil, err
	}

	title, body := returnRejectedNotice(d, note)

	res, err := s.returns.Reject(ctx, repo.DecideRow{
		ID:          r.ID,
		OwnerID:     actor.ID,
		OwnerNote:   note,
		NotifyTitle: title,
		NotifyBody:  body,
	})
	if err != nil {
		return nil, err
	}

	s.logger.InfoContext(ctx, "return.rejected",
		slog.Int64("return_id", r.ID),
		slog.Int64("item_id", r.ItemID),
		slog.Int64("owner_id", actor.ID),
		slog.Int64("submitter_id", res.SubmitterID),
	)
	// 日志到这里就结束了 —— 刻意不打 item 的新状态，因为**没有任何状态被改动**。
	// 如果哪天下意识想在这里加一句「记录一下帖子还是不是 open」，
	// 那说明有人在 Reject 里写了 UPDATE items，先去读 §3.4。

	return &DecideReturnResult{
		ID:         r.ID,
		Status:     to,
		ReviewedAt: res.ReviewedAt.UTC().Format(time.RFC3339),
	}, nil
}

// normalizeOwnerNote 处理发帖人那句备注。
//
// required=false 时允许空（confirm 可以不写话，那条记录里 owner_note 就是默认空串）；
// required=true 时空白也算没写（"   " 不配叫一条理由）。
// 上限按字符数而不是字节数（同 report 的 detail，500 个汉字必须能写满）。
func (s *ItemReturn) normalizeOwnerNote(raw string, required bool) (string, error) {
	note := strings.TrimSpace(raw)
	if n := utf8.RuneCountInString(note); n > model.ReturnOwnerNoteMaxChars {
		return "", apperr.Validation(
			fmt.Sprintf("备注最长 %d 个字，当前 %d 个（一个汉字算 1 个字）",
				model.ReturnOwnerNoteMaxChars, n),
			apperr.FieldError{Field: "owner_note",
				Msg: fmt.Sprintf("最长 %d 个字", model.ReturnOwnerNoteMaxChars)})
	}
	if required && note == "" {
		return "", apperr.Validation("拒绝归还确认必须写一句理由",
			apperr.FieldError{Field: "owner_note",
				Msg: "提交人只能从你这句话里知道为什么被拒绝"})
	}
	return note, nil
}

// prepareDecision 是 confirm / reject 共用的前四步：读记录、读帖子、判权、查状态机。
//
// 抽成一个函数而不是在两处各写一遍，是因为这四步的**顺序**本身就是规则
// （权限在状态前面，状态在字段后面）。写两遍就迟早有一遍顺序不同，
// 而那类差异在第①层单测里长得一模一样、只有读代码才能发现。
//
// 返回 d 是为了让调用方拿到帖子标题去拼通知文案，顺带免掉第二次点查。
//
// ⚠ 这里「权限排在字段校验前面」，而上面 Submit 是「字段排在权限前面」——
// 两个顺序相反，而且都是对的。区别在于动作本身开不开放：
//   - 提交归还确认对**所有**登录用户开放，这里没有任何权限可泄漏，
//     所以先告诉用户「哪个字段错了」（他能改的东西优先）；
//   - confirm / reject 只对**一个人**开放。先回一句「备注太长了」等于暗示
//     「把备注改短你就能审这条记录」，那是在向外描述一条他不该知道的通道
//     （同 service/item_validate.go 的 authorizeItemWrite 第 3 条那条理由）。
func (s *ItemReturn) prepareDecision(ctx context.Context, actor *model.User,
	returnID int64, action string) (*model.ItemReturn, *model.ItemDetail, string, error) {

	r, err := s.returns.GetByID(ctx, returnID)
	if err != nil {
		return nil, nil, "", err
	}

	d, err := s.items.GetByID(ctx, r.ItemID)
	if err != nil {
		return nil, nil, "", err
	}

	if err := authorizeReturnDecision(action, actor.ID, d.UserID, r.SubmitterID); err != nil {
		return nil, nil, "", err
	}

	to, ok := checkTransition(r.Status, action)
	if !ok {
		return nil, nil, "", apperr.New(apperr.CodeReturnIllegalTransition)
	}
	return r, d, to, nil
}

// ---------- #27 提交人撤销 ----------

// CancelReturnResult 是 #27 的 data（计划 §4 第 27 行只有 {id, status}）。
//
// 这里刻意**没有** reviewed_at，虽然数据库那一列在 cancel 时会被填上
// （它的实际含义是「这条记录退出 pending 的时刻」）。
// 对外不给是因为「reviewed」这个词承诺的是「有人审过了」，而没有人审过 ——
// 谁做的、有没有社区含义那两问由 reviewer_id / review_kind 两个 null 回答。
// 前端要看时间可以用 #28 列表里的 submitted_at，那个是真的。
type CancelReturnResult struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

// Cancel 提交人自己撤销那条 pending。
//
// 三个动作里它最轻：**零副作用、不发通知**。
// 顺序同样是 ①记录 ②权限（只看 submitter_id，这一步不需要读帖子）③状态机。
// 它不读 items 是有意的：撤销一条确认和那条帖子是什么状态无关 ——
// 帖子被下架了，提交人仍然有权撤掉自己那条还没被处理的记录
// （反过来才奇怪：平台的治理动作把用户锁在了他自己的记录外面）。
func (s *ItemReturn) Cancel(ctx context.Context, actor *model.User, returnID int64) (*CancelReturnResult, error) {
	r, err := s.returns.GetByID(ctx, returnID)
	if err != nil {
		return nil, err
	}
	// 传 0 给 itemOwnerID 是因为 cancel 分支不读它（那个参数只在 confirm/reject 分支用）。
	// 如果哪天 authorizeReturnDecision 被改成在 cancel 分支也看 itemOwnerID，
	// 这个 0 就会让所有撤销都变成「帖主是 user 0」—— 而第①层那张权限矩阵会立刻红。
	if err := authorizeReturnDecision(actionCancel, actor.ID, 0, r.SubmitterID); err != nil {
		return nil, err
	}
	to, ok := checkTransition(r.Status, actionCancel)
	if !ok {
		return nil, apperr.New(apperr.CodeReturnIllegalTransition)
	}

	res, err := s.returns.Cancel(ctx, returnID, actor.ID)
	if err != nil {
		return nil, err
	}

	s.logger.InfoContext(ctx, "return.cancelled",
		slog.Int64("return_id", returnID),
		slog.Int64("item_id", res.ItemID),
		slog.Int64("submitter_id", actor.ID),
	)
	return &CancelReturnResult{ID: returnID, Status: to}, nil
}

// ---------- #28 / #29 两个列表 ----------

// ReturnListQuery 是 #28/#29 的查询参数（一律传字符串，解析和校验集中在这里，
// 理由同 ListQuery：handler 不该长出「status 必须是四个值之一」这种规则）。
type ReturnListQuery struct {
	Status   string
	Page     string
	PageSize string
}

// ListSubmitted 是 #28「我提交的归还确认」。
func (s *ItemReturn) ListSubmitted(ctx context.Context, actor *model.User,
	q ReturnListQuery) (*Page[model.ReturnEntry], error) {

	status, err := parseReturnStatus(q.Status)
	if err != nil {
		return nil, err
	}
	page, pageSize, err := parsePageQuery(PageQuery{Page: q.Page, PageSize: q.PageSize})
	if err != nil {
		return nil, err
	}
	return s.listReturns(ctx, repo.ReturnListFilter{
		SubmitterID: actor.ID, Status: status, Page: page, PageSize: pageSize,
	}, page, pageSize)
}

// ListReceived 是 #29「我发的帖子收到的归还确认」。
//
// ⚠ 它筛的是 **帖子的作者**（JOIN 出来的 items.user_id），不是那条确认的提交人。
// 两个列表唯一的不同就是这一个条件，其余的分页、状态筛选、排序共用同一份实现
// （repo.ReturnListFilter 的两个字段互斥由这里保证，且都来自 JWT）。
func (s *ItemReturn) ListReceived(ctx context.Context, actor *model.User,
	q ReturnListQuery) (*Page[model.ReturnEntry], error) {

	status, err := parseReturnStatus(q.Status)
	if err != nil {
		return nil, err
	}
	page, pageSize, err := parsePageQuery(PageQuery{Page: q.Page, PageSize: q.PageSize})
	if err != nil {
		return nil, err
	}
	return s.listReturns(ctx, repo.ReturnListFilter{
		OwnerID: actor.ID, Status: status, Page: page, PageSize: pageSize,
	}, page, pageSize)
}

func (s *ItemReturn) listReturns(ctx context.Context, f repo.ReturnListFilter,
	page, pageSize int) (*Page[model.ReturnEntry], error) {

	rows, total, err := s.returns.ListReturns(ctx, f)
	if err != nil {
		return nil, err
	}
	list := make([]model.ReturnEntry, 0, len(rows))
	for i := range rows {
		list = append(list, rows[i].Entry(nil, s.uploads.URL(rows[i].CoverPath)))
	}
	return &Page[model.ReturnEntry]{List: list, Total: total, Page: page, PageSize: pageSize}, nil
}

// parseReturnStatus 是 #28/#29 的 status 白名单。
//
// 空串表示「不筛」（返回 nil 指针，repo 那边就不会拼那一句 WHERE），
// 非法值报 VALIDATION 而不是安静地返回空列表 —— 后者会让前端把一个拼错的
// `?status=Confirmed` 调试半小时，因为它看起来「worksWith」：请求成功、响应合法、列表为空。
//
// 和 repo 那张列白名单一样，这里**不做 trim**：查询参数是前端代码生成的，
// 不是人手打的，宽容匹配只会掩盖前端拼参数时的空格 bug（和 item_type 的处理一致）。
func parseReturnStatus(v string) (*string, error) {
	if v == "" {
		return nil, nil
	}
	if !slices.Contains(model.ReturnStatuses, v) {
		return nil, apperr.Validation("归还确认状态不对",
			apperr.FieldError{Field: "status", Msg: "只能是 " + strings.Join(model.ReturnStatuses, " / ")})
	}
	return &v, nil
}

// ---------- 对外形状与文案 ----------

// summaryOfFound 把一条帖子投影成 #24 里那个 item 摘要。
//
// contact **一律为 nil**，和 #14 广场、#20 匹配列表同一条规则：
// 摘要形状里不携带联系方式，解锁唯一的路径是 #21（那里会写一行 contact_views）。
// 这条纪律在 M5 格外要紧：#24 的读取者里包括 admin，
// 如果嵌套摘要能带 contact，「admin 随手一读就能看到别人的手机号」
// 就绕过了 M4 为 #15 写下的那四条规则（计划 §4：#15 那条路径 admin 没有额外可见性）。
func summaryOfFound(d *model.ItemDetail, coverURL string) model.ItemSummary {
	return d.Summary(nil, coverURL)
}

// 归还确认的四种文案。全部遵守定位原则 1 那条措辞纪律：
// 只说「某人做某事」这个事实，不说「东西还对了」「归属成立」这种平台判断。
//
// 长度是硬约束：notifications.title 是 VARCHAR(100)、content 是 VARCHAR(500)，
// 而 PostgreSQL 数的是**字符**，所以拼接前先 clipRunes 用户可控的那几段
// （帖子标题最长 100 字、归还说明最长 1000 字，直接拼会撞 23514 ——
// 那种失败的后果是整条 confirm 事务回滚，用户会看到一次莫名其妙的 500）。
const (
	returnSubmittedNoticeTitle  = "有人提交了归还确认"
	returnConfirmedNoticeTitle  = "归还确认已通过"
	returnRejectedNoticeTitle   = "归还确认被拒绝"
	itemReturnedHintNoticeTitle = "你关注过的拾物帖已确认归还"

	// 帖子里标题在通知文案中最多占这些字符，剩下的位置留给固定句式。
	noticeTitleBudget   = 40
	noticeMessageBudget = 60
)

// returnSubmittedNotice 是给发帖人的那条：他需要立刻知道有人声称归还了。
//
// content 里带上提交人昵称和说明的开头一段 —— 这不是多余，是 §14-5 那条设计的落地：
// 判断真假的权力全在发帖人手里，那就得给他判断的原料。
// 完整文字和凭证图仍然要点进 #24 才看得到，通知只负责把他叫过来。
func returnSubmittedNotice(d *model.ItemDetail, nickname, message string) (string, string) {
	title := returnSubmittedNoticeTitle
	body := fmt.Sprintf("你的拾物帖「%s」收到一条归还确认：%s 写道「%s」。请核对凭证后决定确认或拒绝。",
		clipRunes(d.Title, noticeTitleBudget), clipRunes(nickname, 20), clipRunes(message, noticeMessageBudget))
	return title, clipRunes(body, 500)
}

// returnConfirmedNotice 是给提交人的那条。
//
// ⚠ 文案里**不写**「那条帖子已关闭」：关帖那一句 SQL 带 `AND status='open'`，
// 已经是 closed / deleted 的帖子不会被再关一次，所以那不是必然发生的事实。
// 通知只说这一行必然成立的部分：发帖人确认了。
func returnConfirmedNotice(d *model.ItemDetail) (string, string) {
	title := returnConfirmedNoticeTitle
	body := fmt.Sprintf("拾物帖「%s」的归还确认已被发帖人确认。感谢你如实记录归还经过。",
		clipRunes(d.Title, noticeTitleBudget))
	return title, clipRunes(body, 500)
}

// returnRejectedNotice 是给提交人的那条，带上发帖人的留言原文。
//
// 同样不写「帖子仍然展示」这种承诺：那条帖子可能已经被下架或关闭，
// 而「仍然展示」是平台的保证。事实部分只有两个：被拒了，理由是这句。
func returnRejectedNotice(d *model.ItemDetail, note string) (string, string) {
	title := returnRejectedNoticeTitle
	body := fmt.Sprintf("拾物帖「%s」的归还确认已被发帖人拒绝。发帖人留言：%s",
		clipRunes(d.Title, noticeTitleBudget), clipRunes(note, noticeMessageBudget))
	return title, clipRunes(body, 500)
}

// returnedHintNotice 是给「当初被 new_match 叫醒过的那些 lost 作者」的那条。
//
// §14-4 把「帖子不关闭会污染候选池」列为已知风险，补救手段之一就是这条提示：
// 东西已经还回去了，你的失物帖还挂在广场上，去点一下「已找到」吧。
// 这是一个**提醒**而不是一个自动动作 —— 平台不代替失主判断「那个钱包就是你的」（原则 1），
// 也不替他关帖（lost 帖的关闭权在他自己手里，那是 #18）。
//
// 返回的 body 是纯静态文案（不含帖子标题）：一次 confirm 要给 N 个作者发通知，
// 而那条拾物帖的标题在这里由 notifications.item_id 跳转时给出，
// 不重复 N 次拼接也就不用担心某个作者有两条失物帖配过对时文案会长得不一样。
func returnedHintNotice(d *model.ItemDetail) (string, string) {
	title := itemReturnedHintNoticeTitle
	body := fmt.Sprintf("拾物帖「%s」已被发帖人确认归还。如果那就是你在找的东西，请到「我的发布」把对应的失物帖标记为已找到。",
		clipRunes(d.Title, noticeTitleBudget))
	return title, clipRunes(body, 500)
}

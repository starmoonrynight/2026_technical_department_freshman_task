package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"
	"unicode/utf8"

	"lostfound/internal/apperr"
	"lostfound/internal/config"
	"lostfound/internal/matcher"
	"lostfound/internal/model"
	"lostfound/internal/repo"
)

// 这个文件是匹配功能的**调度层**：什么时候跑、跑哪一档、算完写不写、写给谁。
//
// 三件事分开在三个包里，各自能被各自的方式测：
//   - matcher      打分（纯函数，零 DB）
//   - repo/match   捞候选 + 写台账（只有 SQL）
//   - 这里         运行时机和方向（§5.8 那张表就是本文件的目录）
//
// 定位原则 1（平台只记录事实，不做裁决）在这里的落点是「谁都不替你决定」：
// 分数、分解、通知都只是「系统认为像」，归属永远由双方线下核对，代码里没有任何
// 「自动认领」「自动判定匹配成功」的状态。

// 四种触发源（§9 的 trigger 字段值）。只有 found_created 和 found_updated 会写库，
// 这个常量列表存在的意义就是把「写入路径只有 found 方向」写成看得见的事实。
//
// found_updated 是对计划 §5.8 那张三行表的补充：§3.7 用途 ① 说去重保护的是
// 「同一条 found 帖被编辑后重新触发匹配」，可那张表里根本没有「编辑」这个触发源，
// 于是 ON CONFLICT 从 HTTP 层永远到不了（新建一次就是新 id，压根不构成冲突）。
// 补上这条路径之后，判据 ③ 才有可达的场景可测（2026-10-07 与用户确认）。
const (
	triggerFoundCreated    = "found_created"
	triggerFoundUpdated    = "found_updated"
	triggerLostCreated     = "lost_created"
	triggerMatchesEndpoint = "matches_endpoint"
)

// MatchStore 是匹配需要的持久化能力。
//
// 和 ItemStore 一样是为「不起数据库就能测权限分支和方向分支」留的接缝（§10 第①层）。
// M3 最该被钉死的两条规则——lost 帖创建一行都不写、found 帖创建只在 Tier 1 写——
// 用一个记录调用次数的 fake 就能测，快且失败信息干净。
type MatchStore interface {
	Candidates(ctx context.Context, f repo.CandidateFilter) ([]model.ItemDetail, error)
	RecordMatches(ctx context.Context, rows []repo.LedgerRow) (int, error)
}

// Match 是匹配的业务对象。
//
// items 那一格是 ItemLookup（只要 GetByID 一个能力），不是 ItemStore：
// M3 最该被钉死的规则是「拿到的这条帖子是不是 #20 的目标帖」，
// 用一个只有点查的接口，fake 里就不可能出现任何写入方法。
type Match struct {
	pairs   MatchStore
	items   ItemLookup
	uploads *Upload
	cfg     config.MatchConfig
	logger  *slog.Logger
}

func NewMatch(pairs MatchStore, items ItemLookup, uploads *Upload, cfg config.MatchConfig, logger *slog.Logger) *Match {
	if logger == nil {
		logger = slog.Default()
	}
	return &Match{pairs: pairs, items: items, uploads: uploads, cfg: cfg, logger: logger}
}

// ---------- 对外形状 ----------

// MatchHit 是「一条候选 + 为什么像」。
//
// Score 和 breakdown.score 是同一个数，出现两次不是冗余：#20 的契约（§4 第 20 行）
// 就是 {item, score, breakdown}，前端列表只读外层那个就能排序/显示徽章，
// 想看分解才去挖 breakdown。两个值同源于 matcher，不可能对不上。
type MatchHit struct {
	Item      model.ItemSummary `json:"item"`
	Score     float64           `json:"score"`
	Breakdown matcher.Breakdown `json:"breakdown"`
}

// MatchesResult 是 #20 GET /api/items/:id/matches 的 data。
//
// Notice 只在 Tier 2 出现（§5.5「API 响应必须带 tier 字段和提示文案」），
// 前端据此显示那条黄色横幅。
type MatchesResult struct {
	Tier   int        `json:"tier"`
	Notice string     `json:"notice,omitempty"`
	List   []MatchHit `json:"list"`
}

// tier2Notice 是 §5.5 指定给前端横幅的文案，逐字照抄。
//
// 放在后端而不是让前端写死：这段话的内容是算法行为的一部分
// （「可能原因②是存在一定概率的不匹配」只有在 Tier 2 放宽了地点筛选时才成立），
// 而算法在后端。
const tier2Notice = "没有找到确定的匹配。可能原因：① 还没有人上传/拾到该物品；② 存在一定概率的不匹配。你可以自行调整下方的地点、时间、分类筛选条件。"

// MatchesQuery 是 #20 的三个查询参数，原样收字符串。
//
// 和 ListQuery 一样：解析和报错都在 service（parseMatchesQuery），
// 因为要给出带字段名的中文 VALIDATION（handler 里 strconv 失败只能吐英文技术细节）。
type MatchesQuery struct {
	Top      string
	MinScore string
	Tier     string
}

// #20 的分页与阈值边界。
const (
	matchesDefaultTop = 10
	matchesMaxTop     = 50
)

// ---------- 三个触发源 ----------

// OnCreated 是 #13 发帖成功之后匹配的唯一入口，方向由 target 自己的类型决定。
//
// 返回值是**两个指针**，它们的「一定出现 / 一定不出现」就是 §4 那对互斥可选字段：
//   - item_type=lost → preview 永远非 nil（空命中是指向空切片的指针，序列化成 `[]`），
//     notified 永远是 nil。Go 的 omitempty 会把 len==0 的切片连同键一起抹掉，
//     所以不用值类型：§13 验收步骤要求「库里还没有 found 帖时响应带 matches_preview: []」，
//     而前端要靠「键在但为空」显示那段诚实文案（§2.6.3）。
//   - item_type=found → notified 永远非 nil（没通知上就是 0，这个 0 必须出现：
//     「这一条 found 帖没够到任何失主」和「这不是 found 帖」在前端是两件事，
//     键消失了就分不出来），preview 永远是 nil。
//
// 两条分支各返回一个非 nil 指针、另一个保持 nil，任何情况下都不会两个都出现。
//
// ⚠ **这个函数没有 error 返回值，是刻意的**（§5.8：匹配失败绝不影响发帖成功）。
// 帖子已经落库了，这时因为「候选查不到」把整个请求报成 500，用户会以为发帖失败并再发一次，
// 于是库里出现两条重复帖子——一个辅助功能拖垮主流程，还顺带制造了脏数据。
// 所以匹配的所有失败都在这里变成 error 级日志。签名里没有 error，
// 将来谁想往上传播也传不了。
func (s *Match) OnCreated(ctx context.Context, target *model.ItemDetail) (*[]MatchHit, *int) {
	switch target.ItemType {
	case model.ItemTypeLost:
		// 算完就丢：不写 match_pairs、不写 notifications、谁都不通知（§2.6.2）
		out, err := s.run(ctx, runInput{
			target:  target,
			trigger: triggerLostCreated,
			// allowFallback 留空：#13 这一档按 §5.8 只跑 Tier 1，
			// 地点是「其他」时前提不成立，结果就是空列表 —— 放宽筛选是 #20 的活，
			// 让用户自己点，不在发帖时偷偷给他一档更不可信的结果。
			minScore: s.cfg.ShowThreshold,
			topN:     matcher.PreviewTopN,
		})
		if err != nil {
			s.logFailure(ctx, target, triggerLostCreated, err)
			// 和没匹配上给出同一个形状：键一定出现，值是空数组。
			// 前端那段文案（「已经登记好了，之后有人发拾物帖匹配上了会通知你」）
			// 在这两种情况下都是诚实的，而「键消失了」会让它得先判一下是不是 null。
			return &[]MatchHit{}, nil
		}
		s.logRun(ctx, out, 0)
		return &out.hits, nil

	case model.ItemTypeFound:
		// 唯一的写入路径之一。Tier 2 的结果一律不写（§3.7），所以先跑再看档位。
		notified := s.notifyMatches(ctx, target, triggerFoundCreated)
		return nil, &notified
	}

	// 到不了这里：#13 在 service.Item.Create 开头就验过 item_type。
	// 不 panic 是因为「发帖类型校验」和「匹配方向」分处两个函数，
	// 将来加第三种 item_type 时这里安静地什么都不做，比崩在半路好排查。
	return nil, nil
}

// OnUpdated 是 #16 改帖之后的匹配入口，签名和 OnCreated 一样**没有 error**
// （同一条纪律：改帖已经落库了，匹配失败不能把它报成 500 让用户再改一次）。
//
// 这条路径补的是那个具体的洞：小王捡到钱包时随手写了「捡到钱包」+ 地点「其他」，
// 没匹配上；后来他把帖子改准了。没有 OnUpdated 的话系统不会再算一次，
// 于是小李永远等不到那条通知 —— 而 §3.7 用途 ① 写去重时假设的正是这条路径存在。
//
// 两道门槛，都是「不跑」而不是「跑了不写」：
//   - **只有 found 帖**。lost 方向的匹配按 §5.8 永远算完就丢，改帖没有可写的东西，
//     跑一次只是白跑一遍 SQL。不对称要在代码里保持对称的简单：
//     入口有两个（OnCreated / OnUpdated），方向规则只有一处（这里 + notifyMatches）。
//   - **状态必须是 open**。closed 的拾物帖意味着东西已经还回去了，
//     这时候再给失主推一条「可能有匹配」是在骗一个已经结束寻找的人；
//     而 §8 允许改 closed 帖（改错了的电话必须能改），所以这个判断不能省。
//
// 返回值刻意是空的：#16 的响应形状（§4）里只有 item，没有 notified_count。
// 改帖的人想知道匹配情况有 #20 可查，把计数塞进响应会让 #16 的契约随类型变化，
// 而那是 #13 才需要的复杂度。数字进 match.run 日志，一处都不少。
func (s *Match) OnUpdated(ctx context.Context, target *model.ItemDetail) {
	if target.ItemType != model.ItemTypeFound || target.Status != model.ItemStatusOpen {
		return
	}
	s.notifyMatches(ctx, target, triggerFoundUpdated)
}

// notifyMatches 跑一次 found 方向的匹配，把 score≥通知线的配对写台账、发通知。
//
// OnCreated 和 OnUpdated 共用这一个函数，为的是「两条路径的去重规则必须一模一样」：
// 分成两份代码的话，将来谁改了一份（比如把通知线调低、或者给改帖加个「先删旧行再插」），
// 另一份还留着老规矩，而这两种改法都会让重复通知回来。
// 台账的 UNIQUE + ON CONFLICT 是去重的全部机制，这里没有任何额外状态。
func (s *Match) notifyMatches(ctx context.Context, target *model.ItemDetail, trigger string) int {
	out, err := s.run(ctx, runInput{
		target:  target,
		trigger: trigger,
		// 不截断（topN=0）、不低于通知线的不要（minScore=NotifyThreshold）：
		// 这一档要的是「所有 score≥0.75 的 lost 帖」，不是「前 10 名」。
		minScore: s.cfg.NotifyThreshold,
		topN:     0,
	})
	if err != nil {
		s.logFailure(ctx, target, trigger, err)
		return 0
	}

	notified := 0
	if out.tier == matcher.Tier1 && len(out.hits) > 0 {
		n, err := s.pairs.RecordMatches(ctx, s.ledgerRows(target, out.hits))
		if err != nil {
			// 台账没写进去 = 这一对下次还会再试一次通知，
			// 这是可接受的降级；把错误往上抛则会让用户重发帖子，不可接受。
			s.logger.ErrorContext(ctx, "match.ledger_failed",
				slog.Int64("item_id", target.ID),
				slog.Int("rows", len(out.hits)),
				slog.String("err", err.Error()))
		} else {
			notified = n
		}
	}
	s.logRun(ctx, out, notified)
	return notified
}

// List 是 #20 GET /api/items/:id/matches（JWT，本人或 Admin）。
//
// **只读**：这一条路径永远不调 RecordMatches。§3.7 的理由是「用户反复刷新
// 不应该往台账里灌数据」——如果这里也写台账，那么「谁被通知过」这个事实
// 就变成了「谁看过详情页」，match_pairs 作为通知台账就不再可信了。
func (s *Match) List(ctx context.Context, viewer *model.User, itemID int64, q MatchesQuery) (*MatchesResult, error) {
	top, minScore, wantTier, err := parseMatchesQuery(q, s.cfg.ShowThreshold)
	if err != nil {
		return nil, err
	}

	target, err := s.items.GetByID(ctx, itemID)
	if err != nil {
		return nil, err
	}
	// 软删的帖子对外不存在（同 #15 Detail 的判断，包括「报 NOT_FOUND 而不是 FORBIDDEN」
	// 那层意思：返回 403 等于确认了「这个 id 曾有一条被删掉的帖子」）。
	if target.Status == model.ItemStatusDeleted && !canSeeDeleted(viewer, target.UserID) {
		return nil, apperr.NotFound("帖子")
	}
	// 「本人或 Admin」是 §4 第 20 行的鉴权列。
	// 为什么匹配结果要限权：它会把**另一条帖子的作者昵称**和排序后的候选一起返回，
	// 而这些候选本质上是「谁在找什么东西」的画像。广场能看单条帖子是另一回事——
	// 那里没有任何「这两条可能是一个东西」的推断。
	if viewer.ID != target.UserID && !viewer.IsAdmin() {
		return nil, apperr.Forbidden("只有发帖人本人和管理员能查看匹配结果")
	}

	out, err := s.run(ctx, runInput{
		target:        target,
		trigger:       triggerMatchesEndpoint,
		wantTier:      wantTier,
		allowFallback: wantTier == 0, // 只有 auto 允许降级；显式 1/2 就是用户要的那一档
		minScore:      minScore,
		topN:          top,
	})
	if err != nil {
		s.logFailure(ctx, target, triggerMatchesEndpoint, err)
		return nil, err
	}
	s.logRun(ctx, out, 0)

	res := &MatchesResult{Tier: out.tier, List: out.hits}
	if out.tier == matcher.Tier2 {
		res.Notice = tier2Notice
	}
	return res, nil
}

// ---------- 打分运行 ----------

// runInput 是一次匹配的全部输入。
type runInput struct {
	target        *model.ItemDetail
	trigger       string
	wantTier      int     // 0=auto、1、2（只有 #20 会用非 0）
	allowFallback bool    // Tier 1 空手而归时是否降级
	minScore      float64 // 低于此分的候选不进结果（运营阈值，不是算法的一部分）
	topN          int     // 0 = 不截断
}

// matchRun 是一次匹配的完整产出，包括 §9 那条日志要的全部数字。
//
// 前三个字段是「这次是谁」，run() 里就填好：日志函数因此不需要再拿一遍 target，
// 少一个参数就少一处「传错了 target」的可能。
type matchRun struct {
	itemID   int64
	itemType string
	trigger  string

	tier           int
	fellBack       bool
	hits           []MatchHit
	candidateCount int
	scoredCount    int
	topScore       float64
	aboveNotify    int
	startedAt      time.Time
}

// run 捞候选、打分、按阈值筛、截断。
//
// 顺序很重要：筛和截断都发生在打分**之后**（§5.1 第三步：分数只决定顺序，不决定去留），
// 所以 minScore/topN 不参与 SQL，也不参与 matcher.BestMatches 的排序。
// 反过来做的话「分类选错但确实是同一个东西」的帖子会在捞的时候就消失，
// 而那种帖子恰恰是最需要出现在结果里让用户自己判断的。
func (s *Match) run(ctx context.Context, in runInput) (*matchRun, error) {
	target := in.target
	mTarget := toMatcherItem(target)
	opt := s.options()

	out := &matchRun{
		itemID:    target.ID,
		itemType:  target.ItemType,
		trigger:   in.trigger,
		tier:      matcher.Tier1,
		startedAt: time.Now(),
		hits:      []MatchHit{},
	}

	// ---- 分档（§5.5 的三个触发条件）----
	strictPossible := matcher.CanTier1(mTarget)
	switch {
	case in.wantTier == matcher.Tier2:
		// 用户显式要放宽的结果，不是降级，fell_back 保持 false（§9 那个字段
		// 的定义是「Tier 1 空手而归」，把主动放宽记成降级会污染 §14-2 的比例统计）
		out.tier = matcher.Tier2
	case !strictPossible && in.allowFallback:
		// 前提不成立（地点是「其他」/ 时间锚点缺失）
		out.tier, out.fellBack = matcher.Tier2, true
	}

	cands, err := s.candidates(ctx, in, out.tier)
	if err != nil {
		return nil, err
	}

	// 严格模式的前提成立、SQL 也确实一条都没捞回来 → 空手而归 → 降级兜底。
	// 只在 auto 允许时做。判据是「候选集为空」而不是「没有高分候选」：
	// 前者说明地点+时间筛太狠（§14-2 要观察的正是这个），后者说明东西确实不像，
	// 放宽地点也救不回来，只会把噪声推到用户面前。
	if out.tier == matcher.Tier1 && len(cands) == 0 && in.allowFallback {
		out.tier, out.fellBack = matcher.Tier2, true
		cands, err = s.candidates(ctx, in, out.tier)
		if err != nil {
			return nil, err
		}
	}
	// 显式 ?tier=1 但前提不成立：留在 1 档，一条候选都不捞（见 candidates()）。
	// 结果就是空列表。这是诚实的答案——「不放宽」在这种情况下确实给不出东西，
	// 比偷偷给他放宽过的结果要好。

	out.candidateCount = len(cands)
	if len(cands) == 0 {
		return out, nil
	}

	byID := make(map[int64]model.ItemDetail, len(cands))
	probe := make([]matcher.Item, 0, len(cands))
	for i := range cands {
		byID[cands[i].ID] = cands[i]
		probe = append(probe, toMatcherItem(&cands[i]))
	}

	results := matcher.BestMatches(mTarget, probe, out.tier, 0, opt)
	out.scoredCount = len(results)
	if len(results) > 0 {
		out.topScore = results[0].Breakdown.Score
	}

	for _, r := range results {
		if r.Breakdown.Score >= s.cfg.NotifyThreshold {
			out.aboveNotify++ // 与 notified_count 的差就是去重命中数（§9）
		}
		if r.Breakdown.Score < in.minScore {
			continue
		}
		cand, ok := byID[r.ItemID]
		if !ok {
			// 到不了这里：候选集就是 byID 的键集合，matcher 不会凭空造 id。
			// 不 panic：一个内部映射对不上不该让整个请求 500，跳过这一条并继续，
			// 少一条结果的后果远小于把已经算出来的东西全丢掉。
			continue
		}
		out.hits = append(out.hits, s.toHit(&cand, r))
		if in.topN > 0 && len(out.hits) >= in.topN {
			break
		}
	}
	return out, nil
}

// candidates 按档位捞候选。
//
// Tier 2 传 LocationID=0，repo 那边就不拼地点条件（§5.5「不做地点硬筛选」）。
// Tier 1 但严格模式前提不成立时直接返回空而不发 SQL：此时唯一的筛选项是那条
// 「location_id = 其他」——所有选「其他」的帖子都会进来，它们其实毫无关系，
// 打出来的分数全靠分类和时间，比空列表更误导人。
func (s *Match) candidates(ctx context.Context, in runInput, tier int) ([]model.ItemDetail, error) {
	target := in.target
	mTarget := toMatcherItem(target)
	if tier == matcher.Tier1 && !matcher.CanTier1(mTarget) {
		return nil, nil
	}

	anchor, ok := timeAnchor(mTarget)
	if !ok {
		// Tier 2 的时间硬筛也要锚点（§5.5：兜底模式只放宽地点，时间硬筛保留）。
		// 锚点缺失只可能出现在脏数据上（CHECK 约束保证 lost 有 last_seen_at、
		// found 有 found_at），此时任何时间条件都写不出来，只能给空结果。
		s.logger.WarnContext(ctx, "match.no_time_anchor",
			slog.Int64("item_id", target.ID),
			slog.String("item_type", target.ItemType))
		return nil, nil
	}

	opposite := model.ItemTypeFound
	if target.ItemType == model.ItemTypeFound {
		opposite = model.ItemTypeLost
	}

	rows, err := s.pairs.Candidates(ctx, repo.CandidateFilter{
		OppositeType: opposite,
		TargetID:     target.ID,
		LocationID:   locationFilter(tier, target),
		AnchorAt:     anchor,
		Tolerance:    time.Duration(s.cfg.TimeToleranceHours) * time.Hour,
	})
	if err != nil {
		return nil, err
	}
	return rows, nil
}

func locationFilter(tier int, target *model.ItemDetail) int64 {
	if tier == matcher.Tier2 {
		return 0
	}
	return target.LocationID
}

// toHit 拼一条对外的匹配结果。
//
// contact 的填充规则**照抄 #14**：lost 帖带、found 帖一律 null，不管谁在看。
// 匹配列表不是解锁的替代品——M4 的 #21 才是。如果这里把 found 帖的联系方式放出来，
// 「点一下认领、后台留一行审计」就变成「刷一次匹配列表就够了」，
// 而 contact_views 作为认领名单的完整性正是定位原则 3 的落点。
func (s *Match) toHit(cand *model.ItemDetail, r matcher.Result) MatchHit {
	var contact *string
	if cand.ItemType == model.ItemTypeLost {
		contact = &cand.Contact
	}
	return MatchHit{
		Item:      cand.Summary(contact, s.uploads.URL(cand.CoverPath)),
		Score:     r.Breakdown.Score,
		Breakdown: r.Breakdown,
	}
}

// ledgerRows 把「score≥通知线的 lost 候选」翻译成台账 + 通知。
//
// 方向在这里钉死：LostItemID 一定是候选（这一档的候选只有 lost），
// FoundItemID 一定是刚发出来的那条 target。
// 反过来写不会报错，只会让通知发给捡到东西的人——而 §2.6.2 明确禁止通知 found 方。
func (s *Match) ledgerRows(target *model.ItemDetail, hits []MatchHit) []repo.LedgerRow {
	rows := make([]repo.LedgerRow, 0, len(hits))
	for _, h := range hits {
		raw, err := json.Marshal(h.Breakdown)
		if err != nil {
			// matcher 的输出全是 float64/string/struct，正常情况下不可能序列化失败。
			// 真失败了说明有人在 Breakdown 里塞了 chan 之类的东西，那条就跳过——
			// 不能因为一行 breakdown 写不进 JSONB 就把整批通知丢掉。
			s.logger.ErrorContext(context.Background(), "match.breakdown_encode_failed",
				slog.Int64("lost_item_id", h.Item.ID),
				slog.String("err", err.Error()))
			continue
		}
		title, body := newMatchNotice(target, h)
		rows = append(rows, repo.LedgerRow{
			LostItemID:  h.Item.ID,
			FoundItemID: target.ID,
			Score:       h.Score,
			Breakdown:   string(raw),
			NotifyTo:    h.Item.AuthorID,
			NotifyTitle: title,
			NotifyBody:  body,
		})
	}
	return rows
}

// newMatchNotice 拼通知文案。
//
// 两个上限是被数据库逼出来的：notifications.title VARCHAR(100)、content VARCHAR(500)。
// 帖子标题本身允许 100 字符，直接拼会超——超了不是截断显示，是 INSERT 报错 22001，
// 而那个错误在 RecordMatches 的事务里，会把**整批**通知一起回滚掉。
// 所以这里必须自己裁，而且按字符裁不是按字节（一个汉字 3 字节）。
//
// 措辞遵守定位原则 1：「系统认为像」而不是「这就是你的东西」，
// 并把「请自行核对」写进正文——通知跳得越准，越要明说这一步不是平台判的。
func newMatchNotice(target *model.ItemDetail, h MatchHit) (title, body string) {
	title = "你丢失的「" + clipRunes(h.Item.Title, 40) + "」可能有匹配"
	body = fmt.Sprintf(
		"有人在%s发布了拾物帖「%s」，系统算出的匹配度是 %d%%。请自行核对物品特征后再联系对方——匹配只是相似度排序，平台不判定归属。",
		target.LocationName, clipRunes(target.Title, 60), int(h.Score*100+0.5))
	return title, body
}

// clipRunes 按字符数截断，超长时补省略号。
func clipRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "…"
}

// toMatcherItem 把 model 的一行投影成 matcher 需要的字段。
//
// 这个函数是 matcher 与数据库之间唯一的接缝，所以「匹配只看这些信息」
// 这句话在这里是可读的：谁往 model 加了新字段而没往这里加，就说明它不参与打分。
func toMatcherItem(d *model.ItemDetail) matcher.Item {
	return matcher.Item{
		ID:                 d.ID,
		ItemType:           d.ItemType,
		Title:              d.Title,
		Description:        d.Description,
		LocationDetail:     d.LocationDetail,
		CategoryID:         d.CategoryID,
		CategoryParent:     d.CategoryParentID,
		LocationID:         d.LocationID,
		LocationParent:     d.LocationParentID,
		LocationTop:        d.LocationTopID,
		LocationIsFreeform: d.LocationIsFreeform,
		LastSeenAt:         d.LastSeenAt,
		LostAt:             d.LostAt,
		FoundAt:            d.FoundAt,
	}
}

// timeAnchor 取这条帖子的时间锚点：lost 看 last_seen_at，found 看 found_at（§5.2）。
func timeAnchor(it matcher.Item) (time.Time, bool) {
	switch it.ItemType {
	case matcher.TypeLost:
		if it.LastSeenAt != nil {
			return *it.LastSeenAt, true
		}
	case matcher.TypeFound:
		if it.FoundAt != nil {
			return *it.FoundAt, true
		}
	}
	return time.Time{}, false
}

// options 把 .env 里的衰减天数交给 matcher。
//
// 容差不在这里（Options 里没有 Tolerance 那个字段）：它只用来框定 SQL 的候选范围，
// 不参与打分。放进 matcher 会让纯函数依赖一个它用不到的参数。
func (s *Match) options() matcher.Options {
	return matcher.Options{Decay: time.Duration(s.cfg.DecayDays) * 24 * time.Hour}
}

// ---------- 参数解析 ----------

// parseMatchesQuery 解析 #20 的三个查询参数。
//
// 边界值都报 VALIDATION 带字段名而不是「静默纠正」：top=0 悄悄变成 10、
// min_score=abc 悄悄变成 0.55，用户看到的列表和 URL 上的参数不一致，
// 那种不一致在调试时会让人怀疑算法而不是怀疑参数。
func parseMatchesQuery(q MatchesQuery, defaultMin float64) (top int, minScore float64, tier int, err error) {
	top = matchesDefaultTop
	if q.Top != "" {
		n, perr := strconv.Atoi(q.Top)
		if perr != nil || n < 1 || n > matchesMaxTop {
			return 0, 0, 0, apperr.Validation("top 参数不对",
				apperr.FieldError{Field: "top", Msg: fmt.Sprintf("必须是 1–%d 的整数", matchesMaxTop)})
		}
		top = n
	}

	minScore = defaultMin
	if q.MinScore != "" {
		f, perr := strconv.ParseFloat(q.MinScore, 64)
		if perr != nil || f < 0 || f > 1 {
			return 0, 0, 0, apperr.Validation("min_score 参数不对",
				apperr.FieldError{Field: "min_score", Msg: "必须是 0–1 之间的数"})
		}
		minScore = f
	}

	switch q.Tier {
	case "", "auto":
	case "1":
		tier = matcher.Tier1
	case "2":
		tier = matcher.Tier2
	default:
		return 0, 0, 0, apperr.Validation("tier 参数不对",
			apperr.FieldError{Field: "tier", Msg: "只能是 auto、1 或 2"})
	}
	return top, minScore, tier, nil
}

// ---------- 可观测性 ----------

// logRun 打 §9 那条 match.run。十个字段一个不少，因为 §14-2 的
// 「fell_back 占比」统计要拿它当唯一数据源。
//
// ⚠ 绝不打分数分布以外的用户内容（标题、描述），也不打 contact ——
// 日志会被复制到 issue、贴到群里，而那些是用户填的隐私。
func (s *Match) logRun(ctx context.Context, out *matchRun, notified int) {
	s.logger.InfoContext(ctx, "match.run",
		slog.Int64("item_id", out.itemID),
		slog.String("item_type", out.itemType),
		slog.String("trigger", out.trigger),
		slog.Int("tier", out.tier),
		slog.Bool("fell_back", out.fellBack),
		slog.Int("candidate_count", out.candidateCount),
		slog.Int("scored_count", out.scoredCount),
		slog.Float64("top_score", out.topScore),
		slog.Int("above_notify", out.aboveNotify),
		slog.Int("notified_count", notified),
		slog.Int64("duration_ms", time.Since(out.startedAt).Milliseconds()))
}

// logFailure 是匹配彻底跑不起来时的那条日志。它替代 match.run 而不是补一条，
// 因为没有数字可报（候选都没捞到）。
func (s *Match) logFailure(ctx context.Context, target *model.ItemDetail, trigger string, err error) {
	s.logger.ErrorContext(ctx, "match.run_failed",
		slog.Int64("item_id", target.ID),
		slog.String("item_type", target.ItemType),
		slog.String("trigger", trigger),
		slog.String("err", err.Error()))
}

package service

import (
	"context"
	"log/slog"
	"time"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
)

// 这个文件是 M4 的第一块：**联系方式怎么解锁、解锁名单怎么看**。
//
// 它是定位原则 2 唯一的落地处 ——「平台不会留联系和私信接口，联系方式是能联系到
// 本人的唯一选项」。既然那是唯一选项，#21 就是全站最要紧的一个写接口：
// 它不放行，整条产品链在「匹配上了但联系不上人」这里断掉；
// 它多放行，泄漏的就是用户的手机号微信。
//
// 也是定位原则 3 的落地处 ——「认领不是排他锁，只是谁看过联系方式的审计日志」。
// 所以 #21 **没有任何排他语义**：第一个人解锁不影响第二个人解锁，
// service 里没有「已被他人认领」这种状态可查，因为表里就没有那个列。

// ContactViewLookup 是 #15 判「这位读者解锁过没有」需要的一个能力。
//
// 单独一个接口而不是把 *repo.Contact 整个塞进 Item 服务：Item 只该读不该写，
// 解锁那张表的写能力（Unlock）一旦出现在 Item 的依赖里，
// 「改帖子的服务能顺手记录解锁」这种奇怪的事就变成可能了。
// 窄接口在这里的作用是让「谁有能力写 contact_views」这个问题只有一个答案：Contact 服务。
type ContactViewLookup interface {
	Viewed(ctx context.Context, itemID, userID int64) (bool, error)
}

// ContactStore 是解锁名单需要的持久化能力（写一行 + 分页读）。
type ContactStore interface {
	Unlock(ctx context.Context, itemID, userID int64) (time.Time, bool, error)
	List(ctx context.Context, itemID int64, page, pageSize int) ([]model.ContactView, int, error)
}

// Contact 是解锁与解锁名单的业务规则。
//
// items 用 ItemLookup（只有 GetByID）而不是 ItemStore：这个服务只该读帖子的
// 类型、状态和作者来判断能不能解锁，写帖子、删图片那些能力对它没有意义，
// 出现在依赖里就是一种它本不该有的可能性。
type Contact struct {
	views  ContactStore
	items  ItemLookup
	logger *slog.Logger
}

func NewContact(views ContactStore, items ItemLookup, logger *slog.Logger) *Contact {
	if logger == nil {
		logger = slog.Default()
	}
	return &Contact{views: views, items: items, logger: logger}
}

// ---------- #21 解锁联系方式 ----------

// UnlockResult 是 #21 POST /api/items/:id/unlock-contact 的 data（计划 §4 第 21 行）。
//
// Contact 是**值而不是指针**：能走到这个响应，说明解锁已经成立，
// 联系方式一定给（§4 的规则里没有任何「解锁成功但不给 contact」的分支）。
// 用 *string 反而会让前端多写一个它永远用不到的判空分支。
//
// UnlockedAt 是**第一次**解锁的时刻。重复调用返回同一个值，这是幂等的证据之一。
// 帖主本人的那次例外，是空串 —— 他名下没有 contact_views 行，也不该有（见 Unlock 里的注释），
// 而空串是本项目的「这个时间为 NULL」的标准形状（model.formatTime 同一条约定）。
type UnlockResult struct {
	Contact         string `json:"contact"`
	UnlockedAt      string `json:"unlocked_at"`
	AlreadyUnlocked bool   `json:"already_unlocked"`
}

// Unlock 记录「这个人来看过这条 found 帖的联系方式」，并把联系方式给他。
//
// 判断顺序（每一步都是刻意排的，改动前先想清楚为什么）：
//
//	① 帖子不存在 / 已软删 → NOT_FOUND
//	② lost 帖            → VALIDATION
//	③ 作者本人            → 直接给，**不写 contact_views**
//	④ closed 帖          → ITEM_CLOSED
//	⑤ 其余              → repo.Unlock（ON CONFLICT DO NOTHING）
//
// ①放在最前：软删的帖子对外就是不存在，这条纪律在 #15、#22 都一样，
// 而且它必须排在所有「能不能操作」的判断之前 —— 否则一个已删帖子会通过
// 错误码的差异向外泄漏「它曾经存在过」（和 M1 登录接口不区分用户名/密码错误同一条理由）。
//
// ②排在④前面：lost 帖的联系方式本来就是公开的，对调解锁接口这件事**根本不成立**，
// 比「帖子关了所以不能操作」更接近问题的本质。而且真按④先判，
// 一条 closed 的 lost 帖会得到 ITEM_CLOSED，用户会以为是帖子的问题而不是类型的问题。
//
// ③是本文件最重要的一条。为什么不写那一行：contact_views 是「谁解锁过」的审计日志，
// 而作者**从来没有解锁过** —— 联系方式是他自己填的，#15 从一开始就给他看。
// 插一行进去是在制造一条假事实（定位原则 1：平台只记录事实），
// 后果是 #22 那份名单里会混进作者自己，他看到「某某解锁了你的联系方式」
// 而那个某某就是他自己。AlreadyUnlocked 在这里返回 true，
// 含义是「你不需要解锁，从来都不需要」。
//
// ④放在③之后：作者改完电话之类的事后修帖，帖子可能已经 closed，
// 那时候他自己那条联系方式仍然应该能看到。
//
// ⚠ 整个方法**不写 notifications 一行**。§16 里 `contact_unlocked` 这个通知 type
// 已经被删掉了，判据里「解锁之后断言 notifications 没有新增行」测的就是这件事。
// 少这一句注释，将来一定有人说「解锁了总该通知一下发帖人吧」。
func (s *Contact) Unlock(ctx context.Context, viewer *model.User, itemID int64) (*UnlockResult, error) {
	d, err := s.items.GetByID(ctx, itemID)
	if err != nil {
		// repo.Item.GetByID 已经把「查不到这一行」翻成 NOT_FOUND，这里不再包一层：
		// 包了会把那个已经是对的码变成错的码。
		return nil, err
	}

	if d.Status == model.ItemStatusDeleted {
		return nil, apperr.NotFound("帖子")
	}

	if d.ItemType == model.ItemTypeLost {
		return nil, apperr.Validation("lost 帖不需要解锁联系方式",
			apperr.FieldError{Field: "id",
				Msg: "lost 帖的联系方式本来就是公开的，详情页直接能看到；只有 found 帖的联系方式需要解锁"})
	}

	if viewer.ID == d.UserID {
		return &UnlockResult{Contact: d.Contact, AlreadyUnlocked: true}, nil
	}

	if d.Status == model.ItemStatusClosed {
		return nil, apperr.New(apperr.CodeItemClosed)
	}

	at, created, err := s.views.Unlock(ctx, itemID, viewer.ID)
	if err != nil {
		return nil, err
	}

	// 解锁成功只进日志，不进通知。日志这里是全站唯一能回答
	// 「谁在什么时候解锁了谁的联系方式」的地方（表里有同样一份事实，但日志能带上 request_id）。
	s.logger.InfoContext(ctx, "contact.unlocked",
		slog.Int64("item_id", itemID),
		slog.Int64("user_id", viewer.ID),
		slog.Int64("owner_id", d.UserID),
		slog.Bool("already_unlocked", !created),
	)

	return &UnlockResult{
		Contact:         d.Contact,
		UnlockedAt:      at.UTC().Format(time.RFC3339),
		AlreadyUnlocked: !created,
	}, nil
}

// ---------- #22 解锁名单 ----------

// Views 返回一条帖子被解锁过的完整名单，分页。
//
// 谁能看：**发帖人本人，或 admin**（计划 §4 第 22 行的鉴权列）。
// 这条不是「登录后都能看」，也不是「登录且解锁过就能看」——
// 名单里是别人的真实姓名，除了「谁来找过我」这个正当需求之外没有任何人需要它。
//
// ⚠ 这一页的存在是定位原则 3 的另一半：认领非排他，所以发帖人**不能**阻止谁解锁，
// 但他有权知道谁解锁过。骚扰的唯一事后补救手段就是这份日志（§14 里场景 7 说的），
// 所以它必须完整、不可被绕过 —— 这也是为什么 admin 在 #15 里没有额外的
// 联系方式可见性：那条路径如果通了，这份名单就不再完整了。
func (s *Contact) Views(ctx context.Context, actor *model.User, itemID int64, q PageQuery) (*Page[model.ContactViewEntry], error) {
	page, pageSize, err := parsePageQuery(q)
	if err != nil {
		return nil, err
	}

	d, err := s.items.GetByID(ctx, itemID)
	if err != nil {
		return nil, err
	}

	// 和 #15 完全同一条软删规则：别人看不到，作者自己和 admin 看得到。
	// 作者被下架之后仍然有权知道「在被下架之前谁来看过」，这条正是治理场景里最需要的证据。
	if d.Status == model.ItemStatusDeleted && !canSeeDeleted(actor, d.UserID) {
		return nil, apperr.NotFound("帖子")
	}

	if actor.ID != d.UserID && !actor.IsAdmin() {
		// FORBIDDEN 而不是 NOT_FOUND：这里没必要藏。#22 的鉴权列写的就是这两个码，
		// 而藏起来的代价是发帖人拼错一个 id 时会收到「帖子不存在」，
		// 明明是没权限却去查那条帖子为什么不见了。
		return nil, apperr.Forbidden("只有发帖人能查看这条帖子的解锁名单")
	}

	rows, total, err := s.views.List(ctx, itemID, page, pageSize)
	if err != nil {
		return nil, err
	}

	list := make([]model.ContactViewEntry, 0, len(rows))
	for _, r := range rows {
		list = append(list, r.View())
	}
	return &Page[model.ContactViewEntry]{List: list, Total: total, Page: page, PageSize: pageSize}, nil
}

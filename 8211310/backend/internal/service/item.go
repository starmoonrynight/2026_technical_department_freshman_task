package service

import (
	"context"
	"log/slog"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
	"lostfound/internal/repo"
)

// ItemStore 是帖子业务需要的全部持久化能力。
//
// 窄接口的理由和 UserStore 一样，但这里更实际：M2 的验收判据里
// 「非本人改他人帖得 FORBIDDEN」「改自己一条 closed 状态的帖子 → 成功」
// 这两条是**权限规则**，它们必须能在不起数据库的情况下被测（§10 第①层），
// 否则每加一条权限分支都要跑一次完整迁移，而且失败时看不清是权限错了还是数据脏了。
type ItemStore interface {
	Create(ctx context.Context, p repo.NewItemRow) (*model.ItemDetail, error)
	GetByID(ctx context.Context, id int64) (*model.ItemDetail, error)
	Update(ctx context.Context, id int64, p repo.UpdateItemRow) (*model.ItemDetail, error)
	SetStatus(ctx context.Context, id int64, status string) error
	List(ctx context.Context, f repo.ListFilter) ([]model.ItemDetail, int, error)
	ListImages(ctx context.Context, itemID int64) ([]model.ItemImage, error)
	ImageWithOwner(ctx context.Context, imageID int64) (model.ItemImage, int64, error)
	DeleteImage(ctx context.Context, imageID int64) (string, error)
}

// DictLookup 是发帖/改帖时校验分类与地点需要的能力。
//
// 单独一个接口而不是复用 DictTrees：建树要的是全量行，校验要的是单行点查，
// 两者的形状完全不同。合在一起的话，为校验写的 fake 就得实现一个它用不上的全量查询。
type DictLookup interface {
	GetCategory(ctx context.Context, id int64) (*model.Category, error)
	GetLocation(ctx context.Context, id int64) (*model.Location, error)
	CountActiveChildren(ctx context.Context, id int64) (int, error)
}

// MatchRunner 是发帖 / 改帖之后那一步匹配需要的能力。
//
// ⚠ 两个签名里**都没有 error**，这不是遗漏，是 §5.8「匹配失败绝不影响发帖成功」
// 在类型层面的落实：接口里没有 error，Create/Update 想往上抛也抛不了，
// 所有失败只能在 Match 内部变成日志。将来若真需要「匹配失败要报错」，
// 必须先改这个签名 —— 而改签名会撞上所有 fake，改的人就会停下来想清楚
// 「为了一个增值功能让用户重发一遍帖子」是不是他要的。
//
// 返回值是指针而不是值，含义见 Match.OnCreated。用接口而不是直接用 *Match，
// 是为了给 M3 的集成测试留一个能数调用次数的接缝。
type MatchRunner interface {
	OnCreated(ctx context.Context, target *model.ItemDetail) (*[]MatchHit, *int)
	OnUpdated(ctx context.Context, target *model.ItemDetail)
}

// ItemLookup 是「只要按 id 取一条帖子」的那些服务共用的接缝。
//
// 三处用它，且三处的判断都只依赖帖子本身的三个属性（类型、状态、作者）：
//   - #20 匹配（Match.OnCreated / Matches）—— 要目标帖的全部字段
//   - #21 解锁、#22 名单（Contact）—— 要类型、状态、user_id
//   - #41 举报（Report）—— 要存在性和状态
//
// 为什么不直接用 ItemStore（它有八个方法）：给这三个服务写单测的 fake
// 就得实现六个永远用不到的方法，那种 fake 会让人干脆不写这个测试（§10 第①层的原话）。
// 反过来才是关键：**接口里没有写方法，服务就越权不了**。
// Contact 服务拿不到 Create/Update/Delete，所以「解锁时顺手改一下帖子状态」
// 这种代码在类型层面就写不出来。
type ItemLookup interface {
	GetByID(ctx context.Context, id int64) (*model.ItemDetail, error)
}

// Item 是帖子的业务规则：校验、权限、contact 可见性、软删语义、图片删除。
type Item struct {
	items   ItemStore
	dict    DictLookup
	uploads *Upload
	match   MatchRunner
	// contacts 只为 #15 的第三条可见性规则存在（「已解锁过就放行」），只读、不写。
	// M2 那版这里什么都没有，因为当时全站没有任何代码能往 contact_views 写行；
	// M4 加了 #21 之后，这一格必须接上，否则 found 帖的 contact 对全世界永远是 null，
	// 而定位原则 2 说的恰恰是「联系方式是能联系到本人的唯一选项」。
	contacts ContactViewLookup
	logger   *slog.Logger
}

func NewItem(items ItemStore, dict DictLookup, uploads *Upload, match MatchRunner,
	contacts ContactViewLookup, logger *slog.Logger) *Item {
	if logger == nil {
		logger = slog.Default()
	}
	return &Item{items: items, dict: dict, uploads: uploads, match: match, contacts: contacts, logger: logger}
}

// ---------- 请求/响应形状 ----------

// CreateItemInput 是 #13 POST /api/items 的输入。
type CreateItemInput struct {
	ItemType string
	ItemFields
	ImagePaths []string
}

// CreateResult 是 #13 的 data。
//
// 后两个字段是**互斥的可选**字段（计划 §4 #13 与「#13 的响应为什么有两个互斥的
// 可选字段」一节）：lost 帖只带 matches_preview，found 帖只带 notified_count。
//
// 两个都是指针而不是「值 + omitempty」，为的是能表达「空数组也要出现」：
//   - MatchesPreview 用值类型 + omitempty 的话，一条都没匹配上时整个键会消失
//     （Go 的 omitempty 把 len==0 的切片也算空），而 §13 验收步骤明确写着
//     库里还没有 found 帖时响应要带 `matches_preview: []` —— 前端要靠「键在但为空」
//     显示「已经登记好了，之后有人发拾物帖匹配上了我们会通知你」，
//     键消失了它就只能显示一个空白区块。
//   - NotifiedCount 用值类型的话 found 帖的 0 和「这不是 found 帖」完全一样，
//     而冒烟场景里「建了一条谁都没匹配上的 found 帖 → notified_count:0」
//     断言的正是「0 出现了」而不是「键消失了」。
type CreateResult struct {
	Item model.ItemView `json:"item"`

	MatchesPreview *[]MatchHit `json:"matches_preview,omitempty"` // 仅 item_type=lost，top 5
	NotifiedCount  *int        `json:"notified_count,omitempty"`  // 仅 item_type=found，被推了 new_match 的 lost 作者数
}

// UpdateItemInput 是 #16 PUT /api/items/:id 的输入。
//
// ImagePaths 是指针切片：nil = 请求里没带这个字段（图片保持原样），
// 空切片 = 明确要求把图片全删掉。理由见 repo.UpdateItemRow 的注释。
type UpdateItemInput struct {
	ItemFields
	ImagePaths  *[]string
	AdminReason string // 仅当操作者是 admin 且不是帖主时必填
}

// StatusResult 是 #18 PATCH /api/items/:id/status 的 data：{id, status}。
type StatusResult struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
}

// ---------- #13 发帖 ----------

// Create 发一条帖子。
//
// 流程刻意是「先全部校验、再写库」：校验分散在写库之后的话，一次失败会留下
// 半条数据（帖子建好了但图片没插上），而那种残留没人会去清。
func (s *Item) Create(ctx context.Context, userID int64, in CreateItemInput) (*CreateResult, error) {
	if in.ItemType != model.ItemTypeLost && in.ItemType != model.ItemTypeFound {
		return nil, apperr.Validation("帖子类型不对",
			apperr.FieldError{Field: "item_type", Msg: "只能是 lost 或 found"})
	}

	norm, err := normalizeItemFields(in.ItemType, in.ItemFields)
	if err != nil {
		return nil, err
	}
	if err := validateImagePaths(in.ImagePaths); err != nil {
		return nil, err
	}
	if err := s.checkDictRefs(ctx, in.CategoryID, in.LocationID); err != nil {
		return nil, err
	}

	d, err := s.items.Create(ctx, repo.NewItemRow{
		ItemType:       in.ItemType,
		UserID:         userID,
		Title:          norm.Title,
		Description:    norm.Description,
		CategoryID:     in.CategoryID,
		LocationID:     in.LocationID,
		LocationDetail: norm.LocationDetail,
		LastSeenAt:     norm.LastSeenAt,
		LostAt:         norm.LostAt,
		FoundAt:        norm.FoundAt,
		Contact:        norm.Contact,
		ImagePaths:     in.ImagePaths,
	})
	if err != nil {
		return nil, err
	}

	// ---- M3：发帖后的匹配，方向由 item_type 决定（§5.8 那张表的落地）----
	//
	// 位置在「帖子已经落库」之后、构造响应之前，且**同步执行**（不开 goroutine）：
	// found 帖的 notified_count 是响应体的一部分，异步就算不出这个数；
	// 而 lost 帖的 matches_preview 是发帖成功页要直接渲染的东西（§2.6.2 第 3 步），
	// 用户点完提交就看到结果，不该等一次轮询。
	//
	// ⚠ 这个调用**不可能往数据库写 posts 以外的东西**：Match.OnCreated 里
	// lost 分支根本不碰 RecordMatches（§4 的实现检查点：matches_preview 分支
	// 不允许出现任何 INSERT）。这一点在 M3 的集成测试里用「发一条 lost 帖之后
	// match_pairs 和 notifications 都是 0 行」钉死。
	preview, notified := s.match.OnCreated(ctx, d)

	s.logger.InfoContext(ctx, "item.create",
		slog.Int64("item_id", d.ID),
		slog.String("item_type", d.ItemType),
		slog.Int64("user_id", userID),
		slog.Int64("category_id", d.CategoryID),
		slog.Int64("location_id", d.LocationID),
		slog.Int("images", len(in.ImagePaths)))

	// 自己的新帖子：联系方式一定可见，不存在「解锁」这回事
	view, err := s.buildView(ctx, d, nil, true)
	if err != nil {
		return nil, err
	}
	return &CreateResult{Item: *view, MatchesPreview: preview, NotifiedCount: notified}, nil
}

// ---------- #15 详情 ----------

// Detail 取一条帖子的详情，按可见性规则决定 contact 给不给。
//
// viewer 是 nil 表示未登录（#15 是公开接口，挂的是 OptionalJWT）。
func (s *Item) Detail(ctx context.Context, viewer *model.User, id int64) (*model.ItemView, error) {
	d, err := s.items.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	if d.Status == model.ItemStatusDeleted && !canSeeDeleted(viewer, d.UserID) {
		// 报 NOT_FOUND 而不是 FORBIDDEN：软删的帖子对外就该是「不存在」。
		// 返回 403 等于确认了「这个 id 曾经有过一条被删的帖子」—— 那是治理信息。
		// 同一条纪律也用在登录接口上（不区分「用户名不存在」和「密码错」）。
		return nil, apperr.NotFound("帖子")
	}

	// admin 也没有额外的联系方式可见性：#15 的规则里没有 admin 分支。
	// 他当然可以从 Adminer 里看到 contact 列，但那是「数据库增删改查」，
	// 走 API 这条路不该给他一个绕过解锁记录的通道 —— 否则 contact_views
	// 这份审计日志就不再完整了（定位原则 5）。
	return s.buildView(ctx, d, viewer, false)
}

// ---------- #16 改帖 ----------

// Update 改一条帖子。
//
// ⚠ 不能改的两样东西：item_type（请求体里没有这个字段，类型从库里已有的行取）
// 和作者（repo.UpdateItemRow 里压根没有 user_id）。
// 「归属可以转让」是这个系统最不该有的能力，所以它在类型层面就不存在。
//
// found 帖改完会重新跑一次匹配（Match.OnUpdated），lost 帖不会 ——
// 这个不对称是 §5.8 的规矩在改帖上的延续：lost 方向的匹配任何时候都只算不写。
func (s *Item) Update(ctx context.Context, actor *model.User, id int64, in UpdateItemInput) (*model.ItemView, error) {
	d, err := s.items.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}

	asAdmin, err := authorizeItemWrite(actor, d.UserID, in.AdminReason)
	if err != nil {
		return nil, err
	}

	// ITEM_CLOSED 只挡 deleted：open 和 closed 都能改（计划 §8 对这个码的说明）。
	// 理由在那里写得很清楚 —— 匹配候选只看 open，改一条 closed 帖不会污染任何结果；
	// 而「东西已经还回来了，但描述里写错了电话」这种情况必须能改。
	if d.Status == model.ItemStatusDeleted {
		return nil, apperr.NewMsg(apperr.CodeItemClosed, "帖子已被删除，无法修改")
	}

	norm, err := normalizeItemFields(d.ItemType, in.ItemFields)
	if err != nil {
		return nil, err
	}
	if in.ImagePaths != nil {
		if err := validateImagePaths(*in.ImagePaths); err != nil {
			return nil, err
		}
	}
	if err := s.checkDictRefs(ctx, in.CategoryID, in.LocationID); err != nil {
		return nil, err
	}

	updated, err := s.items.Update(ctx, id, repo.UpdateItemRow{
		Title:          norm.Title,
		Description:    norm.Description,
		CategoryID:     in.CategoryID,
		LocationID:     in.LocationID,
		LocationDetail: norm.LocationDetail,
		LastSeenAt:     norm.LastSeenAt,
		LostAt:         norm.LostAt,
		FoundAt:        norm.FoundAt,
		Contact:        norm.Contact,
		ImagePaths:     in.ImagePaths,
	})
	if err != nil {
		return nil, err
	}

	attrs := []any{
		slog.Int64("item_id", id),
		slog.Int64("actor_id", actor.ID),
		slog.Bool("as_admin", asAdmin),
	}
	if asAdmin {
		// admin 动别人的数据行是治理动作，级别提到 WARN 并带上理由。
		// 计划 §4：#16 的 admin 分支必须补写 admin_actions —— 那是 M6 的活，
		// 但日志这一半现在就要有，否则 M6 之前的这段时间里 admin 改帖完全无痕。
		attrs = append(attrs, slog.Int64("owner_id", d.UserID), slog.String("reason", in.AdminReason))
		s.logger.WarnContext(ctx, "item.update_by_admin", attrs...)
	} else {
		s.logger.InfoContext(ctx, "item.update", attrs...)
	}

	// 改准了内容就可能配上一个原本没配上的失主，所以这一步不能只属于 #13。
	// 传的是**回读出来的那一条**而不是请求体：类型、状态、字典祖先列都只有库里有，
	// 而匹配方向（found 才写库）和 closed 不通知这两道门槛都在 Match.OnUpdated 里判 ——
	// 这里无条件调用，规则只有一处，将来加第三种 item_type 也只用改 Match。
	//
	// ⚠ 顺序：必须在 items.Update 提交之后。台账的外键指向 items(id)，
	// 而 score/breakdown 要按改完的值算；反过来写在事务里，一旦匹配报错就会牵连改帖本身
	// （§5.8 禁止的那种失败传播）。放在提交后，最坏情况是「帖子改好了、通知没发出去」。
	s.match.OnUpdated(ctx, updated)

	// 操作者刚刚把 contact 作为请求体的一部分提交上来，所以响应里一定回给他 ——
	// 否则他改完自己（或别人）的帖子，看到的却是一个 null，会以为改丢了。
	return s.buildView(ctx, updated, nil, true)
}

// ---------- #17 删帖 ----------

// Delete 软删一条帖子：status → deleted，**不物理删除**。
//
// 软删的理由（§3.1）：物理删除会丢历史，而 item_images / contact_views /
// item_returns / match_pairs / reports 五张表都用外键指着 items，
// CASCADE 下去就是一次连带清库。留着行，这些引用全都还有意义。
func (s *Item) Delete(ctx context.Context, actor *model.User, id int64, adminReason string) error {
	d, err := s.items.GetByID(ctx, id)
	if err != nil {
		return err
	}

	asAdmin, err := authorizeItemWrite(actor, d.UserID, adminReason)
	if err != nil {
		return err
	}
	if d.Status == model.ItemStatusDeleted {
		// 已经删过了。返回 ITEM_CLOSED 而不是当成功：重复删通常是前端连点两次，
		// 让用户看到「这条已经删了」比让他以为又删了一次更清楚。
		return apperr.NewMsg(apperr.CodeItemClosed, "帖子已被删除")
	}

	if err := s.items.SetStatus(ctx, id, model.ItemStatusDeleted); err != nil {
		return err
	}

	attrs := []any{
		slog.Int64("item_id", id),
		slog.Int64("actor_id", actor.ID),
		slog.Bool("as_admin", asAdmin),
	}
	if asAdmin {
		// 同 Update：M6 会在这里补 admin_actions，日志这一半现在就有
		attrs = append(attrs, slog.Int64("owner_id", d.UserID), slog.String("reason", adminReason))
		s.logger.WarnContext(ctx, "item.delete_by_admin", attrs...)
	} else {
		s.logger.InfoContext(ctx, "item.delete", attrs...)
	}
	return nil
}

// ---------- #18 开帖/关帖 ----------

// ChangeStatus 由**发帖人本人**开关自己的帖子。
//
// ⚠ admin 也不行，这是计划 §4 第 18 行「Auth = JWT（本人）」的直接含义，
// 也是 M6 那条 TestAdminBlockedFromOwnerOnlyRoutes 要断言的三个路由之一。
// 关掉一条帖子是「这东西已经还回来了」这个**社区事实**的表态，
// 只有发帖人有资格表态；admin 能销毁内容，但制造不出归属（定位原则 5）。
// admin 想让一条帖子从广场消失，用的是 #43 批量下架（status→deleted），
// 那是一次治理动作，会落 admin_actions、会给作者发通知，和「关帖」是两件事。
func (s *Item) ChangeStatus(ctx context.Context, actor *model.User, id int64, status string) (*StatusResult, error) {
	if status != model.ItemStatusOpen && status != model.ItemStatusClosed {
		return nil, apperr.Validation("status 不对",
			apperr.FieldError{Field: "status", Msg: "只能是 open 或 closed"})
	}

	d, err := s.items.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if actor.ID != d.UserID {
		return nil, apperr.Forbidden("只能操作自己发布的帖子")
	}
	if d.Status == model.ItemStatusDeleted {
		return nil, apperr.NewMsg(apperr.CodeItemClosed, "帖子已被删除，无法修改状态")
	}
	if d.Status == status {
		// 幂等：已经是这个状态了就当成功。前端连点两次不该收到一个 409。
		return &StatusResult{ID: id, Status: status}, nil
	}

	if err := s.items.SetStatus(ctx, id, status); err != nil {
		return nil, err
	}
	s.logger.InfoContext(ctx, "item.status",
		slog.Int64("item_id", id),
		slog.Int64("user_id", actor.ID),
		slog.String("from", d.Status),
		slog.String("to", status))
	return &StatusResult{ID: id, Status: status}, nil
}

// ---------- #14 / #19 列表 ----------

// ListPublic 是 #14 GET /api/items（广场，公开）。
//
// contact 的填充规则：**lost 帖带、found 帖一律 null，不管是谁在查**（计划 §4 第 14 行）。
// 自己的 found 帖在广场上也是锁着的 —— 这样 #14 完全不需要身份，
// 公开、可缓存，也不用为「这一行是不是我的」多发一次查询。
// 想看自己帖子的联系方式去 #19。
func (s *Item) ListPublic(ctx context.Context, q ListQuery) (*Page[model.ItemSummary], error) {
	f, err := parseListQuery(q, publicListStatus)
	if err != nil {
		return nil, err
	}
	rows, total, err := s.items.List(ctx, f)
	if err != nil {
		return nil, err
	}

	list := make([]model.ItemSummary, 0, len(rows))
	for i := range rows {
		var contact *string
		if rows[i].ItemType == model.ItemTypeLost {
			contact = &rows[i].Contact
		}
		list = append(list, rows[i].Summary(contact, s.uploads.URL(rows[i].CoverPath)))
	}
	return &Page[model.ItemSummary]{List: list, Total: total, Page: f.Page, PageSize: f.PageSize}, nil
}

// ListMine 是 #19 GET /api/my/items（我的发布）。
// contact 一律带上 —— 都是自己的帖子，锁着没有任何意义（计划 §4 第 19 行）。
func (s *Item) ListMine(ctx context.Context, userID int64, q ListQuery) (*Page[model.ItemSummary], error) {
	f, err := parseListQuery(q, mineListStatus)
	if err != nil {
		return nil, err
	}
	f.UserID = &userID

	rows, total, err := s.items.List(ctx, f)
	if err != nil {
		return nil, err
	}

	list := make([]model.ItemSummary, 0, len(rows))
	for i := range rows {
		list = append(list, rows[i].Summary(&rows[i].Contact, s.uploads.URL(rows[i].CoverPath)))
	}
	return &Page[model.ItemSummary]{List: list, Total: total, Page: f.Page, PageSize: f.PageSize}, nil
}

// ---------- #42 帖主自删单张图片 ----------

// DeleteImage 删掉一张图片：数据库一行 + 磁盘一个文件。**不动帖子本身。**
//
// 计划 §4：「#42 和 #45 是一对 —— 帖主自删图（隐私泄露时最快的解法，不用等 admin），
// admin 删图（帖主不配合或已被封号时）。两者都只删 item_images 一行 + 磁盘文件，
// 不动帖子本身 —— 真的捡到东西的人不该因为一张照片有问题就丢掉整条帖子。」
//
// 顺序是「先删库、后删文件」，反过来会留下更糟的状态：
// 文件先没了、库里那行还在 → 前端渲染出一张碎图，而数据库看起来完全正常，
// 没有任何线索指向「磁盘上少了一个文件」。
// 先删库的话，万一文件删不掉，得到的只是一个**谁也看不见的**孤儿文件 ——
// 它不在任何响应里，占点磁盘而已，所以这里只记一条 WARN，不把整个请求报成失败。
func (s *Item) DeleteImage(ctx context.Context, actor *model.User, imageID int64) error {
	im, ownerID, err := s.items.ImageWithOwner(ctx, imageID)
	if err != nil {
		return err
	}
	if actor.ID != ownerID {
		// admin 也不行。#42 的 Auth 列写的是「仅帖主本人」，admin 删图是 #45（M6）。
		// 分成两个端点不是为了麻烦，是因为这两件事的**含义**不同：
		// 帖主删图是「我不想让这张照片挂在这儿」，admin 删图是一次治理动作，
		// 要填理由、要落 admin_actions。合成一个端点就分不出是谁删的了。
		return apperr.Forbidden("只能删除自己帖子上的图片")
	}

	path, err := s.items.DeleteImage(ctx, imageID)
	if err != nil {
		return err
	}
	if err := s.uploads.Remove(path); err != nil {
		s.logger.WarnContext(ctx, "item.image_orphaned",
			slog.Int64("image_id", imageID),
			slog.String("path", path),
			slog.String("err", err.Error()))
	}

	s.logger.InfoContext(ctx, "item.image_deleted",
		slog.Int64("image_id", imageID),
		slog.Int64("item_id", im.ItemID),
		slog.Int64("user_id", actor.ID),
		slog.String("path", path))
	return nil
}

// ---------- 内部辅助 ----------

// checkDictRefs 校验分类和地点这两次引用是否合法。
//
// 两条规则来自计划 §3.2：category_id 必须是**小类**（level 2），
// location_id 必须是**叶子节点**。
//
// 为什么要求小类：分类的半分机制（同大类不同小类给 0.5）只在两级都确定时才算得出来。
// 如果允许选大类，「数码电子」和「手机」之间就没法判断到底是不是同一个小类，
// 匹配算法会退化成一堆 0.5 的噪声。
//
// 为什么叶子的定义是「没有子节点」而不是「level=3」：「其他」是 level=1 且
// is_freeform=true 的一级叶子，它必须能被选中（计划 §3.6 专门为它写了前端红色警告）。
// 按 level 判会把它整个拒掉。
func (s *Item) checkDictRefs(ctx context.Context, categoryID, locationID int64) error {
	c, err := s.dict.GetCategory(ctx, categoryID)
	if err != nil {
		return s.dictRefError(err, "category_id", "分类")
	}
	if !c.IsActive {
		return apperr.Validation("这个分类已经停用了",
			apperr.FieldError{Field: "category_id", Msg: "请重新从分类列表里选一个"})
	}
	if c.Level != 2 {
		return apperr.Validation("分类必须选到小类",
			apperr.FieldError{Field: "category_id", Msg: "「" + c.Name + "」是大类，请再往下选一级"})
	}

	l, err := s.dict.GetLocation(ctx, locationID)
	if err != nil {
		return s.dictRefError(err, "location_id", "地点")
	}
	if !l.IsActive {
		return apperr.Validation("这个地点已经停用了",
			apperr.FieldError{Field: "location_id", Msg: "请重新从地点列表里选一个"})
	}
	children, err := s.dict.CountActiveChildren(ctx, locationID)
	if err != nil {
		return err
	}
	if children > 0 {
		return apperr.Validation("地点必须选到最具体的一级",
			apperr.FieldError{Field: "location_id", Msg: "「" + l.Name + "」下面还有 " +
				"更具体的地点，请再往下选一级（选不到就选「其他」并填最近的建筑）"})
	}
	return nil
}

// dictRefError 把字典查询的错误翻译成字段级 VALIDATION。
//
// NOT_FOUND 要换码：对发帖表单来说「你选的分类不存在」是一个**字段错误**
// （前端要把焦点移回分类选择器），不是「你要访问的资源不存在」。
// 保持 404 的话，前端那个统一的「404 就跳首页/提示资源不存在」的拦截器
// 会把用户从填了一半的表单里踢出去。
func (s *Item) dictRefError(err error, field, what string) error {
	if apperr.IsCode(err, apperr.CodeNotFound) {
		return apperr.Validation(what+"不存在",
			apperr.FieldError{Field: field, Msg: "请重新从" + what + "列表里选一个"})
	}
	return err
}

// buildView 把 repo 的一行组装成 #15 的响应形状。
//
// forceContact=true 用于「操作者刚刚把 contact 提交上来」的两个场合
// （#13 发帖、#16 改帖）：那时候再按可见性规则给他一个 null 是荒谬的。
func (s *Item) buildView(ctx context.Context, d *model.ItemDetail, viewer *model.User, forceContact bool) (*model.ItemView, error) {
	imgs, err := s.items.ListImages(ctx, d.ID)
	if err != nil {
		return nil, err
	}

	views := make([]model.ImageView, 0, len(imgs))
	for _, im := range imgs {
		views = append(views, model.ImageView{
			ID:        im.ID,
			URL:       s.uploads.URL(im.Path),
			SortOrder: im.SortOrder,
		})
	}

	locked := s.contactLockedAfterLookup(ctx, d, viewer, forceContact)
	var contact *string
	if !locked {
		contact = &d.Contact
	}

	v := d.View(contact, locked, views)
	return &v, nil
}

// contactLockedAfterLookup 是计划 §4「#15 的 contact 可见性规则」的完整落地。
//
// 前半段是纯函数 contactLocked（只看类型和身份），后半段是那次查库。
// 分成两段的理由写在 contactLocked 的注释里：那两条纯规则能被表驱动单测扫尽，
// 而「查库」这一步只能拿 fake store 测，两段的测法本来就不该混在一起。
//
// 三条不打扰的规则按代价从低到高排：
//   - lost 帖 / 作者本人 → 纯函数就返回 false，**一次查询都不发**
//   - 匿名的 found 请求 → 直接锁，也不发查询（没有 id 可查，发了是白费）
//     这是 #15 作为公开接口的常态路径，省掉这一次往返是有意义的
//   - 已登录的非作者 → 才走 idx_contact_views_user 那一次点查
//
// ⚠ 查询失败一律**按锁着处理**，并且不把错误往上抛：
//   - 往上抛意味着 #15 这个「未登录也能逛」的公开接口在 contact_views 出问题时
//     整条帖子读不出来 —— 用户看到的是广场点进详情就 500。
//   - 反过来，「查询失败但放行」是把别人的手机号发出去。
//     两个后果不对称：前者是体验受损，后者是隐私事故，所以这里 fail closed。
//     失败本身进日志，不静默。
func (s *Item) contactLockedAfterLookup(ctx context.Context, d *model.ItemDetail, viewer *model.User, forceContact bool) bool {
	if forceContact || !contactLocked(&d.Item, viewer) {
		return false
	}
	if viewer == nil {
		return true
	}

	viewed, err := s.contacts.Viewed(ctx, d.ID, viewer.ID)
	if err != nil {
		s.logger.WarnContext(ctx, "contact.viewed_lookup_failed",
			slog.Any("err", err),
			slog.Int64("item_id", d.ID),
			slog.Int64("user_id", viewer.ID),
			slog.String("action", "按锁着处理（fail closed）"),
		)
		return true
	}
	return !viewed
}

// canSeeDeleted 报告 viewer 能不能看一条已软删的帖子。
//
// 只有作者本人和 admin。作者要能看见自己帖子被下架了（否则他会以为帖子凭空消失），
// admin 要能核对治理结果。其他任何人一律看不到 —— 软删对外就是不存在。
func canSeeDeleted(viewer *model.User, ownerID int64) bool {
	if viewer == nil {
		return false
	}
	return viewer.ID == ownerID || viewer.IsAdmin()
}

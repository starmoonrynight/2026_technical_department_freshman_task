package handler

import (
	"github.com/gin-gonic/gin"

	"lostfound/internal/apperr"
	"lostfound/internal/service"
)

// Item 对应 §4 的 #13–#19 和 #42。
//
// 和 Auth 一样，这一层只做 HTTP 翻译：绑定 → 调 service → 写响应。
// 「谁能改这条帖子」「contact 给不给」「closed 的帖子能不能改」全部在 service，
// 因为那些规则要能被不起 HTTP server 的单元测试覆盖（§10 第①层）。
type Item struct {
	Svc *service.Item
}

// ---------- 请求体 ----------

// createItemReq 是 #13 的请求体，字段名和顺序照计划 §4 第 13 行。
//
// 三个时间字段是 **string** 而不是 time.Time：格式错误要能报出
// 「哪个字段、正确的格式长什么样」，交给 encoding/json 自动解析的话
// 用户会收到一句英文的 `parsing time "..." as ...: cannot parse`。
type createItemReq struct {
	ItemType       string   `json:"item_type"`
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	CategoryID     int64    `json:"category_id"`
	LocationID     int64    `json:"location_id"`
	LocationDetail string   `json:"location_detail"`
	LastSeenAt     string   `json:"last_seen_at"`
	LostAt         string   `json:"lost_at"`
	FoundAt        string   `json:"found_at"`
	Contact        string   `json:"contact"`
	ImagePaths     []string `json:"image_paths"`
}

// updateItemReq 是 #16 的请求体：同 #13 但**没有 item_type**（改帖不能改类型），
// 多一个 admin_reason。
//
// ImagePaths 是指针切片，用来区分「请求里没带这个字段」和「明确传了一个空数组」：
// 前者保持图片不变，后者把图片全删掉。encoding/json 的行为正好对得上 ——
// 字段缺失或 null → nil，[] → 指向空切片的指针。
type updateItemReq struct {
	Title          string    `json:"title"`
	Description    string    `json:"description"`
	CategoryID     int64     `json:"category_id"`
	LocationID     int64     `json:"location_id"`
	LocationDetail string    `json:"location_detail"`
	LastSeenAt     string    `json:"last_seen_at"`
	LostAt         string    `json:"lost_at"`
	FoundAt        string    `json:"found_at"`
	Contact        string    `json:"contact"`
	ImagePaths     *[]string `json:"image_paths"`
	AdminReason    string    `json:"admin_reason"`
}

type changeStatusReq struct {
	Status string `json:"status"`
}

// deleteItemReq 是 #17 的请求体。
// 只有 admin 在删别人的帖子时才需要填 admin_reason；帖主删自己的帖子可以什么都不带。
type deleteItemReq struct {
	AdminReason string `json:"admin_reason"`
}

// fields 把请求体里那 9 个共用字段抽出来，Create 和 Update 都靠它。
//
// 抽一层不是为了省字：是为了让「发帖和改帖校验的是同一套规则」这件事
// 在类型上成立。两边各写一遍字段的话，将来给发帖加一条校验、忘了改帖，
// 就出现「新建时不许这样、编辑时却能改成这样」的漏洞 —— 而它不会被任何测试发现，
// 因为两条路径各自的测试都是绿的。
func (r createItemReq) fields() service.ItemFields {
	return service.ItemFields{
		Title:          r.Title,
		Description:    r.Description,
		CategoryID:     r.CategoryID,
		LocationID:     r.LocationID,
		LocationDetail: r.LocationDetail,
		LastSeenAt:     r.LastSeenAt,
		LostAt:         r.LostAt,
		FoundAt:        r.FoundAt,
		Contact:        r.Contact,
	}
}

func (r updateItemReq) fields() service.ItemFields {
	return service.ItemFields{
		Title:          r.Title,
		Description:    r.Description,
		CategoryID:     r.CategoryID,
		LocationID:     r.LocationID,
		LocationDetail: r.LocationDetail,
		LastSeenAt:     r.LastSeenAt,
		LostAt:         r.LostAt,
		FoundAt:        r.FoundAt,
		Contact:        r.Contact,
	}
}

// ---------- #13 发帖 ----------

// Create 处理 #13 POST /api/items（JWT）。
func (h Item) Create(c *gin.Context) {
	var req createItemReq
	if err := bindJSON(c, &req); err != nil {
		apperr.Respond(c, err)
		return
	}

	res, err := h.Svc.Create(c.Request.Context(), apperr.UserID(c), service.CreateItemInput{
		ItemType:   req.ItemType,
		ItemFields: req.fields(),
		ImagePaths: req.ImagePaths,
	})
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, res)
}

// ---------- #14 广场列表 / #19 我的发布 ----------

// List 处理 #14 GET /api/items（公开）。
func (h Item) List(c *gin.Context) {
	page, err := h.Svc.ListPublic(c.Request.Context(), listQueryFrom(c))
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, page)
}

// ListMine 处理 #19 GET /api/my/items（JWT）。
//
// 计划里 #19 的参数只有 item_type/status/page/page_size，这里把 #14 的八个全读了 ——
// 因为读法是同一个函数，多支持 keyword/category/location 不增加一行代码，
// 而「在我的发布里搜一下」是真实需求。反过来，如果只读四个，
// 用户传了 keyword 却**被静默忽略**，那才是最难受的行为：
// 他以为搜过了没结果，实际上是根本没搜。
func (h Item) ListMine(c *gin.Context) {
	page, err := h.Svc.ListMine(c.Request.Context(), apperr.UserID(c), listQueryFrom(c))
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, page)
}

// listQueryFrom 把查询参数原样收进 ListQuery，**一个都不解析**。
// 解析和校验在 service.parseListQuery 里，那里才能给出带字段名的中文 VALIDATION。
func listQueryFrom(c *gin.Context) service.ListQuery {
	return service.ListQuery{
		ItemType:   c.Query("item_type"),
		Keyword:    c.Query("keyword"),
		CategoryID: c.Query("category_id"),
		LocationID: c.Query("location_id"),
		Status:     c.Query("status"),
		Sort:       c.Query("sort"),
		Page:       c.Query("page"),
		PageSize:   c.Query("page_size"),
	}
}

// ---------- #15 详情 ----------

// Detail 处理 #15 GET /api/items/:id（**公开**，挂 OptionalJWT）。
//
// 公开但要知道「当前是谁在看」，因为 contact 的可见性规则有「发帖人本人」和
// 「已解锁过」两个分支（计划 §4）。OptionalJWT 的作用就是：token 有效就认出来，
// 没有或无效就当匿名，绝不因为这个端点是公开的就返 401。
func (h Item) Detail(c *gin.Context) {
	id, err := pathID(c, "id", "帖子")
	if err != nil {
		apperr.Respond(c, err)
		return
	}

	view, err := h.Svc.Detail(c.Request.Context(), apperr.User(c), id)
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, view)
}

// ---------- #16 改帖 ----------

// Update 处理 #16 PUT /api/items/:id（JWT，本人或 Admin）。
func (h Item) Update(c *gin.Context) {
	id, err := pathID(c, "id", "帖子")
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	var req updateItemReq
	if err := bindJSON(c, &req); err != nil {
		apperr.Respond(c, err)
		return
	}

	view, err := h.Svc.Update(c.Request.Context(), apperr.User(c), id, service.UpdateItemInput{
		ItemFields:  req.fields(),
		ImagePaths:  req.ImagePaths,
		AdminReason: req.AdminReason,
	})
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, view)
}

// ---------- #17 删帖 ----------

// Delete 处理 #17 DELETE /api/items/:id（JWT，本人或 Admin）。软删。
func (h Item) Delete(c *gin.Context) {
	id, err := pathID(c, "id", "帖子")
	if err != nil {
		apperr.Respond(c, err)
		return
	}

	// 请求体是**可选**的：帖主删自己的帖子不需要任何理由，
	// 强制他发一个 `{}` 是没有意义的仪式（而 curl -X DELETE 默认就是不带 body 的）。
	// 只有 admin 删别人的帖子时才必须有 admin_reason，那条校验在 service 里。
	var req deleteItemReq
	if err := bindJSONOptional(c, &req); err != nil {
		apperr.Respond(c, err)
		return
	}

	if err := h.Svc.Delete(c.Request.Context(), apperr.User(c), id, req.AdminReason); err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, nil)
}

// ---------- #18 开帖/关帖 ----------

// ChangeStatus 处理 #18 PATCH /api/items/:id/status（JWT，**仅本人**）。
func (h Item) ChangeStatus(c *gin.Context) {
	id, err := pathID(c, "id", "帖子")
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	var req changeStatusReq
	if err := bindJSON(c, &req); err != nil {
		apperr.Respond(c, err)
		return
	}

	res, err := h.Svc.ChangeStatus(c.Request.Context(), apperr.User(c), id, req.Status)
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, res)
}

// ---------- #42 帖主自删单张图片 ----------

// DeleteImage 处理 #42 DELETE /api/item-images/:id（JWT，**仅帖主本人**）。
//
// 注意路径是 /api/item-images/:id 而不是 /api/items/:id/images/:imageId ——
// 图片自己的 id 已经足够定位，多套一层只会让前端多存一个字段。
func (h Item) DeleteImage(c *gin.Context) {
	id, err := pathID(c, "id", "图片")
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	if err := h.Svc.DeleteImage(c.Request.Context(), apperr.User(c), id); err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, nil)
}

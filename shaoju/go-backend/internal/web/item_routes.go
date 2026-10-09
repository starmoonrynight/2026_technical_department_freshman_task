package web

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"lostfound/internal/config"
	"lostfound/internal/httpx"
	"lostfound/internal/models"
	"lostfound/internal/store"
	"lostfound/internal/validate"
)

var (
	itemTypes     = []string{"lost", "found"}
	itemStatuses  = store.ItemStatuses // open(寻找中) / found(已找到) / closed(已结束)
	auditStatuses = []string{"pending", "approved", "rejected"}
	userStatuses  = []string{"active", "disabled"}
	userRoles     = []string{"user", "admin"}
)

// registerItemRoutes 注册失物 / 招领信息的 CRUD 路由。
//
// 权限分三层：
//
//	公开        GET  /api/items            列表（只含已审核通过的）
//	           GET  /api/items/categories  分类字典
//	           GET  /api/items/:id         详情（未过审的仅本人与管理员可见）
//	登录即可    POST /api/items            创建
//	           GET  /api/items/mine        我的发布（含待审核与已驳回）
//	本人/管理员 PUT    /api/items/:id       整体替换
//	           PATCH  /api/items/:id       局部更新
//	           POST   /api/items/:id/status 标记已解决 / 重新开放
//	           DELETE /api/items/:id       删除
//
// 「本人/管理员」由 RequireItemOwner 中间件用会话用户 ID 与 items.user_id 比对后放行。
func registerItemRoutes(g *gin.RouterGroup, deps *Deps) {
	// 公开读接口
	g.GET("", handle(func(c *gin.Context) error { return itemsPublicList(c, deps) }))
	g.GET("/categories", handle(func(c *gin.Context) error { return itemsCategories(c) }))
	// /search 与根路径完全等价，只是让「搜索」这个意图在 URL 上更直观，便于前端书写
	g.GET("/search", handle(func(c *gin.Context) error { return itemsPublicList(c, deps) }))

	// 需要登录
	authed := g.Group("", RequireAuth())
	authed.POST("", handle(func(c *gin.Context) error { return itemsCreate(c, deps) }))
	authed.GET("/mine", handle(func(c *gin.Context) error { return itemsMine(c, deps) }))

	// 需要登录 + 必须是发布者本人（或管理员）
	owned := g.Group("/:id", RequireAuth(), RequireItemOwner(deps.Store))
	owned.PUT("", handle(func(c *gin.Context) error { return itemsUpdate(c, deps) }))
	owned.PATCH("", handle(func(c *gin.Context) error { return itemsPatch(c, deps) }))
	owned.POST("/status", handle(func(c *gin.Context) error { return itemsSetStatus(c, deps) }))
	owned.DELETE("", handle(func(c *gin.Context) error { return itemsDelete(c, deps) }))

	// 公开详情放在最后，避免与上面的 :id 子路由混淆
	g.GET("/:id", handle(func(c *gin.Context) error { return itemsDetail(c, deps) }))
}

// idParam 读取路径上的 :id。
func idParam(c *gin.Context) (int64, error) {
	id, err := validate.Int(c.Param("id"), "id", validate.IntOpts{Label: "ID"})
	if err != nil {
		return 0, err
	}
	return int64(id), nil
}

// readPage 解析分页参数。
//
// 参数名：page（页码，从 1 开始）、limit（每页数量）。
// pageSize 是历史别名，两者同时出现时以 limit 为准。
// 校验顺序与 Node 版一致：page 先于每页数量。
func readPage(c *gin.Context) (int, int, error) {
	page, err := validate.Int(QueryOrNil(c, "page"), "page", validate.IntOpts{
		Min: validate.P(1), Fallback: validate.P(1), Label: "页码",
	})
	if err != nil {
		return 0, 0, err
	}

	raw := QueryOrNil(c, "limit")
	if raw == nil {
		raw = QueryOrNil(c, "pageSize")
	}
	pageSize, err := validate.Int(raw, "limit", validate.IntOpts{
		Min:      validate.P(1),
		Max:      validate.P(config.MaxPageSize),
		Fallback: validate.P(config.DefaultPageSize),
		Label:    "每页数量",
	})
	if err != nil {
		return 0, 0, err
	}
	return page, pageSize, nil
}

// readSort 解析排序参数：sort 指定字段、order 指定方向，两者都有缺省值。
// 非法取值直接 400，而不是静默忽略，避免前端拼错参数却毫无察觉。
func readSort(c *gin.Context) (string, string, error) {
	sort, err := validate.OneOf(QueryOrNil(c, "sort"), "sort", store.ItemSortKeys,
		validate.OneOfOpts{Fallback: validate.P(store.DefaultItemSort), Label: "排序字段"})
	if err != nil {
		return "", "", err
	}

	order, err := validate.OneOf(QueryOrNil(c, "order"), "order", store.ItemSortOrders,
		validate.OneOfOpts{Fallback: validate.P("desc"), Label: "排序方向"})
	if err != nil {
		return "", "", err
	}
	return sort, order, nil
}

// readItemInput 校验并裁剪发布 / 修改所需的字段，顺序与 Node 版一致。
func readItemInput(c *gin.Context) (store.ItemInput, error) {
	var in store.ItemInput
	var err error

	if in.Type, err = validate.OneOf(FieldOrNil(c, "type"), "type", itemTypes, validate.OneOfOpts{Label: "信息类型"}); err != nil {
		return in, err
	}
	if in.Title, err = validate.Str(FieldOrNil(c, "title"), "title", validate.StrOpts{Required: true, Max: 80, Label: "标题"}); err != nil {
		return in, err
	}
	if in.Category, err = validate.Str(FieldOrNil(c, "category"), "category", validate.StrOpts{Max: 20, Label: "分类"}); err != nil {
		return in, err
	}
	if in.Category == "" {
		in.Category = "其他"
	}
	if in.Description, err = validate.Str(FieldOrNil(c, "description"), "description", validate.StrOpts{Max: 2000, Label: "详细描述"}); err != nil {
		return in, err
	}
	if in.Location, err = validate.Str(FieldOrNil(c, "location"), "location", validate.StrOpts{Max: 120, Label: "地点"}); err != nil {
		return in, err
	}
	if in.StoragePlace, err = validate.Str(FieldOrNil(c, "storagePlace"), "storagePlace", validate.StrOpts{Max: 120, Label: "寄放处"}); err != nil {
		return in, err
	}
	if in.HappenedAt, err = validate.Str(FieldOrNil(c, "happenedAt"), "happenedAt", validate.StrOpts{Max: 40, Label: "时间"}); err != nil {
		return in, err
	}
	if in.Contact, err = validate.Str(FieldOrNil(c, "contact"), "contact", validate.StrOpts{Max: 120, Label: "联系方式"}); err != nil {
		return in, err
	}
	if in.ImageURL, err = validate.ImageURL(FieldOrNil(c, "imageUrl")); err != nil {
		return in, err
	}
	return in, nil
}

// readItemPatchInput 局部更新：只覆盖请求体里显式出现的字段，其余沿用当前值。
// 与 PUT 的区别是「缺省即保留」，而不是「缺省即清空」。
func readItemPatchInput(c *gin.Context, row *store.ItemRow) (store.ItemInput, error) {
	in := store.ItemInput{
		Type:         row.Type,
		Title:        row.Title,
		Category:     row.Category,
		Description:  row.Description,
		Location:     row.Location,
		StoragePlace: row.StoragePlace,
		HappenedAt:   row.HappenedAt,
		Contact:      row.Contact,
		ImageURL:     row.ImageURL,
	}

	var err error
	present := func(field string) bool {
		_, ok := BodyField(c, field)
		return ok
	}

	if present("type") {
		if in.Type, err = validate.OneOf(FieldOrNil(c, "type"), "type", itemTypes, validate.OneOfOpts{Label: "信息类型"}); err != nil {
			return in, err
		}
	}
	if present("title") {
		if in.Title, err = validate.Str(FieldOrNil(c, "title"), "title", validate.StrOpts{Required: true, Max: 80, Label: "标题"}); err != nil {
			return in, err
		}
	}
	if present("category") {
		if in.Category, err = validate.Str(FieldOrNil(c, "category"), "category", validate.StrOpts{Max: 20, Label: "分类"}); err != nil {
			return in, err
		}
		if in.Category == "" {
			in.Category = "其他"
		}
	}
	if present("description") {
		if in.Description, err = validate.Str(FieldOrNil(c, "description"), "description", validate.StrOpts{Max: 2000, Label: "详细描述"}); err != nil {
			return in, err
		}
	}
	if present("location") {
		if in.Location, err = validate.Str(FieldOrNil(c, "location"), "location", validate.StrOpts{Max: 120, Label: "地点"}); err != nil {
			return in, err
		}
	}
	if present("storagePlace") {
		if in.StoragePlace, err = validate.Str(FieldOrNil(c, "storagePlace"), "storagePlace", validate.StrOpts{Max: 120, Label: "寄放处"}); err != nil {
			return in, err
		}
	}
	if present("happenedAt") {
		if in.HappenedAt, err = validate.Str(FieldOrNil(c, "happenedAt"), "happenedAt", validate.StrOpts{Max: 40, Label: "时间"}); err != nil {
			return in, err
		}
	}
	if present("contact") {
		if in.Contact, err = validate.Str(FieldOrNil(c, "contact"), "contact", validate.StrOpts{Max: 120, Label: "联系方式"}); err != nil {
			return in, err
		}
	}
	if present("imageUrl") {
		if in.ImageURL, err = validate.ImageURL(FieldOrNil(c, "imageUrl")); err != nil {
			return in, err
		}
	}
	if present("status") {
		value, err := validate.OneOf(FieldOrNil(c, "status"), "status", itemStatuses, validate.OneOfOpts{Label: "状态"})
		if err != nil {
			return in, err
		}
		in.Status = validate.P(value)
	}

	return in, nil
}

// patchableFields PATCH 允许出现的字段名，用于拒绝空请求体。
var patchableFields = []string{"type", "title", "category", "description", "location", "storagePlace", "happenedAt", "contact", "imageUrl", "status"}

// itemsPublicList 公开列表：只展示审核通过的信息。
func itemsPublicList(c *gin.Context, deps *Deps) error {
	opts := store.ListOptions{PublicOnly: true}

	if raw := QueryOrNil(c, "type"); raw != nil {
		value, err := validate.OneOf(raw, "type", itemTypes, validate.OneOfOpts{Label: "类型"})
		if err != nil {
			return err
		}
		opts.Type = validate.P(value)
	}
	category, err := validate.Str(QueryOrNil(c, "category"), "category", validate.StrOpts{Max: 20})
	if err != nil {
		return err
	}
	if category != "" {
		opts.Category = validate.P(category)
	}
	keyword, err := validate.Str(QueryOrNil(c, "keyword"), "keyword", validate.StrOpts{Max: 60, Label: "关键字"})
	if err != nil {
		return err
	}
	if keyword != "" {
		opts.Keyword = validate.P(keyword)
	}
	if raw := QueryOrNil(c, "status"); raw != nil {
		value, err := validate.OneOf(raw, "status", itemStatuses, validate.OneOfOpts{Label: "状态"})
		if err != nil {
			return err
		}
		opts.Status = validate.P(value)
	}

	opts.Page, opts.PageSize, err = readPage(c)
	if err != nil {
		return err
	}
	opts.Sort, opts.Order, err = readSort(c)
	if err != nil {
		return err
	}

	page, err := deps.Store.ListItems(opts)
	if err != nil {
		return err
	}
	httpx.SendData(c, pageWithCategories(page), http.StatusOK)
	return nil
}

// itemsCategories 分类字典。
func itemsCategories(c *gin.Context) error {
	httpx.SendData(c, gin.H{"categories": config.Categories}, http.StatusOK)
	return nil
}

// itemsMine 我的发布：包含待审核与被驳回的信息。
func itemsMine(c *gin.Context, deps *Deps) error {
	user := CurrentUser(c)
	opts := store.ListOptions{UserID: validate.P(user.ID)}

	if raw := QueryOrNil(c, "auditStatus"); raw != nil {
		value, err := validate.OneOf(raw, "auditStatus", auditStatuses, validate.OneOfOpts{Label: "审核状态"})
		if err != nil {
			return err
		}
		opts.AuditStatus = validate.P(value)
	}
	if raw := QueryOrNil(c, "status"); raw != nil {
		value, err := validate.OneOf(raw, "status", itemStatuses, validate.OneOfOpts{Label: "状态"})
		if err != nil {
			return err
		}
		opts.Status = validate.P(value)
	}

	page, pageSize, err := readPage(c)
	if err != nil {
		return err
	}
	opts.Page, opts.PageSize = page, pageSize

	if opts.Sort, opts.Order, err = readSort(c); err != nil {
		return err
	}

	result, err := deps.Store.ListItems(opts)
	if err != nil {
		return err
	}
	httpx.SendData(c, pageWithCategories(result), http.StatusOK)
	return nil
}

// itemsCreate 发布信息，默认进入待审核状态。
func itemsCreate(c *gin.Context, deps *Deps) error {
	in, err := readItemInput(c)
	if err != nil {
		return err
	}
	user := CurrentUser(c)
	if in.Contact == "" {
		in.Contact = user.Contact
	}

	item, err := deps.Store.CreateItem(user.ID, in)
	if err != nil {
		return err
	}
	httpx.SendData(c, gin.H{"item": store.ShapeItem(item)}, http.StatusCreated)
	return nil
}

// itemsDetail 详情：未通过审核的信息仅本人和管理员可见。
func itemsDetail(c *gin.Context, deps *Deps) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}

	row, err := deps.Store.FindItemByID(id)
	if err != nil {
		return err
	}
	if row == nil {
		return httpx.NotFound("该信息不存在或已被删除")
	}

	user := CurrentUser(c)
	isOwner := user != nil && user.ID == row.UserID
	isAdmin := user != nil && user.Role == "admin"

	if row.AuditStatus != "approved" && !isOwner && !isAdmin {
		return httpx.NotFound("该信息不存在或尚未通过审核")
	}

	httpx.SendData(c, gin.H{"item": models.ItemDetail{Item: *store.ShapeItem(row), CanEdit: isOwner || isAdmin}}, http.StatusOK)
	return nil
}

// itemsUpdate 整体替换（PUT）：请求体缺省的字段会被清空。
// 普通用户修改后需要重新审核，管理员修改不受影响。
func itemsUpdate(c *gin.Context, deps *Deps) error {
	row := ContextItem(c)

	in, err := readItemInput(c)
	if err != nil {
		return err
	}
	if raw, ok := BodyField(c, "status"); ok {
		value, err := validate.OneOf(raw, "status", itemStatuses, validate.OneOfOpts{Label: "状态"})
		if err != nil {
			return err
		}
		in.Status = validate.P(value)
	}

	item, err := deps.Store.UpdateItem(row.ID, CurrentUser(c), in)
	if err != nil {
		return err
	}
	httpx.SendData(c, gin.H{"item": store.ShapeItem(item)}, http.StatusOK)
	return nil
}

// itemsPatch 局部更新（PATCH）：只改请求体里出现的字段，其余保持原值。
func itemsPatch(c *gin.Context, deps *Deps) error {
	row := ContextItem(c)

	if !hasAnyField(c, patchableFields) {
		return httpx.BadRequest("没有需要修改的字段")
	}

	in, err := readItemPatchInput(c, row)
	if err != nil {
		return err
	}

	item, err := deps.Store.UpdateItem(row.ID, CurrentUser(c), in)
	if err != nil {
		return err
	}
	httpx.SendData(c, gin.H{"item": store.ShapeItem(item)}, http.StatusOK)
	return nil
}

// itemsSetStatus 标记为已解决 / 重新开放。
func itemsSetStatus(c *gin.Context, deps *Deps) error {
	status, err := validate.OneOf(FieldOrNil(c, "status"), "status", itemStatuses, validate.OneOfOpts{Label: "状态"})
	if err != nil {
		return err
	}

	item, err := deps.Store.SetItemStatus(ContextItem(c).ID, CurrentUser(c), status)
	if err != nil {
		return err
	}
	httpx.SendData(c, gin.H{"item": store.ShapeItem(item)}, http.StatusOK)
	return nil
}

// itemsDelete 删除信息（本人或管理员）。
func itemsDelete(c *gin.Context, deps *Deps) error {
	if err := deps.Store.RemoveItem(ContextItem(c).ID, CurrentUser(c)); err != nil {
		return err
	}
	httpx.SendData(c, gin.H{"ok": true}, http.StatusOK)
	return nil
}

// hasAnyField 判断请求体里是否至少出现了其中一个字段。
func hasAnyField(c *gin.Context, fields []string) bool {
	for _, field := range fields {
		if _, ok := BodyField(c, field); ok {
			return true
		}
	}
	return false
}

// pageWithCategories 前台列表会额外带上分类字典。
func pageWithCategories(page *models.Page) gin.H {
	return gin.H{
		"items":      page.Items,
		"page":       page.Page,
		"pageSize":   page.PageSize,
		"total":      page.Total,
		"totalPages": page.TotalPages,
		"categories": config.Categories,
	}
}

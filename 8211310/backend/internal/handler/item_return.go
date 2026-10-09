package handler

import (
	"github.com/gin-gonic/gin"

	"lostfound/internal/apperr"
	"lostfound/internal/service"
)

// ItemReturn 对应 §4 的 #23–#29：归还确认的提交、查看、三个决定和两个列表。
//
// 这一层没有任何一条业务规则。「谁能确认」「拒绝要不要写理由」「拒绝之后帖子
// 还是不是 open」全在 service —— 前两个是因为它们要能被不起 HTTP 的单测覆盖
// （§10 第①层），第三个是因为它是这个系统最容易被人「顺手改对」的规则：
// 放在 handler 里的话，一次重构就能把 reject 变成关帖，而测试从 HTTP 层看不出来。
//
// 单开一个 handler 而不是塞进 Item，理由和 Contact / Match 一样：依赖的是
// *service.ItemReturn。塞进去就得让 Item 那个 struct 多挂一个服务，
// 而「谁负责推进归还状态机」这件事会从此模糊。
type ItemReturn struct {
	Svc *service.ItemReturn
}

// ---------- 请求体 ----------

// submitReturnReq 是 #23 的请求体，两个字段都必填（必填这件事在 service 里判）。
type submitReturnReq struct {
	Message        string `json:"message"`
	ProofImagePath string `json:"proof_image_path"`
}

// confirmReturnReq 是 #25 的请求体：{owner_note?}，可选。
type confirmReturnReq struct {
	OwnerNote string `json:"owner_note"`
}

// rejectReturnReq 是 #26 的请求体：{owner_note}，必填（那条 VALIDATION 在 service）。
type rejectReturnReq struct {
	OwnerNote string `json:"owner_note"`
}

// ---------- #23 提交归还确认 ----------

// Submit 处理 #23 POST /api/items/:id/returns（JWT，对所有登录用户开放）。
//
// :id 是**帖子** id 而不是归还确认 id —— 提交那一刻记录还不存在，没有 id 可用。
// 这也是这一组里唯一一个以帖子为路径的端点，下面六个全部以归还确认为路径。
func (h ItemReturn) Submit(c *gin.Context) {
	itemID, err := pathID(c, "id", "帖子")
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	var req submitReturnReq
	if err := bindJSON(c, &req); err != nil {
		apperr.Respond(c, err)
		return
	}

	res, err := h.Svc.Submit(c.Request.Context(), apperr.User(c), itemID, service.SubmitReturnInput{
		Message:        req.Message,
		ProofImagePath: req.ProofImagePath,
	})
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, res)
}

// ---------- #24 详情 ----------

// Detail 处理 #24 GET /api/returns/:id（JWT，提交人 / 发帖人 / Admin）。
//
// 鉴权在 service：这里连「当前用户是不是 admin」都不问，
// 因为 handler 一旦开始按 role 分支，就迟早会出现「admin 在 handler 里能看到、
// 在 service 里改不了」这种半途而废的权限模型（计划 §4 对 #15 明确禁止）。
func (h ItemReturn) Detail(c *gin.Context) {
	id, err := pathID(c, "id", "归还确认")
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

// ---------- #25 / #26 发帖人的两个决定 ----------

// Confirm 处理 #25 POST /api/returns/:id/confirm（**仅发帖人**，Admin 也 FORBIDDEN）。
func (h ItemReturn) Confirm(c *gin.Context) {
	id, err := pathID(c, "id", "归还确认")
	if err != nil {
		apperr.Respond(c, err)
		return
	}

	// ⚠ 这里是 bindJSONOptional 在 DELETE 之外的唯一一处用法，理由是计划把
	// owner_note 定成**可选**：拾主可以什么都不写就点确认。
	// 用它不会踩 bindJSON 那条「忘带 body 被当成零值」的坑，因为这里的零值
	// （空备注）本来就是合法输入之一；改成 bindJSON 的话，一个合法的
	// `curl -X POST` 空 body 请求会被判成 VALIDATION，那才是真正的错位。
	// #26 reject 用的就是 bindJSON —— 那里空备注必须被拒。
	var req confirmReturnReq
	if err := bindJSONOptional(c, &req); err != nil {
		apperr.Respond(c, err)
		return
	}

	res, err := h.Svc.Confirm(c.Request.Context(), apperr.User(c), id, req.OwnerNote)
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, res)
}

// Reject 处理 #26 POST /api/returns/:id/reject（**仅发帖人**）。
func (h ItemReturn) Reject(c *gin.Context) {
	id, err := pathID(c, "id", "归还确认")
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	var req rejectReturnReq
	if err := bindJSON(c, &req); err != nil {
		apperr.Respond(c, err)
		return
	}

	res, err := h.Svc.Reject(c.Request.Context(), apperr.User(c), id, req.OwnerNote)
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, res)
}

// ---------- #27 提交人撤销 ----------

// Cancel 处理 #27 POST /api/returns/:id/cancel（JWT，仅提交人）。
//
// **没有请求体**，和 #21 解锁同一条理由：撤销要的全部信息都在
// 「你是谁」（JWT）和「哪条确认」（路径）里，客户端没有任何东西需要声明。
// 这里刻意也不 bindJSONOptional：读了又扔掉的一个字段是将来「顺手加个理由」
// 的入口，而计划 §4 第 27 行那一列写的是「—」。
func (h ItemReturn) Cancel(c *gin.Context) {
	id, err := pathID(c, "id", "归还确认")
	if err != nil {
		apperr.Respond(c, err)
		return
	}

	res, err := h.Svc.Cancel(c.Request.Context(), apperr.User(c), id)
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, res)
}

// ---------- #28 / #29 两个列表 ----------

// ListSubmitted 处理 #28 GET /api/my/returns/submitted（JWT）。
func (h ItemReturn) ListSubmitted(c *gin.Context) {
	page, err := h.Svc.ListSubmitted(c.Request.Context(), apperr.User(c), returnListQueryFrom(c))
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, page)
}

// ListReceived 处理 #29 GET /api/my/returns/received（JWT）。
func (h ItemReturn) ListReceived(c *gin.Context) {
	page, err := h.Svc.ListReceived(c.Request.Context(), apperr.User(c), returnListQueryFrom(c))
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, page)
}

// returnListQueryFrom 把查询参数原样收进来，一个都不解析（同 listQueryFrom 那条理由：
// 只有 service 能给出带字段名的中文 VALIDATION）。
//
// ⚠ 这里**不读** user_id / item_id：「谁的列表」这件事只能来自 JWT。
// 两个列表的唯一区别是提交人还是帖主，而那由路由末段（submitted / received）决定。
func returnListQueryFrom(c *gin.Context) service.ReturnListQuery {
	return service.ReturnListQuery{
		Status:   c.Query("status"),
		Page:     c.Query("page"),
		PageSize: c.Query("page_size"),
	}
}

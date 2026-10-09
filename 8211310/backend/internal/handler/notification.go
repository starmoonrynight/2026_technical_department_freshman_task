package handler

import (
	"github.com/gin-gonic/gin"

	"lostfound/internal/apperr"
	"lostfound/internal/service"
)

// Notification 对应 §4 的 #30–#32：收件箱、未读数、标记已读。
//
// 三条都只用 apperr.UserID(c) —— 收件箱是「谁的」这件事永远不来自请求参数，
// 和 #19「我的发布」是同一条纪律。少这一句约束，就会出现
// 「/api/my/notifications?user_id=7」这种看起来能用、实际上是全站最大的越权面。
type Notification struct {
	Svc *service.Notification
}

// markReadReq 是 #32 的请求体。
//
// 两个字段都是指针，为的是把「没带」和「带了但是 false / 空数组」分开 ——
// 那两种情况的正确响应完全不同（前者是「你没说要标什么」的 VALIDATION，
// 后者是「all 只能是 true」或「ids 不能为空」）。
// encoding/json 的行为正好对得上：字段缺失或 null → 保持 nil。
type markReadReq struct {
	IDs *[]int64 `json:"ids"`
	All *bool    `json:"all"`
}

func (r markReadReq) input() service.MarkReadInput {
	return service.MarkReadInput{IDs: r.IDs, All: r.All}
}

// List 处理 #30 GET /api/my/notifications（JWT）。
//
// 查询参数原样传给 service，一个都不在这里解析 —— 和 listQueryFrom 同一条理由：
// 只有 service 能给出带字段名的中文 VALIDATION。
func (h Notification) List(c *gin.Context) {
	page, err := h.Svc.ListMine(c.Request.Context(), apperr.UserID(c), service.InboxQuery{
		IsRead:   c.Query("is_read"),
		Page:     c.Query("page"),
		PageSize: c.Query("page_size"),
	})
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, page)
}

// UnreadCount 处理 #31 GET /api/my/notifications/unread-count（JWT）。
func (h Notification) UnreadCount(c *gin.Context) {
	res, err := h.Svc.UnreadCount(c.Request.Context(), apperr.UserID(c))
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, res)
}

// MarkRead 处理 #32 PUT /api/my/notifications/read（JWT）。
//
// 计划把「标记已读」做成一个带 body 的 PUT 而不是 POST /:id/read（§16.1 那条决定）：
// 少一条路由，也少一个 Gin 的静态段/参数段冲突点。
func (h Notification) MarkRead(c *gin.Context) {
	var req markReadReq
	if err := bindJSON(c, &req); err != nil {
		apperr.Respond(c, err)
		return
	}

	res, err := h.Svc.MarkRead(c.Request.Context(), apperr.UserID(c), req.input())
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, res)
}

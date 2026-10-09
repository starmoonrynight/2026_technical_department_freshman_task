package handler

import (
	"github.com/gin-gonic/gin"

	"lostfound/internal/apperr"
	"lostfound/internal/service"
)

// Contact 对应 §4 的 #21（解锁联系方式）和 #22（解锁名单）。
//
// #21 是这个系统里唯一一个「把别人的隐私交出去」的端点，所以它的鉴权、
// 类型判断、状态判断全都住在 service（那三层能被不起 HTTP 的单测覆盖），
// 这一层只翻译：路径 id → 整数、查询参数 → 分页、结果 → 信封。
//
// 和 Match 一样单开一个 handler 而不是塞进 Item：#21/#22 的依赖是 *service.Contact，
// 塞进 Item 就得让那个 struct 多挂一个服务，而「谁负责解锁」这件事会开始模糊。
type Contact struct {
	Svc *service.Contact
}

// Unlock 处理 #21 POST /api/items/:id/unlock-contact（JWT）。
//
// **没有请求体**（计划 §4 第 21 行的请求体那一列就是「—」）：
// 解锁要的全部信息都在「你是谁」（JWT）和「哪条帖子」（路径）里，
// 没有任何东西需要客户端声明。让客户端发一个 `{}` 只是多一次可以填错的机会。
//
// 也正因为没有请求体，这个端点天然没有「参数被篡改」的面：
// 客户端不能说「我要以 user_id=7 的身份解锁」—— user_id 只能来自 token。
func (h Contact) Unlock(c *gin.Context) {
	id, err := pathID(c, "id", "帖子")
	if err != nil {
		apperr.Respond(c, err)
		return
	}

	res, err := h.Svc.Unlock(c.Request.Context(), apperr.User(c), id)
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, res)
}

// Views 处理 #22 GET /api/items/:id/contact-views（JWT，发帖人或 Admin）。
func (h Contact) Views(c *gin.Context) {
	id, err := pathID(c, "id", "帖子")
	if err != nil {
		apperr.Respond(c, err)
		return
	}

	page, err := h.Svc.Views(c.Request.Context(), apperr.User(c), id, service.PageQuery{
		Page:     c.Query("page"),
		PageSize: c.Query("page_size"),
	})
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, page)
}

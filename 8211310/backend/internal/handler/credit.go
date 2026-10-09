package handler

import (
	"github.com/gin-gonic/gin"

	"lostfound/internal/apperr"
	"lostfound/internal/service"
)

// Credit 对应 §4 的 #33：我自己的积分流水。
//
// 一个端点、一个方法、零条业务规则（除了分页参数的解析，而那在 service）。
// 单独开一个文件而不是挂到 Notification 上，是因为它的 data 形状里那个
// credit_score 是 users 表的列，不是 notifications 的 —— 两个资源混在一个
// handler 里，将来给积分加第二个来源时就得先拆文件。
type Credit struct {
	Svc *service.Credit
}

// MyLogs 处理 #33 GET /api/my/credit-logs（JWT）。
//
// 没有 :id、没有 ?user_id=：这张表只能读自己的（service/credit.go 顶部写了为什么）。
func (h Credit) MyLogs(c *gin.Context) {
	page, err := h.Svc.MyLogs(c.Request.Context(), apperr.User(c), service.PageQuery{
		Page:     c.Query("page"),
		PageSize: c.Query("page_size"),
	})
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, page)
}

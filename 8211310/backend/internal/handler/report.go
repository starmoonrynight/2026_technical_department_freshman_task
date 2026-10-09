package handler

import (
	"github.com/gin-gonic/gin"

	"lostfound/internal/apperr"
	"lostfound/internal/service"
)

// Report 对应 §4 的 #41：用户举报一条帖子。
//
// 这一层只有一个方法，而且它不调用任何「处置」类服务 —— 这是刻意的形状。
// 用户能做的到「提交」为止（service.Report 的依赖里连 UPDATE 都没有，见 §15 那三行禁令），
// 处置住在 M6 的 admin 端。如果哪天有人在这里加一个 `Resolve`，
// 那就是把「举报 ≠ 仲裁」这条契约从 HTTP 层拆了。
type Report struct {
	Svc *service.Report
}

// createReportReq 是 #41 的请求体。detail 是选填的，缺失时按空串处理。
type createReportReq struct {
	ReasonCode string `json:"reason_code"`
	Detail     string `json:"detail"`
}

// Create 处理 #41 POST /api/items/:id/report（JWT）。
func (h Report) Create(c *gin.Context) {
	id, err := pathID(c, "id", "帖子")
	if err != nil {
		apperr.Respond(c, err)
		return
	}

	var req createReportReq
	if err := bindJSON(c, &req); err != nil {
		apperr.Respond(c, err)
		return
	}

	res, err := h.Svc.Create(c.Request.Context(), apperr.User(c), id, req.ReasonCode, req.Detail)
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, res)
}

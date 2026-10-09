package handler

import (
	"github.com/gin-gonic/gin"

	"lostfound/internal/apperr"
	"lostfound/internal/service"
)

// Match 只对应 §4 的 #20。
//
// 单独一个 struct 而不是挂在 Item 上：#20 的业务对象是 service.Match，
// 它的依赖（候选 SQL + 台账）和 service.Item（帖子增删改）是两套。
// handler 层的形状跟着 service 走，「这个端点归谁管」在文件名上就看得出来。
type Match struct {
	Svc *service.Match
}

// Matches 处理 #20 GET /api/items/:id/matches（JWT，本人或 Admin）。
//
// 三个查询参数原样收成字符串，一个都不在这里解析（同 #14 的 listQueryFrom）：
// top=abc 这种输入要变成带字段名的中文 VALIDATION，而不是 strconv 的英文报错，
// 而能给出「top 必须是 1–50 的整数」这句话的地方是 service。
func (h Match) Matches(c *gin.Context) {
	id, err := pathID(c, "id", "帖子")
	if err != nil {
		apperr.Respond(c, err)
		return
	}

	res, err := h.Svc.List(c.Request.Context(), apperr.User(c), id, service.MatchesQuery{
		Top:      c.Query("top"),
		MinScore: c.Query("min_score"),
		Tier:     c.Query("tier"),
	})
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, res)
}

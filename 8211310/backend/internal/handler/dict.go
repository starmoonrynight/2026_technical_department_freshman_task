package handler

import (
	"github.com/gin-gonic/gin"

	"lostfound/internal/apperr"
	"lostfound/internal/service"
)

// Dict 对应 §4 的 #7、#8 —— 两个公开只读接口。
type Dict struct {
	Svc *service.Dict
}

// Categories 处理 #7 GET /api/categories（公开）。
// data 是两级树：[{id, name, level, sort_order, children:[...]}]。
func (h Dict) Categories(c *gin.Context) {
	tree, err := h.Svc.CategoryTree(c.Request.Context())
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, tree)
}

// Locations 处理 #8 GET /api/locations（公开）。
// data 是三级树，比分类多一个 is_freeform（标记「其他」这种要用户自己输入文本的节点）。
func (h Dict) Locations(c *gin.Context) {
	tree, err := h.Svc.LocationTree(c.Request.Context())
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, tree)
}

package web

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"lostfound/internal/httpx"
	"lostfound/internal/store"
	"lostfound/internal/validate"
)

func registerAdminRoutes(g *gin.RouterGroup, deps *Deps) {
	g.Use(RequireAdmin())

	g.GET("/stats", handle(func(c *gin.Context) error { return adminStats(c, deps) }))
	g.GET("/items", handle(func(c *gin.Context) error { return adminItems(c, deps) }))
	g.POST("/items/:id/audit", handle(func(c *gin.Context) error { return adminAudit(c, deps) }))
	g.DELETE("/items/:id", handle(func(c *gin.Context) error { return adminDeleteItem(c, deps) }))
	g.GET("/users", handle(func(c *gin.Context) error { return adminUsers(c, deps) }))
	g.POST("/users/:id/status", handle(func(c *gin.Context) error { return adminUserStatus(c, deps) }))
	g.POST("/users/:id/role", handle(func(c *gin.Context) error { return adminUserRole(c, deps) }))
}

// adminStats 数据概览。
func adminStats(c *gin.Context, deps *Deps) error {
	stats, err := deps.Store.ItemStats()
	if err != nil {
		return err
	}
	users, err := deps.Store.ListUsers()
	if err != nil {
		return err
	}

	httpx.SendData(c, gin.H{"stats": stats, "userCount": len(users)}, http.StatusOK)
	return nil
}

// adminItems 全部信息（可按审核状态筛选）。
func adminItems(c *gin.Context, deps *Deps) error {
	opts := store.ListOptions{}

	if raw := QueryOrNil(c, "auditStatus"); raw != nil {
		value, err := validate.OneOf(raw, "auditStatus", auditStatuses, validate.OneOfOpts{Label: "审核状态"})
		if err != nil {
			return err
		}
		opts.AuditStatus = validate.P(value)
	}
	if raw := QueryOrNil(c, "type"); raw != nil {
		value, err := validate.OneOf(raw, "type", itemTypes, validate.OneOfOpts{Label: "类型"})
		if err != nil {
			return err
		}
		opts.Type = validate.P(value)
	}
	if raw := QueryOrNil(c, "status"); raw != nil {
		value, err := validate.OneOf(raw, "status", itemStatuses, validate.OneOfOpts{Label: "状态"})
		if err != nil {
			return err
		}
		opts.Status = validate.P(value)
	}
	keyword, err := validate.Str(QueryOrNil(c, "keyword"), "keyword", validate.StrOpts{Max: 60, Label: "关键字"})
	if err != nil {
		return err
	}
	if keyword != "" {
		opts.Keyword = validate.P(keyword)
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
	httpx.SendData(c, page, http.StatusOK)
	return nil
}

// adminAudit 审核：action = approve | reject。
func adminAudit(c *gin.Context, deps *Deps) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	action, err := validate.OneOf(FieldOrNil(c, "action"), "action", []string{"approve", "reject"}, validate.OneOfOpts{Label: "审核动作"})
	if err != nil {
		return err
	}
	remark, err := validate.Str(FieldOrNil(c, "remark"), "remark", validate.StrOpts{Max: 200, Label: "审核备注"})
	if err != nil {
		return err
	}
	if action == "reject" && remark == "" {
		return httpx.BadRequest("驳回时必须填写原因")
	}

	item, err := deps.Store.AuditItem(id, action, remark)
	if err != nil {
		return err
	}
	httpx.SendData(c, gin.H{"item": store.ShapeItem(item)}, http.StatusOK)
	return nil
}

// adminDeleteItem 管理员删除任意信息。
func adminDeleteItem(c *gin.Context, deps *Deps) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	if err := deps.Store.RemoveItem(id, CurrentUser(c)); err != nil {
		return err
	}
	httpx.SendData(c, gin.H{"ok": true}, http.StatusOK)
	return nil
}

// adminUsers 用户列表。
func adminUsers(c *gin.Context, deps *Deps) error {
	users, err := deps.Store.ListUsers()
	if err != nil {
		return err
	}
	httpx.SendData(c, gin.H{"users": users}, http.StatusOK)
	return nil
}

// adminUserStatus 启用 / 停用用户；停用时踢掉其全部会话。
func adminUserStatus(c *gin.Context, deps *Deps) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	status, err := validate.OneOf(FieldOrNil(c, "status"), "status", userStatuses, validate.OneOfOpts{Label: "账号状态"})
	if err != nil {
		return err
	}

	user, err := deps.Store.UpdateUserStatus(id, status)
	if err != nil {
		return err
	}
	if user == nil {
		return httpx.NotFound("用户不存在")
	}
	if status == "disabled" {
		if err := deps.Store.DestroyUserSessions(id); err != nil {
			return err
		}
	}

	httpx.SendData(c, gin.H{"user": store.ToPublic(user)}, http.StatusOK)
	return nil
}

// adminUserRole 调整用户角色。
func adminUserRole(c *gin.Context, deps *Deps) error {
	id, err := idParam(c)
	if err != nil {
		return err
	}
	role, err := validate.OneOf(FieldOrNil(c, "role"), "role", userRoles, validate.OneOfOpts{Label: "角色"})
	if err != nil {
		return err
	}

	target, err := deps.Store.FindUserByID(id)
	if err != nil {
		return err
	}
	if target == nil {
		return httpx.NotFound("用户不存在")
	}

	user, err := deps.Store.UpdateUserRole(id, role)
	if err != nil {
		return err
	}
	httpx.SendData(c, gin.H{"user": store.ToPublic(user)}, http.StatusOK)
	return nil
}

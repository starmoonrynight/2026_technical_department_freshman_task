package web

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"lostfound/internal/config"
	"lostfound/internal/httpx"
	"lostfound/internal/store"
)

const (
	userContextKey    = "laf.user"
	sessionContextKey = "laf.sessionId"
)

type authMiddleware struct {
	deps *Deps
}

// AttachUser 解析 Cookie 中的会话，把当前用户挂到上下文（未登录时为空）。
func (m *authMiddleware) AttachUser() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, err := c.Cookie(config.SessionCookie)
		if err != nil || raw == "" {
			c.Next()
			return
		}

		session, err := m.deps.Store.GetSession(raw)
		if err != nil || session == nil {
			c.Next()
			return
		}

		user, err := m.deps.Store.FindUserByID(session.UserID)
		if err != nil {
			c.Next()
			return
		}
		if user == nil || user.Status != "active" {
			_ = m.deps.Store.DestroySession(session.ID)
			c.Next()
			return
		}

		c.Set(userContextKey, user)
		c.Set(sessionContextKey, session.ID)
		c.Next()
	}
}

// CurrentUser 取出当前登录用户，未登录返回 nil。
func CurrentUser(c *gin.Context) *store.UserRow {
	if value, ok := c.Get(userContextKey); ok {
		if user, ok := value.(*store.UserRow); ok {
			return user
		}
	}
	return nil
}

// CurrentSessionID 取出当前会话 ID。
func CurrentSessionID(c *gin.Context) string {
	if value, ok := c.Get(sessionContextKey); ok {
		if id, ok := value.(string); ok {
			return id
		}
	}
	return ""
}

// RequireAuth 鉴权中间件：要求请求携带有效会话 Cookie。
//
// 会话的解析在 AttachUser 里完成（请求级全局中间件），这里只做「是否登录」的判断。
// 未登录统一返回 401 {"message":"请先登录"}，前端据此跳转到登录页。
//
// 用法：
//
//	guarded := router.Group("/api/xxx", web.RequireAuth())
func RequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		if CurrentUser(c) == nil {
			httpx.Abort(c, httpx.Unauthorized(""))
			return
		}
		c.Next()
	}
}

// RequireRole 要求当前用户属于指定角色之一，需配合 RequireAuth 使用。
func RequireRole(roles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		user := CurrentUser(c)
		if user == nil {
			httpx.Abort(c, httpx.Unauthorized(""))
			return
		}
		for _, role := range roles {
			if user.Role == role {
				c.Next()
				return
			}
		}
		httpx.Abort(c, httpx.Forbidden("仅管理员可访问"))
	}
}

// RequireAdmin 要求管理员角色。
func RequireAdmin() gin.HandlerFunc {
	return RequireRole("admin")
}

const itemContextKey = "laf.item"

// RequireItemOwner 是失物/招领信息的「归属校验」中间件。
//
// 它把鉴权落到实处：先按路径上的 :id 从数据库取出这条信息，再用 Session 解析出的
// 当前用户 ID 与 items.user_id 比对，只有发布者本人（或管理员）才能继续。
// 校验通过的条目会存进上下文，处理器用 ContextItem 直接取，不必再查一次库。
//
// 必须挂在 RequireAuth 之后。
//
// 失败响应：
//
//	400  id 不是合法整数
//	401  未登录（RequireAuth 已拦截，这里是兜底）
//	403  {"message":"只能修改自己发布的信息"}
//	404  {"message":"该信息不存在或已被删除"}
func RequireItemOwner(s *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		user := CurrentUser(c)
		if user == nil {
			httpx.Abort(c, httpx.Unauthorized(""))
			return
		}

		id, err := idParam(c)
		if err != nil {
			httpx.Abort(c, err)
			return
		}

		row, err := s.FindItemByID(id)
		if err != nil {
			httpx.Abort(c, httpx.New(http.StatusInternalServerError, "服务器内部错误"))
			return
		}
		if row == nil {
			httpx.Abort(c, httpx.NotFound("该信息不存在或已被删除"))
			return
		}

		// 核心的一次比对：会话里的用户 ID vs 数据库里的 items.user_id
		if user.Role != "admin" && row.UserID != user.ID {
			httpx.Abort(c, httpx.Forbidden("只能修改自己发布的信息"))
			return
		}

		c.Set(itemContextKey, row)
		c.Next()
	}
}

// ContextItem 取出 RequireItemOwner 查好的条目，未经过该中间件时返回 nil。
func ContextItem(c *gin.Context) *store.ItemRow {
	if value, ok := c.Get(itemContextKey); ok {
		if row, ok := value.(*store.ItemRow); ok {
			return row
		}
	}
	return nil
}

// handle 统一把业务异常交给 httpx 输出。
func handle(fn func(*gin.Context) error) gin.HandlerFunc {
	return func(c *gin.Context) {
		if err := fn(c); err != nil {
			httpx.Abort(c, err)
		}
	}
}

// setSessionCookie 写入会话 Cookie（HttpOnly + SameSite=Lax）。
func setSessionCookie(c *gin.Context, ttlSeconds int, value string) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     config.SessionCookie,
		Value:    value,
		Path:     "/",
		MaxAge:   ttlSeconds,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// clearSessionCookie 删除会话 Cookie。
func clearSessionCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     config.SessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

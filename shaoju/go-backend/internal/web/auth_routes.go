package web

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"lostfound/internal/httpx"
	"lostfound/internal/store"
	"lostfound/internal/validate"
)

func registerAuthRoutes(g *gin.RouterGroup, deps *Deps) {
	// 公开接口
	g.POST("/register", handle(func(c *gin.Context) error { return authRegister(c, deps) }))
	g.POST("/login", handle(func(c *gin.Context) error { return authLogin(c, deps) }))
	g.POST("/logout", handle(func(c *gin.Context) error { return authLogout(c, deps) }))
	g.GET("/me", handle(func(c *gin.Context) error { return authMe(c, deps) }))

	// 以下接口由 RequireAuth 中间件保护：未登录直接 401
	guarded := g.Group("", RequireAuth())
	guarded.PUT("/profile", handle(func(c *gin.Context) error { return authUpdateProfile(c, deps) }))
	guarded.PUT("/password", handle(func(c *gin.Context) error { return authChangePassword(c, deps) }))
}

// aliasedField 按顺序取第一个「显式出现」的字段。
// 用于兼容旧前端：学号既接受 studentId 也接受 username，姓名既接受 name 也接受 nickname。
func aliasedField(c *gin.Context, keys ...string) any {
	body := Body(c)
	for _, key := range keys {
		if value, ok := body[key]; ok {
			return value
		}
	}
	return nil
}

func sessionCookieTTL(deps *Deps) int {
	return int(deps.Cfg.SessionTTL / time.Second)
}

// authRegister 注册并直接登录。
// 请求体：{ studentId, password, name, contact }（name / contact 可选）
func authRegister(c *gin.Context, deps *Deps) error {
	studentID, err := validate.StudentID(aliasedField(c, "studentId", "username"))
	if err != nil {
		return err
	}
	pass, err := validate.Password(FieldOrNil(c, "password"), "")
	if err != nil {
		return err
	}
	name, err := validate.Str(aliasedField(c, "name", "nickname"), "name", validate.StrOpts{Max: 32, Label: "姓名"})
	if err != nil {
		return err
	}
	contact, err := validate.Str(FieldOrNil(c, "contact"), "contact", validate.StrOpts{Max: 120, Label: "联系方式"})
	if err != nil {
		return err
	}

	user, err := deps.Store.Register(studentID, pass, name, contact)
	if err != nil {
		return err
	}

	session, err := deps.Store.CreateSession(user.ID, deps.Cfg.SessionTTL)
	if err != nil {
		return err
	}

	setSessionCookie(c, sessionCookieTTL(deps), session.ID)
	httpx.SendData(c, gin.H{"user": store.ToPublic(user)}, http.StatusCreated)
	return nil
}

// authLogin 登录。
// 请求体：{ studentId, password }
func authLogin(c *gin.Context, deps *Deps) error {
	studentID, err := validate.Str(aliasedField(c, "studentId", "username"), "studentId", validate.StrOpts{
		Required: true, Max: 32, Label: "学号",
	})
	if err != nil {
		return err
	}

	pass := ""
	if raw, ok := Body(c)["password"].(string); ok {
		pass = raw
	}

	user, err := deps.Store.Authenticate(studentID, pass)
	if err != nil {
		return err
	}

	session, err := deps.Store.CreateSession(user.ID, deps.Cfg.SessionTTL)
	if err != nil {
		return err
	}

	setSessionCookie(c, sessionCookieTTL(deps), session.ID)
	httpx.SendData(c, gin.H{"user": store.ToPublic(user)}, http.StatusOK)
	return nil
}

// authLogout 登出：销毁当前会话并清除 Cookie。
func authLogout(c *gin.Context, deps *Deps) error {
	if err := deps.Store.DestroySession(CurrentSessionID(c)); err != nil {
		return err
	}
	clearSessionCookie(c)
	httpx.SendData(c, gin.H{"ok": true}, http.StatusOK)
	return nil
}

// authMe 返回当前登录用户，未登录时 user 为 null。
func authMe(c *gin.Context, deps *Deps) error {
	var payload *store.UserRow
	if user := CurrentUser(c); user != nil {
		payload = user
	}
	httpx.SendData(c, gin.H{"user": store.ToPublic(payload)}, http.StatusOK)
	return nil
}

// authUpdateProfile 修改姓名与联系方式。
func authUpdateProfile(c *gin.Context, deps *Deps) error {
	contact, err := validate.Str(FieldOrNil(c, "contact"), "contact", validate.StrOpts{Max: 120, Label: "联系方式"})
	if err != nil {
		return err
	}
	name, err := validate.Name(aliasedField(c, "name", "nickname"))
	if err != nil {
		return err
	}

	user := CurrentUser(c)
	updated, err := deps.Store.UpdateProfile(user.ID, name, contact)
	if err != nil {
		return err
	}

	httpx.SendData(c, gin.H{"user": store.ToPublic(updated)}, http.StatusOK)
	return nil
}

// authChangePassword 修改口令：成功后其它设备的会话全部失效。
func authChangePassword(c *gin.Context, deps *Deps) error {
	oldPassword := ""
	if raw, ok := Body(c)["oldPassword"].(string); ok {
		oldPassword = raw
	}
	newPassword, err := validate.Password(FieldOrNil(c, "newPassword"), "新密码")
	if err != nil {
		return err
	}
	if oldPassword == newPassword {
		return httpx.BadRequest("新密码不能与原密码相同")
	}

	user := CurrentUser(c)
	if err := deps.Store.ChangePassword(user.ID, oldPassword, newPassword); err != nil {
		return err
	}
	if err := deps.Store.DestroyUserSessions(user.ID); err != nil {
		return err
	}

	session, err := deps.Store.CreateSession(user.ID, deps.Cfg.SessionTTL)
	if err != nil {
		return err
	}

	setSessionCookie(c, sessionCookieTTL(deps), session.ID)
	httpx.SendData(c, gin.H{"ok": true}, http.StatusOK)
	return nil
}

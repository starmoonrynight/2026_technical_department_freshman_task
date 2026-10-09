package handler

import (
	"github.com/gin-gonic/gin"

	"lostfound/internal/apperr"
	"lostfound/internal/service"
)

// Auth 对应 §4 的 #1–#5。
//
// 这一层刻意薄到几乎没有逻辑：绑定请求 → 调 service → 写响应。
// 判断「密码够不够强」「重名了怎么办」全在 service，因为那些规则要能被
// 不启 HTTP server 的单元测试覆盖（§10 第①层）。handler 里每多一个 if，
// 就多一段只能靠起服务才能测到的代码。
type Auth struct {
	Svc *service.Auth
}

// ---------- 请求体 ----------

// registerReq 是 #1 的请求体。
//
// 不用 gin 的 `binding:"required"` 标签：那样校验规则会分散在结构体标签和
// service 两处，而且 gin 的默认报错是英文的、字段名是 Go 的驼峰名，
// 直接透给用户没法看。统一交给 service 用中文 + json 字段名报。
type registerReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Nickname string `json:"nickname"`
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// updateMeReq 是 #4 的请求体。三个字段全是指针，理由见 service.UpdateProfileInput：
// 要区分「请求里没这个字段」（nil，保持原值）和「明确传了空串」（清空）。
// encoding/json 的行为正好对得上：字段缺失或值为 null → nil，值为 "" → 指向空串的指针。
type updateMeReq struct {
	Nickname *string `json:"nickname"`
	Phone    *string `json:"phone"`
	Email    *string `json:"email"`
}

type changePasswordReq struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// ---------- 响应体 ----------

// registerResp 是 #1 的 data：{id, username, nickname, role}，**四个字段**。
//
// 刻意不复用 model.UserView（那是 #3 的 11 个字段）：计划 §4 第 1 行就写了这四个，
// 注册时信用分、创建时间这些对前端没有 immediate 用处，而少返回一点就少一点
// 「前端开始依赖某个字段、将来想删删不掉」的可能。
// 也刻意**不返回 token** —— 计划把注册和登录分成两个端点，注册完要再登一次。
type registerResp struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Nickname string `json:"nickname"`
	Role     string `json:"role"`
}

// ---------- 端点 ----------

// Register 处理 #1 POST /api/auth/register（公开）。
func (h Auth) Register(c *gin.Context) {
	var req registerReq
	if err := bindJSON(c, &req); err != nil {
		apperr.Respond(c, err)
		return
	}

	u, err := h.Svc.Register(c.Request.Context(), service.RegisterInput{
		Username: req.Username,
		Password: req.Password,
		Nickname: req.Nickname,
	})
	if err != nil {
		apperr.Respond(c, err)
		return
	}

	v := u.View()
	apperr.OK(c, registerResp{ID: v.ID, Username: v.Username, Nickname: v.Nickname, Role: v.Role})
}

// Login 处理 #2 POST /api/auth/login（公开）。
func (h Auth) Login(c *gin.Context) {
	var req loginReq
	if err := bindJSON(c, &req); err != nil {
		apperr.Respond(c, err)
		return
	}

	res, err := h.Svc.Login(c.Request.Context(), req.Username, req.Password)
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, res)
}

// Me 处理 #3 GET /api/auth/me（JWT）。
func (h Auth) Me(c *gin.Context) {
	v, err := h.Svc.Me(c.Request.Context(), apperr.UserID(c))
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, v)
}

// UpdateMe 处理 #4 PUT /api/auth/me（JWT），响应形状同 #3。
func (h Auth) UpdateMe(c *gin.Context) {
	var req updateMeReq
	if err := bindJSON(c, &req); err != nil {
		apperr.Respond(c, err)
		return
	}

	v, err := h.Svc.UpdateProfile(c.Request.Context(), apperr.UserID(c), service.UpdateProfileInput{
		Nickname: req.Nickname,
		Phone:    req.Phone,
		Email:    req.Email,
	})
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, v)
}

// ChangePassword 处理 #5 POST /api/auth/change-password（JWT，仅本地用户）。
// data 是 null —— 这个动作没有需要回传的结果，成功本身就是全部信息。
func (h Auth) ChangePassword(c *gin.Context) {
	var req changePasswordReq
	if err := bindJSON(c, &req); err != nil {
		apperr.Respond(c, err)
		return
	}

	if err := h.Svc.ChangePassword(c.Request.Context(), apperr.UserID(c), service.ChangePasswordInput{
		OldPassword: req.OldPassword,
		NewPassword: req.NewPassword,
	}); err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, nil)
}

package apperr

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"lostfound/internal/model"
)

// 本文件是 §8 响应信封的唯一出口，也是 §9「可 debug 四件套」里 RequestID 的落地点。
//
// 为什么 gin.Context 的几个 key 和它们的读写函数住在这个包里，而不是 middleware 包：
// middleware 需要 import apperr 来写错误响应，如果 key 定义在 middleware，
// apperr 反过来读 request_id 就会形成循环导入。放在依赖链更底层的 apperr，
// 依赖方向永远是 middleware → apperr，单向。

// gin.Context 里用到的 key。用 string 而不是自定义类型，是因为 gin 的 c.Set/c.Get 本来就是 string 键。
const (
	CtxRequestID = "apperr_request_id"
	CtxUserID    = "apperr_user_id"
	CtxUser      = "apperr_user"
	CtxLogger    = "apperr_logger"
	CtxError     = "apperr_error"
)

// Envelope 是所有 JSON 响应的统一信封（§8）。
// 成功和失败共用这一个形状 —— 前端只需要一个 axios 拦截器就能统一解包。
type Envelope struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Data      any    `json:"data"`
	RequestID string `json:"request_id"`
}

// OK 写一个成功响应。本系统所有成功都是 HTTP 200，不用 201/204 ——
// 状态信息在 code 字段里，多一套 HTTP 语义只会让前端多一套分支。
func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Envelope{
		Code:      CodeOK,
		Message:   "ok",
		Data:      data,
		RequestID: RequestID(c),
	})
}

// Fail 写一个业务错误响应。
// 有字段级细节时放进 data.errors，否则 data 为 null。
func Fail(c *gin.Context, e *Error) {
	var data any
	if len(e.Fields) > 0 {
		data = gin.H{"errors": e.Fields}
	}
	c.JSON(e.HTTPStatus, Envelope{
		Code:      e.Code,
		Message:   e.Message,
		Data:      data,
		RequestID: RequestID(c),
	})

	// 把原始错误交给访问日志中间件，由它统一打一条带 request_id 的日志。
	// 这里不自己打，是为了避免「一个请求两条日志」—— 报 bug 时按 request_id 捞链路会捞到重复行。
	if e.Err != nil {
		c.Set(CtxError, e.Err)
	}
}

// Respond 是 handler 层唯一该调用的错误出口。
//
// 规则：
//   - err 是（或包装了）*apperr.Error → 按它的 code / http status 写
//   - 其它任何错误 → INTERNAL + 500，真实原因只进日志，不进响应体
//
// 第二条是安全边界：pgx 的报错里可能带 SQL 片段、连接串、表结构。
// 泄漏给前端等于给攻击者画地图。响应里只有 request_id，让用户报 bug 时带上它，
// 我们再拿这个 id 去日志里捞完整链路。
func Respond(c *gin.Context, err error) {
	if err == nil {
		OK(c, nil)
		return
	}
	var ae *Error
	if errors.As(err, &ae) {
		Fail(c, ae)
		return
	}
	Fail(c, Wrap(err, CodeInternal))
}

// Cause 返回访问日志该记录的原始错误，没有则返回 nil。
func Cause(c *gin.Context) error {
	if v, ok := c.Get(CtxError); ok {
		if err, ok := v.(error); ok {
			return err
		}
	}
	return nil
}

// NoRoute 处理「路径不存在」。gin 默认返回一个纯文本 404，
// 前端解信封时会炸，所以换成统一形状。
func NoRoute(c *gin.Context) {
	Fail(c, NewMsg(CodeNotFound, "接口不存在："+c.Request.Method+" "+c.Request.URL.Path))
}

// NoMethod 处理「路径存在但方法不对」，对应 §8 的 METHOD_NOT_ALLOWED(405)。
// 需要在 router 里 engine.HandleMethodNotAllowed = true 才会走到这里。
func NoMethod(c *gin.Context) {
	Fail(c, NewMsg(CodeMethodNotAllowed, c.Request.Method+" 不被这个接口支持"))
}

// ---------- gin.Context 读写 ----------

// SetRequestID 由 RequestID 中间件调用。
func SetRequestID(c *gin.Context, id string) { c.Set(CtxRequestID, id) }

// RequestID 取出当前请求的 request_id。没设置过时返回空串而不是 panic ——
// 单元测试里直接调 handler 不挂中间件是常态，那种情况下信封里 request_id 为空是可接受的。
func RequestID(c *gin.Context) string {
	if v, ok := c.Get(CtxRequestID); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// SetUserID 由 JWT 中间件在验明身份后调用（M1）。
func SetUserID(c *gin.Context, userID int64) { c.Set(CtxUserID, userID) }

// UserID 返回当前登录用户 id；未登录返回 0。
// 约定 0 为「无用户」，因为 users.id 是 BIGSERIAL，从 1 开始，永远不会是 0。
func UserID(c *gin.Context) int64 {
	if v, ok := c.Get(CtxUserID); ok {
		if id, ok := v.(int64); ok {
			return id
		}
	}
	return 0
}

// SetUser 由 JWT 中间件调用，存下这次请求**刚从库里读出来的**那一行 users。
//
// 为什么存整行而不只存 id：中间件本来就要查一次库（为了拿到最新的 role 和 status，
// 好让封号/降权即时生效），查都查了，handler 再查一遍就是纯浪费 ——
// M6 的 admin 校验要看 role，M2 的「仅发帖人」校验要看 id，都会用到它。
//
// 这一行永远是当前请求时刻的最新值，不存在缓存过期问题：每个请求都重新读。
//
// 放在 apperr 包不会造成循环导入 —— apperr 在依赖图的最底层，
// middleware → apperr、model 谁都不 import，方向始终是单向的。
func SetUser(c *gin.Context, u *model.User) { c.Set(CtxUser, u) }

// User 返回当前登录用户；未登录（或没挂 JWT 中间件）时返回 nil。
//
// 返回 nil 而不是 panic，理由和 RequestID/UserID 一样：单元测试里直接调 handler
// 不挂中间件是常态。调用方拿到 nil 就说明「这个路由本该在 JWT 中间件后面」，
// 那是路由配置的 bug，不是这里该兜的。
func User(c *gin.Context) *model.User {
	if v, ok := c.Get(CtxUser); ok {
		if u, ok := v.(*model.User); ok {
			return u
		}
	}
	return nil
}

// SetLogger 由 RequestID 中间件调用：塞一个已经绑好 request_id 的 logger，
// 之后这条请求链路上的每次 Log(c).Info(...) 都自动带上它。
// 这就是「该请求的每条日志都带上 request_id」的实现方式 —— 靠预绑定，不靠每次手写。
func SetLogger(c *gin.Context, l *slog.Logger) { c.Set(CtxLogger, l) }

// Log 返回当前请求的 logger。没挂中间件时退回 slog.Default()。
func Log(c *gin.Context) *slog.Logger {
	if v, ok := c.Get(CtxLogger); ok {
		if l, ok := v.(*slog.Logger); ok {
			return l
		}
	}
	return slog.Default()
}

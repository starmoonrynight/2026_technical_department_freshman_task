package middleware

import (
	"log/slog"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"lostfound/internal/apperr"
)

// Recovery 兜住 panic：打一条带 request_id 和完整堆栈的日志，然后返回统一的 INTERNAL 信封。
//
// 为什么不用 gin.Recovery()：它写的是纯文本 500，会打破 §8「所有 JSON 响应都是同一个信封」的契约，
// 前端的 axios 拦截器解不了。而且它的日志不走我们的 slog，出问题时按 request_id 捞不到。
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			apperr.Log(c).Error("http.panic",
				slog.Any("panic", r),
				slog.String("method", c.Request.Method),
				slog.String("path", c.Request.URL.RequestURI()),
				// 堆栈是排查 panic 的唯一线索，必须整段留下 —— 截断的堆栈等于没有
				slog.String("stack", string(debug.Stack())),
			)
			if !c.Writer.Written() {
				apperr.Fail(c, apperr.New(apperr.CodeInternal))
			}
			c.Abort()
		}()
		c.Next()
	}
}

package middleware

import (
	"log/slog"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lostfound/internal/apperr"
)

// AccessLog 给每个请求打一条结构化日志（§9 四件套之一）。
//
// 字段固定为：time, level, msg, request_id, user_id, method, path, status, duration_ms, err。
// 其中 time / level 由 slog 的 JSON Handler 自己加；request_id 不在这里写，
// 因为 RequestID 中间件已经把它预绑定进 apperr.Log(c) 返回的那个 logger 了 —— 再写一次会出现重复 key。
// err 只在真的出错时出现：每条成功请求都挂一个 err=null 只会让日志变长、grep 变慢。
func AccessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		status := c.Writer.Status()
		// path 用 RequestURI（含 query），因为 ?page=2&keyword=钱包 这种参数往往是排查的关键。
		// 代价是按路径精确过滤时要改成前缀匹配，用 grep 完全够。
		attrs := []any{
			slog.Int64("user_id", apperr.UserID(c)),
			slog.String("method", c.Request.Method),
			slog.String("path", c.Request.URL.RequestURI()),
			slog.Int("status", status),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		}
		if err := apperr.Cause(c); err != nil {
			attrs = append(attrs, slog.String("err", err.Error()))
		}

		switch {
		case status >= 500:
			apperr.Log(c).Error("http.request", attrs...)
		case isQuietPath(c.Request.URL.Path):
			// 健康检查和图片请求量极大，默认级别下会把真正的业务日志冲走。
			// 降级到 Debug：平时看不见，需要排查「图片到底有没有被请求到」时把 LOG_LEVEL 调成 debug 就有。
			apperr.Log(c).Debug("http.request", attrs...)
		default:
			apperr.Log(c).Info("http.request", attrs...)
		}
	}
}

// isQuietPath 报告一个路径是否属于「高频且低信息量」的那一类。
func isQuietPath(path string) bool {
	return path == "/api/health" || strings.HasPrefix(path, "/uploads/")
}

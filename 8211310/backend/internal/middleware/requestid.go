// Package middleware 是 gin 的中间件集合。
package middleware

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"lostfound/internal/apperr"
)

// HeaderRequestID 既是入口也是出口：
// 入口读它（客户端可以自带一个 id，方便把前端的一次操作和后端日志对上），
// 出口写回去（用户报 bug 时能直接从浏览器 devtools 里复制）。
const HeaderRequestID = "X-Request-ID"

// maxRequestIDLen 限制外部传入的长度。
// 不设上限的话，一个 1MB 的 header 会被原样写进每一条日志，日志文件瞬间爆炸。
const maxRequestIDLen = 64

// RequestID 生成（或沿用）请求 id，并把它预绑定进一个 logger 塞进 context。
//
// 这一步是 §9「可 debug 四件套」的第二件：之后这条请求链路上的每次
// apperr.Log(c).Info(...) 都自动带上 request_id，不需要每个调用点手写。
// 报 bug 时给一个 request_id，就能在日志里捞出完整链路。
func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := sanitize(c.GetHeader(HeaderRequestID))
		if id == "" {
			id = newRequestID()
		}

		apperr.SetRequestID(c, id)
		c.Header(HeaderRequestID, id)
		apperr.SetLogger(c, slog.Default().With(slog.String("request_id", id)))

		c.Next()
	}
}

// newRequestID 造一个「日期-随机数」形状的 id，例如 20261007-3f2a91c40b7de518。
//
// 不用 uuid 库（§9）：标准库 crypto/rand + 时间格式化就够了，少一个依赖少一处要解释的东西。
// 前缀带日期是刻意的 —— 日志里肉眼一扫就知道是哪天的请求，也能直接按前缀 grep 一整天的量。
//
// 随机部分用 8 字节（16 位十六进制）而不是计划示例里的 4 位：
// request_id 的全部价值是「给一个 id 就能捞出唯一一条链路」。4 位十六进制只有 6.5 万种，
// 6 位也才 1670 万种 —— 在一天几千个请求的量级上碰撞概率就上了百分之一，
// 一旦撞上，两条请求的日志会混在一起，正好毁掉这个保证。8 字节不花任何代价。
func newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 在 Windows/Linux 上不会失败；真失败了退化成时间戳也比返回空串强，
		// 空 request_id 会让日志里这一整条链路无法被检索。
		return time.Now().UTC().Format("20060102-150405")
	}
	return time.Now().UTC().Format("20060102") + "-" + hex.EncodeToString(b[:])
}

// sanitize 只放行安全字符并截断长度。
//
// 为什么必须过滤：request_id 会原样写进日志。如果放任换行符通过，
// 攻击者就能伪造一行假日志（日志注入），让你以为系统发生过并没有发生的事。
func sanitize(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if len(raw) > maxRequestIDLen {
		raw = raw[:maxRequestIDLen]
	}
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
		}
	}
	return b.String()
}

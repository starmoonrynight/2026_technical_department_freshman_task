// Package handler 只做 HTTP 翻译：绑定请求 → 调 service → 用 apperr 写响应。
// 这一层不写 SQL，也不写业务 if。
package handler

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"lostfound/internal/apperr"
	"lostfound/internal/database"
)

// Version 由构建时注入：
//
//	go build -ldflags "-X lostfound/internal/handler.Version=v0.1.0" ./cmd/server
//
// 不注入时是 "dev"。放在这里而不是写死，是为了让 /api/health 能回答
// 「线上跑的到底是哪个版本」——排查「我明明改了代码怎么没生效」时这是第一句要看的话。
var Version = "dev"

// Health 对应 §4 #38 GET /api/health。
type Health struct {
	Pool *pgxpool.Pool
}

// Get 返回服务与数据库的存活状态。
//
// 数据库连不上时返回 HTTP 500 + code=INTERNAL，但 data 里仍然给出完整形状 ——
// 因为这个接口的用途就是「不登录、不看日志，一句话问清楚哪儿坏了」，
// 把 db 的状态藏起来等于让它失去意义。
func (h Health) Get(c *gin.Context) {
	data := gin.H{
		"status":  "ok",
		"db":      "ok",
		"version": Version,
		"time":    time.Now().UTC().Format(time.RFC3339),
	}

	if err := database.Ping(c.Request.Context(), h.Pool); err != nil {
		apperr.Log(c).Error("health.db_down", slog.String("err", err.Error()))
		data["status"] = "error"
		data["db"] = "error"
		c.JSON(http.StatusInternalServerError, apperr.Envelope{
			Code:      apperr.CodeInternal,
			Message:   "数据库连接失败",
			Data:      data,
			RequestID: apperr.RequestID(c),
		})
		return
	}

	apperr.OK(c, data)
}

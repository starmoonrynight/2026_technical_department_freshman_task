// Package web 装配 Gin 引擎：中间件、路由、静态资源与统一错误处理。
package web

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"lostfound/internal/config"
	"lostfound/internal/httpx"
	"lostfound/internal/store"
	"lostfound/internal/upload"
)

// Deps 路由需要的依赖。
type Deps struct {
	Cfg     config.Config
	Store   *store.Store
	Uploads *upload.Store
}

// NewRouter 组装完整的 HTTP 处理器。
func NewRouter(deps *Deps) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)

	engine := gin.New()
	engine.Use(gin.Recovery())
	engine.Use(bodyParser())

	// multipart 表单在内存里最多保留 2MB，超过部分自动落到临时文件
	engine.MaxMultipartMemory = 2 << 20

	authenticated := &authMiddleware{deps: deps}
	engine.Use(authenticated.AttachUser())

	engine.GET("/api/health", func(c *gin.Context) {
		httpx.SendData(c, gin.H{"ok": true, "time": nowISO()}, http.StatusOK)
	})

	auth := engine.Group("/api/auth")
	registerAuthRoutes(auth, deps)

	items := engine.Group("/api/items")
	registerItemRoutes(items, deps)

	admin := engine.Group("/api/admin")
	registerAdminRoutes(admin, deps)

	uploads := engine.Group("/api/uploads")
	registerUploadRoutes(uploads, deps)

	engine.NoRoute(noRouteHandler(deps.Cfg.PublicDir))
	return engine
}

// noRouteHandler 未匹配的 /api 一律返回 JSON 404，其余交给静态资源。
func noRouteHandler(publicDir string) gin.HandlerFunc {
	serve := staticHandler(publicDir)
	return func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api") {
			c.JSON(http.StatusNotFound, gin.H{"message": "接口不存在"})
			return
		}
		serve(c)
	}
}

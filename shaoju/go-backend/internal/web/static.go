package web

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
)

// staticTypes 显式指定 MIME，避免 Windows 注册表里的 .js 被识别成 text/plain，
// 同时与 Express 4 使用的 mime v1 保持一致。
var staticTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".htm":   "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "application/javascript; charset=utf-8",
	".mjs":   "application/javascript; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".map":   "application/json; charset=utf-8",
	".txt":   "text/plain; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".jpg":   "image/jpeg",
	".jpeg":  "image/jpeg",
	".gif":   "image/gif",
	".webp":  "image/webp",
	".ico":   "image/x-icon",
	".woff":  "font/woff",
	".woff2": "font/woff2",
}

// staticHandler 复刻 express.static(publicDir, { extensions: ['html'], index: 'index.html' })：
// 请求 /login 会返回 public/login.html，请求 / 返回 public/index.html。
func staticHandler(publicDir string) gin.HandlerFunc {
	return func(c *gin.Context) {
		clean := path.Clean("/" + strings.TrimPrefix(c.Request.URL.Path, "/"))

		candidates := make([]string, 0, 3)
		if clean == "/" {
			candidates = append(candidates, "/index.html")
		} else {
			candidates = append(candidates, clean, clean+".html", path.Join(clean, "index.html"))
		}

		for _, candidate := range candidates {
			full := filepath.Join(publicDir, filepath.FromSlash(strings.TrimPrefix(candidate, "/")))
			info, err := os.Stat(full)
			if err != nil || info.IsDir() {
				continue
			}

			c.Header("Cache-Control", "public, max-age=0")
			if mimeType, ok := staticTypes[strings.ToLower(filepath.Ext(full))]; ok {
				c.Header("Content-Type", mimeType)
			}
			http.ServeFile(c.Writer, c.Request, full)
			return
		}

		notFoundPage(c)
	}
}

// notFoundPage 输出与 Express 默认 404 页面一致的内容。
func notFoundPage(c *gin.Context) {
	message := "Cannot " + c.Request.Method + " " + c.Request.URL.Path
	body := "<!DOCTYPE html>\n" +
		"<html lang=\"en\">\n" +
		"<head>\n" +
		"<meta charset=\"utf-8\">\n" +
		"<title>Error</title>\n" +
		"</head>\n" +
		"<body>\n" +
		"<pre>" + escapeHTML(message) + "</pre>\n" +
		"</body>\n" +
		"</html>\n"

	c.Header("Content-Security-Policy", "default-src 'none'")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Data(http.StatusNotFound, "text/html; charset=utf-8", []byte(body))
}

var htmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&#39;",
)

func escapeHTML(value string) string {
	return htmlEscaper.Replace(value)
}

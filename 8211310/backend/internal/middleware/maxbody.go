package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// maxBodyBytes 是 JSON 请求体的上限（1MB）。
//
// 最大的合法请求体是 #13 发帖：5000 字的描述 + 9 条图片路径，撑死 20KB。
// 所以 1MB 已经留了 50 倍余量 —— 它不是用来卡业务大小的，是用来卡
// 「一个客户端能不能让服务器无限地读下去」这件事的。
//
// **为什么需要它**：gin 默认不限制请求体大小。没有这道闸的话，
// 任何一个能连到 8080 端口的人都可以 `curl -X POST --data-binary @/dev/urandom`，
// 我们的进程就会一直在那儿收字节、一直往内存里塞，直到 OOM。
// 这不是理论攻击，是扫描器顺手就会做的事。
const maxBodyBytes int64 = 1 << 20

// MaxBodySize 给请求体加上限。
//
// multipart（#6 图片上传）刻意跳过：它有自己的一套上限
// （单张 5MB、整个请求体 6MB，见 handler.Upload.Create），
// 而且必须**由它自己**来设 —— MaxBytesReader 是可以叠加的，
// 这里先套一层 1MB 的话，上传再套 6MB 也没用，内层那个 1MB 会先炸。
func MaxBodySize() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !strings.HasPrefix(c.ContentType(), "multipart/") {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
		}
		c.Next()
	}
}

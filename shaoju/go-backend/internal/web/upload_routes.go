package web

import (
	"errors"
	"mime"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"lostfound/internal/httpx"
)

// registerUploadRoutes 图片上传接口。
//
//	POST /api/uploads   表单字段名固定为 file，需要登录
//
// 返回的 url 是站内相对路径（/uploads/xxx.png），直接写进 items.image_url 即可。
func registerUploadRoutes(g *gin.RouterGroup, deps *Deps) {
	guarded := g.Group("", RequireAuth())
	guarded.POST("", handle(func(c *gin.Context) error { return uploadImage(c, deps) }))
}

func uploadImage(c *gin.Context, deps *Deps) error {
	store := deps.Uploads
	tooLarge := httpx.New(http.StatusRequestEntityTooLarge,
		"图片不能超过 "+strconv.FormatInt(store.MaxBytes/1024/1024, 10)+" MB")

	// 先确认是 multipart 请求，避免把一个 JSON 请求当成上传去解析
	mediaType, params, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if mediaType != "multipart/form-data" || params["boundary"] == "" {
		return httpx.BadRequest("请使用 multipart/form-data 上传（表单字段名应为 file）")
	}

	// 给整条请求体设上限，避免超大文件把内存与临时目录打满。
	// bodyParser 中间件会跳过 multipart，所以这里拿到的是原始的 Body。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, store.MaxRequestBytes())

	fileHeader, err := c.FormFile("file")
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return tooLarge
		}
		if errors.Is(err, http.ErrMissingFile) {
			return httpx.BadRequest("请选择要上传的图片（表单字段名应为 file）")
		}
		return httpx.BadRequest("解析上传内容失败：" + err.Error())
	}

	if fileHeader.Size > store.MaxBytes {
		return tooLarge
	}

	// 不读客户端给的文件名与 Content-Type：
	// Save 会按文件头判断真实格式，并自己生成随机文件名。
	file, err := fileHeader.Open()
	if err != nil {
		return httpx.BadRequest("读取上传文件失败")
	}
	defer func() { _ = file.Close() }()

	saved, err := store.Save(file)
	if err != nil {
		return err
	}

	httpx.SendData(c, gin.H{
		"url":      saved.URL,
		"filename": saved.Filename,
		"size":     saved.Size,
		"mimeType": saved.MimeType,
	}, http.StatusCreated)
	return nil
}

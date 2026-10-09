package handler

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"lostfound/internal/apperr"
	"lostfound/internal/service"
)

// Upload 对应 §4 的 #6 POST /api/uploads。
type Upload struct {
	Svc *service.Upload
}

// formFieldFile 是 multipart 表单里图片字段的名字。计划 §4 #6 写的是 `file`。
const formFieldFile = "file"

// Create 处理 #6 POST /api/uploads（JWT）。data 是 {path, url}。
//
// 这一层比别的 handler 稍厚一点，因为它要把 multipart 的错误翻译成业务码 ——
// 那些错误是 net/http 特有的，service 不该知道它们存在（§6：service 不 import net/http）。
func (h Upload) Create(c *gin.Context) {
	// ⚠ 必须在读 body 之前掐流量上限。
	//
	// 没有这一行的话，c.FormFile 会老老实实把整个请求体收完再告诉你「太大了」——
	// 一个客户端慢慢地推 10GB 过来，我们的进程就在那儿收 10GB，内存或临时磁盘先炸。
	// MaxBytesReader 让读取在超过上限的那一刻就返回错误，后面的字节根本不进内存。
	//
	// 上限比单张图片的 5MB 多留 1MB，是给 multipart 的边界、字段头这些开销留余量。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, service.MaxUploadBodyBytes())

	fh, err := c.FormFile(formFieldFile)
	if err != nil {
		apperr.Respond(c, translateMultipartErr(err))
		return
	}

	f, err := fh.Open()
	if err != nil {
		apperr.Respond(c, apperr.WrapMsg(err, apperr.CodeValidation, "读取上传的文件失败"))
		return
	}
	defer f.Close()

	// fh.Size 是 mime/multipart 在解析请求体时数出来的，不是客户端声明的，所以可信。
	// service 里还有一道 io.LimitReader 兜底，那是防「申报的大小和实际字节数不一致」。
	res, err := h.Svc.SaveImage(c.Request.Context(), f, fh.Size)
	if err != nil {
		apperr.Respond(c, err)
		return
	}
	apperr.OK(c, res)
}

// translateMultipartErr 把 net/http 的 multipart 错误翻成用户能看懂的业务码。
//
// 三种情况：
//  1. 请求体超过 MaxBytesReader 的上限 → FILE_TOO_LARGE。
//     这条必须单独认出来：不认的话它会掉进最后的通用分支变成 VALIDATION，
//     而用户看到的「请上传 file 字段」和他真正的问题（图太大）毫无关系。
//  2. Content-Type 不是 multipart/form-data，或者表单里没有 file 字段 → VALIDATION。
//  3. 其它（临时文件写失败之类）→ 原样交给 apperr.Respond 变成 INTERNAL。
func translateMultipartErr(err error) error {
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		return apperr.WrapMsg(err, apperr.CodeFileTooLarge, "图片不能超过 5MB")
	}
	if errors.Is(err, http.ErrNotMultipart) || errors.Is(err, http.ErrMissingFile) {
		return apperr.WrapMsg(err, apperr.CodeValidation, "没有收到图片").
			WithField(formFieldFile, "请用 multipart/form-data 上传，字段名是 "+formFieldFile)
	}
	return err
}

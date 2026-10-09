package web

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"

	"lostfound/internal/config"
	"lostfound/internal/httpx"
)

const bodyContextKey = "laf.body"

// bodyParser 复刻 express.json() + express.urlencoded() 的行为：
//   - 只解析 application/json 与 application/x-www-form-urlencoded
//   - 非法 JSON -> 400 请求体不是合法的 JSON
//   - 超过 256kb -> 413 请求内容过大
//   - 其它 Content-Type 或非对象 JSON -> 空对象
//
// multipart/form-data 直接跳过、不读请求体：图片上传要自己调用 ParseMultipartForm，
// 如果这里先把 Body 读空了，后面就拿不到文件了。
func bodyParser() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(bodyContextKey, map[string]any{})

		if c.Request.Body == nil || c.Request.Method == http.MethodGet || c.Request.Method == http.MethodHead {
			c.Next()
			return
		}

		mediaType, _, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
		if mediaType == "multipart/form-data" {
			c.Next()
			return
		}

		limited := http.MaxBytesReader(c.Writer, c.Request.Body, config.MaxBodyBytes)
		raw, err := io.ReadAll(limited)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				httpx.Abort(c, httpx.New(413, "请求内容过大"))
				return
			}
			httpx.Abort(c, httpx.BadRequest("请求体不是合法的 JSON"))
			return
		}
		if len(raw) == 0 {
			c.Next()
			return
		}

		switch {
		case mediaType == "application/json":
			var decoded any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				httpx.Abort(c, httpx.BadRequest("请求体不是合法的 JSON"))
				return
			}
			if object, ok := decoded.(map[string]any); ok {
				c.Set(bodyContextKey, object)
			}
		case mediaType == "application/x-www-form-urlencoded":
			values, err := url.ParseQuery(string(raw))
			if err != nil {
				c.Next()
				return
			}
			object := map[string]any{}
			for key, list := range values {
				if len(list) > 0 {
					object[key] = list[0]
				}
			}
			c.Set(bodyContextKey, object)
		}

		c.Next()
	}
}

// Body 取出当前请求的 JSON / 表单请求体，等价于 Express 的 req.body || {}。
func Body(c *gin.Context) map[string]any {
	if value, ok := c.Get(bodyContextKey); ok {
		if object, ok := value.(map[string]any); ok {
			return object
		}
	}
	return map[string]any{}
}

// BodyField 只取有显式提供的字段，用于区分「未传」与「传了 null」。
func BodyField(c *gin.Context, key string) (any, bool) {
	value, ok := Body(c)[key]
	return value, ok
}

// FieldOrNil 把缺失字段转成 nil，便于复用校验函数。
func FieldOrNil(c *gin.Context, key string) any {
	value, ok := Body(c)[key]
	if !ok {
		return nil
	}
	return value
}

// QueryOrNil 复刻 JS 里 `query.x ? ... : undefined` 的语义：
// 缺失或空字符串视为未提供；同名参数出现多次时返回切片，让校验层按「类型不对」拒绝，
// 这与 Express 把重复参数解析成数组后 Number()/includes() 的行为一致。
func QueryOrNil(c *gin.Context, key string) any {
	values, ok := c.Request.URL.Query()[key]
	if !ok || len(values) == 0 || (len(values) == 1 && values[0] == "") {
		return nil
	}
	if len(values) > 1 {
		return values
	}
	return values[0]
}

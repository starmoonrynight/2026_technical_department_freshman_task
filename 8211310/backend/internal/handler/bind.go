package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"lostfound/internal/apperr"
)

// 这个文件放的是 handler 包共用的请求体绑定辅助函数。
//
// 它们不属于任何一个端点组：auth 的 #1/#2/#4/#5、items 的 #13/#16/#17/#18 都在用。
// 放在包级的单独文件里而不是塞进 auth.go，是因为「解析失败长什么样」必须是
// 全站唯一的一份 —— 前端只有一个 axios 拦截器，它按 code 分支，
// 如果有的接口返回 VALIDATION、有的返回 INTERNAL，前端就得多写一套特例。

// bindJSON 把请求体解析进 dst，失败时返回带字段级细节的 VALIDATION。
//
// 特别处理 UnmarshalTypeError：用户传 {"username": 123} 时，
// 一句「请求体不是合法的 JSON」是没用的（JSON 本身完全合法），
// 得告诉他「username 的类型不对」。te.Field 就是 json 标签里的名字，可以直接用。
func bindJSON(c *gin.Context, dst any) error {
	err := c.ShouldBindJSON(dst)
	if err == nil {
		return nil
	}

	// 请求体超过 middleware.MaxBodySize 的上限时，读到一半就会返回这个错误。
	// 必须比 UnmarshalTypeError 更早认出来：超限的 body 通常是**残缺**的 JSON，
	// 不先拦的话它会掉进下面那个分支，用户收到一句「请求体不是合法的 JSON」——
	// 而他真正的问题是自己发了一个 5MB 的 JSON 过来。
	var maxBytes *http.MaxBytesError
	if errors.As(err, &maxBytes) {
		return apperr.WrapMsg(err, apperr.CodeValidation, "请求体太大了")
	}

	var te *json.UnmarshalTypeError
	if errors.As(err, &te) {
		field := te.Field
		if field == "" {
			field = "(未知字段)"
		}
		return apperr.Validation("字段类型不对",
			apperr.FieldError{Field: field, Msg: "类型不对，期望的是 " + te.Type.String()})
	}

	// 到这里通常是 JSON 语法错误、body 为空、或 Content-Type 不对。
	// 原始错误只进日志（apperr.Respond 会把它交给访问日志），
	// 响应里给一句人能看懂的中文 —— 语法错误的细节对最终用户毫无意义。
	return apperr.WrapMsg(err, apperr.CodeValidation, "请求体不是合法的 JSON")
}

// bindJSONOptional 同 bindJSON，但**空请求体算成功**（dst 保持零值）。
//
// 只给 DELETE 这种「请求体纯属可选」的端点用。给 POST/PUT 用是错的 ——
// 那样一个忘了带 body 的请求会被当成「所有字段都是零值」继续往下走，
// 用户收到的是「标题不能为空」而不是「你没发请求体」，排查时会怀疑是前端漏了字段。
//
// 整个 body 读进内存是安全的：middleware.MaxBodySize 已经在路由层掐了上限。
func bindJSONOptional(c *gin.Context, dst any) error {
	if c.Request.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return apperr.WrapMsg(err, apperr.CodeValidation, "读取请求体失败")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}

	// 非空就把读出来的字节塞回去，交给 bindJSON 去解析。
	// 这里刻意不自己再解一遍：那三段错误翻译只能有一份，
	// 抄出来的第二份迟早会和原版走偏。
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	return bindJSON(c, dst)
}

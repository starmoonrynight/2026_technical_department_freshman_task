// Package httpx 定义统一的业务异常与响应封装，对应 Node 版 src/utils/http.js。
//
// 契约：
//   - 成功：HTTP 状态码 + {"data": ...}
//   - 失败：HTTP 状态码 + {"message": "..."}，可选 details
package httpx

import (
	"fmt"

	"github.com/gin-gonic/gin"
)

// ApiError 是带 HTTP 状态码的业务异常。
type ApiError struct {
	Status  int
	Message string
	Details any
}

func (e *ApiError) Error() string {
	return fmt.Sprintf("%d %s", e.Status, e.Message)
}

// New 构造一个业务异常。
func New(status int, message string) *ApiError {
	return &ApiError{Status: status, Message: message}
}

// NewWithDetails 构造一个带明细的业务异常。
func NewWithDetails(status int, message string, details any) *ApiError {
	return &ApiError{Status: status, Message: message, Details: details}
}

// BadRequest 400：参数校验失败。
func BadRequest(message string) *ApiError { return New(400, message) }

// Unauthorized 401：未登录。
func Unauthorized(message string) *ApiError {
	return New(401, orDefault(message, "请先登录"))
}

// Forbidden 403：权限不足。
func Forbidden(message string) *ApiError {
	return New(403, orDefault(message, "没有权限执行该操作"))
}

// NotFound 404：资源不存在。
func NotFound(message string) *ApiError {
	return New(404, orDefault(message, "资源不存在"))
}

// Conflict 409：与已有数据冲突。
func Conflict(message string) *ApiError { return New(409, message) }

// SendData 输出统一成功响应。
func SendData(c *gin.Context, data any, status int) {
	c.JSON(status, gin.H{"data": data})
}

// Abort 立即结束请求并输出错误响应。
func Abort(c *gin.Context, err error) {
	api, ok := err.(*ApiError)
	if !ok {
		c.JSON(500, gin.H{"message": "服务器内部错误"})
		return
	}

	body := gin.H{"message": api.Message}
	if api.Details != nil {
		body["details"] = api.Details
	}
	c.AbortWithStatusJSON(api.Status, body)
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

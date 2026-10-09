package apperr

import (
	"errors"
	"fmt"
)

// FieldError 是字段级校验失败的一条细节。
// 它会出现在响应的 data.errors 里，前端拿到 Field 就能把焦点定位到具体输入框。
//
// 计划 §8：admin 治理端点缺 reason 时不新加错误码，复用 VALIDATION，
// 细节写成 {field:"reason", msg:"管理员操作必须填写理由"}。
type FieldError struct {
	Field string `json:"field"`
	Msg   string `json:"msg"`
}

// Error 是本系统唯一的业务错误类型。
//
// 分层纪律（§9 四件套之四）：
//   - repo / service 用下面的构造器造它，或用 fmt.Errorf("repo.GetItem(%d): %w", id, err) 包装普通错误
//   - handler 只在最外层用 Respond(c, err) 转换，不允许自己拼 JSON
//   - Err 是内部原因，只进日志，绝不出现在响应体里（响应里只有 request_id + 通用文案）
type Error struct {
	Code       string
	HTTPStatus int
	Message    string // 给人看的中文，可以随时改措辞，前端不许依赖它
	Fields     []FieldError
	Err        error
}

// Error 实现 error 接口。带上 Code 是为了日志里一眼看出是哪类错误。
func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap 让 errors.Is / errors.As 能穿透到内部原因，
// 例如 errors.Is(err, pgx.ErrNoRows) 在包了一层 apperr 之后依然成立。
func (e *Error) Unwrap() error { return e.Err }

// New 用错误码造一个错误，message 用该码的默认中文文案。
func New(code string) *Error {
	status, msg := Lookup(code)
	return &Error{Code: code, HTTPStatus: status, Message: msg}
}

// NewMsg 用错误码造一个错误，并覆盖默认文案。
// 例：NewMsg(CodeNotFound, "帖子不存在") 比 "资源不存在" 对用户有用得多。
func NewMsg(code, message string) *Error {
	status, _ := Lookup(code)
	return &Error{Code: code, HTTPStatus: status, Message: message}
}

// Wrap 把一个内部错误包成业务错误，保留原因供日志和 errors.Is 使用。
func Wrap(err error, code string) *Error {
	e := New(code)
	e.Err = err
	return e
}

// WrapMsg 同 Wrap，但覆盖文案。
func WrapMsg(err error, code, message string) *Error {
	e := NewMsg(code, message)
	e.Err = err
	return e
}

// Internal 是「预料之外」的错误：500 + 通用文案，真实原因只进日志。
func Internal(err error) *Error {
	e := New(CodeInternal)
	e.Err = err
	return e
}

// NotFound 造一个带资源名的 404。
func NotFound(what string) *Error {
	return NewMsg(CodeNotFound, what+"不存在")
}

// Forbidden 造一个带原因的 403。
// 注意：这里的 message 只是给用户看的解释，不是权限判断本身 ——
// 权限判断在 service 层，它决定了要不要造这个错误。
func Forbidden(why string) *Error {
	return NewMsg(CodeForbidden, why)
}

// Validation 造一个 400，可带任意条字段级细节。
func Validation(message string, fields ...FieldError) *Error {
	e := NewMsg(CodeValidation, message)
	e.Fields = fields
	return e
}

// WithField 追加一条字段级细节，返回自身以便链式调用。
func (e *Error) WithField(field, msg string) *Error {
	e.Fields = append(e.Fields, FieldError{Field: field, Msg: msg})
	return e
}

// IsCode 报告 err（可能被包装过若干层）是不是某个错误码。
// 用法：if apperr.IsCode(err, apperr.CodeItemClosed) { ... }
//
// 只比 Code，不比 Message —— Message 随时会改措辞。
func IsCode(err error, code string) bool {
	var ae *Error
	if errors.As(err, &ae) {
		return ae.Code == code
	}
	return false
}

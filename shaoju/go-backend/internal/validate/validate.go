// Package validate 是轻量请求参数校验工具，行为与 Node 版 src/utils/validate.js 对齐：
// 校验失败直接返回 400 业务异常，错误文案逐字一致。
package validate

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"

	"lostfound/internal/httpx"
)

// MaxSafeInteger 与 JS 的 Number.MAX_SAFE_INTEGER 一致。
const MaxSafeInteger = 1<<53 - 1

// P 返回值的指针，用于表达「可选参数」。
func P[T any](v T) *T { return &v }

// StrOpts 对应 JS 里 str(value, field, options)。
type StrOpts struct {
	Required bool
	Min      int
	Max      int // 0 表示使用默认上限 200
	Label    string
}

// Str 校验并裁剪字符串。value 为 nil 时按未提供处理。
func Str(value any, field string, o StrOpts) (string, error) {
	label := orDefault(o.Label, field)
	maximum := o.Max
	if maximum == 0 {
		maximum = 200
	}

	if value == nil {
		if o.Required {
			return "", httpx.BadRequest(label + "不能为空")
		}
		return "", nil
	}

	s, ok := value.(string)
	if !ok {
		return "", httpx.BadRequest(label + "格式不正确")
	}

	trimmed := trim(s)
	if o.Required && trimmed == "" {
		return "", httpx.BadRequest(label + "不能为空")
	}
	if trimmed != "" && utf16Len(trimmed) < o.Min {
		return "", httpx.BadRequest(fmt.Sprintf("%s至少需要 %d 个字符", label, o.Min))
	}
	if utf16Len(trimmed) > maximum {
		return "", httpx.BadRequest(fmt.Sprintf("%s不能超过 %d 个字符", label, maximum))
	}
	return trimmed, nil
}

// IntOpts 对应 JS 里 int(value, field, options)。
type IntOpts struct {
	Min      *int
	Max      *int
	Fallback *int
	Label    string
}

// Int 校验整数参数；空值走 fallback，非法值报错。
// 与 JS 的 Number() 保持一致：允许 "2.0"、忽略首尾空白、拒绝 "abc" 与 "1.5"。
func Int(value any, field string, o IntOpts) (int, error) {
	label := orDefault(o.Label, field)
	minimum := 1
	if o.Min != nil {
		minimum = *o.Min
	}
	maximum := MaxSafeInteger
	if o.Max != nil {
		maximum = *o.Max
	}

	if value == nil || value == "" {
		if o.Fallback != nil {
			return *o.Fallback, nil
		}
		return 0, httpx.BadRequest(label + "必须是数字")
	}

	raw, ok := value.(string)
	if !ok {
		return 0, httpx.BadRequest(label + "必须是整数")
	}
	if raw == "" {
		if o.Fallback != nil {
			return *o.Fallback, nil
		}
		return 0, httpx.BadRequest(label + "必须是数字")
	}

	// JS 的 Number(' ') === 0，纯空白要按 0 处理，而不是当成「未提供」。
	trimmed := trim(raw)
	if trimmed == "" {
		trimmed = "0"
	}

	n, err := parseNumber(trimmed)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, httpx.BadRequest(label + "必须是整数")
	}
	if n != math.Trunc(n) {
		return 0, httpx.BadRequest(label + "必须是整数")
	}

	i := int(n)
	if i < minimum || i > maximum {
		return 0, httpx.BadRequest(fmt.Sprintf("%s必须在 %d 到 %d 之间", label, minimum, maximum))
	}
	return i, nil
}

// parseNumber 覆盖 JS Number() 支持的十进制、十六进制、二进制与八进制字面量。
func parseNumber(value string) (float64, error) {
	lower := strings.ToLower(value)
	sign := 1.0
	body := lower

	if strings.HasPrefix(body, "+") {
		body = body[1:]
	} else if strings.HasPrefix(body, "-") {
		sign = -1
		body = body[1:]
	}

	switch {
	case strings.HasPrefix(body, "0x"):
		n, err := strconv.ParseUint(body[2:], 16, 64)
		if err != nil {
			return 0, err
		}
		return sign * float64(n), nil
	case strings.HasPrefix(body, "0b"):
		n, err := strconv.ParseUint(body[2:], 2, 64)
		if err != nil {
			return 0, err
		}
		return sign * float64(n), nil
	case strings.HasPrefix(body, "0o"):
		n, err := strconv.ParseUint(body[2:], 8, 64)
		if err != nil {
			return 0, err
		}
		return sign * float64(n), nil
	}
	return strconv.ParseFloat(value, 64)
}

// OneOfOpts 对应 JS 里 oneOf(value, field, allowed, options)。
type OneOfOpts struct {
	Fallback *string
	Label    string
}

// OneOf 校验枚举值。
func OneOf(value any, field string, allowed []string, o OneOfOpts) (string, error) {
	label := orDefault(o.Label, field)
	valid := fmt.Sprintf("%s只能是 %s 之一", label, strings.Join(allowed, " / "))

	if value == nil {
		if o.Fallback != nil {
			return *o.Fallback, nil
		}
		return "", httpx.BadRequest(label + "不能为空")
	}

	s, ok := value.(string)
	if !ok {
		return "", httpx.BadRequest(valid)
	}
	if s == "" {
		if o.Fallback != nil {
			return *o.Fallback, nil
		}
		return "", httpx.BadRequest(label + "不能为空")
	}
	for _, item := range allowed {
		if item == s {
			return s, nil
		}
	}
	return "", httpx.BadRequest(valid)
}

var studentIDPattern = regexp.MustCompile(`^\d{8}$`)

// StudentID 校验学号：恰好 8 位数字，例如 20230101。
//
// 长度和字符集合并成一条规则，所以任何不合规的写法都返回同一句提示，
// 前端照抄这句文案。Max 只作为防御性上界，避免超长输入进正则。
func StudentID(value any) (string, error) {
	v, err := Str(value, "studentId", StrOpts{Required: true, Max: 32, Label: "学号"})
	if err != nil {
		return "", err
	}
	if !studentIDPattern.MatchString(v) {
		return "", httpx.BadRequest("学号必须是 8 位数字")
	}
	return v, nil
}

// Name 校验姓名：1~32 个字符，允许中文。
func Name(value any) (string, error) {
	return Str(value, "name", StrOpts{Required: true, Min: 1, Max: 32, Label: "姓名"})
}

// imageURLPattern 允许两种写法：
//   - 站内上传路径 /uploads/xxx.png（由 POST /api/uploads 返回）
//   - 普通 http(s) 图片链接
var imageURLPattern = regexp.MustCompile(`^(/uploads/[A-Za-z0-9._-]+|https?://\S+)$`)

// ImageURL 校验图片地址。留空表示没有图片；非空必须是站内上传路径或 http(s) 链接，
// 这样 data:、javascript: 之类的写法进不了数据库、也进不了前端的 <img src>。
func ImageURL(value any) (string, error) {
	v, err := Str(value, "imageUrl", StrOpts{Max: 500, Label: "图片地址"})
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", nil
	}
	if !imageURLPattern.MatchString(v) {
		return "", httpx.BadRequest("图片地址只能是 /uploads/ 开头的站内路径或 http(s) 链接")
	}
	return v, nil
}

// Password 校验口令长度（6~64 位），不做裁剪，与 JS 的 password() 一致。
func Password(value any, label string) (string, error) {
	if label == "" {
		label = "密码"
	}

	s, ok := value.(string)
	if !ok {
		return "", httpx.BadRequest(label + "格式不正确")
	}
	if utf16Len(s) < 6 {
		return "", httpx.BadRequest(label + "至少需要 6 位")
	}
	if utf16Len(s) > 64 {
		return "", httpx.BadRequest(label + "不能超过 64 位")
	}
	return s, nil
}

// trim 等价于 JS 的 String.prototype.trim()。
func trim(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || r == '\uFEFF'
	})
}

// utf16Len 按 UTF-16 码元计数，与 JS 的 String.length 对齐。
func utf16Len(s string) int {
	return len(utf16.Encode([]rune(s)))
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

package apperr

// 本文件是 §8「错误码与响应信封」的落地，也是前端与冒烟测试的契约。
//
// 铁律：Code 是稳定的机器码，永不改语义、永不复用。
// Message 是给人看的中文，随时可以改措辞。
// 所以前端和测试只允许 match Code，禁止依赖 Message 文本 —— 按人类可读文本做分支判断是脆的。
//
// 计数：下面恰好 22 个常量 = OK + 21 个错误码。加新码时请同步 specs 表和计划 §8 的表格。

const (
	CodeOK       = "OK"
	CodeInternal = "INTERNAL"

	// 通用
	CodeValidation       = "VALIDATION"
	CodeUnauthorized     = "UNAUTHORIZED"
	CodeForbidden        = "FORBIDDEN"
	CodeNotFound         = "NOT_FOUND"
	CodeMethodNotAllowed = "METHOD_NOT_ALLOWED"
	CodeConflict         = "CONFLICT"

	// 认证
	CodeUserAlreadyExists  = "USER_ALREADY_EXISTS"
	CodeUserBanned         = "USER_BANNED"
	CodeInvalidCredentials = "INVALID_CREDENTIALS"
	CodeWeakPassword       = "WEAK_PASSWORD"
	CodeOldPasswordWrong   = "OLD_PASSWORD_WRONG"

	// 上传
	CodeFileTooLarge        = "FILE_TOO_LARGE"
	CodeFileTypeUnsupported = "FILE_TYPE_UNSUPPORTED"

	// 帖子
	CodeItemClosed = "ITEM_CLOSED"

	// 归还确认
	CodeReturnDuplicate         = "RETURN_DUPLICATE"
	CodeReturnIllegalTransition = "RETURN_ILLEGAL_TRANSITION"
	CodeReturnSelf              = "RETURN_SELF"

	// 字典
	CodeCategoryInUse = "CATEGORY_IN_USE"

	// 举报与治理
	CodeReportDuplicate       = "REPORT_DUPLICATE"
	CodeReportAlreadyResolved = "REPORT_ALREADY_RESOLVED"
)

// spec 是一个错误码的固定属性：HTTP 状态码 + 默认中文文案。
type spec struct {
	HTTPStatus int
	Message    string
}

var specs = map[string]spec{
	CodeOK:       {200, "ok"},
	CodeInternal: {500, "服务器内部错误"},

	CodeValidation:       {400, "请求参数校验失败"},
	CodeUnauthorized:     {401, "未登录或登录已过期"},
	CodeForbidden:        {403, "没有权限执行此操作"},
	CodeNotFound:         {404, "资源不存在"},
	CodeMethodNotAllowed: {405, "请求方法不被允许"},
	CodeConflict:         {409, "资源冲突"},

	CodeUserAlreadyExists: {409, "用户名已被注册"},
	CodeUserBanned:        {403, "账号已被封禁，如有疑问请联系管理员"},
	// ⚠ 故意不区分「用户名不存在」和「密码错误」—— 区分了就能用来枚举有哪些账号
	CodeInvalidCredentials: {401, "用户名或密码错误"},
	CodeWeakPassword:       {400, "密码至少 8 位，且不能是纯数字"},
	CodeOldPasswordWrong:   {400, "原密码不正确"},

	CodeFileTooLarge:        {413, "图片不能超过 5MB"},
	CodeFileTypeUnsupported: {415, "只支持 jpg / png / webp / gif 格式的图片"},

	// 语义边界：改自己的帖子时 open/closed 都能改，只有 status='deleted' 才拦（§4 #16）
	CodeItemClosed: {409, "帖子已关闭或已删除，无法执行此操作"},

	CodeReturnDuplicate:         {409, "你已经提交过一条待处理的归还确认"},
	CodeReturnIllegalTransition: {409, "该归还确认当前状态不允许此操作"},
	CodeReturnSelf:              {400, "不能对自己的帖子提交归还确认"},

	CodeCategoryInUse: {409, "该分类或地点仍被帖子引用，或仍有子节点，无法删除"},

	CodeReportDuplicate:       {409, "你已经举报过这条帖子，管理员会看到"},
	CodeReportAlreadyResolved: {409, "这条举报已经被处置过了"},
}

// Lookup 返回一个码的 HTTP 状态码与默认文案。
// 未知码直接 panic：那是写错了常量名的程序员错误，
// 而如果放任它返回 0，gin 会写出一个 HTTP 200 的错误响应 —— 那种 bug 极难发现。
func Lookup(code string) (httpStatus int, message string) {
	s, ok := specs[code]
	if !ok {
		panic("apperr: 未注册的错误码 " + code + "，请在本文件的常量和 specs 表里都加上它")
	}
	return s.HTTPStatus, s.Message
}

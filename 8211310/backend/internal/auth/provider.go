// Package auth 负责一件事：确认「这个人是谁」。
//
// 它**不负责**签发 JWT —— 那是 service/auth.go 的事（计划 §7.3）。这个切分是
// M8 能低成本插入杭电助手 SSO 的全部原因：SSO 只是换一种「确认是谁」的方式，
// 确认完之后签 token、写 users 表、返回信封的流程一模一样，一行都不用改。
//
// 依赖方向：auth → model（+ 一个自己定义的窄接口），不 import repo 的具体类型、
// 不 import gin、不 import net/http。所以这一层能被纯单元测试覆盖（§10 第①层）。
package auth

import "context"

// Provider 的两种实现对应的 source 值。
// 和 model.AuthSource* 是同两个字面量，这里再写一遍是为了让 auth 包不必 import model
// 就能表达自己的语义 —— 但实际上 Identity.Source 会直接存进 users.auth_source，
// 所以两边必须一致。改一处忘了另一处的后果是「登录成功但建不出号」，会立刻暴露。
const (
	SourceLocal   = "local"
	SourceHDUHelp = "hduhelp"
)

// Identity 是「已经确认过身份」的结果。
//
// 字段照计划 §7.3 原样，刻意不带 user id：Provider 只回答「这是谁」，
// 「他在我们库里是第几行」是 service 拿着这个身份去查/去建的结果。
// 让 Provider 返回 id 就等于要求它自己管 users 表，M8 的 SSO 实现会被迫
// 复制一遍 find-or-create 逻辑。
type Identity struct {
	// ExternalID 是外部系统的全局用户标识（杭电助手的 userId）。本地登录时为空串。
	// M8 用它做 find-or-create 的键，users.sso_user_id 上有部分唯一索引兜底。
	ExternalID string
	// Name 是显示名：SSO 场景下是真实姓名，本地场景下是用户名。
	Name string
	// AvatarURL 头像。本地注册没有，SSO 会带回来。
	AvatarURL string
	// Source 是 "local" | "hduhelp"，直接落进 users.auth_source。
	Source string
}

// Credentials 是「用来证明身份的凭据」。
//
// 一个结构体同时容纳两种登录方式，而不是定成两个接口方法：
// 本地登录填 Username/Password，SSO 登录填 Code/CodeVerifier（M8 追加）。
// 用不到的字段留空 —— 这比给 Provider 加第二个方法好，因为那样 service
// 就得知道该调哪个，而它本来不该知道用户是从哪个门进来的。
type Credentials struct {
	// Username 本地登录用。
	Username string
	// Password 本地登录用，明文，只在内存里活到 bcrypt 比对完那一刻。
	Password string

	// Code / CodeVerifier 是 M8 的 OAuth2 授权码 + PKCE 用。
	// 现在不写这两个字段（§8 的同款纪律：不预留用不上的东西）——
	// M8 开工时加，编译器会指出所有需要改的地方。
}

// Provider 是认证方式的抽象。M1 只有 LocalProvider；M8 加 HDUHelpProvider。
//
// 契约：
//   - 成功返回非 nil 的 *Identity 和 nil error
//   - 失败返回 nil 和 apperr（**绝不返回 (nil, nil)**，那会让调用方在
//     解引用时 panic，而且 panic 发生在「认证失败」这条最常见的路径上）
//   - 失败必须是 apperr 的业务码（INVALID_CREDENTIALS / USER_BANNED），
//     不能把 pgx 的原始错误透出去 —— 那里面可能有 SQL 片段和表结构（§8）
type Provider interface {
	Authenticate(ctx context.Context, cred Credentials) (*Identity, error)
}

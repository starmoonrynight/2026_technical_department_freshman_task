// Package model 只放纯数据结构体：没有 SQL，没有 HTTP，没有业务判断。
//
// 为什么单独一层：repo 扫出来的行、service 传递的对象、handler 返回的形状，
// 三者高度重合但不完全相同。把它们塞进任何一个包都会让那个包同时依赖另外两个，
// 而 model 谁都不依赖，所以放在依赖链最底层，谁都能 import 它。
package model

import "time"

// users 表的枚举值。
//
// 写成常量而不是在各处直接写 "banned" 字面量，是为了让拼写错误在编译期就红：
// 'banned' 打成 'baned' 不会有任何报错，只会在运行时表现为「封号了但还能登录」，
// 而这种 bug 从现象往回查到那个字母要多花很久。
// 数据库侧还有 CHECK 兜底（000001 迁移里 role/status/auth_source 各一条）。
const (
	// AuthSourceLocal 本地账号密码。计划 §7.1：这是**永久必备**，不是 SSO 上线前的过渡方案 ——
	// 它既是 SSO 阻塞期的唯一登录方式，也是开发和跑测试的唯一方式（冒烟测试不能依赖外部服务）。
	AuthSourceLocal = "local"
	// AuthSourceHDUHelp 杭电助手 SSO。M8 才会真的写进库，这里先把值定下来。
	AuthSourceHDUHelp = "hduhelp"

	RoleUser  = "user"
	RoleAdmin = "admin"

	UserStatusActive = "active"
	UserStatusBanned = "banned"
)

// User 对应 users 表的一行。
//
// 可空列一律用指针（*string / *int64），不用空串代替 NULL。
// 这不是风格洁癖，是因为这几列的 NULL 有真实语义：
//   - Username / PasswordHash 为 NULL 表示「这是个 SSO 用户，他根本没有密码」。
//     如果用空串表示，那么「SSO 用户」和「密码是空字符串的本地用户」就没法区分了 ——
//     后者是个严重的安全漏洞（bcrypt 比对空串会失败，但代码里任何 `if hash == ""` 的
//     捷径都可能把它放过去）。
//   - StudentID 大概率永远是 NULL（计划 §7.2：杭电助手的 base 身份不返回学号）。
//
// 代价是取值时要判 nil，View() 已经把这件事收在一处了。
type User struct {
	ID           int64
	Username     *string // 本地账号才有
	PasswordHash *string // 本地账号才有；永不存明文（§2.2）
	AuthSource   string
	SSOUserID    *string // 杭电助手全局 userId
	RealName     *string
	StudentID    *string // 见上，可能永远为 NULL
	AvatarURL    *string
	Nickname     string
	Role         string
	Status       string
	Phone        *string
	Email        *string
	CreditScore  int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// IsBanned 报告账号是否被封禁。
// 封禁的语义是「登不上、也操作不了」，两处都要拦（§8 USER_BANNED）：
// 登录时在 auth.LocalProvider 里拦，带 token 访问时在 middleware.JWT 里拦。
// 少了后者，admin 封号之后那个人手上的旧 token 还能继续用满 24 小时。
func (u *User) IsBanned() bool { return u.Status == UserStatusBanned }

// IsAdmin 报告是否管理员。M6 的 admin 路由校验用它。
func (u *User) IsAdmin() bool { return u.Role == RoleAdmin }

// IsLocal 报告是否本地账号密码用户。
// 只有本地用户能改密码（#5）：SSO 用户的密码在杭电助手那边，我们没有也不该有。
func (u *User) IsLocal() bool { return u.AuthSource == AuthSourceLocal }

// UserView 是 #3 GET /api/auth/me 的 data 形状，也是 #2 登录响应里 user 字段的形状。
//
// 字段和顺序照计划 §4 第 3 行原样落地，**11 个，不多不少**：
// 注意 student_id 不在里面 —— 计划没列它，而它大概率永远是 NULL（§7.2），
// 返回一个恒为 null 的字段只会让前端以为「以后会有」而写出等它的代码。
//
// password_hash 绝不在这里出现。这个结构体存在的另一个意义就是当白名单：
// 只有写在这里的字段才可能泄漏出去，将来给 User 加敏感列时不会被顺手带出。
type UserView struct {
	ID          int64  `json:"id"`
	Username    string `json:"username"`
	Nickname    string `json:"nickname"`
	RealName    string `json:"real_name"`
	Role        string `json:"role"`
	AuthSource  string `json:"auth_source"`
	Phone       string `json:"phone"`
	Email       string `json:"email"`
	AvatarURL   string `json:"avatar_url"`
	CreditScore int    `json:"credit_score"`
	CreatedAt   string `json:"created_at"`
}

// View 把一行 users 转成对外形状。
//
// NULL 一律转成空串而不是 JSON null。理由：这些字段在前端全部是「有就显示、没有就不显示」，
// 空串和 null 的处理完全一样，但空串能让 TS 那边的类型是 string 而不是 string | null，
// 少一层到处判空的噪音。需要区分「没填」和「SSO 用户没有用户名」的场合看 auth_source 就够了。
func (u *User) View() UserView {
	return UserView{
		ID:          u.ID,
		Username:    deref(u.Username),
		Nickname:    u.Nickname,
		RealName:    deref(u.RealName),
		Role:        u.Role,
		AuthSource:  u.AuthSource,
		Phone:       deref(u.Phone),
		Email:       deref(u.Email),
		AvatarURL:   deref(u.AvatarURL),
		CreditScore: u.CreditScore,
		// 显式格式化而不是直接放 time.Time：一是和 /api/health 的 time 字段保持一致
		// （那边也是 RFC3339 字符串），二是 time.Time 序列化会带上数据库会话的时区偏移，
		// 先 UTC() 再格式化能保证前端拿到的永远是同一个形状，不用猜那个 +08:00 是怎么来的。
		CreatedAt: u.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// deref 把 *string 的 NULL 折成空串。
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Package service 放业务规则（计划 §6 分层规则）。
//
// 这一层不 import net/http、不 import gin、不写 SQL。
// 好处是它能被纯单元测试覆盖：给定输入断言输出，不用起 HTTP server、不用连数据库。
// 出 bug 时也能立刻分清是「规则错了」还是「查询错了」还是「HTTP 绑定错了」。
package service

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"lostfound/internal/apperr"
	"lostfound/internal/auth"
	"lostfound/internal/model"
	"lostfound/internal/repo"
)

// 字段长度上限，和 000001 迁移里的 VARCHAR/CHECK 一一对应。
//
// 为什么在 Go 里再写一遍而不是让数据库报错：数据库超长会抛
// "value too long for type character varying(32)"，那是 INTERNAL 500，
// 用户看到的是一句英文技术细节。在 service 层先拦，才能返回 VALIDATION +
// 字段名，前端能把焦点定位到具体输入框（§8）。
//
// ⚠ 单位是**字符数**不是字节数：PG 的 VARCHAR(n) 和 char_length() 数的都是字符，
// 一个汉字算 1。这里必须用 utf8.RuneCountInString 对齐，用 len() 的话
// 「昵称最长 32」会变成「最长 10 个汉字」，和数据库的实际行为不一致。
const (
	usernameMinChars = 3
	usernameMaxChars = 32
	nicknameMaxChars = 32
	phoneMaxChars    = 20
	emailMaxChars    = 128
)

// UserStore 是 Auth 需要的**全部**持久化能力。
//
// 定成窄接口而不是直接用 *repo.User，和 auth.UserLookup、middleware.UserByID
// 是同一个理由：M1 的验收判据要求「fakeProvider 能被注入且 JWT 签发逻辑与
// Provider 无关」这条**单元测试**。如果 users 是具体类型，这条测试就必须连数据库，
// 于是它从 §10 的第①层（纯单元测试，秒级、无外部依赖）掉到第②层，
// 「Provider 换掉之后签发逻辑不受影响」这个论断也就再也不能脱离数据库被证明了。
//
// 顺带的好处是依赖面写在类型上：Auth 只用这 5 个方法，repo.User 将来长出
// 十几个方法也不会悄悄扩大 service 能碰到的东西。
type UserStore interface {
	CreateLocal(ctx context.Context, p repo.NewLocalUser) (*model.User, error)
	GetByID(ctx context.Context, id int64) (*model.User, error)
	GetByUsername(ctx context.Context, username string) (*model.User, error)
	UpdateProfile(ctx context.Context, id int64, nickname string, phone, email *string) (*model.User, error)
	UpdatePasswordHash(ctx context.Context, id int64, hash string) error
}

// Auth 是认证相关的业务规则。
//
// provider 是接口而不是具体的 LocalProvider —— 这就是计划 §7.3 说的 seam：
// M8 接杭电助手时只需要在 router.Setup 里换一个实现（或加一个按 source 分发的包装），
// 这个文件一行都不用改。service/auth_test.go 里的 fakeProvider 已经在证明这件事了。
type Auth struct {
	users    UserStore
	provider auth.Provider
	signer   auth.TokenSigner
	logger   *slog.Logger
}

// NewAuth 组装。依赖在 router.Setup 里手工 new（§6：不用 wire/fx）。
func NewAuth(users UserStore, provider auth.Provider, signer auth.TokenSigner, logger *slog.Logger) *Auth {
	if logger == nil {
		logger = slog.Default()
	}
	return &Auth{users: users, provider: provider, signer: signer, logger: logger}
}

// RegisterInput 是 #1 POST /api/auth/register 的请求体。
type RegisterInput struct {
	Username string
	Password string
	Nickname string // 可选
}

// Register 建一个本地账号。响应形状见 §4 第 1 行：{id, username, nickname, role}。
//
// 重名不靠「先 SELECT 再 INSERT」判断：那是竞态的，两个请求同时通过检查，
// 后一个会在 INSERT 时撞唯一索引。这里直接插，让 repo 把 23505 翻译成
// USER_ALREADY_EXISTS —— 唯一索引是原子的，不会漏。
func (s *Auth) Register(ctx context.Context, in RegisterInput) (*model.User, error) {
	username := strings.TrimSpace(in.Username)
	nickname := strings.TrimSpace(in.Nickname)

	if err := validateUsername(username); err != nil {
		return nil, err
	}
	if err := auth.ValidatePassword(in.Password); err != nil {
		return nil, err
	}
	// 昵称留空就回落到用户名。
	// users.nickname 是 NOT NULL DEFAULT ''，放任它为空的话，广场、通知、
	// 归还确认的每一个「显示名」位置都会渲染成一片空白 —— 而这个系统的
	// 所有交互都是「看名字认人」，空名字等于没有身份。
	if nickname == "" {
		nickname = username
	}
	if err := validateLength("nickname", nickname, nicknameMaxChars); err != nil {
		return nil, err
	}

	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("service.Auth.Register: %w", err))
	}

	u, err := s.users.CreateLocal(ctx, repo.NewLocalUser{
		Username:     username,
		PasswordHash: hash,
		Nickname:     nickname,
	})
	if err != nil {
		return nil, err
	}

	// 记 user_id 和 username，**绝不记密码**（明文和哈希都不记）。
	// 日志会长期留存，把哈希写进去等于把离线爆破的原料交出去。
	s.logger.InfoContext(ctx, "auth.register",
		slog.Int64("user_id", u.ID),
		slog.String("username", u.View().Username))

	return u, nil
}

// LoginResult 是 #2 POST /api/auth/login 的 data 形状：{token, expires_at, user}。
type LoginResult struct {
	Token     string         `json:"token"`
	ExpiresAt string         `json:"expires_at"`
	User      model.UserView `json:"user"`
}

// Login 验凭证 → 取用户 → 签 JWT。
//
// 分成「Provider 确认身份」和「repo 取行」两步，看起来多查了一次库，
// 但这是 §7.3 那个切分的直接代价，而且值得：Provider 只回答「这是谁」，
// 完全不知道 users 表长什么样，所以 M8 的 SSO 实现能塞进同一个位置。
// 登录是低频操作，多一次主键/唯一索引查询换掉一整层耦合，划算。
func (s *Auth) Login(ctx context.Context, username, password string) (*LoginResult, error) {
	// ⚠ 这里必须和 Register 用**完全相同**的规范化（TrimSpace）。
	// 注册时 " alice" 被存成 "alice"，登录时如果不 trim，用户照着输入框里的
	// 内容再打一遍（或者密码管理器回填时带了空格）就永远登不上，
	// 而后端日志里会显示「用户名 alice 不存在」—— 明明就在库里。
	username = strings.TrimSpace(username)

	if _, err := s.provider.Authenticate(ctx, auth.Credentials{
		Username: username,
		Password: password,
	}); err != nil {
		// Provider 已经返回了正确的业务码（INVALID_CREDENTIALS / USER_BANNED），原样上抛
		return nil, err
	}

	u, err := s.users.GetByUsername(ctx, username)
	if err != nil {
		// 走到这里说明「刚验通过的用户在库里查不到」—— 只可能是并发删除或者数据不一致。
		// 对用户仍然表现为凭证无效（不泄漏内部状态），但日志里要能看出这是异常路径。
		s.logger.ErrorContext(ctx, "auth.login.user_vanished",
			slog.String("username", username), slog.String("err", err.Error()))
		if apperr.IsCode(err, apperr.CodeNotFound) {
			return nil, apperr.WrapMsg(err, apperr.CodeInvalidCredentials, "用户名或密码错误")
		}
		return nil, err
	}

	tok, err := s.signer.Issue(u.ID)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("service.Auth.Login: %w", err))
	}

	s.logger.InfoContext(ctx, "auth.login",
		slog.Int64("user_id", u.ID),
		slog.String("auth_source", u.AuthSource))

	return &LoginResult{
		Token: tok.Value,
		// 和 UserView.CreatedAt 一样显式格式化成 RFC3339 字符串，
		// 让前端拿到的是一个确定的形状，而不是带时区偏移的 time.Time 序列化结果。
		ExpiresAt: tok.ExpiresAt.UTC().Format(time.RFC3339),
		User:      u.View(),
	}, nil
}

// Me 对应 #3 GET /api/auth/me。
func (s *Auth) Me(ctx context.Context, userID int64) (*model.UserView, error) {
	u, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	v := u.View()
	return &v, nil
}

// UpdateProfileInput 是 #4 PUT /api/auth/me 的请求体。
//
// 三个字段全是指针，因为要区分三种状态：
//
//	nil      —— 请求里没这个字段，保持原值
//	指向""   —— 明确要求清空（phone/email 才有这个语义）
//	指向"值" —— 改成这个值
//
// 用普通 string 的话前两种就分不开了，而「我想删掉我填错的手机号」是真实需求。
// 这也是 repo.UpdateProfile 用无条件覆写、不用 COALESCE 的原因（见那里的注释）。
type UpdateProfileInput struct {
	Nickname *string
	Phone    *string
	Email    *string
}

// UpdateProfile 改自己的资料，返回改完后的完整形状（同 #3）。
func (s *Auth) UpdateProfile(ctx context.Context, userID int64, in UpdateProfileInput) (*model.UserView, error) {
	cur, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return nil, err
	}

	// 合并：nil 表示「不动」，所以拿当前值顶上。合并逻辑放在这里（Go 代码）
	// 而不是 SQL 的 COALESCE，因为只有 Go 这边分得清 nil 和指向空串的指针。
	nickname := cur.Nickname
	if in.Nickname != nil {
		nickname = strings.TrimSpace(*in.Nickname)
		// 昵称不能清空。理由同 Register：这个系统所有交互都靠显示名认人，
		// 空昵称会让通知、归还确认、解锁名单里出现一片空白。
		// 想改就改成别的，想隐藏就用一个不含真名的字符串。
		if nickname == "" {
			return nil, apperr.Validation("昵称不能为空",
				apperr.FieldError{Field: "nickname", Msg: "昵称不能为空"})
		}
		if err := validateLength("nickname", nickname, nicknameMaxChars); err != nil {
			return nil, err
		}
	}

	// phone / email：空串表示清空，转成 nil 写进库（这两列可空）。
	phone := cur.Phone
	if in.Phone != nil {
		phone = emptyToNil(strings.TrimSpace(*in.Phone))
		if err := validateOptionalLength("phone", phone, phoneMaxChars); err != nil {
			return nil, err
		}
	}

	email := cur.Email
	if in.Email != nil {
		email = emptyToNil(strings.TrimSpace(*in.Email))
		if err := validateOptionalLength("email", email, emailMaxChars); err != nil {
			return nil, err
		}
	}
	// ⚠ 刻意**不校验** email 格式和 phone 格式。
	//
	// 这不是漏了：这个系统从头到尾没有任何发邮件、发短信的功能
	// （定位原则 2「平台不做站内私信」，通知全部是站内 notifications 表），
	// 所以这两列纯粹是用户自报的展示信息，没有任何代码依赖它格式正确。
	// 校验一个没人消费的东西，只会让「我就想填个备注」的用户被挡住。
	// 和 §3.2 对 contact 的处理是同一个判断：平台不为用户自己填的信息背书。

	u, err := s.users.UpdateProfile(ctx, userID, nickname, phone, email)
	if err != nil {
		return nil, err
	}
	v := u.View()
	return &v, nil
}

// ChangePasswordInput 是 #5 POST /api/auth/change-password 的请求体。
type ChangePasswordInput struct {
	OldPassword string
	NewPassword string
}

// ChangePassword 改密码。仅本地用户可用（§4 #5「JWT（仅 local 用户）」）。
//
// ⚠ 已知限制，实现时确认过、当前**故意不解决**：
// 改密码不会让已经签出去的旧 token 失效。JWT 是无状态的，签出去只能等它过期
// （默认 24 小时），所以「密码泄漏了赶紧改」这个动作并不能立刻把攻击者踢出去。
// 要真正做到需要给 users 加一列 password_changed_at，让中间件比对 token 的 iat ——
// 那是一次 schema 变更，计划 §3.7 里没有这一列，所以留到需要时再加迁移。
// 记在这里，免得将来有人以为「改密码就安全了」。
func (s *Auth) ChangePassword(ctx context.Context, userID int64, in ChangePasswordInput) error {
	u, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return err
	}

	// SSO 用户没有密码可改：他的密码在杭电助手那边，我们既没有也不该有。
	// 返回 FORBIDDEN 而不是 VALIDATION —— 这不是「你填错了」，是「这个动作对你不成立」。
	if !u.IsLocal() {
		return apperr.Forbidden("该账号通过杭电助手登录，没有本地密码可修改")
	}
	if u.PasswordHash == nil || *u.PasswordHash == "" {
		// auth_source='local' 但哈希为空：users_auth_local 约束本该拦住这种行，
		// 出现它说明有人从 Adminer 里手改过库（§3.4 明确允许 admin 这么干）。
		// 当成 INTERNAL 记日志，不要让用户看到一个莫名其妙的「旧密码错误」。
		s.logger.ErrorContext(ctx, "auth.change_password.missing_hash", slog.Int64("user_id", userID))
		return apperr.Internal(fmt.Errorf("service.Auth.ChangePassword: 本地用户 %d 没有 password_hash", userID))
	}

	if !auth.CheckPassword(*u.PasswordHash, in.OldPassword) {
		// 单独的码（§8）：这个错要提示用户「旧密码不对」，
		// 和注册时的 WEAK_PASSWORD 是完全不同的处理方式，不能合并成 VALIDATION。
		return apperr.NewMsg(apperr.CodeOldPasswordWrong, "原密码不正确").
			WithField("old_password", "原密码不正确")
	}

	if err := auth.ValidatePassword(in.NewPassword); err != nil {
		return err
	}

	hash, err := auth.HashPassword(in.NewPassword)
	if err != nil {
		return apperr.Internal(fmt.Errorf("service.Auth.ChangePassword: %w", err))
	}
	if err := s.users.UpdatePasswordHash(ctx, userID, hash); err != nil {
		return err
	}

	// 只记「谁改了密码」，不记新旧密码的任何形式
	s.logger.InfoContext(ctx, "auth.change_password", slog.Int64("user_id", userID))
	return nil
}

// ---------- 校验辅助 ----------

func validateUsername(username string) error {
	if username == "" {
		return apperr.Validation("请填写用户名",
			apperr.FieldError{Field: "username", Msg: "用户名不能为空"})
	}
	n := utf8.RuneCountInString(username)
	if n < usernameMinChars || n > usernameMaxChars {
		return apperr.Validation(
			fmt.Sprintf("用户名长度必须是 %d–%d 个字符，当前 %d 个", usernameMinChars, usernameMaxChars, n),
			apperr.FieldError{Field: "username", Msg: fmt.Sprintf("%d–%d 个字符", usernameMinChars, usernameMaxChars)})
	}
	return nil
}

// validateLength 校验必填字段的字符数上限。
func validateLength(field, value string, max int) error {
	if n := utf8.RuneCountInString(value); n > max {
		return apperr.Validation(
			fmt.Sprintf("%s 最长 %d 个字符，当前 %d 个", field, max, n),
			apperr.FieldError{Field: field, Msg: fmt.Sprintf("最长 %d 个字符", max)})
	}
	return nil
}

// validateOptionalLength 同 validateLength，但 nil（表示清空）直接放过。
func validateOptionalLength(field string, value *string, max int) error {
	if value == nil {
		return nil
	}
	return validateLength(field, *value, max)
}

// emptyToNil 把空串折成 nil，好让可空列真的写成 NULL 而不是空串。
// 差别不是审美问题：WHERE email = ” 和 WHERE email IS NULL 是两条不同的查询，
// 混着存会让将来的统计和去重都得出错误结果。
func emptyToNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

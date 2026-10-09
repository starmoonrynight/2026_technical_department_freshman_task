package auth

import (
	"context"
	"fmt"
	"log/slog"

	"golang.org/x/crypto/bcrypt"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
)

// UserLookup 是 LocalProvider 需要的**全部**数据库能力：按用户名取一行。
//
// 定成这个窄接口而不是直接依赖 *repo.User，有两个具体好处：
//  1. 计划 §10 要求 auth 的单测不碰数据库。有这个接口，测试里塞一个内存 map
//     实现就行，不用起 PG、不用 migrate、不用 truncate。
//  2. 它把「auth 到底用了 repo 的哪些方法」写在类型上。将来给 repo.User 加十个
//     方法，auth 能碰到的仍然只有这一个 —— 依赖面是被声明出来的，不是靠自觉。
//
// *repo.User 天然满足它，不需要任何适配代码。
type UserLookup interface {
	GetByUsername(ctx context.Context, username string) (*model.User, error)
}

// dummyHash 是一枚**永远不可能被匹配上**的 bcrypt 哈希，唯一用途是烧掉同样的 CPU 时间。
//
// 为什么需要它：bcrypt 在 cost=10 时大约要几十毫秒，而「用户名不存在」这条路
// 一次哈希都不做，几乎是瞬时返回。这个时间差远大于网络抖动，任何人写个脚本
// 挨个试用户名、量响应耗时，就能把全部注册用户名爬出来 —— 而 §8 明确要求
// INVALID_CREDENTIALS「不区分哪个错，防账号枚举」。只做文案上的合并是不够的，
// 计时信道会把同一个信息再泄漏一遍。
//
// 所以用户名不存在时也走一次完整的 bcrypt 比对，让两条路的耗时无法区分。
// 哈希的原文是下面这个写死的字符串，它不是任何人的密码，也不需要保密
// （bcrypt 哈希本来就假定可以公开）。
var dummyHash = mustHashDummy()

func mustHashDummy() string {
	h, err := bcrypt.GenerateFromPassword([]byte("lostfound-timing-equalization-dummy"), bcrypt.DefaultCost)
	if err != nil {
		// 输入 34 字节、cost 合法，bcrypt 在这里没有失败路径。
		// 真失败了说明库或运行环境出了问题，此时继续跑等于悄悄关掉防枚举保护，
		// 所以宁可启动就崩，也不要带着一个哑掉的防护上线。
		panic(fmt.Sprintf("auth: 无法生成计时均衡用的 dummy bcrypt 哈希: %v", err))
	}
	return string(h)
}

// LocalProvider 是本地账号密码认证。计划 §7.1：这是**永久必备**，不是过渡方案。
type LocalProvider struct {
	users  UserLookup
	logger *slog.Logger
}

// NewLocalProvider 组装一个本地认证器。logger 传 nil 时用 slog.Default()。
func NewLocalProvider(users UserLookup, logger *slog.Logger) LocalProvider {
	if logger == nil {
		logger = slog.Default()
	}
	return LocalProvider{users: users, logger: logger}
}

// Authenticate 用用户名 + 密码确认身份。
//
// 失败只有两种业务码，且顺序是有讲究的：
//
//	INVALID_CREDENTIALS —— 用户名不存在、是 SSO 账号没有密码、或密码不对（三者不区分）
//	USER_BANNED         —— 密码**对了**，但账号被封禁
//
// ⚠ 必须先验密码、再判封禁。反过来的话，任何人拿一个错误密码去试某个用户名，
// 就能通过「返回 USER_BANNED 还是 INVALID_CREDENTIALS」探出这个账号是否存在、
// 是否被封 —— 封禁状态是治理信息，不该向匿名用户透露。
// 现在的顺序下，只有真正持有密码的人才会被告知账号被封，这是合理的：
// 他需要知道为什么登不上，否则只会反复重试。
func (p LocalProvider) Authenticate(ctx context.Context, cred Credentials) (*Identity, error) {
	const wrongCredentials = "用户名或密码错误"

	u, err := p.users.GetByUsername(ctx, cred.Username)
	if err != nil {
		if apperr.IsCode(err, apperr.CodeNotFound) {
			// 烧掉和「密码比对失败」相同的时间，见 dummyHash 注释
			_ = bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(cred.Password))
			// 记一条和 auth.local.bad_password **对称**的日志：同样只有用户名，
			// 同样 WARN 级别。这样排查「用户说登不上」时两种情况都能 grep 到，
			// 而不是一种有日志、另一种只能靠猜。
			p.logger.WarnContext(ctx, "auth.local.no_such_user",
				slog.String("username", cred.Username))
			// ⚠ 用 NewMsg 而不是 WrapMsg：WrapMsg 会把 repo 那个 NOT_FOUND 挂进
			// apperr.Error.Err，而 AccessLog 会把它打进 http.request 那行的 err 字段。
			// 后果是「用户名不存在」的 401 在日志里多出一段
			// "NOT_FOUND: 用户不存在: no rows in result set"，
			// 而「密码不对」的 401 没有 —— 两条本来必须同形的记录长得不一样了。
			// 响应体是同一个 code 同一句 message（smoketest 里有断言），
			// 但日志行会被贴进 issue、截图、群里，那条差异就白藏了。
			// 这和本函数下面那句「不记是用户名错还是密码错」是同一条纪律。
			return nil, apperr.NewMsg(apperr.CodeInvalidCredentials, wrongCredentials)
		}
		// 数据库故障不能伪装成「密码错」：那会把一次宕机变成满屏的 401，
		// 排查时会一路往「是不是密码逻辑坏了」的方向查，而真正的原因是连接断了。
		return nil, err
	}

	// SSO 用户的 password_hash 是 NULL（users_auth_local 约束只要求本地账号非空）。
	// 拿 dummyHash 去比而不是直接返回失败，同样是为了不留计时差 ——
	// 否则「这个用户名是 SSO 账号」这件事又能被测出来。
	hash := dummyHash
	if u.PasswordHash != nil && *u.PasswordHash != "" {
		hash = *u.PasswordHash
	}

	if !CheckPassword(hash, cred.Password) {
		// 记一条日志但不记密码、也不记「是用户名错还是密码错」——
		// 前者是明文敏感信息，后者会让日志变成账号枚举的现成数据源。
		// 只记用户名和结果，够排查「用户说登不上」这一类问题了。
		p.logger.WarnContext(ctx, "auth.local.bad_password",
			slog.String("username", cred.Username),
			slog.Int64("user_id", u.ID))
		return nil, apperr.NewMsg(apperr.CodeInvalidCredentials, wrongCredentials)
	}

	if u.IsBanned() {
		p.logger.WarnContext(ctx, "auth.local.banned",
			slog.String("username", cred.Username),
			slog.Int64("user_id", u.ID))
		return nil, apperr.NewMsg(apperr.CodeUserBanned, "账号已被封禁，请联系管理员")
	}

	// Name 放用户名：本地账号没有真实姓名（real_name 是 SSO 才会填的列）。
	// M8 的 HDUHelpProvider 会在这里放杭电助手返回的真实姓名。
	name := cred.Username
	if u.Nickname != "" {
		name = u.Nickname
	}

	return &Identity{
		ExternalID: "", // 本地登录没有外部标识，users.sso_user_id 保持 NULL
		Name:       name,
		AvatarURL:  derefStr(u.AvatarURL),
		Source:     SourceLocal,
	}, nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

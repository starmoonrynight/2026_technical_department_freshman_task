package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
)

// fakeUserLookup 是 UserLookup 的内存实现。
//
// 它的存在就是 UserLookup 被定成窄接口的全部理由：计划 §10 要求 auth 的单测
// **不碰数据库**，有了这个 fake，下面十几个用例跑起来是毫秒级的，
// 不需要起 PG、不需要 migrate、不需要在每个用例前 truncate。
// 对比 smoketest/ 里的集成测试（每个都要建库跑迁移），这个差距决定了
// 「改一行代码敢不敢马上跑一遍测试」。
type fakeUserLookup struct {
	byUsername map[string]*model.User
	// err 非 nil 时所有查询都返回它，用来模拟「数据库挂了」而不是「用户不存在」
	err   error
	calls int
}

func (f *fakeUserLookup) GetByUsername(_ context.Context, username string) (*model.User, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	u, ok := f.byUsername[username]
	if !ok {
		// 和真 repo 保持一致：查无此人返回 apperr NOT_FOUND
		return nil, apperr.WrapMsg(errNoRow, apperr.CodeNotFound, "用户不存在")
	}
	return u, nil
}

// errNoRow 顶替 pgx.ErrNoRows，这样这个测试文件不必 import pgx。
var errNoRow = errors.New("no rows in result set")

const testPassword = "correct-horse-battery"

// newLocalUser 造一个正常的本地账号。哈希只算一次，供多个用例复用 ——
// bcrypt 故意做得慢（cost 10 大约几十毫秒），每个用例都重算会让这个
// 本该毫秒级的单元测试变成一秒多，而慢的测试就是不会被跑的测试。
func newLocalUser(t *testing.T, id int64, username, password string) *model.User {
	t.Helper()
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword 失败: %v", err)
	}
	return &model.User{
		ID:           id,
		Username:     &username,
		PasswordHash: &hash,
		AuthSource:   model.AuthSourceLocal,
		Nickname:     username + "的昵称",
		Role:         model.RoleUser,
		Status:       model.UserStatusActive,
		CreditScore:  100,
	}
}

func TestLocalProviderAuthenticate(t *testing.T) {
	ctx := context.Background()

	active := newLocalUser(t, 1, "alice", testPassword)
	banned := newLocalUser(t, 2, "bob", testPassword)
	banned.Status = model.UserStatusBanned

	// SSO 用户：password_hash 是 NULL（000001 迁移里的 users_auth_local 约束
	// 只要求 auth_source='local' 的行非空，hduhelp 的行这两列都是 NULL）
	ssoName := "carol"
	ssoID := "hduhelp-userid-9527"
	sso := &model.User{
		ID:         3,
		Username:   nil,
		AuthSource: model.AuthSourceHDUHelp,
		SSOUserID:  &ssoID,
		RealName:   &ssoName,
		Nickname:   "卡罗尔",
		Role:       model.RoleUser,
		Status:     model.UserStatusActive,
	}

	lookup := &fakeUserLookup{byUsername: map[string]*model.User{
		"alice": active,
		"bob":   banned,
		"carol": sso,
	}}
	p := NewLocalProvider(lookup, nil)

	t.Run("正确的用户名和密码", func(t *testing.T) {
		id, err := p.Authenticate(ctx, Credentials{Username: "alice", Password: testPassword})
		if err != nil {
			t.Fatalf("应该认证成功: %v", err)
		}
		if id == nil {
			t.Fatal("契约规定成功时 Identity 不能是 nil")
		}
		if id.Source != SourceLocal {
			t.Errorf("Source 应该是 local，实际 %q", id.Source)
		}
		// 本地登录没有外部标识，users.sso_user_id 必须保持 NULL
		if id.ExternalID != "" {
			t.Errorf("本地登录的 ExternalID 应该是空串，实际 %q", id.ExternalID)
		}
		if id.Name == "" {
			t.Error("Name 不该为空，前端要拿它当显示名")
		}
	})

	t.Run("密码错", func(t *testing.T) {
		_, err := p.Authenticate(ctx, Credentials{Username: "alice", Password: "wrong-password"})
		assertCode(t, err, apperr.CodeInvalidCredentials)
	})

	t.Run("用户名不存在", func(t *testing.T) {
		_, err := p.Authenticate(ctx, Credentials{Username: "nobody", Password: testPassword})
		// ⚠ 必须是 INVALID_CREDENTIALS，不能是 NOT_FOUND。
		// 返回 NOT_FOUND 等于开了一个账号枚举接口：挨个试用户名，
		// 看谁 404 谁 401，就能把全部注册用户名爬出来（§8 明确要求不区分）。
		assertCode(t, err, apperr.CodeInvalidCredentials)
	})

	// 上面那条只管住了**响应体**。这一条管住另一半：访问日志。
	//
	// apperr.Fail 只在 e.Err != nil 时把原始错误交给 AccessLog，AccessLog 再把它
	// 打进 http.request 那行的 err 字段。所以「用户名不存在」这条路如果用 WrapMsg
	// 把 repo 的 NOT_FOUND 挂上去，日志里就会多出一段
	//     "err":"NOT_FOUND: 用户不存在: no rows in result set"
	// 而「密码不对」那条没有 —— 两条本该同形的记录在日志里变得可区分了。
	// 日志行会被贴进 issue、截图、群里，那条差异就白藏了。
	t.Run("两种凭证错误的日志形状也必须相同", func(t *testing.T) {
		_, unknown := p.Authenticate(ctx, Credentials{Username: "nobody", Password: testPassword})
		_, wrongPw := p.Authenticate(ctx, Credentials{Username: "alice", Password: "wrong-password"})

		for name, err := range map[string]error{"用户名不存在": unknown, "密码不对": wrongPw} {
			var ae *apperr.Error
			if !errors.As(err, &ae) {
				t.Fatalf("%s: 返回的不是 *apperr.Error: %v", name, err)
			}
			if ae.Err != nil {
				t.Errorf("%s: 错误里挂着内部原因 %v —— AccessLog 会把它打进日志，破坏同形性", name, ae.Err)
			}
		}
		// 最直接的一条：两个错误的文本必须逐字相同（响应 message 也来自它）
		if unknown.Error() != wrongPw.Error() {
			t.Errorf("两种凭证错误的文本不同：\n  %q\n  %q", unknown.Error(), wrongPw.Error())
		}
	})

	t.Run("密码为空", func(t *testing.T) {
		_, err := p.Authenticate(ctx, Credentials{Username: "alice", Password: ""})
		assertCode(t, err, apperr.CodeInvalidCredentials)
	})

	t.Run("SSO 账号不能用密码登录", func(t *testing.T) {
		// carol 没有密码，任何输入都不该让她登进来
		_, err := p.Authenticate(ctx, Credentials{Username: "carol", Password: testPassword})
		assertCode(t, err, apperr.CodeInvalidCredentials)
		_, err = p.Authenticate(ctx, Credentials{Username: "carol", Password: ""})
		assertCode(t, err, apperr.CodeInvalidCredentials)
	})

	t.Run("封禁用户 + 正确密码 → USER_BANNED", func(t *testing.T) {
		_, err := p.Authenticate(ctx, Credentials{Username: "bob", Password: testPassword})
		assertCode(t, err, apperr.CodeUserBanned)
	})

	// 这条是上面那条的**顺序**保护，也是最容易写反的一处。
	// 如果实现成「先查 status 再比密码」，那么任何匿名用户拿一个错密码去试 bob，
	// 就能通过返回 USER_BANNED 还是 INVALID_CREDENTIALS 探出「bob 存在且被封了」。
	// 封禁是治理信息，只该告诉证明了账号所有权的人。
	t.Run("封禁用户 + 错误密码 → 仍然是 INVALID_CREDENTIALS", func(t *testing.T) {
		_, err := p.Authenticate(ctx, Credentials{Username: "bob", Password: "wrong-password"})
		assertCode(t, err, apperr.CodeInvalidCredentials)
	})

	t.Run("数据库故障不能被伪装成密码错", func(t *testing.T) {
		dbDown := errors.New("dial tcp 127.0.0.1:5432: connectex: No connection could be made")
		broken := NewLocalProvider(&fakeUserLookup{err: dbDown}, nil)

		_, err := broken.Authenticate(ctx, Credentials{Username: "alice", Password: testPassword})
		if err == nil {
			t.Fatal("数据库故障必须返回错误")
		}
		// 如果这里把故障翻译成 INVALID_CREDENTIALS，一次宕机就会表现成
		// 「所有用户都密码错误」，排查时会一路去查密码逻辑和 bcrypt，
		// 而真正的原因（连接断了）被自己的代码藏起来了。
		if apperr.IsCode(err, apperr.CodeInvalidCredentials) {
			t.Error("数据库故障不该被翻译成 INVALID_CREDENTIALS，那会掩盖真实原因")
		}
		if !errors.Is(err, dbDown) {
			t.Errorf("原始错误应该能用 errors.Is 检出，实际 %v", err)
		}
	})

	t.Run("成功和失败都不返回 (nil, nil)", func(t *testing.T) {
		// §7.3 的接口契约：失败返回 apperr，**绝不返回 (nil, nil)**。
		// (nil, nil) 会让调用方在解引用 Identity 时 panic，
		// 而且 panic 发生在「认证失败」这条最常见的路径上。
		id, err := p.Authenticate(ctx, Credentials{Username: "alice", Password: "nope"})
		if err == nil && id == nil {
			t.Error("返回了 (nil, nil)，违反 Provider 契约")
		}
	})
}

// TestLocalProviderTimingEqualization 验证「用户名不存在」这条路也做了一次 bcrypt。
//
// 这是防账号枚举的第二半。文案上把两种失败合并成 INVALID_CREDENTIALS 只堵住了
// 响应体这条泄漏路径；如果「用户名不存在」瞬时返回而「密码错」要花几十毫秒，
// 攻击者量响应耗时就能把用户名全爬出来 —— 计时信道泄漏的是同一个信息。
//
// 所以这里断言两种失败的耗时是同一个量级。用比值而不是绝对差值做判据：
// 绝对时间受机器负载影响太大，写成「差值 < 5ms」会在 CI 上偶发失败。
func TestLocalProviderTimingEqualization(t *testing.T) {
	if testing.Short() {
		t.Skip("计时测试在 -short 模式下跳过")
	}

	ctx := context.Background()
	known := newLocalUser(t, 1, "alice", testPassword)
	p := NewLocalProvider(&fakeUserLookup{
		byUsername: map[string]*model.User{"alice": known},
	}, nil)

	const rounds = 5

	var unknownTotal, wrongPwTotal int64
	for i := 0; i < rounds; i++ {
		unknownTotal += timeSince(t, func() {
			_, _ = p.Authenticate(ctx, Credentials{Username: "definitely-not-registered", Password: testPassword})
		})
		wrongPwTotal += timeSince(t, func() {
			_, _ = p.Authenticate(ctx, Credentials{Username: "alice", Password: "wrong-password"})
		})
	}

	unknownAvg := unknownTotal / rounds
	wrongPwAvg := wrongPwTotal / rounds

	// 没做计时均衡时，unknownAvg 会接近 0 而 wrongPwAvg 是几十毫秒，比值轻松超过 100 倍。
	// 阈值取 3 倍足够宽（不会因负载抖动误报），又足够严（能抓住「忘了均衡」这个真实回归）。
	if unknownAvg*3 < wrongPwAvg {
		t.Errorf("「用户名不存在」明显比「密码错」快得多（%dns vs %dns）—— "+
			"计时信道会泄漏用户名是否存在，检查是否漏了 dummyHash 比对",
			unknownAvg, wrongPwAvg)
	}
	t.Logf("计时均衡：用户名不存在 %dns / 密码错 %dns（同一量级，无法区分）", unknownAvg, wrongPwAvg)
}

// timeSince 跑一次 fn 并返回耗时（纳秒）。
func timeSince(t *testing.T, fn func()) int64 {
	t.Helper()
	start := time.Now()
	fn()
	return time.Since(start).Nanoseconds()
}

// assertCode 断言 err 是某个 apperr 码，失败时打印完整错误方便定位。
func assertCode(t *testing.T, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatalf("应该返回 %s，但 err 是 nil", wantCode)
	}
	if !apperr.IsCode(err, wantCode) {
		t.Errorf("错误码不对：期望 %s，实际 %v", wantCode, err)
	}
}

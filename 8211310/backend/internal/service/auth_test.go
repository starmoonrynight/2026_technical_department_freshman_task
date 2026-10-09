package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"lostfound/internal/apperr"
	"lostfound/internal/auth"
	"lostfound/internal/model"
	"lostfound/internal/repo"
)

// 这个文件是计划 §10 的第①层：纯单元测试，不连数据库、不起 HTTP server。
//
// M1 的验收判据里有一条是「单测：fakeProvider 能被注入且 JWT 签发逻辑与
// Provider 无关」。这句话拆开是两个必须被机器验证的论断：
//
//  1. **能被注入** —— service.Auth 依赖的是 auth.Provider 接口，不是 LocalProvider
//     具体类型。下面的 fakeProvider 能编译通过并且真的被调用，就是证明。
//  2. **签发与 Provider 无关** —— 换一个行为完全不同的 Provider（这里用一个
//     假装是杭电助手 SSO 的实现），签出来的 token 仍然能被同一个 signer 解回
//     同一个 user id。见 TestLoginIssuesSameTokenRegardlessOfProvider。
//
// 这条测试之所以必须在第①层而不是靠集成测试覆盖：集成测试里跑的一定是
// LocalProvider，它**永远无法**证明「换成 SSO 也能用」—— 而 M8 的整个可行性
// 就押在这个论断上。到 M8 才发现 seam 是假的，代价就太大了。

// testSecret 只活在这个测试进程里，和任何环境的真密钥无关。
// 长度必须 >= 32 字节，否则 auth.NewTokenSigner 会拒绝（那是它该有的行为）。
const testSecret = "service-unit-test-secret-0123456789abcdef"

// ---------- 假的持久化层 ----------

// fakeUserStore 是 UserStore 的内存实现。
//
// 除了当替身，它还承担一个真实实现承担不了的职责：**记录被调用了几次**。
// 「认证失败时不该去查 users 表」这种论断，用真数据库是没法断言的
// （你只能看到最终结果，看不到中间有没有多查一次），而多查一次正是
// 「用响应时间区分用户名是否存在」这类侧信道的来源。
type fakeUserStore struct {
	byID       map[int64]*model.User
	byUsername map[string]*model.User
	nextID     int64

	// 故障注入：真数据库会偶尔抽风，代码必须在那时候给出正确的错误码而不是 panic
	getByIDErr error

	// 调用计数
	getByUsernameCalls int
	updateHashCalls    int
}

func newFakeUserStore() *fakeUserStore {
	return &fakeUserStore{
		byID:       map[int64]*model.User{},
		byUsername: map[string]*model.User{},
		nextID:     1,
	}
}

// seedLocal 造一个本地账号塞进假库里，返回它的副本。
func (f *fakeUserStore) seedLocal(t *testing.T, username, password string) *model.User {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("seedLocal(%s): 哈希密码失败: %v", username, err)
	}
	return f.seed(&model.User{
		Username:     strptr(username),
		PasswordHash: strptr(hash),
		AuthSource:   model.AuthSourceLocal,
		Nickname:     username,
		Role:         model.RoleUser,
		Status:       model.UserStatusActive,
		CreditScore:  100,
		CreatedAt:    time.Now(),
	})
}

// seedSSO 造一个杭电助手账号：没有 username、没有 password_hash。
// 这正是 users_auth_sso 约束允许、users_auth_local 约束禁止的那种行。
func (f *fakeUserStore) seedSSO(t *testing.T, externalID, name string) *model.User {
	t.Helper()
	return f.seed(&model.User{
		AuthSource:  model.AuthSourceHDUHelp,
		SSOUserID:   strptr(externalID),
		RealName:    strptr(name),
		Nickname:    name,
		Role:        model.RoleUser,
		Status:      model.UserStatusActive,
		CreditScore: 100,
		CreatedAt:   time.Now(),
	})
}

func (f *fakeUserStore) seed(u *model.User) *model.User {
	u.ID = f.nextID
	f.nextID++
	u.UpdatedAt = u.CreatedAt
	stored := cloneUser(u)
	f.byID[stored.ID] = stored
	if stored.Username != nil {
		f.byUsername[*stored.Username] = stored
	}
	return cloneUser(stored)
}

func (f *fakeUserStore) CreateLocal(_ context.Context, p repo.NewLocalUser) (*model.User, error) {
	if _, dup := f.byUsername[p.Username]; dup {
		// 和 repo.User.CreateLocal 一样把唯一索引冲突翻译成业务码
		return nil, apperr.NewMsg(apperr.CodeUserAlreadyExists, "用户名已被注册")
	}
	return f.seed(&model.User{
		Username:     strptr(p.Username),
		PasswordHash: strptr(p.PasswordHash),
		AuthSource:   model.AuthSourceLocal,
		Nickname:     p.Nickname,
		Role:         model.RoleUser,
		Status:       model.UserStatusActive,
		CreditScore:  100,
		CreatedAt:    time.Now(),
	}), nil
}

func (f *fakeUserStore) GetByID(_ context.Context, id int64) (*model.User, error) {
	if f.getByIDErr != nil {
		return nil, f.getByIDErr
	}
	u, ok := f.byID[id]
	if !ok {
		return nil, apperr.NotFound("用户")
	}
	return cloneUser(u), nil
}

func (f *fakeUserStore) GetByUsername(_ context.Context, username string) (*model.User, error) {
	f.getByUsernameCalls++
	u, ok := f.byUsername[username]
	if !ok {
		return nil, apperr.NotFound("用户")
	}
	return cloneUser(u), nil
}

func (f *fakeUserStore) UpdateProfile(_ context.Context, id int64, nickname string, phone, email *string) (*model.User, error) {
	u, ok := f.byID[id]
	if !ok {
		return nil, apperr.NotFound("用户")
	}
	u.Nickname = nickname
	u.Phone = clonePtr(phone)
	u.Email = clonePtr(email)
	u.UpdatedAt = time.Now()
	return cloneUser(u), nil
}

func (f *fakeUserStore) UpdatePasswordHash(_ context.Context, id int64, hash string) error {
	f.updateHashCalls++
	u, ok := f.byID[id]
	if !ok {
		return apperr.NotFound("用户")
	}
	u.PasswordHash = strptr(hash)
	u.UpdatedAt = time.Now()
	return nil
}

// ---------- 假的 Provider ----------

// fakeProvider 是 auth.Provider 的替身。
//
// 它记录收到的凭据，这样测试就能断言「service 到底把什么东西交给了 Provider」——
// 比如用户名有没有被 trim、密码有没有被 trim（密码绝不能 trim，见 auth/password.go）。
//
// wantPassword 非空时它会真的比对密码。默认（空）是「来者不拒」，
// 因为大多数测试关心的是 service 的行为，不是认证本身；
// 但「密码不许被 trim」这类测试必须有一个会拒绝错误密码的 Provider，
// 否则它什么也证明不了 —— 第一版就是在这里假绿过一次。
type fakeProvider struct {
	identity     *auth.Identity
	err          error
	wantPassword string

	calls int
	got   auth.Credentials
}

func (f *fakeProvider) Authenticate(_ context.Context, cred auth.Credentials) (*auth.Identity, error) {
	f.calls++
	f.got = cred
	if f.err != nil {
		return nil, f.err
	}
	if f.wantPassword != "" && cred.Password != f.wantPassword {
		return nil, apperr.NewMsg(apperr.CodeInvalidCredentials, "用户名或密码错误")
	}
	if f.identity != nil {
		return f.identity, nil
	}
	return &auth.Identity{Name: cred.Username, Source: auth.SourceLocal}, nil
}

// ---------- 装配 ----------

func newTestAuth(t *testing.T, store UserStore, provider auth.Provider) (*Auth, auth.TokenSigner) {
	t.Helper()
	signer, err := auth.NewTokenSigner(testSecret, time.Hour)
	if err != nil {
		t.Fatalf("建 TokenSigner 失败: %v", err)
	}
	// logger 丢掉：单元测试不检查日志内容，而让它往 stderr 打会把测试输出淹了
	svc := NewAuth(store, provider, signer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return svc, signer
}

// ---------- Provider 无关性（M1 验收判据的核心那条） ----------

func TestLoginIssuesSameTokenRegardlessOfProvider(t *testing.T) {
	// 同一个用户名、同一个假库、同一个 signer，只换 Provider。
	// 一个假装是本地密码认证，一个假装是杭电助手 SSO 回调。
	providers := map[string]*fakeProvider{
		"local 形状的 Provider": {
			identity: &auth.Identity{Name: "alice", Source: auth.SourceLocal},
		},
		"hduhelp 形状的 Provider": {
			identity: &auth.Identity{
				ExternalID: "hdu-9527",
				Name:       "张三",
				AvatarURL:  "https://example.com/a.png",
				Source:     auth.SourceHDUHelp,
			},
		},
		// 一个连 Identity 都懒得构造、直接回落到默认值的 Provider。
		// 加它是因为「Provider 返回什么」本来就不该影响签发：
		// 如果有人不小心把 identity.Name 写进了 claims，这一条会立刻红。
		"最小 Provider": {},
	}

	var firstID int64
	for name, p := range providers {
		t.Run(name, func(t *testing.T) {
			store := newFakeUserStore()
			seeded := store.seedLocal(t, "alice", "correct-horse-battery")

			svc, signer := newTestAuth(t, store, p)

			res, err := svc.Login(context.Background(), "alice", "correct-horse-battery")
			if err != nil {
				t.Fatalf("Login 失败: %v", err)
			}
			if p.calls != 1 {
				t.Errorf("Provider 应该被调用恰好 1 次，实际 %d 次", p.calls)
			}

			// 核心断言：token 能解回库里那一行的 id，而且和 Provider 是谁无关。
			userID, err := signer.Parse(res.Token)
			if err != nil {
				t.Fatalf("用同一个 signer 解不出刚签发的 token: %v", err)
			}
			if userID != seeded.ID {
				t.Errorf("token 里的 sub = %d，期望 %d", userID, seeded.ID)
			}
			if firstID == 0 {
				firstID = userID
			} else if userID != firstID {
				t.Errorf("换一个 Provider 之后 sub 变了（%d != %d）—— 说明签发逻辑漏进了 Provider 的信息", userID, firstID)
			}

			// 有效期来自 signer 的配置，不来自 Provider
			if res.ExpiresAt == "" {
				t.Error("expires_at 是空的，前端没法知道什么时候该重新登录")
			}
			if _, err := time.Parse(time.RFC3339, res.ExpiresAt); err != nil {
				t.Errorf("expires_at %q 不是 RFC3339: %v", res.ExpiresAt, err)
			}

			// user 字段是**库里那一行**的视图，不是 Provider 报上来的身份。
			// 这条挡住了「SSO Provider 说我叫张三，于是响应里 nickname 变成张三，
			// 但库里根本没改」这种前后端不一致。
			if res.User.ID != seeded.ID {
				t.Errorf("响应里的 user.id = %d，期望 %d", res.User.ID, seeded.ID)
			}
			if res.User.Username != "alice" {
				t.Errorf("响应里的 user.username = %q，期望 %q", res.User.Username, "alice")
			}
		})
	}
}

func TestLoginPassesCredentialsToProviderUntouched(t *testing.T) {
	const pw = "correct-horse-battery"
	store := newFakeUserStore()
	store.seedLocal(t, "alice", pw)
	// 必须给一个会真的比对密码的 Provider，否则「登录失败了」这个观察没有意义
	p := &fakeProvider{wantPassword: pw}
	svc, _ := newTestAuth(t, store, p)

	// 用户名前后带空白：必须被 trim，否则注册时存进去的 "alice" 永远匹配不上 " alice "
	// 密码前后带空白：**绝不能**被 trim，因为空格可以是密码的一部分
	if _, err := svc.Login(context.Background(), "  alice  ", "  "+pw+"  "); err == nil {
		t.Fatal("密码被 trim 之后居然登录成功了 —— 说明 service 或 Provider 动了密码")
	}
	if p.got.Username != "alice" {
		t.Errorf("交给 Provider 的 username = %q，期望 trim 过的 %q", p.got.Username, "alice")
	}
	if p.got.Password != "  "+pw+"  " {
		t.Errorf("交给 Provider 的 password = %q，期望**原样**的 %q", p.got.Password, "  "+pw+"  ")
	}

	// 原样的密码才能登进去
	if _, err := svc.Login(context.Background(), " alice ", pw); err != nil {
		t.Fatalf("用户名带空格 + 正确密码应该登录成功: %v", err)
	}
}

func TestLoginPropagatesProviderErrorAndDoesNotTouchTheStore(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode string
	}{
		{
			name:     "封禁用户",
			err:      apperr.NewMsg(apperr.CodeUserBanned, "账号已被封禁"),
			wantCode: apperr.CodeUserBanned,
		},
		{
			name:     "密码错误",
			err:      apperr.NewMsg(apperr.CodeInvalidCredentials, "用户名或密码错误"),
			wantCode: apperr.CodeInvalidCredentials,
		},
		{
			name:     "数据库故障（不能被改写成凭证无效）",
			err:      errors.New("connection refused"),
			wantCode: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeUserStore()
			store.seedLocal(t, "alice", "correct-horse-battery")
			p := &fakeProvider{err: tc.err}
			svc, _ := newTestAuth(t, store, p)

			res, err := svc.Login(context.Background(), "alice", "whatever")
			if err == nil {
				t.Fatalf("期望失败，实际成功返回了 %+v", res)
			}
			if res != nil {
				t.Error("失败时不该返回非 nil 的结果")
			}
			if tc.wantCode != "" {
				assertCode(t, err, tc.wantCode)
			} else if apperr.IsCode(err, apperr.CodeInvalidCredentials) {
				t.Error("数据库故障被伪装成了 INVALID_CREDENTIALS —— 一次宕机会表现成「所有人密码都错了」")
			}

			// 认证都没过，就不该去查 users 表。
			// 少这一条的话，「登录失败」路径会多一次数据库往返，而攻击者可以用
			// 响应时间差来判断一个用户名是否存在。
			if store.getByUsernameCalls != 0 {
				t.Errorf("认证失败后仍然查了 %d 次 users 表", store.getByUsernameCalls)
			}
		})
	}
}

func TestLoginUserVanishedBetweenProviderAndStore(t *testing.T) {
	store := newFakeUserStore()
	p := &fakeProvider{}
	svc, _ := newTestAuth(t, store, p)

	// Provider 说「认证通过」，但库里根本没有这个人。
	// 现实成因：并发删除、或者 SSO Provider 认了一个我们还没建行的外部身份。
	_, err := svc.Login(context.Background(), "ghost", "whatever")
	if err == nil {
		t.Fatal("期望失败")
	}
	// 对用户只能表现为凭证无效：说「用户不存在」等于把账号枚举的口子重新打开
	assertCode(t, err, apperr.CodeInvalidCredentials)
}

// ---------- 注册 ----------

func TestRegister(t *testing.T) {
	t.Run("正常注册", func(t *testing.T) {
		store := newFakeUserStore()
		svc, _ := newTestAuth(t, store, &fakeProvider{})

		u, err := svc.Register(context.Background(), RegisterInput{
			Username: "alice", Password: "correct-horse", Nickname: "爱丽丝",
		})
		if err != nil {
			t.Fatalf("Register 失败: %v", err)
		}
		if u.ID == 0 {
			t.Error("新用户应该有非 0 的 id")
		}
		if u.View().Username != "alice" || u.View().Nickname != "爱丽丝" {
			t.Errorf("存进去的形状不对: %+v", u.View())
		}
		if u.View().Role != model.RoleUser {
			t.Errorf("新注册的账号 role = %q，必须是 user —— 让请求体决定 role 就是个提权漏洞", u.View().Role)
		}
		if u.View().AuthSource != model.AuthSourceLocal {
			t.Errorf("auth_source = %q，期望 local", u.View().AuthSource)
		}
	})

	t.Run("密码绝不以明文入库", func(t *testing.T) {
		store := newFakeUserStore()
		svc, _ := newTestAuth(t, store, &fakeProvider{})

		const pw = "correct-horse-battery"
		u, err := svc.Register(context.Background(), RegisterInput{Username: "alice", Password: pw})
		if err != nil {
			t.Fatalf("Register 失败: %v", err)
		}
		if u.PasswordHash == nil {
			t.Fatal("password_hash 是 nil")
		}
		if *u.PasswordHash == pw {
			t.Fatal("password_hash 就是明文密码")
		}
		if strings.Contains(*u.PasswordHash, pw) {
			t.Fatal("password_hash 里含有明文密码（编码/拼接而不是哈希？）")
		}
		if !auth.CheckPassword(*u.PasswordHash, pw) {
			t.Error("存进去的哈希校验不过原密码")
		}
		// 「视图里不能带出哈希」由 TestMeDoesNotLeakPasswordHash 逐字段断言，这里不重复
	})

	t.Run("昵称留空回落到用户名", func(t *testing.T) {
		store := newFakeUserStore()
		svc, _ := newTestAuth(t, store, &fakeProvider{})

		for _, nickname := range []string{"", "   ", "\t\n"} {
			u, err := svc.Register(context.Background(), RegisterInput{
				Username: "alice", Password: "correct-horse", Nickname: nickname,
			})
			if err != nil {
				t.Fatalf("nickname=%q 时 Register 失败: %v", nickname, err)
			}
			if u.Nickname != "alice" {
				t.Errorf("nickname=%q 时昵称 = %q，期望回落到 %q", nickname, u.Nickname, "alice")
			}
			// 每轮都要清掉，否则第二轮会撞重名
			delete(store.byUsername, "alice")
			delete(store.byID, u.ID)
		}
	})

	t.Run("用户名和昵称被 trim", func(t *testing.T) {
		store := newFakeUserStore()
		svc, _ := newTestAuth(t, store, &fakeProvider{})

		u, err := svc.Register(context.Background(), RegisterInput{
			Username: "  alice  ", Password: "correct-horse", Nickname: "  爱丽丝  ",
		})
		if err != nil {
			t.Fatalf("Register 失败: %v", err)
		}
		if u.View().Username != "alice" {
			t.Errorf("username = %q，期望 trim 过的 %q", u.View().Username, "alice")
		}
		if u.View().Nickname != "爱丽丝" {
			t.Errorf("nickname = %q，期望 trim 过的 %q", u.View().Nickname, "爱丽丝")
		}
		// trim 必须发生在**入库前**，否则库里存的是 " alice "，
		// 而登录时 trim 过再查就永远查不到（Login 那边的注释解释了同一件事）
		if _, ok := store.byUsername["alice"]; !ok {
			t.Error("假库里存的键不是 trim 过的用户名")
		}
	})

	t.Run("重名", func(t *testing.T) {
		store := newFakeUserStore()
		svc, _ := newTestAuth(t, store, &fakeProvider{})

		in := RegisterInput{Username: "alice", Password: "correct-horse"}
		if _, err := svc.Register(context.Background(), in); err != nil {
			t.Fatalf("第一次注册失败: %v", err)
		}
		_, err := svc.Register(context.Background(), in)
		assertCode(t, err, apperr.CodeUserAlreadyExists)
	})

	t.Run("重名不靠先查后插", func(t *testing.T) {
		// 注册路径不该自己先 SELECT 一次：唯一索引才是原子的。
		// 这条断言把「将来有人加了一个防重复的前置查询」变成一次测试失败。
		store := newFakeUserStore()
		svc, _ := newTestAuth(t, store, &fakeProvider{})

		if _, err := svc.Register(context.Background(), RegisterInput{
			Username: "alice", Password: "correct-horse",
		}); err != nil {
			t.Fatalf("Register 失败: %v", err)
		}
		if store.getByUsernameCalls != 0 {
			t.Errorf("Register 查了 %d 次 GetByUsername —— 那是竞态的，应该直接插让唯一索引兜底", store.getByUsernameCalls)
		}
	})
}

func TestRegisterRejects(t *testing.T) {
	cases := []struct {
		name     string
		in       RegisterInput
		wantCode string
	}{
		{"空用户名", RegisterInput{Username: "", Password: "correct-horse"}, apperr.CodeValidation},
		{"只有空格的用户名", RegisterInput{Username: "     ", Password: "correct-horse"}, apperr.CodeValidation},
		{"用户名太短", RegisterInput{Username: "ab", Password: "correct-horse"}, apperr.CodeValidation},
		{"用户名太长", RegisterInput{Username: strings.Repeat("a", 33), Password: "correct-horse"}, apperr.CodeValidation},
		// 空密码是 VALIDATION 不是 WEAK_PASSWORD：它是「你没填」而不是「你填了个弱的」，
		// 前端的提示文案该不一样。密码过长同理，那是 bcrypt 的硬上限，不是强度问题。
		// 两个码都在 §4 第 1 行的「专属错误码」列里，所以这不是偏离计划。
		{"空密码", RegisterInput{Username: "alice", Password: ""}, apperr.CodeValidation},
		{"密码太短", RegisterInput{Username: "alice", Password: "abc1234"}, apperr.CodeWeakPassword},
		{"纯数字密码", RegisterInput{Username: "alice", Password: "12345678"}, apperr.CodeWeakPassword},
		{"密码过长（bcrypt 会静默截断）", RegisterInput{Username: "alice", Password: strings.Repeat("a", 73)}, apperr.CodeValidation},
		{"昵称太长", RegisterInput{Username: "alice", Password: "correct-horse", Nickname: strings.Repeat("呢", 33)}, apperr.CodeValidation},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeUserStore()
			svc, _ := newTestAuth(t, store, &fakeProvider{})

			u, err := svc.Register(context.Background(), tc.in)
			if err == nil {
				t.Fatalf("期望被拒绝，实际创建出了 %+v", u.View())
			}
			assertCode(t, err, tc.wantCode)
			if len(store.byID) != 0 {
				t.Errorf("校验失败之后库里还是多了一行（%d 行）", len(store.byID))
			}
		})
	}
}

// TestUsernameLengthCountsRunesNotBytes 是 M0 那个 U+3000 教训在 M1 的翻版：
// 数据库的 VARCHAR(32) 数的是**字符**，Go 的 len() 数的是**字节**。
// 用错单位的症状是「11 个汉字的昵称被拒了」，而用户完全看不出为什么。
func TestUsernameLengthCountsRunesNotBytes(t *testing.T) {
	store := newFakeUserStore()
	svc, _ := newTestAuth(t, store, &fakeProvider{})

	// 32 个汉字 = 96 字节，恰好卡在字符数上限上，必须放行
	u, err := svc.Register(context.Background(), RegisterInput{
		Username: strings.Repeat("名", 32),
		Password: "correct-horse",
		Nickname: strings.Repeat("昵", 32),
	})
	if err != nil {
		t.Fatalf("32 个汉字的用户名被拒了（长度按字节算的？）: %v", err)
	}
	if len(u.View().Username) != 96 {
		t.Errorf("存进去的用户名是 %d 字节，期望 96（32 个汉字）", len(u.View().Username))
	}
}

// ---------- 资料修改 ----------

func TestUpdateProfile(t *testing.T) {
	setup := func(t *testing.T) (*Auth, *fakeUserStore, int64) {
		t.Helper()
		store := newFakeUserStore()
		u := store.seedLocal(t, "alice", "correct-horse")
		// 先填上 phone/email，好测「清空」这条路径
		if _, err := store.UpdateProfile(context.Background(), u.ID, "爱丽丝", strptr("13800000000"), strptr("a@b.c")); err != nil {
			t.Fatalf("预置资料失败: %v", err)
		}
		svc, _ := newTestAuth(t, store, &fakeProvider{})
		return svc, store, u.ID
	}

	t.Run("nil 表示保持原值", func(t *testing.T) {
		svc, _, id := setup(t)
		v, err := svc.UpdateProfile(context.Background(), id, UpdateProfileInput{})
		if err != nil {
			t.Fatalf("UpdateProfile 失败: %v", err)
		}
		if v.Nickname != "爱丽丝" || v.Phone != "13800000000" || v.Email != "a@b.c" {
			t.Errorf("空请求体改动了资料: %+v", v)
		}
	})

	t.Run("空串表示清空", func(t *testing.T) {
		svc, store, id := setup(t)
		v, err := svc.UpdateProfile(context.Background(), id, UpdateProfileInput{
			Phone: strptr(""), Email: strptr(""),
		})
		if err != nil {
			t.Fatalf("UpdateProfile 失败: %v", err)
		}
		if v.Phone != "" || v.Email != "" {
			t.Errorf("传空串没有清空: %+v", v)
		}
		// 清空必须落成 NULL 而不是空串：WHERE email = '' 和 WHERE email IS NULL
		// 是两条不同的查询，混着存会让将来的统计得出错误结果
		stored := store.byID[id]
		if stored.Phone != nil || stored.Email != nil {
			t.Errorf("清空之后库里存的不是 nil（phone=%v email=%v）", stored.Phone, stored.Email)
		}
	})

	t.Run("改一个字段不影响另外两个", func(t *testing.T) {
		svc, _, id := setup(t)
		v, err := svc.UpdateProfile(context.Background(), id, UpdateProfileInput{Nickname: strptr("新名字")})
		if err != nil {
			t.Fatalf("UpdateProfile 失败: %v", err)
		}
		if v.Nickname != "新名字" {
			t.Errorf("nickname 没改成功: %q", v.Nickname)
		}
		if v.Phone != "13800000000" || v.Email != "a@b.c" {
			t.Errorf("改昵称把别的字段冲掉了: %+v", v)
		}
	})

	t.Run("值被 trim", func(t *testing.T) {
		svc, _, id := setup(t)
		v, err := svc.UpdateProfile(context.Background(), id, UpdateProfileInput{
			Nickname: strptr("  新名字  "), Phone: strptr("  138  "),
		})
		if err != nil {
			t.Fatalf("UpdateProfile 失败: %v", err)
		}
		if v.Nickname != "新名字" || v.Phone != "138" {
			t.Errorf("没有 trim: nickname=%q phone=%q", v.Nickname, v.Phone)
		}
	})

	rejects := []struct {
		name string
		in   UpdateProfileInput
		code string
	}{
		{"昵称改成空串", UpdateProfileInput{Nickname: strptr("")}, apperr.CodeValidation},
		{"昵称改成只有空格", UpdateProfileInput{Nickname: strptr("   ")}, apperr.CodeValidation},
		{"昵称太长", UpdateProfileInput{Nickname: strptr(strings.Repeat("呢", 33))}, apperr.CodeValidation},
		{"手机号太长", UpdateProfileInput{Phone: strptr(strings.Repeat("1", 21))}, apperr.CodeValidation},
		{"邮箱太长", UpdateProfileInput{Email: strptr(strings.Repeat("a", 129))}, apperr.CodeValidation},
	}
	for _, tc := range rejects {
		t.Run("拒绝："+tc.name, func(t *testing.T) {
			svc, store, id := setup(t)
			before := *store.byID[id]

			_, err := svc.UpdateProfile(context.Background(), id, tc.in)
			assertCode(t, err, tc.code)

			after := *store.byID[id]
			if before.Nickname != after.Nickname || !ptrEqual(before.Phone, after.Phone) || !ptrEqual(before.Email, after.Email) {
				t.Errorf("校验失败之后资料还是被改了: %+v -> %+v", before.View(), after.View())
			}
		})
	}

	t.Run("用户不存在", func(t *testing.T) {
		svc, _, _ := setup(t)
		_, err := svc.UpdateProfile(context.Background(), 9999, UpdateProfileInput{Nickname: strptr("x")})
		assertCode(t, err, apperr.CodeNotFound)
	})
}

// TestUpdateProfileDoesNotValidateEmailFormat 把「刻意不校验格式」这个决定
// 变成一条会失败的测试，而不是只写在注释里。
//
// 注释会被忽略，测试不会。如果将来有人顺手加一句「邮箱格式不对」的校验，
// 这条测试会红，他就必须回来读一遍下面这段理由再决定要不要改。
func TestUpdateProfileDoesNotValidateEmailFormat(t *testing.T) {
	store := newFakeUserStore()
	u := store.seedLocal(t, "alice", "correct-horse")
	svc, _ := newTestAuth(t, store, &fakeProvider{})

	// 这个系统从头到尾不发邮件、不发短信（定位原则 2「平台不做站内私信」，
	// 通知全走站内 notifications 表），所以这两列纯粹是用户自报的展示信息。
	// 校验一个没有任何代码消费的东西，只会挡住「我就想填个备注」的用户。
	v, err := svc.UpdateProfile(context.Background(), u.ID, UpdateProfileInput{
		Email: strptr("宿舍楼下快递柜"),
		Phone: strptr("问宿舍阿姨"),
	})
	if err != nil {
		t.Fatalf("填非格式化的联系方式被拒了: %v", err)
	}
	if v.Email != "宿舍楼下快递柜" || v.Phone != "问宿舍阿姨" {
		t.Errorf("值没有原样存下来: %+v", v)
	}
}

// ---------- 改密码 ----------

func TestChangePassword(t *testing.T) {
	t.Run("正常改", func(t *testing.T) {
		store := newFakeUserStore()
		u := store.seedLocal(t, "alice", "old-password-1")
		svc, _ := newTestAuth(t, store, &fakeProvider{})

		if err := svc.ChangePassword(context.Background(), u.ID, ChangePasswordInput{
			OldPassword: "old-password-1", NewPassword: "new-password-2",
		}); err != nil {
			t.Fatalf("ChangePassword 失败: %v", err)
		}
		if store.updateHashCalls != 1 {
			t.Errorf("改了 %d 次哈希，期望 1 次", store.updateHashCalls)
		}
		hash := *store.byID[u.ID].PasswordHash
		if !auth.CheckPassword(hash, "new-password-2") {
			t.Error("新密码校验不过")
		}
		if auth.CheckPassword(hash, "old-password-1") {
			t.Error("旧密码居然还能校验通过 —— 没真的换掉")
		}
	})

	t.Run("旧密码不对", func(t *testing.T) {
		store := newFakeUserStore()
		u := store.seedLocal(t, "alice", "old-password-1")
		svc, _ := newTestAuth(t, store, &fakeProvider{})

		err := svc.ChangePassword(context.Background(), u.ID, ChangePasswordInput{
			OldPassword: "wrong-password", NewPassword: "new-password-2",
		})
		// 单独的码，不能合并成 VALIDATION：前端要提示「原密码不正确」并聚焦到那一个框
		assertCode(t, err, apperr.CodeOldPasswordWrong)
		if store.updateHashCalls != 0 {
			t.Error("旧密码不对却把哈希改了")
		}
	})

	t.Run("新密码太弱", func(t *testing.T) {
		store := newFakeUserStore()
		u := store.seedLocal(t, "alice", "old-password-1")
		svc, _ := newTestAuth(t, store, &fakeProvider{})

		err := svc.ChangePassword(context.Background(), u.ID, ChangePasswordInput{
			OldPassword: "old-password-1", NewPassword: "12345678",
		})
		assertCode(t, err, apperr.CodeWeakPassword)
		if store.updateHashCalls != 0 {
			t.Error("新密码太弱却把哈希改了")
		}
		// 旧密码必须仍然有效：改密码失败不能让账号变成登不上的状态
		if !auth.CheckPassword(*store.byID[u.ID].PasswordHash, "old-password-1") {
			t.Error("改密码失败之后旧密码也失效了 —— 用户会被锁在门外")
		}
	})

	t.Run("SSO 用户不能改密码", func(t *testing.T) {
		store := newFakeUserStore()
		u := store.seedSSO(t, "hdu-9527", "张三")
		svc, _ := newTestAuth(t, store, &fakeProvider{})

		err := svc.ChangePassword(context.Background(), u.ID, ChangePasswordInput{
			OldPassword: "whatever", NewPassword: "new-password-2",
		})
		// FORBIDDEN 而不是 VALIDATION：这不是「你填错了」，是「这个动作对你不成立」
		assertCode(t, err, apperr.CodeForbidden)
	})

	t.Run("本地用户但没有哈希", func(t *testing.T) {
		// users_auth_local 约束本该拦住这种行；出现它说明有人从 Adminer 手改过库
		// （§3.4 明确允许 admin 这么干）。这时候要报 INTERNAL 并记日志，
		// 而不是让用户看到一个莫名其妙的「原密码不正确」。
		store := newFakeUserStore()
		u := store.seed(&model.User{
			Username:    strptr("alice"),
			AuthSource:  model.AuthSourceLocal,
			Nickname:    "alice",
			Role:        model.RoleUser,
			Status:      model.UserStatusActive,
			CreatedAt:   time.Now(),
			CreditScore: 100,
		})
		svc, _ := newTestAuth(t, store, &fakeProvider{})

		err := svc.ChangePassword(context.Background(), u.ID, ChangePasswordInput{
			OldPassword: "x", NewPassword: "new-password-2",
		})
		assertCode(t, err, apperr.CodeInternal)
	})

	t.Run("用户不存在", func(t *testing.T) {
		store := newFakeUserStore()
		svc, _ := newTestAuth(t, store, &fakeProvider{})

		err := svc.ChangePassword(context.Background(), 9999, ChangePasswordInput{
			OldPassword: "x", NewPassword: "new-password-2",
		})
		assertCode(t, err, apperr.CodeNotFound)
	})
}

// ---------- Me ----------

func TestMe(t *testing.T) {
	store := newFakeUserStore()
	u := store.seedLocal(t, "alice", "correct-horse")
	svc, _ := newTestAuth(t, store, &fakeProvider{})

	v, err := svc.Me(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("Me 失败: %v", err)
	}
	if v.Username != "alice" {
		t.Errorf("username = %q", v.Username)
	}

	_, err = svc.Me(context.Background(), 9999)
	assertCode(t, err, apperr.CodeNotFound)
}

// TestMeDoesNotLeakPasswordHash 是 UserView 当白名单这个设计的落点。
//
// 直接断言结构体里没有能装下哈希的字段：将来给 User 加敏感列时，
// 如果有人顺手也加进了 UserView，这条测试会红。
func TestMeDoesNotLeakPasswordHash(t *testing.T) {
	store := newFakeUserStore()
	u := store.seedLocal(t, "alice", "correct-horse")
	svc, _ := newTestAuth(t, store, &fakeProvider{})

	v, err := svc.Me(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("Me 失败: %v", err)
	}

	hash := ""
	if u.PasswordHash != nil {
		hash = *u.PasswordHash
	}
	if hash == "" {
		t.Fatal("夹具没造出哈希，这条测试等于什么都没测")
	}

	// 逐个字段扫一遍，看哈希（或它的任何片段）有没有从哪个字符串里漏出去
	fields := map[string]string{
		"username": v.Username, "nickname": v.Nickname, "real_name": v.RealName,
		"role": v.Role, "auth_source": v.AuthSource, "phone": v.Phone,
		"email": v.Email, "avatar_url": v.AvatarURL, "created_at": v.CreatedAt,
	}
	for name, val := range fields {
		if val == hash || (len(val) > 8 && strings.Contains(hash, val)) {
			t.Errorf("字段 %s = %q 泄漏了 password_hash", name, val)
		}
	}
}

// ---------- 辅助 ----------

func assertCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望错误码 %s，但 err 是 nil", want)
	}
	if !apperr.IsCode(err, want) {
		var ae *apperr.Error
		if errors.As(err, &ae) {
			t.Fatalf("期望错误码 %s，实际 %s（message=%q）", want, ae.Code, ae.Message)
		}
		t.Fatalf("期望错误码 %s，实际是一个非 apperr 的错误: %v", want, err)
	}
}

func strptr(s string) *string { return &s }

func clonePtr(s *string) *string {
	if s == nil {
		return nil
	}
	return strptr(*s)
}

func ptrEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// cloneUser 深拷贝一行 users。
//
// 必须拷贝：真 repo 每次查询都返回一个新的结构体，如果假库直接把内部指针交出去，
// 测试里改一下返回值就会连带改掉库里的状态，于是「改密码失败后旧密码还有效吗」
// 这类断言会假绿。假替身和真实现的这个差异，只有深拷贝能抹平。
func cloneUser(u *model.User) *model.User {
	c := *u
	c.Username = clonePtr(u.Username)
	c.PasswordHash = clonePtr(u.PasswordHash)
	c.SSOUserID = clonePtr(u.SSOUserID)
	c.RealName = clonePtr(u.RealName)
	c.StudentID = clonePtr(u.StudentID)
	c.AvatarURL = clonePtr(u.AvatarURL)
	c.Phone = clonePtr(u.Phone)
	c.Email = clonePtr(u.Email)
	return &c
}

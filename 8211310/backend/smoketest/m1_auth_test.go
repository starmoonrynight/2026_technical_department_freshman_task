package smoketest

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"lostfound/internal/apperr"
	"lostfound/internal/auth"
)

// 本文件覆盖 M1 的完成判据（§12），逐字对照：
//
//	冒烟：注册 → 登录 → 带 token 访问 /api/auth/me → 错 token 得 UNAUTHORIZED
//	      → 重名得 USER_ALREADY_EXISTS → 弱密码得 WEAK_PASSWORD
//	      → banned 用户登录得 USER_BANNED
//
// 这条链在 TestM1CriterionChain 里按同样的顺序走了一遍，读那个函数就等于读判据。
// 其余测试把链上每一步的响应形状、HTTP 状态码，以及几条判据没写但 §4/§8 写死了的
// 约束（#4 #5 两个端点、SSO 用户不能改密码、封号即时生效）各自钉住。
//
// 断言纪律（本包统一）：只断言 code 和字段集合，不断言 message 措辞 ——
// message 是给人看的中文，改一个字不该让测试红。唯一的例外是
// TestLoginDoesNotRevealWhetherUserExists，那里 message 必须**逐字相同**，
// 因为「文案不同」本身就是账号枚举的泄漏渠道。

// ---------- 判据链 ----------

func TestM1CriterionChain(t *testing.T) {
	harness.TruncateAll(t)

	const (
		username = "chainuser"
		password = "correct-horse-battery"
	)

	// ① 注册
	reg := harness.Post(t, "/api/auth/register", map[string]any{
		"username": username,
		"password": password,
	}, "")
	RequireCode(t, reg, apperr.CodeOK)
	if reg.HTTPStatus != http.StatusOK {
		t.Errorf("注册期望 HTTP 200，实际 %d", reg.HTTPStatus)
	}
	var registered struct {
		ID int64 `json:"id"`
	}
	reg.DataInto(t, &registered)

	// ② 登录
	login := harness.Post(t, "/api/auth/login", map[string]any{
		"username": username,
		"password": password,
	}, "")
	RequireCode(t, login, apperr.CodeOK)

	var session struct {
		Token string `json:"token"`
	}
	login.DataInto(t, &session)
	if session.Token == "" {
		t.Fatal("登录成功但 token 是空的，后面几步没法走")
	}

	// ③ 带 token 访问 /api/auth/me
	me := harness.Get(t, "/api/auth/me", session.Token)
	RequireCode(t, me, apperr.CodeOK)

	var view struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	}
	me.DataInto(t, &view)
	if view.ID != registered.ID {
		t.Errorf("me.id = %d，注册时拿到的是 %d —— token 里的 sub 解错了人", view.ID, registered.ID)
	}
	if view.Username != username {
		t.Errorf("me.username = %q，期望 %q", view.Username, username)
	}

	// ④ 错 token 得 UNAUTHORIZED
	bad := harness.Get(t, "/api/auth/me", tamperSignature(session.Token))
	RequireCode(t, bad, apperr.CodeUnauthorized)
	if bad.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("错 token 期望 HTTP 401，实际 %d", bad.HTTPStatus)
	}

	// ⑤ 重名得 USER_ALREADY_EXISTS
	dup := harness.Post(t, "/api/auth/register", map[string]any{
		"username": username,
		"password": password,
	}, "")
	RequireCode(t, dup, apperr.CodeUserAlreadyExists)
	if dup.HTTPStatus != http.StatusConflict {
		t.Errorf("重名期望 HTTP 409，实际 %d", dup.HTTPStatus)
	}

	// ⑥ 弱密码得 WEAK_PASSWORD
	weak := harness.Post(t, "/api/auth/register", map[string]any{
		"username": "weakling",
		"password": "12345678", // 够 8 位但纯数字，中 §8 两条规则里的第二条
	}, "")
	RequireCode(t, weak, apperr.CodeWeakPassword)

	// ⑦ banned 用户登录得 USER_BANNED
	//
	// 这里必须用「密码正确 + 已封禁」的账号：LocalProvider 是先验密码再判封禁的，
	// 密码错了会得到 INVALID_CREDENTIALS。那个顺序是刻意的，理由写在 auth/local.go。
	harness.Ban(t, registered.ID)

	banned := harness.Post(t, "/api/auth/login", map[string]any{
		"username": username,
		"password": password,
	}, "")
	RequireCode(t, banned, apperr.CodeUserBanned)
	if banned.HTTPStatus != http.StatusForbidden {
		t.Errorf("封禁用户登录期望 HTTP 403，实际 %d", banned.HTTPStatus)
	}
}

// ---------- #1 注册 ----------

// registerFields 是 §4 第 1 行 data 列的字段集合，四个，不多不少。
var registerFields = []string{"id", "nickname", "role", "username"}

func TestRegisterResponseShape(t *testing.T) {
	harness.TruncateAll(t)

	r := harness.Post(t, "/api/auth/register", map[string]any{
		"username": "shapeuser",
		"password": "correct-horse",
		"nickname": "小明",
	}, "")
	RequireCode(t, r, apperr.CodeOK)

	assertSameSet(t, "register 的 data", registerFields, dataKeys(t, r))

	var data struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Nickname string `json:"nickname"`
		Role     string `json:"role"`
	}
	r.DataInto(t, &data)

	if data.ID <= 0 {
		t.Errorf("id = %d，期望是正数", data.ID)
	}
	if data.Username != "shapeuser" {
		t.Errorf("username = %q", data.Username)
	}
	if data.Nickname != "小明" {
		t.Errorf("nickname = %q，期望原样返回", data.Nickname)
	}
	// 让请求体决定 role 就是个提权漏洞，所以这里必须是写死的 user
	if data.Role != "user" {
		t.Errorf("role = %q，新注册账号必须是 user", data.Role)
	}
}

// TestRegisterCannotSelfAssignAdmin 单独钉住提权这条路。
//
// handler 的请求体结构里根本没有 role 字段，所以传了也会被 encoding/json 丢掉。
// 但「结构体里没这个字段」是个很容易被将来的重构破坏的事实（有人为了省事
// 直接把 model.User 当请求体用），而破坏它的后果是任何人都能注册成管理员。
func TestRegisterCannotSelfAssignAdmin(t *testing.T) {
	harness.TruncateAll(t)

	for _, body := range []map[string]any{
		{"username": "sneaky1", "password": "correct-horse", "role": "admin"},
		{"username": "sneaky2", "password": "correct-horse", "credit_score": 200},
		{"username": "sneaky3", "password": "correct-horse", "status": "banned"},
	} {
		RequireOK(t, harness.Post(t, "/api/auth/register", body, ""), "注册 "+body["username"].(string))
	}

	rows := harness.Count(t, `SELECT count(*) FROM users WHERE role <> 'user' OR credit_score <> 100 OR status <> 'active'`)
	if rows != 0 {
		t.Errorf("有 %d 个新账号拿到了不该有的 role/credit_score/status —— 请求体能决定内部字段", rows)
	}
}

// TestRegisterDoesNotReturnToken 钉住「注册和登录是两个端点」这个契约。
//
// 很多系统注册完直接把 token 发回来（省一次请求），但计划 §4 把它们分开了：
// #1 的 data 列只有四个字段，没有 token。这条测试存在的意义是防止将来有人
// 「顺手优化」成注册即登录 —— 那会让前端少写一个跳转，代价是「注册」和「登录」
// 两个动作在日志和风控上再也分不开了。
func TestRegisterDoesNotReturnToken(t *testing.T) {
	harness.TruncateAll(t)

	r := harness.Post(t, "/api/auth/register", map[string]any{
		"username": "notoken", "password": "correct-horse",
	}, "")
	RequireCode(t, r, apperr.CodeOK)

	if _, has := dataKeys(t, r)["token"]; has {
		t.Error("注册响应里出现了 token —— §4 第 1 行的 data 只有 id/username/nickname/role")
	}
	// 注册完必须真的能登录，否则「不返回 token」就等于「注册完用不了」
	RequireOK(t, harness.Post(t, "/api/auth/login", map[string]any{
		"username": "notoken", "password": "correct-horse",
	}, ""), "注册完再登录")
}

func TestRegisterNicknameFallsBackToUsername(t *testing.T) {
	harness.TruncateAll(t)

	// nickname 是可选字段（§4 第 1 行标了 ?）。不传时必须回落到用户名：
	// users.nickname 是 NOT NULL DEFAULT ''，放任它为空的话，广场、通知、
	// 归还确认里每一个显示名的位置都会渲染成一片空白。
	r := harness.Post(t, "/api/auth/register", map[string]any{
		"username": "nonickname", "password": "correct-horse",
	}, "")
	RequireCode(t, r, apperr.CodeOK)

	var data struct {
		Nickname string `json:"nickname"`
	}
	r.DataInto(t, &data)
	if data.Nickname != "nonickname" {
		t.Errorf("没传 nickname 时得到 %q，期望回落到用户名 %q", data.Nickname, "nonickname")
	}

	// 库里也必须是这个值，不能只是响应里好看
	row := harness.QueryRow(t, `SELECT nickname FROM users WHERE username = $1`, "nonickname")
	if row["nickname"] != "nonickname" {
		t.Errorf("库里的 nickname = %v", row["nickname"])
	}
}

func TestRegisterRejects(t *testing.T) {
	harness.TruncateAll(t)

	// 先占一个名字，用来测重名
	RequireOK(t, harness.Post(t, "/api/auth/register", map[string]any{
		"username": "taken", "password": "correct-horse",
	}, ""), "预置用户 taken")

	cases := []struct {
		name     string
		body     map[string]any
		wantCode string
		wantHTTP int
	}{
		{
			name:     "重名",
			body:     map[string]any{"username": "taken", "password": "correct-horse"},
			wantCode: apperr.CodeUserAlreadyExists, wantHTTP: http.StatusConflict,
		},
		{
			name:     "弱密码：太短",
			body:     map[string]any{"username": "fresh1", "password": "abc1234"},
			wantCode: apperr.CodeWeakPassword, wantHTTP: http.StatusBadRequest,
		},
		{
			name:     "弱密码：纯数字",
			body:     map[string]any{"username": "fresh2", "password": "12345678"},
			wantCode: apperr.CodeWeakPassword, wantHTTP: http.StatusBadRequest,
		},
		{
			name:     "用户名为空",
			body:     map[string]any{"username": "", "password": "correct-horse"},
			wantCode: apperr.CodeValidation, wantHTTP: http.StatusBadRequest,
		},
		{
			name:     "用户名太短",
			body:     map[string]any{"username": "ab", "password": "correct-horse"},
			wantCode: apperr.CodeValidation, wantHTTP: http.StatusBadRequest,
		},
		{
			name:     "缺 password 字段",
			body:     map[string]any{"username": "fresh3"},
			wantCode: apperr.CodeValidation, wantHTTP: http.StatusBadRequest,
		},
		{
			name:     "字段类型不对",
			body:     map[string]any{"username": 12345, "password": "correct-horse"},
			wantCode: apperr.CodeValidation, wantHTTP: http.StatusBadRequest,
		},
		{
			name:     "请求体不是合法 JSON",
			body:     nil, // Do 会因此不带请求体，服务端拿到空 body
			wantCode: apperr.CodeValidation, wantHTTP: http.StatusBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := harness.Post(t, "/api/auth/register", tc.body, "")
			RequireCode(t, r, tc.wantCode)
			if r.HTTPStatus != tc.wantHTTP {
				t.Errorf("期望 HTTP %d，实际 %d", tc.wantHTTP, r.HTTPStatus)
			}
			if r.RequestID == "" {
				t.Error("失败响应里也必须有 request_id，否则用户报「注册失败」时没法捞日志")
			}
		})
	}

	// 上面这些失败请求一个都不该在库里留下行（taken 是预置的那一个）
	if n := harness.Count(t, `SELECT count(*) FROM users`); n != 1 {
		t.Errorf("users 表里有 %d 行，期望 1 行（失败的注册不该留下半成品）", n)
	}
}

func TestRegisterTrimsUsernameBeforeStoring(t *testing.T) {
	harness.TruncateAll(t)

	// 注册时 trim、登录时也 trim，两边必须是同一套规范化。
	// 只 trim 一边的后果是用户永远登不上，而后端日志显示「用户名不存在」—— 明明就在库里。
	RequireOK(t, harness.Post(t, "/api/auth/register", map[string]any{
		"username": "  trimme  ", "password": "correct-horse",
	}, ""), "注册带空格的用户名")

	row := harness.QueryRow(t, `SELECT username FROM users WHERE id = (SELECT min(id) FROM users)`)
	if row["username"] != "trimme" {
		t.Errorf("库里存的 username = %q，期望 trim 过的 %q", row["username"], "trimme")
	}

	RequireOK(t, harness.Post(t, "/api/auth/login", map[string]any{
		"username": " trimme ", "password": "correct-horse",
	}, ""), "用带空格的用户名登录")
}

// ---------- #2 登录 ----------

// loginFields 是 §4 第 2 行 data 列的三个字段。
var loginFields = []string{"expires_at", "token", "user"}

func TestLoginResponseShape(t *testing.T) {
	harness.TruncateAll(t)
	RequireOK(t, harness.Post(t, "/api/auth/register", map[string]any{
		"username": "loginshape", "password": "correct-horse", "nickname": "小明",
	}, ""), "预置用户")

	r := harness.Post(t, "/api/auth/login", map[string]any{
		"username": "loginshape", "password": "correct-horse",
	}, "")
	RequireCode(t, r, apperr.CodeOK)
	if r.HTTPStatus != http.StatusOK {
		t.Errorf("期望 HTTP 200，实际 %d", r.HTTPStatus)
	}

	assertSameSet(t, "login 的 data", loginFields, dataKeys(t, r))

	var data struct {
		Token     string                     `json:"token"`
		ExpiresAt string                     `json:"expires_at"`
		User      map[string]json.RawMessage `json:"user"`
	}
	r.DataInto(t, &data)

	if data.Token == "" {
		t.Error("token 是空的")
	}
	if strings.Count(data.Token, ".") != 2 {
		t.Errorf("token 不是三段的 JWT 形状: %q", truncate(data.Token))
	}

	// 前端要靠它决定什么时候重新登录，所以必须是个能解析的时间，而不是空串或毫秒数
	expiresAt, err := time.Parse(time.RFC3339, data.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at = %q 不是 RFC3339: %v", data.ExpiresAt, err)
	}
	if until := time.Until(expiresAt); until <= 0 || until > 25*time.Hour {
		t.Errorf("expires_at 距现在 %s，不在「刚签发、有效期约 24 小时」的合理范围内", until)
	}

	// #2 的 user 和 #3 的 data 是同一个形状（§4 第 2 行的「user:{...}」就是 UserView）
	assertSameSet(t, "login 的 user", userViewFields, data.User)

	// 上面取键集合用的是 map，这里再解一次拿具体值 —— 两种解法各有各的用处：
	// map 能发现「多出来的字段」，结构体只能发现「我想到要声明的字段」。
	var typed struct {
		User struct {
			Username    string `json:"username"`
			Nickname    string `json:"nickname"`
			AuthSource  string `json:"auth_source"`
			CreditScore int    `json:"credit_score"`
		} `json:"user"`
	}
	if err := json.Unmarshal(r.Data, &typed); err != nil {
		t.Fatalf("解 user 失败: %v", err)
	}
	if typed.User.Username != "loginshape" || typed.User.Nickname != "小明" {
		t.Errorf("user 里的身份不对: %+v", typed.User)
	}
	if typed.User.AuthSource != "local" {
		t.Errorf("auth_source = %q，期望 local", typed.User.AuthSource)
	}
	if typed.User.CreditScore != 100 {
		t.Errorf("credit_score = %d，新账号应该是 100（§5 的初始分）", typed.User.CreditScore)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	harness.TruncateAll(t)
	harness.RegisterAndLogin(t, "wrongpw", "correct-horse")

	r := harness.Post(t, "/api/auth/login", map[string]any{
		"username": "wrongpw", "password": "wrong-password",
	}, "")
	RequireCode(t, r, apperr.CodeInvalidCredentials)
	if r.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("期望 HTTP 401，实际 %d", r.HTTPStatus)
	}
}

// TestLoginDoesNotRevealWhetherUserExists 是账号枚举防护的验收点。
//
// 「用户名不存在」和「密码不对」必须在**外部可见的每一处**都长得一样：
// 同一个 code、同一个 HTTP 状态、同一句 message。差任何一个，攻击者就能拿一本
// 用户名字典跑一遍登录接口，把「这个学校有谁注册过」全捞出来。
//
// 计时差的那一半在 internal/auth/local_test.go 里测（dummyHash 烧掉同样的时间），
// 这里测的是响应内容的那一半 —— 两层缺一不可。
func TestLoginDoesNotRevealWhetherUserExists(t *testing.T) {
	harness.TruncateAll(t)
	harness.RegisterAndLogin(t, "realuser", "correct-horse")

	unknown := harness.Post(t, "/api/auth/login", map[string]any{
		"username": "nosuchuser", "password": "correct-horse",
	}, "")
	wrongPw := harness.Post(t, "/api/auth/login", map[string]any{
		"username": "realuser", "password": "wrong-password",
	}, "")

	if unknown.Code != wrongPw.Code {
		t.Errorf("code 不同：%q（用户名不存在） vs %q（密码错）", unknown.Code, wrongPw.Code)
	}
	if unknown.HTTPStatus != wrongPw.HTTPStatus {
		t.Errorf("HTTP 状态不同：%d vs %d", unknown.HTTPStatus, wrongPw.HTTPStatus)
	}
	// message 也要逐字相同 —— 这是本文件唯一断言 message 的地方，理由见上面
	if unknown.Message != wrongPw.Message {
		t.Errorf("message 不同：%q vs %q —— 这就是账号枚举的泄漏渠道", unknown.Message, wrongPw.Message)
	}
	if unknown.Code != apperr.CodeInvalidCredentials {
		t.Errorf("期望两者都是 INVALID_CREDENTIALS，实际 %q", unknown.Code)
	}
}

// ---------- #3 GET /api/auth/me ----------

// userViewFields 是 §4 第 3 行 data 列的 11 个字段。
//
// 这份清单同时充当白名单：多一个字段（比如 password_hash、student_id）
// 或少一个字段，assertSameSet 都会指名道姓地报出来。
var userViewFields = []string{
	"id", "username", "nickname", "real_name", "role", "auth_source",
	"phone", "email", "avatar_url", "credit_score", "created_at",
}

func TestMeRequiresToken(t *testing.T) {
	harness.TruncateAll(t)

	cases := []struct {
		name  string
		token string
	}{
		{"完全不带 Authorization 头", ""},
		{"空 Bearer", "Bearer "},
		{"只有 Bearer 这个词", "Bearer"},
		{"随手写的一串", "Bearer not-a-jwt-at-all"},
		{"少了签名段", "Bearer aaa.bbb"},
		{"没有 Bearer 前缀的裸 token", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.aaa"},
		{"用了 Basic 而不是 Bearer", "Basic dXNlcjpwYXNz"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := harness.Get(t, "/api/auth/me", tc.token)
			// §8：没带 token 和 token 无效都是 UNAUTHORIZED。
			// 分成两个码只会让前端多写一套分支，而两者的处理方式完全一样（跳登录页）。
			RequireCode(t, r, apperr.CodeUnauthorized)
			if r.HTTPStatus != http.StatusUnauthorized {
				t.Errorf("期望 HTTP 401，实际 %d", r.HTTPStatus)
			}
			if r.RequestID == "" {
				t.Error("401 响应也必须有 request_id")
			}
		})
	}
}

// TestMeRejectsForgedToken 是「签名真的被校验了」的证明。
//
// 用一把**别的**密钥给同一个 user id 签一个 token：claims 完全合法，只差签名。
// 这就是攻击者手里的东西 —— sub/iat/exp 是标准字段，谁都知道该长什么样。
// 如果中间件忘了验签、或者接受了 alg=none，这个 token 就会被当成 user 1 放进去。
// 这类漏洞在功能测试里永远不红，因为「正常登录」那条路走得好好的。
func TestMeRejectsForgedToken(t *testing.T) {
	harness.TruncateAll(t)
	s := harness.RegisterAndLogin(t, "honest", "correct-horse")

	forged := forgeTokenWithAnotherSecret(t, s.UserID)
	if forged == s.Token {
		t.Fatal("两把不同的密钥签出了相同的 token，这条测试的前提不成立")
	}

	r := harness.Get(t, "/api/auth/me", forged)
	RequireCode(t, r, apperr.CodeUnauthorized)
	if r.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("期望 HTTP 401，实际 %d", r.HTTPStatus)
	}
}

func TestMeReturnsTheRightUser(t *testing.T) {
	harness.TruncateAll(t)
	alice := harness.RegisterAndLogin(t, "alice", "correct-horse")
	bob := harness.RegisterAndLogin(t, "bobby", "correct-horse")

	if alice.UserID == bob.UserID {
		t.Fatalf("两个用户的 id 相同（%d），夹具坏了", alice.UserID)
	}

	// 两个人各拿自己的 token，必须各看到自己。
	// 这条挡住了「中间件把 user_id 存进了包级变量」这类串号 bug ——
	// 那种 bug 在单用户测试里完全看不出来。
	for _, s := range []Session{alice, bob} {
		r := harness.Get(t, "/api/auth/me", s.Token)
		RequireCode(t, r, apperr.CodeOK)

		var v struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
			Role     string `json:"role"`
		}
		r.DataInto(t, &v)
		if v.ID != s.UserID || v.Username != s.Username {
			t.Errorf("token 属于 %s(%d)，但 /me 返回了 %s(%d)", s.Username, s.UserID, v.Username, v.ID)
		}
		if v.Role != "user" {
			t.Errorf("role = %q", v.Role)
		}
	}
}

func TestMeDoesNotLeakPasswordHash(t *testing.T) {
	harness.TruncateAll(t)
	s := harness.RegisterAndLogin(t, "leaky", "correct-horse")

	r := harness.Get(t, "/api/auth/me", s.Token)
	RequireCode(t, r, apperr.CodeOK)

	// 检查原始 JSON 而不是解结构体：解结构体只能检查「我声明过的字段」，
	// 而这里要防的恰恰是「多出来一个我没想到的字段」。
	raw := string(r.Data)
	for _, prefix := range []string{"$2a$", "$2b$", "$2y$"} {
		if strings.Contains(raw, prefix) {
			t.Errorf("响应里出现了 bcrypt 哈希（%s）: %s", prefix, truncate(raw))
		}
	}
	if strings.Contains(raw, "password") {
		t.Errorf("响应里出现了 password 字样: %s", truncate(raw))
	}

	// 库里确实存了哈希 —— 否则上面几条断言只是因为它压根没存而通过
	row := harness.QueryRow(t, `SELECT password_hash FROM users WHERE id = $1`, s.UserID)
	hash, _ := row["password_hash"].(string)
	if !strings.HasPrefix(hash, "$2") {
		t.Fatalf("库里存的 password_hash 不像 bcrypt 哈希: %q", truncate(hash))
	}
	if strings.Contains(raw, hash) {
		t.Error("响应里含有完整的 password_hash")
	}
}

// ---------- #4 PUT /api/auth/me ----------

func TestUpdateMe(t *testing.T) {
	harness.TruncateAll(t)
	s := harness.RegisterAndLogin(t, "updateme", "correct-horse")

	r := harness.Do(t, http.MethodPut, "/api/auth/me", map[string]any{
		"nickname": "新昵称",
		"phone":    "13800001111",
		"email":    "me@example.com",
	}, s.Token)
	RequireCode(t, r, apperr.CodeOK)

	// §4 第 4 行：data「同 #3」—— 改完直接把新形状返回，省一次 GET
	assertSameSet(t, "PUT /api/auth/me 的 data", userViewFields, dataKeys(t, r))

	var v struct {
		Nickname string `json:"nickname"`
		Phone    string `json:"phone"`
		Email    string `json:"email"`
	}
	r.DataInto(t, &v)
	if v.Nickname != "新昵称" || v.Phone != "13800001111" || v.Email != "me@example.com" {
		t.Errorf("改完的值不对: %+v", v)
	}

	// 再 GET 一次，确认真的落库了，而不只是响应里好看
	again := harness.Get(t, "/api/auth/me", s.Token)
	RequireCode(t, again, apperr.CodeOK)
	var v2 struct {
		Nickname string `json:"nickname"`
		Phone    string `json:"phone"`
		Email    string `json:"email"`
	}
	again.DataInto(t, &v2)
	if v2 != v {
		t.Errorf("重新 GET 得到的和 PUT 返回的不一致: %+v vs %+v", v2, v)
	}
}

func TestUpdateMePartialKeepsOtherFields(t *testing.T) {
	harness.TruncateAll(t)
	s := harness.RegisterAndLogin(t, "partial", "correct-horse")

	RequireOK(t, harness.Do(t, http.MethodPut, "/api/auth/me", map[string]any{
		"nickname": "第一次", "phone": "13800001111", "email": "a@b.c",
	}, s.Token), "预置资料")

	// 只传 nickname：phone 和 email 必须原封不动。
	// 这就是「字段缺失」和「字段为空串」必须区分开的原因 ——
	// 用普通 string 接收的话两者都是 ""，一改昵称就把手机号冲掉了。
	r := harness.Do(t, http.MethodPut, "/api/auth/me", map[string]any{
		"nickname": "第二次",
	}, s.Token)
	RequireCode(t, r, apperr.CodeOK)

	var v struct {
		Nickname string `json:"nickname"`
		Phone    string `json:"phone"`
		Email    string `json:"email"`
	}
	r.DataInto(t, &v)
	if v.Nickname != "第二次" {
		t.Errorf("nickname = %q", v.Nickname)
	}
	if v.Phone != "13800001111" || v.Email != "a@b.c" {
		t.Errorf("只改昵称却把别的字段冲掉了: phone=%q email=%q", v.Phone, v.Email)
	}
}

func TestUpdateMeEmptyStringClears(t *testing.T) {
	harness.TruncateAll(t)
	s := harness.RegisterAndLogin(t, "clearme", "correct-horse")

	RequireOK(t, harness.Do(t, http.MethodPut, "/api/auth/me", map[string]any{
		"phone": "13800001111",
	}, s.Token), "预置手机号")

	RequireOK(t, harness.Do(t, http.MethodPut, "/api/auth/me", map[string]any{
		"phone": "",
	}, s.Token), "清空手机号")

	// 清空必须落成 NULL 而不是空串：WHERE phone = '' 和 WHERE phone IS NULL
	// 是两条不同的查询，混着存会让将来的统计和去重都得出错误结果。
	row := harness.QueryRow(t, `SELECT phone FROM users WHERE id = $1`, s.UserID)
	if row["phone"] != nil {
		t.Errorf("清空之后库里存的不是 NULL，而是 %#v", row["phone"])
	}
}

func TestUpdateMeRejects(t *testing.T) {
	harness.TruncateAll(t)
	s := harness.RegisterAndLogin(t, "rejectme", "correct-horse")

	cases := []struct {
		name string
		body map[string]any
	}{
		{"昵称改成空串", map[string]any{"nickname": ""}},
		{"昵称只有空格", map[string]any{"nickname": "    "}},
		{"昵称太长（33 个汉字）", map[string]any{"nickname": strings.Repeat("昵", 33)}},
		{"昵称类型不对", map[string]any{"nickname": 42}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := harness.Do(t, http.MethodPut, "/api/auth/me", tc.body, s.Token)
			RequireCode(t, r, apperr.CodeValidation)
			if r.HTTPStatus != http.StatusBadRequest {
				t.Errorf("期望 HTTP 400，实际 %d", r.HTTPStatus)
			}
		})
	}

	// 昵称是这个系统里唯一的显示名，失败的修改不该把它冲成空
	r := harness.Get(t, "/api/auth/me", s.Token)
	RequireCode(t, r, apperr.CodeOK)
	var v struct {
		Nickname string `json:"nickname"`
	}
	r.DataInto(t, &v)
	if v.Nickname != "rejectme" {
		t.Errorf("失败的修改把昵称改成了 %q", v.Nickname)
	}
}

// TestUpdateMeDoesNotValidateContactFormat 把「刻意不校验格式」钉成一条会红的测试。
//
// 这个系统从头到尾不发邮件、不发短信（定位原则 2「平台不做站内私信」，
// 通知全部走站内 notifications 表），所以 phone/email 纯粹是用户自报的展示信息。
// 校验一个没有任何代码消费的东西，只会挡住「我就想填个备注」的用户 ——
// 和 §3.2 对 items.contact 不做校验是同一个判断：平台不为用户自报的信息背书。
func TestUpdateMeDoesNotValidateContactFormat(t *testing.T) {
	harness.TruncateAll(t)
	s := harness.RegisterAndLogin(t, "freeform", "correct-horse")

	r := harness.Do(t, http.MethodPut, "/api/auth/me", map[string]any{
		"phone": "问宿舍阿姨",
		"email": "图书馆前台 86919001",
	}, s.Token)
	RequireCode(t, r, apperr.CodeOK)

	var v struct {
		Phone string `json:"phone"`
		Email string `json:"email"`
	}
	r.DataInto(t, &v)
	if v.Phone != "问宿舍阿姨" || v.Email != "图书馆前台 86919001" {
		t.Errorf("非格式化的联系方式没有原样存下来: %+v", v)
	}
}

func TestUpdateMeRequiresToken(t *testing.T) {
	harness.TruncateAll(t)

	r := harness.Do(t, http.MethodPut, "/api/auth/me", map[string]any{"nickname": "黑客"}, "")
	RequireCode(t, r, apperr.CodeUnauthorized)
}

// TestUpdateMeCannotTouchOtherUsers 挡住「改自己的资料时顺手改别人」这条路。
//
// service 只接受一个 userID，而且它来自中间件从 token 解出的 sub，
// 请求体里没有任何地方能指定「改谁」。但请求体里塞一个 id 字段是
// 太自然的错误了（前端往往就是拿整个 user 对象回传的），所以钉住它。
func TestUpdateMeCannotTouchOtherUsers(t *testing.T) {
	harness.TruncateAll(t)
	alice := harness.RegisterAndLogin(t, "alice", "correct-horse")
	bob := harness.RegisterAndLogin(t, "bobby", "correct-horse")

	RequireOK(t, harness.Do(t, http.MethodPut, "/api/auth/me", map[string]any{
		"id":       bob.UserID,
		"user_id":  bob.UserID,
		"nickname": "被篡改",
		"role":     "admin",
	}, alice.Token), "alice 改自己的资料")

	bobRow := harness.QueryRow(t, `SELECT nickname, role FROM users WHERE id = $1`, bob.UserID)
	if bobRow["nickname"] != "bobby" {
		t.Errorf("alice 改到了 bob 的昵称: %v", bobRow["nickname"])
	}
	if bobRow["role"] != "user" {
		t.Errorf("bob 的 role 被改成了 %v", bobRow["role"])
	}

	aliceRow := harness.QueryRow(t, `SELECT nickname, role FROM users WHERE id = $1`, alice.UserID)
	if aliceRow["nickname"] != "被篡改" {
		t.Errorf("alice 自己的昵称没改成功: %v", aliceRow["nickname"])
	}
	if aliceRow["role"] != "user" {
		t.Errorf("alice 通过请求体把自己的 role 改成了 %v", aliceRow["role"])
	}
}

// ---------- #5 POST /api/auth/change-password ----------

func TestChangePassword(t *testing.T) {
	harness.TruncateAll(t)
	s := harness.RegisterAndLogin(t, "changer", "old-password-1")

	r := harness.Post(t, "/api/auth/change-password", map[string]any{
		"old_password": "old-password-1",
		"new_password": "new-password-2",
	}, s.Token)
	RequireCode(t, r, apperr.CodeOK)

	// §4 第 5 行：data 是 null —— 这个动作没有需要回传的结果，成功本身就是全部信息
	if string(r.Data) != "null" {
		t.Errorf("期望 data 是 null，实际是 %s", truncate(string(r.Data)))
	}

	RequireOK(t, harness.Post(t, "/api/auth/login", map[string]any{
		"username": "changer", "password": "new-password-2",
	}, ""), "用新密码登录")

	bad := harness.Post(t, "/api/auth/login", map[string]any{
		"username": "changer", "password": "old-password-1",
	}, "")
	RequireCode(t, bad, apperr.CodeInvalidCredentials)
}

// TestChangePasswordDoesNotInvalidateExistingToken 记录的是一条**已知限制**，
// 不是理想行为。
//
// JWT 是无状态的，签出去只能等它过期（默认 24 小时）。所以「密码泄漏了赶紧改」
// 这个动作并不能立刻把攻击者踢出去。要真正做到需要给 users 加一列
// password_changed_at，让中间件比对 token 的 iat —— 那是一次 schema 变更，
// 计划 §3.7 里没有这一列。
//
// 把它写成测试而不是只写在注释里，是为了让将来补上这一列的人**必须**回来改这条
// 测试：那时它会红，红得莫名其妙，于是他会读到这里，然后明白自己刚刚修好了什么。
func TestChangePasswordDoesNotInvalidateExistingToken(t *testing.T) {
	harness.TruncateAll(t)
	s := harness.RegisterAndLogin(t, "stale", "old-password-1")

	RequireOK(t, harness.Post(t, "/api/auth/change-password", map[string]any{
		"old_password": "old-password-1", "new_password": "new-password-2",
	}, s.Token), "改密码")

	RequireCode(t, harness.Get(t, "/api/auth/me", s.Token), apperr.CodeOK) // ← 旧 token 仍然有效
}

func TestChangePasswordWrongOld(t *testing.T) {
	harness.TruncateAll(t)
	s := harness.RegisterAndLogin(t, "wrongold", "old-password-1")

	r := harness.Post(t, "/api/auth/change-password", map[string]any{
		"old_password": "not-my-password",
		"new_password": "new-password-2",
	}, s.Token)
	// 单独的码（§8）：前端要提示「原密码不正确」并聚焦到那一个输入框，
	// 合并进 VALIDATION 就丢失了这个信息
	RequireCode(t, r, apperr.CodeOldPasswordWrong)
	if r.HTTPStatus != http.StatusBadRequest {
		t.Errorf("期望 HTTP 400，实际 %d", r.HTTPStatus)
	}

	// 失败之后旧密码必须仍然有效，否则用户会被锁在自己的账号外面
	RequireOK(t, harness.Post(t, "/api/auth/login", map[string]any{
		"username": "wrongold", "password": "old-password-1",
	}, ""), "改密码失败后用旧密码登录")
}

func TestChangePasswordRejectsWeakNew(t *testing.T) {
	harness.TruncateAll(t)
	s := harness.RegisterAndLogin(t, "weaknew", "old-password-1")

	for _, weak := range []string{"short", "12345678"} {
		r := harness.Post(t, "/api/auth/change-password", map[string]any{
			"old_password": "old-password-1", "new_password": weak,
		}, s.Token)
		RequireCode(t, r, apperr.CodeWeakPassword)
	}

	RequireOK(t, harness.Post(t, "/api/auth/login", map[string]any{
		"username": "weaknew", "password": "old-password-1",
	}, ""), "改密码被拒后用旧密码登录")
}

func TestChangePasswordRequiresToken(t *testing.T) {
	harness.TruncateAll(t)

	r := harness.Post(t, "/api/auth/change-password", map[string]any{
		"old_password": "x", "new_password": "new-password-2",
	}, "")
	RequireCode(t, r, apperr.CodeUnauthorized)
}

// TestChangePasswordForbiddenForSSOUser 覆盖 §4 第 5 行那个括号：「JWT（仅 local 用户）」。
//
// SSO 用户的密码在杭电助手那边，我们既没有也不该有。
// 返回 FORBIDDEN 而不是 VALIDATION —— 这不是「你填错了」，是「这个动作对你不成立」。
func TestChangePasswordForbiddenForSSOUser(t *testing.T) {
	harness.TruncateAll(t)
	id := seedSSOUser(t, "hdu-9527", "张三")

	// 这个 token 不是登录换来的（SSO 用户没有密码可登），是用同一把密钥铸的。
	// 它和生产环境里 M8 的 SSO 回调签出的 token 完全同源，中间件分不出区别。
	r := harness.Post(t, "/api/auth/change-password", map[string]any{
		"old_password": "whatever", "new_password": "new-password-2",
	}, harness.TokenFor(t, id))
	RequireCode(t, r, apperr.CodeForbidden)
	if r.HTTPStatus != http.StatusForbidden {
		t.Errorf("期望 HTTP 403，实际 %d", r.HTTPStatus)
	}

	// 但同一个 token 访问 /me 是好的：FORBIDDEN 只针对改密码这一个动作
	me := harness.Get(t, "/api/auth/me", harness.TokenFor(t, id))
	RequireCode(t, me, apperr.CodeOK)
	var v struct {
		AuthSource string `json:"auth_source"`
		RealName   string `json:"real_name"`
		Username   string `json:"username"`
	}
	me.DataInto(t, &v)
	if v.AuthSource != "hduhelp" {
		t.Errorf("auth_source = %q，期望 hduhelp", v.AuthSource)
	}
	if v.RealName != "张三" {
		t.Errorf("real_name = %q", v.RealName)
	}
	if v.Username != "" {
		t.Errorf("SSO 用户的 username 应该是空串（库里是 NULL），实际 %q", v.Username)
	}
}

// TestSSOUserCannotLoginWithPassword 补上 SSO 账号的另一半：
// 它的 username 和 password_hash 都是 NULL，拿密码去登必须失败，
// 而且失败方式要和「用户名不存在」一致（同 TestLoginDoesNotRevealWhetherUserExists）。
func TestSSOUserCannotLoginWithPassword(t *testing.T) {
	harness.TruncateAll(t)
	seedSSOUser(t, "hdu-9527", "张三")

	r := harness.Post(t, "/api/auth/login", map[string]any{
		"username": "张三", "password": "whatever",
	}, "")
	RequireCode(t, r, apperr.CodeInvalidCredentials)
}

// ---------- 封禁 ----------

// TestBanTakesEffectImmediately 是「治理动作即时生效」这个设计决定的验收点。
//
// role 和 status 刻意**不放**进 JWT 的 claims（见 auth.TokenSigner.Issue 的注释），
// 中间件每个请求都回库里读一遍。代价是每个已登录请求多一次主键查询，
// 换来的是：admin 一点封禁，那个人手上的旧 token 立刻失效。
//
// 如果哪天有人为了性能把 status 塞进 claims「优化」一下，这条测试会红 ——
// 而那时线上正在发生的事是「封号之后还能继续发帖最长 24 小时」。
func TestBanTakesEffectImmediately(t *testing.T) {
	harness.TruncateAll(t)
	s := harness.RegisterAndLogin(t, "spammer", "correct-horse")

	RequireCode(t, harness.Get(t, "/api/auth/me", s.Token), apperr.CodeOK)

	harness.Ban(t, s.UserID)

	// 同一个 token，同一秒，立刻被拒
	r := harness.Get(t, "/api/auth/me", s.Token)
	RequireCode(t, r, apperr.CodeUserBanned)
	if r.HTTPStatus != http.StatusForbidden {
		t.Errorf("期望 HTTP 403，实际 %d", r.HTTPStatus)
	}

	// 登录那条路也堵着
	RequireCode(t, harness.Post(t, "/api/auth/login", map[string]any{
		"username": "spammer", "password": "correct-horse",
	}, ""), apperr.CodeUserBanned)
}

// ---------- 辅助 ----------

// seedSSOUser 直接往库里插一个杭电助手账号，返回它的 id。
//
// 走 SQL 而不是走接口：M1 没有「注册 SSO 用户」的端点（那是 M8 的 SSO 回调干的），
// 而 M1 又必须测「SSO 用户不能改密码」这条 §4 的约束。
func seedSSOUser(t *testing.T, externalID, name string) int64 {
	t.Helper()
	var id int64
	err := harness.Pool.QueryRow(context.Background(), `
		INSERT INTO users (auth_source, sso_user_id, real_name, nickname)
		VALUES ('hduhelp', $1, $2, $2)
		RETURNING id`, externalID, name).Scan(&id)
	if err != nil {
		t.Fatalf("插入 SSO 用户失败: %v", err)
	}
	return id
}

// forgeTokenWithAnotherSecret 用一把**别的**密钥给同一个 user id 签 token。
//
// 密钥长度必须 >= 32 字节，否则 NewTokenSigner 会拒绝 —— 那是它该有的行为，
// 而攻击者当然会给自己生成一把够长的。
func forgeTokenWithAnotherSecret(t *testing.T, userID int64) string {
	t.Helper()
	signer, err := auth.NewTokenSigner("attacker-secret-0123456789abcdef0123456789abcdef", time.Hour)
	if err != nil {
		t.Fatalf("建攻击者 signer 失败: %v", err)
	}
	tok, err := signer.Issue(userID)
	if err != nil {
		t.Fatalf("攻击者签 token 失败: %v", err)
	}
	return tok.Value
}

// tamperSignature 改掉签名段的第一个字符，造一个「差一点点对」的 token。
//
// ⚠ 改的必须是**第一个**字符，不能是最后一个。base64url 的最后一位里有 2 个
// 填充位是被忽略的：32 字节的 HMAC 编成 43 个字符，第 43 个字符只承载 4 个有效位，
// 于是 'A'/'B'/'C'/'D' 四个不同的字符解出来是**完全相同**的签名字节。
// 改最后一位有很高概率造出一个依然有效的 token，测试会假红（以为验签坏了）。
// 第一版就是踩在这里。
func tamperSignature(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[2] == "" {
		return token + "tampered"
	}
	first := parts[2][0]
	repl := byte('A')
	if first == 'A' {
		repl = 'B'
	}
	parts[2] = string(repl) + parts[2][1:]
	return strings.Join(parts, ".")
}

// dataKeys 返回 data 对象的全部键。
//
// 用 map[string]json.RawMessage 而不是解进一个结构体：结构体只会告诉你
// 「我声明过的字段在不在」，而这里要防的是**多出来**一个没声明的字段
// （password_hash 就是这么泄漏的）。
func dataKeys(t *testing.T, r Response) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(r.Data, &m); err != nil {
		t.Fatalf("data 不是一个 JSON 对象，没法取键集合: %v\n原始 data: %s", err, truncate(string(r.Data)))
	}
	return m
}

// assertSameSet 断言「实际字段集合」和「计划里写死的字段集合」恰好相同，
// 并且把多出来的和少掉的都点名报出来。
//
// 只比数量是不够的：多一个 password_hash、少一个 created_at，数量可能正好抵消。
func assertSameSet[V any](t *testing.T, what string, want []string, got map[string]V) {
	t.Helper()

	wantSet := make(map[string]bool, len(want))
	for _, k := range want {
		if wantSet[k] {
			t.Fatalf("%s 的期望字段清单里有重复项 %q", what, k)
		}
		wantSet[k] = true
	}

	var extra, missing []string
	for k := range got {
		if !wantSet[k] {
			extra = append(extra, k)
		}
	}
	for k := range wantSet {
		if _, ok := got[k]; !ok {
			missing = append(missing, k)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)

	if len(extra) > 0 {
		t.Errorf("%s 多出了计划里没有的字段 %v —— 如果是有意的，请先改计划 §4 再改这里", what, extra)
	}
	if len(missing) > 0 {
		t.Errorf("%s 少了字段 %v", what, missing)
	}
}

package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"lostfound/internal/apperr"
)

// testSecret 是测试用的密钥，长度刚好过 minSecretBytes 的门槛。
// 它不是秘密也不需要保密 —— 只在这个文件里用来签发/校验测试 token。
const testSecret = "unit-test-secret-0123456789abcdef0123456789abcdef"

func newTestSigner(t *testing.T, ttl time.Duration) TokenSigner {
	t.Helper()
	s, err := NewTokenSigner(testSecret, ttl)
	if err != nil {
		t.Fatalf("NewTokenSigner 失败: %v", err)
	}
	return s
}

// signWith 用任意算法和任意 claims 造一个 token，专门用来喂给 Parse 做**负面**测试。
// 正常的签发路径只走 TokenSigner.Issue；这个helper 的存在意义就是造出
// 「长得像 token 但不该被接受」的东西。
func signWith(t *testing.T, secret string, method jwt.SigningMethod, claims jwt.MapClaims) string {
	t.Helper()
	var key any = []byte(secret)
	if method == jwt.SigningMethodNone {
		// jwt 库要求显式表态才允许签 alg=none，这是它自己的一道防呆
		key = jwt.UnsafeAllowNoneSignatureType
	}
	raw, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatalf("造测试 token 失败: %v", err)
	}
	return raw
}

func TestTokenIssueParseRoundTrip(t *testing.T) {
	s := newTestSigner(t, time.Hour)

	tok, err := s.Issue(42)
	if err != nil {
		t.Fatalf("Issue 失败: %v", err)
	}
	if tok.Value == "" {
		t.Fatal("签出来的 token 是空的")
	}

	got, err := s.Parse(tok.Value)
	if err != nil {
		t.Fatalf("自己签的 token 解不开: %v", err)
	}
	if got != 42 {
		t.Errorf("user id 不对：期望 42，实际 %d", got)
	}
}

func TestTokenExpiresAtMatchesTTL(t *testing.T) {
	const ttl = 24 * time.Hour
	s := newTestSigner(t, ttl)

	before := time.Now()
	tok, err := s.Issue(1)
	if err != nil {
		t.Fatalf("Issue 失败: %v", err)
	}
	after := time.Now()

	// expires_at 要返回给前端（#2 响应里的字段），所以它必须真的是 now+ttl。
	// 用 before/after 夹住而不是断言等于某个具体时刻：签发本身要花时间，
	// 写成相等会在慢机器上偶发失败，那种「偶尔红一下」的测试比没有测试更糟。
	lo, hi := before.Add(ttl), after.Add(ttl)
	if tok.ExpiresAt.Before(lo) || tok.ExpiresAt.After(hi) {
		t.Errorf("ExpiresAt=%s 不在期望区间 [%s, %s] 内", tok.ExpiresAt, lo, hi)
	}
	if s.TTL() != ttl {
		t.Errorf("TTL() 应该原样返回配置值 %s，实际 %s", ttl, s.TTL())
	}
}

// TestTokenClaimsCarryNoRole 钉住「claims 里只有 sub」这个决定。
//
// 这是 jwt.go 里那段长注释的可执行版本。如果有人图省事把 role 塞进 claims
// （很诱人，因为那样 admin 校验就不用查库了），这条测试会红。
// 后果是真实的：admin 给某人降权后，那人手上的旧 token 里还写着 role=admin，
// 能继续调管理接口直到 token 过期（默认 24 小时）—— 封号和降权都会失效。
func TestTokenClaimsCarryNoRole(t *testing.T) {
	s := newTestSigner(t, time.Hour)
	tok, err := s.Issue(7)
	if err != nil {
		t.Fatalf("Issue 失败: %v", err)
	}

	parsed, _, err := jwt.NewParser().ParseUnverified(tok.Value, jwt.MapClaims{})
	if err != nil {
		t.Fatalf("解析测试 token 失败: %v", err)
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatalf("claims 类型不对: %T", parsed.Claims)
	}

	// role / status / username 一律不许出现：这些值会变，而 token 签出去就改不了，
	// 只能每次请求从库里读最新的。
	for _, forbidden := range []string{"role", "status", "username", "admin", "is_admin"} {
		if _, exists := claims[forbidden]; exists {
			t.Errorf("claims 里不该有 %q —— 它会让权限变更无法即时生效", forbidden)
		}
	}
	if claims["sub"] != "7" {
		t.Errorf("sub 应该是用户 id 的字符串形式，实际 %v", claims["sub"])
	}
}

func TestParseRejectsBadTokens(t *testing.T) {
	s := newTestSigner(t, time.Hour)

	good, err := s.Issue(1)
	if err != nil {
		t.Fatalf("Issue 失败: %v", err)
	}

	// 篡改签名：把最后一段的最后一个字符换掉。
	// 不能只是「截断」—— 截断会被当成格式错误，而改一个字符才是真正在测试验签。
	tampered := good.Value[:len(good.Value)-1] + flipLastChar(good.Value)

	cases := []struct {
		name  string
		token string
	}{
		{"空串", ""},
		{"只有空格", "   "},
		{"完全不是 token", "not-a-jwt"},
		{"少了签名段", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0"},
		{"签名被改了一个字符", tampered},
		{"另一个密钥签的", signWith(t, "a-completely-different-secret-key-0123456789",
			jwt.SigningMethodHS256, jwt.MapClaims{"sub": "1"})},
		// alg=none：JWT 最经典的攻击。攻击者把算法声明成 none、去掉签名，
		// 如果服务端「尊重 token 自己声明的算法」，就会当成合法 token 接受。
		{"alg=none 无签名", signWith(t, "", jwt.SigningMethodNone, jwt.MapClaims{"sub": "1"})},
		// 缺 exp：jwt 库的默认行为是「没有 exp 就当永不过期」。
		// 那意味着任何一条漏签 exp 的代码路径都会造出一枚永久有效的 token，
		// 而且不会有任何报错。我们用 WithExpirationRequired 把它变成当场拒绝。
		{"没有 exp（永不过期的 token）", signWith(t, testSecret, jwt.SigningMethodHS256,
			jwt.MapClaims{"sub": "1", "iat": time.Now().Unix()})},
		{"已经过期", signWith(t, testSecret, jwt.SigningMethodHS256,
			jwt.MapClaims{"sub": "1", "exp": time.Now().Add(-time.Hour).Unix()})},
		{"sub 不是数字", signWith(t, testSecret, jwt.SigningMethodHS256,
			jwt.MapClaims{"sub": "admin", "exp": time.Now().Add(time.Hour).Unix()})},
		{"sub 缺失", signWith(t, testSecret, jwt.SigningMethodHS256,
			jwt.MapClaims{"exp": time.Now().Add(time.Hour).Unix()})},
		{"sub 是 0（我们约定 0 表示未登录）", signWith(t, testSecret, jwt.SigningMethodHS256,
			jwt.MapClaims{"sub": "0", "exp": time.Now().Add(time.Hour).Unix()})},
		{"sub 是负数", signWith(t, testSecret, jwt.SigningMethodHS256,
			jwt.MapClaims{"sub": "-1", "exp": time.Now().Add(time.Hour).Unix()})},
		{"sub 溢出 int64", signWith(t, testSecret, jwt.SigningMethodHS256,
			jwt.MapClaims{"sub": "99999999999999999999999", "exp": time.Now().Add(time.Hour).Unix()})},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			userID, err := s.Parse(tc.token)
			if err == nil {
				t.Fatalf("这个 token 应该被拒绝，但解析成功了，userID=%d", userID)
			}
			if userID != 0 {
				t.Errorf("拒绝时 userID 必须是 0，实际 %d", userID)
			}
			// 所有失败都必须是同一个码：§8 规定「缺 token / token 无效或过期」都是 UNAUTHORIZED。
			// 分成多个码等于告诉攻击者「你离成功还差哪一步」。
			if !apperr.IsCode(err, apperr.CodeUnauthorized) {
				t.Errorf("错误码应该是 %s，实际 %v", apperr.CodeUnauthorized, err)
			}
		})
	}
}

// flipLastChar 把最后一个字符换成别的，用于伪造一个签名段被篡改的 token。
func flipLastChar(tok string) string {
	last := tok[len(tok)-1]
	if last == 'a' {
		return "b"
	}
	return "a"
}

// TestParseErrorKeepsInternalCause 验证内部原因被保留下来供日志使用。
//
// §8 的规矩是「响应里只有通用文案，日志里有完整链路」。前半句由 apperr 的
// Respond 保证（它不会把 Err 写进响应体），后半句就靠这里 ——
// 如果 Parse 把原始错误丢掉，访问日志里就只剩一句「凭证无效」，
// 排查「用户说 token 明明没过期」这类问题时无从下手。
func TestParseErrorKeepsInternalCause(t *testing.T) {
	s := newTestSigner(t, time.Hour)

	expired := signWith(t, testSecret, jwt.SigningMethodHS256,
		jwt.MapClaims{"sub": "1", "exp": time.Now().Add(-time.Hour).Unix()})

	_, err := s.Parse(expired)
	if err == nil {
		t.Fatal("过期 token 应该被拒")
	}
	// 能用 errors.Is 穿透到 jwt 库的哨兵错误，说明原因没被吃掉
	if !errors.Is(err, jwt.ErrTokenExpired) {
		t.Errorf("过期错误应该能用 errors.Is(err, jwt.ErrTokenExpired) 检出，实际 %v", err)
	}

	// 但对外仍然只是 UNAUTHORIZED，不泄漏「过期」还是「伪造」
	if !apperr.IsCode(err, apperr.CodeUnauthorized) {
		t.Errorf("对外的码必须是 UNAUTHORIZED，实际 %v", err)
	}
}

func TestNewTokenSignerRejectsBadConfig(t *testing.T) {
	cases := []struct {
		name   string
		secret string
		ttl    time.Duration
	}{
		{"密钥为空", "", time.Hour},
		{"密钥太短（HS256 可被离线爆破）", "short", time.Hour},
		{"密钥刚好差一个字节", strings.Repeat("a", minSecretBytes-1), time.Hour},
		{"有效期为 0", testSecret, 0},
		{"有效期为负", testSecret, -time.Hour},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewTokenSigner(tc.secret, tc.ttl); err == nil {
				t.Error("这个配置应该在启动时就被拒绝，而不是等第一个用户登录时才炸")
			}
		})
	}

	// 边界：刚好够长必须通过，否则上面那条「差一个字节」的测试就只是在测一个过严的实现
	t.Run("密钥刚好达到下限", func(t *testing.T) {
		if _, err := NewTokenSigner(strings.Repeat("a", minSecretBytes), time.Hour); err != nil {
			t.Errorf("%d 字节的密钥应该被接受: %v", minSecretBytes, err)
		}
	})
}

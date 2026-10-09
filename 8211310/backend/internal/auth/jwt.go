package auth

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"lostfound/internal/apperr"
)

// 签名算法固定 HS256。
//
// 写死而不是从配置读：允许配置算法是 JWT 最经典的漏洞来源之一 ——
// 如果服务端接受 token 头部里声明的算法，攻击者就能把 alg 改成 none，
// 或者在「服务端用 RSA 公钥验签」的部署里把 alg 改成 HS256 然后用那个
// **公开的**公钥当 HMAC 密钥自己签一个。固定一个对称算法，这两条路都不存在。
// 用 var 而不是 const：jwt.SigningMethodHS256 是库里的一个变量（*SigningMethodHMAC），
// 不是编译期常量。它只在包初始化时赋值一次，之后只读，所以 var 在这里没有可变性风险。
var signingMethod = jwt.SigningMethodHS256

// minSecretBytes 是 HMAC 密钥的长度下限。
//
// 这不是洁癖：HS256 的密钥是可以离线爆破的 —— 攻击者拿到任意一个 token 之后，
// 在自己机器上不限速地试密钥，试出来就能伪造**任何人**的 token，包括 admin。
// 而 .env.example 推荐的生成方式 `openssl rand -hex 32` 出来是 64 个字符，
// 所以下限定在 32 字节既拦得住「随手写个 abc123」，又不会和推荐做法冲突。
const minSecretBytes = 32

// TokenSigner 签发和校验本系统自己的 JWT。
//
// 值类型、无状态、可并发使用 —— 里面只有密钥和有效期两个只读字段。
type TokenSigner struct {
	secret []byte
	ttl    time.Duration
}

// Token 是签发结果。ExpiresAt 要返回给前端（#2 的 expires_at 字段），
// 让它知道什么时候该重新登录，而不是等拿到 401 才反应过来。
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// NewTokenSigner 造一个签发器。secret 太短或 ttl 非正都直接返回 error ——
// 这两件事在启动时就能确定，绝不该拖到第一个用户登录时才炸。
func NewTokenSigner(secret string, ttl time.Duration) (TokenSigner, error) {
	if len(secret) < minSecretBytes {
		return TokenSigner{}, fmt.Errorf(
			"auth.NewTokenSigner: JWT_SECRET 至少 %d 字节，当前 %d 字节（生成一个：openssl rand -hex 32）",
			minSecretBytes, len(secret))
	}
	if ttl <= 0 {
		return TokenSigner{}, fmt.Errorf("auth.NewTokenSigner: token 有效期必须是正数，当前 %s", ttl)
	}
	return TokenSigner{secret: []byte(secret), ttl: ttl}, nil
}

// TTL 返回配置的有效期。handler 要用它算 expires_at。
func (s TokenSigner) TTL() time.Duration { return s.ttl }

// Issue 给一个用户 id 签发 token。
//
// ⚠ claims 里**只放 sub 和两个时间戳**，刻意不放 role、不放 status、不放 username。
//
// 原因：JWT 是无状态的，签出去就改不了，只能等它过期（默认 24 小时）。
// 把 role 放进去意味着 admin 给某人降权之后，那人手上的旧 token 里仍然写着
// role=admin，还能继续调管理接口，最长 24 小时。封号同理 —— 而封号的整个意义
// 就是「立刻生效」。
//
// 所以 middleware.JWT 每次请求都拿 sub 去库里读一遍 role 和 status。
// 代价是每个已登录请求多一次主键查询（有索引，微秒级），换来的是治理动作即时生效。
// 对一个校园失物招领系统，这个交换毫无疑问是划算的。
func (s TokenSigner) Issue(userID int64) (Token, error) {
	now := time.Now()
	expiresAt := now.Add(s.ttl)

	claims := jwt.RegisteredClaims{
		Subject:   strconv.FormatInt(userID, 10),
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(expiresAt),
	}

	raw, err := jwt.NewWithClaims(signingMethod, claims).SignedString(s.secret)
	if err != nil {
		// HS256 对一个 []byte 密钥不会失败；真失败了说明密钥类型或库出了问题，
		// 属于 INTERNAL，不该让用户看到细节。
		return Token{}, fmt.Errorf("auth.TokenSigner.Issue(%d): %w", userID, err)
	}
	return Token{Value: raw, ExpiresAt: expiresAt}, nil
}

// Parse 校验 token 并取出用户 id。任何不合法都返回 UNAUTHORIZED。
//
// 三个 parser 选项每一个都对应一类真实攻击，不是防御性摆设：
//
//   - WithValidMethods：只接受 HS256。挡掉 alg=none 和算法混淆（见 signingMethod 注释）。
//   - WithExpirationRequired：**没有 exp 的 token 一律拒绝**。默认行为是「没有 exp 就当作
//     永不过期」，于是任何一个漏签 exp 的代码路径都会造出一枚永久有效的 token。
//     要求必须有，就把这个失败模式从「静默永久有效」变成「当场拒绝」。
//   - 校验签名：jwt.Parse 本身就会做，签名不对直接返回错误。
//
// 返回的 error 一律是 apperr UNAUTHORIZED，但 message 区分「过期」和「无效」——
// 前端按 code 分支（两种都是 401，都要跳登录页），message 只用来给用户看，
// 而「登录已过期」比「凭证无效」友好得多。这个区分不泄漏任何有用信息：
// 能拿到「过期」这个答复的人，本来就持有一个曾经有效的 token。
func (s TokenSigner) Parse(raw string) (int64, error) {
	if raw == "" {
		return 0, apperr.NewMsg(apperr.CodeUnauthorized, "缺少登录凭证")
	}

	parsed, err := jwt.Parse(raw,
		func(t *jwt.Token) (any, error) { return s.secret, nil },
		jwt.WithValidMethods([]string{signingMethod.Alg()}),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		// 过期是最常见的一种，单独给文案；其余（签名不对、格式不对、已失效）合并。
		if errors.Is(err, jwt.ErrTokenExpired) {
			return 0, apperr.WrapMsg(err, apperr.CodeUnauthorized, "登录已过期，请重新登录")
		}
		return 0, apperr.WrapMsg(err, apperr.CodeUnauthorized, "登录凭证无效，请重新登录")
	}

	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok || !parsed.Valid {
		return 0, apperr.NewMsg(apperr.CodeUnauthorized, "登录凭证无效，请重新登录")
	}

	// sub 是我们自己签的数字字符串。走到「格式不对」这个分支只有一种可能：
	// 有人用同一个密钥签了别的东西，或者代码里某处签错了。属于配置/代码错误，
	// 但对用户仍然只能表现为 401 —— 内部原因进日志（Wrap 的 Err 会被访问日志打出来）。
	sub, err := claims.GetSubject()
	if err != nil || sub == "" {
		return 0, apperr.WrapMsg(err, apperr.CodeUnauthorized, "登录凭证无效，请重新登录")
	}
	userID, err := strconv.ParseInt(sub, 10, 64)
	if err != nil {
		return 0, apperr.WrapMsg(err, apperr.CodeUnauthorized, "登录凭证无效，请重新登录")
	}
	if userID <= 0 {
		return 0, apperr.NewMsg(apperr.CodeUnauthorized, "登录凭证无效，请重新登录")
	}
	return userID, nil
}

package auth

import (
	"fmt"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"lostfound/internal/apperr"
)

// 密码策略。计划 §8 对 WEAK_PASSWORD 的定义只有两条：「< 8 位」或「纯数字」。
// 这里就只实现这两条，不自作聪明加复杂度要求 —— 这是一个靠自觉维持的校园平台
// （定位原则 1 的同一套价值观），把密码规则定得比银行还严只会逼人写在便利贴上。
const (
	// minPasswordChars 是**字符数**下限，不是字节数。
	// 用字节数的话「密码密码密码密码」是 24 字节却只有 8 个字，
	// 而用户脑子里数的是「我输了 8 个字」。按用户的计数方式来，规则才可解释。
	minPasswordChars = 8

	// maxPasswordBytes 是 bcrypt 的硬上限，不是我们自己定的策略。
	//
	// bcrypt 只取输入的前 72 字节，**超出部分静默丢弃**。后果是
	// "aaaa...(72个a)" 和 "aaaa...(72个a)bbb" 哈希完全相同 ——
	// 用户设了一个更长的密码，系统却只保护了前 72 字节，而且没有任何提示。
	// 显式拒绝比静默截断诚实：宁可让用户知道有上限，也不要让他以为更长就更安全。
	maxPasswordBytes = 72
)

// ValidatePassword 校验密码强度，不合格返回 apperr（WEAK_PASSWORD 或 VALIDATION）。
//
// ⚠ 这个函数**绝不会 trim 密码**。
// 项目里别处大量使用 strings.TrimSpace（M0 那三个 btrim 约束就是为了对齐它），
// 所以很容易顺手在这里也加一个 —— 那是错的：密码的首尾空格是密码本身的一部分，
// trim 掉会让「设密码时带了空格」和「登录时没带」变成两个不同的密码，
// 用户永远登不上，而且从现象上完全看不出原因。
func ValidatePassword(pw string) error {
	if len(pw) > maxPasswordBytes {
		return apperr.Validation(
			fmt.Sprintf("密码太长（最多 %d 字节）", maxPasswordBytes),
			apperr.FieldError{Field: "password", Msg: "密码最长 72 字节，这是 bcrypt 算法的上限"})
	}

	// 空密码单独给 VALIDATION 而不是 WEAK_PASSWORD：
	// 它是「你没填」，不是「你填了个弱的」，前端提示文案应该不一样。
	if pw == "" {
		return apperr.Validation("请填写密码",
			apperr.FieldError{Field: "password", Msg: "密码不能为空"})
	}

	if n := utf8.RuneCountInString(pw); n < minPasswordChars {
		return apperr.NewMsg(apperr.CodeWeakPassword,
			fmt.Sprintf("密码至少 %d 位，当前 %d 位", minPasswordChars, n)).
			WithField("password", fmt.Sprintf("至少 %d 位", minPasswordChars))
	}

	if isAllDigits(pw) {
		return apperr.NewMsg(apperr.CodeWeakPassword, "密码不能是纯数字").
			WithField("password", "不能是纯数字，加点字母或符号")
	}

	return nil
}

// isAllDigits 报告字符串是否全由数字组成。
//
// 用 unicode.IsDigit 而不是 `r >= '0' && r <= '9'`：前者覆盖 Unicode 的 Nd 类，
// 包含全角数字 ０–９（U+FF10–U+FF19）。中文输入法下敲数字很可能出来全角，
// 而「１２３４５６７８」和「12345678」一样弱，不该因为写法不同就绕过规则。
// 这和 M0 里 U+3000 全角空格绕过 btrim 是同一类坑：只考虑 ASCII 就会漏。
//
// 空串返回 false（由上面的空密码分支单独处理），避免「所有字符都满足」对空集为真。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// HashPassword 生成 bcrypt 哈希。
//
// 用 bcrypt.DefaultCost（当前是 10）。不自己挑 cost：这个数字要随硬件进步往上调，
// 由库维护比自己写死一个魔数靠谱；而且 10 在登录这种低频路径上大约几十毫秒，
// 慢正是它的设计目的（拖慢暴力破解），不是性能问题。
//
// 哈希结果形如 $2a$10$....，60 个字符，users.password_hash 是 VARCHAR(100)，够用且有余量。
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		// bcrypt 只在输入超过 72 字节时报错，而 ValidatePassword 已经拦在前面了。
		// 仍然包一层：调用顺序万一被人改动，这里会给出能看懂的原因而不是一个裸 error。
		return "", fmt.Errorf("auth.HashPassword: %w", err)
	}
	return string(b), nil
}

// CheckPassword 比对明文和哈希。
//
// 返回 bool 而不是 error：bcrypt 的 error 只有「哈希格式不合法」和「不匹配」两种，
// 对调用方来说都是「密码不对」，区分它们没有意义（而且区分了还可能泄漏
// 「这个账号存的哈希是坏的」这种内部状态）。格式不合法会记进日志，由调用方决定。
func CheckPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

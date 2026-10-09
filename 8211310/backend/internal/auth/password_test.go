package auth

import (
	"errors"
	"strings"
	"testing"

	"lostfound/internal/apperr"
)

// 全角数字 ０–９（U+FF10–U+FF19）。
//
// 用 string(rune(0xFF10)) 拼而不是直接把字符写进源码：M0 踩过一次坑 ——
// 编辑器和各种「格式化工具」会静默改写非 ASCII 的不可见/易混字符，
// 而改完之后测试仍然绿，只是测的不再是原来那个字符。
// rune 构造全是 ASCII，怎么折腾都不会坏。
func fullWidthDigits(n int) string {
	return strings.Repeat(string(rune(0xFF10)), n)
}

func TestValidatePassword(t *testing.T) {
	// 期望的 code 用空串表示「应该通过」。
	// 只断言 code，不断言 message —— §8 明确规定 message 随时可改，
	// 依赖文案的测试会在一次无害的措辞调整后集体变红，然后就没人信测试了。
	cases := []struct {
		name     string
		password string
		wantCode string
	}{
		// ---- 应该通过 ----
		{"8 个字母", "abcdefgh", ""},
		{"8 位字母数字混合", "abcd1234", ""},
		{"7 位数字加 1 个字母", "1234567a", ""},
		{"常见的够强密码", "correct-horse-battery", ""},
		{"8 个汉字（24 字节，但按字符数算是 8 位）", "密码密码密码密码", ""},
		{"带空格，长度按字符数算", "my pass word", ""},
		{"恰好 72 字节，bcrypt 的上限", strings.Repeat("a", 72), ""},

		// ---- 太短 ----
		{"空密码走 VALIDATION 而不是 WEAK_PASSWORD", "", apperr.CodeValidation},
		{"7 个字母", "abcdefg", apperr.CodeWeakPassword},
		{"1 个字符", "a", apperr.CodeWeakPassword},
		{"6 个汉字（按字符数只有 6 位）", "密码密码密码", apperr.CodeWeakPassword},

		// ---- 纯数字 ----
		{"8 位纯数字", "12345678", apperr.CodeWeakPassword},
		{"20 位纯数字，长也拦", "12345678901234567890", apperr.CodeWeakPassword},
		// 这条是 M0 那个 U+3000 教训的直系后代：只判 ASCII '0'-'9' 的话，
		// 中文输入法下敲出来的全角数字能直接绕过「纯数字」规则。
		{"8 位全角数字，同样算纯数字", fullWidthDigits(8), apperr.CodeWeakPassword},

		// ---- 太长 ----
		{"73 字节超出 bcrypt 上限", strings.Repeat("a", 73), apperr.CodeValidation},

		// ---- 策略边界，刻意记录在案 ----
		//
		// 8 个空格能通过校验。这不是漏掉了，是计划 §8 对 WEAK_PASSWORD 的定义
		// 就只有「< 8 位」和「纯数字」两条，空格既不是数字也够长。
		// 把它写成一条测试是为了让这个边界**显式**：将来谁想收紧策略
		// （比如加一条「不能全是空白」），会看到这条测试并主动去改计划，
		// 而不是悄悄加个规则让文档和代码对不上。
		{"8 个空格（计划 §8 的两条规则都拦不住它）", strings.Repeat(" ", 8), ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.password)

			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("密码 %q 应该通过，实际被拒: %v", tc.password, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("密码 %q 应该被拒（%s），实际通过了", tc.password, tc.wantCode)
			}
			if !apperr.IsCode(err, tc.wantCode) {
				t.Errorf("密码 %q 的错误码不对：期望 %s，实际 %v", tc.password, tc.wantCode, err)
			}
		})
	}
}

// TestValidatePasswordRejectsCarryFieldDetail 校验错误里带了字段名。
//
// 前端要靠 data.errors[].field 把焦点定位到密码输入框（§8）。
// 这件事值得单独测：它是纯 UX 契约，漏了不会有任何功能异常，
// 只会在前端表现为「报错了但不知道是哪个框」，而这种问题通常要等到联调才发现。
func TestValidatePasswordRejectsCarryFieldDetail(t *testing.T) {
	err := ValidatePassword("12345678")
	if err == nil {
		t.Fatal("纯数字密码应该被拒")
	}

	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("期望 *apperr.Error，实际 %T", err)
	}
	if len(ae.Fields) == 0 {
		t.Fatal("WEAK_PASSWORD 应该带字段级细节，否则前端无法定位到密码框")
	}
	if ae.Fields[0].Field != "password" {
		t.Errorf("字段名应该是 password，实际 %q", ae.Fields[0].Field)
	}
}

// TestPasswordIsNeverTrimmed 钉住「密码不 trim」这条规则。
//
// 这个项目里到处都在 trim（M0 那三个 btrim 约束、config 的 getEnv、
// requestid 的 sanitize），所以「顺手在密码上也 trim 一下」是一个
// 极容易犯、且后果极难查的错：用户设密码时首尾带了空格，
// 注册时被 trim 掉存进库，登录时输入的原始串又被 trim 掉 ——
// 看起来一切正常，直到某个用密码管理器的用户永远登不上，
// 因为管理器忠实地回填了那个空格，而后端把它删了。
func TestPasswordIsNeverTrimmed(t *testing.T) {
	withSpaces := "  abcd1234  "

	if err := ValidatePassword(withSpaces); err != nil {
		t.Fatalf("首尾带空格的密码应该按原样接受（长度按 12 个字符算），实际被拒: %v", err)
	}

	hash, err := HashPassword(withSpaces)
	if err != nil {
		t.Fatalf("HashPassword 失败: %v", err)
	}

	if !CheckPassword(hash, withSpaces) {
		t.Error("用原样的密码（带空格）应该能验通过")
	}
	// 这两条才是真正的保护：如果实现里偷偷 trim 了，带空格和不带空格会变成同一个密码
	if CheckPassword(hash, strings.TrimSpace(withSpaces)) {
		t.Error("trim 之后的密码不应该验通过 —— 说明 HashPassword 或 CheckPassword 偷偷 trim 了")
	}
	if CheckPassword(hash, "abcd1234") {
		t.Error("去掉空格的密码不应该验通过")
	}
}

func TestCheckPasswordRejectsWrongPassword(t *testing.T) {
	hash, err := HashPassword("correct-horse-battery")
	if err != nil {
		t.Fatalf("HashPassword 失败: %v", err)
	}

	if !CheckPassword(hash, "correct-horse-battery") {
		t.Error("正确的密码应该验通过")
	}
	if CheckPassword(hash, "wrong-horse-battery") {
		t.Error("错误的密码不应该验通过")
	}
	if CheckPassword(hash, "") {
		t.Error("空密码不应该验通过")
	}
	// 哈希本身坏了（比如被人从 Adminer 里手改过）必须是「验不通过」而不是 panic。
	// 这条路径真实存在：计划 §3.4 明确说 admin 可以用裸 SQL 改库。
	if CheckPassword("not-a-bcrypt-hash", "correct-horse-battery") {
		t.Error("非法哈希不应该验通过")
	}
	if CheckPassword("", "correct-horse-battery") {
		t.Error("空哈希不应该验通过")
	}
}

// TestHashPasswordIsSalted 验证同一个密码两次哈希结果不同。
//
// bcrypt 自带随机盐，这是它相对 MD5/SHA 的核心优势之一：
// 两个用户用了同一个弱密码，库里的哈希也完全不同，
// 攻击者拿到一张彩虹表也没法批量反查。
func TestHashPasswordIsSalted(t *testing.T) {
	const pw = "same-password-123"

	h1, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("第一次哈希失败: %v", err)
	}
	h2, err := HashPassword(pw)
	if err != nil {
		t.Fatalf("第二次哈希失败: %v", err)
	}

	if h1 == h2 {
		t.Error("同一个密码两次哈希结果相同 —— 说明没有加盐")
	}
	if !CheckPassword(h1, pw) || !CheckPassword(h2, pw) {
		t.Error("两个不同的哈希都应该能验通过同一个密码")
	}
	if !strings.HasPrefix(h1, "$2") {
		t.Errorf("bcrypt 哈希应该以 $2a$/$2b$ 开头，实际 %q", h1[:min(len(h1), 7)])
	}
}

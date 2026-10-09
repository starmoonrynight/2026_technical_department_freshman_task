package middleware

import (
	"regexp"
	"strings"
	"testing"
)

// 这一层是计划 §10 的第①层：纯单元测试，不需要数据库、不需要起服务。
//
// sanitize 值得单独测，因为它是「日志注入」的唯一防线，而这条防线失效时
// 症状不是报错，是日志里多出一行你以为系统写过、其实没有的记录 —— 最难发现的那类 bug。

func TestSanitizeRequestID(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"空串返回空，让调用方去生成新的", "", ""},
		{"纯空白返回空", "   \t ", ""},
		{"合法 id 原样通过", "20261007-3f2a91c40b7de518", "20261007-3f2a91c40b7de518"},
		{"白名单里的点下划线保留", "a.b_c-d", "a.b_c-d"},
		{"换行符被删掉 —— 这是日志注入的主要手法", "abc\nFAKE-LOG-LINE", "abcFAKE-LOG-LINE"},
		{"回车符被删掉", "abc\rdef", "abcdef"},
		{"制表符被删掉", "abc\tdef", "abcdef"},
		{"标点和空格被删掉", "abc!!def@@ ghi", "abcdefghi"},
		{"中文被删掉（不在白名单里）", "abc请求def", "abcdef"},
		{"全是非法字符则返回空", "!!!@@@###", ""},
		{"超长被截断到 64 字节", strings.Repeat("a", 200), strings.Repeat("a", 64)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitize(tt.in); got != tt.want {
				t.Errorf("sanitize(%q) = %q，期望 %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeNeverLeaksControlChars(t *testing.T) {
	// 穷举 0x00–0x1f 和 0x7f：一个都不许留下。
	// 逐个字符试比构造几个「典型攻击串」可靠 —— 攻击者不会按我们的 imagination 出牌。
	for b := 0; b < 0x20; b++ {
		in := "prefix" + string(rune(b)) + "suffix"
		got := sanitize(in)
		if strings.ContainsRune(got, rune(b)) {
			t.Errorf("控制字符 0x%02x 穿透了 sanitize（输入 %q → 输出 %q）", b, in, got)
		}
	}
	if got := sanitize("prefix\x7fsuffix"); strings.ContainsRune(got, 0x7f) {
		t.Errorf("DEL(0x7f) 穿透了 sanitize（输出 %q）", got)
	}
}

// requestIDPattern 是 newRequestID 的输出形状：8 位日期 + 16 位十六进制。
var requestIDPattern = regexp.MustCompile(`^\d{8}-[0-9a-f]{16}$`)

func TestNewRequestIDShape(t *testing.T) {
	for i := 0; i < 100; i++ {
		id := newRequestID()
		if !requestIDPattern.MatchString(id) {
			t.Fatalf("newRequestID() = %q，不符合 %s", id, requestIDPattern)
		}
	}
}

func TestNewRequestIDIsUnique(t *testing.T) {
	// 只抽 200 个：8 字节随机数有 1.8e19 种，200 次抽样的碰撞概率约 1e-15，
	// 不会偶发红。抽太多反而是在测 crypto/rand，不是测我们的代码。
	const n = 200
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id := newRequestID()
		if seen[id] {
			t.Fatalf("第 %d 次生成的 request_id %q 和之前重复了 —— 两条请求的日志会混在一起", i, id)
		}
		seen[id] = true
	}
}

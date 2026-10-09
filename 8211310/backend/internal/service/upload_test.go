package service

import (
	"net/http"
	"strings"
	"testing"

	"lostfound/internal/apperr"
)

// 上传这条链路上真正有安全含义的判断只有两个：
//   - ClassifyUpload：这份字节能不能收、收下来存成什么扩展名
//   - IsUploadPath：发帖时带回来的路径是不是我们自己签发过的
//
// 两个都是纯函数，所以放在 §10 的第①层。真正的落盘（SaveImage）留给集成测试，
// 因为它的失败模式是文件系统层面的，用假的文件系统测等于没测。

func TestClassifyUploadAccepts(t *testing.T) {
	cases := []struct {
		sniffed string
		ext     string
	}{
		{"image/jpeg", ".jpg"},
		{"image/png", ".png"},
		{"image/webp", ".webp"},
		{"image/gif", ".gif"},
	}
	for _, tc := range cases {
		t.Run(tc.sniffed, func(t *testing.T) {
			got, err := ClassifyUpload(1024, tc.sniffed)
			if err != nil {
				t.Fatalf("不该报错：%v", err)
			}
			if got != tc.ext {
				t.Fatalf("扩展名 %q，期望 %q", got, tc.ext)
			}
		})
	}
}

// TestClassifyUploadRejectsType 钉住「只支持 jpg/png/webp/gif」（计划 §4 #6）。
//
// ⚠ 这里的输入必须是 **http.DetectContentType 的输出**，不是客户端声明的
// Content-Type。后者是请求头，想写什么写什么。
// 把 shell.php 改名成 photo.png 传上来：声明的类型是 image/png，
// 嗅探出来的是 text/plain（因为内容是脚本），于是被拒 —— 而且即使收下来，
// 存盘的扩展名也是**我们**根据嗅探结果决定的，磁盘上不会出现 .php。
func TestClassifyUploadRejectsType(t *testing.T) {
	unsupported := []string{
		"",
		"text/plain",
		"text/plain; charset=utf-8", // DetectContentType 对文本的真实输出
		"text/html; charset=utf-8",
		"application/octet-stream",
		"application/pdf",
		"image/svg+xml", // SVG 能内嵌 <script>，不在白名单里
		"image/bmp",     // 计划没列
		"image/tiff",    // 计划没列
		"IMAGE/PNG",     // MIME 是小写敏感的约定，大写不认
		"image/png ",    // 尾随空格不认（DetectContentType 不会产出，但白名单就该严格）
		"image/apng",    // APNG 的嗅探结果就是 image/png，这里只是确认别的字符串不放行
		"video/mp4",
		"application/x-php",
	}
	for _, s := range unsupported {
		t.Run("拒绝 "+s, func(t *testing.T) {
			_, err := ClassifyUpload(1024, s)
			if !apperr.IsCode(err, apperr.CodeFileTypeUnsupported) {
				t.Fatalf("期望 %s，实际 %v", apperr.CodeFileTypeUnsupported, err)
			}
		})
	}
}

func TestClassifyUploadSizeBoundary(t *testing.T) {
	cases := []struct {
		name string
		size int64
		ok   bool
	}{
		{"0 字节", 0, true},
		{"1 字节", 1, true},
		{"刚好 5MB", MaxUploadBytes, true},
		{"5MB + 1", MaxUploadBytes + 1, false},
		{"100MB", 100 << 20, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ClassifyUpload(tc.size, "image/png")
			if tc.ok {
				if err != nil {
					t.Fatalf("不该报错：%v", err)
				}
				return
			}
			if !apperr.IsCode(err, apperr.CodeFileTooLarge) {
				t.Fatalf("期望 %s，实际 %v", apperr.CodeFileTooLarge, err)
			}
		})
	}

	// 5MB 这个数字是计划 §4 #6 写死的，改了就是破坏契约。
	// 单独断言一次，免得有人在重构时把它「顺手」调成 10MB 而没人发现。
	if MaxUploadBytes != 5<<20 {
		t.Fatalf("MaxUploadBytes 应该是 5MB，实际 %d", MaxUploadBytes)
	}
}

// TestClassifyUploadSizeBeforeType 钉住两个检查的**先后顺序**。
//
// 一个 50MB 的 .txt：如果先查类型，用户收到的是「格式不支持」，
// 于是他换一个格式再传一次 50MB —— 而真正的问题从来都是大小。
// 先查大小能让「传了个巨大的错文件」这种最常见的失误一次就说清楚。
func TestClassifyUploadSizeBeforeType(t *testing.T) {
	_, err := ClassifyUpload(MaxUploadBytes+1, "text/plain")
	if !apperr.IsCode(err, apperr.CodeFileTooLarge) {
		t.Fatalf("超限 + 类型不对时应该先报 %s，实际 %v", apperr.CodeFileTooLarge, err)
	}
}

func TestMaxUploadBodyBytes(t *testing.T) {
	// body 上限必须**严格大于**图片上限，否则一个刚好 5MB 的图片
	// 加上 multipart 的边界和头部就会超过 body 上限 ——
	// 用户传了一张完全合规的图，收到的却是「请求体太大」。
	if MaxUploadBodyBytes() <= MaxUploadBytes {
		t.Fatalf("body 上限 %d 必须大于图片上限 %d", MaxUploadBodyBytes(), MaxUploadBytes)
	}
}

// ---------- IsUploadPath ----------

func TestIsUploadPath(t *testing.T) {
	valid := []string{
		"2026/10/0123456789abcdef0123456789abcdef.jpg",
		"2025/01/ffffffffffffffffffffffffffffffff.png",
		"2026/12/00000000000000000000000000000000.webp",
		"2099/09/abcdefabcdefabcdefabcdefabcdefab.gif",
	}
	for _, p := range valid {
		if !IsUploadPath(p) {
			t.Errorf("%q 应该被接受", p)
		}
	}

	invalid := []struct {
		name string
		path string
	}{
		{"空串", ""},
		{"路径穿越", "../../.env"},
		{"藏在合法前缀后的穿越", "2026/10/0123456789abcdef0123456789abcdef.jpg/../../.env"},
		{"绝对路径", "/etc/passwd"},
		{"Windows 反斜杠", "..\\..\\secret.txt"},
		{"盘符", "C:\\Windows\\win.ini"},
		{"HTML 注入", "<script>alert(1)</script>"},
		{"可执行扩展名", "2026/10/0123456789abcdef0123456789abcdef.php"},
		{"无扩展名", "2026/10/0123456789abcdef0123456789abcdef"},
		{"大写十六进制", "2026/10/0123456789ABCDEF0123456789ABCDEF.jpg"},
		{"31 位十六进制", "2026/10/0123456789abcdef0123456789abcde.jpg"},
		{"33 位十六进制", "2026/10/0123456789abcdef0123456789abcdefa.jpg"},
		{"月份一位数", "2026/1/0123456789abcdef0123456789abcdef.jpg"},
		{"年份三位", "026/10/0123456789abcdef0123456789abcdef.jpg"},
		{"没有日期目录", "0123456789abcdef0123456789abcdef.jpg"},
		{"前导斜杠", "/2026/10/0123456789abcdef0123456789abcdef.jpg"},
		{"尾随斜杠", "2026/10/0123456789abcdef0123456789abcdef.jpg/"},
		{"双扩展名", "2026/10/0123456789abcdef0123456789abcdef.jpg.php"},
		{"十六进制里混入 g", "2026/10/g123456789abcdef0123456789abcdeg.jpg"},
		// 换行这条值得单列：很多正则方言（PCRE 不带 /D）里 `$` 会匹配
		// 「结尾的换行符之前」，于是 "…jpg\n" 能通过校验，
		// 而这个字符串随后会被拼进 HTTP 头或 URL —— 那就是响应头注入。
		// Go 的 regexp 默认 `$` 就是 \z（文本结尾），这里把该行为钉死。
		{"尾随换行", "2026/10/0123456789abcdef0123456789abcdef.jpg\n"},
		{"内部换行", "2026/10/0123456789abcdef0123456789abcde\nf.jpg"},
		{"NUL 字节", "2026/10/0123456789abcdef0123456789abcdef.jpg\x00.png"},
		{"全角数字", "２０２６/１０/0123456789abcdef0123456789abcdef.jpg"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if IsUploadPath(tc.path) {
				t.Fatalf("%q 不该被接受（它会被原样写进 item_images.path 再拼成 /uploads/<path>）", tc.path)
			}
		})
	}
}

// TestIsUploadPathMatchesGeneratedShape 把「白名单正则」和「真实生成器」对在一起验。
//
// newPath 用的是 crypto/rand，形状是随机的，但它产出的每一个路径都必须能通过
// IsUploadPath。这两个东西分别写在文件的两端，改了一个忘了另一个的话：
//   - 正则太严 → 用户传了图、发帖时却被拒（#13 报 VALIDATION）
//   - 正则太松 → 防线形同虚设
//
// 所以这里直接跑生成器 200 次，逐个过白名单。
func TestIsUploadPathMatchesGeneratedShape(t *testing.T) {
	dir := t.TempDir()
	svc, err := NewUpload(dir, "/uploads", nil)
	if err != nil {
		t.Fatalf("NewUpload: %v", err)
	}

	for i := 0; i < 200; i++ {
		rel, abs, err := svc.newPath(".png")
		if err != nil {
			t.Fatalf("第 %d 次 newPath: %v", i, err)
		}
		if !IsUploadPath(rel) {
			t.Fatalf("newPath 产出的 %q 过不了 IsUploadPath（白名单和生成器走偏了）", rel)
		}
		if !strings.HasPrefix(abs, dir) {
			t.Errorf("绝对路径 %q 不在上传目录 %q 里面", abs, dir)
		}
		if svc.URL(rel) != "/uploads/"+rel {
			t.Errorf("URL(%q) = %q", rel, svc.URL(rel))
		}
	}
}

// TestDetectContentTypeOnRealSignatures 用**真实的文件头字节**验一遍嗅探结果。
//
// ClassifyUpload 的白名单是四个 MIME 字符串，而 DetectContentType 的输出长什么样
// 是标准库说了算。如果哪天它开始对 PNG 输出 "image/png; charset=binary"，
// 我们的 map 查找就会全部落空，而所有上传都会变成 FILE_TYPE_UNSUPPORTED ——
// 这条测试会在升级 Go 版本时立刻发现。
func TestDetectContentTypeOnRealSignatures(t *testing.T) {
	cases := []struct {
		name    string
		head    []byte
		want    string
		allowed bool // 我们的白名单收不收
	}{
		{"JPEG", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}, "image/jpeg", true},
		{"PNG", []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, "image/png", true},
		{"GIF87a", []byte("GIF87a...."), "image/gif", true},
		{"GIF89a", []byte("GIF89a...."), "image/gif", true},
		{"WEBP", append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 32)...), "image/webp", true},
		{"纯文本", []byte("hello world"), "text/plain; charset=utf-8", false},
		{"空", nil, "text/plain; charset=utf-8", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := http.DetectContentType(tc.head)
			if got != tc.want {
				t.Fatalf("DetectContentType = %q，期望 %q（标准库行为变了？白名单要跟着改）", got, tc.want)
			}
			_, err := ClassifyUpload(1, got)
			if tc.allowed && err != nil {
				t.Fatalf("图片内容不该被拒：%v", err)
			}
			if !tc.allowed && !apperr.IsCode(err, apperr.CodeFileTypeUnsupported) {
				t.Fatalf("非图片内容应该被拒成 %s，实际 %v", apperr.CodeFileTypeUnsupported, err)
			}
		})
	}
}

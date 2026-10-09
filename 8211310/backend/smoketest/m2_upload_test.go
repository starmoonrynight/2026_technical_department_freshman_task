package smoketest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lostfound/internal/apperr"
	"lostfound/internal/service"
)

// 本文件覆盖 #6 POST /api/uploads 和 #40 GET /uploads/*filepath。
//
// 这两个端点是配对的：#6 把字节写到磁盘并返回一个相对路径，#40 把那个路径
// 变回字节。中间还夹着 #13 发帖（把路径写进 item_images）和 #42 删图
// （把文件从磁盘上拿掉），那两半在 m2_items_test.go 里。
//
// 判据里写的是「上传假图片」，但这里的「假」只是指**不是真照片**：
// 字节内容是真的 PNG/JPEG 魔数，只是后面跟着填充。必须是真魔数 ——
// service 用 http.DetectContentType 嗅探内容而不信客户端声明的 Content-Type，
// 传一段 "hello" 上去会被正确地拒掉，那就测不到「存下去」这一半了。

// ---------- 假图片 ----------

// 这些函数字刻意只造「魔数 + 填充」，不造合法图片：
// DetectContentType 只看前 512 字节的签名，而 #40 的静态服务只按扩展名猜 Content-Type，
// 两边都不需要图片能真的解码。造合法 PNG 得写 IHDR/IDAT/CRC，
// 那几十行代码测的是「我会不会写 PNG」，不是「上传接口对不对」。

func fakePNG(extra int) []byte {
	return pad([]byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}, extra)
}

func fakeJPEG(extra int) []byte {
	return pad([]byte{0xFF, 0xD8, 0xFF, 0xE0}, extra)
}

func fakeGIF(extra int) []byte {
	return pad([]byte("GIF89a"), extra)
}

func fakeWEBP(extra int) []byte {
	// RIFF 的布局是 "RIFF" + 4 字节长度 + "WEBP" + 一个 chunk。
	// http.DetectContentType 认的是**最后一个 chunk 的 id**：只有 VP8 / VP8L / VP8X
	// 三种算 webp，少写这一段就会被判成 application/octet-stream。
	// 这不是 Go 的多此一举 —— 一个只有 RIFF+WEBP 头、后面没有 chunk 的文件
	// 确实是坏的，任何解码器都打不开它。
	return pad([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), extra)
}

func pad(magic []byte, extra int) []byte {
	out := make([]byte, 0, len(magic)+extra)
	out = append(out, magic...)
	for i := 0; i < extra; i++ {
		out = append(out, byte('a'+i%26))
	}
	return out
}

// ---------- #6 上传 ----------

// uploadResult 是 #6 的 data：{path, url}。
type uploadResult struct {
	Path string `json:"path"`
	URL  string `json:"url"`
}

// upload 是测试里最常用的那一步：传一张假 PNG，拿回 path。
func upload(t *testing.T, s Session, extra int) uploadResult {
	t.Helper()
	r := harness.PostMultipart(t, "/api/uploads", "file", "photo.png", fakePNG(extra), s.Token)
	RequireOK(t, r, "POST /api/uploads")
	var res uploadResult
	r.DataInto(t, &res)
	return res
}

func TestM2UploadHappyPath(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "uploader", "correct-horse-battery")

	content := fakePNG(2048)
	r := harness.PostMultipart(t, "/api/uploads", "file", "photo.png", content, me.Token)
	RequireOK(t, r, "POST /api/uploads")
	if r.HTTPStatus != http.StatusOK {
		t.Errorf("#6 成功应该是 HTTP 200，实际 %d", r.HTTPStatus)
	}

	var res uploadResult
	r.DataInto(t, &res)

	// path 必须是我们自己签发的形状。这一条不是洁癖：#13 发帖时会拿
	// service.IsUploadPath 校验 image_paths，形状不对就发不了帖，
	// 两个端点各说各话的话，「上传成功但发不了帖」是最难查的一类 bug。
	if !service.IsUploadPath(res.Path) {
		t.Fatalf("返回的 path %q 不符合 IsUploadPath 的形状（YYYY/MM/<32位十六进制>.<扩展名>）", res.Path)
	}
	if res.URL != "/uploads/"+res.Path {
		t.Errorf("url 应该是 /uploads/%s，实际 %q", res.Path, res.URL)
	}
	if !strings.HasSuffix(res.Path, ".png") {
		t.Errorf("传的是 PNG 魔数，扩展名应该是 .png，实际 path=%q", res.Path)
	}

	// 目录是当前 UTC 年月。写死 2026/10 会在下个月变红，所以现算。
	wantDir := time.Now().UTC().Format("2006/01")
	if got := filepath.ToSlash(filepath.Dir(res.Path)); got != wantDir {
		t.Errorf("path 的日期目录应该是 %s（UTC 当前年月），实际 %s", wantDir, got)
	}

	// 磁盘上真的有这个文件，而且字节一模一样。
	abs := filepath.Join(harness.Cfg.UploadDir, filepath.FromSlash(res.Path))
	onDisk, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("上传说成功了，但读不到文件 %s: %v", abs, err)
	}
	if string(onDisk) != string(content) {
		t.Errorf("磁盘上的文件和传上去的不一样：期望 %d 字节，实际 %d 字节", len(content), len(onDisk))
	}
}

// TestM2UploadRequiresAuth 钉住 #6 的 Auth = JWT。
//
// 匿名上传等于给全网开一个免费图床：不用注册、不用发帖，
// 一个循环就能把磁盘填满，而且填进去的文件谁也不知道是谁的。
func TestM2UploadRequiresAuth(t *testing.T) {
	harness.TruncateAll(t)

	r := harness.PostMultipart(t, "/api/uploads", "file", "photo.png", fakePNG(64), "")
	RequireCode(t, r, apperr.CodeUnauthorized)
	if r.HTTPStatus != http.StatusUnauthorized {
		t.Errorf("匿名上传期望 HTTP 401，实际 %d", r.HTTPStatus)
	}
}

// TestM2UploadSniffsContentNotFilename 是 #6 最重要的一条安全断言。
//
// 存下去的扩展名由**我们**根据字节内容决定，客户端说什么完全不算数。
// 少了这条，磁盘上就会出现 .php / .html / .svg 这样的文件，
// 而 #40 是按扩展名猜 Content-Type 的 —— 一个 .html 会被当成 text/html 发出去，
// 浏览器就会执行它，那是一次挂在**我们自己域名下**的 XSS。
func TestM2UploadSniffsContentNotFilename(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "sniffer", "correct-horse-battery")

	cases := []struct {
		name     string
		filename string
		content  []byte
		wantExt  string
	}{
		{"PNG 内容叫 shell.php", "shell.php", fakePNG(128), ".png"},
		{"JPEG 内容叫 x.html", "x.html", fakeJPEG(128), ".jpg"},
		{"GIF 内容叫 x.svg", "x.svg", fakeGIF(128), ".gif"},
		{"WEBP 内容叫 x.txt", "x.txt", fakeWEBP(128), ".webp"},
		{"全大写扩展名", "PHOTO.PNG", fakePNG(128), ".png"},
		{"没有扩展名", "photo", fakeJPEG(128), ".jpg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := harness.PostMultipart(t, "/api/uploads", "file", tc.filename, tc.content, me.Token)
			RequireOK(t, r, "POST /api/uploads "+tc.filename)
			var res uploadResult
			r.DataInto(t, &res)
			if !strings.HasSuffix(res.Path, tc.wantExt) {
				t.Errorf("文件名 %q、内容魔数对应 %s，但存成了 %q", tc.filename, tc.wantExt, res.Path)
			}
			if !service.IsUploadPath(res.Path) {
				t.Errorf("存出来的 path %q 不合法", res.Path)
			}
		})
	}
}

// TestM2UploadRejectsNonImage 钉住 FILE_TYPE_UNSUPPORTED。
//
// 注意这些 case 的文件名都写着 .png —— 那正是「不能信文件名」的另一面：
// 一个叫 photo.png 的文本文件仍然是文本文件。
func TestM2UploadRejectsNonImage(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "rejecter", "correct-horse-battery")

	cases := []struct {
		name    string
		content []byte
	}{
		{"纯文本", []byte("hello world, this is not an image at all")},
		{"空文件", nil},
		{"HTML", []byte("<html><body><script>alert(1)</script></body></html>")},
		{"SVG（XML，能带脚本，所以不在白名单里）", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`)},
		{"ZIP", []byte("PK\x03\x04some zip content here")},
		{"ELF", []byte("\x7fELF\x02\x01\x01\x00binary")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := countFilesUnder(t, harness.Cfg.UploadDir)
			r := harness.PostMultipart(t, "/api/uploads", "file", "photo.png", tc.content, me.Token)
			RequireCode(t, r, apperr.CodeFileTypeUnsupported)
			if r.HTTPStatus != http.StatusUnsupportedMediaType {
				t.Errorf("期望 HTTP 415，实际 %d", r.HTTPStatus)
			}
			if after := countFilesUnder(t, harness.Cfg.UploadDir); after != before {
				t.Errorf("拒绝了但磁盘上多了文件：%d → %d", before, after)
			}
		})
	}
}

// TestM2UploadRejectsTooLarge 钉住 FILE_TOO_LARGE，并且要分清两条不同的路径。
//
// 5MB < 文件 ≤ 6MB：请求体能收完，是 ClassifyUpload 按 fh.Size 拒的。
// 文件 > 6MB：MaxBytesReader 在读的过程中就掐断了，是 translateMultipartErr
//
//	把 http.MaxBytesError 翻成 FILE_TOO_LARGE 的。
//
// 两条都必须是同一个码 —— 对用户来说「图太大」只有一种解释，
// 而前端只按 code 分支（它不会去读 message 里的措辞）。
func TestM2UploadRejectsTooLarge(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "toobig", "correct-horse-battery")

	const mb = 1 << 20

	cases := []struct {
		name string
		size int
	}{
		{"刚好超过 5MB", 5*mb + 16},
		{"超过 body 上限 6MB", 7 * mb},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := countFilesUnder(t, harness.Cfg.UploadDir)
			r := harness.PostMultipart(t, "/api/uploads", "file", "big.png", fakePNG(tc.size), me.Token)
			RequireCode(t, r, apperr.CodeFileTooLarge)
			if r.HTTPStatus != http.StatusRequestEntityTooLarge {
				t.Errorf("期望 HTTP 413，实际 %d", r.HTTPStatus)
			}
			if after := countFilesUnder(t, harness.Cfg.UploadDir); after != before {
				t.Errorf("拒绝了但磁盘上多了文件：%d → %d（半成品没清干净）", before, after)
			}
		})
	}

	// 边界另一侧：正好 5MB 必须成功。只测「太大被拒」不测「临界放行」的话，
	// 把上限改成 1KB 也能让上面两条全绿。
	t.Run("正好 5MB 放行", func(t *testing.T) {
		r := harness.PostMultipart(t, "/api/uploads", "file", "edge.png", fakePNG(5*mb-8), me.Token)
		RequireOK(t, r, "POST /api/uploads 5MB")
	})
}

// TestM2UploadMissingFileField 钉住表单字段名。
//
// 计划 §4 #6 写的字段名是 `file`。前端哪天写成了 `image`，
// 得到的必须是「没有收到图片，字段名是 file」而不是 INTERNAL ——
// 后者会让前端以为是服务端坏了，而其实是自己拼错了表单。
func TestM2UploadMissingFileField(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "fieldname", "correct-horse-battery")

	r := harness.PostMultipart(t, "/api/uploads", "image", "photo.png", fakePNG(64), me.Token)
	RequireCode(t, r, apperr.CodeValidation)

	var body struct {
		Errors []struct {
			Field string `json:"field"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(r.Data, &body); err == nil && len(body.Errors) > 0 {
		if body.Errors[0].Field != "file" {
			t.Errorf("字段级错误应该指向 \"file\"，实际指向 %q", body.Errors[0].Field)
		}
	}
}

// TestM2UploadPathsAreUnguessableAndUnique 钉住文件名的两个性质。
//
// 不重复：两次上传同样的字节得到两个不同的 path（否则第二张图会覆盖第一张，
//
//	而 O_EXCL 会让它直接报 INTERNAL —— 两种都不是「两张图都在」）。
//
// 不可猜：文件名是 crypto/rand 出来的 32 位十六进制。可猜的文件名意味着
//
//	任何人能枚举 /uploads/2026/10/00000001.jpg，把别人**还没发帖**的
//	图片翻出来看 —— 那些图里可能有学生证。
func TestM2UploadPathsAreUnguessableAndUnique(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "unique", "correct-horse-battery")

	content := fakePNG(512)
	seen := make(map[string]bool, 20)
	for i := 0; i < 20; i++ {
		r := harness.PostMultipart(t, "/api/uploads", "file", "same.png", content, me.Token)
		RequireOK(t, r, fmt.Sprintf("POST /api/uploads 第 %d 次", i+1))
		var res uploadResult
		r.DataInto(t, &res)

		if seen[res.Path] {
			t.Fatalf("第 %d 次上传拿到了重复的 path %q", i+1, res.Path)
		}
		seen[res.Path] = true

		base := filepath.Base(res.Path)
		hexPart := strings.TrimSuffix(base, filepath.Ext(base))
		if len(hexPart) != 32 {
			t.Errorf("文件名应该是 32 位十六进制（16 字节 crypto/rand），实际 %q", hexPart)
		}
		for _, c := range hexPart {
			if !strings.ContainsRune("0123456789abcdef", c) {
				t.Errorf("文件名 %q 里有非十六进制字符 %q", hexPart, c)
				break
			}
		}
	}
}

// ---------- #40 静态文件 ----------

// TestM2StaticServesUploadedFile 钉住「#6 返回的 url 真的能拿到那张图」。
//
// #40 是全部 50 个端点里唯一**不走 JSON 信封**的：它直接回图片二进制。
// 所以这里的断言不是 code==OK，而是「响应体逐字节等于传上去的东西」。
func TestM2StaticServesUploadedFile(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "statics", "correct-horse-battery")

	content := fakePNG(4096)
	r := harness.PostMultipart(t, "/api/uploads", "file", "photo.png", content, me.Token)
	RequireOK(t, r, "POST /api/uploads")
	var res uploadResult
	r.DataInto(t, &res)

	// #40 是公开的：未登录也要能看到图片。帖子的图片对所有人可见
	// （只有联系方式是锁着的），锁图片等于让广场上的帖子全是碎图。
	status, body, header := harness.GetRaw(t, res.URL, "")
	if status != http.StatusOK {
		t.Fatalf("GET %s 期望 200，实际 %d\n%s", res.URL, status, truncate(string(body)))
	}
	if string(body) != string(content) {
		t.Errorf("拿回来的字节和传上去的不一样：期望 %d 字节，实际 %d 字节", len(content), len(body))
	}
	if ct := header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type 应该是 image/png，实际 %q", ct)
	}
	if len(body) > 0 && body[0] == '{' {
		t.Error("#40 返回了 JSON —— 它不该走信封，直接回二进制")
	}
}

// TestM2StaticSupportsHead 钉住 HEAD 也可用。
//
// 这个 HEAD 不是我们注册的，是 gin 的 Static() 自动加的
// （router_test.go 里专门为它写了一条豁免）。它值得测是因为浏览器
// 和前端图片预加载确实会发 HEAD，而「GET 有 HEAD 没有」会让
// 前端在某些优化路径上拿到 404。
func TestM2StaticSupportsHead(t *testing.T) {
	harness.TruncateAll(t)
	me := harness.RegisterAndLogin(t, "header", "correct-horse-battery")
	res := upload(t, me, 2048)

	req, err := http.NewRequest(http.MethodHead, harness.Server.URL+res.URL, nil)
	if err != nil {
		t.Fatalf("构造 HEAD 请求失败: %v", err)
	}
	resp, err := harness.Server.Client().Do(req)
	if err != nil {
		t.Fatalf("HEAD %s 失败: %v", res.URL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("HEAD %s 期望 200，实际 %d", res.URL, resp.StatusCode)
	}
}

// TestM2StaticRejectsTraversal 钉住路径穿越。
//
// 用 URL 编码的 %2e%2e 而不是字面的 ../：Go 的 http.Client 会在发送前
// 把字面的 ../ 规范化掉，那样请求根本到不了服务端，测试就成了自欺欺人。
// 编码形式会原样出现在 URL.Path 里，交给 http.Dir 去挡 —— 它拒绝
// 任何清洗后逃出根目录的路径，这是标准库给的保证，不是我们自己写的。
func TestM2StaticRejectsTraversal(t *testing.T) {
	harness.TruncateAll(t)

	// 在上传目录**外面**放一个诱饵，它的内容是判断「有没有被读到」的唯一凭据。
	secret := filepath.Join(harness.Cfg.UploadDir, "..", "smoketest-secret.txt")
	const marker = "THIS-FILE-MUST-NEVER-BE-SERVED"
	if err := os.WriteFile(secret, []byte(marker), 0o600); err != nil {
		t.Fatalf("准备诱饵文件失败: %v", err)
	}
	defer os.Remove(secret)

	for _, path := range []string{
		"/uploads/%2e%2e/smoketest-secret.txt",
		"/uploads/..%2fsmoketest-secret.txt",
		"/uploads/%2e%2e%2f%2e%2e%2fetc%2fpasswd",
	} {
		status, body, _ := harness.GetRaw(t, path, "")
		if status == http.StatusOK {
			t.Errorf("GET %s 返回了 200 —— 穿越没被挡住\n%s", path, truncate(string(body)))
		}
		if strings.Contains(string(body), marker) {
			t.Errorf("GET %s 把上传目录外面的文件内容读出来了", path)
		}
	}
}

// TestM2StaticMissingFileIs404 钉住「不存在的图片」的响应形状。
//
// ⚠ 这里的结果和「#40 不走 JSON 信封」并不矛盾，而是 gin 的一个实现细节：
// Static() 生成的 handler 会先自己 fs.Open 一次，打不开就**把 handler 链换成
// engine.noRoute** 再返回 —— 于是落到了我们注册的 apperr.NoRoute 上，
// 得到的是一个规规矩矩的信封（code=NOT_FOUND）。
// 只有「文件真的存在」那条路径才由 http.FileServer 直接回二进制。
//
// 这个行为值得钉住是因为它同时是两件好事：
//  1. 全站只有一种 404 形状，前端那个 axios 拦截器不用为 /uploads 开特例；
//  2. 它反过来证明了 #40 的 404 不会泄漏目录列表（onlyFilesFS + 我们自己的 NoRoute）。
//
// 而它对用户没有坏处：<img> 的加载失败只看 HTTP 状态码，不会去解 body。
func TestM2StaticMissingFileIs404(t *testing.T) {
	harness.TruncateAll(t)

	// 形状合法但从来没被签发过的 path。用 IsUploadPath 认得的形状是为了
	// 让这次 404 一定发生在「路由匹配上了、文件不在」这一支，
	// 而不是「路径本身就不合法」那一支 —— 两者都会 404，但走的是不同的代码。
	missing := "/uploads/2026/10/0123456789abcdef0123456789abcdef.png"
	status, body, header := harness.GetRaw(t, missing, "")
	if status != http.StatusNotFound {
		t.Fatalf("不存在的图片期望 404，实际 %d", status)
	}
	if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("期望落到 JSON 信封（Content-Type: application/json），实际 %q", ct)
	}

	var env struct {
		Code      string `json:"code"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("404 响应不是合法 JSON: %v\n%s", err, truncate(string(body)))
	}
	if env.Code != apperr.CodeNotFound {
		t.Errorf("期望 code=%s，实际 %s", apperr.CodeNotFound, env.Code)
	}
	if env.RequestID == "" {
		t.Error("信封里应该有 request_id（排查一次「图片打不开」就靠它）")
	}
}

// ---------- 辅助 ----------

// countFilesUnder 数一个目录下（递归）有多少个文件。
//
// 用它的断言全都是「拒绝了就不该留下文件」。少了这道检查的话，
// 一个「先建文件、后校验」的实现会让上传目录里堆满 0 字节的垃圾，
// 而所有响应码都是对的、测试全绿。
func countFilesUnder(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("数 %s 下的文件失败: %v", root, err)
	}
	return n
}

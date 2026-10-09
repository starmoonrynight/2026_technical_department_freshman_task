package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"lostfound/internal/apperr"
)

// 上传的限制与识别规则。
const (
	// MaxUploadBytes 是单张图片的上限（5MB）。计划 §4 #6 写死的数字。
	MaxUploadBytes int64 = 5 << 20

	// maxUploadBodyBytes 是整个 multipart 请求体的上限。
	//
	// 比图片上限多留 1MB 是给 multipart 的边界、头部、以及「同时传了好几张但每张都不大」
	// 之外的表单字段留余量。它的作用是**在读取之前就把总流量掐死** ——
	// 没有它的话，一个客户端可以慢慢地推 10GB 过来，我们的进程会一直在那儿收。
	maxUploadBodyBytes int64 = MaxUploadBytes + (1 << 20)

	// sniffBytes 是 http.DetectContentType 需要的字节数（它的实现只看前 512 字节）。
	sniffBytes = 512
)

// MaxUploadBodyBytes 暴露给 handler 用 http.MaxBytesReader 掐流量。
func MaxUploadBodyBytes() int64 { return maxUploadBodyBytes }

// imageExts 是「嗅探出来的 MIME 类型 → 我们存的扩展名」。
//
// 只有这四种（计划 §4 #6：jpg/png/webp/gif）。map 里没有的类型一律 FILE_TYPE_UNSUPPORTED。
var imageExts = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

// uploadPathRe 是我们自己生成的上传路径的形状：YYYY/MM/<32位十六进制>.<扩展名>。
//
// 只查**形状**，不查日历合法性："2026/13/…" 能通过。这不是疏漏 —— 这个正则的
// 职责是「保证路径逃不出 uploads 目录、且不含任何需要转义的字符」，
// 而月份是不是 13 和这两件事无关。真去查日历就得解析日期，
// 代码变长、收益为零（没有人能从这种路径里得到任何东西）。
//
// 这个正则不只是「格式好看」，它是一道安全边界，见 IsUploadPath 的注释。
var uploadPathRe = regexp.MustCompile(`^\d{4}/\d{2}/[0-9a-f]{32}\.(jpg|png|webp|gif)$`)

// IsUploadPath 报告一个字符串是不是本服务自己签发过的上传路径。
//
// **为什么发帖时要校验 image_paths**：#13 的请求体里带的是一个字符串数组，
// 这些字符串会被原样写进 item_images.path，之后又被拼成 `/uploads/<path>` 返给前端。
// 如果不校验形状，用户就能塞 `"../../.env"` 或者 `"<script>"` 进来：
//   - 前者在 #40 那边会被 http.Dir 挡住（它拒绝带 .. 的路径），但**存进库的字符串本身**
//     仍然是一个指向 uploads 目录之外的路径，而删图时会拿它去 os.Remove
//   - 后者会被前端渲染成一个奇怪的 URL，具体后果取决于前端怎么拼
//
// 用白名单正则一次性解决：只有「日期目录 + 32 位十六进制 + 四个扩展名之一」能通过，
// 而这种形状只有 #6 会生成。校验通过就意味着「这个路径指向的文件一定是我们自己写下去的」，
// 后面所有拿它拼 URL、删文件的地方都不用再担心。
func IsUploadPath(p string) bool { return uploadPathRe.MatchString(p) }

// StoredImage 是 #6 POST /api/uploads 的 data：{path, url}。
//
// path 是相对 UPLOAD_DIR 的路径，前端**要留着它**：发帖时把它放进 image_paths 数组。
// url 是可以直接塞进 <img src> 的绝对路径。两个都返回是因为用途不同 ——
// 只给 url 的话，前端得自己把 /uploads/ 前缀砍掉才能得到 path，
// 而那等于让前端知道我们的目录布局。
type StoredImage struct {
	Path string `json:"path"`
	URL  string `json:"url"`
}

// Upload 负责「把字节写到磁盘」和「把相对路径拼成 URL」两件事。
type Upload struct {
	dir     string // 绝对路径，构造函数里就转好
	baseURL string // 形如 /uploads，末尾不带斜杠
	logger  *slog.Logger
}

// NewUpload 检查并创建上传目录。
//
// 目录不存在就建，而不是等到第一次上传失败：第一次上传失败时用户看到的是一张
// 传不上去的图片，而原因（目录没建）藏在服务器日志里 —— 启动时建好，
// 建不出来就直接 fatal，问题在部署那一刻暴露，不在用户点上传那一刻。
//
// 转成绝对路径同理：UPLOAD_DIR 默认是 ./uploads，相对的是**进程的工作目录**。
// 从 backend/ 下跑和从仓库根跑是两个不同的目录，而图片会静默地写到另一个地方去，
// 表现是「上传成功了但图片 404」。存绝对路径之后，日志里打出来的就是真实位置。
func NewUpload(dir, baseURL string, logger *slog.Logger) (*Upload, error) {
	if logger == nil {
		logger = slog.Default()
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("service.NewUpload: UPLOAD_DIR %q 转绝对路径失败: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("service.NewUpload: 创建上传目录 %s 失败: %w", abs, err)
	}

	return &Upload{
		dir:     abs,
		baseURL: strings.TrimRight(baseURL, "/"),
		logger:  logger,
	}, nil
}

// Dir 返回上传目录的绝对路径。#40 的静态文件路由和冒烟测试都要用它。
func (s *Upload) Dir() string { return s.dir }

// URL 把相对路径拼成对外 URL。空串进空串出 —— 没有封面图的帖子不该得到一个
// 只有前缀的 "/uploads"，那在前端会渲染成一次注定 404 的请求。
func (s *Upload) URL(path string) string {
	if path == "" {
		return ""
	}
	return s.baseURL + "/" + strings.TrimLeft(path, "/")
}

// AbsPath 把相对路径翻成磁盘上的绝对路径。
//
// ⚠ 只在「路径已经过 IsUploadPath 校验」之后调用。它自己不做穿越检查 ——
// 检查统一放在 IsUploadPath 那一处，两个地方各查一遍的结果是
// 谁也说不清到底哪个在负责，而漏掉的那条路径就成了洞。
func (s *Upload) AbsPath(path string) string {
	// filepath.FromSlash：库里存的一律是斜杠（跨平台一致，Windows 上也是），
	// 拼到本地文件系统时要换成分隔符。
	return filepath.Join(s.dir, filepath.FromSlash(path))
}

// ClassifyUpload 是「这份字节能不能收」的纯判断。
//
// 单独抽成函数是为了让它进 §10 的第①层：给定大小和嗅探结果，断言返回的扩展名或错误码。
// 这条判断是整个上传功能里唯一有安全含义的部分，而它不需要文件系统也不需要 HTTP。
//
// sniffed 必须是 http.DetectContentType 的输出，**不能是客户端声明的 Content-Type** ——
// 后者是请求头，想写什么写什么。把 photo.png 改名成 shell.php 传上来，
// 声明的类型是 image/png，嗅探出来的也是 image/png（我们只看内容，不看扩展名），
// 而最终存下去的扩展名由**我们**决定，所以磁盘上不会出现 .php。
func ClassifyUpload(size int64, sniffed string) (string, error) {
	if size > MaxUploadBytes {
		return "", apperr.NewMsg(apperr.CodeFileTooLarge,
			fmt.Sprintf("图片不能超过 %dMB", MaxUploadBytes>>20))
	}
	ext, ok := imageExts[sniffed]
	if !ok {
		return "", apperr.NewMsg(apperr.CodeFileTypeUnsupported,
			"只支持 jpg / png / webp / gif 格式的图片")
	}
	return ext, nil
}

// SaveImage 把一张图片落到磁盘，返回它的相对路径和 URL。
//
// src 必须是**可重读**的：前 512 字节要先读出来做类型嗅探，然后倒回去再整体写盘。
// multipart.File 天然实现 io.ReadSeeker，所以 handler 直接把它传进来就行。
func (s *Upload) SaveImage(ctx context.Context, src io.ReadSeeker, size int64) (StoredImage, error) {
	head := make([]byte, sniffBytes)
	// 用 ReadFull 而不是 Read：Read 允许「读了一部分就返回」，
	// 那样一张 3KB 的图可能只嗅探到前 1 字节，DetectContentType 会给出 text/plain，
	// 用户得到的是一句「格式不支持」而他传的明明是一张正常的 PNG。
	// 文件比 512 字节还小时 ReadFull 返回 ErrUnexpectedEOF，那是正常的，忽略即可。
	n, err := io.ReadFull(src, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return StoredImage{}, apperr.Internal(fmt.Errorf("service.Upload.SaveImage 读头部: %w", err))
	}

	ext, err := ClassifyUpload(size, http.DetectContentType(head[:n]))
	if err != nil {
		return StoredImage{}, err
	}

	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return StoredImage{}, apperr.Internal(fmt.Errorf("service.Upload.SaveImage 回卷: %w", err))
	}

	relPath, absPath, err := s.newPath(ext)
	if err != nil {
		return StoredImage{}, err
	}

	// O_EXCL：文件已存在就报错而不是覆盖。文件名是 16 字节随机数，撞上的概率可以忽略，
	// 但「忽略」和「静默覆盖掉别人的一张图」是两件事 —— 加一个标志位就把后者变成不可能。
	f, err := os.OpenFile(absPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return StoredImage{}, apperr.Internal(fmt.Errorf("service.Upload.SaveImage 建文件 %s: %w", absPath, err))
	}

	// LimitReader 是第二道大小防线：size 来自 multipart 头，虽然实测是准的，
	// 但「往磁盘上写用户给的字节」这条路上多一道硬上限没有坏处 ——
	// 多读一个字节就说明申报的大小是假的，立刻删掉半成品并返回 FILE_TOO_LARGE。
	written, err := io.Copy(f, io.LimitReader(src, MaxUploadBytes+1))
	closeErr := f.Close()
	if err != nil {
		_ = os.Remove(absPath)
		return StoredImage{}, apperr.Internal(fmt.Errorf("service.Upload.SaveImage 写盘: %w", err))
	}
	if closeErr != nil {
		// Close 的错误必须查：写盘是有缓冲的，磁盘满了往往到 Close 才报出来。
		// 忽略它的话会得到一个「大小不对的半截图片文件」，而响应是 200。
		_ = os.Remove(absPath)
		return StoredImage{}, apperr.Internal(fmt.Errorf("service.Upload.SaveImage 关闭文件: %w", closeErr))
	}
	if written > MaxUploadBytes {
		_ = os.Remove(absPath)
		return StoredImage{}, apperr.NewMsg(apperr.CodeFileTooLarge,
			fmt.Sprintf("图片不能超过 %dMB", MaxUploadBytes>>20))
	}

	s.logger.InfoContext(ctx, "upload.saved",
		slog.String("path", relPath),
		slog.Int64("bytes", written))

	return StoredImage{Path: relPath, URL: s.URL(relPath)}, nil
}

// Remove 删掉一个磁盘文件。文件本来就不存在**不算错误**。
//
// 返回 error 而不是内部吞掉：调用方（#42 删图）要决定「数据库那行已经删了，
// 文件删不掉怎么办」。目前的答案是记一条 WARN 然后当成功 —— 因为库里那行没了之后
// 这个文件对任何人都不可见了，留在磁盘上只是占地方，为一个已经不可见的文件
// 把整个删图请求报成失败，用户只会再点一次。
func (s *Upload) Remove(path string) error {
	err := os.Remove(s.AbsPath(path))
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// newPath 生成一个不重复的相对路径，并把它所在的年月目录建出来。
func (s *Upload) newPath(ext string) (rel, abs string, err error) {
	// crypto/rand 而不是 math/rand：文件名要不可猜。
	// 用可猜的文件名意味着任何人能枚举出 /uploads/2026/10/00000001.jpg 这样的 URL，
	// 把别人上传的、还没发帖的图片翻出来看 —— 那些图里可能有学生证。
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", "", apperr.Internal(fmt.Errorf("service.Upload.newPath 取随机数: %w", err))
	}

	dir := time.Now().UTC().Format("2006/01")
	if err := os.MkdirAll(filepath.Join(s.dir, filepath.FromSlash(dir)), 0o755); err != nil {
		return "", "", apperr.Internal(fmt.Errorf("service.Upload.newPath 建目录 %s: %w", dir, err))
	}

	rel = dir + "/" + hex.EncodeToString(buf) + ext
	return rel, filepath.Join(s.dir, filepath.FromSlash(rel)), nil
}

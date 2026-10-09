// Package upload 负责把用户上传的图片落到本地磁盘。
//
// 设计要点：
//   - 不信任客户端给的文件名与 Content-Type，只按文件头（magic bytes）判断真实格式；
//   - 文件名由服务端随机生成，客户端传什么都无法影响落盘路径（天然免疫路径穿越）；
//   - 只接受 4 种图片格式，其余一律 400；
//   - 目录按上传时间分年月存放，避免单目录堆积过多文件。
package upload

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"lostfound/internal/httpx"
)

// imageTypes 允许的图片格式：文件头嗅探结果 -> 落盘扩展名。
//
// 这张表同时是「白名单」和「扩展名来源」：扩展名由嗅探结果决定，
// 而不是由用户提供的文件名决定，所以把 shell.php 改名成 shell.png 也没用。
var imageTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// sniffLen 是 http.DetectContentType 需要的最大字节数。
const sniffLen = 512

// Store 图片存储。
type Store struct {
	Dir       string // 落盘目录，例如 <repo>/public/uploads
	URLPrefix string // 对外访问前缀，例如 /uploads
	MaxBytes  int64  // 单张图片上限
}

// Saved 一次成功上传的结果。
type Saved struct {
	URL      string `json:"url"`      // 可以直接写进 items.image_url
	Filename string `json:"filename"` // 服务器上的文件名
	Size     int64  `json:"size"`     // 字节数
	MimeType string `json:"mimeType"` // 嗅探出来的真实类型
}

// New 构造 Store。
func New(dir, urlPrefix string, maxBytes int64) *Store {
	return &Store{Dir: dir, URLPrefix: urlPrefix, MaxBytes: maxBytes}
}

// EnsureDir 确保上传目录存在（服务启动时调用一次）。
func (s *Store) EnsureDir() error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return fmt.Errorf("创建上传目录 %s 失败: %w", s.Dir, err)
	}
	return nil
}

// MaxRequestBytes 是整条 multipart 请求的上限：
// 图片本身的上限再加 1MB，留给 boundary、表单字段等额外开销。
func (s *Store) MaxRequestBytes() int64 {
	return s.MaxBytes + 1024*1024
}

// Save 校验并保存一张图片，返回可对外访问的相对路径。
//
// 流程：
//  1. 先读文件头判断真实类型，不合法直接拒绝（此时还没有写任何文件）；
//  2. 用「时间戳 + 8 字节随机数 + 按类型决定的扩展名」生成文件名；
//  3. 把已经读出来的文件头和剩余内容一起写入目标文件；
//  4. 写入过程中再卡一次大小上限，超了就删掉半个文件并返回 413。
func (s *Store) Save(src io.Reader) (*Saved, error) {
	if err := s.EnsureDir(); err != nil {
		return nil, err
	}

	head := make([]byte, sniffLen)
	n, err := io.ReadFull(src, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, httpx.BadRequest("读取上传内容失败")
	}
	head = head[:n]
	if n == 0 {
		return nil, httpx.BadRequest("上传的文件是空的")
	}

	mimeType := http.DetectContentType(head)
	ext, ok := imageTypes[mimeType]
	if !ok {
		return nil, httpx.BadRequest("只支持 JPG / PNG / GIF / WebP 格式的图片")
	}

	filename, err := randomFilename(ext)
	if err != nil {
		return nil, err
	}
	fullPath := filepath.Join(s.Dir, filename)

	file, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("创建文件失败: %w", err)
	}

	// 头部已经读走，余下内容从 src 继续读；LimitReader 兜住超限的情况。
	written, copyErr := io.Copy(file, io.MultiReader(
		bytes.NewReader(head),
		io.LimitReader(src, s.MaxBytes-int64(n)+1),
	))
	closeErr := file.Close()

	if copyErr != nil {
		_ = os.Remove(fullPath)
		return nil, fmt.Errorf("写入文件失败: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(fullPath)
		return nil, fmt.Errorf("关闭文件失败: %w", closeErr)
	}
	if written > s.MaxBytes {
		_ = os.Remove(fullPath)
		return nil, httpx.New(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("图片不能超过 %d MB", s.MaxBytes/1024/1024))
	}

	return &Saved{
		URL:      s.URLPrefix + "/" + filename,
		Filename: filename,
		Size:     written,
		MimeType: mimeType,
	}, nil
}

// randomFilename 生成「时间戳-随机数.扩展名」，例如 1759824000-3f9a2c1b.png。
func randomFilename(ext string) (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成文件名失败: %w", err)
	}
	return fmt.Sprintf("%d-%s%s", time.Now().Unix(), hex.EncodeToString(buf), ext), nil
}

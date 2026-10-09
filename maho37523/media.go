package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const maxImageBytes = 5 << 20

var ErrInvalidMedia = errors.New("图片不存在、不属于你或已用于其他帖子")

type MediaService struct {
	db    *sql.DB
	dir   string
	slots chan struct{}
}
type MediaHandler struct{ service *MediaService }

func newMediaService(d *sql.DB, dir string) (*MediaService, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(abs, 0750); err != nil {
		return nil, err
	}
	return &MediaService{db: d, dir: abs, slots: make(chan struct{}, 2)}, nil
}
func (s *MediaService) Get(ctx context.Context, id int) (Media, error) {
	var m Media
	err := s.db.QueryRowContext(ctx, "SELECT id,user_id,path,mime,width,height,created_at FROM media WHERE id=?", id).Scan(&m.ID, &m.UserID, &m.Path, &m.MIME, &m.Width, &m.Height, &m.CreatedAt)
	m.URL = fmt.Sprintf("/api/media/%d/content", m.ID)
	return m, err
}
func (s *MediaService) Save(ctx context.Context, userID int, r io.Reader) (Media, error) {
	var m Media
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return m, rateLimitError("图片处理繁忙，请稍后重试")
	}
	data, err := io.ReadAll(io.LimitReader(r, maxImageBytes+1))
	if err != nil {
		return m, err
	}
	if len(data) > maxImageBytes {
		return m, validationError("单张图片最多 5 MiB")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || (format != "jpeg" && format != "png") {
		return m, validationError("只支持实际内容为 JPEG 或 PNG 的图片")
	}
	if cfg.Width < 1 || cfg.Height < 1 || cfg.Width > 8000 || cfg.Height > 8000 || int64(cfg.Width)*int64(cfg.Height) > 16_000_000 {
		return m, validationError("图片尺寸过大，最多 1600 万像素")
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return m, validationError("图片损坏，无法解码")
	}
	// Re-encode to remove EXIF and embedded metadata; limit both storage and AI payload size.
	w, h := cfg.Width, cfg.Height
	if w > 1600 || h > 1600 {
		if w >= h {
			h = h * 1600 / w
			w = 1600
		} else {
			w = w * 1600 / h
			h = 1600
		}
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	bounds := src.Bounds()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := src.At(bounds.Min.X+x*cfg.Width/w, bounds.Min.Y+y*cfg.Height/h)
			r, g, b, a := c.RGBA()
			dst.SetRGBA(x, y, color.RGBA{uint8((r + 65535 - a) >> 8), uint8((g + 65535 - a) >> 8), uint8((b + 65535 - a) >> 8), 255})
		}
	}
	file, err := os.CreateTemp(s.dir, "image-*.jpg")
	if err != nil {
		return m, err
	}
	path := file.Name()
	keep := false
	defer func() {
		file.Close()
		if !keep {
			os.Remove(path)
		}
	}()
	if err = jpeg.Encode(file, dst, &jpeg.Options{Quality: 85}); err != nil {
		return m, err
	}
	if err = file.Close(); err != nil {
		return m, err
	}
	m = Media{UserID: userID, Path: filepath.Base(path), MIME: "image/jpeg", Width: w, Height: h, CreatedAt: time.Now().Unix()}
	err = s.db.QueryRowContext(ctx, "INSERT INTO media(user_id,path,mime,width,height,created_at) SELECT ?,?,?,?,?,? WHERE (SELECT count(*) FROM media WHERE user_id=? AND created_at>?)<30 RETURNING id", m.UserID, m.Path, m.MIME, m.Width, m.Height, m.CreatedAt, userID, m.CreatedAt-3600).Scan(&m.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return Media{}, rateLimitError("每小时最多上传 30 张图片")
	}
	if err != nil {
		return Media{}, err
	}
	keep = true
	m.URL = fmt.Sprintf("/api/media/%d/content", m.ID)
	return m, nil
}
func (h *MediaHandler) Upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "POST")
		return
	}
	user, ok := requireLogin(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImageBytes+(1<<20))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		writeJSON(w, 400, Response{Code: 400, Message: "上传无效或超过 5 MiB"})
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	files := r.MultipartForm.File["image"]
	if len(files) != 1 {
		writeJSON(w, 400, Response{Code: 400, Message: "每次请上传一张 image 图片"})
		return
	}
	var count int
	if err := h.service.db.QueryRowContext(r.Context(), "SELECT count(*) FROM media WHERE user_id=? AND created_at>?", user.ID, time.Now().Add(-time.Hour).Unix()).Scan(&count); err != nil {
		writeItemError(w, err)
		return
	}
	if count >= 30 {
		writeJSON(w, 429, Response{Code: 429, Message: "每小时最多上传 30 张图片"})
		return
	}
	file, err := files[0].Open()
	if err != nil {
		writeItemError(w, err)
		return
	}
	defer file.Close()
	media, err := h.service.Save(r.Context(), user.ID, file)
	if err != nil {
		writeItemError(w, err)
		return
	}
	writeJSON(w, 201, Response{Code: 0, Message: "图片已上传，请勿上传未遮挡的姓名、学号或证件号码", Data: media})
}
func (h *MediaHandler) Content(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	m, err := h.service.Get(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var public int
	if err = h.service.db.QueryRowContext(r.Context(), "SELECT count(*) FROM item_images x JOIN items i ON i.id=x.item_id WHERE x.media_id=? AND i.deleted_at=0", id).Scan(&public); err != nil {
		writeItemError(w, err)
		return
	}
	if public == 0 {
		user, err := currentUser(r)
		if err != nil || user.ID != m.UserID {
			http.NotFound(w, r)
			return
		}
	}
	file, err := h.service.Open(m)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", m.MIME)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=300")
	http.ServeContent(w, r, m.Path, time.Unix(m.CreatedAt, 0), file)
}
func (s *MediaService) Open(m Media) (*os.File, error) {
	if m.Path == "" || filepath.Base(m.Path) != m.Path {
		return nil, ErrInvalidMedia
	}
	return os.Open(filepath.Join(s.dir, m.Path))
}
func currentUser(r *http.Request) (User, error) {
	var user User
	cookie, err := r.Cookie("session")
	if err != nil {
		return user, err
	}
	err = db.QueryRowContext(r.Context(), "SELECT u.id,u.username FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token=? AND s.expires_at>?", cookie.Value, time.Now().Unix()).Scan(&user.ID, &user.Username)
	return user, err
}
func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeJSON(w, 405, Response{Code: 405, Message: "请求方法不支持，只允许 " + allow})
}

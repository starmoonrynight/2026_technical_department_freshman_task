// Package config 集中管理全部可调参数，与 Node 版 src/config.js 一一对应。
// 每一项都可以通过环境变量覆盖。
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	// SessionCookie 会话 Cookie 名称，与 Node 版保持一致，两版可互换登录态。
	SessionCookie = "laf_sid"
	// DefaultPageSize 默认每页条数。
	DefaultPageSize = 10
	// MaxPageSize 每页条数上限。
	MaxPageSize = 50
	// MaxBodyBytes 请求体上限，对应 express.json({ limit: '256kb' })。
	MaxBodyBytes = 256 * 1024
	// UploadURLPrefix 上传图片的对外访问前缀。
	UploadURLPrefix = "/uploads"
)

// Categories 物品分类字典。
var Categories = []string{
	"证件卡类", "电子产品", "书籍资料", "衣物鞋帽", "钥匙", "钱包箱包", "运动器材", "首饰配件", "其他",
}

// AdminSeed 初始管理员账号（学号 / 口令 / 姓名）。
type AdminSeed struct {
	StudentID string
	Password  string
	Name      string
}

// Config 进程级配置。
type Config struct {
	Root      string
	Host      string
	Port      int
	DBFile    string
	PublicDir string

	// UploadDir 图片落盘目录，默认在 public/ 下，这样静态资源中间件可以直接访问。
	UploadDir string
	// MaxUploadBytes 单张图片大小上限。
	MaxUploadBytes int64

	SessionTTL time.Duration

	DefaultAdmin AdminSeed
}

// Load 读取环境变量并解析出运行配置。
func Load() Config {
	root := env("ROOT_DIR", "")
	if root == "" {
		root = findRepoRoot()
	}

	publicDir := env("PUBLIC_DIR", filepath.Join(root, "public"))
	dbFile := env("DB_FILE", filepath.Join(root, "data", "lostfound.db"))
	uploadDir := env("UPLOAD_DIR", filepath.Join(publicDir, "uploads"))

	return Config{
		Root:      root,
		Host:      env("HOST", "127.0.0.1"),
		Port:      envInt("PORT", 3001),
		DBFile:    dbFile,
		PublicDir: publicDir,
		UploadDir: uploadDir,

		MaxUploadBytes: envInt64("MAX_UPLOAD_MB", 5) * 1024 * 1024,

		SessionTTL: time.Duration(envInt64("SESSION_TTL_MS", int64(7*24*60*60*1000))) * time.Millisecond,

		DefaultAdmin: AdminSeed{
			StudentID: env("ADMIN_STUDENT_ID", env("ADMIN_USER", "10000000")),
			Password:  env("ADMIN_PASSWORD", "admin123"),
			Name:      env("ADMIN_NAME", env("ADMIN_NICKNAME", "系统管理员")),
		},
	}
}

// Address 监听地址。
func (c Config) Address() string {
	return c.Host + ":" + strconv.Itoa(c.Port)
}

// findRepoRoot 从当前工作目录向上寻找仓库根目录（含 public/index.html 的那一层）。
// Go 可执行文件没有 __dirname，只能靠标记文件定位，找不到时退化为工作目录。
func findRepoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}

	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "public", "index.html")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "."
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	return int(envInt64(key, int64(fallback)))
}

func envInt64(key string, fallback int64) int64 {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

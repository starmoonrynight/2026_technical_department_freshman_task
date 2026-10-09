// Package config 把 .env 一次性解析进 Config 结构体。
//
// 纪律：不用全局变量。Config 通过函数参数逐层传入 —— 全局变量在测试里没法替换，
// 会让「跑测试时不小心连到开发库」这种事故变得可能。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config 是全部运行时配置。ENV=prod 时 debug 端点关闭、gin 走 release 模式。
type Config struct {
	Env  string // dev | prod
	Port string

	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string

	JWTSecret     string
	JWTExpire     time.Duration
	UploadDir     string
	UploadBaseURL string

	LogLevel string

	Match MatchConfig

	// SSO（M8 才用，现在允许为空）
	SSOStateKey      string
	HDUHelpAppID     string
	HDUHelpAppSecret string
	HDUHelpRedirect  string
	HDUHelpScope     string
}

// MatchConfig 是匹配算法的全部可调参数。
// 做成环境变量的意义：调参不用改代码、不用重新编译。
type MatchConfig struct {
	TimeToleranceHours int
	DecayDays          int
	NotifyThreshold    float64 // 高于此值写 match_pairs + 发通知（仅 found 帖创建这条路径）
	ShowThreshold      float64 // 高于此值才在结果里展示
}

// DSN 拼出 pgx 需要的连接串。
// 分成 5 个变量而不是一整条 DSN 字符串，是因为对不熟连接串语法的人友好得多。
func (c Config) DSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=disable",
		c.DBUser, c.DBPassword, c.DBHost, c.DBPort, c.DBName,
	)
}

// IsProd 报告是否生产环境。
func (c Config) IsProd() bool { return c.Env == "prod" }

// Load 读 .env（存在才读，不报错）再从环境变量解析。
// 任何必填项缺失或格式错误都直接返回 error，由 main 打日志并 fatal ——
// 绝不带着一个半残的 Config 继续跑。
func Load() (Config, error) {
	// .env 不存在时 godotenv 返回 error，这在生产环境是正常的（用真环境变量），所以忽略
	_ = godotenv.Load()

	c := Config{
		Env:  getEnv("ENV", "dev"),
		Port: getEnv("PORT", "8080"),

		DBHost:     getEnv("DB_HOST", "localhost"),
		DBPort:     getEnv("DB_PORT", "5432"),
		DBUser:     getEnv("DB_USER", "lf"),
		DBPassword: getEnv("DB_PASSWORD", "lf"),
		DBName:     getEnv("DB_NAME", "lostfound"),

		JWTSecret:     os.Getenv("JWT_SECRET"),
		UploadDir:     getEnv("UPLOAD_DIR", "./uploads"),
		UploadBaseURL: getEnv("UPLOAD_BASE_URL", "/uploads"),
		LogLevel:      strings.ToLower(getEnv("LOG_LEVEL", "info")),

		SSOStateKey:      os.Getenv("SSO_STATE_KEY"),
		HDUHelpAppID:     os.Getenv("HDUHELP_APP_ID"),
		HDUHelpAppSecret: os.Getenv("HDUHELP_APP_SECRET"),
		HDUHelpRedirect:  os.Getenv("HDUHELP_REDIRECT_URI"),
		HDUHelpScope:     getEnv("HDUHELP_SCOPE", "USER_BASIC"),
	}

	// JWT_SECRET 必填且无默认值。给个默认值等于给所有部署实例同一把钥匙。
	if strings.TrimSpace(c.JWTSecret) == "" {
		return Config{}, fmt.Errorf("config: 环境变量 JWT_SECRET 必填，且没有默认值（复制 .env.example 改一个随机串即可）")
	}

	hours, err := getInt("JWT_EXPIRE_HOURS", 24)
	if err != nil {
		return Config{}, err
	}
	c.JWTExpire = time.Duration(hours) * time.Hour

	tol, err := getInt("MATCH_TIME_TOLERANCE_HOURS", 24)
	if err != nil {
		return Config{}, err
	}
	decay, err := getInt("MATCH_DECAY_DAYS", 14)
	if err != nil {
		return Config{}, err
	}
	notify, err := getFloat("MATCH_NOTIFY_THRESHOLD", 0.75)
	if err != nil {
		return Config{}, err
	}
	show, err := getFloat("MATCH_SHOW_THRESHOLD", 0.55)
	if err != nil {
		return Config{}, err
	}
	c.Match = MatchConfig{
		TimeToleranceHours: tol,
		DecayDays:          decay,
		NotifyThreshold:    notify,
		ShowThreshold:      show,
	}

	// 阈值写反是很容易犯的错，而且错了之后症状是「通知特别多」或「一条都没有」，很难往配置上想
	if c.Match.ShowThreshold > c.Match.NotifyThreshold {
		return Config{}, fmt.Errorf(
			"config: MATCH_SHOW_THRESHOLD (%.2f) 不应大于 MATCH_NOTIFY_THRESHOLD (%.2f)，否则会出现「展示了但从不通知」的怪状态",
			c.Match.ShowThreshold, c.Match.NotifyThreshold)
	}

	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("config: LOG_LEVEL 只能是 debug/info/warn/error，当前是 %q", c.LogLevel)
	}

	return c, nil
}

// Redacted 返回一份可以安全打进日志/接口的副本，敏感值打码。
// /api/debug/config 用的就是它。
func (c Config) Redacted() map[string]any {
	return map[string]any{
		"env":                    c.Env,
		"port":                   c.Port,
		"log_level":              c.LogLevel,
		"db_host":                c.DBHost,
		"db_port":                c.DBPort,
		"db_user":                c.DBUser,
		"db_name":                c.DBName,
		"db_password":            mask(c.DBPassword),
		"jwt_secret":             mask(c.JWTSecret),
		"jwt_expire_hours":       int(c.JWTExpire.Hours()),
		"upload_dir":             c.UploadDir,
		"match_notify_threshold": c.Match.NotifyThreshold,
		"match_show_threshold":   c.Match.ShowThreshold,
		"match_decay_days":       c.Match.DecayDays,
		"hduhelp_app_id":         mask(c.HDUHelpAppID),
		"hduhelp_app_secret":     mask(c.HDUHelpAppSecret),
		"sso_state_key":          mask(c.SSOStateKey),
	}
}

// mask 只保留「配了没配」这个信息，不泄漏内容。
func mask(s string) string {
	if s == "" {
		return "(未配置)"
	}
	return "***"
}

func getEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func getInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: 环境变量 %s 必须是整数，当前是 %q", key, raw)
	}
	return n, nil
}

func getFloat(key string, fallback float64) (float64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("config: 环境变量 %s 必须是数字，当前是 %q", key, raw)
	}
	return f, nil
}

// 失物招领系统后端入口。
//
// 顺序（§6）：loadConfig → slog → pgxpool.New + Ping → 跑迁移 → router.Setup（内部 repo → auth → service → handler → 路由）→ ListenAndServe
// 每步失败都 fatal 并带上下文。整个文件刻意保持在 80 行以内、可通读：
// 依赖全部手工按顺序 new，不用 wire/fx —— 生成代码和隐式魔法对初学者是调试黑洞。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"lostfound/internal/config"
	"lostfound/internal/database"
	"lostfound/internal/router"
)

func main() {
	// 这里用 slog.Default()（文本、stderr）：配置还没加载，JSON logger 还没建起来，
	// 而「启动失败的原因」必须保证人能读懂
	if err := run(); err != nil {
		slog.Error("startup.failed", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger) // 之后 apperr.Log(c) 在没预绑定 logger 的场合也能拿到它

	pool, err := database.Open(context.Background(), cfg, logger)
	if err != nil {
		return err
	}
	defer pool.Close()

	if err := database.Migrate(pool, logger); err != nil {
		return err
	}

	// 依赖装配全部在 router.Setup 里面（§10：冒烟测试必须走和生产完全相同的装配路径，
	// 所以不能有一部分只在 main.go 里发生）。main.go 只负责「起进程」这件事。
	engine, err := router.Setup(cfg, pool)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           engine,
		ReadHeaderTimeout: 10 * time.Second,
	}

	logger.Info("server.listening", slog.String("addr", srv.Addr), slog.String("env", cfg.Env))

	// 打连接坐标而不是打 Adminer 的 URL：数据库可能跑在 docker-compose 里（Adminer 在
	// :8081），也可能是本机原生 PostgreSQL（用 pgAdmin）。写死一个地址有一半概率是死链，
	// 而这几个值正是任何图形客户端都要填的东西。
	fmt.Printf("\n  健康检查  curl http://localhost:%s/api/health\n  数据库    %s@%s:%s/%s  (Adminer / pgAdmin 里照这个填)\n\n",
		cfg.Port, cfg.DBUser, cfg.DBHost, cfg.DBPort, cfg.DBName)

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: 监听 %s 失败（端口被占了？）: %w", srv.Addr, err)
	}
	return nil
}

// newLogger 用 JSON Handler —— 结构化日志才能被 grep / jq 处理（§9 四件套之一）。
// level 已经在 config.Load 里白名单校验过，这里的 default 分支只是兜住 info。
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}

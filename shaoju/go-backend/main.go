// 失物招领系统 —— Go + Gin + SQLite 后端。
//
// 与 Node 版共用同一套 HTTP 契约、同一个 SQLite 文件、同一种口令散列格式，
// 可以按需在两种实现之间切换。
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"lostfound/internal/bootstrap"
	"lostfound/internal/config"
	"lostfound/internal/database"
	"lostfound/internal/store"
	"lostfound/internal/upload"
	"lostfound/internal/web"
)

func main() {
	cfg := config.Load()

	db, err := database.Open(cfg.DBFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[fatal]", err)
		os.Exit(1)
	}
	defer db.Close()

	s := store.New(db)

	admin, err := bootstrap.Run(s, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[fatal]", err)
		os.Exit(1)
	}
	if admin.Created {
		fmt.Printf("[bootstrap] 已创建默认管理员 %s / %s，请登录后尽快修改口令\n", admin.StudentID, admin.Password)
	}

	// 图片上传：目录默认在 public/uploads，静态资源中间件会直接把它吐出去
	uploads := upload.New(cfg.UploadDir, config.UploadURLPrefix, cfg.MaxUploadBytes)
	if err := uploads.EnsureDir(); err != nil {
		fmt.Fprintln(os.Stderr, "[fatal]", err)
		os.Exit(1)
	}

	router := web.NewRouter(&web.Deps{Cfg: cfg, Store: s, Uploads: uploads})

	server := &http.Server{
		Addr:              cfg.Address(),
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 每小时清理一次过期会话
	cleanupCtx, stopCleanup := context.WithCancel(context.Background())
	defer stopCleanup()
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-cleanupCtx.Done():
				return
			case <-ticker.C:
				if _, err := s.CleanupExpired(); err != nil {
					fmt.Fprintln(os.Stderr, "[session] 清理过期会话失败:", err)
				}
			}
		}
	}()

	go func() {
		fmt.Println()
		fmt.Println("  失物招领系统已启动（Go + Gin）")
		fmt.Printf("  前台首页: http://%s:%d/\n", cfg.Host, cfg.Port)
		fmt.Printf("  管理后台: http://%s:%d/admin\n", cfg.Host, cfg.Port)
		fmt.Printf("  数据库:   %s\n", cfg.DBFile)
		fmt.Println()

		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "[fatal]", err)
			os.Exit(1)
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	sig := <-signals

	fmt.Printf("\n收到 %s，正在关闭服务...\n", sig)
	stopCleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "[shutdown]", err)
	}
}

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	db, err = openDatabase()
	if err != nil {
		return fmt.Errorf("数据库初始化失败: %w", err)
	}
	defer db.Close()
	media, err := newMediaService(db, config.UploadDir)
	if err != nil {
		return err
	}
	repo := &ItemRepository{db: db}
	items := &ItemHandler{service: &ItemService{repo: repo}}
	client, err := newDeepSeekClient(config, media)
	if err != nil {
		return err
	}
	ai := &AIService{db: db, repo: repo, media: media, client: client, config: config}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	workerCtx, cancelWorker := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Add(1)
	go func() { defer workers.Done(); ai.Run(workerCtx) }()
	defer func() { cancelWorker(); workers.Wait() }()
	router := newRouter(items, &MediaHandler{service: media}, &AIHandler{service: ai}, &NotificationHandler{db: db})
	server := &http.Server{Addr: config.Address, Handler: securityHeaders(http.NewCrossOriginProtection().Handler(router)), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 1 << 20}
	errs := make(chan error, 1)
	go func() { errs <- server.ListenAndServe() }()
	previewAddress := config.Address
	if strings.HasPrefix(previewAddress, ":") {
		previewAddress = "localhost" + previewAddress
	}
	log.Printf("服务器启动，监听 %s；浏览器打开 http://%s", config.Address, previewAddress)
	if !ai.enabled() {
		log.Println("AI 未启用：配置 DEEPSEEK_API_KEY 后重启；不会发送任何 AI 请求")
	}
	select {
	case err = <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}

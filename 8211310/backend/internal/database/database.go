// Package database 负责两件事：建连接池、跑迁移。
//
// 为什么迁移是「嵌入式」的（§9）：本机没有 psql，任何「命令行跑迁移」的方案都要先装工具。
// 嵌入式 = `go run ./cmd/server` 一条命令同时建表 + 起服务。
package database

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/golang-migrate/migrate/v4"
	migratepgx "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"lostfound/internal/config"
	"lostfound/migrations"
)

// Open 建连接池并 Ping 一次。
// Ping 失败直接返回错误 —— 数据库连不上时「服务起来了但每个接口都 500」是最难排查的状态，
// 不如启动时就 fatal，日志里写清楚是连不上库。
func Open(ctx context.Context, cfg config.Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN())
	if err != nil {
		// ⚠ 故意不把 DSN 写进错误信息：DSN 里带密码，而这条错误最终会进日志
		return nil, fmt.Errorf("database.Open: 解析数据库配置失败（检查 DB_* 环境变量里有没有奇怪字符）: %w", err)
	}

	poolCfg.MaxConns = 10
	poolCfg.MinConns = 1
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnIdleTime = 30 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute
	poolCfg.ConnConfig.Tracer = &queryTracer{
		logger:  logger,
		enabled: cfg.LogLevel == "debug",
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("database.Open: 创建连接池失败: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf(
			"database.Open: 连不上数据库 %s:%s/%s（docker compose up -d 起了吗？）: %w",
			cfg.DBHost, cfg.DBPort, cfg.DBName, err)
	}

	logger.Info("database.connected",
		slog.String("host", cfg.DBHost),
		slog.String("port", cfg.DBPort),
		slog.String("db", cfg.DBName),
		slog.Int("max_conns", int(poolCfg.MaxConns)))
	return pool, nil
}

// Ping 供 /api/health 使用，带 2 秒超时 —— 健康检查不能因为库卡住而把请求也挂住。
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return pool.Ping(ctx)
}

// Migrate 把 migrations.FS 里的 *.sql 应用到最新。
//
// 纪律（§9）：已发布的迁移文件永远不改内容，只加新序号文件。
// 因为 golang-migrate 会按文件内容的校验和记账，改了老文件会导致 checksum 不匹配、启动失败。
func Migrate(pool *pgxpool.Pool, logger *slog.Logger) error {
	m, cleanup, err := newMigrator(pool)
	if err != nil {
		return err
	}
	defer cleanup()

	before, _, _ := m.Version()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("database.Migrate: 应用迁移失败（不要手工改已发布的迁移文件）: %w", err)
	}

	after, _, _ := m.Version()
	if before != after {
		// m.Version() 返回 uint；迁移版本号只是 1、2、3 这种小整数，转 int64 打日志足够
		logger.Info("database.migrated",
			slog.Int64("from_version", int64(before)),
			slog.Int64("to_version", int64(after)))
	} else {
		logger.Debug("database.migrate_noop", slog.Int64("version", int64(after)))
	}
	return nil
}

// MigrateDown 把全部迁移回滚到版本 0，也就是删掉所有表。
//
// ⚠ 破坏性操作。main.go 永远不调用它。
// 存在的理由有两个：① 集成测试每个用例开头要把 schema 重置成干净状态；
// ② 本地开发库被手工改坏了，`MigrateDown` + `Migrate` 比对着 Adminer 一张张表删快得多。
func MigrateDown(pool *pgxpool.Pool, logger *slog.Logger) error {
	m, cleanup, err := newMigrator(pool)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("database.MigrateDown: 回滚迁移失败: %w", err)
	}
	logger.Debug("database.migrated_down")
	return nil
}

// newMigrator 装配 golang-migrate：源是内嵌的 migrations.FS，驱动复用现有连接池。
// 返回的 cleanup 必须调用，否则会从池里漏掉一个连接。
func newMigrator(pool *pgxpool.Pool) (*migrate.Migrate, func(), error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, nil, fmt.Errorf("database: 读取内嵌迁移文件失败: %w", err)
	}

	// OpenDBFromPool 复用同一个连接池，且「关掉这个 sql.DB 不会关掉 pool」（pgx 文档明说）。
	// 它还会自动把 MaxIdleConns 设成 0，避免把池里的连接全占住饿死业务请求。
	sqlDB := stdlib.OpenDBFromPool(pool)

	driver, err := migratepgx.WithInstance(sqlDB, &migratepgx.Config{})
	if err != nil {
		_ = sqlDB.Close()
		_ = src.Close()
		return nil, nil, fmt.Errorf("database: 初始化迁移驱动失败: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "lostfound", driver)
	if err != nil {
		_ = sqlDB.Close()
		_ = src.Close()
		return nil, nil, fmt.Errorf("database: 初始化迁移器失败: %w", err)
	}

	cleanup := func() {
		// Close 返回 (sourceErr, databaseErr)；迁移器已经跑完，这里的错误不值得让调用方处理
		_, _ = m.Close()
		_ = sqlDB.Close()
	}
	return m, cleanup, nil
}

// ---------- SQL 查询日志（§9 四件套之三）----------

// queryTracer 实现 pgx.QueryTracer。
// LOG_LEVEL=debug 时打印 SQL + 参数 + 耗时 + 行数 —— 这是「不熟 SQL 的人学会看
// 自己系统在执行什么」的最直接教材。非 debug 时所有方法立即返回，零开销。
type queryTracer struct {
	logger  *slog.Logger
	enabled bool
}

// traceKey / traceInfo 用来把 Start 阶段的信息带到 End 阶段。
//
// 为什么必须这么绕：pgx 的 TraceQueryEndData 只有 CommandTag 和 Err 两个字段，
// 既不含 SQL 也不含耗时。想让「SQL + 参数 + 耗时 + 行数」出现在同一条日志里，
// 只能在 Start 时把它们连同起始时间一起塞进 context —— pgx 会把
// TraceQueryStart 返回的 context 原样传给 TraceQueryEnd。
//
// key 用私有类型，避免和别人的 context key 撞上。
type traceKey struct{}

type traceInfo struct {
	sql   string
	args  []any
	start time.Time
}

var _ pgx.QueryTracer = (*queryTracer)(nil)

func (t *queryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !t.enabled {
		return ctx
	}
	return context.WithValue(ctx, traceKey{}, traceInfo{
		sql:   data.SQL,
		args:  data.Args,
		start: time.Now(),
	})
}

func (t *queryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if !t.enabled {
		return
	}

	info, _ := ctx.Value(traceKey{}).(traceInfo)
	attrs := []any{
		slog.String("sql", info.sql),
		slog.String("command_tag", data.CommandTag.String()),
		slog.Int64("rows", data.CommandTag.RowsAffected()),
		slog.Int64("duration_ms", time.Since(info.start).Milliseconds()),
	}
	// 参数可能含敏感值（密码哈希、联系方式），所以只在 debug 级别出现 —— 而 debug 级别本来就不该在 prod 开
	if len(info.args) > 0 {
		attrs = append(attrs, slog.Any("args", info.args))
	}
	if data.Err != nil {
		attrs = append(attrs, slog.String("err", data.Err.Error()))
		t.logger.Error("sql.query", attrs...)
		return
	}
	t.logger.Debug("sql.query", attrs...)
}

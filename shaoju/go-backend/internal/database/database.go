// Package database 打开 SQLite 连接、执行初始化 SQL 并运行版本化迁移。
//
// 表结构定义在 schema.sql 中（用 go:embed 嵌入），迁移逻辑在 migrate.go 中。
// 与 Node 版共用同一个 .db 文件时，两边都只做幂等的 CREATE IF NOT EXISTS。
package database

import (
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 的 SQLite 驱动，无需 cgo
)

//go:embed schema.sql
var schemaSQL string

// DB 包装 *sql.DB。
type DB struct {
	*sql.DB
}

// Open 打开（必要时创建）数据库、执行初始化 SQL，并把历史库升级到最新结构。
func Open(dbFile string) (*DB, error) {
	if dir := filepath.Dir(dbFile); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建数据目录失败: %w", err)
		}
	}

	dsn := "file:" + filepath.ToSlash(dbFile) +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)"

	handle, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	if err := handle.Ping(); err != nil {
		handle.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	// SQLite 单写多读；限制连接数可以减少写锁竞争。
	handle.SetMaxOpenConns(8)
	handle.SetMaxIdleConns(8)

	db := &DB{handle}

	if err := db.execSchema(); err != nil {
		handle.Close()
		return nil, err
	}
	if err := db.Migrate(); err != nil {
		handle.Close()
		return nil, err
	}
	return db, nil
}

// execSchema 执行初始化 SQL。
// PRAGMA 语句单独执行：它们的返回值在部分驱动上会让多语句 Exec 报错。
func (db *DB) execSchema() error {
	pragmas := make([]string, 0, 3)
	statements := make([]string, 0, 16)

	for _, raw := range strings.Split(schemaSQL, ";") {
		statement := stripSQLComments(raw)
		if statement == "" {
			continue
		}
		if strings.HasPrefix(strings.ToUpper(statement), "PRAGMA") {
			pragmas = append(pragmas, statement)
			continue
		}
		statements = append(statements, statement)
	}

	for _, pragma := range pragmas {
		if _, err := db.Exec(pragma); err != nil {
			return fmt.Errorf("执行 %s 失败: %w", pragma, err)
		}
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			return fmt.Errorf("初始化表结构失败: %w\nSQL: %s", err, statement)
		}
	}
	return nil
}

// stripSQLComments 去掉行注释与首尾空白，让按分号切分的结果可以直接执行。
func stripSQLComments(raw string) string {
	lines := strings.Split(raw, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if idx := strings.Index(line, "--"); idx >= 0 {
			line = line[:idx]
		}
		line = strings.TrimSpace(line)
		if line != "" {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// Now 统一的时间格式（ISO 8601，与 JS 的 Date#toISOString 一致）。
func Now() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}

// Tx 执行一段事务。
func (db *DB) Tx(fn func(tx *sql.Tx) error) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

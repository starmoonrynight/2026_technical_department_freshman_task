// Package store 承载全部数据访问与业务逻辑，对应 Node 版的 src/services。
package store

import (
	"database/sql"
	"errors"

	"lostfound/internal/database"
)

// Store 数据库访问入口。
type Store struct {
	DB *database.DB
}

// New 构造 Store。
func New(db *database.DB) *Store {
	return &Store{DB: db}
}

// wrap 原样返回驱动错误，业务层只需判断 sql.ErrNoRows。
func wrap(err error) error {
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return err
}

// firstNonEmpty 取第一个非空字符串。
// 用于在「正式列」与「兼容列」之间择一：Node 版写入的历史数据只有兼容列有值。
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

package web

import (
	"lostfound/internal/database"
)

// nowISO 统一的时间格式，与 JS 的 Date#toISOString 一致。
func nowISO() string {
	return database.Now()
}

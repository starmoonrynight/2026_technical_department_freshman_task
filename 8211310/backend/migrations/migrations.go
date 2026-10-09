package migrations

import "embed"

// FS 内嵌 backend/migrations/*.sql。
// 本机没有 psql/createdb，所以迁移只能作为库嵌进二进制，由 golang-migrate 在启动时应用。
//
//go:embed *.sql
var FS embed.FS

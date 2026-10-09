package main

import (
	"database/sql"
	"fmt"
	_ "modernc.org/sqlite"
	"os"
	"time"
)

// Authentication still uses this connection; the new services receive it explicitly.
var db *sql.DB

func openDatabase() (*sql.DB, error) {
	return openDatabaseAt(envOr("APP_DB_PATH", "lostfound.db"), true)
}
func openDatabaseAt(path string, backup bool) (*sql.DB, error) {
	d, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	fail := func(e error) (*sql.DB, error) { d.Close(); return nil, e }
	if err = d.Ping(); err != nil {
		return fail(err)
	}
	for _, q := range []string{"PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=5000"} {
		if _, err = d.Exec(q); err != nil {
			return fail(err)
		}
	}
	var exists, version int
	if err = d.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'").Scan(&exists); err != nil {
		return fail(err)
	}
	if exists > 0 {
		if err = d.QueryRow("SELECT COALESCE(MAX(version),0) FROM schema_migrations").Scan(&version); err != nil {
			return fail(err)
		}
	}
	if version >= 1 {
		return d, nil
	}
	if backup && path != ":memory:" {
		if info, e := os.Stat(path); e == nil && info.Size() > 0 {
			var tables int
			if e = d.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='items'").Scan(&tables); e != nil {
				return fail(e)
			}
			if tables > 0 {
				target := fmt.Sprintf("%s.backup-%d", path, time.Now().UnixNano())
				if _, e = d.Exec("VACUUM INTO ?", target); e != nil {
					return fail(fmt.Errorf("迁移前备份失败: %w", e))
				}
			}
		}
	}
	tx, err := d.Begin()
	if err != nil {
		return fail(err)
	}
	// Roll back before closing the single connection on errors (closing first would deadlock).
	txFail := func(e error) (*sql.DB, error) { tx.Rollback(); return fail(e) }
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS users(id INTEGER PRIMARY KEY AUTOINCREMENT,username TEXT NOT NULL UNIQUE,password_hash TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sessions(token TEXT PRIMARY KEY,user_id INTEGER NOT NULL,expires_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS items(id INTEGER PRIMARY KEY AUTOINCREMENT,type TEXT NOT NULL,name TEXT NOT NULL,location TEXT NOT NULL,description TEXT NOT NULL DEFAULT '',status TEXT NOT NULL DEFAULT 'searching',user_id INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY,applied_at INTEGER NOT NULL)`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return txFail(err)
		}
	}
	rows, err := tx.Query("PRAGMA table_info(items)")
	if err != nil {
		return txFail(err)
	}
	present := map[string]bool{}
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var def any
		if err = rows.Scan(&cid, &name, &typ, &notNull, &def, &pk); err != nil {
			rows.Close()
			return txFail(err)
		}
		present[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return txFail(err)
	}
	columns := map[string]string{"user_id": "INTEGER NOT NULL DEFAULT 0", "category": "TEXT NOT NULL DEFAULT ''", "color": "TEXT NOT NULL DEFAULT ''", "brand": "TEXT NOT NULL DEFAULT ''", "tags_json": "TEXT NOT NULL DEFAULT '[]'", "contact": "TEXT NOT NULL DEFAULT ''", "occurred_at": "INTEGER NOT NULL DEFAULT 0", "created_at": "INTEGER NOT NULL DEFAULT 0", "updated_at": "INTEGER NOT NULL DEFAULT 0", "revision": "INTEGER NOT NULL DEFAULT 1", "allow_ai": "INTEGER NOT NULL DEFAULT 0", "deleted_at": "INTEGER NOT NULL DEFAULT 0"}
	for name, decl := range columns {
		if !present[name] {
			if _, err = tx.Exec("ALTER TABLE items ADD COLUMN " + name + " " + decl); err != nil {
				return txFail(err)
			}
		}
	}
	for _, q := range []string{
		`CREATE TABLE media(id INTEGER PRIMARY KEY AUTOINCREMENT,user_id INTEGER NOT NULL REFERENCES users(id),path TEXT NOT NULL UNIQUE,mime TEXT NOT NULL,width INTEGER NOT NULL,height INTEGER NOT NULL,created_at INTEGER NOT NULL)`,
		`CREATE TABLE item_images(item_id INTEGER NOT NULL REFERENCES items(id),media_id INTEGER NOT NULL UNIQUE REFERENCES media(id),position INTEGER NOT NULL,PRIMARY KEY(item_id,media_id))`,
		`CREATE TABLE ai_jobs(id INTEGER PRIMARY KEY AUTOINCREMENT,kind TEXT NOT NULL,user_id INTEGER NOT NULL,item_id INTEGER REFERENCES items(id),media_id INTEGER REFERENCES media(id),revision INTEGER NOT NULL DEFAULT 0,dedup_key TEXT NOT NULL UNIQUE,status TEXT NOT NULL DEFAULT 'pending',attempts INTEGER NOT NULL DEFAULT 0,run_after INTEGER NOT NULL,lease_until INTEGER NOT NULL DEFAULT 0,result TEXT NOT NULL DEFAULT '',error TEXT NOT NULL DEFAULT '',created_at INTEGER NOT NULL)`,
		`CREATE TABLE matches(id INTEGER PRIMARY KEY AUTOINCREMENT,lost_id INTEGER NOT NULL REFERENCES items(id),found_id INTEGER NOT NULL REFERENCES items(id),lost_revision INTEGER NOT NULL,found_revision INTEGER NOT NULL,score REAL NOT NULL,reasons TEXT NOT NULL,conflicts TEXT NOT NULL,uncertainty TEXT NOT NULL,model TEXT NOT NULL,created_at INTEGER NOT NULL,UNIQUE(lost_id,found_id,lost_revision,found_revision))`,
		`CREATE TABLE notifications(id INTEGER PRIMARY KEY AUTOINCREMENT,user_id INTEGER NOT NULL REFERENCES users(id),match_id INTEGER NOT NULL UNIQUE REFERENCES matches(id),read_at INTEGER NOT NULL DEFAULT 0,created_at INTEGER NOT NULL)`,
		`CREATE TABLE match_reviews(lost_id INTEGER NOT NULL REFERENCES items(id),found_id INTEGER NOT NULL REFERENCES items(id),lost_revision INTEGER NOT NULL,found_revision INTEGER NOT NULL,result TEXT NOT NULL,created_at INTEGER NOT NULL,PRIMARY KEY(lost_id,found_id,lost_revision,found_revision))`,
		`CREATE TABLE ai_usage(day TEXT PRIMARY KEY,calls INTEGER NOT NULL DEFAULT 0)`,
		`CREATE INDEX idx_items_active ON items(deleted_at,type,status,id)`,
		`CREATE INDEX idx_jobs_ready ON ai_jobs(status,run_after,lease_until)`,
		`CREATE INDEX idx_notifications_user ON notifications(user_id,id)`,
	} {
		if _, err = tx.Exec(q); err != nil {
			return txFail(err)
		}
	}
	now := time.Now().Unix()
	if _, err = tx.Exec("UPDATE items SET created_at=?,updated_at=? WHERE created_at=0", now, now); err != nil {
		return txFail(err)
	}
	if _, err = tx.Exec("INSERT INTO schema_migrations VALUES(1,?)", now); err != nil {
		return txFail(err)
	}
	if err = tx.Commit(); err != nil {
		return fail(err)
	}
	return d, nil
}

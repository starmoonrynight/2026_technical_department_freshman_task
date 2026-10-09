package database

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// migration 一次结构变更。
//
// Up 必须写成幂等的：老库需要真正改动，全新库则应当是空操作
// （因为 schema.sql 里已经是最新结构）。
type migration struct {
	Version int
	Name    string
	Up      func(*DB) error
}

var migrations = []migration{
	{
		Version: 1,
		Name:    "初始表结构 users / items / sessions",
		// 建表由 schema.sql 负责，这里只是把版本号记下来。
		Up: func(*DB) error { return nil },
	},
	{
		Version: 2,
		Name:    "users 增加 student_id（学号）与 name（姓名）",
		Up:      migrateUserIdentity,
	},
	{
		Version: 3,
		Name:    "items.status 扩展为三态：open(寻找中) / found(已找到) / closed(已结束)",
		Up:      migrateItemStatus,
	},
	{
		Version: 4,
		Name:    "items 增加 storage_place（寄放处，选填）",
		Up:      migrateItemStoragePlace,
	},
	{
		Version: 5,
		Name:    "演示账号的学号改成 8 位数字",
		Up:      migrateDemoStudentIDs,
	},
}

// Migrate 依次执行尚未应用的迁移。
func (db *DB) Migrate() error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			name       TEXT    NOT NULL,
			applied_at TEXT    NOT NULL
		)`); err != nil {
		return fmt.Errorf("创建迁移记录表失败: %w", err)
	}

	for _, m := range migrations {
		applied, err := db.migrationApplied(m.Version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}

		if err := m.Up(db); err != nil {
			return fmt.Errorf("迁移 %d（%s）失败: %w", m.Version, m.Name, err)
		}
		if _, err := db.Exec(
			`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.Version, m.Name, Now(),
		); err != nil {
			return fmt.Errorf("记录迁移 %d 失败: %w", m.Version, err)
		}
		fmt.Printf("[migrate] 已应用迁移 %d：%s\n", m.Version, m.Name)
	}
	return nil
}

func (db *DB) migrationApplied(version int) (bool, error) {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&count); err != nil {
		return false, fmt.Errorf("查询迁移记录失败: %w", err)
	}
	return count > 0, nil
}

// migrateUserIdentity 给历史库补上 student_id / name 两列，并用 username / nickname 回填。
// 全新库执行到这里时两列已存在，只剩唯一索引需要建立。
func migrateUserIdentity(db *DB) error {
	if _, err := ensureColumn(db, "users", "student_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if _, err := ensureColumn(db, "users", "name", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}

	// 历史数据里 username 就是学号，nickname 就是姓名。
	if _, err := db.Exec(`UPDATE users SET student_id = username WHERE student_id = ''`); err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE users SET name = nickname WHERE name = ''`); err != nil {
		return err
	}

	// 学号唯一；用部分索引而不是列约束，这样 Node 版写入的 '' 占位行不会互相冲突。
	if _, err := db.Exec(
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_student_id ON users(student_id) WHERE student_id <> ''`,
	); err != nil {
		return err
	}
	return nil
}

// demoStudentIDRenames 演示账号的旧学号 -> 新学号（8 位数字）。
// 取值与 Node 版 scripts/seed.js、Go 版 cmd/seed 完全一致，改一处就要三处一起改。
var demoStudentIDRenames = [][2]string{
	{"admin", "10000000"},
	{"zhangsan", "20230001"},
	{"lisi", "20230002"},
	{"wangwu", "20230003"},
}

// migrateDemoStudentIDs 把老库里的演示账号换成 8 位学号。
//
// 只改 student_id 与兼容列 username，users.id 不动，所以这些人发布的条目、
// 已有会话、口令散列都不受影响。新学号已经被别人占用（真实用户抢先注册了），
// 或者该账号早就改过了，就跳过这一条，不会报错。
func migrateDemoStudentIDs(db *DB) error {
	for _, rename := range demoStudentIDRenames {
		oldID, newID := rename[0], rename[1]

		var taken int
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM users WHERE student_id = ? OR username = ?`, newID, newID,
		).Scan(&taken); err != nil {
			return fmt.Errorf("检查学号 %s 是否被占用失败: %w", newID, err)
		}
		if taken > 0 {
			continue
		}

		if _, err := db.Exec(
			`UPDATE users SET student_id = ?, username = ? WHERE student_id = ? OR username = ?`,
			newID, newID, oldID, oldID,
		); err != nil {
			return fmt.Errorf("把演示账号 %s 改名为 %s 失败: %w", oldID, newID, err)
		}
	}
	return nil
}

// migrateItemStoragePlace 给 items 补上 storage_place 列（寄放处，选填）。
//
// 纯新增列，历史数据统一填空字符串，不需要像 v3 那样重建整张表。
// ALTER TABLE ADD COLUMN 带 NOT NULL 时必须给默认值，空字符串正好满足这个要求。
func migrateItemStoragePlace(db *DB) error {
	_, err := ensureColumn(db, "items", "storage_place", "TEXT NOT NULL DEFAULT ''")
	return err
}

// migrateItemStatus 把 items.status 的取值从两态（open / closed）扩展为三态
// （open 寻找中 / found 已找到 / closed 已结束）。
//
// SQLite 不能修改已有的 CHECK 约束，只能按官方推荐的「重建表」流程走一遍：
// 建新表 -> 拷数据 -> 删旧表 -> 改名 -> 重建索引。整个过程放在一个事务里，
// 任何一步失败都会整体回滚，不会留下半成品。
//
// 旧的 open / closed 取值原样保留，含义不变，所以历史数据不需要改写。
func migrateItemStatus(db *DB) error {
	already, err := itemStatusAllowsFound(db)
	if err != nil {
		return err
	}
	if already {
		// 全新库（schema.sql 里已是三态）或已经升级过的库
		return nil
	}

	return db.Tx(func(tx *sql.Tx) error {
		// 说明：这段 DDL 是 v3 迁移当时 items 表结构的快照，故意不从 schema.sql 复用，
		// 这样以后 schema.sql 再变也不会影响这一次历史迁移的语义。
		// 没有任何表通过外键引用 items，所以不需要临时关闭 foreign_keys。
		statements := []string{
			`CREATE TABLE items_new (
				id           INTEGER PRIMARY KEY AUTOINCREMENT,
				type         TEXT    NOT NULL CHECK (type IN ('lost', 'found')),
				title        TEXT    NOT NULL,
				category     TEXT    NOT NULL DEFAULT '其他',
				description  TEXT    NOT NULL DEFAULT '',
				location     TEXT    NOT NULL DEFAULT '',
				happened_at  TEXT    NOT NULL DEFAULT '',
				contact      TEXT    NOT NULL DEFAULT '',
				image_url    TEXT    NOT NULL DEFAULT '',
				status       TEXT    NOT NULL DEFAULT 'open'
				             CHECK (status IN ('open', 'found', 'closed')),
				audit_status TEXT    NOT NULL DEFAULT 'pending'
				             CHECK (audit_status IN ('pending', 'approved', 'rejected')),
				audit_remark TEXT    NOT NULL DEFAULT '',
				user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				created_at   TEXT    NOT NULL,
				updated_at   TEXT    NOT NULL
			)`,
			`INSERT INTO items_new (id, type, title, category, description, location, happened_at,
			                        contact, image_url, status, audit_status, audit_remark,
			                        user_id, created_at, updated_at)
			 SELECT id, type, title, category, description, location, happened_at,
			        contact, image_url, status, audit_status, audit_remark,
			        user_id, created_at, updated_at
			   FROM items`,
			`DROP TABLE items`,
			`ALTER TABLE items_new RENAME TO items`,
			`CREATE INDEX IF NOT EXISTS idx_items_audit  ON items(audit_status)`,
			`CREATE INDEX IF NOT EXISTS idx_items_type   ON items(type)`,
			`CREATE INDEX IF NOT EXISTS idx_items_user   ON items(user_id)`,
			`CREATE INDEX IF NOT EXISTS idx_items_status ON items(status)`,
		}

		for _, statement := range statements {
			if _, err := tx.Exec(statement); err != nil {
				return fmt.Errorf("重建 items 表失败: %w\nSQL: %s", err, statement)
			}
		}
		return nil
	})
}

// itemStatusAllowsFound 判断 items.status 的 CHECK 里是否已经允许 'found'。
//
// 直接读建表语句是最省事的判断方式（PRAGMA table_info 看不到 CHECK 约束），
// 但要注意 items 里还有一句 CHECK (type IN ('lost', 'found'))，
// 所以不能简单地找 'found'，必须精确定位到 status 那一句。
// 做法是先把空白折叠成单个空格，再匹配规范化后的约束文本。
func itemStatusAllowsFound(db *DB) (bool, error) {
	var ddl sql.NullString
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'items'`).Scan(&ddl)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil // 表还不存在，交给 schema.sql 建
	}
	if err != nil {
		return false, fmt.Errorf("读取 items 建表语句失败: %w", err)
	}

	normalized := strings.Join(strings.Fields(ddl.String), " ")
	return strings.Contains(normalized, "status IN ('open', 'found', 'closed')"), nil
}

// ensureColumn 在列不存在时执行 ALTER TABLE ADD COLUMN，返回是否真的改动了表。
func ensureColumn(db *DB, table, column, definition string) (bool, error) {
	exists, err := columnExists(db, table, column)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}

	statement := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)
	if _, err := db.Exec(statement); err != nil {
		return false, fmt.Errorf("%s 失败: %w", statement, err)
	}
	return true, nil
}

func columnExists(db *DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, fmt.Errorf("读取 %s 表结构失败: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid       int
			name      string
			ctype     string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dfltValue, &pk); err != nil {
			return false, err
		}
		if strings.EqualFold(name, column) {
			return true, nil
		}
	}
	return false, rows.Err()
}

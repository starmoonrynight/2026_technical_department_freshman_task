-- ============================================================================
-- 校园失物招领系统 · SQLite 初始化脚本（Go 版）
--
-- 执行方式：服务启动时自动执行（internal/database/database.go 用 go:embed 嵌入本文件），
--           语句全部是幂等的 CREATE ... IF NOT EXISTS，可以安全地重复运行。
--
-- 兼容说明：Node 版后端与现有前端仍在读取 users.username / users.nickname，
--           因此本表同时保留这两列作为「兼容列」，由 store 层与 student_id / name
--           一起写入、保持一致；等前端切换到学号后再删除即可。
-- ============================================================================

PRAGMA journal_mode = WAL;   -- 读写并发：读不阻塞写
PRAGMA foreign_keys = ON;    -- 打开外键约束，items.user_id 才能真正级联
PRAGMA busy_timeout = 5000;  -- 遇到写锁时最多等 5 秒再报错

-- ---------------------------------------------------------------------------
-- users：用户
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  student_id    TEXT    NOT NULL DEFAULT '',                    -- 学号，登录凭据
  password_hash TEXT    NOT NULL,                               -- scrypt$N$r$p$salt$hash
  name          TEXT    NOT NULL DEFAULT '',                    -- 姓名
  contact       TEXT    NOT NULL DEFAULT '',                    -- 联系方式（邮箱 / 手机）
  role          TEXT    NOT NULL DEFAULT 'user'   CHECK (role   IN ('user', 'admin')),
  status        TEXT    NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
  created_at    TEXT    NOT NULL,                               -- ISO 8601 字符串

  -- 兼容列，与 student_id / name 同步写入，供 Node 版后端与旧前端使用
  username      TEXT    NOT NULL UNIQUE,
  nickname      TEXT    NOT NULL DEFAULT ''
);

-- ---------------------------------------------------------------------------
-- items：失物 / 招领信息，通过 user_id 关联发布者
--   status 三态：open = 寻找中（默认） / found = 已找到 / closed = 已结束
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS items (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  type         TEXT    NOT NULL CHECK (type         IN ('lost', 'found')),
  title        TEXT    NOT NULL,
  category     TEXT    NOT NULL DEFAULT '其他',
  description  TEXT    NOT NULL DEFAULT '',
  location     TEXT    NOT NULL DEFAULT '',
  storage_place TEXT   NOT NULL DEFAULT '',                     -- 寄放处：捡到物品后存放在哪里（选填）
  happened_at  TEXT    NOT NULL DEFAULT '',
  contact      TEXT    NOT NULL DEFAULT '',
  image_url    TEXT    NOT NULL DEFAULT '',
  status       TEXT    NOT NULL DEFAULT 'open'
               CHECK (status IN ('open', 'found', 'closed')),
  audit_status TEXT    NOT NULL DEFAULT 'pending' CHECK (audit_status IN ('pending', 'approved', 'rejected')),
  audit_remark TEXT    NOT NULL DEFAULT '',
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at   TEXT    NOT NULL,
  updated_at   TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_items_audit  ON items(audit_status);
CREATE INDEX IF NOT EXISTS idx_items_type   ON items(type);
CREATE INDEX IF NOT EXISTS idx_items_user   ON items(user_id);
CREATE INDEX IF NOT EXISTS idx_items_status ON items(status);

-- ---------------------------------------------------------------------------
-- sessions：登录会话（服务端可撤销，见 README「为什么用 Session 而不是 JWT」）
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT    PRIMARY KEY,                               -- 32 字节随机数的十六进制
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at TEXT    NOT NULL,
  expires_at TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);

-- ---------------------------------------------------------------------------
-- schema_migrations：迁移记录，保证升级脚本只执行一次
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT    NOT NULL,
  applied_at TEXT    NOT NULL
);

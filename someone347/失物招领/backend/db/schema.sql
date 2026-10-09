PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS campuses (id TEXT PRIMARY KEY, name TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS users (
  id TEXT PRIMARY KEY,
  account TEXT NOT NULL UNIQUE CHECK(length(account) BETWEEN 4 AND 20),
  nickname TEXT NOT NULL CHECK(length(nickname) BETWEEN 1 AND 20),
  password_hash TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL REFERENCES users(id),
  csrf TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expires_at);
CREATE TABLE IF NOT EXISTS posts (
  id TEXT PRIMARY KEY,
  owner_id TEXT NOT NULL REFERENCES users(id),
  type TEXT NOT NULL CHECK(type IN ('LOST','FOUND')),
  title TEXT NOT NULL CHECK(length(title) BETWEEN 2 AND 30),
  category TEXT NOT NULL CHECK(category IN ('CARD','DIGITAL','KEY','CLOTHING','BOOK','OTHER')),
  campus_id TEXT NOT NULL REFERENCES campuses(id),
  location TEXT NOT NULL,
  event_date TEXT NOT NULL,
  event_period TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL,
  storage_location TEXT NOT NULL DEFAULT '',
  illustration TEXT,
  status TEXT NOT NULL DEFAULT 'OPEN' CHECK(status IN ('OPEN','RESOLVED','CLOSED')),
  deleted_at TEXT,
  version INTEGER NOT NULL DEFAULT 1 CHECK(version>0),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_posts_feed ON posts(type,status,created_at DESC,id DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_posts_owner ON posts(owner_id,created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_posts_filter ON posts(campus_id,category,event_date) WHERE deleted_at IS NULL;
CREATE TABLE IF NOT EXISTS images (
  id TEXT PRIMARY KEY,
  owner_id TEXT NOT NULL REFERENCES users(id),
  mime TEXT NOT NULL CHECK(mime IN ('image/png','image/jpeg')),
  filename TEXT NOT NULL UNIQUE,
  size INTEGER NOT NULL CHECK(size>0),
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS post_images (
  post_id TEXT NOT NULL REFERENCES posts(id),
  image_id TEXT NOT NULL REFERENCES images(id),
  position INTEGER NOT NULL CHECK(position BETWEEN 0 AND 2),
  PRIMARY KEY(post_id,image_id),
  UNIQUE(post_id,position)
);
CREATE TABLE IF NOT EXISTS post_requests (
  user_id TEXT NOT NULL REFERENCES users(id),
  request_key TEXT NOT NULL,
  payload_hash TEXT NOT NULL,
  post_id TEXT NOT NULL REFERENCES posts(id),
  PRIMARY KEY(user_id,request_key)
);
CREATE TABLE IF NOT EXISTS conversations (
  id TEXT PRIMARY KEY,
  post_id TEXT NOT NULL REFERENCES posts(id),
  initiator_id TEXT NOT NULL REFERENCES users(id),
  owner_id TEXT NOT NULL REFERENCES users(id),
  created_at TEXT NOT NULL,
  last_activity_at TEXT NOT NULL,
  CHECK(initiator_id<>owner_id),
  UNIQUE(post_id,initiator_id,owner_id)
);
CREATE INDEX IF NOT EXISTS idx_conversations_owner ON conversations(owner_id,last_activity_at DESC);
CREATE INDEX IF NOT EXISTS idx_conversations_initiator ON conversations(initiator_id,last_activity_at DESC);
CREATE TABLE IF NOT EXISTS messages (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  sender_id TEXT NOT NULL REFERENCES users(id),
  content TEXT NOT NULL CHECK(length(content) BETWEEN 1 AND 500),
  client_message_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(sender_id,client_message_id)
);
CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id,id);
CREATE TABLE IF NOT EXISTS conversation_reads (
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  user_id TEXT NOT NULL REFERENCES users(id),
  last_read_message_id INTEGER NOT NULL REFERENCES messages(id),
  PRIMARY KEY(conversation_id,user_id)
);

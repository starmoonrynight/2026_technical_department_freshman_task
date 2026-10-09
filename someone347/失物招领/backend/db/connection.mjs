import crypto from "node:crypto";
import { DatabaseSync } from "node:sqlite";
import fs from "node:fs";
import path from "node:path";

export function transaction(db, callback) {
  db.exec("BEGIN IMMEDIATE");
  try {
    const value = callback();
    db.exec("COMMIT");
    return value;
  } catch (error) {
    db.exec("ROLLBACK");
    throw error;
  }
}
export function openDatabase(config) {
  fs.mkdirSync(config.dataDir, { recursive: true });
  const uploadsDir = path.join(config.dataDir, "uploads");
  fs.mkdirSync(uploadsDir, { recursive: true });
  const db = new DatabaseSync(path.join(config.dataDir, "app.sqlite"), {
    timeout: 5000,
  });
  db.exec(
    "PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;",
  );
  if (
    !db
      .prepare(
        "SELECT name FROM sqlite_master WHERE type='table' AND name='schema_migrations'",
      )
      .get()
  ) {
    transaction(db, () => {
      db.exec(
        fs.readFileSync(new URL("./schema.sql", import.meta.url), "utf8"),
      );
      db.prepare("INSERT INTO schema_migrations VALUES(1,?)").run(
        new Date().toISOString(),
      );
    });
  }
  db.prepare("INSERT OR IGNORE INTO campuses VALUES(?,?)").run(
    "xiasha",
    "下沙校区",
  );
  db.prepare("INSERT OR IGNORE INTO campuses VALUES(?,?)").run(
    "qing-shan",
    "青山湖校区（示例）",
  );
  return db;
}

/** Imports the earlier demo once. Keeps store.json untouched as a backup. */
export function importSnapshot(db, config, source) {
  transaction(db, () => {
    for (const u of source.users || [])
      db.prepare("INSERT INTO users VALUES(?,?,?,?,?)").run(
        u.id,
        u.account,
        u.nickname,
        u.password_hash,
        u.created_at,
      );
    for (const p of source.posts || []) {
      db.prepare("INSERT OR IGNORE INTO campuses VALUES(?,?)").run(
        p.campus_id,
        p.campus_id,
      );
      db.prepare(
        `INSERT INTO posts(id,owner_id,type,title,category,campus_id,location,event_date,event_period,description,storage_location,illustration,status,deleted_at,version,created_at,updated_at)
        VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
      ).run(
        p.id,
        p.owner_id,
        p.type,
        p.title,
        p.category,
        p.campus_id,
        p.location,
        p.event_date,
        p.event_period || "",
        p.description,
        p.type === "FOUND" ? p.storage_location || "" : "",
        p.illustration || null,
        p.status,
        p.deleted_at || null,
        p.version || 1,
        p.created_at,
        p.updated_at,
      );
    }
    for (const image of source.images || []) {
      // Never trust legacy filenames; new random IDs become local filenames.
      const bytes = Buffer.from(image.data, "base64");
      const filename = `${crypto.randomUUID()}.${image.mime === "image/png" ? "png" : "jpg"}`;
      fs.writeFileSync(path.join(config.dataDir, "uploads", filename), bytes);
      db.prepare("INSERT INTO images VALUES(?,?,?,?,?,?)").run(
        image.id,
        image.owner_id,
        image.mime,
        filename,
        bytes.length,
        new Date().toISOString(),
      );
    }
    for (const p of source.posts || [])
      (p.image_ids || []).forEach((imageId, i) =>
        db
          .prepare("INSERT INTO post_images VALUES(?,?,?)")
          .run(p.id, imageId, i),
      );
    for (const c of source.conversations || [])
      db.prepare("INSERT INTO conversations VALUES(?,?,?,?,?,?)").run(
        c.id,
        c.post_id,
        c.initiator_id,
        c.owner_id,
        c.created_at,
        c.last_activity_at,
      );
    for (const m of source.messages || [])
      db.prepare("INSERT INTO messages VALUES(?,?,?,?,?,?)").run(
        m.id,
        m.conversation_id,
        m.sender_id,
        m.content,
        m.client_message_id,
        m.created_at,
      );
    for (const r of source.reads || [])
      if (r.last_read_message_id > 0)
        db.prepare("INSERT INTO conversation_reads VALUES(?,?,?)").run(
          r.conversation_id,
          r.user_id,
          r.last_read_message_id,
        );
    for (const r of source.idempotency || [])
      db.prepare("INSERT INTO post_requests VALUES(?,?,?,?)").run(
        r.user_id,
        r.key,
        "legacy",
        r.post_id,
      );
    db.prepare("INSERT INTO schema_migrations VALUES(2,?)").run(
      new Date().toISOString(),
    );
  });
}

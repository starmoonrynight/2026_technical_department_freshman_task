import { randomUUID } from "node:crypto";
import { invariant } from "../lib/errors.mjs";
import {
  now,
  text,
  integer,
  pagination,
  listResult,
} from "../lib/validation.mjs";
import { transaction } from "../db/connection.mjs";

export class MessageService {
  constructor(db, posts) {
    this.db = db;
    this.posts = posts;
  }
  get(id, user) {
    const c = this.db.prepare("SELECT * FROM conversations WHERE id=?").get(id);
    invariant(c, 404, "会话不存在");
    invariant(
      [c.initiator_id, c.owner_id].includes(user.id),
      403,
      "无权访问此会话",
    );
    return c;
  }
  detail(id, user) {
    const c = this.get(id, user);
    const other = this.db
      .prepare("SELECT id,nickname FROM users WHERE id=?")
      .get(c.owner_id === user.id ? c.initiator_id : c.owner_id);
    const post = this.posts.get(c.post_id, true);
    // Deleted posts keep only a minimal context in existing private conversations.
    if (post.deleted_at) {
      post.description = "";
      post.image_ids = [];
      post.image_urls = [];
      post.cover_url = null;
    }
    return { ...c, other, post };
  }
  create(postId, user) {
    return transaction(this.db, () => {
      const p = this.posts.get(postId);
      invariant(p.owner_id !== user.id, 400, "不能与自己创建会话");
      invariant(
        p.status === "OPEN",
        409,
        "帖子已结束，请从消息列表继续已有对话",
      );
      let c = this.db
        .prepare(
          "SELECT id FROM conversations WHERE post_id=? AND initiator_id=? AND owner_id=?",
        )
        .get(postId, user.id, p.owner_id);
      if (!c) {
        c = { id: randomUUID() };
        const t = now();
        this.db
          .prepare("INSERT INTO conversations VALUES(?,?,?,?,?,?)")
          .run(c.id, postId, user.id, p.owner_id, t, t);
      }
      return { conversation_id: c.id };
    });
  }
  unread(id, userId) {
    return this.db
      .prepare(
        `SELECT COUNT(*) n FROM messages WHERE conversation_id=? AND sender_id<>? AND id>COALESCE((SELECT last_read_message_id FROM conversation_reads WHERE conversation_id=? AND user_id=?),0)`,
      )
      .get(id, userId, id, userId).n;
  }
  list(query, user) {
    const paging = pagination(query);
    const total = this.db
      .prepare(
        "SELECT COUNT(*) n FROM conversations WHERE initiator_id=? OR owner_id=?",
      )
      .get(user.id, user.id).n;
    const rows = this.db
      .prepare(
        "SELECT id FROM conversations WHERE initiator_id=? OR owner_id=? ORDER BY last_activity_at DESC,id DESC LIMIT ? OFFSET ?",
      )
      .all(
        user.id,
        user.id,
        paging.pageSize,
        (paging.page - 1) * paging.pageSize,
      );
    const items = rows.map((c) => ({
      ...this.detail(c.id, user),
      last_message:
        this.db
          .prepare(
            "SELECT * FROM messages WHERE conversation_id=? ORDER BY id DESC LIMIT 1",
          )
          .get(c.id) || null,
      unread_count: this.unread(c.id, user.id),
    }));
    const unread_total = this.db
      .prepare(
        `SELECT COUNT(*) n FROM messages m JOIN conversations c ON c.id=m.conversation_id LEFT JOIN conversation_reads r ON r.conversation_id=c.id AND r.user_id=? WHERE (c.initiator_id=? OR c.owner_id=?) AND m.sender_id<>? AND m.id>COALESCE(r.last_read_message_id,0)`,
      )
      .get(user.id, user.id, user.id, user.id).n;
    return { ...listResult(items, total, paging), unread_total };
  }
  history(id, query, user) {
    this.get(id, user);
    invariant(
      !(query.has("before_id") && query.has("after_id")),
      400,
      "分页条件不能同时使用",
    );
    const forward = query.has("after_id");
    const rawCursor = query.get(forward ? "after_id" : "before_id");
    const cursor =
      forward && rawCursor === "0" ? 0 : integer(rawCursor, "cursor", null);
    const limit = integer(query.get("limit"), "limit", 30, 100);
    let items = this.db
      .prepare(
        `SELECT * FROM messages WHERE conversation_id=? ${cursor ? "AND id" + (forward ? ">" : "<") + "?" : ""} ORDER BY id ${forward ? "ASC" : "DESC"} LIMIT ?`,
      )
      .all(id, ...(cursor ? [cursor] : []), limit + 1);
    const has_more = items.length > limit;
    items = items.slice(0, limit);
    if (!forward) items.reverse();
    return {
      items,
      has_more,
      next_cursor: (forward ? items.at(-1) : items[0])?.id || null,
    };
  }
  send(id, body, user) {
    const content = text(body.content, "content", 1, 500),
      key = text(body.client_message_id, "client_message_id", 1, 100);
    return transaction(this.db, () => {
      this.get(id, user);
      const old = this.db
        .prepare(
          "SELECT * FROM messages WHERE sender_id=? AND client_message_id=?",
        )
        .get(user.id, key);
      if (old) {
        invariant(
          old.conversation_id === id && old.content === content,
          409,
          "消息标识已用于不同内容",
        );
        return old;
      }
      const stamp = now();
      const result = this.db
        .prepare(
          "INSERT INTO messages(conversation_id,sender_id,content,client_message_id,created_at) VALUES(?,?,?,?,?)",
        )
        .run(id, user.id, content, key, stamp);
      this.db
        .prepare("UPDATE conversations SET last_activity_at=? WHERE id=?")
        .run(stamp, id);
      return this.db
        .prepare("SELECT * FROM messages WHERE id=?")
        .get(result.lastInsertRowid);
    });
  }
  read(id, body, user) {
    return transaction(this.db, () => {
      this.get(id, user);
      invariant(
        Number.isSafeInteger(body.last_read_message_id),
        400,
        "无效已读位置",
      );
      invariant(
        this.db
          .prepare("SELECT id FROM messages WHERE id=? AND conversation_id=?")
          .get(body.last_read_message_id, id),
        400,
        "无效已读位置",
      );
      this.db
        .prepare(
          `INSERT INTO conversation_reads VALUES(?,?,?) ON CONFLICT(conversation_id,user_id) DO UPDATE SET last_read_message_id=MAX(last_read_message_id,excluded.last_read_message_id)`,
        )
        .run(id, user.id, body.last_read_message_id);
      return this.db
        .prepare(
          "SELECT * FROM conversation_reads WHERE conversation_id=? AND user_id=?",
        )
        .get(id, user.id);
    });
  }
}

import { randomUUID } from "node:crypto";
import { invariant } from "../lib/errors.mjs";
import {
  now,
  validDate,
  TYPES,
  STATUSES,
  CATEGORIES,
  pagination,
  listResult,
  validatePost,
  text,
} from "../lib/validation.mjs";
import { digest } from "../lib/password.mjs";
import { transaction } from "../db/connection.mjs";

const SELECT = `SELECT p.*,u.nickname AS author_name,c.name AS campus_name FROM posts p JOIN users u ON u.id=p.owner_id JOIN campuses c ON c.id=p.campus_id`;
export class PostService {
  constructor(db) {
    this.db = db;
  }
  serialize(row) {
    const p = { ...row };
    p.author = { id: p.owner_id, nickname: p.author_name };
    delete p.author_name;
    p.image_ids = this.db
      .prepare(
        "SELECT image_id FROM post_images WHERE post_id=? ORDER BY position",
      )
      .all(p.id)
      .map((x) => x.image_id);
    p.image_urls = p.image_ids.map((id) => `/media/${id}`);
    p.cover_url = p.image_urls[0] || null;
    return p;
  }
  get(id, includeDeleted = false) {
    const p = this.db
      .prepare(
        `${SELECT} WHERE p.id=?${includeDeleted ? "" : " AND p.deleted_at IS NULL"}`,
      )
      .get(id);
    invariant(p, 404, "信息已删除或不存在");
    return this.serialize(p);
  }
  list(query, ownerId) {
    const paging = pagination(query);
    const conditions = ["p.deleted_at IS NULL"],
      params = [];
    if (ownerId) {
      conditions.push("p.owner_id=?");
      params.push(ownerId);
    }
    for (const [field, choices, fallback] of [
      ["type", TYPES, ""],
      ["status", [...STATUSES, "ALL"], ownerId ? "ALL" : "OPEN"],
      ["category", CATEGORIES, ""],
    ]) {
      const value = query.get(field) || fallback;
      if (!value) continue;
      invariant(choices.includes(value), 400, `${field}参数不正确`);
      if (value !== "ALL") {
        conditions.push(`p.${field}=?`);
        params.push(value);
      }
    }
    const campus = query.get("campus_id");
    if (campus) {
      invariant(
        this.db.prepare("SELECT id FROM campuses WHERE id=?").get(campus),
        400,
        "校区不存在",
      );
      conditions.push("p.campus_id=?");
      params.push(campus);
    }
    const from = query.get("date_from"),
      to = query.get("date_to");
    for (const value of [from, to])
      if (value) invariant(validDate(value), 400, "日期格式错误");
    invariant(!from || !to || from <= to, 400, "开始日期不能晚于结束日期");
    if (from) {
      conditions.push("p.event_date>=?");
      params.push(from);
    }
    if (to) {
      conditions.push("p.event_date<=?");
      params.push(to);
    }
    const keyword = (query.get("keyword") || "").trim();
    invariant(keyword.length <= 100, 400, "关键词不能超过100字");
    if (keyword) {
      // Escape wildcard characters so a literal '%' does not match every post.
      const pattern = "%" + keyword.replace(/[\\%_]/g, "\\$&") + "%";
      conditions.push(
        "(p.title LIKE ? ESCAPE '\\' OR p.description LIKE ? ESCAPE '\\')",
      );
      params.push(pattern, pattern);
    }
    const where = conditions.join(" AND ");
    const total = this.db
      .prepare(`SELECT COUNT(*) AS n FROM posts p WHERE ${where}`)
      .get(...params).n;
    const items = this.db
      .prepare(
        `${SELECT} WHERE ${where} ORDER BY p.created_at DESC,p.id DESC LIMIT ? OFFSET ?`,
      )
      .all(...params, paging.pageSize, (paging.page - 1) * paging.pageSize)
      .map((p) => this.serialize(p));
    return listResult(items, total, paging);
  }
  attachImages(postId, imageIds) {
    this.db.prepare("DELETE FROM post_images WHERE post_id=?").run(postId);
    imageIds.forEach((imageId, i) =>
      this.db
        .prepare("INSERT INTO post_images VALUES(?,?,?)")
        .run(postId, imageId, i),
    );
  }
  create(body, user, key) {
    text(key, "Idempotency-Key", 1, 100);
    const p = validatePost(body, user, this.db);
    const payloadHash = digest(JSON.stringify(p));
    return transaction(this.db, () => {
      const old = this.db
        .prepare(
          "SELECT * FROM post_requests WHERE user_id=? AND request_key=?",
        )
        .get(user.id, key);
      if (old) {
        invariant(
          old.payload_hash === "legacy" || old.payload_hash === payloadHash,
          409,
          "提交标识已用于不同内容，请重新提交",
        );
        return this.get(old.post_id);
      }
      const id = randomUUID(),
        stamp = now();
      this.db
        .prepare(
          `INSERT INTO posts(id,owner_id,type,title,category,campus_id,location,event_date,event_period,description,storage_location,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
        )
        .run(
          id,
          user.id,
          p.type,
          p.title,
          p.category,
          p.campus_id,
          p.location,
          p.event_date,
          p.event_period,
          p.description,
          p.storage_location,
          stamp,
          stamp,
        );
      this.attachImages(id, p.image_ids);
      this.db
        .prepare("INSERT INTO post_requests VALUES(?,?,?,?)")
        .run(user.id, key, payloadHash, id);
      return this.get(id);
    });
  }
  mutate(id, body, user, action) {
    return transaction(this.db, () => {
      const p = this.get(id);
      invariant(p.owner_id === user.id, 403, "只能管理自己的帖子");
      invariant(
        Number.isSafeInteger(body.version) && body.version === p.version,
        409,
        "内容已被更新，请刷新后重试",
      );
      if (action === "delete")
        this.db
          .prepare(
            "UPDATE posts SET deleted_at=?,updated_at=?,version=version+1 WHERE id=? AND version=?",
          )
          .run(now(), now(), id, body.version);
      else if (action === "status") {
        invariant(
          STATUSES.includes(body.status) &&
            (p.status === "OPEN"
              ? body.status !== "OPEN"
              : body.status === "OPEN"),
          409,
          "不支持此状态变更",
        );
        this.db
          .prepare(
            "UPDATE posts SET status=?,updated_at=?,version=version+1 WHERE id=? AND version=?",
          )
          .run(body.status, now(), id, body.version);
      } else {
        const v = validatePost(body, user, this.db, p);
        this.db
          .prepare(
            `UPDATE posts SET title=?,category=?,campus_id=?,location=?,event_date=?,event_period=?,description=?,storage_location=?,updated_at=?,version=version+1 WHERE id=? AND version=?`,
          )
          .run(
            v.title,
            v.category,
            v.campus_id,
            v.location,
            v.event_date,
            v.event_period,
            v.description,
            v.storage_location,
            now(),
            id,
            body.version,
          );
        this.attachImages(id, v.image_ids);
      }
      return action === "delete" ? { deleted: true } : this.get(id);
    });
  }
}

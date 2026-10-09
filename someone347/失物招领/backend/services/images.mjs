import fs from "node:fs";
import path from "node:path";
import { randomUUID } from "node:crypto";
import { invariant } from "../lib/errors.mjs";
import { now } from "../lib/validation.mjs";

export class ImageService {
  constructor(db, config) {
    this.db = db;
    this.dir = path.join(config.dataDir, "uploads");
  }
  upload(body, user) {
    const match =
      typeof body.data_url === "string" &&
      /^data:(image\/(?:jpeg|png));base64,([A-Za-z0-9+/=]+)$/.exec(
        body.data_url,
      );
    invariant(match, 400, "仅支持JPG、PNG图片");
    const bytes = Buffer.from(match[2], "base64");
    invariant(bytes.length <= 10 * 1024 * 1024, 413, "单张图片不能超过10MB");
    const png = bytes
      .subarray(0, 8)
      .equals(Buffer.from([137, 80, 78, 71, 13, 10, 26, 10]));
    const jpeg = bytes[0] === 255 && bytes[1] === 216 && bytes[2] === 255;
    invariant(
      match[1] === "image/png" ? png : jpeg,
      400,
      "文件内容不是有效图片",
    );
    const id = randomUUID(),
      filename = id + (png ? ".png" : ".jpg"),
      file = path.join(this.dir, filename);
    fs.writeFileSync(file, bytes, { flag: "wx" });
    try {
      this.db
        .prepare("INSERT INTO images VALUES(?,?,?,?,?,?)")
        .run(id, user.id, match[1], filename, bytes.length, now());
    } catch (e) {
      fs.unlinkSync(file);
      throw e;
    }
    return { image_id: id, url: `/media/${id}` };
  }
  get(id, user) {
    const image = this.db.prepare("SELECT * FROM images WHERE id=?").get(id);
    invariant(image, 404, "图片不存在");
    const published = this.db
      .prepare(
        "SELECT 1 FROM post_images i JOIN posts p ON p.id=i.post_id WHERE image_id=? AND p.deleted_at IS NULL",
      )
      .get(id);
    invariant(published || user?.id === image.owner_id, 404, "图片不存在");
    return {
      mime: image.mime,
      bytes: fs.readFileSync(path.join(this.dir, image.filename)),
    };
  }
}

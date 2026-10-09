import { randomUUID } from "node:crypto";
import { fail, invariant } from "../lib/errors.mjs";
import { text, now } from "../lib/validation.mjs";
import {
  hashPassword,
  verifyPassword,
  digest,
  randomToken,
} from "../lib/password.mjs";
import { transaction } from "../db/connection.mjs";

export class AuthService {
  constructor(db, config) {
    this.db = db;
    this.config = config;
    this.attempts = new Map();
  }
  publicUser(u) {
    return { id: u.id, account: u.account, nickname: u.nickname };
  }
  read(cookie) {
    if (!cookie) return null;
    const session = this.db
      .prepare(
        `SELECT s.*,u.id,u.account,u.nickname FROM sessions s JOIN users u ON u.id=s.user_id WHERE token_hash=? AND expires_at>?`,
      )
      .get(digest(cookie), Date.now());
    return session || null;
  }
  require(session) {
    invariant(session, 401, "请先登录");
    return session;
  }
  rateLimit(ip) {
    const t = Date.now();
    for (const [k, v] of this.attempts)
      if (v.until <= t) this.attempts.delete(k);
    const attempt = this.attempts.get(ip) || { count: 0, until: t + 600000 };
    invariant(
      attempt.count < this.config.loginLimit,
      429,
      "登录尝试过于频繁，请10分钟后再试",
    );
    attempt.count++;
    this.attempts.set(ip, attempt);
  }
  async register(body) {
    invariant(
      typeof body.account === "string" &&
        /^[A-Za-z0-9_]{4,20}$/.test(body.account),
      400,
      "账号需为4—20位字母、数字或下划线",
      { account: "账号格式不正确" },
    );
    const nickname = text(body.nickname, "nickname", 1, 20);
    invariant(
      typeof body.password === "string" &&
        body.password.length >= 8 &&
        body.password.length <= 64,
      400,
      "密码需为8—64位",
      { password: "密码长度不符合要求" },
    );
    const passwordHash = await hashPassword(body.password);
    const u = {
      id: randomUUID(),
      account: body.account,
      nickname,
      password_hash: passwordHash,
      created_at: now(),
    };
    try {
      this.db
        .prepare("INSERT INTO users VALUES(?,?,?,?,?)")
        .run(u.id, u.account, u.nickname, u.password_hash, u.created_at);
    } catch (e) {
      if (
        this.db.prepare("SELECT id FROM users WHERE account=?").get(u.account)
      )
        fail(409, "账号已经被使用", { account: "账号已存在" });
      throw e;
    }
    return u;
  }
  async login(body) {
    invariant(
      typeof body.account === "string" &&
        typeof body.password === "string" &&
        body.password.length <= 64,
      400,
      "账号或密码错误",
    );
    const u = this.db
      .prepare("SELECT * FROM users WHERE account=?")
      .get(body.account);
    // Compute a hash even for unknown accounts to avoid a trivial timing distinction.
    const encoded =
      u?.password_hash || "00000000000000000000000000000000:" + "00".repeat(64);
    const valid = await verifyPassword(body.password, encoded);
    invariant(u && valid, 401, "账号或密码错误");
    return u;
  }
  establish(u, oldCookie, res) {
    const token = randomToken(),
      csrf = randomToken();
    transaction(this.db, () => {
      if (oldCookie)
        this.db
          .prepare("DELETE FROM sessions WHERE token_hash=?")
          .run(digest(oldCookie));
      this.db
        .prepare("DELETE FROM sessions WHERE expires_at<=?")
        .run(Date.now());
      this.db
        .prepare("INSERT INTO sessions VALUES(?,?,?,?)")
        .run(digest(token), u.id, csrf, Date.now() + this.config.sessionTtl);
    });
    res.setHeader(
      "Set-Cookie",
      this.cookie(token, Math.floor(this.config.sessionTtl / 1000)),
    );
    return { user: this.publicUser(u), csrf };
  }
  cookie(token, age) {
    return `hdu_sid=${token}; HttpOnly; SameSite=Lax; Path=/; Max-Age=${age}${this.config.secureCookie ? "; Secure" : ""}`;
  }
  logout(session, res) {
    this.db
      .prepare("DELETE FROM sessions WHERE token_hash=?")
      .run(session.token_hash);
    res.setHeader("Set-Cookie", this.cookie("", 0));
    return { logged_out: true };
  }
}

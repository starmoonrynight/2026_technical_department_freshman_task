import http from "node:http";
import fs from "node:fs";
import path from "node:path";
import { openDatabase, importSnapshot } from "./db/connection.mjs";
import { seed } from "./db/seed.mjs";
import { AuthService } from "./services/auth.mjs";
import { PostService } from "./services/posts.mjs";
import { MessageService } from "./services/messages.mjs";
import { ImageService } from "./services/images.mjs";
import { fail, invariant } from "./lib/errors.mjs";

async function readBody(req, limit) {
  invariant(
    (req.headers["content-type"] || "").split(";")[0] === "application/json",
    415,
    "请使用 application/json",
  );
  const chunks = [];
  let length = 0;
  for await (const chunk of req) {
    length += chunk.length;
    invariant(length <= limit, 413, "请求内容过大");
    chunks.push(chunk);
  }
  let value;
  try {
    value = JSON.parse(Buffer.concat(chunks).toString() || "{}");
  } catch {
    fail(400, "请求格式错误");
  }
  invariant(
    value && typeof value === "object" && !Array.isArray(value),
    400,
    "请求内容必须是JSON对象",
  );
  return value;
}

/** HTTP adapter. Business rules live in services; SQLite persists all shared state. */
export function createApp(config) {
  const db = openDatabase(config);
  if (
    !db.prepare("SELECT 1 FROM users LIMIT 1").get() &&
    !db.prepare("SELECT 1 FROM schema_migrations WHERE version=2").get()
  ) {
    const legacy = path.join(config.dataDir, "store.json");
    if (fs.existsSync(legacy))
      importSnapshot(db, config, JSON.parse(fs.readFileSync(legacy, "utf8")));
    else if (config.seedDemo) importSnapshot(db, config, seed());
  }
  const auth = new AuthService(db, config),
    posts = new PostService(db),
    messages = new MessageService(db, posts),
    images = new ImageService(db, config);
  const routes = [];
  const route = (method, pattern, handler) =>
    routes.push({
      method,
      pattern,
      regex: new RegExp("^" + pattern.replace(/:[a-z_]+/g, "([^/]+)") + "$"),
      handler,
    });
  route("GET", "/api/health", () => ({
    status: db.prepare("SELECT 1 ok").get().ok === 1 ? "ok" : "error",
  }));
  route("GET", "/api/campuses", () =>
    db.prepare("SELECT * FROM campuses ORDER BY rowid").all(),
  );
  for (const action of ["register", "login"])
    route("POST", `/api/auth/${action}`, async (c) => {
      auth.rateLimit(c.req.socket.remoteAddress);
      return auth.establish(await auth[action](c.body), c.cookie, c.res);
    });
  route("GET", "/api/auth/me", (c) => ({
    user: auth.publicUser(c.user()),
    csrf: c.session.csrf,
  }));
  route("POST", "/api/auth/logout", (c) => auth.logout(c.user(), c.res));
  route("GET", "/api/posts", (c) => posts.list(c.query));
  route("GET", "/api/me/posts", (c) => posts.list(c.query, c.user().id));
  route("POST", "/api/posts", (c) =>
    posts.create(c.body, c.user(), c.req.headers["idempotency-key"]),
  );
  route("GET", "/api/posts/:id", (c, id) => posts.get(id));
  route("PATCH", "/api/posts/:id", (c, id) =>
    posts.mutate(id, c.body, c.user(), "edit"),
  );
  route("DELETE", "/api/posts/:id", (c, id) =>
    posts.mutate(id, c.body, c.user(), "delete"),
  );
  route("PATCH", "/api/posts/:id/status", (c, id) =>
    posts.mutate(id, c.body, c.user(), "status"),
  );
  route("POST", "/api/images", (c) => images.upload(c.body, c.user()));
  route("POST", "/api/posts/:id/conversations", (c, id) =>
    messages.create(id, c.user()),
  );
  route("GET", "/api/conversations", (c) => messages.list(c.query, c.user()));
  route("GET", "/api/conversations/:id", (c, id) =>
    messages.detail(id, c.user()),
  );
  route("GET", "/api/conversations/:id/messages", (c, id) =>
    messages.history(id, c.query, c.user()),
  );
  route("POST", "/api/conversations/:id/messages", (c, id) =>
    messages.send(id, c.body, c.user()),
  );
  route("POST", "/api/conversations/:id/read", (c, id) =>
    messages.read(id, c.body, c.user()),
  );
  const server = http.createServer(async (req, res) => {
    res.setHeader("X-Content-Type-Options", "nosniff");
    res.setHeader("Referrer-Policy", "same-origin");
    res.setHeader("X-Frame-Options", "DENY");
    const send = (status, value) => {
      res.writeHead(status, {
        "Content-Type": "application/json; charset=utf-8",
        "Cache-Control": "no-store",
      });
      res.end(JSON.stringify(value));
    };
    try {
      const origin = config.publicOrigin || `http://${req.headers.host}`;
      const url = new URL(req.url, origin),
        pathname = url.pathname;
      const cookie = req.headers.cookie
        ?.split(";")
        .map((x) => x.trim())
        .find((x) => x.startsWith("hdu_sid="))
        ?.slice(8);
      const session = auth.read(cookie);
      if (!pathname.startsWith("/api/")) {
        invariant(
          ["GET", "HEAD"].includes(req.method),
          405,
          "不支持此请求方法",
        );
        if (pathname.startsWith("/media/")) {
          const image = images.get(pathname.slice(7), session);
          res.writeHead(200, {
            "Content-Type": image.mime,
            "Cache-Control": "private, no-store",
          });
          res.end(req.method === "HEAD" ? undefined : image.bytes);
          return;
        }
        let relative;
        try {
          relative = decodeURIComponent(
            pathname === "/" ? "/index.html" : pathname,
          );
        } catch {
          fail(400, "路径格式错误");
        }
        const base = path.resolve(config.frontendDir),
          file = path.resolve(base, "." + relative);
        invariant(
          file.startsWith(base + path.sep) &&
            fs.existsSync(file) &&
            fs.statSync(file).isFile(),
          404,
          "页面不存在",
        );
        const mime =
          {
            ".html": "text/html; charset=utf-8",
            ".css": "text/css; charset=utf-8",
            ".js": "text/javascript; charset=utf-8",
            ".svg": "image/svg+xml",
          }[path.extname(file)] || "application/octet-stream";
        res.writeHead(200, {
          "Content-Type": mime,
          "Cache-Control": "no-cache",
        });
        res.end(req.method === "HEAD" ? undefined : fs.readFileSync(file));
        return;
      }
      const matching = routes.filter((r) => r.regex.test(pathname));
      invariant(matching.length, 404, "接口不存在");
      const selected = matching.find((r) => r.method === req.method);
      if (!selected) {
        res.setHeader("Allow", matching.map((r) => r.method).join(", "));
        fail(405, "不支持此请求方法");
      }
      const write = ["POST", "PATCH", "DELETE"].includes(req.method);
      if (write) {
        invariant(
          !req.headers.origin || req.headers.origin === origin,
          403,
          "请求来源不匹配",
        );
        if (!["/api/auth/login", "/api/auth/register"].includes(pathname)) {
          auth.require(session);
          invariant(
            req.headers["x-csrf-token"] === session.csrf,
            403,
            "操作校验失败，请重新登录",
          );
        }
      }
      const body = write ? await readBody(req, config.maxBodyBytes) : {};
      const context = {
        req,
        res,
        query: url.searchParams,
        body,
        session,
        cookie,
        user: () => auth.require(session),
      };
      send(200, {
        data: await selected.handler(
          context,
          ...selected.regex.exec(pathname).slice(1),
        ),
      });
    } catch (e) {
      if (!e.status) console.error(e);
      if (!res.headersSent)
        send(e.status || 500, {
          code: e.status || 500,
          message: e.status ? e.message : "服务暂不可用，请重试",
          ...(e.field_errors ? { field_errors: e.field_errors } : {}),
        });
      else res.end();
    }
  });
  server.requestTimeout = 30000;
  server.on("close", () => db.close());
  return { server, db, routes };
}

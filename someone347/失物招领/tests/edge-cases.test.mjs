import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { once } from "node:events";
import { createApp } from "../backend/app.mjs";
import { configuration } from "../backend/config.mjs";
import { seed } from "../backend/db/seed.mjs";

async function fixture(t, legacy = false) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "hdu-sql-test-"));
  if (legacy)
    fs.writeFileSync(path.join(dir, "store.json"), JSON.stringify(seed()));
  let app, base;
  async function start() {
    app = createApp(configuration({ dataDir: dir, port: 0, loginLimit: 100 }));
    app.server.listen(0, "127.0.0.1");
    await once(app.server, "listening");
    base = `http://127.0.0.1:${app.server.address().port}`;
  }
  async function stop() {
    const closed = once(app.server, "close");
    app.server.close();
    app.server.closeAllConnections();
    await closed;
  }
  await start();
  t.after(async () => {
    await stop();
    fs.rmSync(dir, { recursive: true, force: true });
  });
  function client() {
    let cookie = "",
      csrf = "";
    const request = async (route, method = "GET", body, extra = {}) => {
      const response = await fetch(base + route, {
        method,
        headers: {
          "Content-Type": "application/json",
          Cookie: cookie,
          "X-CSRF-Token": csrf,
          ...extra,
        },
        ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
      });
      const next = response.headers.get("set-cookie");
      if (next) cookie = next.split(";")[0];
      const value = await response.json();
      if (value.data?.csrf) csrf = value.data.csrf;
      return { status: response.status, ...value };
    };
    request.login = (account) =>
      request("/api/auth/login", "POST", { account, password: "demo12345" });
    request.media = async (url) =>
      fetch(base + url, { headers: { Cookie: cookie } });
    return request;
  }
  return {
    client,
    dir,
    get routes() { return app.routes; },
    get db() {
      return app.db;
    },
    restart: async () => {
      await stop();
      await start();
    },
  };
}
const fields = {
  type: "FOUND",
  title: "测试字面%水杯",
  category: "OTHER",
  campus_id: "xiasha",
  location: "教学楼旁",
  event_date: "2026-01-01",
  description: "这是用于自动测试的描述",
  image_ids: [],
};
test("SQL filters, literal wildcard, pagination, validation and concurrent version conflict", async (t) => {
  const f = await fixture(t),
    a = f.client();
  await a.login("demo");
  const created = await a("/api/posts", "POST", fields, {
    "Idempotency-Key": "first",
  });
  assert.equal(created.status, 200);
  const id = created.data.id;
  assert.equal((await a("/api/posts?keyword=%25")).data.total, 1);
  assert.equal(
    (await a("/api/posts?keyword=%27%20OR%201%3D1--")).data.total,
    0,
  );
  const page1 = await a("/api/posts?page_size=1"),
    page2 = await a("/api/posts?page_size=1&page=2");
  assert.notEqual(page1.data.items[0].id, page2.data.items[0].id);
  assert.equal(page1.data.has_more, true);
  for (const query of [
    "page=-1",
    "page=1.5",
    "category=INVALID",
    "date_from=2026-02-30",
    "date_from=2026-02-01&date_to=2026-01-01",
  ])
    assert.equal((await a("/api/posts?" + query)).status, 400);
  assert.equal(
    (
      await a(
        "/api/posts",
        "POST",
        { ...fields, title: "别的标题" },
        { "Idempotency-Key": "first" },
      )
    ).status,
    409,
  );
  assert.equal((await a("/api/posts", "POST", null)).status, 400);
  const bad = await a(
    "/api/posts",
    "POST",
    { ...fields, event_date: "2099-01-01" },
    { "Idempotency-Key": "future" },
  );
  assert.equal(bad.status, 400);
  assert.ok(bad.field_errors.event_date);
  const results = await Promise.all([
    a(`/api/posts/${id}`, "PATCH", { title: "并发修改甲", version: 1 }),
    a(`/api/posts/${id}`, "PATCH", { title: "并发修改乙", version: 1 }),
  ]);
  assert.deepEqual(results.map((x) => x.status).sort(), [200, 409]);
  assert.equal((await a(`/api/posts/${id}`, "PUT", {})).status, 405);
});

test('OpenAPI describes every implemented API route and references valid schemas', async t => {
  const f=await fixture(t);
  const spec=JSON.parse(fs.readFileSync(new URL('../docs/openapi.json',import.meta.url),'utf8'));
  const actual=f.routes.map(r=>`${r.method.toLowerCase()} ${r.pattern.replace(/:([a-z_]+)/g,'{$1}')}`).sort();
  const documented=Object.entries(spec.paths).flatMap(([p,methods])=>Object.keys(methods).map(m=>`${m} ${p}`)).sort();
  assert.deepEqual(documented,actual);
  function walk(value) {
    if (!value || typeof value!=='object') return;
    if (value.$ref) assert.ok(spec.components.schemas[value.$ref.split('/').at(-1)],value.$ref);
    Object.values(value).forEach(walk);
  }
  walk(spec);
});
test("message cursors, monotonic reads, duplicate payload conflicts and private uploads", async (t) => {
  const f = await fixture(t),
    a = f.client(),
    b = f.client(),
    guest = f.client();
  await a.login("demo");
  await b.login("xiaoming");
  const cid = (await a("/api/posts/p-headphones/conversations", "POST", {}))
    .data.conversation_id;
  assert.deepEqual(
    (await a(`/api/conversations/${cid}/messages?after_id=0`)).data.items,
    [],
  );
  const sent = [];
  for (let i = 0; i < 5; i++)
    sent.push(
      (
        await b(`/api/conversations/${cid}/messages`, "POST", {
          content: "测试消息" + i,
          client_message_id: "msg-" + i,
        })
      ).data,
    );
  const latest = (await a(`/api/conversations/${cid}/messages?limit=2`)).data;
  assert.deepEqual(
    latest.items.map((m) => m.id),
    sent.slice(3).map((m) => m.id),
  );
  assert.equal(latest.has_more, true);
  const earlier = (
    await a(
      `/api/conversations/${cid}/messages?before_id=${latest.next_cursor}&limit=2`,
    )
  ).data;
  assert.deepEqual(
    earlier.items.map((m) => m.id),
    sent.slice(1, 3).map((m) => m.id),
  );
  assert.equal(
    (await a(`/api/conversations/${cid}/messages?before_id=2&after_id=1`))
      .status,
    400,
  );
  assert.equal(
    (
      await b(`/api/conversations/${cid}/messages`, "POST", {
        content: "修改同一消息",
        client_message_id: "msg-0",
      })
    ).status,
    409,
  );
  await a(`/api/conversations/${cid}/read`, "POST", {
    last_read_message_id: sent[4].id,
  });
  const read = await a(`/api/conversations/${cid}/read`, "POST", {
    last_read_message_id: sent[0].id,
  });
  assert.equal(read.data.last_read_message_id, sent[4].id);
  assert.equal(
    (
      await a(`/api/conversations/${cid}/read`, "POST", {
        last_read_message_id: 1,
      })
    ).status,
    400,
  );
  const png =
    "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a1ZkAAAAASUVORK5CYII=";
  const image = (await a("/api/images", "POST", { data_url: png })).data;
  assert.equal((await a.media(image.url)).status, 200);
  assert.equal((await guest.media(image.url)).status, 404);
  assert.equal(
    (
      await b(
        "/api/posts",
        "POST",
        { ...fields, image_ids: [image.image_id] },
        { "Idempotency-Key": "stolen" },
      )
    ).status,
    400,
  );
  const p = (
    await a(
      "/api/posts",
      "POST",
      { ...fields, image_ids: [image.image_id] },
      { "Idempotency-Key": "image-post" },
    )
  ).data;
  assert.equal(
    (await guest.media(image.url)).headers.get("content-type"),
    "image/png",
  );
  await a(`/api/posts/${p.id}`, "DELETE", { version: 1 });
  assert.equal((await guest.media(image.url)).status, 404);
});
test("legacy import runs once; data and hashed sessions survive restart; logout revokes session", async (t) => {
  const f = await fixture(t, true),
    a = f.client();
  await a.login("demo");
  const original = fs.readFileSync(path.join(f.dir, "store.json"), "utf8");
  const p = (
    await a("/api/posts", "POST", fields, { "Idempotency-Key": "persist" })
  ).data;
  const token = f.db
    .prepare("SELECT token_hash FROM sessions")
    .get().token_hash;
  assert.match(token, /^[a-f0-9]{64}$/);
  await f.restart();
  assert.equal((await a("/api/auth/me")).data.user.account, "demo");
  assert.equal((await a(`/api/posts/${p.id}`)).data.title, fields.title);
  assert.equal(f.db.prepare("SELECT COUNT(*) n FROM users").get().n, 2);
  assert.equal(
    fs.readFileSync(path.join(f.dir, "store.json"), "utf8"),
    original,
  );
  await a("/api/auth/logout", "POST", {});
  assert.equal((await a("/api/auth/me")).status, 401);
});

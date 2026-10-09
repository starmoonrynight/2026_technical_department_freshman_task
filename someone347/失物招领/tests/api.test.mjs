import { once } from "node:events";
import { DatabaseSync } from "node:sqlite";
import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
const temp = fs.mkdtempSync(path.join(os.tmpdir(), "hdu-demo-test-"));
let child, base;
before(async () => {
  child = spawn(process.execPath, ["server.mjs"], {
    cwd: root,
    env: { ...process.env, PORT: "0", DATA_DIR: temp },
    stdio: ["ignore", "pipe", "pipe"],
  });
  base = await new Promise((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error("Server startup timeout")),
      10000,
    );
    child.stdout.on("data", (data) => {
      const m = data.toString().match(/http:\/\/127\.0\.0\.1:\d+/);
      if (m) {
        clearTimeout(timer);
        resolve(m[0]);
      }
    });
    child.once("error", reject);
  });
});
after(async () => {
  if (child && child.exitCode === null) {
    const closed = once(child, "exit");
    child.kill();
    await closed;
  }
  fs.rmSync(temp, {
    recursive: true,
    force: true,
    maxRetries: 5,
    retryDelay: 100,
  });
});
function client() {
  let cookie = "",
    csrf = "";
  return async (route, method = "GET", body, extraHeaders = {}) => {
    const res = await fetch(`${base}${route}`, {
      method,
      headers: {
        "Content-Type": "application/json",
        Cookie: cookie,
        ...(csrf ? { "X-CSRF-Token": csrf } : {}),
        ...extraHeaders,
      },
      ...(body !== undefined ? { body: JSON.stringify(body) } : {}),
    });
    if (res.headers.get("set-cookie"))
      cookie = res.headers.get("set-cookie").split(";")[0];
    const value = await res.json();
    if (value.data?.csrf) csrf = value.data.csrf;
    return { status: res.status, ...value };
  };
}
test("authentication, ownership, post lifecycle, messaging and persistence", async () => {
  const guest = client(),
    ming = client(),
    mine = client(),
    stranger = client();
  assert.equal((await guest("/api/posts")).status, 200);
  assert.equal((await guest("/api/conversations")).status, 401);
  assert.equal(
    (
      await guest("/api/auth/login", "POST", {
        account: "demo",
        password: "wrong123",
      })
    ).status,
    401,
  );
  assert.equal(
    (
      await mine("/api/auth/login", "POST", {
        account: "demo",
        password: "demo12345",
      })
    ).status,
    200,
  );
  await ming("/api/auth/login", "POST", {
    account: "xiaoming",
    password: "demo12345",
  });
  const registered = await stranger("/api/auth/register", "POST", {
    account: "new_user",
    nickname: "新同学",
    password: "test12345",
  });
  assert.equal(registered.status, 200);
  assert.equal(
    (
      await guest("/api/auth/register", "POST", {
        account: "new_user",
        nickname: "重复",
        password: "test12345",
      })
    ).status,
    409,
  );
  const date = new Date().toLocaleDateString("en-CA", {
    timeZone: "Asia/Shanghai",
  });
  const fields = {
    type: "FOUND",
    title: "测试蓝色水杯",
    category: "OTHER",
    campus_id: "xiasha",
    location: "图书馆二楼",
    description: "带有一张小猫贴纸的蓝色水杯",
    event_date: date,
    event_period: "下午",
    image_ids: [],
  };
  assert.equal(
    (
      await mine(
        "/api/posts",
        "POST",
        { ...fields, title: "a" },
        { "Idempotency-Key": "bad" },
      )
    ).status,
    400,
  );
  const created = await mine("/api/posts", "POST", fields, {
    "Idempotency-Key": "post-key",
  });
  assert.equal(created.status, 200);
  const pid = created.data.id;
  assert.equal(
    (
      await mine("/api/posts", "POST", fields, {
        "Idempotency-Key": "post-key",
      })
    ).data.id,
    pid,
  );
  assert.equal(
    (await ming(`/api/posts/${pid}`, "PATCH", { ...fields, version: 1 }))
      .status,
    403,
  );
  assert.equal(
    (await mine(`/api/posts/${pid}`, "PATCH", { ...fields, version: 0 }))
      .status,
    409,
  );
  assert.equal(
    (
      await mine(`/api/posts/${pid}`, "PATCH", {
        ...fields,
        type: "LOST",
        version: 1,
      })
    ).status,
    400,
  );
  const update = await mine(`/api/posts/${pid}`, "PATCH", {
    ...fields,
    title: "测试绿色水杯",
    version: 1,
  });
  assert.equal(update.data.version, 2);
  assert.equal(
    (await guest("/api/posts?keyword=绿色水杯&type=FOUND&category=OTHER")).data
      .total,
    1,
  );
  assert.equal(
    (await mine(`/api/posts/${pid}/conversations`, "POST")).status,
    400,
  );
  const convo = await ming(`/api/posts/${pid}/conversations`, "POST");
  const cid = convo.data.conversation_id;
  assert.equal(
    (await ming(`/api/posts/${pid}/conversations`, "POST")).data
      .conversation_id,
    cid,
  );
  assert.equal(
    (await stranger(`/api/conversations/${cid}/messages`)).status,
    403,
  );
  assert.equal(
    (
      await stranger(`/api/conversations/${cid}/messages`, "POST", {
        content: "越权",
        client_message_id: "invalid",
      })
    ).status,
    403,
  );
  const message = await ming(`/api/conversations/${cid}/messages`, "POST", {
    content: "<script>hello</script>",
    client_message_id: "test-message",
  });
  assert.equal(message.status, 200);
  assert.equal(
    (
      await ming(`/api/conversations/${cid}/messages`, "POST", {
        content: "<script>hello</script>",
        client_message_id: "test-message",
      })
    ).data.id,
    message.data.id,
  );
  assert.equal(
    (await mine("/api/conversations")).data.items.find((c) => c.id === cid)
      .unread_count,
    1,
  );
  assert.equal(
    (
      await mine(`/api/conversations/${cid}/read`, "POST", {
        last_read_message_id: message.data.id,
      })
    ).status,
    200,
  );
  assert.equal(
    (await mine("/api/conversations")).data.items.find((c) => c.id === cid)
      .unread_count,
    0,
  );
  assert.equal(
    (
      await mine(`/api/posts/${pid}/status`, "PATCH", {
        status: "RESOLVED",
        version: 2,
      })
    ).status,
    200,
  );
  assert.equal((await guest("/api/posts?keyword=绿色水杯")).data.total, 0);
  assert.equal(
    (await stranger(`/api/posts/${pid}/conversations`, "POST")).status,
    409,
  );
  assert.equal(
    (
      await ming(`/api/conversations/${cid}/messages`, "POST", {
        content: "已完成仍可沟通",
        client_message_id: "after-end",
      })
    ).status,
    200,
  );
  const reopen = await mine(`/api/posts/${pid}/status`, "PATCH", {
    status: "OPEN",
    version: 3,
  });
  assert.equal(reopen.status, 200);
  assert.equal(
    (await mine(`/api/posts/${pid}`, "DELETE", { version: 4 })).status,
    200,
  );
  assert.equal((await guest(`/api/posts/${pid}`)).status, 404);
  assert.equal(
    (await ming(`/api/posts/${pid}/conversations`, "POST")).status,
    404,
  );
  assert.equal(
    (await ming(`/api/conversations/${cid}/messages`)).data.items.length,
    2,
  );
  const stored = new DatabaseSync(path.join(temp, "app.sqlite"));
  assert.ok(
    stored.prepare("SELECT deleted_at FROM posts WHERE id=?").get(pid)
      .deleted_at,
  );
  assert.ok(
    stored
      .prepare("SELECT password_hash FROM users WHERE account=?")
      .get("new_user").password_hash,
  );
  assert.equal(
    stored
      .prepare("SELECT COUNT(*) n FROM messages WHERE client_message_id=?")
      .get("test-message").n,
    1,
  );
  stored.close();
  await mine("/api/auth/logout", "POST");
  assert.equal((await mine("/api/auth/me")).status, 401);
});
test("server enforces CSRF, origin and image type validation", async () => {
  const mine = client();
  await mine("/api/auth/login", "POST", {
    account: "demo",
    password: "demo12345",
  });
  assert.equal(
    (await mine("/api/auth/logout", "POST", {}, { "X-CSRF-Token": "invalid" }))
      .status,
    403,
  );
  assert.equal(
    (
      await mine(
        "/api/auth/logout",
        "POST",
        {},
        { Origin: "https://untrusted.example" },
      )
    ).status,
    403,
  );
  assert.equal(
    (
      await mine("/api/images", "POST", {
        data_url: "data:image/png;base64,SGVsbG8=",
      })
    ).status,
    400,
  );
  const png =
    "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a1ZkAAAAASUVORK5CYII=";
  const image = await mine("/api/images", "POST", { data_url: png });
  assert.equal(image.status, 200);
  assert.equal((await fetch(`${base}${image.data.url}`)).status, 404); // Unpublished uploads are private.
});

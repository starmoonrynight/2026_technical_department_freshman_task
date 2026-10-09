// Optional browser verification: PLAYWRIGHT_MODULE and CHROMIUM_PATH can use an existing installation.
const { chromium } = require(
  process.env.PLAYWRIGHT_MODULE || "playwright-core",
);
const { spawn } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const assert = require("node:assert/strict");
const root = path.resolve(__dirname, "..");
const temp = fs.mkdtempSync(path.join(os.tmpdir(), "hdu-ui-test-"));
// Screenshots are temporary unless an explicit output directory is requested.
const screenshots = process.env.UI_SCREENSHOT_DIR
  ? path.resolve(process.env.UI_SCREENSHOT_DIR)
  : path.join(temp, "screenshots");
fs.mkdirSync(screenshots, { recursive: true });
const child = spawn(process.execPath, ["server.mjs"], {
  cwd: root,
  env: { ...process.env, PORT: "0", DATA_DIR: temp },
  stdio: ["ignore", "pipe", "pipe"],
});
let browser;
(async () => {
  const base = await new Promise((resolve, reject) => {
    child.stdout.on("data", (d) => {
      const m = d.toString().match(/http:\/\/127\.0\.0\.1:\d+/);
      if (m) resolve(m[0]);
    });
    child.on("error", reject);
  });
  browser = await chromium.launch({
    headless: true,
    ...(process.env.CHROMIUM_PATH
      ? { executablePath: process.env.CHROMIUM_PATH }
      : {}),
  });
  const context = await browser.newContext({
    viewport: { width: 1440, height: 1000 },
  });
  const page = await context.newPage();
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.goto(base);
  await page.waitForSelector(".item-card");
  assert.equal(await page.locator(".item-card").count(), 5);
  assert.equal(
    await page.evaluate(
      () => document.documentElement.scrollWidth > innerWidth,
    ),
    false,
  );
  await page.screenshot({
    path: path.join(screenshots, "desktop-preview.png"),
    fullPage: true,
  });
  await page
    .getByRole("textbox", { name: "搜索物品名称、颜色或特征" })
    .fill("校园卡");
  await page.getByRole("button", { name: "搜索", exact: true }).click();
  await page.waitForFunction(
    () => document.querySelectorAll(".item-card").length === 1,
  );
  await page.getByRole("button", { name: "寻物信息", exact: true }).click();
  await page.getByRole("heading", { name: "暂未找到相关信息" }).waitFor();
  await page.getByRole("button", { name: "清空筛选" }).click();
  await page.waitForSelector(".item-card");
  await page.getByRole("button", { name: "招领信息", exact: true }).click();
  await page.getByRole("button", { name: "筛选", exact: true }).click();
  await page
    .getByRole("button", { name: "青山湖校区（示例）", exact: true })
    .click();
  await page.getByRole("button", { name: "确定", exact: true }).click();
  await page.getByRole("heading", { name: "暂未找到相关信息" }).waitFor();
  await page.getByRole("button", { name: "清空筛选" }).click();
  await page
    .getByRole("button", { name: "发布信息", exact: true })
    .first()
    .click();
  await page
    .locator("#page")
    .getByRole("button", { name: "登录 / 注册", exact: true })
    .click();
  await page.getByRole("button", { name: "填入小杭账号" }).click();
  await page.getByRole("button", { name: "登录", exact: true }).last().click();
  await page.locator("#post-form").waitFor();
  await page.getByRole("button", { name: "我捡到了物品" }).click();
  await page.getByLabel("物品名称").fill("测试白色水杯");
  await page.getByLabel("物品类别").selectOption("OTHER");
  await page.getByLabel("拾取地点").fill("图书馆测试位置");
  await page.getByLabel("物品描述").fill("白色水杯，有一个特别的蓝色贴纸。");
  await page
    .getByLabel("上传物品图片")
    .setInputFiles({
      name: "sample.png",
      mimeType: "image/png",
      buffer: Buffer.from(
        "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+a1ZkAAAAASUVORK5CYII=",
        "base64",
      ),
    });
  await page.waitForSelector(".upload-preview");
  await page
    .locator("#post-form")
    .getByRole("button", { name: "发布信息", exact: true })
    .click();
  await page
    .getByRole("heading", { name: "测试白色水杯", exact: true })
    .waitFor();
  const postedHash = await page.evaluate(() => location.hash);
  await page.getByRole("button", { name: "编辑", exact: true }).click();
  await page.getByLabel("物品名称").fill("修改后的白色水杯");
  await page.getByRole("button", { name: "保存修改" }).click();
  await page
    .getByRole("heading", { name: "修改后的白色水杯", exact: true })
    .waitFor();
  await page.getByRole("button", { name: "更新状态", exact: true }).click();
  await page.locator("#resolve-post").click();
  await page.locator("#confirm-action").click();
  await page.getByRole("button", { name: "重新开启", exact: true }).waitFor();
  await page.reload();
  await page.getByRole("button", { name: "重新开启", exact: true }).waitFor();
  await page.getByRole("button", { name: "重新开启", exact: true }).click();
  await page.locator("#confirm-action").click();
  await page.getByRole("button", { name: "更新状态", exact: true }).waitFor();
  await page.goto(`${base}/#/`);
  await page.waitForSelector(".item-card");
  await page
    .getByRole("button", { name: "查看黑色卡套校园卡", exact: true })
    .click();
  await page.getByRole("button", { name: "联系发布者" }).click();
  await page.locator("#message-input").waitFor();
  assert.equal(await page.locator(".bubble").count(), 2);
  await page
    .getByRole("textbox", { name: "输入消息" })
    .fill("这是浏览器测试消息。");
  await page.getByRole("button", { name: "发送", exact: true }).click();
  await page.getByText("这是浏览器测试消息。", { exact: true }).waitFor();
  const second = await browser.newContext({
    viewport: { width: 390, height: 844 },
    isMobile: true,
    hasTouch: true,
  });
  const phone = await second.newPage();
  phone.on("pageerror", (e) => errors.push(e.message));
  await phone.goto(`${base}/#/login`);
  await phone.getByRole("button", { name: "填入小明账号" }).click();
  await phone
    .locator("#auth-form")
    .getByRole("button", { name: "登录", exact: true })
    .last()
    .click();
  await phone.waitForSelector(".item-card");
  assert.equal(
    await phone.evaluate(
      () => document.documentElement.scrollWidth > innerWidth,
    ),
    false,
  );
  await phone.screenshot({
    path: path.join(screenshots, "mobile-preview.png"),
    fullPage: true,
  });
  await phone
    .locator(".mobile-nav")
    .getByRole("button", { name: /消息/ })
    .click();
  await phone.waitForSelector(".conversation-row");
  await phone.locator(".conversation-row").first().click();
  await phone.getByText("这是浏览器测试消息。", { exact: true }).waitFor();
  await phone
    .getByRole("textbox", { name: "输入消息" })
    .fill("已收到，我们下午见。");
  await phone.getByRole("button", { name: "发送", exact: true }).click();
  await page
    .getByText("已收到，我们下午见。", { exact: true })
    .waitFor({ timeout: 10000 });
  await phone.screenshot({
    path: path.join(screenshots, "chat-preview.png"),
    fullPage: true,
  });
  assert.equal(
    await phone.evaluate(
      () => document.documentElement.scrollWidth > innerWidth,
    ),
    false,
  );
  // Expire the actual server session while editing, then verify same-account recovery.
  await page.goto(`${base}/${postedHash}/edit`);
  await page.locator('#field-title').fill('登录失效仍保留的草稿');
  await page.evaluate(async () => {
    const current=await (await fetch('/api/auth/me')).json();
    await fetch('/api/auth/logout',{method:'POST',headers:{'Content-Type':'application/json','X-CSRF-Token':current.data.csrf},body:'{}'});
  });
  await page.getByRole('button',{name:'保存修改',exact:true}).click();
  await page.waitForSelector('#auth-form');
  await page.getByRole('button',{name:'填入小杭账号'}).click();
  await page.locator('#auth-form').getByRole('button',{name:'登录',exact:true}).last().click();
  await page.waitForSelector('#field-title');
  assert.equal(await page.locator('#field-title').inputValue(),'登录失效仍保留的草稿');
  assert.deepEqual(errors, []);
  console.log(
    JSON.stringify({
      passed: true,
      postedHash,
      checks: [
        "desktop/mobile layout",
        "search/filter",
        "login redirect",
        "upload/publish/edit",
        "status/reload",
        "two-user messaging",
      ],
      pageErrors: errors,
    }),
  );
})()
  .catch((e) => {
    console.error(e);
    process.exitCode = 1;
  })
  .finally(async () => {
    await browser?.close();
    child.kill();
    await new Promise((resolve) => child.once("exit", resolve));
    fs.rmSync(temp, { recursive: true, force: true });
  });

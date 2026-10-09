import { icon } from "./lib/icons.js";
import { state } from "./lib/state.js";
import { esc, labels, localToday, date, token } from "./lib/utils.js";
import { api } from "./lib/api.js";
import { art, imageFor } from "./lib/art.js";
const app = document.getElementById("app"),
  overlay = document.getElementById("overlay");
function toast(message) {
  const t = document.getElementById("toast");
  t.textContent = message;
  t.classList.add("show");
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => t.classList.remove("show"), 3000);
}
function badge(p) {
  const text = p.deleted_at
    ? "已删除"
    : p.status === "CLOSED"
      ? "已关闭"
      : p.status === "RESOLVED"
        ? p.type === "LOST"
          ? "已找回"
          : "已归还"
        : p.type === "LOST"
          ? "寻找中"
          : "待认领";
  return `<span class="badge ${p.status === "RESOLVED" ? "resolved" : p.status === "CLOSED" || p.deleted_at ? "closed" : p.type === "LOST" ? "lost" : ""}">${text}</span>`;
}
function countBadge() {
  return state.unread
    ? `<span class="unread" data-unread>${state.unread > 99 ? "99+" : state.unread}</span>`
    : '<span class="unread" data-unread hidden></span>';
}
function shell() {
  const active =
    state.route === "/"
      ? "home"
      : state.route.startsWith("/messages")
        ? "messages"
        : state.route === "/me"
          ? "me"
          : state.route.startsWith("/posts/new")
            ? "publish"
            : "";
  const nav = [
    ["home", "/", "home", "失物广场"],
    ["publish", "/posts/new", "plus", "发布信息"],
    ["messages", "/messages", "message", "站内消息"],
    ["me", "/me", "user", "我的发布"],
  ];
  const title =
    active === "messages"
      ? "站内消息"
      : active === "me"
        ? "我的发布"
        : active === "publish"
          ? "发布信息"
          : "失物招领";
  app.innerHTML = `<aside class="sidebar"><a class="brand" href="#/" data-nav="/" aria-label="杭电失物招领首页"><span class="logo">${icon("search")}</span><span>杭电失物招领<small>LOST & FOUND</small></span></a><nav class="side-nav" aria-label="主导航">${nav.map(([key, route, i, text]) => `<button class="nav-btn ${key === active ? "active" : ""} ${key === "publish" ? "publish" : ""}" data-nav="${route}">${icon(i)}${text}${key === "messages" ? countBadge() : ""}</button>`).join("")}</nav><div class="side-foot"><div class="side-note">${icon("heart")}<strong>每一份善意，都值得被看见</strong><p>让遗失的物品找到归途，<br>让热心的同学得到回应。</p></div><p>HDU · 校园失物招领<br>本地演示版</p></div></aside><main class="main"><header class="topbar"><div class="top-left"><span class="dot"></span><strong>${title}</strong><span> / 校园生活</span></div><div class="top-right"><label class="campus-select">${icon("pin")}<select id="header-campus" aria-label="校区"><option value="">全部校区</option>${state.campuses.map((c) => `<option value="${esc(c.id)}" ${state.filters.campus_id === c.id ? "selected" : ""}>${esc(c.name)}</option>`).join("")}</select></label><span class="demo-tag">DEMO</span><button class="user-btn" data-nav="${state.user ? "/me" : "/login"}"><span class="avatar">${state.user ? esc(state.user.nickname[0]) : icon("user")}</span><span>${state.user ? esc(state.user.nickname) : "登录 / 注册"}</span></button></div></header><div class="page ${state.route.startsWith("/messages/") ? "chat-page" : ""}" id="page"><div class="loading"><span class="spinner"></span>正在加载</div></div></main><nav class="mobile-nav" aria-label="底部导航">${nav.map(([key, route, i]) => `<button class="${key === active ? "active" : ""}" data-nav="${route}">${key === "publish" ? `<span class="pub-icon">${icon("plus")}</span>` : icon(i)}<span>${{ home: "首页", publish: "发布", messages: "消息", me: "我的" }[key]}</span>${key === "messages" ? countBadge() : ""}</button>`).join("")}</nav>`;
}
function empty(title, description, buttons = "") {
  return `<div class="empty"><div class="empty-icon">${icon("search")}</div><h2>${esc(title)}</h2><p>${esc(description)}</p>${buttons ? `<div class="buttons">${buttons}</div>` : ""}</div>`;
}
function modal(content, label = "对话框") {
  overlay.innerHTML = `<div class="modal-backdrop" data-close-backdrop><section class="modal" role="dialog" aria-modal="true" aria-label="${esc(label)}">${content}</section></div>`;
  document.body.style.overflow = "hidden";
  overlay.querySelector("button,input")?.focus();
}
function closeModal() {
  overlay.innerHTML = "";
  document.body.style.overflow = "";
}
function confirmAction(title, description, onConfirm) {
  modal(
    `<div class="modal-head"><h2>${esc(title)}</h2><button class="icon-btn" data-action="close-modal" aria-label="关闭">${icon("close")}</button></div><p>${esc(description)}</p><div class="modal-footer"><button class="secondary" data-action="close-modal">取消</button><button class="primary" id="confirm-action">确认</button></div>`,
    title,
  );
  document.getElementById("confirm-action").onclick = async (e) => {
    e.target.disabled = true;
    try {
      await onConfirm();
      closeModal();
    } catch (err) {
      e.target.disabled = false;
      toast(err.message);
    }
  };
}
function draftKey() {
  return `hdu-draft-${state.user?.id || "guest"}`;
}
function captureDraft() {
  const f = document.getElementById("post-form");
  if (!f || !state.draft) return;
  for (const field of [
    "title",
    "category",
    "campus_id",
    "location",
    "event_date",
    "event_period",
    "description",
    "storage_location",
  ])
    state.draft[field] = f.elements[field]?.value || "";
  if (!state.draft.id)
    try {
      sessionStorage.setItem(draftKey(), JSON.stringify(state.draft));
    } catch {}
}
function navigate(route, bypass = false) {
  if (state.route === "/" && route !== "/") state.scrollHome = window.scrollY;
  captureDraft();
  if (state.dirty && document.getElementById("post-form") && !bypass) {
    confirmAction(
      "离开编辑页面？",
      state.draft.id
        ? "未保存的修改不会提交。"
        : "未发布的内容会保留为当前浏览器草稿。",
      () => {
        state.dirty = false;
        navigate(route, true);
      },
    );
    return;
  }
  closeModal();
  if (location.hash === `#${route}`) render();
  else location.hash = route;
}
function requireLogin(action) {
  if (state.user) return action();
  state.pending = { action, returnRoute: state.route };
  navigate("/login", true);
}
function card(p) {
  state.posts.set(p.id, p);
  return `<button class="item-card" data-nav="/posts/${esc(p.id)}" aria-label="查看${esc(p.title)}"><div class="item-image">${imageFor(p)}${badge(p)}</div><div class="item-info"><h3>${esc(p.title)}</h3><div class="item-meta">${icon("pin")}${esc(p.location)}</div><div class="item-meta">${icon("clock")}${p.type === "FOUND" ? "拾取" : "丢失"}：${date(p.event_date)} ${esc(p.event_period)}</div><div class="item-bottom"><span class="author"><span class="tiny-avatar">${esc(p.author.nickname[0])}</span>${esc(p.author.nickname)}</span><span>${esc(p.campus_name.replace("（示例）", ""))}</span><span class="arrow">${icon("arrow")}</span></div></div></button>`;
}
async function home(rid) {
  const result = await paginated(
    "/api/posts",
    { ...state.filters, type: state.type },
    state.homeLimit,
  );
  if (rid !== state.renderId) return;
  document.getElementById("page").innerHTML =
    `<section class="hero"><div class="hero-text"><div class="eyebrow"><span class="dot"></span> CAMPUS LOST & FOUND</div><h1>让每一份遗失，<br>都有回音。</h1><p>在这里寻找你的物品，也为他人的失而复得搭一座桥。</p></div><div class="hero-art" aria-hidden="true"><div class="art-orbit"></div><div class="art-card one">${icon("bag")}</div><div class="art-card two">${icon("search")}</div><span class="art-spark">✦</span></div></section><div class="section-top"><div class="type-tabs"><button class="type-tab ${state.type === "FOUND" ? "active" : ""}" data-action="type" data-value="FOUND">招领信息</button><button class="type-tab ${state.type === "LOST" ? "active" : ""}" data-action="type" data-value="LOST">寻物信息</button></div><button class="filter-btn" data-action="filter">${icon("filter")}筛选</button></div><form class="searchbar" id="search-form">${icon("search")}<input name="keyword" value="${esc(state.filters.keyword)}" placeholder="搜索物品名称、颜色或特征" aria-label="搜索物品名称、颜色或特征"><button type="submit">搜索</button></form><div class="category-row"><button class="category-btn ${!state.filters.category ? "active" : ""}" data-action="category" data-value="">全部物品</button>${Object.entries(
      labels,
    )
      .map(
        ([key, value]) =>
          `<button class="category-btn ${state.filters.category === key ? "active" : ""}" data-action="category" data-value="${key}">${value}</button>`,
      )
      .join(
        "",
      )}</div><div class="results-head"><span>找到 <strong>${result.total}</strong> 条${state.type === "FOUND" ? "招领" : "寻物"}信息${state.filters.status !== "OPEN" ? " · 含结束状态" : ""}</span><span>最新发布优先${Object.entries(state.filters).some(([k, v]) => v && !(k === "status" && v === "OPEN")) ? ' · <button class="clear-btn" data-action="clear-filters">清空筛选</button>' : ""}</span></div>${result.items.length ? `<div class="grid">${result.items.map(card).join("")}</div>${result.has_more ? '<button class="load-more" data-action="load-more">加载更多</button>' : '<p class="end-note">已经看完啦，愿你的物品早日回到身边。</p>'}` : empty("暂未找到相关信息", "换个关键词，或发布一条信息，让更多同学帮你寻找。", '<button class="secondary" data-action="filter">调整筛选</button><button class="primary" data-nav="/posts/new">发布信息</button>')}`;
}
async function paginated(route, query, pages) {
  const results = await Promise.all(
    Array.from({ length: pages }, (_, i) =>
      api(
        `${route}?${new URLSearchParams({ ...query, page: i + 1, page_size: 20 })}`,
      ),
    ),
  );
  const unique = new Map(
    results.flatMap((result) => result.items).map((item) => [item.id, item]),
  );
  return {
    ...results[0],
    items: [...unique.values()],
    has_more: results.at(-1).has_more,
  };
}
function showFilters() {
  let temp = { ...state.filters };
  function draw() {
    modal(
      `<div class="modal-head"><h2>筛选信息</h2><button class="icon-btn" data-action="close-modal" aria-label="关闭筛选">${icon("close")}</button></div>${[
        [
          "campus_id",
          "校区",
          [["", "全部校区"], ...state.campuses.map((c) => [c.id, c.name])],
        ],
        ["category", "物品类别", [["", "全部物品"], ...Object.entries(labels)]],
        [
          "status",
          "状态",
          [
            ["OPEN", "进行中"],
            ["RESOLVED", "已完成"],
            ["CLOSED", "已关闭"],
            ["ALL", "全部"],
          ],
        ],
      ]
        .map(
          ([key, title, options]) =>
            `<div class="filter-label">${title}</div><div class="filter-options">${options.map(([v, label]) => `<button class="${temp[key] === v ? "active" : ""}" data-filter="${key}" data-value="${v}">${esc(label)}</button>`).join("")}</div>`,
        )
        .join(
          "",
        )}<div class="filter-label">事发日期</div><div class="fields-two"><div class="field" style="margin:0"><label for="date-from">开始日期</label><input id="date-from" type="date" value="${esc(temp.date_from)}" max="${localToday()}"></div><div class="field" style="margin:0"><label for="date-to">结束日期</label><input id="date-to" type="date" value="${esc(temp.date_to)}" max="${localToday()}"></div></div><div class="modal-footer"><button class="secondary" id="reset-filters">重置</button><button class="primary" id="apply-filters">确定</button></div>`,
      "筛选信息",
    );
    overlay.querySelectorAll("[data-filter]").forEach(
      (btn) =>
        (btn.onclick = () => {
          temp.date_from = document.getElementById("date-from").value;
          temp.date_to = document.getElementById("date-to").value;
          temp[btn.dataset.filter] = btn.dataset.value;
          draw();
        }),
    );
    document.getElementById("reset-filters").onclick = () => {
      temp = {
        ...temp,
        campus_id: "",
        category: "",
        status: "OPEN",
        date_from: "",
        date_to: "",
      };
      draw();
    };
    document.getElementById("apply-filters").onclick = () => {
      temp.date_from = document.getElementById("date-from").value;
      temp.date_to = document.getElementById("date-to").value;
      if (temp.date_from && temp.date_to && temp.date_from > temp.date_to)
        return toast("开始日期不能晚于结束日期");
      state.filters = temp;
      state.homeLimit = 1;
      closeModal();
      render();
    };
  }
  draw();
}
async function detail(pid, rid) {
  const p = await api(`/api/posts/${pid}`);
  if (rid !== state.renderId) return;
  state.posts.set(p.id, p);
  const mine = p.owner_id === state.user?.id;
  document.getElementById("page").innerHTML =
    `<button class="back" data-nav="${mine ? "/me" : "/"}">${icon("back")}返回${mine ? "我的发布" : "信息列表"}</button><div class="detail-layout"><div><div class="detail-image" id="detail-image">${p.image_urls.length ? `<button data-action="preview" data-url="${esc(p.image_urls[0])}" style="width:100%;padding:0" aria-label="放大图片"><img src="${esc(p.image_urls[0])}" alt="${esc(p.title)}"></button>` : imageFor(p)}</div>${p.image_urls.length > 1 ? `<div class="gallery-row">${p.image_urls.map((url, i) => `<button class="${i === 0 ? "active" : ""}" data-action="gallery" data-url="${esc(url)}" aria-label="查看第${i + 1}张图片"><img src="${esc(url)}" alt="第${i + 1}张物品图片"></button>`).join("")}</div>` : ""}<div class="notice">${icon("shield")}<span>请通过物品的独有特征核实归属，确认后再约定归还。示例图片仅用于演示。</span></div></div><section class="detail-panel"><div style="display:flex;gap:10px;align-items:center"><span class="badge ${p.type === "LOST" ? "lost" : ""}">${p.type === "FOUND" ? "招领信息" : "寻物信息"}</span>${badge(p)}</div><h1>${esc(p.title)}</h1><div class="detail-row">${icon("bag")}<span>${labels[p.category]}</span></div><div class="detail-row">${icon("pin")}<span>${esc(p.campus_name)} · ${esc(p.location)}</span></div><div class="detail-row">${icon("clock")}<span>${p.type === "FOUND" ? "拾取" : "丢失"}时间：${date(p.event_date)} ${esc(p.event_period)}</span></div>${p.storage_location ? `<div class="detail-row">${icon("shield")}<span>保管地点：${esc(p.storage_location)}</span></div>` : ""}<div class="detail-description"><h3>物品描述</h3><p>${esc(p.description)}</p></div><div class="detail-author"><span class="avatar">${esc(p.author.nickname[0])}</span><div>${esc(p.author.nickname)}<small>发布于 ${date(p.created_at, true)}</small></div></div><div class="detail-actions">${mine ? `<button class="secondary" data-nav="/posts/${esc(p.id)}/edit">${icon("edit")} 编辑</button><button class="primary" data-action="status" data-id="${esc(p.id)}">${p.status === "OPEN" ? "更新状态" : "重新开启"}</button><button class="icon-btn" data-action="delete" data-id="${esc(p.id)}" aria-label="删除帖子">${icon("close")}</button>` : p.status === "OPEN" ? `<button class="primary full" data-action="contact" data-id="${esc(p.id)}">${icon("message")}联系发布者</button>` : `<button class="secondary full" disabled>帖子已结束</button>`}</div></section></div>`;
}
function defaultDraft() {
  let saved;
  try {
    saved = JSON.parse(sessionStorage.getItem(draftKey()) || "null");
  } catch {}
  return (
    saved || {
      type: state.type === "FOUND" ? "LOST" : "FOUND",
      title: "",
      category: "",
      campus_id: state.filters.campus_id || "xiasha",
      location: "",
      event_date: localToday(),
      event_period: "",
      description: "",
      storage_location: "",
      image_ids: [],
      image_urls: [],
      submitKey: token(),
    }
  );
}
const field = (key, label, value, attributes = "", optional = false) =>
  `<div class="field" data-field="${key}"><label for="field-${key}">${label}${optional ? "<small>选填</small>" : "<b>*</b>"}</label><input id="field-${key}" name="${key}" value="${esc(value)}" ${attributes} ${optional ? "" : "required"}><div class="field-error"></div></div>`;
async function postForm(pid, rid) {
  if (!state.user) {
    document.getElementById("page").innerHTML = empty(
      "登录后发布信息",
      "用一条清晰的信息，为失物和失主搭一座桥。",
      '<button class="primary" data-action="login-to-publish">登录 / 注册</button>',
    );
    return;
  }
  if (pid && state.draft?.id !== pid) {
    const p = await api(`/api/posts/${pid}`);
    if (rid !== state.renderId) return;
    if (p.owner_id !== state.user.id) throw new Error("只能编辑自己的帖子");
    state.draft = { ...p };
  }
  if (!pid && (!state.draft || state.draft.id)) state.draft = defaultDraft();
  drawPostForm();
}
function drawPostForm() {
  const d = state.draft;
  document.getElementById("page").innerHTML =
    `<button class="back" data-nav="${d.id ? `/posts/${d.id}` : "/"}">${icon("back")}返回</button><div class="page-title"><div><h1>${d.id ? "编辑信息" : "发布一条信息"}</h1><p>清楚描述物品特征，让寻找更有方向。</p></div></div><div class="form-layout"><form class="form-card" id="post-form"><div class="segmented"><button type="button" class="${d.type === "LOST" ? "active" : ""}" data-action="draft-type" data-value="LOST" ${d.id ? "disabled" : ""}>我丢失了物品</button><button type="button" class="${d.type === "FOUND" ? "active" : ""}" data-action="draft-type" data-value="FOUND" ${d.id ? "disabled" : ""}>我捡到了物品</button></div><div class="field"><label>物品图片 <small>选填</small></label><div class="uploads">${(d.image_urls || []).map((url, i) => `<div class="upload-preview"><img src="${esc(url)}" alt="已上传物品图片"><button type="button" data-action="remove-image" data-index="${i}" aria-label="移除第${i + 1}张图片">${icon("close")}</button></div>`).join("")}${d.image_ids.length < 3 ? `<label class="upload-box">${icon("plus")}<span>${state.uploading ? "上传中…" : "添加图片"}</span><input type="file" id="image-input" accept="image/jpeg,image/png" multiple aria-label="上传物品图片" ${state.uploading ? "disabled" : ""}></label>` : ""}</div><div class="field-hint">最多3张 JPG / PNG，每张不超过10MB。照片中请遮挡完整姓名、学号等个人信息。</div></div>${field("title", "物品名称", d.title, 'minlength="2" maxlength="30" placeholder="例如：黑色卡套校园卡"')}<div class="fields-two"><div class="field" data-field="category"><label for="field-category">物品类别 <b>*</b></label><select id="field-category" name="category" required><option value="">请选择类别</option>${Object.entries(
      labels,
    )
      .map(
        ([v, label]) =>
          `<option value="${v}" ${d.category === v ? "selected" : ""}>${label}</option>`,
      )
      .join(
        "",
      )}</select><div class="field-error"></div></div><div class="field" data-field="campus_id"><label for="field-campus_id">校区 <b>*</b></label><select id="field-campus_id" name="campus_id" required>${state.campuses.map((c) => `<option value="${esc(c.id)}" ${d.campus_id === c.id ? "selected" : ""}>${esc(c.name)}</option>`).join("")}</select><div class="field-error"></div></div></div>${field("location", d.type === "FOUND" ? "拾取地点" : "丢失地点", d.location, 'minlength="2" maxlength="50" placeholder="例如：图书馆二楼，大致区域也可以"')}<div class="fields-two">${field("event_date", d.type === "FOUND" ? "拾取日期" : "丢失日期", d.event_date, `type="date" max="${localToday()}"`)}${field("event_period", "时段", d.event_period, 'maxlength="30" placeholder="例如：下午，大约14:00"', true)}</div><div class="field" data-field="description"><label for="field-description">物品描述 <b>*</b></label><textarea id="field-description" name="description" required minlength="5" maxlength="300" placeholder="颜色、外观、相关情况；招领时保留独有特征供核实">${esc(d.description)}</textarea><div class="field-error"></div></div>${d.type === "FOUND" ? field("storage_location", "保管地点", d.storage_location, 'maxlength="100" placeholder="例如：本人保管或已交给服务台"', true) : ""}<div class="notice">${icon("message")}<span>发布后，同学可以通过站内消息联系你，无需填写微信或QQ。</span></div><div class="form-submit"><button type="button" class="secondary" data-nav="${d.id ? `/posts/${d.id}` : "/"}">取消</button><button class="primary" type="submit" ${state.uploading ? "disabled" : ""}>${d.id ? "保存修改" : "发布信息"}</button></div></form><aside class="form-side"><h3>${icon("heart")} 一点小建议</h3><p>准确的地点和时间，能让物品更快找到归途。</p><p>记不清时间？填写大致时段就可以。</p><p>拾到物品时，可以保留一两个独有特征，联系后再向对方核实。</p><p>归还完成后，别忘了更新帖子状态。</p></aside></div>`;
}
function fieldErrors(form, errors) {
  form.querySelectorAll(".invalid").forEach((e) => {
    e.classList.remove("invalid");
    e.querySelector(".field-error").textContent = "";
  });
  for (const [key, text] of Object.entries(errors || {})) {
    const el = form.querySelector(`[data-field="${key}"]`);
    if (el) {
      el.classList.add("invalid");
      el.querySelector(".field-error").textContent = text;
    }
  }
  form
    .querySelector(".invalid input,.invalid select,.invalid textarea")
    ?.focus();
}
async function uploadImages(files) {
  captureDraft();
  if (state.uploading) return;
  const slots = 3 - state.draft.image_ids.length;
  if (files.length > slots) return toast("最多上传3张图片");
  state.uploading = true;
  drawPostForm();
  try {
    for (const file of files) {
      if (
        !["image/jpeg", "image/png"].includes(file.type) ||
        file.size > 10 * 1024 * 1024
      )
        throw new Error("仅支持不超过10MB的JPG或PNG图片");
      const data = await new Promise((resolve, reject) => {
        const r = new FileReader();
        r.onload = () => resolve(r.result);
        r.onerror = () => reject(new Error("读取图片失败"));
        r.readAsDataURL(file);
      });
      const result = await api("/api/images", {
        method: "POST",
        body: JSON.stringify({ data_url: data }),
      });
      state.draft.image_ids.push(result.image_id);
      state.draft.image_urls.push(result.url);
      state.dirty = true;
    }
  } catch (e) {
    toast(e.message);
  } finally {
    state.uploading = false;
    if (document.getElementById("post-form")) {
      drawPostForm();
      captureDraft();
    }
  }
}
async function savePost(form) {
  captureDraft();
  const d = state.draft;
  if (state.uploading) return toast("请等待图片上传完成");
  const button = form.querySelector("[type=submit]");
  button.disabled = true;
  button.textContent = "正在保存…";
  try {
    const p = await api(d.id ? `/api/posts/${d.id}` : "/api/posts", {
      method: d.id ? "PATCH" : "POST",
      body: JSON.stringify(d),
      headers: d.id ? {} : { "Idempotency-Key": d.submitKey },
    });
    state.dirty = false;
    state.draft = null;
    sessionStorage.removeItem(draftKey());
    toast(d.id ? "修改已保存" : "发布成功，愿物品早日归位");
    navigate(`/posts/${p.id}`, true);
  } catch (e) {
    fieldErrors(form, e.fields);
    toast(e.message);
    button.disabled = false;
    button.textContent = d.id ? "保存修改" : "发布信息";
  }
}
function auth() {
  const registering = state.route === "/register";
  document.getElementById("page").innerHTML =
    `<div class="auth-wrap"><form class="auth-card" id="auth-form"><div class="logo">${icon("search")}</div><h1>杭电失物招领</h1><p>找回物品，也遇见校园里的善意。</p><div class="segmented"><button type="button" class="${!registering ? "active" : ""}" data-nav="/login">登录</button><button type="button" class="${registering ? "active" : ""}" data-nav="/register">注册</button></div>${field("account", "账号", "", 'pattern="[A-Za-z0-9_]{4,20}" maxlength="20" autocomplete="username" placeholder="4—20位字母、数字或下划线"')}${registering ? field("nickname", "昵称", "", 'maxlength="20" placeholder="同学们怎么称呼你"') : ""}${field("password", "密码", "", `type="password" minlength="8" maxlength="64" autocomplete="${registering ? "new-password" : "current-password"}" placeholder="8—64位密码"`)}<button type="button" class="clear-btn" data-action="show-password">显示密码</button>${registering ? field("confirmation", "确认密码", "", 'type="password" minlength="8" maxlength="64" autocomplete="new-password" placeholder="请再次输入密码"') : ""}<div class="auth-error" id="auth-error" role="alert"></div><button class="primary full" type="submit">${registering ? "注册并登录" : "登录"}</button><button type="button" class="auth-skip" data-nav="/">暂不登录，返回首页</button></form>${!registering ? '<div class="demo-accounts">演示账号：demo（小杭） / xiaoming（小明）<br>密码均为 demo12345。分别登录即可体验双方对话。<br><button data-action="fill-demo" data-value="demo">填入小杭账号</button> <button data-action="fill-demo" data-value="xiaoming">填入小明账号</button></div>' : ""}</div>`;
}
async function authenticate(form) {
  const registering = state.route === "/register";
  const b = Object.fromEntries(new FormData(form));
  if (registering && b.password !== b.confirmation)
    return fieldErrors(form, { confirmation: "两次输入的密码不一致" });
  const btn = form.querySelector("[type=submit]");
  btn.disabled = true;
  document.getElementById("auth-error").textContent = "";
  try {
    const result = await api(
      `/api/auth/${registering ? "register" : "login"}`,
      { method: "POST", body: JSON.stringify(b) },
    );
    state.user = result.user;
    state.csrf = result.csrf;
    state.unread = 0;
    state.draft =
      state.expiredDraft?.userId === state.user.id
        ? state.expiredDraft.draft
        : null;
    state.expiredDraft = null;
    state.failed = null;
    state.chatDrafts = {};
    toast(`欢迎你，${state.user.nickname}`);
    const pending = state.pending;
    state.pending = null;
    if (pending) {
      navigate(pending.returnRoute, true);
      await pending.action();
    } else navigate("/", true);
    refreshUnread();
  } catch (e) {
    fieldErrors(form, e.fields);
    document.getElementById("auth-error").textContent = e.message;
    btn.disabled = false;
  }
}
async function mine(rid) {
  if (!state.user) {
    document.getElementById("page").innerHTML = empty(
      "登录后管理你的发布",
      "查看、编辑和更新信息，记录每一次失而复得。",
      '<button class="primary" data-action="login-to-me">登录 / 注册</button>',
    );
    return;
  }
  const result = await paginated(
    "/api/me/posts",
    { type: state.mineType, status: state.mineStatus },
    state.mineLimit,
  );
  if (rid !== state.renderId) return;
  result.items.forEach((p) => state.posts.set(p.id, p));
  document.getElementById("page").innerHTML =
    `<div class="page-title"><div><h1>我的发布</h1><p>每一条信息，都连接着一份期待。</p></div><button class="primary" data-nav="/posts/new">${icon("plus")}发布信息</button></div><section class="profile"><span class="avatar">${esc(state.user.nickname[0])}</span><div><h2>${esc(state.user.nickname)}</h2><p>账号：${esc(state.user.account)}</p></div><button class="secondary" data-action="logout">退出登录</button></section><div class="mine-controls">${[
      ["", "全部"],
      ["LOST", "寻物"],
      ["FOUND", "招领"],
    ]
      .map(
        ([v, label]) =>
          `<button class="category-btn ${state.mineType === v ? "active" : ""}" data-action="mine-type" data-value="${v}">${label}</button>`,
      )
      .join("")}<select id="mine-status" aria-label="我的帖子状态">${[
      ["ALL", "全部状态"],
      ["OPEN", "进行中"],
      ["RESOLVED", "已完成"],
      ["CLOSED", "已关闭"],
    ]
      .map(
        ([v, label]) =>
          `<option value="${v}" ${state.mineStatus === v ? "selected" : ""}>${label}</option>`,
      )
      .join(
        "",
      )}</select></div>${result.items.length ? `<div class="mine-list">${result.items.map((p) => `<article class="mine-card"><button class="mine-thumb" data-nav="/posts/${p.id}" aria-label="查看${esc(p.title)}">${imageFor(p)}</button><div class="mine-body"><button style="padding:0;text-align:left" data-nav="/posts/${p.id}"><h3>${esc(p.title)} ${badge(p)}</h3></button><p>${esc(p.location)} · ${date(p.event_date)} · ${p.type === "LOST" ? "寻物" : "招领"}</p></div><div class="mine-actions"><button data-nav="/posts/${p.id}/edit">编辑</button><button data-action="status" data-id="${p.id}">${p.status === "OPEN" ? "更新状态" : "重新开启"}</button><button class="danger" data-action="delete" data-id="${p.id}">删除</button></div></article>`).join("")}</div>` : empty("这里还没有发布信息", "发布寻物或招领信息，让校园里的善意相遇。", '<button class="primary" data-nav="/posts/new">发布信息</button>')}`;
  if (result.has_more)
    document
      .getElementById("page")
      .insertAdjacentHTML(
        "beforeend",
        '<button class="load-more" data-action="more-mine">加载更多</button>',
      );
}
function changeStatus(pid) {
  const p = state.posts.get(pid);
  if (!p) return;
  const update = (status) =>
    confirmAction(
      status === "OPEN"
        ? "重新开启这条信息？"
        : status === "RESOLVED"
          ? p.type === "LOST"
            ? "确认已经取回物品？"
            : "确认已经归还物品？"
          : "关闭这条信息？",
      status === "OPEN"
        ? "开启后，其他同学可以再次从详情联系你。"
        : "信息将从默认列表中移除，已有对话仍可继续。",
      async () => {
        await api(`/api/posts/${pid}/status`, {
          method: "PATCH",
          body: JSON.stringify({ status, version: p.version }),
        });
        toast("状态已更新");
        render();
      },
    );
  if (p.status !== "OPEN") return update("OPEN");
  modal(
    `<div class="modal-head"><h2>更新物品状态</h2><button class="icon-btn" data-action="close-modal" aria-label="关闭">${icon("close")}</button></div><div class="status-options"><button id="resolve-post">${p.type === "LOST" ? "已找回" : "已归还"}<small>物品已实际取回或完成归还</small></button><button id="close-post">关闭信息<small>停止寻找或招领，未完成归还</small></button></div>`,
    "更新状态",
  );
  document.getElementById("resolve-post").onclick = () => update("RESOLVED");
  document.getElementById("close-post").onclick = () => update("CLOSED");
}
function deletePost(pid) {
  const p = state.posts.get(pid);
  if (p)
    confirmAction(
      "删除这条信息？",
      "删除后无法恢复，已有聊天记录仍会保留。",
      async () => {
        await api(`/api/posts/${pid}`, {
          method: "DELETE",
          body: JSON.stringify({ version: p.version }),
        });
        toast("信息已删除");
        navigate("/me", true);
      },
    );
}
async function contact(pid) {
  return requireLogin(async () => {
    try {
      const c = await api(`/api/posts/${pid}/conversations`, {
        method: "POST",
      });
      navigate(`/messages/${c.conversation_id}`, true);
    } catch (e) {
      toast(e.message);
    }
  });
}
async function refreshUnread() {
  if (!state.user || document.hidden) return;
  const actorId = state.user.id;
  try {
    const result = await api("/api/conversations");
    if (state.user?.id !== actorId) return;
    state.unread = result.unread_total;
    document.querySelectorAll("[data-unread]").forEach((el) => {
      el.hidden = !state.unread;
      el.textContent = state.unread > 99 ? "99+" : state.unread;
    });
  } catch (e) {
    if (e.status === 401) {
      state.user = null;
      state.csrf = "";
      state.unread = 0;
      if (state.route.startsWith("/messages")) render();
    }
  }
}
async function conversations(rid) {
  if (!state.user) {
    document.getElementById("page").innerHTML = empty(
      "登录后查看站内消息",
      "和同学核实物品特征，约定归还时间。",
      '<button class="primary" data-action="login-to-messages">登录 / 注册</button>',
    );
    return;
  }
  const result = await paginated(
    "/api/conversations",
    {},
    state.conversationLimit,
  );
  if (rid !== state.renderId) return;
  state.conversations = result.items;
  state.unread = result.unread_total;
  document.getElementById("page").innerHTML =
    `<div class="page-title"><div><h1>站内消息</h1><p>每一次回应，都让物品离家更近一步。</p></div><span class="badge">${result.unread_total} 条未读</span></div>${result.items.length ? `<div class="conversation-list">${result.items.map((c) => `<button class="conversation-row" data-nav="/messages/${c.id}"><span class="avatar">${esc(c.other.nickname[0])}</span><div class="conversation-body"><div class="conversation-title"><strong>${esc(c.other.nickname)}</strong><small>${date(c.last_activity_at, true)}</small></div><div class="conversation-sub">关于：${esc(c.post?.title || "物品信息已删除")} ${c.post?.deleted_at ? "· 已删除" : ""}</div><div class="conversation-preview">${esc(c.last_message?.content || "暂无消息，打个招呼吧")}</div></div>${c.unread_count ? `<span class="unread">${c.unread_count}</span>` : icon("arrow")}</button>`).join("")}</div>` : empty("暂无消息", "先去物品详情联系发布者，开启一段对话。", '<button class="primary" data-nav="/">去失物广场</button>')}`;
  refreshUnread();
  if (result.has_more)
    document
      .getElementById("page")
      .insertAdjacentHTML(
        "beforeend",
        '<button class="load-more" data-action="more-conversations">加载更多</button>',
      );
}
async function chat(cid, rid) {
  if (!state.user) {
    document.getElementById("page").innerHTML = empty(
      "登录后继续对话",
      "聊天内容仅对双方开放。",
      '<button class="primary" data-action="login-to-chat">登录</button>',
    );
    return;
  }
  const [c, result] = await Promise.all([
    api(`/api/conversations/${cid}`),
    api(`/api/conversations/${cid}/messages`),
  ]);
  if (rid !== state.renderId) return;
  state.chat = c;
  state.messages = result.items;
  state.older = result.has_more;
  const p = c.post;
  document.getElementById("page").innerHTML =
    `<section class="chat-wrap"><header class="chat-top"><button class="icon-btn" data-nav="/messages" aria-label="返回消息列表">${icon("back")}</button><span class="avatar">${esc(c.other.nickname[0])}</span><div><strong>${esc(c.other.nickname)}</strong><small>站内对话 · 关于同一件物品</small></div></header><button class="chat-post" ${p && !p.deleted_at ? `data-nav="/posts/${p.id}"` : "disabled"}><div class="mini">${p ? imageFor(p) : art()}</div><div><h3>${p?.deleted_at ? "物品信息已删除" : esc(p?.title || "物品信息已删除")}</h3>${p ? badge(p) : ""}</div><span class="arrow">${icon("arrow")}</span></button><div class="chat-log" id="chat-log"></div><div id="chat-failure"></div><form class="chat-compose" id="chat-form"><textarea name="content" id="message-input" placeholder="输入消息，与对方核实物品…" aria-label="输入消息" maxlength="500" rows="1">${esc(state.chatDrafts[cid] || "")}</textarea><button class="primary" type="submit">${icon("send")}发送</button></form></section>`;
  drawMessages(true);
  drawFailure();
  await markRead();
}
function drawMessages(scrollBottom = false, keepPosition = false) {
  const log = document.getElementById("chat-log");
  if (!log || !state.chat) return;
  const oldHeight = log.scrollHeight,
    oldScroll = log.scrollTop;
  log.innerHTML = `<div class="chat-hint">请核实物品独有特征，确认后再约定归还。</div>${state.older ? '<button class="load-more" data-action="older-messages" style="margin:0 auto 16px">查看更早消息</button>' : ""}${
    state.messages.length
      ? state.messages
          .map((m) => {
            const own = m.sender_id === state.user.id;
            return `<div class="message ${own ? "own" : ""}"><span class="avatar">${esc((own ? state.user.nickname : state.chat.other.nickname)[0])}</span><div><div class="bubble">${esc(m.content)}</div><div class="message-time">${date(m.created_at, true)}${own ? " · 发送成功" : ""}</div></div></div>`;
          })
          .join("")
      : '<div class="chat-empty">还没有消息，先打个招呼吧。</div>'
  }`;
  if (scrollBottom) log.scrollTop = log.scrollHeight;
  else if (keepPosition)
    log.scrollTop = log.scrollHeight - oldHeight + oldScroll;
  else log.scrollTop = oldScroll;
}
async function markRead() {
  const last = state.messages.at(-1);
  if (
    !last ||
    !state.chat ||
    !document.getElementById("chat-log") ||
    document.hidden
  )
    return;
  try {
    await api(`/api/conversations/${state.chat.id}/read`, {
      method: "POST",
      body: JSON.stringify({ last_read_message_id: last.id }),
    });
    await refreshUnread();
  } catch {}
}
function drawFailure() {
  const box = document.getElementById("chat-failure");
  if (box)
    box.innerHTML =
      state.failed?.cid === state.chat?.id
        ? `<div class="failed-box">发送失败：${esc(state.failed.error)} <button data-action="retry-message">重试</button></div>`
        : "";
}
async function sendMessage(form, retry = false) {
  if (state.sending) return;
  const cid = state.chat?.id;
  if (!cid) return;
  const text = retry
    ? state.failed?.content
    : form.elements.content.value.trim();
  if (!text) return toast("请输入消息");
  if (text.length > 500) return toast("消息不能超过500字");
  state.sending = true;
  const pending = retry
    ? state.failed
    : { cid, content: text, client_message_id: token() };
  const button = form.querySelector("[type=submit]");
  button.disabled = true;
  button.textContent = "发送中…";
  try {
    await api(`/api/conversations/${cid}/messages`, {
      method: "POST",
      body: JSON.stringify(pending),
    });
    state.failed = null;
    state.chatDrafts[cid] = "";
    if (state.chat?.id === cid && document.getElementById("message-input")) {
      form.elements.content.value = "";
      await pollChat(true);
      drawFailure();
    }
  } catch (e) {
    state.failed = { ...pending, error: e.message };
    state.chatDrafts[cid] = text;
    drawFailure();
    toast(e.message);
  } finally {
    state.sending = false;
    button.disabled = false;
    button.innerHTML = `${icon("send")}发送`;
  }
}
async function pollChat(forceBottom = false) {
  const actorId = state.user?.id;
  const c = state.chat;
  const log = document.getElementById("chat-log");
  if (!c || !log || document.hidden || !state.user) return;
  const bottom = log.scrollHeight - log.scrollTop - log.clientHeight < 80;
  try {
    const result = await api(
      `/api/conversations/${c.id}/messages?after_id=${state.messages.at(-1)?.id || 0}&limit=100`,
    );
    if (
      state.user?.id !== actorId ||
      state.chat?.id !== c.id ||
      !document.getElementById("chat-log")
    )
      return;
    if (result.items.length) {
      const ids = new Set(state.messages.map((m) => m.id));
      state.messages.push(...result.items.filter((m) => !ids.has(m.id)));
      drawMessages(forceBottom || bottom);
      if (!bottom && !forceBottom)
        log.insertAdjacentHTML(
          "beforeend",
          '<button class="new-message-btn" data-action="scroll-chat">有新消息 ↓</button>',
        );
    }
    if (forceBottom || bottom) await markRead();
  } catch (e) {
    if (forceBottom) toast(e.message);
  }
}
async function render() {
  const rid = ++state.renderId;
  state.route = location.hash.slice(1) || "/";
  state.chat = null;
  shell();
  try {
    if (state.route === "/") await home(rid);
    else if (state.route === "/login" || state.route === "/register") auth();
    else if (state.route === "/posts/new") await postForm(null, rid);
    else if (/^\/posts\/[^/]+\/edit$/.test(state.route))
      await postForm(state.route.split("/")[2], rid);
    else if (/^\/posts\/[^/]+$/.test(state.route))
      await detail(state.route.split("/")[2], rid);
    else if (state.route === "/me") await mine(rid);
    else if (state.route === "/messages") await conversations(rid);
    else if (/^\/messages\/[^/]+$/.test(state.route))
      await chat(state.route.split("/")[2], rid);
    else
      document.getElementById("page").innerHTML = empty(
        "页面没有找到",
        "返回失物广场继续浏览。",
        '<button class="primary" data-nav="/">返回首页</button>',
      );
    if (rid === state.renderId)
      window.scrollTo(0, state.route === "/" ? state.scrollHome : 0);
  } catch (e) {
    if (rid !== state.renderId) return;
    document.getElementById("page").innerHTML = empty(
      e.status === 404 ? "信息已删除或不存在" : "暂时无法加载",
      e.message,
      '<button class="secondary" data-action="retry-page">重试</button><button class="primary" data-nav="/">返回首页</button>',
    );
  }
}
document.addEventListener("click", async (e) => {
  const nav = e.target.closest("[data-nav]");
  if (nav) {
    e.preventDefault();
    navigate(nav.dataset.nav);
    return;
  }
  if (e.target.matches("[data-close-backdrop]")) return closeModal();
  const el = e.target.closest("[data-action]");
  if (!el) return;
  const action = el.dataset.action;
  try {
    if (action === "close-modal") closeModal();
    else if (action === "type") {
      state.type = el.dataset.value;
      state.homeLimit = 1;
      state.scrollHome = 0;
      render();
    } else if (action === "category") {
      state.filters.category = el.dataset.value;
      state.homeLimit = 1;
      render();
    } else if (action === "filter") showFilters();
    else if (action === "clear-filters") {
      state.filters = {
        campus_id: "",
        category: "",
        status: "OPEN",
        keyword: "",
        date_from: "",
        date_to: "",
      };
      state.homeLimit = 1;
      render();
    } else if (action === "load-more") {
      state.scrollHome = window.scrollY;
      state.homeLimit++;
      render();
    } else if (action === "more-mine") {
      state.mineLimit++;
      render();
    } else if (action === "more-conversations") {
      state.conversationLimit++;
      render();
    } else if (action === "retry-page") render();
    else if (action === "mine-type") {
      state.mineType = el.dataset.value;
      state.mineLimit = 1;
      render();
    } else if (action === "status") changeStatus(el.dataset.id);
    else if (action === "delete") deletePost(el.dataset.id);
    else if (action === "contact") {
      el.disabled = true;
      await contact(el.dataset.id);
      el.disabled = false;
    } else if (action === "login-to-publish")
      requireLogin(() => navigate("/posts/new", true));
    else if (action === "login-to-me")
      requireLogin(() => navigate("/me", true));
    else if (action === "login-to-messages")
      requireLogin(() => navigate("/messages", true));
    else if (action === "login-to-chat") requireLogin(() => render());
    else if (action === "fill-demo") {
      const f = document.getElementById("auth-form");
      f.elements.account.value = el.dataset.value;
      f.elements.password.value = "demo12345";
    } else if (action === "show-password") {
      const input = document.querySelector("input[name=password]");
      input.type = input.type === "password" ? "text" : "password";
      el.textContent = input.type === "password" ? "显示密码" : "隐藏密码";
    } else if (action === "draft-type") {
      captureDraft();
      state.draft.type = el.dataset.value;
      state.dirty = true;
      drawPostForm();
    } else if (action === "remove-image") {
      captureDraft();
      state.draft.image_ids.splice(Number(el.dataset.index), 1);
      state.draft.image_urls.splice(Number(el.dataset.index), 1);
      state.dirty = true;
      drawPostForm();
      captureDraft();
    } else if (action === "preview")
      modal(
        `<div class="modal-head"><h2>物品图片</h2><button class="icon-btn" data-action="close-modal" aria-label="关闭图片">${icon("close")}</button></div><img src="${esc(el.dataset.url)}" alt="物品图片" style="width:100%;object-fit:contain;max-height:65vh">`,
        "物品图片",
      );
    else if (action === "gallery") {
      document
        .querySelectorAll(".gallery-row button")
        .forEach((b) => b.classList.toggle("active", b === el));
      document.getElementById("detail-image").innerHTML =
        `<button data-action="preview" data-url="${esc(el.dataset.url)}" style="width:100%;padding:0" aria-label="放大图片"><img src="${esc(el.dataset.url)}" alt="物品图片"></button>`;
    } else if (action === "logout")
      confirmAction(
        "退出当前账号？",
        "退出后仍可浏览物品，重新登录可继续管理发布和聊天。",
        async () => {
          await api("/api/auth/logout", { method: "POST" });
          state.user = null;
          state.csrf = "";
          state.unread = 0;
          state.draft = null;
          state.dirty = false;
          state.pending = null;
          state.expiredDraft = null;
          state.chat = null;
          state.messages = [];
          state.conversations = [];
          state.chatDrafts = {};
          state.failed = null;
          navigate("/login", true);
        },
      );
    else if (action === "retry-message")
      await sendMessage(document.getElementById("chat-form"), true);
    else if (action === "scroll-chat") {
      const log = document.getElementById("chat-log");
      log.scrollTop = log.scrollHeight;
      el.remove();
      markRead();
    } else if (action === "older-messages") {
      el.disabled = true;
      const log = document.getElementById("chat-log");
      const result = await api(
        `/api/conversations/${state.chat.id}/messages?before_id=${state.messages[0]?.id}&limit=30`,
      );
      state.messages = [...result.items, ...state.messages];
      state.older = result.has_more;
      drawMessages(false, true);
    }
  } catch (err) {
    toast(err.message);
    el.disabled = false;
  }
});
document.addEventListener("submit", async (e) => {
  if (
    !["search-form", "post-form", "auth-form", "chat-form"].includes(
      e.target.id,
    )
  )
    return;
  e.preventDefault();
  if (e.target.id === "search-form") {
    state.filters.keyword = e.target.elements.keyword.value.trim();
    state.homeLimit = 1;
    render();
  } else if (e.target.id === "post-form") await savePost(e.target);
  else if (e.target.id === "auth-form") await authenticate(e.target);
  else await sendMessage(e.target);
});
document.addEventListener("input", (e) => {
  if (e.target.closest("#post-form")) {
    state.dirty = true;
    captureDraft();
  }
  if (e.target.id === "message-input" && state.chat)
    state.chatDrafts[state.chat.id] = e.target.value;
});
document.addEventListener("change", (e) => {
  if (e.target.id === "header-campus") {
    state.filters.campus_id = e.target.value;
    state.homeLimit = 1;
    if (state.route !== "/") navigate("/");
    else render();
  } else if (e.target.id === "mine-status") {
    state.mineStatus = e.target.value;
    state.mineLimit = 1;
    render();
  } else if (e.target.id === "image-input")
    uploadImages(Array.from(e.target.files));
});
document.addEventListener("keydown", (e) => {
  if (e.key === "Escape") closeModal();
  if (
    e.target.id === "message-input" &&
    e.key === "Enter" &&
    !e.shiftKey &&
    !e.isComposing
  ) {
    e.preventDefault();
    document.getElementById("chat-form").requestSubmit();
  }
});
document.addEventListener(
  "scroll",
  (e) => {
    if (
      e.target.id === "chat-log" &&
      e.target.scrollHeight - e.target.scrollTop - e.target.clientHeight < 40
    ) {
      e.target.querySelector(".new-message-btn")?.remove();
      markRead();
    }
  },
  true,
);
window.addEventListener("session-expired", () => {
  if (!state.user) return;
  captureDraft();
  state.expiredDraft = { userId: state.user.id, draft: state.draft };
  const returnRoute = state.route;
  state.user = null;
  state.csrf = "";
  state.unread = 0;
  state.chat = null;
  state.messages = [];
  state.conversations = [];
  state.chatDrafts = {};
  state.failed = null;
  state.pending = { returnRoute, action: () => {} };
  state.renderId++;
  navigate("/login", true);
  toast("登录已失效，请重新登录");
});
window.addEventListener("beforeunload", (e) => {
  if (state.dirty && document.getElementById("post-form")) {
    e.preventDefault();
    e.returnValue = "";
  }
});
window.addEventListener("hashchange", () => {
  closeModal();
  render();
});
document.addEventListener("visibilitychange", () => {
  if (!document.hidden) {
    refreshUnread();
    pollChat();
  }
});
let polling = false;
setInterval(async () => {
  if (polling || document.hidden) return;
  polling = true;
  try {
    await refreshUnread();
    if (state.route === "/messages" && state.user) {
      const inputActive = document.activeElement?.tagName === "INPUT";
      if (!inputActive) await conversations(state.renderId);
    }
    await pollChat();
  } finally {
    polling = false;
  }
}, 5000);
async function start() {
  try {
    state.campuses = await api("/api/campuses");
    try {
      const authResult = await api("/api/auth/me");
      state.user = authResult.user;
      state.csrf = authResult.csrf;
    } catch {}
    await render();
    refreshUnread();
  } catch (e) {
    app.innerHTML = `<main style="padding:50px">${empty("无法连接本地服务", e.message, '<button class="primary" onclick="location.reload()">重试</button>')}</main>`;
  }
}
start();

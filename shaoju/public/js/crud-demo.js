/**
 * 失物 / 招领 CRUD 接口示例。
 *
 * 全部请求都用原生 Fetch API 直接发出，方便对照接口路径阅读：
 *
 *   GET    /api/items            列表（公开，只返回审核通过的）
 *   GET    /api/items/categories 分类字典
 *   POST   /api/items            创建（需登录）
 *   GET    /api/items/:id        详情（未过审的仅本人与管理员可见）
 *   PUT    /api/items/:id        整体替换（本人或管理员）
 *   PATCH  /api/items/:id        局部更新（本人或管理员）
 *   POST   /api/items/:id/status 修改进度状态（寻找中 / 已找到 / 已结束）
 *   DELETE /api/items/:id        删除（本人或管理员）
 *
 * 关键点：Cookie 会自动带上（credentials: 'same-origin'），后端从会话里解析出用户 ID，
 * 再和数据库里的 items.user_id 比对，所以前端不需要（也无法）自己声明「我是谁」。
 */
(function () {
  'use strict';

  const state = {
    page: 1,
    pageSize: 4,
    total: 0,
    totalPages: 1,
    items: [],
    categories: [],
    current: null, // 当前选中的条目（详情接口返回的完整对象）
    canEdit: false,
  };

  const $ = (id) => document.getElementById(id);

  /* ------------------------------------------------------------------ *
   * 0. 一个最小的 Fetch 封装
   * ------------------------------------------------------------------ */

  /**
   * 调用后端接口。
   * 约定：成功 { data: ... }；失败为 HTTP 状态码 + { message: '...' }。
   * 失败时抛出的 Error 上带 status 字段，调用方可以按 401 / 403 / 404 分别处理。
   */
  async function request(method, path, body) {
    const init = {
      method,
      credentials: 'same-origin', // 让浏览器带上会话 Cookie
      headers: {},
    };
    if (body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body);
    }

    let response;
    try {
      response = await fetch(path, init);
    } catch {
      throw new Error('无法连接服务器，请确认后端已启动');
    }

    const text = await response.text();
    let payload = null;
    if (text) {
      try {
        payload = JSON.parse(text);
      } catch {
        payload = null;
      }
    }

    if (!response.ok) {
      const error = new Error((payload && payload.message) || 'HTTP ' + response.status);
      error.status = response.status;
      throw error;
    }
    return payload ? payload.data : null;
  }

  /** 把对象拼成查询串，跳过空值。 */
  function toQuery(params) {
    const search = new URLSearchParams();
    for (const [key, value] of Object.entries(params)) {
      if (value === undefined || value === null || value === '') continue;
      search.append(key, value);
    }
    const text = search.toString();
    return text ? '?' + text : '';
  }

  /* ------------------------------------------------------------------ *
   * 1. 列表：GET /api/items
   * ------------------------------------------------------------------ */

  async function loadCategories() {
    const data = await request('GET', '/api/items/categories');
    state.categories = data.categories;

    for (const id of ['f-category', 'c-category', 'e-category']) {
      const select = $(id);
      select.innerHTML = state.categories
        .map((name) => '<option value="' + App.esc(name) + '">' + App.esc(name) + '</option>')
        .join('');
    }
  }

  function readFilters() {
    return {
      type: $('f-type').value,
      category: $('f-category').value,
      status: $('f-status').value,
      keyword: $('f-keyword').value.trim(),
      page: state.page,
      pageSize: state.pageSize,
    };
  }

  async function loadList() {
    const data = await request('GET', '/api/items' + toQuery(readFilters()));

    state.items = data.items;
    state.total = data.total;
    state.totalPages = data.totalPages;
    renderList();
    App.renderPager($('pager'), { page: data.page, totalPages: data.totalPages, total: data.total }, (page) => {
      state.page = page;
      loadList().catch(showError);
    });
  }

  function renderList() {
    const grid = $('item-grid');
    if (!state.items.length) {
      grid.innerHTML = '<div class="empty"><div class="big">🔍</div>没有符合条件的公开信息</div>';
      return;
    }

    grid.innerHTML = state.items
      .map(function (item) {
        return (
          '<div class="item-card">' +
          App.typeBadge(item.type) +
          App.statusBadge(item.status) +
          App.auditBadge(item.auditStatus) +
          '<h3>' + App.esc(item.title) + '</h3>' +
          '<p class="desc">' + App.esc(item.description || '（没有填写描述）') + '</p>' +
          '<div class="meta">' +
          '<span>📍 ' + App.esc(item.location || '未填写') + '</span>' +
          '<span>🗂 ' + App.esc(item.category) + '</span>' +
          '<span>👤 ' + App.esc(item.owner.name || item.owner.studentId) + '</span>' +
          '</div>' +
          '<div class="btn-row mt-16">' +
          '<button class="btn sm" data-select="' + item.id + '">选中</button>' +
          '<a class="btn ghost sm" href="/item?id=' + item.id + '">详情页</a>' +
          '</div>' +
          '</div>'
        );
      })
      .join('');

    grid.querySelectorAll('button[data-select]').forEach(function (button) {
      button.addEventListener('click', function () {
        selectItem(Number(button.dataset.select)).catch(showError);
      });
    });
  }

  /* ------------------------------------------------------------------ *
   * 2. 创建：POST /api/items
   * ------------------------------------------------------------------ */

  async function createItem(event) {
    event.preventDefault();
    App.hideAlert($('create-alert'));

    const body = {
      type: $('c-type').value,
      title: $('c-title').value.trim(),
      category: $('c-category').value,
      description: $('c-description').value.trim(),
      location: $('c-location').value.trim(),
      storagePlace: $('c-storagePlace').value.trim(),
      happenedAt: $('c-happenedAt').value.trim(),
      contact: $('c-contact').value.trim(),
      imageUrl: $('c-imageUrl').value.trim(),
    };

    if (!body.title) {
      App.showAlert($('create-alert'), '标题不能为空', 'error');
      return;
    }

    try {
      const data = await request('POST', '/api/items', body);
      App.toast('创建成功，编号 ' + data.item.id + '（待审核）');
      $('create-form').reset();
      state.page = 1;
      await loadList();
    } catch (err) {
      if (err.status === 401) {
        App.showAlert($('create-alert'), '请先登录：未登录时后端返回 401 ' + err.message, 'error');
        return;
      }
      App.showAlert($('create-alert'), err.message, 'error');
    }
  }

  /* ------------------------------------------------------------------ *
   * 3. 详情：GET /api/items/:id
   * ------------------------------------------------------------------ */

  async function selectItem(id) {
    App.hideAlert($('edit-alert'));
    state.current = await request('GET', '/api/items/' + id);
    state.canEdit = Boolean(state.current.canEdit);

    const item = state.current.item;
    $('selected-tip').innerHTML =
      '当前选中：<b>#' + item.id + ' ' + App.esc(item.title) + '</b>　' +
      App.statusBadge(item.status) + '　' +
      (state.canEdit ? '<span class="badge found">你是发布者或管理员，可以修改</span>'
                     : '<span class="badge neutral">不是发布者，修改/删除会被拒绝（403）</span>');

    // 切换按钮的文案跟着当前状态走：寻找中 -> 已找到 -> 已结束 -> 重新开放
    const next = STATUS_NEXT[item.status] || STATUS_NEXT.open;
    $('btn-toggle-status').textContent = 'POST ' + next.label;

    $('detail-box').innerHTML =
      row('ID', item.id) +
      row('类型', App.typeLabel(item.type)) +
      row('标题', item.title) +
      row('分类', item.category) +
      row('地点', item.location) +
      row('寄放处', item.storagePlace) +
      row('时间', item.happenedAt) +
      row('联系方式', item.contact) +
      row('进度', App.statusLabel(item.status)) +
      row('审核', App.esc(item.auditStatus) + (item.auditRemark
        ? '（<span class="reject-reason">' + App.esc(item.auditRemark) + '</span>）'
        : ''), true) +
      row('发布者', item.owner.name + '（' + item.owner.studentId + '）') +
      row('创建时间', App.formatDate(item.createdAt)) +
      row('描述', item.description);

    fillEditForm(item);
  }

  function row(key, value, raw) {
    return '<div class="detail-row"><span class="k">' + App.esc(key) + '</span><span>' +
      (raw ? value : App.esc(value === undefined || value === null || value === '' ? '—' : value)) + '</span></div>';
  }

  function fillEditForm(item) {
    $('edit-form').style.display = '';
    $('e-type').value = item.type;
    $('e-title').value = item.title;
    $('e-category').value = item.category;
    $('e-description').value = item.description;
    $('e-location').value = item.location;
    $('e-storagePlace').value = item.storagePlace || '';
    $('e-happenedAt').value = item.happenedAt;
    $('e-contact').value = item.contact;
    $('e-status').value = item.status;
  }

  function readEditForm() {
    return {
      type: $('e-type').value,
      title: $('e-title').value.trim(),
      category: $('e-category').value,
      description: $('e-description').value.trim(),
      location: $('e-location').value.trim(),
      storagePlace: $('e-storagePlace').value.trim(),
      happenedAt: $('e-happenedAt').value.trim(),
      contact: $('e-contact').value.trim(),
      status: $('e-status').value,
    };
  }

  /* ------------------------------------------------------------------ *
   * 4. 修改：PUT / PATCH /api/items/:id
   * ------------------------------------------------------------------ */

  /** PUT：请求体里没写的字段会被清空，所以这里提交表单的全部字段。 */
  async function saveWithPut() {
    await save('PUT', readEditForm(), 'PUT 整体替换成功');
  }

  /** PATCH：只发送改动过的字段，其余保持原值。 */
  async function saveWithPatch() {
    const item = state.current.item;
    const form = readEditForm();
    const changed = {};
    for (const [key, value] of Object.entries(form)) {
      if (value !== item[key]) changed[key] = value;
    }

    if (!Object.keys(changed).length) {
      App.showAlert($('edit-alert'), '没有字段发生变化，PATCH 请求体为空会被后端拒绝（400 没有需要修改的字段）', 'info');
      return;
    }
    await save('PATCH', changed, 'PATCH 局部更新成功：' + Object.keys(changed).join('、'));
  }

  async function save(method, body, successText) {
    if (!state.current) return;
    App.hideAlert($('edit-alert'));

    try {
      const data = await request(method, '/api/items/' + state.current.item.id, body);
      App.toast(successText);
      App.showAlert($('edit-alert'), successText + '（当前审核状态：' + data.item.auditStatus + '）', 'success');
      await selectItem(data.item.id);
      await loadList();
    } catch (err) {
      App.showAlert($('edit-alert'), describeError(err), 'error');
    }
  }

  /* ------------------------------------------------------------------ *
   * 5. 状态与删除
   * ------------------------------------------------------------------ */

  /**
   * 三态的推荐流转：寻找中 -> 已找到 -> 已结束 -> 重新开放。
   * 后端不限制流转方向，这里只是为了给「一键切换」按钮定一个顺序。
   */
  const STATUS_NEXT = {
    open: { status: 'found', label: '标记为已找到' },
    found: { status: 'closed', label: '标记为已结束' },
    closed: { status: 'open', label: '重新开放为寻找中' },
  };

  async function toggleStatus() {
    if (!state.current) return;
    const item = state.current.item;
    const next = STATUS_NEXT[item.status] || STATUS_NEXT.open;
    App.hideAlert($('edit-alert'));

    try {
      const data = await request('POST', '/api/items/' + item.id + '/status', { status: next.status });
      App.toast(next.label + '成功');
      App.showAlert($('edit-alert'), '进度状态已改为「' + App.statusLabel(data.item.status) + '」', 'success');
      await selectItem(item.id);
      await loadList();
    } catch (err) {
      App.showAlert($('edit-alert'), describeError(err), 'error');
    }
  }

  async function deleteItem() {
    if (!state.current) return;
    const item = state.current.item;
    if (!window.confirm('确定要删除「' + item.title + '」吗？该操作不可撤销。')) return;

    try {
      await request('DELETE', '/api/items/' + item.id);
      App.toast('已删除');
      state.current = null;
      state.canEdit = false;
      $('detail-box').innerHTML = '';
      $('edit-form').style.display = 'none';
      $('selected-tip').textContent = '已删除，请重新在列表里选择。';
      App.hideAlert($('edit-alert'));
      await loadList();
    } catch (err) {
      App.showAlert($('edit-alert'), describeError(err), 'error');
    }
  }

  /** 把后端的状态码翻译成人话，顺便说明权限规则。 */
  function describeError(err) {
    if (err.status === 401) return '401 未登录：请先登录再操作。' + err.message;
    if (err.status === 403) return '403 权限不足：只能修改或删除自己发布的信息。' + err.message;
    if (err.status === 404) return '404 找不到：' + err.message;
    return err.message;
  }

  function showError(err) {
    App.showAlert($('alert'), describeError(err), 'error');
  }

  /* ------------------------------------------------------------------ *
   * 6. 页面初始化
   * ------------------------------------------------------------------ */

  (async function init() {
    App.renderHeader(null);

    try {
      await loadCategories();
    } catch (err) {
      showError(err);
    }

    const user = await App.loadUser();
    $('who').innerHTML = user
      ? '当前登录：<b>' + App.esc(user.name || user.studentId) + '</b>（学号 ' + App.esc(user.studentId) +
        '，角色 ' + App.esc(user.role) + '）—— 可以创建信息，也只能改自己发布的信息。'
      : '当前未登录：列表和详情可以直接看，但创建 / 修改 / 删除会返回 <b>401 请先登录</b>。' +
        '<a href="/login?redirect=/crud-demo">去登录</a>，或用演示账号 <b>zhangsan / 123456</b>。';

    $('f-pageSize').value = String(state.pageSize);
    await loadList().catch(showError);

    $('btn-search').addEventListener('click', function () {
      state.page = 1;
      state.pageSize = Number($('f-pageSize').value);
      loadList().catch(showError);
    });
    $('btn-reset').addEventListener('click', function () {
      for (const id of ['f-type', 'f-category', 'f-status', 'f-keyword']) $(id).value = '';
      state.page = 1;
      loadList().catch(showError);
    });
    $('f-keyword').addEventListener('keydown', function (event) {
      if (event.key === 'Enter') $('btn-search').click();
    });

    $('create-form').addEventListener('submit', createItem);
    $('btn-put').addEventListener('click', function () { saveWithPut().catch(showError); });
    $('btn-patch').addEventListener('click', function () { saveWithPatch().catch(showError); });
    $('btn-toggle-status').addEventListener('click', function () { toggleStatus().catch(showError); });
    $('btn-refresh-detail').addEventListener('click', function () {
      if (state.current) selectItem(state.current.item.id).catch(showError);
    });
    $('btn-delete').addEventListener('click', deleteItem);
  })();
})();

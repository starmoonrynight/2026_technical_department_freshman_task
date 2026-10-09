(function () {
  'use strict';

  const els = {
    stats: document.getElementById('stats'),
    tabs: document.getElementById('tabs'),
    thead: document.getElementById('thead'),
    tbody: document.getElementById('tbody'),
    pager: document.getElementById('pager'),
    alert: document.getElementById('alert'),
    filters: document.getElementById('filters'),
    keyword: document.getElementById('keyword'),
    auditFilter: document.getElementById('auditFilter'),
    filterBtn: document.getElementById('filter-btn'),
  };

  const state = { view: 'pending', page: 1, pageSize: 10, keyword: '', auditStatus: '' };

  /* ---------------- 统计 ---------------- */

  async function loadStats() {
    try {
      const data = await API.get('/api/admin/stats');
      const s = data.stats;

      const card = (label, value, warn) =>
        '<div class="stat"><div class="label">' + label + '</div>' +
        '<div class="value' + (warn ? ' warn' : '') + '">' + value + '</div></div>';

      // 进度状态卡片由后端的 byStatus 驱动，顺序固定为 寻找中 / 已找到 / 已结束
      const statusCards = (s.byStatus || [])
        .map((row) => card(App.statusLabel(row.status), row.count))
        .join('');

      els.stats.innerHTML =
        card('信息总数', s.total) +
        card('寻物启事', s.lost) +
        card('失物招领', s.found) +
        statusCards +
        card('待审核', s.pending, s.pending > 0) +
        card('注册用户', data.userCount) +
        card('解决率', s.resolvedRate + '%');
    } catch (err) {
      App.showAlert(els.alert, err.message, 'error');
    }
  }

  /* ---------------- 表格渲染 ---------------- */

  const HEADERS = {
    items: ['标题', '类型', '分类', '发布者', '进度', '审核', '发布时间', '操作'],
    users: ['ID', '学号', '姓名', '联系方式', '角色', '状态', '发布数', '操作'],
  };

  function renderHead() {
    const cols = HEADERS[state.view] || HEADERS.items;
    els.thead.innerHTML = '<tr>' + cols.map((c) => '<th>' + c + '</th>').join('') + '</tr>';
  }

  function itemRow(item) {
    const remark = item.auditStatus === 'rejected' && item.auditRemark
      ? '<div class="reject-reason">驳回原因：' + App.esc(item.auditRemark) + '</div>'
      : '';

    const actions = ['<a class="btn ghost sm" href="/item?id=' + item.id + '">查看</a>'];

    if (item.auditStatus !== 'approved') {
      actions.push('<button class="btn success sm" data-action="approve" data-id="' + item.id + '">通过</button>');
    }
    if (item.auditStatus !== 'rejected') {
      actions.push('<button class="btn ghost sm" data-action="reject" data-id="' + item.id + '">驳回</button>');
    }
    actions.push('<button class="btn danger sm" data-action="delete-item" data-id="' + item.id + '">删除</button>');

    return (
      '<tr>' +
      '<td><span class="cell-title">' + App.esc(item.title) + '</span>' + remark + '</td>' +
      '<td>' + App.typeBadge(item.type) + '</td>' +
      '<td>' + App.esc(item.category) + '</td>' +
      '<td class="small">' + App.esc(item.owner.name || item.owner.nickname || item.owner.studentId) + '</td>' +
      '<td>' + App.statusBadge(item.status) + '</td>' +
      '<td>' + App.auditBadge(item.auditStatus) + '</td>' +
      '<td class="small muted">' + App.formatDate(item.createdAt) + '</td>' +
      '<td>' + actions.join(' ') + '</td>' +
      '</tr>'
    );
  }

  function userRow(user) {
    const roleBadge = user.role === 'admin'
      ? '<span class="badge primary">管理员</span>'
      : '<span class="badge neutral">普通用户</span>';

    const statusBadge = user.status === 'active'
      ? '<span class="badge found">正常</span>'
      : '<span class="badge danger">已停用</span>';

    const toggleStatus = user.status === 'active'
      ? '<button class="btn danger sm" data-action="disable-user" data-id="' + user.id + '">停用</button>'
      : '<button class="btn success sm" data-action="enable-user" data-id="' + user.id + '">启用</button>';

    const toggleRole = user.role === 'admin'
      ? '<button class="btn ghost sm" data-action="demote" data-id="' + user.id + '">取消管理员</button>'
      : '<button class="btn ghost sm" data-action="promote" data-id="' + user.id + '">设为管理员</button>';

    return (
      '<tr>' +
      '<td>' + user.id + '</td>' +
      '<td><span class="cell-title">' + App.esc(user.studentId || user.username) + '</span></td>' +
      '<td>' + App.esc(user.name || user.nickname || '-') + '</td>' +
      '<td class="small muted">' + App.esc(user.contact || '未填写') + '</td>' +
      '<td>' + roleBadge + '</td>' +
      '<td>' + statusBadge + '</td>' +
      '<td>' + (user.itemCount === undefined ? '-' : user.itemCount) + '</td>' +
      '<td>' + toggleStatus + ' ' + toggleRole + '</td>' +
      '</tr>'
    );
  }

  async function load() {
    App.hideAlert(els.alert);
    renderHead();
    els.tbody.innerHTML = '<tr><td colspan="8" class="muted">加载中…</td></tr>';
    els.pager.innerHTML = '';

    try {
      if (state.view === 'users') {
        const data = await API.get('/api/admin/users');
        els.tbody.innerHTML = data.users.length
          ? data.users.map(userRow).join('')
          : '<tr><td colspan="8" class="empty">暂无用户</td></tr>';
        els.pager.innerHTML = '<span class="info">共 ' + data.users.length + ' 个用户</span>';
        return;
      }

      const auditStatus = state.view === 'pending' ? 'pending' : state.auditStatus;

      const data = await API.get('/api/admin/items', {
        auditStatus: auditStatus,
        keyword: state.keyword,
        page: state.page,
        pageSize: state.pageSize,
      });

      els.tbody.innerHTML = data.items.length
        ? data.items.map(itemRow).join('')
        : '<tr><td colspan="8" class="empty">没有符合条件的信息</td></tr>';

      App.renderPager(els.pager, data, function (page) {
        state.page = page;
        load();
      });
    } catch (err) {
      els.tbody.innerHTML = '';
      App.showAlert(els.alert, err.message, 'error');
    }
  }

  /* ---------------- 事件 ---------------- */

  els.tabs.addEventListener('click', function (event) {
    const btn = event.target.closest('button[data-view]');
    if (!btn) return;

    els.tabs.querySelectorAll('button').forEach(function (b) {
      b.classList.toggle('active', b === btn);
    });

    state.view = btn.dataset.view;
    state.page = 1;
    els.filters.style.display = state.view === 'items' ? 'flex' : 'none';
    load();
  });

  els.filterBtn.addEventListener('click', function () {
    state.keyword = els.keyword.value.trim();
    state.auditStatus = els.auditFilter.value;
    state.page = 1;
    load();
  });

  els.keyword.addEventListener('keydown', function (event) {
    if (event.key === 'Enter') els.filterBtn.click();
  });

  els.tbody.addEventListener('click', async function (event) {
    const btn = event.target.closest('button[data-action]');
    if (!btn) return;

    const id = btn.dataset.id;
    const action = btn.dataset.action;
    App.hideAlert(els.alert);

    try {
      if (action === 'approve') {
        await API.post('/api/admin/items/' + id + '/audit', { action: 'approve' });
        App.toast('已通过审核');
      } else if (action === 'reject') {
        const remark = prompt('请输入驳回原因：');
        if (remark === null) return;
        if (!remark.trim()) {
          App.showAlert(els.alert, '驳回时必须填写原因', 'error');
          return;
        }
        await API.post('/api/admin/items/' + id + '/audit', { action: 'reject', remark: remark.trim() });
        App.toast('已驳回');
      } else if (action === 'delete-item') {
        if (!confirm('确定要删除这条信息吗？')) return;
        await API.del('/api/admin/items/' + id);
        App.toast('已删除');
      } else if (action === 'enable-user' || action === 'disable-user') {
        const status = action === 'disable-user' ? 'disabled' : 'active';
        if (status === 'disabled' && !confirm('停用后该用户将无法登录，确定继续吗？')) return;
        await API.post('/api/admin/users/' + id + '/status', { status: status });
        App.toast(status === 'disabled' ? '已停用该账号' : '已启用该账号');
      } else if (action === 'promote' || action === 'demote') {
        const role = action === 'promote' ? 'admin' : 'user';
        if (role === 'user' && !confirm('确定要取消该用户的管理员权限吗？')) return;
        await API.post('/api/admin/users/' + id + '/role', { role: role });
        App.toast(role === 'admin' ? '已设为管理员' : '已取消管理员');
      } else {
        return;
      }

      await loadStats();
      await load();
    } catch (err) {
      App.showAlert(els.alert, err.message, 'error');
    }
  });

  /* ---------------- 初始化 ---------------- */

  (async function init() {
    const user = await App.requireLogin();
    if (!user) return;

    if (user.role !== 'admin') {
      await App.renderHeader(null);
      App.showAlert(els.alert, '当前账号不是管理员，无法访问管理后台。', 'error');
      document.getElementById('table').style.display = 'none';
      return;
    }

    await App.renderHeader('/admin');
    await loadStats();
    await load();
  })();
})();

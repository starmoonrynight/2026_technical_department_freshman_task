(function () {
  'use strict';

  const els = {
    tabs: document.getElementById('tabs'),
    tbody: document.getElementById('tbody'),
    pager: document.getElementById('pager'),
    alert: document.getElementById('alert'),
  };

  const state = { auditStatus: '', page: 1, pageSize: 10 };

  function rowHtml(item) {
    const remark = item.auditStatus === 'rejected' && item.auditRemark
      ? '<div class="reject-reason">驳回原因：' + App.esc(item.auditRemark) + '</div>'
      : '';

    return (
      '<tr>' +
      '<td><a class="cell-title" href="/item?id=' + item.id + '">' + App.esc(item.title) + '</a>' + remark + '</td>' +
      '<td>' + App.typeBadge(item.type) + '</td>' +
      '<td>' + App.esc(item.category) + '</td>' +
      '<td>' + App.statusBadge(item.status) + '</td>' +
      '<td>' + App.auditBadge(item.auditStatus) + '</td>' +
      '<td class="small muted">' + App.formatDate(item.createdAt) + '</td>' +
      '<td>' +
      '<a class="btn ghost sm" href="/publish?id=' + item.id + '">编辑</a> ' +
      // 三态直接在下拉框里切换：寻找中 / 已找到 / 已结束
      '<select class="status-select status-' + App.esc(item.status) + '" data-action="status" data-id="' + item.id +
      '" data-current="' + App.esc(item.status) + '">' + App.statusOptions(item.status) + '</select> ' +
      '<button class="btn danger sm" data-action="delete" data-id="' + item.id + '">删除</button>' +
      '</td>' +
      '</tr>'
    );
  }

  async function load() {
    App.hideAlert(els.alert);
    els.tbody.innerHTML = '<tr><td colspan="7" class="muted">加载中…</td></tr>';

    try {
      const data = await API.get('/api/items/mine', {
        auditStatus: state.auditStatus,
        page: state.page,
        pageSize: state.pageSize,
      });

      if (!data.items.length) {
        els.tbody.innerHTML = '<tr><td colspan="7" class="empty">还没有发布任何信息，去<a href="/publish">发布一条</a>吧</td></tr>';
      } else {
        els.tbody.innerHTML = data.items.map(rowHtml).join('');
      }

      App.renderPager(els.pager, data, function (page) {
        state.page = page;
        load();
      });
    } catch (err) {
      els.tbody.innerHTML = '';
      App.showAlert(els.alert, err.message, 'error');
    }
  }

  els.tabs.addEventListener('click', function (event) {
    const btn = event.target.closest('button[data-audit]');
    if (!btn) return;

    els.tabs.querySelectorAll('button').forEach(function (b) {
      b.classList.toggle('active', b === btn);
    });

    state.auditStatus = btn.dataset.audit;
    state.page = 1;
    load();
  });

  /** 下拉框改状态：只有发布者本人能改（后端 RequireItemOwner 会校验）。 */
  els.tbody.addEventListener('change', async function (event) {
    const select = event.target.closest('select[data-action="status"]');
    if (!select) return;

    const id = select.dataset.id;
    const previous = select.dataset.current;
    const next = select.value;
    if (next === previous) return;

    App.hideAlert(els.alert);
    select.disabled = true;

    try {
      const data = await API.post('/api/items/' + id + '/status', { status: next });
      App.toast('状态已改为「' + App.statusLabel(data.item.status) + '」');
      load();
    } catch (err) {
      App.showAlert(els.alert, err.message, 'error');
      select.value = previous; // 失败时回滚下拉框
      select.disabled = false;
    }
  });

  els.tbody.addEventListener('click', async function (event) {
    const btn = event.target.closest('button[data-action]');
    if (!btn) return;

    const id = btn.dataset.id;
    App.hideAlert(els.alert);

    if (btn.dataset.action === 'delete') {
      if (!confirm('确定要删除这条信息吗？删除后无法恢复。')) return;
      btn.disabled = true;
      try {
        await API.del('/api/items/' + id);
        App.toast('已删除');
        load();
      } catch (err) {
        App.showAlert(els.alert, err.message, 'error');
        btn.disabled = false;
      }
    }
  });

  (async function init() {
    const user = await App.requireLogin();
    if (!user) return;

    await App.renderHeader('/my');
    await load();
  })();
})();

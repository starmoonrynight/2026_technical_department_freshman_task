(function () {
  'use strict';

  const detail = document.getElementById('detail');
  const alertBox = document.getElementById('alert');
  const id = new URLSearchParams(location.search).get('id');

  let item = null;

  function row(label, value) {
    return '<div class="detail-row"><span class="k">' + label + '</span><span>' +
      (value ? App.esc(value) : '<span class="muted">未填写</span>') + '</span></div>';
  }

  function render() {
    const image = item.imageUrl
      ? '<img class="detail-image" src="' + App.esc(item.imageUrl) + '" alt="' + App.esc(item.title) + '" loading="lazy">'
      : '';

    const notice = item.auditStatus === 'pending'
      ? '<div class="alert info show">该信息还在等待管理员审核，暂未在首页公开展示。</div>'
      : item.auditStatus === 'rejected'
        ? '<div class="alert error show">该信息未通过审核：' +
          '<span class="reject-reason">' + App.esc(item.auditRemark || '未填写原因') + '</span></div>'
        : '';

    // 三态操作按钮：只显示「能改成的目标状态」，当前状态对应的按钮不出现
    const statusButtons = item.canEdit
      ? (item.status !== 'found' ? statusButton('found', '标记为已找到', 'success') : '') +
        (item.status !== 'closed' ? statusButton('closed', '标记为已结束', 'ghost') : '') +
        (item.status !== 'open' ? statusButton('open', '重新开放为寻找中', 'ghost') : '')
      : '';

    const actions = item.canEdit
      ? '<div class="btn-row mt-16">' +
        '<a class="btn ghost" href="/publish?id=' + item.id + '">修改</a>' +
        statusButtons +
        '<button class="btn danger" id="delete-btn">删除</button>' +
        '</div>'
      : '';

    detail.innerHTML =
      '<div class="card">' +
      notice +
      '<div class="btn-row" style="gap:8px;margin-bottom:10px">' +
      App.typeBadge(item.type) + App.statusBadge(item.status) + App.auditBadge(item.auditStatus) +
      '<span class="badge neutral">' + App.esc(item.category) + '</span>' +
      '</div>' +
      '<h1 class="page-title">' + App.esc(item.title) + '</h1>' +
      '<p class="page-subtitle">发布于 ' + App.formatDate(item.createdAt) +
      ' · 发布者 ' + App.esc(item.owner.name || item.owner.nickname || item.owner.studentId) + '</p>' +
      image +
      '<p style="white-space:pre-wrap">' + (item.description ? App.esc(item.description) : '<span class="muted">暂无详细描述</span>') + '</p>' +
      '<div class="mt-16">' +
      row('地点', item.location) +
      row('寄放处', item.storagePlace) +
      row('时间', item.happenedAt) +
      row('联系方式', item.contact) +
      '</div>' +
      actions +
      '</div>';

    bindActions();
  }

  function statusButton(status, label, kind) {
    return '<button class="btn ' + kind + '" data-status="' + status + '">' + label + '</button>';
  }

  /** 只有发布者本人或管理员能调用这个接口，其它人会被后端拒绝（403）。 */
  async function setStatus(status, button) {
    button.disabled = true;
    try {
      const data = await API.post('/api/items/' + item.id + '/status', { status });
      item = Object.assign({}, item, data.item, { canEdit: true });
      App.toast('状态已改为「' + App.statusLabel(data.item.status) + '」');
      render();
    } catch (err) {
      App.showAlert(alertBox, err.message, 'error');
      button.disabled = false;
    }
  }

  function bindActions() {
    detail.querySelectorAll('button[data-status]').forEach(function (button) {
      button.addEventListener('click', function () {
        setStatus(button.dataset.status, button);
      });
    });

    const remove = document.getElementById('delete-btn');
    if (remove) {
      remove.addEventListener('click', async function () {
        if (!confirm('确定要删除这条信息吗？删除后无法恢复。')) return;
        remove.disabled = true;
        try {
          await API.del('/api/items/' + item.id);
          App.toast('已删除');
          setTimeout(function () {
            location.href = '/my';
          }, 500);
        } catch (err) {
          App.showAlert(alertBox, err.message, 'error');
          remove.disabled = false;
        }
      });
    }
  }

  (async function init() {
    await App.renderHeader(null);

    if (!id) {
      App.showAlert(alertBox, '缺少信息编号', 'error');
      return;
    }

    detail.innerHTML = '<div class="card empty">加载中…</div>';

    try {
      const data = await API.get('/api/items/' + encodeURIComponent(id));
      item = data.item;
      document.title = item.title + ' · 校园失物招领';
      render();
    } catch (err) {
      detail.innerHTML = '';
      App.showAlert(alertBox, err.message, 'error');
    }
  })();
})();

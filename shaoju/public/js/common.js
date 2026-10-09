/**
 * 公共工具：登录态、顶部导航、提示、分页。
 */
(function (global) {
  'use strict';

  const state = { user: null, loading: null };

  /* ---------------- 基础工具 ---------------- */

  function esc(value) {
    return String(value === undefined || value === null ? '' : value).replace(/[&<>"']/g, function (ch) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch];
    });
  }

  function pad(n) {
    return String(n).padStart(2, '0');
  }

  function formatDate(value) {
    if (!value) return '';
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    return date.getFullYear() + '-' + pad(date.getMonth() + 1) + '-' + pad(date.getDate()) +
      ' ' + pad(date.getHours()) + ':' + pad(date.getMinutes());
  }

  function typeLabel(type) {
    return type === 'lost' ? '寻物启事' : '失物招领';
  }

  function typeBadge(type) {
    return '<span class="badge ' + (type === 'lost' ? 'lost' : 'found') + '">' + typeLabel(type) + '</span>';
  }

  /**
   * 进度状态：open = 寻找中（红） / found = 已找到（绿） / closed = 已结束（灰）。
   * 状态值与后端 items.status 一一对应。
   */
  const STATUS_LABELS = { open: '寻找中', found: '已找到', closed: '已结束' };

  function statusLabel(status) {
    return STATUS_LABELS[status] || status || '';
  }

  function statusBadge(status) {
    const kind = STATUS_LABELS[status] ? status : 'open';
    return '<span class="badge status-' + kind + '">' + statusLabel(kind) + '</span>';
  }

  /** 生成三态下拉框的 <option>，用于「我的发布」里直接改状态。 */
  function statusOptions(current) {
    return Object.keys(STATUS_LABELS)
      .map(function (value) {
        return '<option value="' + value + '"' + (value === current ? ' selected' : '') + '>' +
          STATUS_LABELS[value] + '</option>';
      })
      .join('');
  }

  function auditBadge(status) {
    if (status === 'approved') return '<span class="badge found">已通过</span>';
    if (status === 'rejected') return '<span class="badge danger">已驳回</span>';
    return '<span class="badge warn">待审核</span>';
  }

  /* ---------------- 登录态 ---------------- */

  function loadUser(force) {
    if (!state.loading || force) {
      state.loading = API.get('/api/auth/me')
        .then(function (data) {
          state.user = data && data.user ? data.user : null;
          return state.user;
        })
        .catch(function () {
          state.user = null;
          return null;
        });
    }
    return state.loading;
  }

  function currentUser() {
    return state.user;
  }

  /** 需要登录的页面调用；未登录则跳转到登录页。 */
  async function requireLogin() {
    const user = await loadUser();
    if (!user) {
      const back = encodeURIComponent(location.pathname + location.search);
      location.replace('/login?redirect=' + back);
      return null;
    }
    return user;
  }

  /* ---------------- 顶部导航 ---------------- */

  const NAV = [
    { href: '/', label: '首页' },
    { href: '/publish', label: '发布信息' },
    { href: '/my', label: '我的发布' },
  ];

  async function renderHeader(active) {
    const mount = document.getElementById('header');
    if (!mount) return;

    const user = await loadUser();

    const links = NAV.map(function (item) {
      const cls = item.href === active ? ' class="active"' : '';
      return '<a href="' + item.href + '"' + cls + '>' + item.label + '</a>';
    }).join('');

    const adminLink = user && user.role === 'admin'
      ? '<a href="/admin"' + (active === '/admin' ? ' class="active"' : '') + '>管理后台</a>'
      : '';

    const right = user
      ? '<span class="who">你好，<b>' + esc(user.nickname || user.username) + '</b></span>' +
        '<button class="btn ghost sm" id="logout-btn">退出</button>'
      : '<a class="btn ghost sm" href="/login">登录</a>' +
        '<a class="btn sm" href="/register">注册</a>';

    mount.className = 'site-header';
    mount.innerHTML =
      '<div class="inner">' +
      '<a class="brand" href="/"><span class="dot"></span>校园失物招领</a>' +
      '<nav class="nav-links">' + links + adminLink + '</nav>' +
      '<div class="user-area">' + right + '</div>' +
      '</div>';

    const logoutBtn = document.getElementById('logout-btn');
    if (logoutBtn) {
      logoutBtn.addEventListener('click', async function () {
        try {
          await API.post('/api/auth/logout');
        } catch {
          /* 忽略登出接口异常，本地照常跳转 */
        }
        state.loading = null;
        state.user = null;
        location.href = '/';
      });
    }
  }

  /* ---------------- 交互反馈 ---------------- */

  let toastTimer = null;

  function toast(message, duration) {
    let el = document.getElementById('global-toast');
    if (!el) {
      el = document.createElement('div');
      el.id = 'global-toast';
      el.className = 'toast';
      document.body.appendChild(el);
    }
    el.textContent = message;
    el.classList.add('show');

    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () {
      el.classList.remove('show');
    }, duration || 2200);
  }

  function showAlert(el, message, kind) {
    if (!el) return;
    el.textContent = message;
    el.className = 'alert ' + (kind || 'error') + ' show';
  }

  function hideAlert(el) {
    if (!el) return;
    el.className = 'alert';
    el.textContent = '';
  }

  /* ---------------- 分页 ---------------- */

  function pageNumbers(page, totalPages) {
    const pages = [];
    const push = (n) => pages.push(n);

    if (totalPages <= 7) {
      for (let i = 1; i <= totalPages; i += 1) push(i);
      return pages;
    }

    push(1);
    if (page > 4) push('...');
    for (let i = Math.max(2, page - 1); i <= Math.min(totalPages - 1, page + 1); i += 1) push(i);
    if (page < totalPages - 3) push('...');
    push(totalPages);
    return pages;
  }

  /**
   * 渲染分页控件。
   * @param {HTMLElement} container
   * @param {{page:number,totalPages:number,total:number}} result
   * @param {(page:number)=>void} onGo
   */
  function renderPager(container, result, onGo) {
    if (!container) return;

    const { page, totalPages, total } = result;
    if (!total || totalPages <= 1) {
      container.innerHTML = total ? '<span class="info">共 ' + total + ' 条</span>' : '';
      return;
    }

    const buttons = pageNumbers(page, totalPages).map(function (n) {
      if (n === '...') return '<button disabled>…</button>';
      return '<button class="' + (n === page ? 'active' : '') + '" data-page="' + n + '">' + n + '</button>';
    }).join('');

    container.innerHTML =
      '<button data-page="' + (page - 1) + '"' + (page <= 1 ? ' disabled' : '') + '>上一页</button>' +
      buttons +
      '<button data-page="' + (page + 1) + '"' + (page >= totalPages ? ' disabled' : '') + '>下一页</button>' +
      '<span class="info">共 ' + total + ' 条 / ' + totalPages + ' 页</span>';

    container.querySelectorAll('button[data-page]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        const target = Number(btn.dataset.page);
        if (!target || target === page || target < 1 || target > totalPages) return;
        onGo(target);
      });
    });
  }

  global.App = {
    state: state,
    esc: esc,
    formatDate: formatDate,
    typeLabel: typeLabel,
    typeBadge: typeBadge,
    statusLabel: statusLabel,
    statusBadge: statusBadge,
    statusOptions: statusOptions,
    auditBadge: auditBadge,
    loadUser: loadUser,
    currentUser: currentUser,
    requireLogin: requireLogin,
    renderHeader: renderHeader,
    toast: toast,
    showAlert: showAlert,
    hideAlert: hideAlert,
    renderPager: renderPager,
  };
})(window);

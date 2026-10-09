(function () {
  'use strict';

  /**
   * 首页：关键词搜索 + 筛选 + 排序 + 分页。
   *
   * 查询条件全部通过查询串传给后端：
   *   GET /api/items?keyword=钱包&type=lost&status=open&sort=createdAt&order=desc&page=2&limit=9
   * 后端用 WHERE + LIKE 过滤、ORDER BY 排序、LIMIT + OFFSET 取当前页。
   */
  const state = {
    type: '',
    category: '',
    status: '',
    keyword: '',
    sort: 'createdAt',
    order: 'desc',
    page: 1,
    limit: 9, // 每页数量，对应接口参数 limit
  };

  const els = {
    keyword: document.getElementById('keyword'),
    category: document.getElementById('category'),
    status: document.getElementById('status'),
    sort: document.getElementById('sort'),
    limit: document.getElementById('limit'),
    searchBtn: document.getElementById('search-btn'),
    tabs: document.getElementById('type-tabs'),
    list: document.getElementById('list'),
    pager: document.getElementById('pager'),
    summary: document.getElementById('summary'),
    alert: document.getElementById('alert'),
  };

  const SORT_LABEL = {
    'createdAt:desc': '最新发布',
    'createdAt:asc': '最早发布',
    'happenedAt:desc': '发生时间（新→旧）',
    'happenedAt:asc': '发生时间（旧→新）',
    'status:asc': '按进度状态',
    'title:asc': '按标题',
  };

  function cardHtml(item) {
    // 有图片就显示缩略图；图片地址一律经 App.esc 转义后再放进 src
    const thumb = item.imageUrl
      ? '<img class="thumb" src="' + App.esc(item.imageUrl) + '" alt="' + App.esc(item.title) + '" loading="lazy">'
      : '';

    return (
      '<article class="item-card">' +
      thumb +
      '<div class="btn-row" style="gap:8px">' + App.typeBadge(item.type) + App.statusBadge(item.status) + '</div>' +
      '<h3><a href="/item?id=' + item.id + '">' + App.esc(item.title) + '</a></h3>' +
      '<p class="desc">' + App.esc(item.description || '暂无详细描述') + '</p>' +
      '<div class="meta">' +
      '<span>📍 ' + App.esc(item.location || '未填写地点') + '</span>' +
      '<span>🏷️ ' + App.esc(item.category) + '</span>' +
      '</div>' +
      '<div class="meta">' +
      '<span>🕒 ' + App.formatDate(item.createdAt) + '</span>' +
      '<span>发布者：' + App.esc(item.owner.name || item.owner.nickname || item.owner.studentId) + '</span>' +
      '</div>' +
      '</article>'
    );
  }

  /** 从下拉框取值，拆成 sort 字段与 order 方向。 */
  function readSort() {
    const [sort, order] = els.sort.value.split(':');
    state.sort = sort;
    state.order = order || 'desc';
  }

  /** 当前查询条件的可读描述，显示在列表上方。 */
  function describeQuery(total, totalPages) {
    const parts = [];
    if (state.keyword) parts.push('关键词「' + state.keyword + '」');
    if (state.type) parts.push(state.type === 'lost' ? '寻物启事' : '失物招领');
    if (state.category) parts.push(state.category);
    if (state.status) parts.push(App.statusLabel(state.status));
    parts.push(SORT_LABEL[els.sort.value] || '默认排序');

    return '共 ' + total + ' 条' + (totalPages > 1 ? '，' + totalPages + ' 页' : '') +
      '　·　' + parts.join(' · ');
  }

  async function load() {
    App.hideAlert(els.alert);
    els.list.innerHTML = '<div class="empty">加载中…</div>';
    els.pager.innerHTML = '';
    els.summary.textContent = '';

    try {
      const data = await API.get('/api/items', {
        keyword: state.keyword,
        type: state.type,
        category: state.category,
        status: state.status,
        sort: state.sort,
        order: state.order,
        page: state.page,
        limit: state.limit,
      });

      if (!data.items.length) {
        els.list.innerHTML = '<div class="empty"><div class="big">🔍</div>没有找到符合条件的信息</div>';
      } else {
        els.list.innerHTML = data.items.map(cardHtml).join('');
      }

      els.summary.textContent = describeQuery(data.total, data.totalPages);

      // 分页按钮：回调里改页码再拉一次数据
      App.renderPager(els.pager, data, function (page) {
        state.page = page;
        load();
        window.scrollTo({ top: 0, behavior: 'smooth' });
      });
    } catch (err) {
      els.list.innerHTML = '';
      App.showAlert(els.alert, err.message, 'error');
    }
  }

  /** 条件发生变化：回到第 1 页重新查询。 */
  function search() {
    state.keyword = els.keyword.value.trim();
    state.category = els.category.value;
    state.status = els.status.value;
    readSort();
    state.limit = Number(els.limit.value) || 9;
    state.page = 1;
    load();
  }

  function bind() {
    els.searchBtn.addEventListener('click', search);

    els.keyword.addEventListener('keydown', function (event) {
      if (event.key === 'Enter') search();
    });

    els.category.addEventListener('change', search);
    els.status.addEventListener('change', search);
    els.sort.addEventListener('change', search);
    els.limit.addEventListener('change', search);

    els.tabs.addEventListener('click', function (event) {
      const btn = event.target.closest('button[data-type]');
      if (!btn) return;

      els.tabs.querySelectorAll('button').forEach(function (b) {
        b.classList.toggle('active', b === btn);
      });

      state.type = btn.dataset.type;
      state.page = 1;
      load();
    });
  }

  async function initCategories() {
    try {
      const data = await API.get('/api/items/categories');
      const options = (data.categories || [])
        .map(function (name) {
          return '<option value="' + App.esc(name) + '">' + App.esc(name) + '</option>';
        })
        .join('');
      els.category.insertAdjacentHTML('beforeend', options);
    } catch {
      /* 分类加载失败不影响主流程 */
    }
  }

  (async function init() {
    bind();
    readSort();
    await App.renderHeader('/');
    await initCategories();
    await load();
  })();
})();

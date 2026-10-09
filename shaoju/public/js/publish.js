(function () {
  'use strict';

  const els = {
    form: document.getElementById('item-form'),
    title: document.getElementById('title'),
    category: document.getElementById('category'),
    happenedAt: document.getElementById('happenedAt'),
    location: document.getElementById('location'),
    storagePlace: document.getElementById('storagePlace'),
    contact: document.getElementById('contact'),
    description: document.getElementById('description'),
    imageUrl: document.getElementById('imageUrl'),
    imageFile: document.getElementById('imageFile'),
    imagePreview: document.getElementById('image-preview'),
    tabs: document.getElementById('type-tabs'),
    submitBtn: document.getElementById('submit-btn'),
    alert: document.getElementById('alert'),
    pageTitle: document.getElementById('page-title'),
    pageSubtitle: document.getElementById('page-subtitle'),
  };

  // 编辑模式：/publish?id=3
  const editId = new URLSearchParams(location.search).get('id');
  const isEdit = Boolean(editId);
  let type = 'lost';
  let user = null;

  function setType(next) {
    type = next;
    els.tabs.querySelectorAll('button[data-type]').forEach(function (btn) {
      btn.classList.toggle('active', btn.dataset.type === type);
    });
  }

  async function loadCategories() {
    try {
      const data = await API.get('/api/items/categories');
      els.category.innerHTML = (data.categories || [])
        .map(function (name) {
          return '<option value="' + App.esc(name) + '">' + App.esc(name) + '</option>';
        })
        .join('');
    } catch {
      els.category.innerHTML = '<option value="其他">其他</option>';
    }
  }

  /** ISO 字符串 -> datetime-local 需要的本地格式 */
  function toLocalInput(value) {
    if (!value) return '';
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return '';
    const pad = (n) => String(n).padStart(2, '0');
    return date.getFullYear() + '-' + pad(date.getMonth() + 1) + '-' + pad(date.getDate()) +
      'T' + pad(date.getHours()) + ':' + pad(date.getMinutes());
  }

  /** 当前本地时间，格式与 collect() 里交给后端的 happenedAt 一致：YYYY-MM-DD HH:mm */
  function localNow() {
    return toLocalInput(new Date().toISOString()).replace('T', ' ');
  }

  /**
   * 把 datetime-local 的 max 卡在当前时刻，原生选择器就不会再给出未来的日期和时间。
   * 页面可能开着过夜甚至跨天，所以每次聚焦 / 点击时都重新算一遍，而不是只在加载时设一次。
   */
  function refreshHappenedAtMax() {
    els.happenedAt.max = toLocalInput(new Date().toISOString());
  }

  async function loadItem() {
    const data = await API.get('/api/items/' + encodeURIComponent(editId));
    const item = data.item;

    if (!item.canEdit) {
      App.showAlert(els.alert, '你没有权限修改这条信息', 'error');
      els.form.style.display = 'none';
      return;
    }

    setType(item.type);
    els.title.value = item.title;
    els.category.value = item.category;
    els.happenedAt.value = toLocalInput(item.happenedAt);
    els.location.value = item.location;
    els.storagePlace.value = item.storagePlace || '';
    els.contact.value = item.contact;
    els.description.value = item.description;
    els.imageUrl.value = item.imageUrl;
    renderPreview(item.imageUrl);

    if (item.auditStatus === 'approved' && !(user && user.role === 'admin')) {
      App.showAlert(els.alert, '这条信息已通过审核，修改后将重新进入待审核状态。', 'info');
    }
  }

  function collect() {
    const happenedRaw = els.happenedAt.value;
    return {
      type: type,
      title: els.title.value.trim(),
      category: els.category.value,
      location: els.location.value.trim(),
      storagePlace: els.storagePlace.value.trim(),
      happenedAt: happenedRaw ? happenedRaw.replace('T', ' ') : '',
      contact: els.contact.value.trim(),
      description: els.description.value.trim(),
      imageUrl: els.imageUrl.value.trim(),
    };
  }

  /**
   * 渲染图片预览。
   * 选了本地文件就用 URL.createObjectURL 本地预览（还没上传，不需要等网络）；
   * 否则显示 imageUrl 里已有的地址。
   *
   * 释放 blob URL 的时机是这里的关键：要释放的是「上一次创建、现在已经不显示」的那个，
   * 绝不能释放本次正要显示的那个 —— 一旦 revoke 掉正在被 <img> 使用的 blob URL，
   * 浏览器就再也取不到这份数据，预览必然是碎图，且与文件格式完全无关。
   */
  let currentBlobUrl = null;

  function renderPreview(src) {
    if (currentBlobUrl && currentBlobUrl !== src) {
      URL.revokeObjectURL(currentBlobUrl);
      currentBlobUrl = null;
    }
    currentBlobUrl = src && src.indexOf('blob:') === 0 ? src : null;

    if (!src) {
      els.imagePreview.innerHTML = '';
      return;
    }

    els.imagePreview.innerHTML =
      '<figure><img src="' + App.esc(src) + '" alt="图片预览">' +
      '<figcaption class="muted small">图片预览</figcaption></figure>' +
      '<button type="button" class="btn ghost sm" id="clear-image">移除图片</button>';

    document.getElementById('clear-image').addEventListener('click', function () {
      els.imageFile.value = '';
      els.imageUrl.value = '';
      renderPreview('');
    });
  }

  const IMAGE_MIME = /^image\/(jpeg|png|gif|webp)$/;

  /**
   * 有些环境（部分 Windows + 浏览器组合、云盘占位文件）拿到的 file.type 是空串，
   * 只按 MIME 判断会误报「格式不支持」。这里退化成读文件头字节自己认，
   * 判定规则与后端 upload.detectImageType 保持一致。
   */
  async function sniffImageType(file) {
    const head = new Uint8Array(await file.slice(0, 12).arrayBuffer());
    if (head[0] === 0xff && head[1] === 0xd8) return 'image/jpeg';
    if (head[0] === 0x89 && head[1] === 0x50 && head[2] === 0x4e && head[3] === 0x47) return 'image/png';
    if (head[0] === 0x47 && head[1] === 0x49 && head[2] === 0x46) return 'image/gif';
    if (head[0] === 0x52 && head[1] === 0x49 && head[2] === 0x46 && head[3] === 0x46 &&
      head[8] === 0x57 && head[9] === 0x45 && head[10] === 0x42 && head[11] === 0x50) return 'image/webp';
    return '';
  }

  els.imageFile.addEventListener('change', async function () {
    const file = els.imageFile.files && els.imageFile.files[0];
    if (!file) {
      renderPreview(els.imageUrl.value.trim());
      return;
    }

    const mime = IMAGE_MIME.test(file.type)
      ? file.type
      : await sniffImageType(file).catch(function () { return ''; });

    if (!IMAGE_MIME.test(mime)) {
      App.showAlert(els.alert, '只支持 JPG / PNG / GIF / WebP 格式的图片', 'error');
      els.imageFile.value = '';
      renderPreview(els.imageUrl.value.trim());
      return;
    }
    if (file.size > 5 * 1024 * 1024) {
      App.showAlert(els.alert, '图片不能超过 5MB', 'error');
      els.imageFile.value = '';
      renderPreview(els.imageUrl.value.trim());
      return;
    }

    App.hideAlert(els.alert);
    renderPreview(URL.createObjectURL(file));
  });

  /**
   * 提交前先把本地图片传到 POST /api/uploads，拿到站内路径再写进 imageUrl。
   * 这样发布接口本身仍然只是普通的 JSON 请求，不需要处理 multipart。
   */
  async function uploadIfNeeded() {
    const file = els.imageFile.files && els.imageFile.files[0];
    if (!file) return;

    els.submitBtn.textContent = '上传图片中…';
    const data = await API.upload('/api/uploads', file);
    els.imageUrl.value = data.url;
  }

  els.tabs.addEventListener('click', function (event) {
    const btn = event.target.closest('button[data-type]');
    if (btn) setType(btn.dataset.type);
  });

  // 打开选择器前后都刷新一次上限，保证「最晚只能选到现在」
  els.happenedAt.addEventListener('focus', refreshHappenedAtMax);
  els.happenedAt.addEventListener('click', refreshHappenedAtMax);

  els.form.addEventListener('submit', async function (event) {
    event.preventDefault();
    App.hideAlert(els.alert);

    // 先做本地校验，避免标题都没填就把图片传上去留下一堆孤儿文件
    const payload = collect();
    if (!payload.title) {
      App.showAlert(els.alert, '请填写标题', 'error');
      return;
    }
    // 丢失 / 拾取时间不能是未来。max 只约束原生选择器，手打、脚本赋值都能绕开，这里再兜一道。
    // 两个时间串都是 YYYY-MM-DD HH:mm 的定长格式，直接按字典序比大小即可。
    if (payload.happenedAt && payload.happenedAt > localNow()) {
      App.showAlert(els.alert, '丢失 / 拾取时间不能晚于当前时间', 'error');
      return;
    }

    els.submitBtn.disabled = true;
    els.submitBtn.textContent = '提交中…';

    try {
      // 把选中的本地图片传到 POST /api/uploads，返回的 /uploads/xxx.png 回填到 imageUrl
      await uploadIfNeeded();
      payload.imageUrl = els.imageUrl.value.trim();

      if (isEdit) {
        await API.put('/api/items/' + encodeURIComponent(editId), payload);
        App.toast('保存成功');
      } else {
        const created = await API.post('/api/items', payload);
        App.toast('发布成功，等待管理员审核');
        setTimeout(function () {
          location.href = '/item?id=' + created.item.id;
        }, 600);
        return;
      }
      setTimeout(function () {
        location.href = '/my';
      }, 600);
    } catch (err) {
      App.showAlert(els.alert, err.message, 'error');
      els.submitBtn.disabled = false;
      els.submitBtn.textContent = isEdit ? '保存修改' : '提交';
    }
  });

  (async function init() {
    user = await App.requireLogin();
    if (!user) return;

    await App.renderHeader('/publish');
    await loadCategories();
    refreshHappenedAtMax();

    if (isEdit) {
      els.pageTitle.textContent = '修改信息';
      els.pageSubtitle.textContent = '修改后的信息需要重新经过管理员审核。';
      els.submitBtn.textContent = '保存修改';
      try {
        await loadItem();
      } catch (err) {
        App.showAlert(els.alert, err.message, 'error');
        els.form.style.display = 'none';
      }
    }
  })();
})();

(function () {
  'use strict';

  const form = document.getElementById('login-form');
  const alertBox = document.getElementById('alert');
  const submitBtn = document.getElementById('submit-btn');

  function redirectTarget() {
    const params = new URLSearchParams(location.search);
    const target = params.get('redirect');
    // 只允许站内相对路径，避免被用于跳转到外部地址
    if (target && target.startsWith('/') && !target.startsWith('//')) return target;
    return '/';
  }

  form.addEventListener('submit', async function (event) {
    event.preventDefault();
    App.hideAlert(alertBox);

    // 这一栏填的是学号。但 #username 这个 id 和请求体里的 username 键都保持不变：
    // Go 版登录接口在取不到 studentId 时会回退读 username，Node 版只认 username，
    // 所以 username 是两版都接受的交集 —— 改成 studentId 反而会让 Node 版登不上。
    const username = document.getElementById('username').value.trim();
    const password = document.getElementById('password').value;

    if (!username || !password) {
      App.showAlert(alertBox, '请填写学号和密码', 'error');
      return;
    }

    submitBtn.disabled = true;
    submitBtn.textContent = '登录中…';

    try {
      const data = await API.post('/api/auth/login', { username, password });
      App.toast('欢迎回来，' + (data.user.nickname || data.user.username));
      setTimeout(function () {
        location.href = redirectTarget();
      }, 400);
    } catch (err) {
      App.showAlert(alertBox, err.message, 'error');
      submitBtn.disabled = false;
      submitBtn.textContent = '登录';
    }
  });

  (async function init() {
    App.renderHeader(null);
    // 已登录用户直接跳走，避免重复登录
    const user = await App.loadUser();
    if (user) location.replace(redirectTarget());
  })();
})();

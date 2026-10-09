(function () {
  'use strict';

  const form = document.getElementById('register-form');
  const alertBox = document.getElementById('alert');
  const submitBtn = document.getElementById('submit-btn');

  form.addEventListener('submit', async function (event) {
    event.preventDefault();
    App.hideAlert(alertBox);

    const studentId = document.getElementById('studentId').value.trim();
    const password = document.getElementById('password').value;
    const confirm = document.getElementById('confirm').value;
    const name = document.getElementById('name').value.trim();
    const contact = document.getElementById('contact').value.trim();

    if (!studentId || !password) {
      App.showAlert(alertBox, '请填写学号和密码', 'error');
      return;
    }
    // 学号规则与 Go 版 validate.StudentID 逐字对齐：恰好 8 位数字。
    // 前端先拦一道只是为了少一次往返，后端仍然是唯一的权威校验。
    if (!/^\d{8}$/.test(studentId)) {
      App.showAlert(alertBox, '学号必须是 8 位数字', 'error');
      return;
    }
    if (password.length < 6) {
      App.showAlert(alertBox, '密码至少需要 6 位', 'error');
      return;
    }
    if (password !== confirm) {
      App.showAlert(alertBox, '两次输入的密码不一致', 'error');
      return;
    }

    submitBtn.disabled = true;
    submitBtn.textContent = '提交中…';

    try {
      // 学号 / 姓名是 Go 版后端的字段名，username / nickname 是 Node 版的字段名。
      // 两套都发出去，同一份前端就能同时跑在两版后端上：Go 读 studentId / name，
      // Node 读 username / nickname，多余的键两边都会忽略。
      await API.post('/api/auth/register', {
        studentId,
        username: studentId,
        password,
        name,
        nickname: name,
        contact,
      });
      App.toast('注册成功，正在进入首页');
      setTimeout(function () {
        location.href = '/';
      }, 500);
    } catch (err) {
      App.showAlert(alertBox, err.message, 'error');
      submitBtn.disabled = false;
      submitBtn.textContent = '注册并登录';
    }
  });

  (async function init() {
    App.renderHeader(null);
    const user = await App.loadUser();
    if (user) location.replace('/');
  })();
})();

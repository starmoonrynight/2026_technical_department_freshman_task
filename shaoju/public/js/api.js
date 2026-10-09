/**
 * 统一的接口调用封装。
 * 约定：服务端成功返回 { data: ... }，失败返回 { message: ... }。
 */
(function (global) {
  'use strict';

  async function request(path, options) {
    const opts = options || {};
    const init = {
      method: opts.method || 'GET',
      credentials: 'same-origin',
      headers: {},
    };

    if (opts.form !== undefined) {
      // FormData：绝对不要手写 Content-Type，浏览器要自己带上 multipart 的 boundary
      init.body = opts.form;
    } else if (opts.body !== undefined) {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(opts.body);
    }

    let response;
    try {
      response = await fetch(path, init);
    } catch (networkError) {
      throw new Error('无法连接服务器，请确认服务已启动');
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
      const error = new Error((payload && payload.message) || '请求失败（HTTP ' + response.status + '）');
      error.status = response.status;
      throw error;
    }

    return payload ? payload.data : null;
  }

  global.API = {
    request,
    get: (path, query) => request(path + buildQuery(query)),
    post: (path, body) => request(path, { method: 'POST', body: body || {} }),
    put: (path, body) => request(path, { method: 'PUT', body: body || {} }),
    patch: (path, body) => request(path, { method: 'PATCH', body: body || {} }),
    del: (path) => request(path, { method: 'DELETE' }),

    /** 上传单个文件到 path，返回 { url, filename, size, mimeType }。 */
    upload: (path, file, field) => {
      const form = new FormData();
      form.append(field || 'file', file);
      return request(path, { method: 'POST', form });
    },
  };

  /** 把对象拼成查询串，自动忽略空值。 */
  function buildQuery(query) {
    if (!query) return '';

    const params = new URLSearchParams();
    for (const [key, value] of Object.entries(query)) {
      if (value === undefined || value === null || value === '') continue;
      params.append(key, value);
    }

    const str = params.toString();
    return str ? '?' + str : '';
  }

  global.buildQuery = buildQuery;
})(window);

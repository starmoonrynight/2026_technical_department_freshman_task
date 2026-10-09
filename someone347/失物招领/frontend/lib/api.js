import { state } from "./state.js";
export async function api(route, options = {}) {
  let response;
  try {
    response = await fetch(route, {
      credentials: "same-origin",
      ...options,
      headers: {
        "Content-Type": "application/json",
        ...(state.csrf ? { "X-CSRF-Token": state.csrf } : {}),
        ...options.headers,
      },
    });
  } catch {
    throw Object.assign(new Error("网络连接失败，请检查连接后重试"), {
      status: 0,
    });
  }
  let result;
  try {
    result = await response.json();
  } catch {
    throw new Error("服务响应异常，请重试");
  }
  if (!response.ok) {
    if (
      response.status === 401 &&
      state.user &&
      !route.startsWith("/api/auth/login")
    )
      window.dispatchEvent(new Event("session-expired"));
    throw Object.assign(new Error(result.message || "操作失败"), {
      status: response.status,
      fields: result.field_errors,
    });
  }
  return result.data;
}

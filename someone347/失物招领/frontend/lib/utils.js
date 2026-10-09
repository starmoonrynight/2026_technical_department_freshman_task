export const esc = (v) =>
  String(v ?? "").replace(
    /[&<>"']/g,
    (c) =>
      ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[
        c
      ],
  );
export const labels = {
  CARD: "校园卡 / 证件",
  DIGITAL: "数码产品",
  KEY: "钥匙",
  CLOTHING: "衣物",
  BOOK: "书籍",
  OTHER: "其他",
};
export const localToday = () =>
  new Date().toLocaleDateString("en-CA", { timeZone: "Asia/Shanghai" });
export const date = (v, withTime = false) =>
  v
    ? new Intl.DateTimeFormat("zh-CN", {
        timeZone: "Asia/Shanghai",
        month: "numeric",
        day: "numeric",
        ...(withTime ? { hour: "2-digit", minute: "2-digit" } : {}),
      }).format(new Date(v.length === 10 ? `${v}T12:00:00+08:00` : v))
    : "";
export const token = () => crypto.randomUUID();

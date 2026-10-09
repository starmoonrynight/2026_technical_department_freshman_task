import { fail, invariant } from "./errors.mjs";

export const CATEGORIES = [
  "CARD",
  "DIGITAL",
  "KEY",
  "CLOTHING",
  "BOOK",
  "OTHER",
];
export const TYPES = ["FOUND", "LOST"];
export const STATUSES = ["OPEN", "RESOLVED", "CLOSED"];
export const today = () =>
  new Date().toLocaleDateString("en-CA", { timeZone: "Asia/Shanghai" });
export const now = () => new Date().toISOString();
export function validDate(value) {
  if (typeof value !== "string" || !/^\d{4}-\d{2}-\d{2}$/.test(value))
    return false;
  const d = new Date(`${value}T00:00:00Z`);
  return Number.isFinite(d.getTime()) && d.toISOString().slice(0, 10) === value;
}
export function integer(value, name, fallback, max = Number.MAX_SAFE_INTEGER) {
  if (value === undefined || value === null || value === "") return fallback;
  const n = Number(value);
  invariant(
    Number.isSafeInteger(n) && n >= 1 && n <= max,
    400,
    `${name}必须是1—${max}之间的整数`,
  );
  return n;
}
export function pagination(query) {
  return {
    page: integer(query.get("page"), "page", 1, 100000),
    pageSize: integer(query.get("page_size"), "page_size", 20, 50),
  };
}
export function listResult(items, total, { page, pageSize }) {
  return {
    items,
    total,
    page,
    page_size: pageSize,
    has_more: page * pageSize < total,
  };
}
export function text(value, name, min, max) {
  invariant(typeof value === "string", 400, `${name}必须是字符串`, {
    [name]: "请填写文本",
  });
  const result = value.trim();
  invariant(
    result.length >= min && result.length <= max,
    400,
    `${name}需为${min}—${max}字`,
    { [name]: `请填写${min}—${max}字` },
  );
  return result;
}
export function validatePost(body, actor, db, previous) {
  const b = previous ? { ...previous, ...body } : body;
  const errors = {};
  const result = {};
  for (const [key, min, max] of [
    ["title", 2, 30],
    ["location", 2, 50],
    ["description", 5, 300],
    ["event_period", 0, 30],
    ["storage_location", 0, 100],
  ]) {
    const value = b[key] ?? "";
    if (
      typeof value !== "string" ||
      value.trim().length < min ||
      value.trim().length > max
    )
      errors[key] = `请填写${min}—${max}字`;
    result[key] = typeof value === "string" ? value.trim() : "";
  }
  result.type = b.type;
  if (!TYPES.includes(b.type)) errors.type = "请选择寻物或招领";
  if (previous && previous.type !== b.type) errors.type = "发布后不能修改类型";
  result.category = b.category;
  if (!CATEGORIES.includes(b.category)) errors.category = "请选择有效类别";
  result.campus_id = b.campus_id;
  if (
    typeof b.campus_id !== "string" ||
    !db.prepare("SELECT id FROM campuses WHERE id=?").get(b.campus_id)
  )
    errors.campus_id = "请选择有效校区";
  result.event_date = b.event_date;
  if (!validDate(b.event_date) || b.event_date > today())
    errors.event_date = "日期无效或晚于今天";
  result.image_ids = b.image_ids ?? [];
  if (
    !Array.isArray(result.image_ids) ||
    result.image_ids.length > 3 ||
    new Set(result.image_ids).size !== result.image_ids.length ||
    result.image_ids.some(
      (id) =>
        typeof id !== "string" ||
        !db
          .prepare("SELECT id FROM images WHERE id=? AND owner_id=?")
          .get(id, actor.id),
    )
  )
    errors.image_ids = "最多选择3张本人上传的图片";
  if (result.type === "LOST" && result.storage_location)
    errors.storage_location = "寻物信息不能填写保管地点";
  if (Object.keys(errors).length) fail(400, "请检查填写内容", errors);
  return result;
}

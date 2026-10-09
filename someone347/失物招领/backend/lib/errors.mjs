export class HttpError extends Error {
  constructor(status, message, fields) {
    super(message);
    this.status = status;
    this.field_errors = fields;
  }
}
export function fail(status, message, fields) {
  throw new HttpError(status, message, fields);
}
export function invariant(condition, status, message, fields) {
  if (!condition) fail(status, message, fields);
}

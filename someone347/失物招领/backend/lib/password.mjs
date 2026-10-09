import {
  randomBytes,
  scrypt as scryptCallback,
  timingSafeEqual,
  createHash,
} from "node:crypto";
import { promisify } from "node:util";
const scrypt = promisify(scryptCallback);
export async function hashPassword(password) {
  const salt = randomBytes(16).toString("hex");
  return `${salt}:${(await scrypt(password, salt, 64)).toString("hex")}`;
}
export async function verifyPassword(password, encoded) {
  const [salt, expected] = encoded.split(":");
  const actual = await scrypt(password, salt, 64);
  const stored = Buffer.from(expected, "hex");
  return actual.length === stored.length && timingSafeEqual(actual, stored);
}
export const digest = (value) =>
  createHash("sha256").update(value).digest("hex");
export const randomToken = () => randomBytes(32).toString("hex");

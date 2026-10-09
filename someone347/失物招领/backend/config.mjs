import path from "node:path";
import { fileURLToPath } from "node:url";

export const ROOT = path.dirname(path.dirname(fileURLToPath(import.meta.url)));
export function configuration(overrides = {}) {
  const production = process.env.NODE_ENV === "production";
  return {
    root: ROOT,
    dataDir: path.resolve(process.env.DATA_DIR || path.join(ROOT, "data")),
    frontendDir: path.join(ROOT, "frontend"),
    host: process.env.HOST || "127.0.0.1",
    port: Number(process.env.PORT ?? 4173),
    publicOrigin: process.env.PUBLIC_ORIGIN || "",
    secureCookie: production || process.env.SECURE_COOKIE === "true",
    seedDemo: process.env.SEED_DEMO !== "false" && !production,
    sessionTtl: 24 * 60 * 60 * 1000,
    maxBodyBytes: 15 * 1024 * 1024,
    loginLimit: Number(process.env.LOGIN_LIMIT || 15),
    ...overrides,
  };
}

import fs from "node:fs";
import { fileURLToPath } from "node:url";
import { configuration } from "./backend/config.mjs";
import { createApp } from "./backend/app.mjs";
const envFile = fileURLToPath(new URL("./.env", import.meta.url));
if (fs.existsSync(envFile)) process.loadEnvFile(envFile);
const config = configuration();
const { server } = createApp(config);
server.listen(config.port, config.host, () =>
  console.log(`杭电失物招领：http://${config.host}:${server.address().port}`),
);
for (const signal of ["SIGINT", "SIGTERM"])
  process.once(signal, () => {
    server.close(() => process.exit(0));
    setTimeout(() => process.exit(1), 5000).unref();
  });

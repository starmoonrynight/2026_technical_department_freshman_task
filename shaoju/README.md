# 校园失物招领系统

一个可直接运行的失物招领平台示例项目。前端为原生 **HTML / CSS / JavaScript**（无构建步骤），后端提供**两套等价实现**，共用同一个 SQLite 数据库与同一套接口契约：

| 后端 | 技术栈 | 目录 | 默认端口 |
| --- | --- | --- | --- |
| Node.js | Express 4 + `node:sqlite` | `server.js`、`src/` | 3000 |
| Go | Gin + `modernc.org/sqlite`（纯 Go，无 cgo） | `go-backend/` | 3001 |

两版的口令散列格式、会话 Cookie、响应结构完全一致，可以指向同一个 `.db` 文件交替或同时运行，登录态与数据互通。Go 版细节见 [`go-backend/README.md`](go-backend/README.md)。

## 功能

| 模块 | 说明 |
| --- | --- |
| 用户注册 / 登录 | 以**学号**为登录凭据，学号唯一；注册后自动登录；会话保存在数据库中，通过 HttpOnly Cookie 携带 |
| 鉴权中间件 | `RequireAuth` / `RequireRole` / `RequireAdmin`，统一守卫需要登录或管理员权限的接口 |
| 失物发布 | 发布「寻物启事」（丢了东西）或「失物招领」（捡到东西），可附带一张图片，还可填写**寄放处**（捡到的东西现在存放在哪里，选填） |
| 图片上传 | 上传的图片存在服务器本地 `public/uploads/`，列表与详情页直接展示；只接受 JPG / PNG / GIF / WebP，单张 ≤ 5MB |
| 信息查找 | 按关键字搜索（`LIKE` 匹配标题/描述/地点/分类）、按分类、类型、状态筛选，支持排序与分页 |
| 我的发布 | 查看自己发布的全部信息，含待审核与已驳回；可编辑、删除、切换进度状态 |
| 状态管理 | 发布者本人（或管理员）可把信息标记为「寻找中 / 已找到 / 已结束」，列表与详情页按状态显示不同颜色的标签 |
| 管理员后台 | 数据概览、信息审核（通过 / 驳回并填写原因）、信息管理、用户管理（启用停用、角色调整） |

信息审核流程：用户发布 → `pending` 待审核 → 管理员操作 → `approved` 公开展示 或 `rejected` 驳回（需填写原因）。普通用户修改已通过的信息后会重新进入待审核状态。

## 环境要求

- **运行 Node 版**：Node.js ≥ 22.5.0（需要内置的 `node:sqlite` 模块；本项目在 Node 24 上开发测试），除 Express 外无其它运行时依赖
- **运行 Go 版**：无需任何额外运行时，`go-backend/bin/` 中的可执行文件可直接运行；从源码构建需要 Go ≥ 1.26

```bash
node -v   # 确认版本 >= 22.5.0
```

## 快速开始

### Node.js 版

```bash
# 1. 安装依赖
npm install

# 2. 写入演示数据（可选，会自动创建管理员账号）
npm run seed

# 3. 启动服务
npm start
```

启动后访问：

- 前台首页：<http://127.0.0.1:3000/>
- 管理后台：<http://127.0.0.1:3000/admin>

开发时可用 `npm run dev`（`node --watch`，改动文件后自动重启）。

### Go 版

两种后端共用 `data/lostfound.db`，因此演示数据只需写入一次（任选一条命令）。

```powershell
# 直接用已编译好的可执行文件
cd go-backend
.\bin\seed.exe         # 写入演示数据（与 npm run seed 等价）
.\bin\lostfound.exe    # 默认监听 3001
```

```bash
# 或从源码构建
cd go-backend
go build -o bin/lostfound.exe .
go build -o bin/seed.exe ./cmd/seed
```

启动后访问 <http://127.0.0.1:3001/>，管理后台 <http://127.0.0.1:3001/admin>。
本机没有 Go 时，可用 `powershell -ExecutionPolicy Bypass -File tools/setup-go.ps1` 在工作区内临时安装一套（详见 `go-backend/README.md`）。

### 默认账号

| 角色 | 学号 | 密码 | 姓名 |
| --- | --- | --- | --- |
| 管理员 | `10000000` | `admin123` | 系统管理员 |
| 普通用户 | `20230001` / `20230002` / `20230003` | `123456` | 张三 / 李四 / 王五 |

> `10000000` 账号在首次启动时自动创建（Go 版可用 `ADMIN_STUDENT_ID` / `ADMIN_PASSWORD` / `ADMIN_NAME` 覆盖，Node 版对应 `ADMIN_USER` / `ADMIN_PASSWORD` / `ADMIN_NICKNAME`）。**请在正式使用前修改默认口令。**
>
> 学号规则为**恰好 8 位数字**，演示账号也遵守这条规则。
> 老库里遗留的 `admin` / `zhangsan` 这类学号，Go 版启动时会自动改写成上表的新学号（迁移 v5），
> 只改 `student_id` 与兼容列 `username`，`users.id` 不动，所以这些账号发布的条目与会话都不受影响。

### 常用命令

| 命令 | 作用 |
| --- | --- |
| `npm start` | 启动服务 |
| `npm run dev` | 启动服务并在文件变动时自动重启 |
| `npm run seed` | 写入演示用户与信息（可重复执行，不会重复插入） |
| `npm run reset` | 删除数据库文件，恢复到全新状态 |

## 目录结构

```
.
├── server.js                 # 服务入口：启动监听、引导初始化、优雅退出
├── package.json
├── src/
│   ├── app.js                # Express 装配：中间件、路由、静态资源、错误处理
│   ├── config.js             # 集中配置（端口、数据库路径、分类、会话时长等）
│   ├── db.js                 # SQLite 连接、建表、事务封装
│   ├── bootstrap.js          # 启动引导：确保管理员账号、清理过期会话
│   ├── middleware/
│   │   └── auth.js           # Cookie 解析、登录态识别、requireAuth / requireAdmin
│   ├── routes/
│   │   ├── auth.js           # /api/auth   注册、登录、登出、资料、改密
│   │   ├── items.js          # /api/items  列表、详情、发布、修改、删除
│   │   └── admin.js          # /api/admin  统计、审核、信息与用户管理
│   ├── services/
│   │   ├── users.js          # 用户业务逻辑
│   │   ├── sessions.js       # 会话管理
│   │   └── items.js          # 失物/招领业务逻辑
│   └── utils/
│       ├── http.js           # ApiError 与统一响应
│       ├── password.js       # scrypt 口令散列与校验
│       └── validate.js       # 请求参数校验
├── scripts/
│   ├── seed.js               # 演示数据
│   └── reset.js              # 清空数据库
├── public/                   # 前端静态资源
│   ├── index.html            # 首页：搜索与列表
│   ├── login.html            # 登录
│   ├── register.html         # 注册
│   ├── publish.html          # 发布 / 编辑信息
│   ├── item.html             # 信息详情
│   ├── my.html               # 我的发布
│   ├── admin.html            # 管理后台
│   ├── crud-demo.html        # 失物/招领 CRUD 接口示例页
│   ├── uploads/              # 用户上传的图片（内容已加入 .gitignore）
│   ├── css/style.css
│   └── js/                   # api.js、common.js、crud-demo.js 及各页面脚本
├── go-backend/               # Go + Gin 后端（详见其 README）
│   ├── main.go
│   ├── cmd/seed/             # 演示数据
│   ├── internal/             # config / database / store / web / validate / secure / httpx
│   │   └── database/
│   │       ├── schema.sql    # 数据库初始化 SQL
│   │       └── migrate.go    # 版本化迁移（老库自动补列回填）
│   └── bin/                  # 构建产物（已加入 .gitignore）
├── tools/                    # 验证脚本与 Go 工具链安装脚本
│   ├── user-module-test.cjs  # 用户模块接口测试（59 项）
│   ├── item-crud-test.cjs    # 失物/招领 CRUD 与权限矩阵测试（60 项）
│   ├── item-search-test.cjs  # 搜索 / 排序 / 分页测试（46 项）
│   ├── item-status-test.cjs  # 三态状态管理测试（36 项）
│   ├── upload-test.cjs       # 图片上传测试（35 项）
│   ├── make-legacy-db.cjs    # 造一个升级前的旧结构库，用于验证迁移
│   ├── check-migration.cjs   # 校验迁移后 CHECK / 数据 / 索引 / 外键是否完好
│   ├── db-inspect.cjs        # 查看表结构、索引与迁移版本
│   ├── contract-parity.cjs   # 两版契约对比（91 项）
│   ├── cross-compat.cjs      # 两版共库的交叉兼容验证
│   └── setup-go.ps1          # 本机没有 Go 时临时装一套
└── data/                     # SQLite 数据库文件（已加入 .gitignore）
```

## API 一览

所有接口均返回 JSON。成功为 `{ "data": ... }`，失败为对应 HTTP 状态码 + `{ "message": "..." }`。

### 认证 `/api/auth`

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| POST | `/register` | 公开 | 注册并登录，请求体 `{ studentId, password, name, contact }`，学号唯一 |
| POST | `/login` | 公开 | 登录，请求体 `{ studentId, password }`，成功签发会话 Cookie |
| POST | `/logout` | 公开 | 登出，销毁服务端会话 |
| GET | `/me` | 公开 | 当前登录用户（未登录返回 `{"user": null}`） |
| PUT | `/profile` | 登录 | 修改姓名、联系方式 |
| PUT | `/password` | 登录 | 修改口令，其它会话失效 |

登录态用**数据库会话 + HttpOnly Cookie**（`laf_sid`）承载，鉴权中间件为 `RequireAuth` / `RequireRole` / `RequireAdmin`。
选择 Session 而非 JWT 的理由见 [`go-backend/README.md` 第 4.3 节](go-backend/README.md)。

> Go 版以**学号**（`student_id`）作为登录凭据，同时接受旧字段名 `username` / `nickname`；
> Node 版仍使用 `username` / `nickname`。两版共用同一个数据库时的兼容方案同样见该 README。

### 失物信息 `/api/items`

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| GET | `/` | 公开 | 列表，仅返回审核通过的信息 |
| GET | `/categories` | 公开 | 分类字典 |
| GET | `/:id` | 公开 | 详情；未通过审核的信息仅本人与管理员可见 |
| POST | `/` | 登录 | 发布信息 |
| GET | `/mine` | 登录 | 我的发布（含待审核、已驳回） |
| PUT | `/:id` | 本人/管理员 | 修改信息（整体替换，缺省字段会被清空） |
| PATCH | `/:id` | 本人/管理员 | 修改信息（局部更新，只改请求体里出现的字段） |
| POST | `/:id/status` | 本人/管理员 | 改进度状态：`{ status: "open" \| "found" \| "closed" }` |
| DELETE | `/:id` | 本人/管理员 | 删除信息 |

`status` 三态：`open` = 寻找中（红标签）、`found` = 已找到（绿标签）、`closed` = 已结束（灰标签）。
新建默认 `open`；后端只校验取值合法、不限制流转方向，发布者可以随时纠正（例如把「已找到」改回「寻找中」）。
详见 [`go-backend/README.md` 第 6.4 节](go-backend/README.md)。

列表 / 搜索支持的查询参数：

| 参数 | 取值 | 缺省 | 说明 |
| --- | --- | --- | --- |
| `keyword` | ≤ 60 字 | — | 对标题、描述、地点、分类做 `LIKE '%…%'` 模糊匹配 |
| `type` | `lost` / `found` | — | 信息类型 |
| `category` | 分类名 | — | 物品分类 |
| `status` | `open` / `found` / `closed` | — | 进度状态：寻找中 / 已找到 / 已结束 |
| `sort` | `createdAt` / `updatedAt` / `happenedAt` / `title` / `category` / `status` / `type` / `id` | `createdAt` | 排序字段（白名单，非法值 400） |
| `order` | `asc` / `desc` | `desc` | 排序方向 |
| `page` | ≥ 1 | `1` | 页码 |
| `limit` | 1 ~ 50 | `10` | 每页数量（`pageSize` 为历史别名） |

`GET /api/items/search` 与 `GET /api/items` 完全等价。SQL 构建逻辑与注入防护说明见
[`go-backend/README.md` 第 6.3 节](go-backend/README.md)。

「本人」由 Go 版 `RequireItemOwner` 中间件用会话中的用户 ID 与 `items.user_id` 比对后放行，
非本人返回 `403 {"message":"只能修改自己发布的信息"}`，未登录返回 `401`。
逐条说明与前端调用示例见 [`go-backend/README.md` 第六节](go-backend/README.md)；
可直接打开的示例页是 <http://127.0.0.1:3001/crud-demo>（源码 `public/crud-demo.html` + `public/js/crud-demo.js`）。

### 图片上传 `/api/uploads`

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| POST | `/` | 登录 | `multipart/form-data`，表单字段名固定为 `file`；返回 `{ url, filename, size, mimeType }` |

返回的 `url` 形如 `/uploads/1759824000-3f9a2c1b8d4e5f60.png`，直接写进创建 / 修改接口的 `imageUrl` 字段即可。
`imageUrl` 只接受 `/uploads/...` 站内路径或 `http(s)` 链接。详见 [`go-backend/README.md` 第七节](go-backend/README.md)。

### 管理后台 `/api/admin`（均需管理员）

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/stats` | 数据概览 |
| GET | `/items` | 全部信息，可按 `auditStatus`、`type`、`status`、`keyword` 筛选 |
| POST | `/items/:id/audit` | 审核，`{ action: "approve" \| "reject", remark }` |
| DELETE | `/items/:id` | 删除任意信息 |
| GET | `/users` | 用户列表 |
| POST | `/users/:id/status` | 启用 / 停用账号 |
| POST | `/users/:id/role` | 调整角色 |

另有 `GET /api/health` 用于健康检查。

## 数据库

SQLite 文件默认位于 `data/lostfound.db`（可用环境变量 `DB_FILE` 修改），启用 WAL 模式与外键约束。

- **users** — 学号（`student_id`，唯一）、口令散列、姓名、联系方式、角色（`user` / `admin`）、账号状态
- **items** — 失物/招领信息，含类型、分类、地点、**寄放处**、时间、联系方式、审核状态与进度状态；`user_id` 外键指向 `users(id)`，`ON DELETE CASCADE`
- **sessions** — 登录会话，带过期时间，服务启动与每小时自动清理
- **schema_migrations** — 迁移版本记录（Go 版）

Go 版的初始化 SQL 在 [`go-backend/internal/database/schema.sql`](go-backend/internal/database/schema.sql)，
启动时自动执行并把历史库升级到最新结构（补 `student_id` / `name` 列并按旧的 `username` / `nickname` 回填，补 `items.storage_place` 寄放处列）。
用 `node tools/db-inspect.cjs` 可以查看当前库的实际结构、索引与已应用的迁移。

## 配置项

均可通过环境变量覆盖：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PORT` | `3000`（Node）/ `3001`（Go） | 监听端口 |
| `HOST` | `127.0.0.1` | 监听地址 |
| `DB_FILE` | `data/lostfound.db` | 数据库文件路径，两版默认指向同一个文件 |
| `SESSION_TTL_MS` | `604800000`（7 天） | 会话有效期 |
| `UPLOAD_DIR` | `public/uploads` | 图片落盘目录 |
| `MAX_UPLOAD_MB` | `5` | 单张图片大小上限（MB） |
| `ADMIN_STUDENT_ID` / `ADMIN_USER` | `10000000` | 初始管理员学号（Go 版 / Node 版变量名） |
| `ADMIN_PASSWORD` | `admin123` | 初始管理员口令 |
| `ADMIN_NAME` / `ADMIN_NICKNAME` | `系统管理员` | 初始管理员姓名（Go 版 / Node 版变量名） |

## 安全说明

- 口令使用 **scrypt** 加盐散列存储（Node 版用内置 `crypto`，Go 版用 `golang.org/x/crypto/scrypt`，格式一致），校验时使用恒定时间比较，数据库中不保存明文。
- 会话 ID 为 32 字节随机数，Cookie 设置为 `HttpOnly` + `SameSite=Lax`。
- 所有 SQL 均使用参数化预编译语句，避免 SQL 注入。
- 前端所有动态内容经 `App.esc()` 转义后插入，避免 XSS。
- 权限在服务端校验：普通用户只能操作自己发布的信息，管理接口统一由 `requireAdmin` 守卫；停用账号会同时销毁其全部会话，且不允许停用或降级最后一个可用管理员。

## 后续可以扩展的方向

- 回收未被引用的上传图片（加一张 `uploads` 表记录 `filename / user_id / item_id`，删除信息时一并清理，或用定时任务扫孤儿文件）
- 站内私信 / 认领申请流程
- 邮件或短信通知、找回密码
- 部署为 HTTPS 服务并加上 `Secure` Cookie、`helmet`、速率限制
- 更换为 MySQL / PostgreSQL（`src/services/` 已把 SQL 访问集中隔离，便于替换）

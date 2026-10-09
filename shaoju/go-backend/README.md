# 失物招领系统 —— Go + Gin + SQLite 后端

Node.js 版本的等价实现，**共用同一套 HTTP 契约、同一个 SQLite 文件、同一种口令散列格式**。
两版可以任选其一运行，也可以同时运行（SQLite WAL 模式支持多进程读写），数据与登录态完全互通。

- 路由器：**Gin**
- 数据库：**SQLite**，驱动为 `modernc.org/sqlite`（纯 Go，无需 cgo，也无需安装 gcc）
- 口令散列：`golang.org/x/crypto/scrypt`，存储格式与 Node 版逐字节一致

---

## 一、运行

### 方式 A：直接用已编译好的可执行文件

仓库中的 `bin/` 目录已经放好了编译产物（若被 `.gitignore` 排除，请自行构建）：

```powershell
cd go-backend
.\bin\lostfound.exe            # 默认 http://127.0.0.1:3001
```

写入演示数据（与 Node 版 `npm run seed` 等价，操作同一个数据库，可重复执行）：

```powershell
.\bin\seed.exe
```

### 方式 B：从源码构建

需要 Go ≥ 1.26（`go.mod` 中声明的版本）。

```powershell
cd go-backend
go mod download
go build -o bin\lostfound.exe .
go build -o bin\seed.exe .\cmd\seed
```

若本机没有 Go，可用仓库自带的脚本在工作区内临时装一套（约 1GB，用完可直接删除）：

```powershell
powershell -ExecutionPolicy Bypass -File tools\setup-go.ps1
. .gotmp\env.ps1
cd go-backend
go build -o bin\lostfound.exe .
```

> Windows 沙箱下 `curl` / `Invoke-WebRequest` 会因 schannel 无凭据而失败，
> 因此 `tools/setup-go.ps1` 改用 Node 自带的 OpenSSL 下载（`tools/download-go.cjs`）。

### 方式 C：安装到系统路径

```powershell
go install lostfound@latest
```

---

## 二、环境变量

与 Node 版变量名完全一致，可以共用同一份 `.env` 或启动脚本。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `PORT` | `3001` | 监听端口（Node 版默认 3000，两版可同时运行） |
| `HOST` | `127.0.0.1` | 监听地址 |
| `DB_FILE` | `data/lostfound.db` | 数据库文件路径，**默认与 Node 版同一个文件** |
| `SESSION_TTL_MS` | `604800000` | 会话有效期 |
| `ADMIN_STUDENT_ID` | `10000000` | 初始管理员学号（兼容旧名 `ADMIN_USER`） |
| `ADMIN_PASSWORD` | `admin123` | 初始管理员口令 |
| `ADMIN_NAME` | `系统管理员` | 初始管理员姓名（兼容旧名 `ADMIN_NICKNAME`） |
| `UPLOAD_DIR` | `<PUBLIC_DIR>/uploads` | 图片落盘目录 |
| `MAX_UPLOAD_MB` | `5` | 单张图片大小上限（MB） |
| `ROOT_DIR` | 自动探测 | 仓库根目录（用于定位 `public/`） |
| `PUBLIC_DIR` | `<ROOT_DIR>/public` | 前端静态资源目录 |

`ROOT_DIR` 的自动探测方式：从当前工作目录逐级向上，找到第一个含 `public/index.html` 的目录。
因此无论从仓库根目录还是从 `go-backend/` 下启动，都能正确找到前端资源。

---

## 三、目录结构

```
go-backend/
├── main.go                       # 入口：配置、建库、引导、监听、优雅退出
├── go.mod / go.sum
├── cmd/
│   └── seed/main.go              # 演示数据（等价于 scripts/seed.js）
└── internal/
    ├── config/config.go          # 配置与路径探测
    ├── database/
    │   ├── database.go           # SQLite 连接、初始化 SQL、事务、统一时间格式
    │   ├── schema.sql            # ★ 数据库初始化 SQL（go:embed 嵌入）
    │   └── migrate.go            # ★ 版本化迁移（老库自动补列并回填）
    ├── httpx/httpx.go            # ApiError 与 { data } / { message } 响应封装
    ├── validate/validate.go      # 参数校验（学号 / 姓名 / 口令 / 枚举 / 分页）
    ├── secure/password.go        # scrypt 口令散列
    ├── models/models.go          # 返回给前端的结构体
    ├── upload/upload.go          # ★ 图片落盘：文件头嗅探、随机文件名、大小限制
    ├── store/                    # 数据访问与业务逻辑（对应 src/services）
    │   ├── store.go
    │   ├── users.go              # ★ 用户：按学号读写、角色与状态
    │   ├── sessions.go           # ★ 会话：签发 / 校验 / 撤销
    │   ├── item_query.go         # ★ 搜索的 SQL 构建：WHERE + LIKE、ORDER BY、LIMIT/OFFSET
    │   └── items.go
    ├── bootstrap/bootstrap.go    # 默认管理员、历史数据回填、过期会话清理
    └── web/                      # HTTP 层（对应 src/routes + src/middleware + src/app.js）
        ├── router.go
        ├── middleware.go         # ★ 鉴权中间件 AttachUser / RequireAuth / RequireRole / RequireAdmin
        ├── body.go               # 复刻 express.json / express.urlencoded 的行为
        ├── static.go             # 复刻 express.static 的 extensions:['html']
        ├── auth_routes.go        # ★ 注册 / 登录 / 当前用户 / 登出 / 资料 / 改密
        ├── item_routes.go
        ├── upload_routes.go      # ★ POST /api/uploads 图片上传
        └── admin_routes.go
```

分层与 Node 版一致：`web` 只做参数校验与响应，业务规则集中在 `store`，通用能力放在 `httpx` / `validate` / `secure`。

---

## 四、用户模块与鉴权

### 4.1 数据表

初始化 SQL 在 [`internal/database/schema.sql`](internal/database/schema.sql)，由 `go:embed` 嵌入，服务每次启动都会执行（全部是 `CREATE ... IF NOT EXISTS`，可重复运行）。

**users**

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `id` | INTEGER PK AUTOINCREMENT | 主键，`items.user_id` 指向它 |
| `student_id` | TEXT NOT NULL DEFAULT '' | **学号**，登录凭据，部分唯一索引 `idx_users_student_id` 保证唯一 |
| `password_hash` | TEXT NOT NULL | `scrypt$N$r$p$salt$hash` |
| `name` | TEXT NOT NULL DEFAULT '' | **姓名** |
| `contact` | TEXT NOT NULL DEFAULT '' | 联系方式 |
| `role` | TEXT NOT NULL DEFAULT 'user' | `user` / `admin`，CHECK 约束 |
| `status` | TEXT NOT NULL DEFAULT 'active' | `active` / `disabled`，CHECK 约束 |
| `created_at` | TEXT NOT NULL | ISO 8601 |
| `username` / `nickname` | TEXT | **兼容列**，见 4.5 |

**items** 通过 `user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE` 关联发布者，并建有 `idx_items_user` 索引，`ON DELETE CASCADE` 表示删除用户时其发布的信息一并删除。

`items` 的字段：

| 列 | 类型 | 说明 |
| --- | --- | --- |
| `type` | TEXT NOT NULL | `lost` 寻物启事 / `found` 失物招领，CHECK 约束 |
| `title` | TEXT NOT NULL | 标题，≤ 80 字 |
| `category` | TEXT NOT NULL DEFAULT '其他' | 分类，取值来自 `config.Categories` |
| `description` | TEXT NOT NULL DEFAULT '' | 详细描述，≤ 2000 字 |
| `location` | TEXT NOT NULL DEFAULT '' | 丢失 / 拾取地点，≤ 120 字 |
| `storage_place` | TEXT NOT NULL DEFAULT '' | **寄放处**（v4 新增）：捡到的东西现在存放在哪里，选填，≤ 120 字 |
| `happened_at` | TEXT NOT NULL DEFAULT '' | 丢失 / 拾取时间 |
| `contact` | TEXT NOT NULL DEFAULT '' | 联系方式，留空时用账号里的联系方式 |
| `image_url` | TEXT NOT NULL DEFAULT '' | 图片地址，`/uploads/…` 或 http(s) 链接 |
| `status` | TEXT NOT NULL DEFAULT 'open' | 进度三态，见下方 |
| `audit_status` / `audit_remark` | TEXT NOT NULL DEFAULT 'pending' / '' | 审核状态与驳回原因 |
| `user_id` | INTEGER NOT NULL REFERENCES users(id) | 发布者 |
| `created_at` / `updated_at` | TEXT NOT NULL | ISO 8601 |

`items.status` 是**三态**进度状态，由发布者本人（或管理员）维护，见 [第 6.4 节](#64-进度状态管理三态)：

| 值 | 界面文案 | 含义 |
| --- | --- | --- |
| `open` | 寻找中（红） | 默认值，事情还没有结果 |
| `found` | 已找到（绿） | 失物已寻回 / 招领已认领 |
| `closed` | 已结束（灰） | 主动结束，不再跟进 |

### 4.2 迁移

`schema_migrations` 表记录已应用的版本，`internal/database/migrate.go` 里是一个有序的迁移列表：

| 版本 | 内容 |
| --- | --- |
| 1 | 初始表结构 `users` / `items` / `sessions` |
| 2 | `users` 增加 `student_id`（学号）与 `name`（姓名），用旧的 `username` / `nickname` 回填，并建立学号唯一索引 |
| 3 | `items.status` 从两态（`open` / `closed`）扩展为三态（`open` / `found` / `closed`） |
| 4 | `items` 增加 `storage_place`（寄放处，选填），纯新增列，历史数据填空字符串 |
| 5 | 演示账号的学号改成 8 位数字：`admin`→`10000000`、`zhangsan`→`20230001`、`lisi`→`20230002`、`wangwu`→`20230003` |

老库（Node 版创建的）启动 Go 版时会自动补列 + 回填，不需要手工执行 SQL。
`ensureColumn` 通过 `PRAGMA table_info` 判断列是否存在，因为 SQLite 没有 `ADD COLUMN IF NOT EXISTS`。

**v3 为什么要重建整张表**：SQLite 不能修改已有的 `CHECK` 约束，只能按官方推荐的流程走一遍
「建新表 → 拷数据 → 删旧表 → 改名 → 重建索引」，整个过程在一个事务里，任何一步失败都会整体回滚。
旧的 `open` / `closed` 取值原样保留、语义不变，所以历史数据不需要改写。

**一个容易踩的坑**：判断「是否已经升级过」时不能简单地看建表语句里有没有 `'found'` ——
`items` 里还有一句 `CHECK (type IN ('lost', 'found'))`，会导致误判为已升级而跳过迁移。
正确做法是先把空白折叠，再精确匹配 `status IN ('open', 'found', 'closed')`：

```go
func itemStatusAllowsFound(db *DB) (bool, error) {
	var ddl sql.NullString
	err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'items'`).Scan(&ddl)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil // 表还不存在，交给 schema.sql 建
	}
	if err != nil {
		return false, fmt.Errorf("读取 items 建表语句失败: %w", err)
	}

	normalized := strings.Join(strings.Fields(ddl.String), " ")
	return strings.Contains(normalized, "status IN ('open', 'found', 'closed')"), nil
}
```

Node 版 `src/db.js` 里的 `migrateItemStatus` 是等价实现（Node 没有迁移表，用同样的结构判断来决定是否执行）。

### 4.3 为什么用 Session 而不是 JWT

选择 **数据库会话 + HttpOnly Cookie**，理由：

1. **可撤销**。JWT 一旦签发，在过期前始终有效；要做到「登出立即失效 / 停用账号立即踢下线 / 改密后其它设备失效」就必须再维护一张黑名单表，等于把服务端状态又加回来了。本项目的 `sessions` 表天生支持这三种撤销（`DestroySession` / `DestroyUserSessions`），登出、停用账号、改密都在用。
2. **没有额外依赖**。签发与校验只涉及 `crypto/rand` 和一次主键查询；用 JWT 需要引入 `github.com/golang-jwt/jwt` 并自行管理密钥轮换。
3. **不受 XSS 影响**。会话 ID 放在 `HttpOnly` Cookie 里，前端 JS 读不到；JWT 常见的 `localStorage` 存法一旦 XSS 就直接泄露令牌。配合 `SameSite=Lax` 还能挡掉大部分 CSRF。
4. **规模匹配**。这是单机 SQLite 应用，会话查询就是一次带索引的主键查找，不构成瓶颈；JWT 的「无状态、便于水平扩展」优势在这里用不上。

**什么时候该换成 JWT**：需要多实例/多服务共享登录态、或者要给第三方客户端发访问令牌时。届时的做法是「短期 access token（JWT）+ 长期 refresh token（存库可撤销）」，而不是只用 JWT。

### 4.4 接口

请求与响应都是 JSON。成功 `{"data": ...}`，失败为状态码 + `{"message": "..."}`。

| 方法 | 路径 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| POST | `/api/auth/register` | 公开 | 用户注册，学号唯一，成功后直接登录 |
| POST | `/api/auth/login` | 公开 | 用户登录，签发会话 Cookie |
| GET | `/api/auth/me` | 公开 | 获取当前用户信息（未登录返回 `{"user": null}`） |
| POST | `/api/auth/logout` | 公开 | 退出登录，销毁服务端会话 |
| PUT | `/api/auth/profile` | **RequireAuth** | 修改姓名与联系方式 |
| PUT | `/api/auth/password` | **RequireAuth** | 修改口令，其它设备会话全部失效 |

**注册**

```http
POST /api/auth/register
Content-Type: application/json

{ "studentId": "20230101", "password": "secret123", "name": "张三", "contact": "zhangsan@example.com" }
```

```json
// 201
{ "data": { "user": { "id": 5, "studentId": "20230101", "name": "张三", "contact": "zhangsan@example.com",
  "role": "user", "status": "active", "createdAt": "2026-10-05T03:20:00.000Z",
  "username": "20230101", "nickname": "张三" } } }
```

失败示例：学号重复 → `409 {"message":"该学号已被注册"}`；学号格式不对 → `400 {"message":"学号必须是 8 位数字"}`；口令太短 → `400 {"message":"密码至少需要 6 位"}`。

> 学号规则：**恰好 8 位数字**（`validate.StudentID`，正则 `^\d{8}$`），长度和字符集合并成一句提示。
> 这条规则只在**注册**时生效；登录只要求学号非空，这样即使库里有历史遗留的非 8 位学号也还能登进来。
> 内置的演示账号（`10000000` / `20230001` ~ `20230003`）都已经是 8 位数字，见 [迁移 v5](#42-迁移)。

**登录**

```http
POST /api/auth/login
Content-Type: application/json

{ "studentId": "20230101", "password": "secret123" }
```

成功返回 `200` 与上面的 `user` 对象，并写入 Cookie：

```
Set-Cookie: laf_sid=<64 位十六进制>; Path=/; Max-Age=604800; HttpOnly; SameSite=Lax
```

学号或口令错误 → `401 {"message":"学号或密码错误"}`；账号被停用 → `403 {"message":"该账号已被停用，请联系管理员"}`。

**获取当前用户 / 退出**

```http
GET  /api/auth/me        → 200 {"data":{"user":{...}}}   未登录 / 会话过期 → {"user": null}
POST /api/auth/logout    → 200 {"data":{"ok":true}}      同时删除数据库中的会话与 Cookie
```

### 4.5 鉴权中间件

在 `internal/web/middleware.go`：

| 中间件 | 作用 | 失败响应 |
| --- | --- | --- |
| `AttachUser()` | 请求级全局中间件，解析 `laf_sid` Cookie → 查会话 → 查用户，把用户挂到 `gin.Context` | 不拦截，未登录时上下文里没有用户 |
| `RequireAuth()` | 要求已登录 | `401 {"message":"请先登录"}` |
| `RequireRole(roles...)` | 要求角色属于给定集合 | 未登录 `401`；角色不符 `403 {"message":"仅管理员可访问"}` |
| `RequireAdmin()` | `RequireRole("admin")` 的简写 | 同上 |

保护接口的写法：

```go
items := engine.Group("/api/items")
guarded := items.Group("", web.RequireAuth())      // 这一组全部需要登录
guarded.POST("", createItem)
guarded.PUT("/:id", updateItem)

admin := engine.Group("/api/admin", web.RequireAdmin())
```

`AttachUser` 在 `NewRouter` 里通过 `engine.Use(...)` 全局挂载，所以任何处理器都可以用 `web.CurrentUser(c)` / `web.CurrentSessionID(c)` 取当前登录用户。
会话过期、用户被删除或被停用时，`AttachUser` 会顺手删掉这条会话。

### 4.6 与 Node 版共用数据库时的兼容

Node 版后端与现有前端页面仍在读写 `users.username` / `users.nickname`，因此：

- 本表同时保留这两列，Go 版写入时把 `student_id` / `name` 一起写进兼容列，两边取值始终相同（见 `store.CreateUser`、`store.UpdateProfile`）。
- 读取时用 `firstNonEmpty(student_id, username)` 取值，所以 Node 版创建的历史账号也能正常显示。
- 学号唯一性用**部分唯一索引**（`WHERE student_id <> ''`）而不是列约束，这样 Node 版写入的 `student_id=''` 占位行不会互相冲突；Go 版启动时（`bootstrap.Run`）会把这些行按 `username` 回填成学号。
- 接口同时接受新旧字段名：学号接受 `studentId` 或 `username`，姓名接受 `name` 或 `nickname`。前端改完之后可以把兼容列与兼容字段一起删掉。

> 因此 Go 版的用户对象比 Node 版多出 `studentId` / `name` 两个键，`tools/contract-parity.cjs` 会在涉及用户对象的地方报出这些**预期内**的差异。

---

## 五、与 Node 版的一致性

以下行为都做了逐条对齐，不是「差不多」：

| 方面 | 对齐内容 |
| --- | --- |
| 响应封装 | 成功 `{"data": ...}`；失败为状态码 + `{"message": "..."}`，`details` 可选 |
| 校验文案 | 所有 400 文案（含中文标点、`至少需要 N 个字符`、`必须在 A 到 B 之间`、`只能是 X / Y 之一`）逐字一致 |
| JS 语义 | 数字按 `Number()` 解析（支持 `10.0`、`0x1`、纯空白视为 `0`）；重复查询参数按「类型不对」拒绝；字符串按 UTF-16 码元计长度 |
| 请求体 | 只解析 `application/json` 与 `application/x-www-form-urlencoded`；非法 JSON → 400；超过 256kb → 413；其它 Content-Type 视为空对象 |
| 口令 | `scrypt$N$r$p$salt(base64)$hash(base64)`，`N=16384, r=8, p=1, keylen=64`，恒定时间比较 |
| 会话 | Cookie 名 `laf_sid`，32 字节随机十六进制，`HttpOnly` + `SameSite=Lax` + `Path=/`，数据库中带过期时间 |
| 静态资源 | `/login` → `public/login.html`，`/` → `public/index.html`；未命中时返回与 Express 默认 404 相同的 HTML |
| 兜底 | 未匹配的 `/api/*` 返回 `{"message":"接口不存在"}`；未匹配的其它路径返回 HTML 404 |
| 业务规则 | 公开列表只展示 `approved`；普通用户修改后重新置为 `pending`；驳回必须填原因；不允许停用或降级最后一个可用管理员 |

**唯一一处「有意不对称」：寄放处 `storagePlace`**。它是 v4 新增的字段，按需求只改了 Go 版后端：

| | Go 版 | Node 版 |
| --- | --- | --- |
| `items.storage_place` 列 | 迁移自动补列，正常读写 | 不读不写（列存在但被忽略） |
| `POST` / `PUT` / `PATCH` 请求体里的 `storagePlace` | 校验并落库，≤ 120 字 | 静默忽略 |
| 响应里的 `storagePlace` | 返回 | 不返回 |
| 经 Node 版修改信息 | — | 不会清空已有的寄放处（SQL 里没有这一列，只是不更新它） |

因此用 Node 版发布 / 修改的信息，寄放处一律为空；`tools/contract-parity.cjs` 在比对前会把双方条目里的 `storagePlace` 抹掉，否则每一步 items 响应都会报差异。Node 版补齐后应把这段归一化删掉。

### 交叉兼容

两版指向同一个 `.db` 文件时会话与数据互通，可以只用一个数据库文件交替启动：

```powershell
# 终端 1
$env:DB_FILE='data\lostfound.db'; $env:PORT=3000; node server.js
# 终端 2（复用同一份数据）
$env:DB_FILE='data\lostfound.db'; $env:PORT=3001; .\go-backend\bin\lostfound.exe
```

Go 登录拿到的 Cookie 可以在 Node 上继续用，反之亦然；Go 注册的账号可以用 Node 登录（同一份 scrypt 散列）。

---

## 六、失物 / 招领信息 CRUD

### 6.1 路由

全部实现在 [`internal/web/item_routes.go`](internal/web/item_routes.go)，注册方式把「公开 / 需登录 / 需本人」三层权限直接写在路由上：

```go
func registerItemRoutes(g *gin.RouterGroup, deps *Deps) {
	// 公开读接口
	g.GET("", handle(func(c *gin.Context) error { return itemsPublicList(c, deps) }))
	g.GET("/categories", handle(func(c *gin.Context) error { return itemsCategories(c) }))

	// 需要登录
	authed := g.Group("", RequireAuth())
	authed.POST("", handle(func(c *gin.Context) error { return itemsCreate(c, deps) }))
	authed.GET("/mine", handle(func(c *gin.Context) error { return itemsMine(c, deps) }))

	// 需要登录 + 必须是发布者本人（或管理员）
	owned := g.Group("/:id", RequireAuth(), RequireItemOwner(deps.Store))
	owned.PUT("", handle(func(c *gin.Context) error { return itemsUpdate(c, deps) }))
	owned.PATCH("", handle(func(c *gin.Context) error { return itemsPatch(c, deps) }))
	owned.POST("/status", handle(func(c *gin.Context) error { return itemsSetStatus(c, deps) }))
	owned.DELETE("", handle(func(c *gin.Context) error { return itemsDelete(c, deps) }))

	g.GET("/:id", handle(func(c *gin.Context) error { return itemsDetail(c, deps) }))
}
```

| 方法 | 路径 | 权限 | 说明 |
| --- | --- | --- | --- |
| GET | `/api/items` | 公开 | 列表；只返回 `auditStatus = approved` 的信息 |
| GET | `/api/items/categories` | 公开 | 分类字典 |
| GET | `/api/items/:id` | 公开 | 详情；未过审的仅发布者与管理员可见 |
| POST | `/api/items` | 登录 | 创建，默认 `pending` + `open` |
| GET | `/api/items/mine` | 登录 | 我的发布（含待审核与已驳回） |
| PUT | `/api/items/:id` | **本人/管理员** | 整体替换：请求体缺省的字段会被清空 |
| PATCH | `/api/items/:id` | **本人/管理员** | 局部更新：只改请求体里出现的字段 |
| POST | `/api/items/:id/status` | **本人/管理员** | 改进度状态：寻找中 / 已找到 / 已结束 |
| DELETE | `/api/items/:id` | **本人/管理员** | 删除 |

### 6.2 权限：会话用户 ID 与 items.user_id 比对

`RequireItemOwner` 中间件（[`internal/web/middleware.go`](internal/web/middleware.go)）把「当前登录用户」与「这条信息的发布者」绑定起来：

```go
func RequireItemOwner(s *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		user := CurrentUser(c)              // 由 AttachUser 从会话 Cookie 解析出来
		if user == nil {
			httpx.Abort(c, httpx.Unauthorized(""))
			return
		}

		id, err := idParam(c)
		if err != nil {
			httpx.Abort(c, err)
			return
		}

		row, err := s.FindItemByID(id)      // 取出这条信息
		if err != nil {
			httpx.Abort(c, httpx.New(http.StatusInternalServerError, "服务器内部错误"))
			return
		}
		if row == nil {
			httpx.Abort(c, httpx.NotFound("该信息不存在或已被删除"))
			return
		}

		// 核心的一次比对：会话里的用户 ID vs 数据库里的 items.user_id
		if user.Role != "admin" && row.UserID != user.ID {
			httpx.Abort(c, httpx.Forbidden("只能修改自己发布的信息"))
			return
		}

		c.Set(itemContextKey, row)          // 查出来的行放进上下文，处理器不必再查一次
		c.Next()
	}
}
```

处理器里用 `ContextItem(c)` 取这条已经校验过的信息：

```go
func itemsPatch(c *gin.Context, deps *Deps) error {
	row := ContextItem(c)                              // 中间件已经校验过归属

	if !hasAnyField(c, patchableFields) {
		return httpx.BadRequest("没有需要修改的字段")
	}
	in, err := readItemPatchInput(c, row)              // 缺省字段沿用 row 的当前值
	if err != nil {
		return err
	}

	item, err := deps.Store.UpdateItem(row.ID, CurrentUser(c), in)
	if err != nil {
		return err
	}
	httpx.SendData(c, gin.H{"item": store.ShapeItem(item)}, http.StatusOK)
	return nil
}
```

权限矩阵（已由 `tools/item-crud-test.cjs` 逐条验证）：

| 操作 | 匿名 | 其他登录用户 | 发布者本人 | 管理员 |
| --- | --- | --- | --- | --- |
| 列表 / 详情（已过审） | ✅ 200 | ✅ 200 | ✅ 200 | ✅ 200 |
| 详情（未过审） | ❌ 404 | ❌ 404 | ✅ 200 `canEdit: true` | ✅ 200 |
| 创建 | ❌ 401 | ✅ 201 | ✅ 201 | ✅ 201 |
| 修改 / 局部更新 / 改状态 / 删除 | ❌ 401 | ❌ **403** | ✅ 200 | ✅ 200 |

`store` 层的 `AssertCanModify` 在写操作里会再校验一次同样的规则（同一份数据、同一份判断），
所以即使将来有人绕过路由层直接调用 store，权限也不会失守；路由层的中间件则保证未授权的请求
在解析请求体之前就被拒绝。

### 6.3 搜索、排序与分页（SQL 构建）

查询条件的拼装集中在 [`internal/store/item_query.go`](internal/store/item_query.go)，
`ListItems` 只负责把它拼成完整语句并执行：

```sql
SELECT COUNT(*) FROM items i JOIN users u ON u.id = i.user_id [WHERE …]

SELECT i.id, i.type, …, u.student_id AS owner_student_id, u.name AS owner_name
FROM items i JOIN users u ON u.id = i.user_id
[WHERE …]
ORDER BY <白名单列> <ASC|DESC>, i.id <ASC|DESC>
LIMIT ? OFFSET ?
```

**WHERE + LIKE（关键词搜索）**

```go
// buildItemWhere 把过滤条件编译成 WHERE 子句与对应的参数列表。
//
// 三条原则：
//  1. SQL 文本里的每一段都是代码里的常量；
//  2. 用户输入一律只作为 ? 的参数传入，绝不做字符串拼接；
//  3. 条件按固定顺序追加，参数顺序与占位符顺序严格一致。
func buildItemWhere(o ListOptions) (string, []any) {
	conditions := make([]string, 0, 6)
	args := make([]any, 0, 10)

	if o.PublicOnly {
		conditions = append(conditions, "i.audit_status = 'approved'")
	} else if o.AuditStatus != nil {
		conditions = append(conditions, "i.audit_status = ?")
		args = append(args, *o.AuditStatus)
	}
	if o.Type != nil {
		conditions = append(conditions, "i.type = ?")
		args = append(args, *o.Type)
	}
	if o.Category != nil {
		conditions = append(conditions, "i.category = ?")
		args = append(args, *o.Category)
	}
	if o.Status != nil {
		conditions = append(conditions, "i.status = ?")
		args = append(args, *o.Status)
	}
	if o.UserID != nil {
		conditions = append(conditions, "i.user_id = ?")
		args = append(args, *o.UserID)
	}

	// 关键词：四个字段做 LIKE 模糊匹配，任意一个命中即可。
	// % 通配符由后端拼在参数值两端，整个值仍然是参数，不会改变 SQL 结构。
	if o.Keyword != nil {
		conditions = append(conditions,
			"(i.title LIKE ? OR i.description LIKE ? OR i.location LIKE ? OR i.category LIKE ?)")
		like := "%" + *o.Keyword + "%"
		args = append(args, like, like, like, like)
	}

	if len(conditions) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conditions, " AND "), args
}
```

**ORDER BY（白名单）**

排序字段和方向没法写成 `?` 占位符（占位符只能表示「值」，不能表示列名或关键字），
所以它们必须被拼进 SQL 文本 —— 唯一安全的做法是先过白名单：

```go
var itemSortColumns = map[string]string{
	"id":         "i.id",
	"createdAt":  "i.created_at",
	"updatedAt":  "i.updated_at",
	"happenedAt": "i.happened_at",
	"title":      "i.title",
	"category":   "i.category",
	"status":     "i.status",
	"type":       "i.type",
}

// ItemSortKeys 传给接口层做参数校验（顺序固定，便于生成错误文案）。
var ItemSortKeys = []string{
	"createdAt", "updatedAt", "happenedAt", "title", "category", "status", "type", "id",
}

var ItemSortOrders = []string{"desc", "asc"}

// buildItemOrderBy 把排序参数编译成 ORDER BY 子句。
//
//	o.Sort   排序字段，必须命中 itemSortColumns，否则退回 createdAt
//	o.Order  asc / desc，否则退回 desc
//
// 末尾总会再追加一个 i.id 作为「第二排序键」：
// 同一时刻创建的两条记录如果顺序不确定，LIMIT/OFFSET 翻页时就会出现
// 同一条记录在第 1 页和第 2 页各出现一次、另一条却完全看不到的情况。
func buildItemOrderBy(o ListOptions) string {
	column, ok := itemSortColumns[o.Sort]
	if !ok {
		column = itemSortColumns[DefaultItemSort]
	}

	direction := "DESC"
	if strings.EqualFold(o.Order, "asc") {
		direction = "ASC"
	}

	return fmt.Sprintf(" ORDER BY %s %s, i.id %s", column, direction, direction)
}
```

**LIMIT + OFFSET（分页）**

```go
func (s *Store) ListItems(o ListOptions) (*models.Page, error) {
	where, args := buildItemWhere(o)
	orderBy := buildItemOrderBy(o)
	page, pageSize, offset := normalizePaging(o.Page, o.PageSize)

	// 先数总数：用来算总页数，前端的分页按钮依赖它。
	var total int64
	if err := s.DB.QueryRow(`SELECT COUNT(*) `+itemFrom+where, args...).Scan(&total); err != nil {
		return nil, wrap(err)
	}

	// 再取当前页。LIMIT / OFFSET 也走占位符，避免手工拼数字。
	rows, err := s.DB.Query(
		`SELECT `+itemSelectFields+` `+itemFrom+where+orderBy+` LIMIT ? OFFSET ?`,
		append(args, pageSize, offset)...,
	)
	// …扫描 rows、组装 models.Page
	totalPages := int(math.Max(1, math.Ceil(float64(total)/float64(pageSize))))

	return &models.Page{
		Items: items, Page: page, PageSize: pageSize, Total: total, TotalPages: totalPages,
	}, nil
}
```

**查询参数**

| 参数 | 取值 | 缺省 | 说明 |
| --- | --- | --- | --- |
| `keyword` | ≤ 60 字 | — | 对 `title` / `description` / `location` / `category` 做 `LIKE '%…%'` |
| `type` | `lost` / `found` | — | |
| `category` | 分类名 | — | |
| `status` | `open`(寻找中) / `found`(已找到) / `closed`(已结束) | — | |
| `sort` | `createdAt` / `updatedAt` / `happenedAt` / `title` / `category` / `status` / `type` / `id` | `createdAt` | 非法值 → `400 排序字段只能是 … 之一` |
| `order` | `asc` / `desc` | `desc` | 非法值 → `400 排序方向只能是 desc / asc 之一` |
| `page` | ≥ 1 的整数 | `1` | 页码 |
| `limit` | 1 ~ 50 | `10` | 每页数量；`pageSize` 是历史别名，同时给出时以 `limit` 为准 |

`GET /api/items/search` 与 `GET /api/items` 完全等价，只是让「搜索」这个意图在 URL 上更直观。

两处与安全相关的细节：

- 排序字段与方向走白名单，`?sort=id;DROP TABLE items--` 会得到 `400` 而不是执行 SQL；
- 关键词整体作为 `?` 参数传入，`?keyword=' OR '1'='1` 只会被当成一串普通字符去匹配（结果为 0 条）。

### 6.4 进度状态管理（三态）

`items.status` 有三个取值，界面文案与颜色如下：

| 值 | 文案 | 标签样式 | 含义 |
| --- | --- | --- | --- |
| `open` | 寻找中 | `.badge.status-open`（红） | 默认值 |
| `found` | 已找到 | `.badge.status-found`（绿） | 已寻回 / 已认领 |
| `closed` | 已结束 | `.badge.status-closed`（灰） | 主动结束 |

**后端更新逻辑**

状态有三个入口，都要求「发布者本人或管理员」（由 `RequireItemOwner` 中间件用会话用户 ID 与
`items.user_id` 比对后放行）：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| POST | `/api/items/:id/status` | 专门改状态，请求体 `{ "status": "found" }` |
| PATCH | `/api/items/:id` | 局部更新，可以只提交 `{ "status": "closed" }` |
| PUT | `/api/items/:id` | 整体替换，请求体里带上 `status` |

```go
// statuses 的合法取值来自 store.ItemStatuses，顺序即前端标签顺序
var itemStatuses = store.ItemStatuses // {"open", "found", "closed"}

// itemsSetStatus 标记为已找到 / 已结束 / 重新开放。
// 路径上的 :id 已经由 RequireItemOwner 校验过归属，这里直接用上下文里的行。
func itemsSetStatus(c *gin.Context, deps *Deps) error {
	status, err := validate.OneOf(FieldOrNil(c, "status"), "status", itemStatuses,
		validate.OneOfOpts{Label: "状态"})
	if err != nil {
		return err
	}

	item, err := deps.Store.SetItemStatus(ContextItem(c).ID, CurrentUser(c), status)
	if err != nil {
		return err
	}
	httpx.SendData(c, gin.H{"item": store.ShapeItem(item)}, http.StatusOK)
	return nil
}
```

数据访问层只改 `status` 与 `updated_at` 两列：

```go
// SetItemStatus 修改进度状态；归属校验在调用前后各做一次（中间件 + AssertCanModify）。
func (s *Store) SetItemStatus(id int64, user *UserRow, status string) (*ItemRow, error) {
	row, err := s.FindItemByID(id)
	if err != nil {
		return nil, err
	}
	if err := AssertCanModify(row, user); err != nil {
		return nil, err
	}

	if _, err := s.DB.Exec(
		`UPDATE items SET status = ?, updated_at = ? WHERE id = ?`,
		status, database.Now(), id,
	); err != nil {
		return nil, wrap(err)
	}
	return s.FindItemByID(id)
}
```

**流转规则**：后端不限制方向，只校验取值合法 —— `open → found → closed`、`closed → open`
（重新开放）、`found → open`（撤销误标）都可以。这样发布者可以随时纠正操作，
不需要管理员介入。非法取值返回 `400 状态只能是 open / found / closed 之一`。

**统计口径**：`GET /api/admin/stats` 新增 `byStatus` 数组，固定按 `open` / `found` / `closed`
顺序输出、缺的补 0，前端可以直接按顺序渲染状态卡片：

```json
{ "data": { "stats": {
  "total": 9, "open": 8, "closed": 0,
  "resolvedRate": 11,
  "byStatus": [
    { "status": "open",   "count": 8 },
    { "status": "found",  "count": 1 },
    { "status": "closed", "count": 0 }
  ]
} } }
```

> 注意 `resolvedRate`（解决率）的口径从「已结束 / 总数」改成了「（已找到 + 已结束）/ 总数」，
> 因为三态之后「已找到」同样是「有结论」的终态。`stats.lost` / `stats.found` 仍然是**信息类型**
> 的数量（寻物启事 / 失物招领），与进度状态无关，别混淆。

**前端状态标签渲染**

标签统一由 `public/js/common.js` 生成，三处页面（首页 / 详情 / 后台 / 我的发布）共用：

```js
/** 进度状态：open = 寻找中（红） / found = 已找到（绿） / closed = 已结束（灰）。 */
const STATUS_LABELS = { open: '寻找中', found: '已找到', closed: '已结束' };

function statusLabel(status) {
  return STATUS_LABELS[status] || status || '';
}

function statusBadge(status) {
  const kind = STATUS_LABELS[status] ? status : 'open';
  return '<span class="badge status-' + kind + '">' + statusLabel(kind) + '</span>';
}

/** 生成三态下拉框的 <option>，用于「我的发布」里直接改状态。 */
function statusOptions(current) {
  return Object.keys(STATUS_LABELS)
    .map(function (value) {
      return '<option value="' + value + '"' + (value === current ? ' selected' : '') + '>' +
        STATUS_LABELS[value] + '</option>';
    })
    .join('');
}
```

颜色在 `public/css/style.css` 里：

```css
/* 进度状态：寻找中 = 红，已找到 = 绿，已结束 = 灰 */
.badge.status-open { background: #fef2f2; color: #b91c1c; }
.badge.status-found { background: var(--found-soft); color: var(--found); }
.badge.status-closed { background: #f1f4fa; color: var(--muted); }
```

「我的发布」表格里直接用下拉框改状态，失败时回滚：

```js
'<select class="status-select status-' + App.esc(item.status) + '" data-action="status" data-id="' + item.id +
'" data-current="' + App.esc(item.status) + '">' + App.statusOptions(item.status) + '</select>'

els.tbody.addEventListener('change', async function (event) {
  const select = event.target.closest('select[data-action="status"]');
  if (!select) return;

  const id = select.dataset.id;
  const previous = select.dataset.current;
  if (select.value === previous) return;

  select.disabled = true;
  try {
    const data = await API.post('/api/items/' + id + '/status', { status: select.value });
    App.toast('状态已改为「' + App.statusLabel(data.item.status) + '」');
    load();
  } catch (err) {
    App.showAlert(els.alert, err.message, 'error');
    select.value = previous;   // 失败时回滚下拉框
    select.disabled = false;
  }
});
```

详情页则按当前状态渲染「能改成什么」，当前状态对应的按钮不出现：

```js
const statusButtons = item.canEdit
  ? (item.status !== 'found'  ? statusButton('found',  '标记为已找到', 'success') : '') +
    (item.status !== 'closed' ? statusButton('closed', '标记为已结束', 'ghost')   : '') +
    (item.status !== 'open'   ? statusButton('open',   '重新开放为寻找中', 'ghost') : '')
  : '';
```

### 6.5 响应示例

```http
POST /api/items
Content-Type: application/json
Cookie: laf_sid=...

{ "type": "lost", "title": "丢失一个黑色钱包", "category": "钱包箱包",
  "description": "内有校园卡和银行卡", "location": "图书馆三楼", "happenedAt": "2026-10-01 15:30" }
```

```json
// 201
{ "data": { "item": {
  "id": 12, "type": "lost", "title": "丢失一个黑色钱包", "category": "钱包箱包",
  "description": "内有校园卡和银行卡", "location": "图书馆三楼", "happenedAt": "2026-10-01 15:30",
  "contact": "zhangsan@example.com", "imageUrl": "",
  "status": "open", "auditStatus": "pending", "auditRemark": "",
  "userId": 2,
  "owner": { "id": 2, "studentId": "20230001", "name": "张三", "username": "20230001", "nickname": "张三" },
  "createdAt": "2026-10-05T06:00:00.000Z", "updatedAt": "2026-10-05T06:00:00.000Z"
} } }
```

```json
// 403：改别人的信息
{ "message": "只能修改自己发布的信息" }
```

```json
// 200：列表
{ "data": { "items": [ /* … */ ], "page": 1, "pageSize": 10, "total": 8, "totalPages": 1,
  "categories": ["证件卡类", "电子产品", "..."] } }
```

列表支持的查询参数：`type`、`category`、`status`、`keyword`、`page`、`pageSize`。

### 6.6 PUT 与 PATCH 的区别

| | PUT | PATCH |
| --- | --- | --- |
| 语义 | 整体替换 | 局部更新 |
| 请求体里没出现的字段 | **清空为默认值** | **保持原值** |
| 典型用途 | 编辑表单整表提交 | 「只改标题」「只标记已解决」 |
| 空请求体 | 允许（等价于清空可选字段） | `400 没有需要修改的字段` |

两者都会让普通用户的信息重新回到 `pending`（管理员修改不受影响）。

### 6.7 前端调用示例

完整可运行页面：**`public/crud-demo.html`** + **`public/js/crud-demo.js`**，启动后端后访问 <http://127.0.0.1:3001/crud-demo>。

用的是原生 Fetch API，核心封装：

```js
async function request(method, path, body) {
  const init = {
    method,
    credentials: 'same-origin', // 关键：带上会话 Cookie，后端据此解析出用户 ID
    headers: {},
  };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }

  const response = await fetch(path, init);
  const text = await response.text();
  const payload = text ? JSON.parse(text) : null;

  if (!response.ok) {
    const error = new Error((payload && payload.message) || 'HTTP ' + response.status);
    error.status = response.status;   // 调用方可以按 401 / 403 / 404 分别处理
    throw error;
  }
  return payload ? payload.data : null;
}
```

五个操作：

```js
// 列表
const list = await request('GET', '/api/items?' + new URLSearchParams({
  type: 'lost', status: 'open', keyword: '钱包', page: 1, pageSize: 10,
}));

// 详情
const detail = await request('GET', '/api/items/' + id);          // detail.canEdit 表示能否修改

// 创建
const created = await request('POST', '/api/items', {
  type: 'lost', title: '丢失一个黑色钱包', category: '钱包箱包',
  description: '内有校园卡', location: '图书馆三楼', happenedAt: '2026-10-01 15:30',
});

// 修改（整体替换）
await request('PUT', '/api/items/' + id, {
  type: 'lost', title: '改过的标题', category: '钱包箱包',
  description: '', location: '', happenedAt: '', contact: '', status: 'open',
});

// 局部更新（只改标题，其余字段保持原值）
await request('PATCH', '/api/items/' + id, { title: '只改标题' });

// 切换进度状态
await request('POST', '/api/items/' + id + '/status', { status: 'closed' });

// 删除
await request('DELETE', '/api/items/' + id);
```

错误处理（把状态码翻译成人话）：

```js
try {
  await request('DELETE', '/api/items/' + id);
} catch (err) {
  if (err.status === 401) alert('请先登录');
  else if (err.status === 403) alert('只能修改或删除自己发布的信息');
  else if (err.status === 404) alert('这条信息不存在或已被删除');
  else alert(err.message);
}
```

**Axios 版本**（本项目没引入 Axios，下面只是等价写法，需要先 `<script src="https://cdn.jsdelivr.net/npm/axios/dist/axios.min.js"></script>`）：

```js
// 让每个请求都带上会话 Cookie
axios.defaults.withCredentials = true;

axios.interceptors.response.use(
  (res) => res.data.data,                                   // 自动剥掉 { data: ... }
  (err) => Promise.reject(Object.assign(
    new Error(err.response?.data?.message || '请求失败'),
    { status: err.response?.status },
  )),
);

const list    = await axios.get('/api/items', { params: { type: 'lost', page: 1, pageSize: 10 } });
const detail  = await axios.get(`/api/items/${id}`);
const created = await axios.post('/api/items', { type: 'lost', title: '丢失一个黑色钱包' });
await axios.put(`/api/items/${id}`, fullBody);
await axios.patch(`/api/items/${id}`, { title: '只改标题' });
await axios.post(`/api/items/${id}/status`, { status: 'closed' });
await axios.delete(`/api/items/${id}`);
```

> `withCredentials = true` 是 Axios 的关键配置，等价于 Fetch 的 `credentials: 'same-origin'`；
> 少了它浏览器不会带 Cookie，所有需要登录的接口都会返回 401。

### 6.8 实现分布

- `/api/auth/*` → `internal/web/auth_routes.go`
- `/api/items/*` → `internal/web/item_routes.go`
- `/api/admin/*` → `internal/web/admin_routes.go`
- 鉴权与归属校验中间件 → `internal/web/middleware.go`
- `/api/health`、静态资源、404 → `internal/web/router.go`、`internal/web/static.go`

---

## 七、图片上传

发布失物 / 招领信息时可以带一张图片。图片存在服务器本地，数据库里只保存访问路径。

| | 值 |
| --- | --- |
| 落盘目录 | `public/uploads/`（`UPLOAD_DIR` 可改），默认就在静态资源目录里，因此无需额外配置就能访问 |
| 访问路径 | `/uploads/<时间戳>-<16 位随机十六进制>.<扩展名>` |
| 数据库字段 | `items.image_url`（TEXT，已有字段，不需要迁移） |
| 大小上限 | 5 MB（`MAX_UPLOAD_MB` 可改） |
| 允许格式 | JPG / PNG / GIF / WebP |

### 7.1 接口

```http
POST /api/uploads
Content-Type: multipart/form-data; boundary=----WebKitFormBoundaryXXX
Cookie: laf_sid=...

------WebKitFormBoundaryXXX
Content-Disposition: form-data; name="file"; filename="钱包.png"
Content-Type: image/png

<二进制内容>
------WebKitFormBoundaryXXX--
```

```json
// 201
{ "data": { "url": "/uploads/1759824000-3f9a2c1b8d4e5f60.png",
            "filename": "1759824000-3f9a2c1b8d4e5f60.png",
            "size": 20480,
            "mimeType": "image/png" } }
```

失败：未登录 `401`；不是 multipart / 没有 `file` 字段 / 格式不对 → `400`；超过 5MB → `413`。
表单字段名固定为 `file`。

拿到 `url` 之后，把它放进创建 / 修改接口的 `imageUrl` 字段即可：

```jsonc
POST /api/items
{ "type": "lost", "title": "丢失一个黑色钱包", "category": "钱包箱包",
  "imageUrl": "/uploads/1759824000-3f9a2c1b8d4e5f60.png" }
```

`imageUrl` 做了白名单校验：留空表示没有图片，非空只能是 `/uploads/...` 站内路径或 `http(s)` 链接，
所以 `javascript:`、`data:` 之类的写法既进不了数据库，也进不了前端的 `<img src>`。

### 7.2 落盘逻辑（`internal/upload/upload.go`）

核心是「只信文件头，不信文件名」：

```go
// imageTypes 允许的图片格式：文件头嗅探结果 -> 落盘扩展名。
//
// 这张表同时是「白名单」和「扩展名来源」：扩展名由嗅探结果决定，
// 而不是由用户提供的文件名决定，所以把 shell.php 改名成 shell.png 也没用。
var imageTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// Save 校验并保存一张图片，返回可对外访问的相对路径。
func (s *Store) Save(src io.Reader) (*Saved, error) {
	if err := s.EnsureDir(); err != nil {
		return nil, err
	}

	// 1. 先读文件头判断真实类型，不合法直接拒绝（此时还没有写任何文件）
	head := make([]byte, sniffLen)
	n, err := io.ReadFull(src, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, httpx.BadRequest("读取上传内容失败")
	}
	head = head[:n]
	if n == 0 {
		return nil, httpx.BadRequest("上传的文件是空的")
	}

	mimeType := http.DetectContentType(head)
	ext, ok := imageTypes[mimeType]
	if !ok {
		return nil, httpx.BadRequest("只支持 JPG / PNG / GIF / WebP 格式的图片")
	}

	// 2. 文件名完全由服务端生成，客户端传的 filename 一律丢弃
	filename, err := randomFilename(ext)
	if err != nil {
		return nil, err
	}
	fullPath := filepath.Join(s.Dir, filename)

	file, err := os.OpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("创建文件失败: %w", err)
	}

	// 3. 头部已经读走，余下内容从 src 继续读；LimitReader 兜住超限的情况
	written, copyErr := io.Copy(file, io.MultiReader(
		bytes.NewReader(head),
		io.LimitReader(src, s.MaxBytes-int64(n)+1),
	))
	closeErr := file.Close()

	if copyErr != nil {
		_ = os.Remove(fullPath)
		return nil, fmt.Errorf("写入文件失败: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(fullPath)
		return nil, fmt.Errorf("关闭文件失败: %w", closeErr)
	}
	// 4. 写超了就删掉半个文件，不会留下垃圾
	if written > s.MaxBytes {
		_ = os.Remove(fullPath)
		return nil, httpx.New(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("图片不能超过 %d MB", s.MaxBytes/1024/1024))
	}

	return &Saved{
		URL:      s.URLPrefix + "/" + filename,
		Filename: filename,
		Size:     written,
		MimeType: mimeType,
	}, nil
}

// randomFilename 生成「时间戳-随机数.扩展名」，例如 1759824000-3f9a2c1b.png。
func randomFilename(ext string) (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("生成文件名失败: %w", err)
	}
	return fmt.Sprintf("%d-%s%s", time.Now().Unix(), hex.EncodeToString(buf), ext), nil
}
```

### 7.3 路由处理（`internal/web/upload_routes.go`）

```go
func registerUploadRoutes(g *gin.RouterGroup, deps *Deps) {
	guarded := g.Group("", RequireAuth())   // 必须登录才能上传
	guarded.POST("", handle(func(c *gin.Context) error { return uploadImage(c, deps) }))
}

func uploadImage(c *gin.Context, deps *Deps) error {
	store := deps.Uploads
	tooLarge := httpx.New(http.StatusRequestEntityTooLarge,
		"图片不能超过 "+strconv.FormatInt(store.MaxBytes/1024/1024, 10)+" MB")

	// 先确认是 multipart 请求，避免把一个 JSON 请求当成上传去解析
	mediaType, params, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if mediaType != "multipart/form-data" || params["boundary"] == "" {
		return httpx.BadRequest("请使用 multipart/form-data 上传（表单字段名应为 file）")
	}

	// 给整条请求体设上限，避免超大文件把内存与临时目录打满
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, store.MaxRequestBytes())

	fileHeader, err := c.FormFile("file")
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return tooLarge
		}
		if errors.Is(err, http.ErrMissingFile) {
			return httpx.BadRequest("请选择要上传的图片（表单字段名应为 file）")
		}
		return httpx.BadRequest("解析上传内容失败：" + err.Error())
	}
	if fileHeader.Size > store.MaxBytes {
		return tooLarge
	}

	file, err := fileHeader.Open()
	if err != nil {
		return httpx.BadRequest("读取上传文件失败")
	}
	defer func() { _ = file.Close() }()

	saved, err := store.Save(file)   // 内部丢弃客户端文件名，自己生成
	if err != nil {
		return err
	}

	httpx.SendData(c, gin.H{
		"url": saved.URL, "filename": saved.Filename,
		"size": saved.Size, "mimeType": saved.MimeType,
	}, http.StatusCreated)
	return nil
}
```

**bodyParser 必须跳过 multipart**（`internal/web/body.go`）：那个中间件会把请求体一次性读到内存里，
如果它先把 Body 读空了，后面 `ParseMultipartForm` 就拿不到文件了：

```go
mediaType, _, _ := mime.ParseMediaType(c.GetHeader("Content-Type"))
if mediaType == "multipart/form-data" {
	c.Next()   // 原样把 Body 留给上传处理器
	return
}
```

### 7.4 前端表单与上传

「发布信息」页的表单控件（`public/publish.html`）：

```html
<div class="field">
  <label for="imageFile">物品图片<span class="hint">选填，JPG / PNG / GIF / WebP，单张不超过 5MB</span></label>
  <input type="file" id="imageFile" accept="image/png,image/jpeg,image/gif,image/webp">
  <div class="image-preview" id="image-preview"></div>
</div>

<div class="field">
  <label for="imageUrl">图片地址<span class="hint">选填；上传图片后会自动填在这里，也可以直接粘贴图片链接</span></label>
  <input type="text" id="imageUrl" maxlength="500" placeholder="/uploads/… 或 https://…">
</div>
```

统一的上传封装（`public/js/api.js`）——**注意不要手写 `Content-Type`**，
必须让浏览器自己带上 multipart 的 boundary：

```js
/** 上传单个文件到 path，返回 { url, filename, size, mimeType }。 */
upload: (path, file, field) => {
  const form = new FormData();
  form.append(field || 'file', file);
  return request(path, { method: 'POST', form });
},

// request() 内部：
if (opts.form !== undefined) {
  // FormData：绝对不要手写 Content-Type，浏览器要自己带上 multipart 的 boundary
  init.body = opts.form;
} else if (opts.body !== undefined) {
  init.headers['Content-Type'] = 'application/json';
  init.body = JSON.stringify(opts.body);
}
```

发布页的交互（`public/js/publish.js`）：选中文件先本地预览，提交时先上传拿到路径再发 JSON：

```js
els.imageFile.addEventListener('change', function () {
  const file = els.imageFile.files && els.imageFile.files[0];
  if (!file) { renderPreview(els.imageUrl.value.trim()); return; }

  if (!/^image\/(jpeg|png|gif|webp)$/.test(file.type)) {
    App.showAlert(els.alert, '只支持 JPG / PNG / GIF / WebP 格式的图片', 'error');
    els.imageFile.value = '';
    return;
  }
  if (file.size > 5 * 1024 * 1024) {
    App.showAlert(els.alert, '图片不能超过 5MB', 'error');
    els.imageFile.value = '';
    return;
  }

  previewObjectUrl = URL.createObjectURL(file);   // 本地预览，不用等上传
  renderPreview(previewObjectUrl);
});

/**
 * 提交前先把本地图片传到 POST /api/uploads，拿到站内路径再写进 imageUrl。
 * 这样发布接口本身仍然只是普通的 JSON 请求，不需要处理 multipart。
 */
async function uploadIfNeeded() {
  const file = els.imageFile.files && els.imageFile.files[0];
  if (!file) return;

  els.submitBtn.textContent = '上传图片中…';
  const data = await API.upload('/api/uploads', file);
  els.imageUrl.value = data.url;
}

els.form.addEventListener('submit', async function (event) {
  event.preventDefault();

  const payload = collect();
  if (!payload.title) { App.showAlert(els.alert, '请填写标题', 'error'); return; }

  els.submitBtn.disabled = true;
  try {
    await uploadIfNeeded();
    payload.imageUrl = els.imageUrl.value.trim();

    if (isEdit) await API.put('/api/items/' + encodeURIComponent(editId), payload);
    else await API.post('/api/items', payload);
  } catch (err) {
    App.showAlert(els.alert, err.message, 'error');
    els.submitBtn.disabled = false;
  }
});
```

列表和详情页展示图片（`public/js/index.js` / `item.js`）：

```js
// 列表缩略图：固定高度 + object-fit: cover，保证卡片高度整齐
const thumb = item.imageUrl
  ? '<img class="thumb" src="' + App.esc(item.imageUrl) + '" alt="' + App.esc(item.title) + '" loading="lazy">'
  : '';

// 详情页大图：object-fit: contain，整张图都能看到
const image = item.imageUrl
  ? '<img class="detail-image" src="' + App.esc(item.imageUrl) + '" alt="' + App.esc(item.title) + '" loading="lazy">'
  : '';
```

```css
.item-card .thumb { width: 100%; height: 152px; object-fit: cover; border-radius: 10px; margin-bottom: 10px; }
.detail-image { width: 100%; max-height: 420px; object-fit: contain; border-radius: 12px; margin-bottom: 16px; }
```

### 7.5 安全要点

| 风险 | 处理方式 |
| --- | --- |
| 上传可执行脚本（webshell） | 按文件头嗅探真实类型，只有 4 种图片能通过；把 `shell.php` 改名成 `.png` 依然被拒 |
| 路径穿越（`../../evil.png`） | 客户端文件名一律丢弃，落盘名由服务端随机生成；`O_EXCL` 保证不覆盖已有文件 |
| SVG 携带脚本 | 不在白名单里，一律拒绝 |
| 超大文件打满磁盘 / 内存 | 请求体 `MaxBytesReader` + 文件头 `Size` 预检 + 拷贝时 `LimitReader`；multipart 超过 2MB 自动落临时文件 |
| 图片地址被写成 `javascript:` / `data:` | `validate.ImageURL` 白名单：只接受 `/uploads/...` 或 `http(s)://` |
| 上传接口被匿名滥用 | `RequireAuth` 中间件，未登录一律 401 |

**已知限制**：删除信息或更换图片时不会自动清理旧文件（没有建立「谁上传了哪个文件」的映射）。
如需回收，可以再加一张 `uploads` 表记录 `filename / user_id / item_id / created_at`，
在删除信息时按 `item_id` 清理，并加一个定时任务删除超过一定时间仍未被引用的孤儿文件。

### 7.6 Node 版实现

Node 版提供了完全等价的功能，且没有引入 Express 之外的依赖：

| 文件 | 作用 |
| --- | --- |
| `src/utils/multipart.js` | 自写的极简 multipart 解析器（RFC 7578 常见子集），全程在 Buffer 上操作、二进制安全 |
| `src/services/uploads.js` | 文件头嗅探 + 随机文件名 + 落盘，与 Go 的 `internal/upload` 一一对应 |
| `src/routes/uploads.js` | `POST /api/uploads`；超过上限时先排空请求再回 413，避免客户端只看到 ECONNRESET |

两个坑值得记一下：

1. Express 4 **不会**捕获 async 处理器里抛出的异常，必须自己 `.catch(next)`，否则会变成
   unhandledRejection 直接把进程带崩；
2. 上传超限时如果立刻 `req.destroy()`，客户端拿到的只有 `ECONNRESET` 而不是 413 ——
   正确做法是停止缓冲、继续把剩余数据排空，等请求结束再回 413。

---

## 八、验证工具

`tools/` 下的脚本，用来验证用户模块与两版共库的行为。

### 8.1 用户模块接口测试

覆盖注册（学号唯一、8 位数字格式校验）、登录、当前用户、登出、鉴权中间件、资料与改密、与失物信息的关联、后台用户列表，共 59 项断言。

```powershell
# 全新库：直接起服务即可（会跳过依赖演示账号的 2 项断言）
Remove-Item .gotmp\user-test.db* -ErrorAction SilentlyContinue
$env:DB_FILE='E:\test\.gotmp\user-test.db'; $env:PORT=3301; .\go-backend\bin\lostfound.exe
node tools\user-module-test.cjs http://127.0.0.1:3301

# 历史库：先用 Node 版建一个旧结构的库，再让 Go 版启动并自动迁移
Remove-Item .gotmp\old.db* -ErrorAction SilentlyContinue
$env:DB_FILE='E:\test\.gotmp\old.db'; node scripts\seed.js
$env:DB_FILE='E:\test\.gotmp\old.db'; $env:PORT=3303; .\go-backend\bin\lostfound.exe
node tools\user-module-test.cjs http://127.0.0.1:3303
```

### 8.2 失物 / 招领 CRUD 与权限矩阵

覆盖五个操作的正常路径与完整权限矩阵（匿名 / 他人 / 本人 / 管理员），共 60 项断言。

```powershell
Remove-Item .gotmp\crud.db* -ErrorAction SilentlyContinue
$env:DB_FILE='E:\test\.gotmp\crud.db'; node scripts\seed.js
$env:DB_FILE='E:\test\.gotmp\crud.db'; $env:PORT=3600; .\go-backend\bin\lostfound.exe
node tools\item-crud-test.cjs http://127.0.0.1:3600
```

### 8.3 搜索、排序与分页

覆盖关键词 LIKE、8 个排序字段 × 2 个方向、分页边界、参数校验与注入防护，共 46 项断言。
同一份脚本可以直接跑在两版后端上做对比。

```powershell
Remove-Item .gotmp\search.db* -ErrorAction SilentlyContinue
$env:DB_FILE='E:\test\.gotmp\search.db'; node scripts\seed.js
$env:DB_FILE='E:\test\.gotmp\search.db'; $env:PORT=3700; .\go-backend\bin\lostfound.exe
node tools\item-search-test.cjs http://127.0.0.1:3700

# 换成 Node 版跑同一套断言
$env:DB_FILE='E:\test\.gotmp\search.db'; $env:PORT=3701; node server.js
node tools\item-search-test.cjs http://127.0.0.1:3701
```

### 8.4 状态管理与迁移

状态管理接口测试（36 项断言，两版后端都能跑）：

```powershell
Remove-Item .gotmp\status.db* -ErrorAction SilentlyContinue
$env:DB_FILE='E:\test\.gotmp\status.db'; node scripts\seed.js
$env:DB_FILE='E:\test\.gotmp\status.db'; $env:PORT=3800; .\go-backend\bin\lostfound.exe
node tools\item-status-test.cjs http://127.0.0.1:3800
```

迁移验证：先造一个「升级前」的两态库，再让新版后端启动并自动迁移。

```powershell
node tools\make-legacy-db.cjs .gotmp\legacy.db     # 生成旧结构库（3 条数据）
$env:DB_FILE='E:\test\.gotmp\legacy.db'; $env:PORT=3810; .\go-backend\bin\lostfound.exe
node tools\check-migration.cjs .gotmp\legacy.db    # 校验 CHECK / 数据 / 索引 / 外键
node tools\db-inspect.cjs .gotmp\legacy.db         # 查看表结构、索引与迁移版本
```

`tools/make-legacy-db.cjs` 造出来的库会先自我验证一句 `UPDATE items SET status='found'` 被拒绝，
确认 CHECK 约束确实还是旧的两态。

### 8.5 图片上传

覆盖上传、格式嗅探、路径安全、大小限制、落盘与静态访问，共 35 项断言。两版后端都能跑，
测试结束后会清掉自己产生的图片文件。

```powershell
Remove-Item .gotmp\upload.db* -ErrorAction SilentlyContinue
$env:DB_FILE='E:\test\.gotmp\upload.db'; node scripts\seed.js
$env:DB_FILE='E:\test\.gotmp\upload.db'; $env:PORT=3900; .\go-backend\bin\lostfound.exe
node tools\upload-test.cjs http://127.0.0.1:3900
```

### 8.6 查看库结构

```powershell
node tools\db-inspect.cjs                        # 默认 data/lostfound.db
node tools\db-inspect.cjs .gotmp\old.db          # 指定文件
```

输出各表的列、行数、索引与已应用的迁移版本，用来确认 `student_id` / `name` 是否补上、回填是否正确。

### 8.7 两版契约对比

注意：**必须准备两份内容相同但互相独立的库**，因为脚本会依次向两个后端发起同一套写操作。

```powershell
# 1) 准备两份相同的种子数据库
Remove-Item .gotmp\*.db* -ErrorAction SilentlyContinue
$env:DB_FILE='E:\test\.gotmp\node-test.db'; node scripts\seed.js
Copy-Item .gotmp\node-test.db .gotmp\go-test.db

# 2) 分别启动两版
$env:DB_FILE='E:\test\.gotmp\node-test.db'; $env:PORT=3100; node server.js          # 终端 1
$env:DB_FILE='E:\test\.gotmp\go-test.db';   $env:PORT=3101; .\go-backend\bin\lostfound.exe  # 终端 2

# 3) 跑 126 项请求并逐条比对
node tools\contract-parity.cjs http://127.0.0.1:3100 http://127.0.0.1:3101
```

脚本会抹平用户模块改造带来的四类**刻意差异**，只把真正的回归报成 `DIFF`：
`studentId` / `name` 两个新增键、`storagePlace`（寄放处，见 [第五节](#五与-node-版的一致性)）、
「用户名→学号 / 昵称→姓名」的文案改名、学号规则（Go 要求恰好 8 位数字，Node 仍沿用旧的用户名规则）。

### 8.8 交叉兼容

让两版同时指向**同一个**数据库，验证会话、口令散列、学号回退与数据写入的互认：

```powershell
$env:DB_FILE='E:\test\.gotmp\shared.db'; node scripts\seed.js
$env:DB_FILE='E:\test\.gotmp\shared.db'; $env:PORT=3200; node server.js
$env:DB_FILE='E:\test\.gotmp\shared.db'; $env:PORT=3201; .\go-backend\bin\lostfound.exe
node tools\cross-compat.cjs http://127.0.0.1:3201 http://127.0.0.1:3200
```

### 8.9 寄放处（`storagePlace`）

覆盖发布时写入、详情读回、`PATCH` 局部更新、`PUT` 整体替换、长度与类型校验、
列表与「我的发布」里是否带该字段、过审后第三方是否可见，共 21 项断言。

```powershell
Remove-Item .gotmp\place.db* -ErrorAction SilentlyContinue
$env:DB_FILE='E:\test\.gotmp\place.db'; node scripts\seed.js
$env:DB_FILE='E:\test\.gotmp\place.db'; $env:PORT=3800; .\go-backend\bin\lostfound.exe
node tools\storage-place-test.cjs http://127.0.0.1:3800
```

再给第二个参数就会附带跑一段 Node 版对照（6 项），验证 Node 版**忽略但不报错**：

```powershell
node tools\storage-place-test.cjs http://127.0.0.1:3800 http://127.0.0.1:3801
```

测试结束后会删掉自己创建的条目。

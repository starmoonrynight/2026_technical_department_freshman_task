# 失物招领系统

杭电校园失物招领平台。同学自愿发布「我丢了什么」（lost）和「我捡到什么」（found），
平台负责**搜索、自动匹配、通知、记录**，双方通过帖子里的联系方式**自行线下沟通和归还**。

> 完整设计文档见 `C:\Users\TestUser\.qoder-cn\plans\patient-mountain-wolf.md`（第 6 版·定稿，16 节）。
> 这份 README 只讲**怎么跑起来**。设计上的「为什么」全在计划里，别在这里重复。

---

## 技术栈

| 项 | 选择 |
|---|---|
| 后端 | Go + Gin |
| 数据库 | PostgreSQL 16（Docker Compose） |
| 认证 | 本地账号密码（bcrypt + JWT）；杭电助手 OAuth2+PKCE 为 M8 待接入 |
| 前端 | React + Vite（M7 才创建） |
| 图片 | 存本地 `backend/uploads/`，DB 只存路径 |

---

## 目录结构

```
失物招领系统/
├── docs/                        # 杭电助手 SSO 调研文档（M8 的唯一权威依据，不要覆盖）
├── docker-compose.yml           # postgres + adminer，只跑这两个
├── scripts/
│   ├── setup-postgres.sh        # 原生 Windows PostgreSQL 的初始化脚本
│   ├── smoke.sh                 # 冒烟脚本的运行器：选节、补依赖、跑前置检查
│   └── smoke/                   # 判据本体，一个里程碑一个文件 + lib.sh
└── backend/
    ├── cmd/server/main.go       # 唯一入口，可通读
    ├── migrations/              # golang-migrate 迁移文件，程序启动时自动应用
    ├── uploads/                 # 图片（git 忽略，只留 .gitkeep）
    ├── smoketest/               # 端到端冒烟测试（需要真实测试库）
    └── internal/
        ├── config/              # .env → Config 结构体，无全局变量
        ├── database/            # pgxpool + 自动迁移 + SQL tracer
        ├── apperr/              # 错误码 + Error 类型 + 统一响应信封
        ├── middleware/          # RequestID、访问日志、panic 恢复
        ├── handler/             # HTTP 层：只做翻译，不写 SQL、不写业务 if
        ├── service/             # 业务规则（M2 起）
        ├── repo/                # 所有 SQL 只出现在这一层（M2 起）
        ├── matcher/             # 匹配算法，纯函数（M3 起）
        ├── auth/                # Provider 接口 + local + hduhelp（M1 起）
        └── router/              # 全部路由注册在这一个文件
```

**分层规则（只有三层）**：`handler` → `service` → `repo`。
依赖全部在 `main.go` 里手工按顺序 new，**不用 wire/fx** —— 生成代码和隐式魔法对初学者是调试黑洞。

---

## 跑起来

### 1. 起数据库

两条路，选一条。**本机走 B**（Docker daemon 起不来，见下面「常见坑」）。

#### 路径 A：Docker Compose（计划 §11 定的方案）

```bash
docker compose up -d
```

会拉起两个容器：

| 服务 | 端口 | 用途 |
|---|---|---|
| `postgres:16-alpine` | 5432 | 数据库，账号密码都是 `lf`，库名 `lostfound` |
| `adminer:latest` | 8081 | 浏览器里的数据库管理界面 |

**Adminer 是看表、改数据、试 SQL 的主工具。** 打开 <http://localhost:8081>，登录时：

- 系统：PostgreSQL
- 服务器：**`postgres`**（⚠ 不是 `localhost` —— Adminer 在容器里，`localhost` 指的是它自己）
- 用户名 `lf` / 密码 `lf` / 数据库 `lostfound`

#### 路径 B：原生 Windows PostgreSQL（本机在用）

```bash
scoop install postgresql        # PG 18.6，用户级安装，不需要管理员权限
bash scripts/setup-postgres.sh  # 建 lf 角色 + lostfound 库，并预检 pg_trgm
```

原生包只带一个 `postgres` 超级用户、空密码，而 `.env` 和 compose 用的都是 `lf/lf/lostfound`
这套约定。`setup-postgres.sh` 负责把两边对齐，**幂等，重复跑没副作用**。它同时会：

- `pg_ctl start` 起服务（不需要注册 Windows 服务，也就不需要管理员权限）
- 建 `lf` 角色，给 `SUPERUSER`（迁移第一句 `CREATE EXTENSION pg_trgm` 要用）
  和 `CREATEDB`（测试代码要 `CREATE DATABASE lostfound_test`）
- **预检 `pg_trgm` 是否随包提供** —— 这是 PG 18 原生包和 `postgres:16-alpine` 之间
  唯一的真实差异点，缺了的话中文子串搜索的 GIN 索引建不出来

图形界面用**自带的 pgAdmin 4**（开始菜单搜 pgAdmin），它承担的就是计划里 Adminer 的角色。
第一次打开要设一个 pgAdmin 自己的主密码（和数据库无关），然后 Register → Server：
Host 填 **`127.0.0.1`**（不是 `postgres`，那是 compose 里的服务名，只在容器网络里有意义），
用户名 `lf`，密码 `lf`。

停 / 起服务：

```bash
pg_ctl stop                                        # PGDATA 已在环境变量里
pg_ctl -l "$PGDATA/postgres.log" start
```

> **PG 18 而不是 16，有影响吗？** 没有。本项目用到的全是标准特性：`BIGSERIAL`、
> `timestamptz`、`CHECK` 约束、部分唯一索引、`JSONB`、`GIN + gin_trgm_ops`、自引用外键、`setval`。
> `docker-compose.yml` 保留在仓库里，换到有 Docker 的机器上直接可用。

### 2. 配环境变量

```bash
cd backend
cp .env.example .env
```

然后编辑 `.env`，**只需要填一个东西**：

```
JWT_SECRET=<一串随机字符>
```

Git Bash 里生成：

```bash
openssl rand -hex 32
```

`JWT_SECRET` 必填且没有默认值 —— 给个默认值等于给所有部署实例同一把钥匙。
缺了程序会立刻 fatal 并打印缺哪个变量，不会带着半残的配置继续跑。

`.env` 已在 `.gitignore` 里，不会被提交。

### 3. 起后端

```bash
go run ./cmd/server
```

启动时会**自动把数据库迁移到最新版本**（`internal/database` 用 golang-migrate 库 + `//go:embed`
把 `migrations/*.sql` 编进二进制）。所以这一条命令同时完成了建表 + 种子数据 + 起服务，
**本机不需要装 psql、createdb 或任何迁移工具**。

看到 `server.listening` 就成了。验证：

```bash
curl localhost:8080/api/health
# {"code":"OK","message":"ok","data":{"status":"ok","db":"ok",...},"request_id":"..."}
```

### 4. 跑测试

```bash
cd backend

# 纯单元测试（不需要数据库，任何机器上都能跑）
go test ./internal/...

# 集成测试（需要数据库）
TEST_DB_DSN="postgres://lf:lf@127.0.0.1:5432/postgres?sslmode=disable" \
REQUIRE_INTEGRATION=1 go test ./...
```

⚠ 用 `127.0.0.1` 而不是 `localhost`：Windows 上 `localhost` 可能先解析成 IPv6 的 `::1`，
而 PostgreSQL 不一定在监听它，报出来的错会很难往这上面想。

`TEST_DB_DSN` 指向**默认库 `postgres`**，测试代码会自己 `CREATE DATABASE lostfound_test`
（用 Go 代码建库，绕开本机没有 createdb），然后迁移 + `TRUNCATE`，测试之间零串扰。

⚠ **`REQUIRE_INTEGRATION=1` 的意义**：`TEST_DB_DSN` 为空时集成测试会打印醒目 banner 并**跳过**，
这样 `go test ./...` 在任何机器上都不会红。但「全绿」可能意味着一条集成测试都没跑 ——
所以最终验收必须带这个变量，带了之后「DSN 为空」就直接算**失败**，不是跳过。

### 5. 手工冒烟

后端跑着的时候，另开一个终端：

```bash
bash scripts/smoke.sh            # 全部里程碑的判据都跑一遍
bash scripts/smoke.sh m5         # 只跑 M5（归还确认 · 积分）
bash scripts/smoke.sh m3         # 跑 M3，运行器自动补上它依赖的 m1 m2
bash scripts/smoke.sh --list     # 看有哪些节、谁依赖谁
```

判据本体在 `scripts/smoke/` 下，一个里程碑一个文件（`lib.sh` 是公共工具，不含任何判据）：

| 文件 | 覆盖的端点 | 依赖 |
|---|---|---|
| `smoke/m0-baseline.sh` | #38 健康检查、信封契约、RequestID 防线 | 无 |
| `smoke/m1-auth.sh` | #1–#5 + 封禁 | 无 |
| `smoke/m2-items.sh` | #6–#8、#13–#19、#40、#42 | m1 |
| `smoke/m3-match.sh` | #20 | m1 m2 |
| `smoke/m4-notify.sh` | #21、#22、#30–#32、#41 | m1 m2 m3 |
| `smoke/m5-return.sh` | #23–#29、#33 | 无（自包含） |

⚠ **为什么大部分节不能单独跑**：它们要借用前面节建出来的用户和帖子
（`$TOKEN`、`$MY_ID`、`$LOC_ID` 这些全局变量），所以必须跟着前面的节一起跑。
这种借依赖写在每个文件开头的 `# deps:` 一行里，运行器照它自动补齐 ——
宁可多跑几节，也不要因为 `$TOKEN` 是空的而报一堆假的 401。
只有 `m5` 是自己注册五个用户、自己发帖、自己传图的，所以只有它真的能单跑。
**这条自包含不是为了方便，是为了断言的诚实**：上一版 M5 借的是 M3/M4 留下的数据，
于是「拾主手上有几条通知」实际测的是「上一节跑出了什么」，改 M4 会让 M5 假红。

它用 curl 打一遍全链路，只断言 `code` 字段，**从不断言 message 文本**
（message 是给人看的中文，随时可改；code 是稳定的机器码，永不改语义）。

有几条判据在接口上看不出来，必须直接查库 —— 比如「校验失败了却还是把帖子插进去」：
每条错误码都对，但库里已经堆了一堆不该存在的行。这些步骤会自己找本机 psql：

```bash
/d/Scoop/apps/postgresql/current/bin/psql.exe   # scoop 的原生安装不在 PATH 上
```

找不到就 SKIP（不算 FAIL，也不算 PASS —— 那样「全绿」会悄悄少测几条判据）。

---

## 调试

### 结构化日志

`LOG_LEVEL=debug` 重启，每条请求日志都带 `request_id`，且能看到 SQL 语句、参数、耗时、影响行数：

```json
{"time":"...","level":"DEBUG","msg":"sql.query","request_id":"20261007-3f2a...","sql":"SELECT ...","args":[7],"command_tag":"SELECT 1","rows":1,"duration_ms":2}
```

**报 bug 时给一个 `request_id`，就能在日志里捞出完整链路。** 这是整套日志设计的全部目的。

日志字段固定为：`time, level, msg, request_id, user_id, method, path, status, duration_ms, err`。

### 其他调试入口

| 入口 | 用途 |
|---|---|
| `GET /api/health` | 服务和数据库是否活着 |
| `GET /api/debug/config` | 当前生效的配置（仅 `ENV=dev` + admin，敏感值打码）（M6） |
| `GET /api/items/:id/matches` 的 `breakdown` | 匹配调试器：每个信号的分数和权重都能验算（M3） |
| Adminer <http://localhost:8081>（路径 A）<br>pgAdmin 4（路径 B，开始菜单搜）<br>`psql -h 127.0.0.1 -U lf -d lostfound` | 直接看表、改数据、试 SQL |

> 计划 §13 的验收步骤写的是「在 Adminer 里跑 SQL」，指的其实就是这一行里的任意一个 ——
> 关键是**能手工执行 SQL 并看到结果**，用哪个工具无所谓。

### 错误响应

所有 JSON 响应都是同一个信封：

```json
{"code": "OK", "message": "ok", "data": { ... }, "request_id": "20261007-3f2a..."}
```

`code` 是稳定的机器码（定义在 `internal/apperr/codes.go`），`message` 是给人看的中文。
**前端和测试只允许 match `code`。** 未预期的错误一律返回 `INTERNAL`，原始错误只进日志、
不进响应体 —— 因为 pgx 的错误信息里可能带 SQL 片段和连接串。

---

## 迁移纪律

**已发布的迁移文件永远不改内容，只加新序号文件。**

`migrations/` 下是 `000001_xxx.up.sql` / `.down.sql`。改了已发布的文件会让
`schema_migrations` 里的校验和对不上，之后所有环境都迁移不了。

要回退（⚠ 会删数据）：`internal/database` 里有 `MigrateDown`，但 `main.go` **从不调用它** ——
它只给测试和「开发库被搞坏了」这种情况用。

---

## 当前进度

- **M0 地基 —— 已完成并验收（2026-10-07）**
  骨架、docker-compose、config、pgxpool、自动迁移、slog、RequestID/访问日志/恢复
  中间件、apperr + 信封、`/api/health`、路由注册测试。
  验收实测：迁移 version 1；12 张业务表 + `schema_migrations`；`categories` 55 行、
  `locations` 91 行；`pg_trgm` 1.6 + 13 个索引；`/api/health` 返回 `"code":"OK"`
  且 `"db":"ok"`；集成测试 18 项全绿、`scripts/smoke.sh` 16 项全绿。
- **M1 认证 —— 已完成并验收（2026-10-07）**
  §4 的 #1–#5 五个端点（注册 / 登录 / `GET,PUT /api/auth/me` / 改密码）、
  `model.User` + `repo.User`、`internal/auth`（Provider 接缝 + bcrypt 密码策略 +
  HS256 TokenSigner）、`middleware.JWT`（每请求回库读 role/status，封号即时生效）。
  验收实测：路由 6 条；纯单测（不连库）auth 59 + middleware 15 + router 7 +
  service 49 = 130 项全绿；集成测试 66 项全绿（含 M0 的 18 项）；
  `scripts/smoke.sh` **60 项全绿、0 跳过**，打的是真 dev server + 真开发库。
  判据链逐条走通：注册 → 登录 → 带 token 访问
  `/api/auth/me` → 错 token 得 `UNAUTHORIZED` → 重名得 `USER_ALREADY_EXISTS`
  → 弱密码得 `WEAK_PASSWORD` → banned 用户登录得 `USER_BANNED`；
  `fakeProvider` 注入单测证明「JWT 签发与 Provider 无关」。
  另外实测了两条安全性质：账号枚举的**计时**侧信道已抹平（用户名不存在 48.0ms
  vs 密码错 48.0ms，靠 dummy bcrypt 比对补齐），**响应与日志**两条路径也同形
  （同 code、同 message、访问日志里同样不带 `err` 字段）。
- **M2 字典·物品·上传 —— 已完成并验收（2026-10-07）**
  §4 的 #6–#8、#13–#19、#40、#42 共 12 条新路由（累计 18 条 / 最终 50 条）：
  图片上传（`crypto/rand` 文件名 + magic bytes 嗅探，**不看扩展名和 Content-Type**）、
  静态文件服务、分类/地点两棵只读字典树、发帖 / 广场分页筛选 / 详情（`OptionalJWT`）/
  改帖 / 软删 / 关帖 / 我的发布 / **帖主自删单张图片**。
  验收实测：纯单测（不连库）auth 59 + middleware 15 + router 7 + service 239 +
  repo 42 + model 5 = **367 项全绿**；集成测试 `smoketest` **350 项全绿**
  （106 个顶层测试函数）；`scripts/smoke.sh` **197 项全绿、0 跳过**。
  几条只有测了才成立的性质：
  - `contact` **只校验非空，不校验内容** —— 空串、纯半角空格、全角空格 `U+3000` 都得
    `VALIDATION`；填「无」「问宿舍阿姨」都返回成功。这是原则 2 的正面证据，
    只测拒绝不测接受的话，「把所有 contact 都拒掉」的实现也能全绿。
  - 时间语义由 **DB 的 `CHECK`** 兜底：`last_seen_at > lost_at` 和「found 帖填了
    `last_seen_at`」两条，既在 API 层测了 `VALIDATION`，也绕过 service 直接 `INSERT`
    测了 SQLSTATE 23514 + 约束名 `items_time_semantics`。
    CHECK 守的是那些不经过我们代码的写入（pgAdmin 手改、将来的迁移），没测过的防线
    和没有防线看不出区别。
  - `found` 帖的 `contact` 在**广场和详情页都是 `null`**，包括作者本人看自己的 ——
    #14/#15 因此不需要身份，可以缓存。想看自己的联系方式只有 #19。
  - #19 的 `user_id` 来自 JWT，**不是查询参数**；`?user_id=别人` 会被忽略。
  - `keyword` 走 `ILIKE ... ESCAPE '\'`，搜 `100%` 时那个 `%` 是字面量。
  - `sort` 过两张白名单（service 决定用户能请求什么，repo 决定什么能拼进 SQL），
    `sort=created_at;DROP TABLE items` 得 `VALIDATION` 而不是 `500`。
  - #42 只有**帖主本人**能删，别人和 **admin** 都得 `FORBIDDEN`（admin 删图是 #45）。
    删完 `item_images` 少一行、磁盘文件消失、帖子和其余图片都在，
    封面由 #14 的 `LEFT JOIN LATERAL ... ORDER BY sort_order LIMIT 1` 现算，不需要「重算封面」的代码。
  - admin 改别人的帖**必须带 `admin_reason`**，且改不动作者 —— 归属是改不了的（原则 5）。
- **M3 匹配 —— 已完成并验收（2026-10-07）**
  §4 的 #20 一条新路由（累计 19 条 / 最终 50 条）+ `internal/matcher`
  （字符 bigram + Dice 相似度、Tier1/Tier2 两套加权打分）。
  ⚠ **对计划 §12 的一处更正**：那一节把 M3 写成「#20、#44 两条端点」，可 `#44` 是
  §4 里的「批量恢复」（M6 的治理端点），匹配功能从头到尾**只有 #20 一条对外端点**——
  「建 found 帖时同步匹配并通知」不是路由，是 `POST /api/items`（#13）内部的一步，
  改帖重匹配同理是 #16 内部的一步（对外只有 `match.run` 日志里的
  `trigger=found_created` / `found_updated` 可观测）。
  §4 那张 50 行的表是权威，端点数一律从它数，不从里程碑小结倒推。
  验收实测：纯单测（不连库）auth 59 + middleware 15 + router 7 + service 305 +
  repo 42 + model 5 + matcher 53 = **481 项全绿**（另有 `router` 里 1 项
  `TestFinalRouteCount` **按设计 SKIP**，凑满 50 条路由那天才启用）；
  集成测试 `smoketest` **366 项全绿**（112 个顶层测试函数）；
  `scripts/smoke.sh` **247 项全绿、0 跳过**。
  几条只有测了才成立的性质：
  - 通知是**不对称**的：只有 **found 帖这一侧**（创建和改帖）会写 `match_pairs` 并给
    lost 作者发 `new_match`；lost 帖无论是创建还是改帖都只算不写（创建时响应里只有
    `matches_preview`，**压根不出现 `notified_count` 这个键**）。冒烟脚本两头都测了。
  - `notified_count` 和 `matches_preview` 用了 `*int` / `*[]MatchHit`，
    为的是「0 也要出现」：`notified_count:0` 是去重的证据，
    `matches_preview:[]` 是「没匹配上」的证据 —— 用值类型的话这两个都会变成
    字段消失，而字段消失和「没匹配上」在客户端看不出区别。
  - Tier1 三条信号（cat 0.45 / text 0.40 / time 0.15）**权重和必须为 1**，
    且 `Σ(w×s)` 恰等于外层 `score`；断言是按键集合相等做的，
    多一条 `attr` 也会红 —— 计划里删掉的东西不能阴魂不散。
  - **Tier2 永不写台账**：测的是一对自由地点、总分 0.82（> 通知阈值 0.75）的帖子，
    依然 0 行。用低分配对测不出这条规则，只有高分还拒绝才证明「是 tier 决定的」。
  - #20 **只读**：连点两次 GET，台账和通知数量一行都不涨；错参数（`tier=3`、
    `limit=abc` 等 6 种）全得 `VALIDATION`；`user_id` 来自 JWT，别人的帖得 403。
  - #20 的 `contact` 策略与 #14 完全一致（found 帖一律 `null`），匹配列表
    **绕不过** M4 的解锁与审计。
  - 台账的去重就是 `UNIQUE(lost_item_id, found_item_id)` + `ON CONFLICT DO NOTHING`，
    没有 `notified` 布尔列。
  - **对计划的一处修订（2026-10-07，已与用户确认）**：判据 ③ 原文是「重复建同样的
    found 帖 → `ON CONFLICT` 生效、台账仍一行」，这条**按字面是做不到也不该做的** ——
    再发一次帖拿到的是新的 `item id`，台账的键是「一对帖子」，那是**另一件拾获物**，
    该各发一条通知（和定位原则 ③「认领非排他」同一个逻辑）。真正会让同一对第二次
    参与匹配的动作是**改帖**，而 §5.8 的触发源表里根本没有「编辑」这一行 ——
    计划前后不一致，于是那条 SQL 无处可达，还留下一个功能洞：小王把「捡到钱包 / 地点其他」
    改成「黑色长款钱包 / 图书馆」之后系统不会重算，小李永远等不到通知。
    现在 `Match.OnUpdated` 补上了这条路径（§9 的 `trigger` 多一个值 `found_updated`），
    判据 ③ 由三条测试分担：改帖→再改帖（走 HTTP，验 `ON CONFLICT`）、
    repo 直连两次写同一对（验那条 SQL 本身 + 混合批次旧对跳过新对照发）、
    同一 lost 被两条不同 found 命中（验「不去重才是对的」，防止有人把键改成 `lost_item_id`）。
    `OnUpdated` 还有两道「不跑」的门槛，都用**全表**行数而不是按 id 数来断言 ——
    按 `lost_item_id` 数的话，方向写反的脏行会躲过去：
    lost 帖改帖不跑（不对称是方向的规则，不是发帖的规则）、
    `closed` 的 found 帖改帖不跑（东西已经还回去了，再推「可能有匹配」是在骗一个寻找已结束的人）。
    这两条门槛都用「故意改坏实现」验过：去掉哪一道，对应的断言都会红。
  - 两个**只有真 PostgreSQL 才能发现**的坑：`make_interval(hours => $n)` 的
    `hours` 是 integer，传 float8 报的是「函数不存在」（SQLSTATE 42883），
    得用 `secs`；不带类型的锚点参数会被猜成 interval，整条比较变成
    `timestamptz <= interval`，必须写 `$n::timestamptz` —— 而且转型只加在参数上，
    列名一旦包上函数，索引就用不上了。
  - 匹配把 `"item":{"id":…}` 也塞进了 `matches_preview`，于是冒烟脚本里
    取新建帖 id 的 `jval '"item":\{"id":([0-9]+)'` 贪婪匹配到了**候选帖**的 id，
    级联出 14 个 M2 FAIL（「只能操作自己发布的帖子」）。换成锚定行首的
    `itemIDOf()` 才恢复 —— 这不是 M3 的功能 bug，是 M3 让一个既有断言变松了。
- **M4 联系方式解锁 · 通知 · 举报 —— 已完成并验收（2026-10-07）**
  §4 的 #21、#22、#30、#31、#32、#41 共 6 条新路由（累计 25 条 / 最终 50 条）：
  解锁 found 帖的联系方式并留下审计行、发帖人查「谁来看过」、收件箱三件套
  （列表 / 未读数 / 标已读）、用户举报。
  验收实测：纯单测（不连库）auth 59 + middleware 15 + router 7 + service **397** +
  repo 42 + model 5 + matcher 53 = **578 项全绿**（`TestFinalRouteCount` 仍按设计 SKIP）；
  集成测试 `smoketest` **431 项全绿**（139 个顶层测试函数）；
  `scripts/smoke.sh` **330 项全绿、0 跳过**。
  §12 那两条判据链各自走通了一遍，几条只有测了才成立的性质：
  - **认领不是排他锁**（定位原则 3 的原话是「不需要设一个人认领其他人不允许查看的规则」）：
    A 解锁完，B 照样解得开，`contact_views` 变成两行而不是一行。这条是 M4 里最容易被
    后续改动悄悄推翻的性质 —— 谁给这张表加一个 `status` 列、或者在 `Unlock` 里加一句
    「已经有人认领就返回 409」，前面几条判据全都还是绿的，**只有这一条会红**。
  - #21 的判据顺序不能换：deleted → `NOT_FOUND` 排在类型和状态判断前面，lost →
    `VALIDATION`，**作者本人直接拿到联系方式且一行都不写**，closed → `ITEM_CLOSED`，
    最后才是 `repo.Unlock`（`ON CONFLICT DO NOTHING`）。幂等那一步返回的是**第一次**
    解锁的 `created_at`，不是「再记一次」—— 这张表说的是「他最早什么时候看过」。
  - `contact_unlocked` 这个通知类型在第 4 版就被删了，所以 #21 **写零条通知**：
    断言的是解锁者、另一个失主、拾主**三个人**的通知数都不涨（只盯解锁者本人的话，
    「平台给拾主发了一条『有人看了你的联系方式』」这种实现正好漏掉）。
  - #22 只有发帖人和 admin 读得到；**解锁过名单但不是作者的人一样 `FORBIDDEN`**。
    `real_name` 是全项目唯一一次进对外响应（#15 的 `AuthorView` 刻意没有它），
    所以它在 M8（SSO）之前一直是空串，但**键必须在** —— 前端按它渲染一行，
    「接口漏字段」和「本来没数据」是两种不同的故障。
  - 收件箱的归属只认 token：`?user_id=别人` 被忽略（忽略之后仍然只能看到自己的），
    `all=true` 只清自己的未读、一条都碰不到别人，别人的 id 混进批次里 → `FORBIDDEN`
    且**一条都没改**（这两条都在真库上回头核对过，不是只看错误码）。
    `updated_count` 数的是「真的改了几行」，第二次点同一条是 0；已读不可逆，
    「标回未读」这个端点**刻意不存在**（`POST /api/my/notifications` 得 405、
    `PUT /unread` 得 404，四个探测写完 notifications 表一行没多）。
    `item_id` / `return_id` 是指针：admin_action 那类不挂帖子的通知必须回 `"item_id":null`，
    键消失和值为 null 前端分不出来。
  - **#41 举报的关键断言是「它什么都改变不了」**：被举报人零通知、帖子 `status` 没变、
    广场那一页的 id 顺序一条没变、双方信用分没动、而且**库里那一行的 status 真的是
    open**（响应里的 `status` 是拼出来的常量，只有读回那一行才能证明平台没替 admin 表态）。
    重复举报 `REPORT_DUPLICATE`，**换一个 reason_code 也算重复**（管的是「同人对同帖的
    待处理举报」），换一个用户可以（多人举报是处理优先级）；表外的 code 在 service 的
    白名单就拒掉，是 `VALIDATION` 而不是撞 DB 的 `CHECK_VIOLATION`；M4 没有任何
    读 `reports` 的端点 —— 加了就等于让被举报人能反查「谁举报了我」。
  - 三条禁令是**靠接口形状实现**的，不是靠注释：`ContactStore` 2 个方法、
    `ReportStore` 1 个、`ItemLookup` 1 个、`NotificationStore` 4 个全只读，
    「顺手把帖子删了」「给被举报人发一条通知」这类代码在这些文件里**编译不出来**。
    `TestM4ServicesCannotWriteNotifications` 用 `reflect.NumMethod` 钉住方法数，
    报错文案直接指向 §3.7 那三行禁令。
  - #15 的两段式闸门（纯函数 `contactLocked` + 一次带索引的 `Viewed()` 点查）在
    数据库出错时 **fail closed**：宁可锁着也不泄漏。这条用「故意改坏实现」验过 ——
    把那行的 `return true` 改成 `return false`，对应单测立刻红。
  - `reason_code` / `detail` 两侧空白会被削掉，而**削完的那一份才是进库的那一份**。
    第①层用 fake store 钉了「交给 repo 的字符串是 trim 过的」，第③层在真库上再验一次：
    少了这一步那个 trim 只存在于单元测试的想象里，真实结果是带空格的 `" spam "`
    撞 `reports_reason_code_check` 的 23514，用户收到的是 `INTERNAL`。
  - ⚠ 两处**测试自己写错**、产品没问题的红，记下来是因为它们的形状很有迷惑性：
    ① 第②层一开始把 `"spam "` 当成非法值断言 `VALIDATION`，红了之后才看清是
    service 的 trim 在放行 —— 第①层早就把这条写成了合法行为；
    ② 冒烟脚本里 `req GET <path> <token>` 少了一个空请求体占位，token 被当成 body，
    请求实际是**匿名**发出去的（401），于是「没越权读到别人的东西」那条断言
    因为什么都没匹配到而**假绿**。M2 那节的 `?user_id=` 检查就是这样坏了一路的；
    现在两处都先断言「这个请求真的带着 token 拿到了 200」再谈有没有泄漏。
- **M5 归还确认 · 积分 —— 已完成并验收（2026-10-07）**
  §4 的 #23–#29、#33 共 8 条新路由（累计 33 条 / 最终 50 条）：
  对一条 found 帖提交归还（可带凭证图）、发帖人查详情、确认、拒绝、提交人撤销、
  「我提交的」/「我收到的」两个列表、积分流水。
  验收实测：纯单测（不连库）auth 59 + middleware 15 + router 7 + service **496** +
  repo 42 + model 5 + matcher 53 = **677 项全绿**（`TestFinalRouteCount` 仍按设计 SKIP，
  它报的「当前 33 条」是从 `expectedRoutes` 那张表数出来的，不是写死的字面量）；
  集成测试 `smoketest` **554 项全绿**（180 个顶层测试函数）；
  `scripts/smoke.sh` **447 项全绿、0 跳过**。
  §12 那条全链走通了一遍，几条只有测了才成立的性质：
  - **归属只能由发帖人产生**（定位原则 5「admin 能销毁内容和账号，但不能制造归属」）：
    #25/#26 对 admin 一律 `FORBIDDEN`，哪怕把他的 `role` 临时提成 admin ——
    替拾主点「确认收到」就是替他做归属判断。admin 改归属是 #46（M6），
    `ReviewKindAdminDataFix` 这个常量在 M5 **只声明、没有任何写路径**。
  - 撤销和「被审」是两件事：`cancelled` 那行的 `reviewer_id`、`review_kind` 都是 NULL，
    但 `reviewed_at` **会填** —— 它的实际含义是「这条记录退出 pending 的时刻」。
    这一条第③层一开始写反了（要求三个全 NULL），红了之后读 `repo/item_return.go` 的
    `Cancel` 才发现是**断言错、代码对**。
  - 被拒之后重新提交是**一条新记录**，不是把 rejected 那行改回 pending ——
    那个唯一索引只挡 pending（`item_id, submitter_id WHERE status='pending'`），
    所以同一个人对同一条帖可以有一条 rejected 和一条 pending 并存。
  - 通知是不对称的，而且撤销这一支**一条都不发**：确认 → 提交人 `return_confirmed`
    + 配对 lost 帖的作者各一条 `item_returned_hint`；拒绝 → 只通知提交人；
    撤销 → 谁都不通知。`item_returned_hint` 按**作者**去重：失主 A 有两条帖命中这条
    found 帖，他只收到 **1** 条 —— 少了这条断言，「同一人两帖发两条骚扰通知」的实现
    能一路绿到底。
  - 加分只发生在 confirm 那一支：发帖人 +10、提交人 +2，两条 `credit_logs` 都挂着
    这条归还确认（`ref_type='item_return'` + `ref_id`）；拒绝则**一行流水都不写**，
    两个人的分数还是 100。#33 存在的唯一理由是让 110 可解释，所以判据是
    **Σ流水 + 100 == credit_score**，而且是从**响应**里算出来的，不是从库里。
  - 提交归还**不需要**先解锁联系方式（§16 删掉了那道前置校验）—— 这一条专门测了：
    小刘一行 `contact_views` 都没写，仍然拿到了 200。
  - §13 第 10 步的审计不变式是全表级的：不能有「已终态却没有 `review_kind`」的行，
    也不能有「还 pending 却已经有 `review_kind`」的行。
  - ⚠ 第③层这次有**三条红是测试自己的错**，产品一行都没改，形状各不相同：
    ① 数**绝对值**而不是增量 —— 「拾主那边还是 1 条 `return_submitted`」借的是上一节
    留下的数据，而本节小刘提交了三次，每次都给拾主发一条，于是永远红；
    ② 上面那条 `reviewed_at`，代码对断言错；
    ③ 在整个响应体里搜 `"message"` 判「列表里不该有 message」—— 信封**自己**就有
    顶层的 `"message":"ok"`，这条判据会永远红。改成数 `"message":` 出现的次数
    （信封恒 1 次）。顺带记一个 bash 坑：`${BODY#*'"data":}` 这种在 `${}` 里放单引号的
    写法会让整份文件从那一行起解析错位，报出来的是「某变量: unbound variable」，
    和真实原因毫无关系 —— 剥信封这件事现在不做，改成计数。
- **冒烟脚本按里程碑拆开了**（M5 验收过程中做的，不是重构癖）：原来那份 1646 行、
  后来 2016 行的单文件没法只跑一节，而上面①②两条假红的根源恰恰是「跨节借数据」。
  现在是 `scripts/smoke.sh`（运行器：选节、按 `# deps:` 补依赖、跑前置检查）
  + `scripts/smoke/{lib.sh, m0…m5-*.sh}`。选节跑：`bash scripts/smoke.sh m5`；
  看依赖表：`--list`。
- **未开工**：
  M6 治理与管理后台 · M7 前端 · M8 杭电助手 SSO（外部阻塞中）

路由清单见 `internal/router/router.go` —— 那是 §4 全部 50 条路由的唯一注册点，
每个里程碑的占位注释都标了对应的端点编号。

---

## 常见坑

**`proxy.golang.org` 连不上。** 国内网络环境常见，报
`dial tcp ...: connectex: A connection attempt failed`。设成 goproxy：

```bash
go env -w GOPROXY=https://goproxy.cn,direct
```

**`docker compose up -d` 起不来。** 需要 Docker Desktop 或可用的 WSL2 发行版在跑。
`docker version` 只输出 Client 段、并报
`failed to connect to the docker API at npipe:////./pipe/docker_engine`，说明 daemon 没起来。
**本机就是这个状态**：装了 docker CLI 29.3.0（scoop），但没有 Docker Desktop，
`wsl -l -v` 也显示没有任何发行版，所以 daemon 无处可跑 —— 因此走上面的路径 B。

**Adminer / pgAdmin 连不上数据库。** 两边要填的 host 不一样，这是最容易搞混的一点：

| 工具 | Host 填 | 为什么 |
|---|---|---|
| Adminer（路径 A） | `postgres` | Adminer 自己在容器里，`localhost` 指的是它自己；`postgres` 是 compose 的服务名 |
| pgAdmin 4（路径 B） | `127.0.0.1` | pgAdmin 跑在 Windows 上，直接连本机端口 |

**scoop 装完东西，当前终端里找不到命令。** scoop 改的是**用户环境变量**，
只对「安装之后新开的终端」生效。装完 `postgresql` 却发现没有 `pg_ctl`，
关掉终端重开一个就行，不用怀疑安装失败。

**Git Bash 里用 curl 发中文，服务端收到的是问号。** 这条坑长得很像后端编码坏了，
所以值得单写一段。现象：

```bash
curl -X POST .../api/auth/register -d '{"nickname":"冒烟"}'   # 库里存成了 "??"
```

原因不在后端，也不在 curl，而在 **Git Bash 把参数交给 Windows 原生 exe 的那一刻**：
argv 要按当前代码页转换一次，本机是 `cp1252`，汉字不在里面，于是每个字变成一个 `?`。
请求在离开 shell 时就已经烂了。

改成从 stdin 喂给 curl 就没事了 —— 管道里的字节由 bash 直接写，不经过这次转换：

```bash
printf '%s' '{"nickname":"冒烟"}' | curl -X POST .../api/auth/register --data-binary @-
```

`scripts/smoke/lib.sh` 里的 `req()` 函数统一走 stdin，就是这个原因。
以后手写 curl 命令测中文时记得这一点，否则会误判成「后端不支持 UTF-8」。
顺带一个判据：**响应里的**中文一直是好的（stdout 不做这次转换），
所以「发出去变问号、收回来正常」这个组合就是 argv 的锅，不是数据库编码的锅。

**同一条坑还有三个变种**，都在这台机器上实测过：

| 中招的位置 | 症状 | 对策 |
|---|---|---|
| URL 里的查询参数（`?keyword=冒烟`） | 中文变 `???`，筛选「查不到」—— 而且**搜不到时的断言会假绿** | 冒烟脚本里一律先百分号编码 |
| `psql -c "…'中文'…"` | 写进库的是 `???`，或按中文匹配永远 0 行 | 脚本里的库级断言只数行数（`count(*)`），不比对中文字面量 |
| `curl -o 输出文件`（本仓库路径含中文） | 文件根本没被写出来，后续 `head` 报 no such file | 临时文件放 `mktemp -d`（纯 ASCII 路径） |

**Go 版本是 1.26.0，计划 §1.2 写的是 1.25.4。** 这是 `GOTOOLCHAIN=auto` 的正常行为：
scoop 装的是 go 1.25.4，但依赖里 `golang.org/x/text v0.42.0` 要求 `go >= 1.26.0`，
于是 Go 自己下载了 1.26.0 的工具链来编译。`go.mod` 里因此写的是 `go 1.26.0`。
**不要「修回」1.25.4**，会直接编译失败。

**`CREATE EXTENSION pg_trgm` 报权限不足。** 两条路径建出来的 `lf` 都是超级用户，正常情况下不会遇到。
`scripts/setup-postgres.sh` 会在建库之前先查 `pg_available_extensions`，扩展不存在就提前报错退出。
将来换托管库受限时，退路是去掉 trgm 索引，`ILIKE` 在小数据量下依然可用（只是慢）。

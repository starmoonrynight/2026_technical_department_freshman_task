#!/usr/bin/env bash
#
# 原生 Windows PostgreSQL 的一次性初始化脚本。
#
# 存在的理由：计划 §11 定的是 docker compose（postgres:16-alpine + Adminer），
# 但这台机器上 Docker daemon 起不来（没装 Docker Desktop，WSL 也没有任何发行版），
# 而且当前 shell 没有管理员权限，装不了。所以改用 scoop 的原生 Windows 版 PostgreSQL：
#
#   scoop install postgresql        # PG 18.6，用户级安装，不需要管理员
#
# 装完之后 compose 里那套「lf 超级用户 + lostfound 库」的约定不会自动出现 ——
# 原生包只建了一个 postgres 超级用户、空密码。这个脚本负责把两边对齐，
# 让同一份 backend/.env 在 Docker 路径和原生路径下都能用。
#
# 用法（Git Bash）：
#   scoop install postgresql
#   bash scripts/setup-postgres.sh
#
# 幂等：重复跑没有副作用，角色和库已存在就跳过。

set -uo pipefail

GREEN=$'\033[32m'; RED=$'\033[31m'; DIM=$'\033[2m'; RESET=$'\033[0m'
ok()   { printf '  %s✓%s %s\n' "$GREEN" "$RESET" "$1"; }
bad()  { printf '  %s✗%s %s\n' "$RED" "$RESET" "$1"; }
note() { printf '    %s%s%s\n' "$DIM" "$1" "$RESET"; }
step() { printf '\n%s== %s ==%s\n' "$DIM" "$1" "$RESET"; }

# 这些值必须和 backend/.env.example、docker-compose.yml 里的一致
DB_USER=lf
DB_PASSWORD=lf
DB_NAME=lostfound
PG_PORT="${PGPORT:-5432}"

export PGCLIENTENCODING=UTF8

# ---------------------------------------------------------------- 0. 找 postgres 二进制
step "0. 定位 PostgreSQL"

if ! command -v pg_ctl >/dev/null 2>&1; then
  bad "PATH 上找不到 pg_ctl"
  cat <<'EOF'

  两种可能：
    1. 还没装：  scoop install postgresql
    2. 装了但这个终端是安装之前开的 —— scoop 改的是用户环境变量，
       旧终端读不到。关掉重开一个 Git Bash 就行。

EOF
  exit 1
fi

PG_BIN="$(cd "$(dirname "$(command -v pg_ctl)")" && pwd)"
APP_DIR=""

# ⚠ 不能靠 pg_ctl 的位置反推安装目录：scoop 除了 manifest 里的 env_add_path（把
# <app>/bin 加进 PATH）之外，还会在 D:\Scoop\shims 里建同名 shim，而 shims 目录
# 通常排在 PATH 更前面。那样 dirname 出来的是 shims 目录，<root>/data 就成了
# D:\Scoop\data —— 错的，而且报出来的「数据目录不存在」会让人往完全错误的方向查。
# 所以先用 scoop prefix 拿权威路径，拿不到再退回 pg_ctl 的位置。
if APP_DIR="$(scoop prefix postgresql 2>/dev/null)" && [[ -d "$APP_DIR" ]]; then
  PG_BIN="$(cd "$APP_DIR" && pwd)/bin"
  ok "scoop 安装目录 $(cd "$APP_DIR" && pwd)"
else
  note "scoop prefix 不可用，退回用 pg_ctl 的位置推断"
fi
ok "二进制目录 $PG_BIN"

# 数据目录：优先用 PGDATA（scoop manifest 的 env_set 会把它写进用户环境变量，
# 但只对「安装之后新开的终端」生效），否则按几个候选位置依次探测。
DATA_DIR=""
for cand in "${PGDATA:-}" "$APP_DIR/data" "$PG_BIN/../data" "$PG_BIN/../../data"; do
  [[ -n "$cand" && -f "$cand/PG_VERSION" ]] && { DATA_DIR="$(cd "$cand" && pwd)"; break; }
done

if [[ -z "$DATA_DIR" ]]; then
  # DATA_DIR 只有 pg_ctl start 需要。服务已经在跑的话没有它也能继续建角色和库。
  if pg_isready -h 127.0.0.1 -p "$PG_PORT" -q 2>/dev/null; then
    note "找不到数据目录，但服务已经在端口 $PG_PORT 上跑着 —— 跳过启动，继续建角色和库"
  else
    bad "找不到数据目录（里面应该有 PG_VERSION 文件），而且服务也没在跑"
    note "试过的候选：${PGDATA:-(PGDATA 未设置)}、$APP_DIR/data、$PG_BIN/../data"
    note ""
    note "显式指定一下再跑：  PGDATA=<路径> bash scripts/setup-postgres.sh"
    note ""
    note "如果确实还没 initdb，手动跑一次："
    note "  \"$PG_BIN/initdb.exe\" --username=postgres --encoding=UTF8 --locale=C -D \"$PG_BIN/../data\""
    exit 1
  fi
else
  ok "数据目录 $DATA_DIR（PG $(cat "$DATA_DIR/PG_VERSION")）"
fi

# ---------------------------------------------------------------- 1. 起服务
step "1. 启动数据库服务"

if pg_isready -h 127.0.0.1 -p "$PG_PORT" -q 2>/dev/null; then
  ok "已经在跑了（端口 $PG_PORT）"
else
  printf '    启动中…\n'
  # -l 把服务器日志落到文件，否则 pg_ctl start 会因为找不到 stdout 而报错
  if pg_ctl -D "$DATA_DIR" -l "$DATA_DIR/postgres.log" -w start; then
    ok "启动成功"
    note "服务器日志：$DATA_DIR/postgres.log"
  else
    bad "启动失败，最后 20 行日志："
    tail -20 "$DATA_DIR/postgres.log" 2>/dev/null | sed 's/^/      /'
    exit 1
  fi
fi

# 后面所有 psql 都用 postgres 超级用户、空密码（initdb 默认 trust 认证）。
# -w 是「绝不弹出密码提示」—— 万一 pg_hba.conf 不是 trust，
# 缺了它会卡在交互式输入上让脚本永远不返回，而不是干脆地报错。
PSQL=(psql -w -h 127.0.0.1 -p "$PG_PORT" -U postgres -d postgres -v ON_ERROR_STOP=1 -tA)

if ! "${PSQL[@]}" -c 'SELECT 1' >/dev/null 2>&1; then
  bad "连不上 postgres 超级用户"
  note "试试手工连一次看报什么错：  psql -h 127.0.0.1 -U postgres -d postgres"
  exit 1
fi
ok "psql 可连（这台机器现在也有 psql 了，计划里原本假设没有）"

# ---------------------------------------------------------------- 2. 预检 pg_trgm
step "2. 预检 pg_trgm 扩展"

# 迁移文件第一行就是 CREATE EXTENSION IF NOT EXISTS pg_trgm，
# 索引 idx_items_title_trgm / idx_items_desc_trgm 依赖它。
# 这是「PG 18 原生包 vs 计划里的 postgres:16-alpine」唯一的真实差异点，
# 所以放在建库之前查 —— 缺了的话后面全白跑。
TRGM="$("${PSQL[@]}" -c "SELECT count(*) FROM pg_available_extensions WHERE name='pg_trgm'" 2>/dev/null)"
if [[ "$TRGM" == "1" ]]; then
  ok "pg_trgm 可用（随包提供，中文子串搜索的 GIN 索引能建）"
else
  bad "pg_available_extensions 里没有 pg_trgm"
  note "这意味着安装包没带 contrib 扩展。先别继续，把这条反馈给我。"
  exit 1
fi

# ---------------------------------------------------------------- 3. 建角色
step "3. 创建 $DB_USER 角色"

# SUPERUSER + CREATEDB 是刻意和 docker-compose 对齐的：
#   POSTGRES_USER=lf 在官方镜像里建出来的就是超级用户。
# 对齐的意义是「同一份代码在两条路径下行为一致」，不会出现
# Docker 里能跑、原生包里报权限不足的怪事。两个具体依赖：
#   SUPERUSER → CREATE EXTENSION pg_trgm（迁移文件第一条语句）
#   CREATEDB  → smoketest/setup_test.go 的 ensureTestDatabase 要 CREATE DATABASE lostfound_test
# 这是本地开发库，不是生产环境。
if [[ "$("${PSQL[@]}" -c "SELECT count(*) FROM pg_roles WHERE rolname='$DB_USER'" 2>/dev/null)" == "1" ]]; then
  "${PSQL[@]}" -c "ALTER ROLE $DB_USER LOGIN SUPERUSER CREATEDB PASSWORD '$DB_PASSWORD'" >/dev/null
  ok "角色已存在，密码和权限已重置"
else
  "${PSQL[@]}" -c "CREATE ROLE $DB_USER LOGIN SUPERUSER CREATEDB PASSWORD '$DB_PASSWORD'" >/dev/null
  ok "角色已创建（LOGIN SUPERUSER CREATEDB）"
fi

# ---------------------------------------------------------------- 4. 建库
step "4. 创建 $DB_NAME 数据库"

# 不传 ENCODING / TEMPLATE：initdb 用的是 --encoding=UTF8 --locale=C，
# template1 已经是 UTF8，继承过来就对。显式传 ENCODING 反而可能撞上
# 「encoding 与 locale 不匹配」的报错。
if [[ "$("${PSQL[@]}" -c "SELECT count(*) FROM pg_database WHERE datname='$DB_NAME'" 2>/dev/null)" == "1" ]]; then
  ok "库已存在，跳过"
else
  "${PSQL[@]}" -c "CREATE DATABASE $DB_NAME OWNER $DB_USER" >/dev/null
  ok "库已创建，owner=$DB_USER"
fi

# lostfound_test 故意不在这里建 —— smoketest/setup_test.go 的 ensureTestDatabase
# 会自己建，而且每次跑测试都 migrate down → up 一遍。在这里建反而会让
# 「测试库是不是干净的」这件事变得说不清。

# ---------------------------------------------------------------- 5. 验收
step "5. 验收"

VER="$("${PSQL[@]}" -c 'SHOW server_version' 2>/dev/null)"
ok "PostgreSQL $VER"

# 用 lf + 密码连一次，同时验两件事：密码认证这条路通（后端走的就是它），
# 以及库的编码是 UTF8。
# ⚠ 这里必须带 PGPASSWORD 和 -w —— 少了任何一个，psql 会弹出交互式密码提示，
# 脚本就永远卡在这一行不返回。
LF_PSQL=(env PGPASSWORD="$DB_PASSWORD" psql -w -h 127.0.0.1 -p "$PG_PORT" -U "$DB_USER" -d "$DB_NAME" -tA)

if ! ENC="$("${LF_PSQL[@]}" -c 'SHOW server_encoding' 2>/dev/null)"; then
  bad "$DB_USER 密码认证连不上 $DB_NAME —— backend/.env 会连不上"
  note "手工试一次看报什么错：  PGPASSWORD=$DB_PASSWORD psql -h 127.0.0.1 -U $DB_USER -d $DB_NAME"
  exit 1
fi
ok "$DB_USER 用密码可连 $DB_NAME"

if [[ "$ENC" == "UTF8" ]]; then
  ok "服务器编码 UTF8（中文标题/描述不会乱码）"
else
  bad "服务器编码是 ${ENC:-未知}，期望 UTF8"
  exit 1
fi

PGADMIN="$APP_DIR/pgAdmin 4/runtime/pgAdmin4.exe"
CTL_D="$DATA_DIR"; [[ -n "$CTL_D" ]] && CTL_D=" -D \"$CTL_D\""

cat <<EOF

${GREEN}全部就绪。${RESET}接下来：

  cd backend
  cp .env.example .env        # 然后给 JWT_SECRET 填一个随机串
  go run ./cmd/server         # 启动时自动建表 + 灌种子数据

  # 另开一个终端验收
  curl localhost:8080/api/health
  bash scripts/smoke.sh

  # 集成测试
  cd backend
  TEST_DB_DSN="postgres://$DB_USER:$DB_PASSWORD@127.0.0.1:$PG_PORT/postgres?sslmode=disable" \\
  REQUIRE_INTEGRATION=1 go test ./...

${DIM}图形界面${RESET}：这个包自带 pgAdmin 4，替代计划里的 Adminer（§9 说的
「不熟 SQL 时看表、改数据、试 SQL 的主工具」）。开始菜单里搜 pgAdmin，
或者跑 "$PGADMIN"。
第一次用要设一个 pgAdmin 自己的主密码（跟数据库无关），然后
Register → Server，Host 填 ${DIM}127.0.0.1${RESET}（不是 postgres —— 那是 compose
里的服务名，只在容器网络里有意义），用户名 $DB_USER，密码 $DB_PASSWORD。

${DIM}命令行${RESET}：这个包也带了 psql（计划原本假设本机没有）：
  PGPASSWORD=$DB_PASSWORD psql -h 127.0.0.1 -U $DB_USER -d $DB_NAME

${DIM}停 / 起服务${RESET}：
  pg_ctl$CTL_D stop
  pg_ctl$CTL_D -l "$DATA_DIR/postgres.log" start
EOF

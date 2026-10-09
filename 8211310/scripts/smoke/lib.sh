GREEN=$'\033[32m'; RED=$'\033[31m'; YELLOW=$'\033[33m'; DIM=$'\033[2m'; RESET=$'\033[0m'
pass=0; fail=0; skipped=0

tmp_headers="$(mktemp)"
trap 'rm -f "$tmp_headers"' EXIT

# check <描述> <期望包含的子串> <实际输出>
check() {
  if [[ "$3" == *"$2"* ]]; then
    printf '  %sPASS%s %s\n' "$GREEN" "$RESET" "$1"
    pass=$((pass + 1))
  else
    printf '  %sFAIL%s %s\n' "$RED" "$RESET" "$1"
    printf '       %s期望包含: %s%s\n' "$DIM" "$2" "$RESET"
    printf '       %s实际输出: %s%s\n' "$DIM" "${3:0:400}" "$RESET"
    fail=$((fail + 1))
  fi
}

# check_re <描述> <ERE 模式> <实际输出> —— check 的正则版。
#
# 用来验「形状」而不是某个字面值，比如「images 数组里只剩一个对象」。
# 这类断言用子串写不出来（子串匹配不了括号和次数），用字面值写又会被 id 的
# 随机性绑死 —— 每次跑 id 都不一样。
check_re() {
  if printf '%s' "$3" | grep -qE "$2"; then
    printf '  %sPASS%s %s\n' "$GREEN" "$RESET" "$1"
    pass=$((pass + 1))
  else
    printf '  %sFAIL%s %s\n' "$RED" "$RESET" "$1"
    printf '       %s期望匹配: %s%s\n' "$DIM" "$2" "$RESET"
    printf '       %s实际输出: %s%s\n' "$DIM" "${3:0:400}" "$RESET"
    fail=$((fail + 1))
  fi
}

# skip <描述> —— 前置条件不满足时用，和 PASS/FAIL 都区分开。
#
# 为什么不算 FAIL：这一层的职责是「环境活着吗」，缺 psql 不代表后端坏了。
# 为什么也不算 PASS：那样「全绿」会悄悄少测一条判据 —— 这正是 §10 里
# REQUIRE_INTEGRATION 那个开关要防的同一类假象。所以单独计数并打出来。
skip() {
  printf '  %sSKIP%s %s\n' "$YELLOW" "$RESET" "$1"
  skipped=$((skipped + 1))
}

section() { printf '\n%s== %s ==%s\n' "$DIM" "$1" "$RESET"; }

# req <method> <path> [json body] [token]
#
# 结果放进两个全局变量：STATUS（HTTP 状态码）和 BODY（响应体）。
# 用全局变量而不是 stdout，是因为一次请求要同时给出这两样，而 bash 函数只能
# 从 stdout 返回一个字符串 —— 硬塞在一起就得再解析一遍，反而更容易出错。
#
# ⚠ 请求体一律走 stdin（--data-binary @-），**绝不**作为 curl 的命令行参数。
#
# 这不是风格选择，是这台机器上实测出来的坑：Git Bash 把参数交给 Windows 原生的
# curl.exe 时要按当前代码页（cp1252）转换一次，中文字符不在 cp1252 里，
# 于是每个汉字都变成一个 '?'。症状是「注册接口把昵称存成了 ??」——
# 看起来像后端编码坏了，实际上是请求在离开 shell 的那一刻就已经烂了。
# 走 stdin 的字节由 bash 直接写进管道，不经过这次转换，所以是干净的 UTF-8。
#
# 反过来说：如果你在别的环境里看到中文变问号，先用这个办法分清是 shell 的锅还是后端的锅：
#   printf '%s' '{"nickname":"冒烟"}' | curl -X PUT ... --data-binary @-
#
# ⚠ 同一条坑对 **URL 查询串** 也成立，而它更隐蔽：`?keyword=冒烟` 作为参数发出去
#   会变成 `?keyword=???`，于是「搜索能命中」这条检查会假红，而「搜索不该命中」
#   那条检查会假绿。所以本脚本里所有出现在 URL 里的非 ASCII 都写成百分号编码，
#   并在旁边注释出它对应的中文 —— 失败信息里看到的仍是 %E5%86%92 这种可读性差的东西，
#   但那至少是**诚实**的可读性差。
#
# ⚠ M3 之后 #13 的响应里出现了**第二个** `"item":{"id":…}` —— 它是 matches_preview
#   数组里每一条匹配的候选帖（MatchHit 的形状就是 {item, score, breakdown}）。
#   `"item":\{"id":([0-9]+)` 因此会抠到候选帖的 id 而不是刚建出来那条，
#   而且因为候选是别人发的帖，症状是后面整段「本人改帖」变成 403 FORBIDDEN，
#   看起来像是权限模块坏了。全部五处抽取一律改用 `itemIDOf`（锚在 `"data":{` 上）。
STATUS=""; BODY=""
req() {
  local method="$1" path="$2" body="${3:-}" token="${4:-}"
  local -a args=(-s -X "$method" -w '\n%{http_code}' "$BASE$path")
  if [[ -n "$token" ]]; then
    args+=(-H "Authorization: Bearer $token")
  fi

  local out
  if [[ -n "$body" ]]; then
    args+=(-H 'Content-Type: application/json' --data-binary @-)
    out="$(printf '%s' "$body" | curl "${args[@]}")"
  else
    out="$(curl "${args[@]}")"
  fi
  STATUS="${out##*$'\n'}"
  BODY="${out%$'\n'*}"
}

# req_upload <token> <文件路径> [表单字段名，默认 file]
#
# multipart 请求没法用 req 发：req 只会把字符串塞进 --data-binary，
# 而 #6 的契约恰恰是「字段名必须叫 file、Content-Type 必须是 multipart 带 boundary」。
# curl 的 -F 会自己生成 boundary，不要手写 —— 手写的会和 curl 里的不一致。
req_upload() {
  local token="$1" file="$2" field="${3:-file}"
  local dir base out
  dir="$(dirname "$file")"; base="$(basename "$file")"

  # ⚠ 先 cd 再传**相对文件名**，而不是把绝对路径交给 curl。
  # Git Bash 会把「看起来像绝对路径」的参数换算成 Windows 路径再交给原生的 curl.exe，
  # 但 -F 的值是 `file=@/d/.../fake.png` 这种带 `file=@` 前缀的形式，换算规则在这里不靠谱。
  # 相对路径不参与换算；而 multipart 的 filename 本来也只该是 basename ——
  # 把本机目录结构写进一次上传请求里没有任何意义（它还会被存进日志）。
  out="$(cd "$dir" && curl -s -X POST -w '\n%{http_code}' \
        -H "Authorization: Bearer $token" -F "${field}=@${base}" "$BASE/api/uploads")"
  STATUS="${out##*$'\n'}"
  BODY="${out%$'\n'*}"
}

# ---------------------------------------------------------------- 配置读取
# 这个脚本打的是**你正在跑的那个 dev server**，所以路径、端口都得从 backend/.env 里读，
# 而不是假设默认值 —— 有人把 UPLOAD_DIR 改成 D:/data/uploads 之后，
# 按默认值去找文件的检查会报「文件不存在」，而那恰恰是它要检查的事情本身。
envval() { sed -n "s/^$1=//p" "$ROOT/backend/.env" 2>/dev/null | head -1; }

DB_USER="$(envval DB_USER)";         DB_USER="${DB_USER:-lf}"
DB_PASSWORD="$(envval DB_PASSWORD)"; DB_PASSWORD="${DB_PASSWORD:-lf}"
DB_NAME="$(envval DB_NAME)";         DB_NAME="${DB_NAME:-lostfound}"
DB_PORT="$(envval DB_PORT)";         DB_PORT="${DB_PORT:-5432}"

# UPLOAD_DIR 是**相对后端进程工作目录**的（go run ./cmd/server 是在 backend/ 下跑的），
# 所以相对路径要拼上 backend/。
UPLOAD_DIR="$(envval UPLOAD_DIR)"; UPLOAD_DIR="${UPLOAD_DIR:-./uploads}"
case "$UPLOAD_DIR" in
  /*)                       UPLOAD_ABS="$UPLOAD_DIR" ;;
  [A-Za-z]:[\\/]*)          UPLOAD_ABS="$UPLOAD_DIR" ;;
  *)                        UPLOAD_ABS="$ROOT/backend/$UPLOAD_DIR" ;;
esac

# 假图片：只有魔数是真的，后面全是填充字节。
#
# 为什么不用一张真 PNG：#6 的判定依据是**内容嗅探**（http.DetectContentType 只看前几个字节），
# 不是文件后缀，也不是能不能解码。所以 8 个魔数字节就足够让它认成 image/png，
# 而脚本里塞一段 base64 的真图片只会让这份脚本更难读。
# 反过来说，M2 的测试里确实验了「内容是 PNG 但后缀叫 .php」—— 那也正是这个意思。
make_fake() { # make_fake <输出路径> <魔数 printf 串> <填充字节数>
  printf "$2" > "$1"
  head -c "$3" /dev/zero | tr '\0' 'a' >> "$1"
}

# 一个临时目录而不是几个 mktemp 文件：这里要用到**文件名**（shell.php），
# 而 mktemp 给不出想要的后缀；#40 那一步还要让 curl 用 -o 往里面写下载回来的字节。
#
# ⚠ 必须是**纯 ASCII** 的路径。这一度是本机上一个最难自查的失败：
#   把临时目录放在仓库里（backend/.smoke-tmp）时，curl -o 写出来的文件根本不存在，
#   报的是「字节不一致」「首字节是 0x」这种和真实原因毫无关系的话。
#   原因在 Git Bash 那一层：仓库路径里有中文（失物招领系统），而 curl.exe 是原生程序，
#   它拿到的路径要按当前代码页（cp1252）换算一次 —— 和上面请求体那一条是同一个坑，
#   只不过这次挨打的是文件路径而不是 argv。mktemp -d 给的 /tmp/tmp.XXXXXX 是 ASCII 的，
#   所以它一直没事。
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir" "$tmp_headers"' EXIT
png_path="$tmp_dir/fake.png"
png_named_php_path="$tmp_dir/shell.php"
txt_path="$tmp_dir/notes.txt"

make_fake "$png_path" '\211PNG\r\n\032\n' 1024
make_fake "$png_named_php_path" '\211PNG\r\n\032\n' 1024
make_fake "$txt_path" 'this is not an image, it is a text file: ' 512

# jval <json> <带一个捕获组的 sed(ERE) 模式> —— 从一行 JSON 里抠一个值出来。
#
# 用 sed 而不是 jq：jq 既不是 Git Bash 自带的也不是 Windows 自带的，
# 而这一层脚本的定位是「随手能跑」。
#
# ⚠ 模式必须写得**足够具体**。`.*` 是最长匹配，所以 `"id":([0-9]+)` 抠到的是
#   响应里**最后一个** id，而一个 ItemView 里有 item.id、category.id、location.id、
#   author.id、images[].id 五六个 id。这里一律带上紧邻的上下文（`"item":\{"id":`），
#   宁可难读也不能抠错 —— 抠错比抠不到更糟，它会静默地测另一件事。
jval() { printf '%s' "$1" | sed -nE "s|.*$2.*|\1|p" | head -1; }

# itemIDOf <json> —— 抠出 #13 建出来那条帖子自己的 id。
#
# 为什么不直接写 jval 的模式：M3 之后同一个响应里有**好几个** `"item":{"id":`
# （matches_preview 数组里每条候选帖都是这个形状），而 `.*` 是最长匹配，
# 拿到的是最后一个 —— 也就是别人那条帖子的 id。后面所有「本人改帖」因此变成 403，
# 症状长得像权限模块坏了。锚在信封的 `"data":{` 上就只有唯一一处。
itemIDOf() { jval "$1" '"data":\{"item":\{"id":([0-9]+)'; }

# psql 可执行文件的位置。
#
# scoop 装的 PostgreSQL **不会**把它的 bin 加进 PATH（M0 那一步是手动 initdb + pg_ctl 起的），
# 所以 command -v 找不到它。这里主动认几个已知位置，找不到就退化成 SKIP。
#
# 值得专门写这一段的原因：只有能直接看数据库，才测得出
# 「接口返回了正确的错误码，但库里其实已经写进去了一行」这一类错误 ——
# 而那正是 §10 第③层相对第②层的唯一增量（第②层有 Pool，但没有 HTTP 之外的环境）。
PSQL=""
if command -v psql >/dev/null 2>&1; then
  PSQL=psql
else
  for cand in \
    /d/Scoop/apps/postgresql/current/bin/psql.exe \
    /c/Scoop/apps/postgresql/current/bin/psql.exe \
    "$HOME/scoop/apps/postgresql/current/bin/psql.exe"; do
    if [[ -x "$cand" ]]; then PSQL="$cand"; break; fi
  done
fi

# db <SQL> —— 直接查开发库，用于那些「接口看不出来越过了数据库」的断言。
# 没有 psql 或者连不上就返回空串，调用方一律走 SKIP（见上面的 skip 注释）。
HAS_PSQL=0
if [[ -n "$PSQL" ]] && "$PSQL" -w -h 127.0.0.1 -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" -tAc 'SELECT 1' >/dev/null 2>&1; then
  HAS_PSQL=1
fi
db() {
  [[ "$HAS_PSQL" == 1 ]] || return 0
  PGPASSWORD="$DB_PASSWORD" "$PSQL" -w -h 127.0.0.1 -p "$DB_PORT" -U "$DB_USER" -d "$DB_NAME" -tA -c "$1" 2>/dev/null
}

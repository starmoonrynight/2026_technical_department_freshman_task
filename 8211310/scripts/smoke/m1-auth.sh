# ---------------------------------------------------------------- M1 认证
# deps:
section "M1 认证：#1–#5（注册 → 登录 → /me → 改资料 → 改密码）"

# 每次跑都换一个用户名：这个脚本打的是**开发库**，重复注册会撞 USER_ALREADY_EXISTS。
# 后缀是「秒 + bash 随机数」，同一秒里连跑两次也不会撞。
U="smoke$(date +%s)$RANDOM"
P="correct-horse-battery"
P2="new-password-staple"
printf '  %s本轮冒烟用户：%s%s\n' "$DIM" "$U" "$RESET"

# ---- #1 注册 ----
req POST /api/auth/register "{\"username\":\"$U\",\"password\":\"$P\",\"nickname\":\"冒烟\"}"
check '#1 注册返回 HTTP 200'          '200'              "$STATUS"
check '  code 是 OK'                  '"code":"OK"'     "$BODY"
check '  data.role 是 user（不能让请求体决定角色）' '"role":"user"' "$BODY"
check '  data.nickname 原样返回'      '"nickname":"冒烟"' "$BODY"
if [[ "$BODY" == *'"token"'* ]]; then
  printf '  %sFAIL%s 注册响应里出现了 token —— §4 第 1 行的 data 只有四个字段\n' "$RED" "$RESET"; fail=$((fail + 1))
else
  printf '  %sPASS%s 注册响应里没有 token（§4：注册和登录是两个端点）\n' "$GREEN" "$RESET"; pass=$((pass + 1))
fi

# 重名
req POST /api/auth/register "{\"username\":\"$U\",\"password\":\"$P\"}"
check '  重名返回 HTTP 409'           '409'                        "$STATUS"
check '  code 是 USER_ALREADY_EXISTS' '"code":"USER_ALREADY_EXISTS"' "$BODY"

# 弱密码（8 位但纯数字，中 §8 两条规则里的第二条）
req POST /api/auth/register '{"username":"weakling_smoke","password":"12345678"}'
check '  弱密码返回 HTTP 400'         '400'                  "$STATUS"
check '  code 是 WEAK_PASSWORD'       '"code":"WEAK_PASSWORD"' "$BODY"

# 用户名不存在和密码错误必须**完全同形**，否则登录接口就是一本用户名字典的探针
req POST /api/auth/login '{"username":"definitely_not_registered_smoke","password":"whatever"}'
unknown_status="$STATUS"; unknown_body="$BODY"
req POST /api/auth/login "{\"username\":\"$U\",\"password\":\"wrong-password\"}"
check '#2 密码错误返回 HTTP 401'      '401' "$STATUS"
check '  code 是 INVALID_CREDENTIALS' '"code":"INVALID_CREDENTIALS"' "$BODY"
if [[ "$unknown_status" == "$STATUS" && "$unknown_body" == *'"code":"INVALID_CREDENTIALS"'* ]]; then
  printf '  %sPASS%s 用户名不存在与密码错误同形（防账号枚举）\n' "$GREEN" "$RESET"; pass=$((pass + 1))
else
  printf '  %sFAIL%s 用户名不存在（%s）与密码错误（%s）不同形，能拿它枚举注册用户\n' \
    "$RED" "$RESET" "$unknown_status" "$STATUS"; fail=$((fail + 1))
fi

# ---- #2 登录 ----
req POST /api/auth/login "{\"username\":\"$U\",\"password\":\"$P\"}"
check '#2 登录返回 HTTP 200'          '200'              "$STATUS"
check '  code 是 OK'                  '"code":"OK"'     "$BODY"
check '  data 里有 expires_at'        '"expires_at":"'  "$BODY"
TOKEN="$(printf '%s' "$BODY" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')"
if [[ ${#TOKEN} -gt 20 ]]; then
  printf '  %sPASS%s 拿到 token（%d 字符）\n' "$GREEN" "$RESET" "${#TOKEN}"
  pass=$((pass + 1))
else
  printf '  %sFAIL%s 没从响应里取出 token，后面几步都会连带失败\n' "$RED" "$RESET"
  printf '       %s实际输出: %s%s\n' "$DIM" "${BODY:0:400}" "$RESET"
  fail=$((fail + 1))
fi

# ---- #3 GET /api/auth/me ----
req GET /api/auth/me '' "$TOKEN"
check '#3 带 token 访问 /me 返回 200' '200'          "$STATUS"
check '  code 是 OK'                  '"code":"OK"' "$BODY"
check "  返回的是本人（$U）"           "\"username\":\"$U\"" "$BODY"
if [[ "$BODY" == *password* || "$BODY" == *'$2a$'* || "$BODY" == *'$2b$'* ]]; then
  printf '  %sFAIL%s /me 的响应里出现了 password 或 bcrypt 哈希\n' "$RED" "$RESET"; fail=$((fail + 1))
else
  printf '  %sPASS%s /me 的响应里没有 password_hash\n' "$GREEN" "$RESET"; pass=$((pass + 1))
fi

req GET /api/auth/me
check '  不带 token 返回 HTTP 401'    '401'                  "$STATUS"
check '  code 是 UNAUTHORIZED'        '"code":"UNAUTHORIZED"' "$BODY"
check '  401 也带 request_id'         '"request_id":"'       "$BODY"

# 改签名的**第一个**字符。不能改最后一个：base64url 末位有 2 个填充位被忽略，
# 'A'/'B'/'C'/'D' 解出来是同样的字节，改了等于没改，测试会假红。
sig="${TOKEN##*.}"
prefix="${TOKEN%.*}"
repl="A"; [[ "${sig:0:1}" == "A" ]] && repl="B"
req GET /api/auth/me '' "${prefix}.${repl}${sig:1}"
check '  签名被改一个字符 → 401'      '401'                  "$STATUS"
check '  code 是 UNAUTHORIZED'        '"code":"UNAUTHORIZED"' "$BODY"

# ---- #4 PUT /api/auth/me ----
req PUT /api/auth/me '{"nickname":"冒烟改名","phone":"问宿舍阿姨"}' "$TOKEN"
check '#4 改资料返回 HTTP 200'        '200'          "$STATUS"
check '  code 是 OK'                  '"code":"OK"' "$BODY"
check '  新昵称生效'                  '"nickname":"冒烟改名"' "$BODY"
# 只传了 nickname 和 phone，email 必须原封不动（「字段缺失」和「空串」是两回事）
req PUT /api/auth/me '{"email":"a@b.c"}' "$TOKEN"
check '  改 email 不会冲掉 nickname'  '"nickname":"冒烟改名"' "$BODY"
# 这个系统不发邮件也不发短信，所以刻意不校验格式 —— 和 items.contact 同一个判断
req PUT /api/auth/me '{"phone":"图书馆前台 86919001"}' "$TOKEN"
check '  非格式化的联系方式被原样接受' '"phone":"图书馆前台 86919001"' "$BODY"
req PUT /api/auth/me '{"nickname":""}' "$TOKEN"
check '  昵称清空被拒（VALIDATION）'  '"code":"VALIDATION"' "$BODY"
req PUT /api/auth/me '{"nickname":"黑客"}'
check '  改资料不带 token → 401'      '"code":"UNAUTHORIZED"' "$BODY"

# ---- #5 POST /api/auth/change-password ----
req POST /api/auth/change-password "{\"old_password\":\"not-mine\",\"new_password\":\"$P2\"}" "$TOKEN"
check '#5 旧密码不对返回 HTTP 400'    '400'                       "$STATUS"
check '  code 是 OLD_PASSWORD_WRONG'  '"code":"OLD_PASSWORD_WRONG"' "$BODY"

req POST /api/auth/change-password "{\"old_password\":\"$P\",\"new_password\":\"12345678\"}" "$TOKEN"
check '  新密码太弱 → WEAK_PASSWORD'  '"code":"WEAK_PASSWORD"' "$BODY"

req POST /api/auth/change-password "{\"old_password\":\"$P\",\"new_password\":\"$P2\"}" "$TOKEN"
check '  改密码成功返回 HTTP 200'     '200'          "$STATUS"
check '  code 是 OK'                  '"code":"OK"' "$BODY"
check '  data 是 null（§4：没有需要回传的结果）' '"data":null' "$BODY"

req POST /api/auth/login "{\"username\":\"$U\",\"password\":\"$P2\"}"
check '  新密码能登录'                '"code":"OK"' "$BODY"
req POST /api/auth/login "{\"username\":\"$U\",\"password\":\"$P\"}"
check '  旧密码登不上了'              '"code":"INVALID_CREDENTIALS"' "$BODY"

# 已知限制：改密码**不会**让旧 token 失效（JWT 无状态，只能等它过期）。
# 要真正做到需要给 users 加一列 password_changed_at，计划 §3.7 里没有。
req GET /api/auth/me '' "$TOKEN"
check '  旧 token 仍然有效（已知限制，见 m1_auth_test.go）' '"code":"OK"' "$BODY"

# ---------------------------------------------------------------- 封禁
# 这一条是 M1 的判据（JWT 中间件每请求回库读一次 status），但它以前用的是 $U/$TOKEN
# —— 也就是本文件开头那个共享用户。那正是「封禁必须排在整脚本最末尾」这个顺序约束的
# 唯一来源：把它挪回 M1，后面每一节都会拿着一个已经被封掉的 token 去跑。
#
# 现在它自己注册一个一次性用户，于是两件事一起解决：这一节能待在它该在的位置，
# 而「别的节动了 $U 会不会把这条判据搞坏」这种耦合也不存在了。
#
# 这一步必须直接改库：M6 才有封禁/解封端点。所以它需要 psql 和开发库的凭据，
# 拿不到就 SKIP —— 这条判据在 backend/smoketest/m1_auth_test.go 里是无条件覆盖的。
BAN_U="ban$(date +%s)$RANDOM"
BAN_P="correct-horse-battery"
req POST /api/auth/register "{\"username\":\"$BAN_U\",\"password\":\"$BAN_P\"}"
BAN_ID="$(jval "$BODY" '"data":\{"id":([0-9]+)')"
req POST /api/auth/login "{\"username\":\"$BAN_U\",\"password\":\"$BAN_P\"}"
BAN_TOKEN="$(jval "$BODY" '"token":"([^"]{20,})"')"

if [[ "$HAS_PSQL" != 1 ]]; then
  skip '封禁后登录 → USER_BANNED（本机找不到 psql，或者连不上开发库；试试 bash scripts/setup-postgres.sh）'
elif [[ -z "$BAN_ID" || -z "$BAN_TOKEN" ]]; then
  printf '  %sFAIL%s 专门用来被封的那个用户没建起来（id=%s），这一条测不了\n' "$RED" "$RESET" "$BAN_ID"
  fail=$((fail+1))
else
  db "UPDATE users SET status='banned', updated_at=now() WHERE id=$BAN_ID" >/dev/null
  req POST /api/auth/login "{\"username\":\"$BAN_U\",\"password\":\"$BAN_P\"}"
  check '封禁后登录返回 HTTP 403'     '403'                 "$STATUS"
  check '  code 是 USER_BANNED'       '"code":"USER_BANNED"' "$BODY"

  # 旧 token 也必须立刻失效。role/status 刻意不进 JWT claims、中间件每请求回库读一次，
  # 换的就是这一条：admin 一点封禁，那个人手上的 token 当场作废，而不是还能用满 24 小时。
  req GET /api/auth/me '' "$BAN_TOKEN"
  check '  封禁后旧 token 立刻失效'   '"code":"USER_BANNED"' "$BODY"
fi


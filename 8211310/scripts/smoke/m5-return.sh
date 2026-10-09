# ---------------------------------------------------------------- M5 归还确认 · 积分
# deps:
section "M5 归还确认 #23–#29、积分 #33（§12 全链 + §13 第 6、10 步）"

# ⚠ 这一节是**自包含**的：自己注册五个用户、自己发帖、自己上传凭证图，
#   不读别的节留下的任何全局变量，所以它可以单独跑：
#       bash scripts/smoke.sh m5
#   其余节现在还做不到这件事（它们借 m1-auth 的 $TOKEN、m2-items 的 $MY_ID/$LOC_ID）。
#   这不是风格问题：上一版 M5 就是借 M3/M4 留下的数据写的，于是「拾主手上有几条通知」
#   这种断言实际测的是**上一节跑出了什么**，改 M4 会让 M5 假红 —— 本轮那三条 FAIL 里
#   有两条就是这么来的。
#
# 五个用户各自只承担一个角色，谁的权限都不是「顺手正好」：
#   M5_F  拾主 / 发帖人 —— 唯一能 confirm、能 reject 的人
#   M5_L  小刘 —— **从没解锁过任何联系方式**的提交人（§12 的第一条判据就是他能不能提交）
#   M5_A  失主 A —— 有**两条**命中的 lost 帖，确认之后只该收到 **1** 条提示
#   M5_B  失主 B —— 有**一条**命中的 lost 帖
#   M5_O  局外人 —— 什么关系都不是：#24 那个 403 由他提供，被临时提成 admin 的也是他
M5_P="correct-horse-battery"
M5_TAG="m5$(date +%s)$RANDOM"
M5_CONTACT="wx_smoke_m5_finder"
M5_MSG="东西还在，我在三号楼下面等你，可以当面核对"
M5_NOTE="核对过了，卡号和夹层里的东西都对得上"
M5_REJ_NOTE="这张照片对不上我捡到的那串钥匙"

# 36=衣物箱包→钱包，70=图书馆，都是 migrations 里种好的叶子。
# 这一对和 m3 用的是同一对：「这种参数会写台账」是在那一节证明的，
# 这里只是复用那个已经成立的结论 —— 换分类或换地点就得重新验匹配，那不是这一节的职责。
M5_CAT=36
M5_LOC=70
# 三个时刻都相对 now（UTC），这样「found_at 落在丢失窗口里 → S_time=1.0」
# 是这条链自己保证的，不依赖跑脚本的那一天。顺序有讲究：lost_at 必须晚于 last_seen_at
# （M2 那条时间校验会拒反的），而 found_at 夹在中间。
M5_T_SEEN="$(date -u -d '-3 hours' +%Y-%m-%dT%H:%M:%SZ)"
M5_T_LOST="$(date -u -d '-2 hours' +%Y-%m-%dT%H:%M:%SZ)"
M5_T_FOUND="$(date -u -d '-90 minutes' +%Y-%m-%dT%H:%M:%SZ)"

# m5_new <后缀> <变量前缀> —— 注册 + 登录 + 取 id，结果写进 $<前缀>_TOK 和 $<前缀>_ID。
#
# 为什么用 printf -v 而不是把值 echo 出去：一次注册要同时给出 token 和 id 两样东西，
# 而 bash 函数只能从 stdout 返回一个字符串 —— 硬塞在一起就得再解析一遍，反而更容易出错。
# （req / req_upload 用全局 STATUS、BODY 是同一个理由。）
m5_new() {
  local suffix="$1" pfx="$2" tk
  # ⚠ name 必须单独一行 local：同一个 local 语句里后面的赋值看不到前面的那个
  #   （bash 先展开整条语句再赋值），所以在 `local a=1 b="$a"` 里 b 会拿到空值 ——
  #   而 set -u 会把它变成「suffix: unbound variable」这种和真实原因无关的报错。
  local name="${M5_TAG}$suffix"
  req POST /api/auth/register "{\"username\":\"$name\",\"password\":\"$M5_P\"}"
  req POST /api/auth/login "{\"username\":\"$name\",\"password\":\"$M5_P\"}"
  tk="${pfx}_TOK"; printf -v "$tk" '%s' "$(jval "$BODY" '"token":"([^"]{20,})"')"
  # ${!tk} 是「取 tk 这个名字所指向的那个变量的值」—— 直接写 "$tk" 交出去的是
  # 变量名本身，于是 /me 收到一个长得像用户名的 Bearer，401，id 也就取不到了。
  req GET /api/auth/me '' "${!tk}"
  tk="${pfx}_ID"; printf -v "$tk" '%s' "$(jval "$BODY" '"data":\{"id":([0-9]+)')"
}

# m5_lost_body <contact 后四位> —— 一条会命中下面那条 found 帖的 lost 帖。
m5_lost_body() {
  printf '{"item_type":"lost","title":"%s 黑色长款钱包","description":"%s 夹层里有一张校园卡","category_id":%d,"location_id":%d,"location_detail":"三楼自习室B区","last_seen_at":"%s","lost_at":"%s","contact":"1380000%s"}' \
    "$M5_TAG" "$M5_TAG" "$M5_CAT" "$M5_LOC" "$M5_T_SEEN" "$M5_T_LOST" "$1"
}

# m5_found_body <contact> —— 和上面那批 lost 帖同分类、同地点、时间在窗口内。
m5_found_body() {
  printf '{"item_type":"found","title":"%s 黑色长款钱包","description":"%s 夹层里有一张校园卡","category_id":%d,"location_id":%d,"location_detail":"三楼自习室B区","found_at":"%s","contact":"%s"}' \
    "$M5_TAG" "$M5_TAG" "$M5_CAT" "$M5_LOC" "$M5_T_FOUND" "$1"
}

n_count()      { db "SELECT count(*) FROM notifications WHERE user_id=$1 AND type='$2'"; }
user_credit()  { db "SELECT credit_score FROM users WHERE id=$1"; }

m5_new f M5_finder
m5_new l M5_liu
m5_new a M5_ownera
m5_new b M5_ownerb
m5_new o M5_out

if [[ -z "$M5_finder_TOK" || -z "$M5_liu_TOK" || -z "$M5_ownera_ID" || -z "$M5_out_ID" ]]; then
  printf '  %sFAIL%s 这一节要的五个用户没建齐（拾主=%s 小刘=%s 失主A=%s），M5 整段测不了\n' \
    "$RED" "$RESET" "$M5_finder_ID" "$M5_liu_ID" "$M5_ownera_ID"
  fail=$((fail+1))
  # 这些文件是被 runner **source** 的，所以 return 是合法的：它只结束这一节，
  # 不影响后面的节继续跑（换行 exit 会把整个冒烟一起结束掉，那是错的）。
  return 0
fi

# 归还凭证图：现场走一次 #6 拿 path。它是这条链的入场券（§16.2「不表态就按必填实现」），
# 而整个 credit_score 体系就挂在这张图上 —— 所以它必须是一个真的 path，不是编出来的串。
req_upload "$M5_liu_TOK" "$png_path"
check '小刘上传一张归还凭证图返回 HTTP 200' '200' "$STATUS"
M5_PROOF="$(jval "$BODY" '"path":"([^"]+\.png)"')"

# ---- 场景：三条 lost 帖先落地，最后建那条 found 帖 ----
# 顺序是刻意的：lost 帖创建时一行都不写（M3 的判据④），台账和通知都发生在
# found 帖创建的那一刻 —— 反过来（found 先、lost 后）的话这两条 lost 帖
# 根本不会进台账，下面那条 item_returned_hint 就没东西可收了。
req POST /api/items "$(m5_lost_body 1111)" "$M5_ownera_TOK"
check '失主 A 建第一条 lost 帖返回 HTTP 200' '200' "$STATUS"
M5_A1="$(itemIDOf "$BODY")"
req POST /api/items "$(m5_lost_body 2222)" "$M5_ownera_TOK"
check '失主 A 建第二条（标题描述都一样，专门用来测按作者去重）返回 HTTP 200' '200' "$STATUS"
M5_A2="$(itemIDOf "$BODY")"
req POST /api/items "$(m5_lost_body 3333)" "$M5_ownerb_TOK"
M5_B1="$(itemIDOf "$BODY")"

req POST /api/items "$(m5_found_body "$M5_CONTACT")" "$M5_finder_TOK"
check '拾主建本轮这条 found 帖返回 HTTP 200' '200' "$STATUS"
M5_FOUND="$(itemIDOf "$BODY")"
check_re '  有人被通知了（台账写了、hint 才有来源）' '"notified_count":[0-9]+' "$BODY"

# 另外两条帖只为 #23 的两道门存在：一条被拾主自己关掉（⑤ closed 不收新确认），
# 一条被拾主删掉（② 已软删的帖子是 404，不是 410、也不是 500）。
req POST /api/items "$(m5_found_body "closed_m5_contact")" "$M5_finder_TOK"
M5_CLOSED="$(itemIDOf "$BODY")"
req PATCH "/api/items/$M5_CLOSED/status" '{"status":"closed"}' "$M5_finder_TOK"
req POST /api/items "$(m5_found_body "gone_m5_contact")" "$M5_finder_TOK"
M5_GONE="$(itemIDOf "$BODY")"
req DELETE "/api/items/$M5_GONE" '' "$M5_finder_TOK"

# ---------- #23 的五道门：顺序本身就是判据 ----------
# service/item_return.go 顶部那张 ①–⑥ 表是这么排的：
#   ①字段 → ②不存在/已删 → ③lost 帖 → ④自己的帖 → ⑤不是 open → ⑥写一行 pending
# ① 排在所有查库之前（字段错了是用户当下能改的东西，而「帖子不存在」他改不了）。
# ③ 排在 ④ 前面：对 lost 帖提交归还确认这件事**根本不成立**，比「这是你自己的帖子」
# 更接近问题的本质。判断顺序错了的症状很具体：失主对自己那条 lost 帖点归还，
# 会看到「不能对自己的帖子提交归还确认」，然后困惑地去发一条 found 帖试试。
req POST "/api/items/$M5_FOUND/returns" "{\"message\":\"$M5_MSG\"}" "$M5_liu_TOK"
check '① 不带凭证图 → HTTP 400'          '400'                        "$STATUS"
check '   错误指向 proof_image_path'      '"field":"proof_image_path"' "$BODY"

req POST "/api/items/$M5_FOUND/returns" "{\"message\":\"太短了\",\"proof_image_path\":\"$M5_PROOF\"}" "$M5_liu_TOK"
check '   说明短于 5 个字 → HTTP 400'     '400'                        "$STATUS"
check '     错误指向 message'             '"field":"message"'          "$BODY"

req POST "/api/items/$M5_FOUND/returns" "{\"message\":\"$M5_MSG\",\"proof_image_path\":\"../../etc/passwd\"}" "$M5_liu_TOK"
check '   凭证图路径不是 #6 那种形状 → HTTP 400' '400'                 "$STATUS"
# 这条不是洁癖：这个值将来会被拼成 #24 的 proof_image_url，
# 不校验形状就等于让客户端塞进任意字符串（绝对磁盘路径、别人的 URL、带 ../ 的相对路径）。
check '     同样指向 proof_image_path'    '"field":"proof_image_path"' "$BODY"

req POST "/api/items/$M5_GONE/returns" "{\"message\":\"$M5_MSG\",\"proof_image_path\":\"$M5_PROOF\"}" "$M5_liu_TOK"
check '② 对一条**已软删**的帖子提交 → HTTP 404' '404'                  "$STATUS"
req POST "/api/items/999999999/returns" "{\"message\":\"$M5_MSG\",\"proof_image_path\":\"$M5_PROOF\"}" "$M5_liu_TOK"
check '   对不存在的帖子提交 → HTTP 404'  '404'                "$STATUS"
check '     code 是 NOT_FOUND'            '"code":"NOT_FOUND"' "$BODY"

req POST "/api/items/$M5_A1/returns" "{\"message\":\"$M5_MSG\",\"proof_image_path\":\"$M5_PROOF\"}" "$M5_ownera_TOK"
check '③ 失主对**自己的 lost 帖**提交 → 说的是「失物帖不需要归还确认」' '"code":"VALIDATION"' "$BODY"
if [[ "$BODY" == *RETURN_SELF* ]]; then
  printf '  %sFAIL%s ③④ 的顺序反了：lost 帖那一支被 RETURN_SELF 抢先答掉了\n' "$RED" "$RESET"; fail=$((fail+1))
else
  printf '  %sPASS%s   响应里没有 RETURN_SELF（③ 确实排在 ④ 前面）\n' "$GREEN" "$RESET"; pass=$((pass+1))
fi

req POST "/api/items/$M5_FOUND/returns" "{\"message\":\"$M5_MSG\",\"proof_image_path\":\"$M5_PROOF\"}" "$M5_finder_TOK"
check '④ 拾主对自己的 found 帖提交 → HTTP 400' '400'                   "$STATUS"
check '   code 是 RETURN_SELF'                 '"code":"RETURN_SELF"'   "$BODY"

req POST "/api/items/$M5_CLOSED/returns" "{\"message\":\"$M5_MSG\",\"proof_image_path\":\"$M5_PROOF\"}" "$M5_liu_TOK"
check '⑤ closed 的拾物帖不收新确认 → HTTP 409' '409'                    "$STATUS"
check '   code 是 ITEM_CLOSED'                 '"code":"ITEM_CLOSED"'   "$BODY"

if [[ "$HAS_PSQL" != 1 ]]; then
  skip '①–⑤ 那六次被拒的请求一行都没写（本机没有 psql，数不了 item_returns）'
else
  check '   被拒的六次一共没多写行（小刘名下 0 行）' '0' \
    "$(db "SELECT count(*) FROM item_returns WHERE submitter_id=$M5_liu_ID")"
fi

# ---------- 判据①：从没解锁过联系方式的人直接提交成功 ----------
req POST "/api/items/$M5_FOUND/returns" "{\"message\":\"$M5_MSG\",\"proof_image_path\":\"$M5_PROOF\"}" "$M5_liu_TOK"
check '⑥ 小刘从没解锁过、也没发过任何帖，直接提交 → HTTP 200（§16 删掉了那道前置校验）' '200' "$STATUS"
M5_R1="$(jval "$BODY" '"data":\{"id":([0-9]+)')"
check '   状态是 pending'              '"status":"pending"'       "$BODY"
check '   item_id 就是那条 found 帖'   "\"item_id\":$M5_FOUND,"   "$BODY"
check_re '   submitted_at 是 UTC 的 RFC3339' \
  '"submitted_at":"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z' "$BODY"

req POST "/api/items/$M5_FOUND/returns" "{\"message\":\"$M5_MSG\",\"proof_image_path\":\"$M5_PROOF\"}" "$M5_liu_TOK"
check '同一个人对同一帖再提交一次 → HTTP 409' '409'                       "$STATUS"
check '   code 是 RETURN_DUPLICATE'           '"code":"RETURN_DUPLICATE"' "$BODY"

# 那条唯一索引是**部分**索引（WHERE status='pending'），管的是「同人对同帖的待处理」，
# 不是「一条帖只能有一条确认」。这里数的是三件事：小刘只有 1 行、
# 他一行都没解锁过（这条链不经过 contact_views）、拾主只收到 1 条 return_submitted。
if [[ "$HAS_PSQL" != 1 ]]; then
  skip '提交只写一行、没绕过解锁、拾主收到一条通知、积分没动（没有 psql）'
else
  check '   小刘名下正好 1 行' '1' \
    "$(db "SELECT (count(*)=1)::int FROM item_returns WHERE submitter_id=$M5_liu_ID")"
  check '   他一行都没解锁过（归还确认这条路根本不经过 contact_views）' '0' \
    "$(db "SELECT count(*) FROM contact_views WHERE item_id=$M5_FOUND AND user_id=$M5_liu_ID")"
  check '   拾主收到 1 条 return_submitted' '1' "$(n_count "$M5_finder_ID" return_submitted)"
  check '   那条通知挂着这一条归还确认'     '1' \
    "$(db "SELECT (count(*)=1)::int FROM notifications WHERE user_id=$M5_finder_ID AND type='return_submitted' AND return_id=$M5_R1")"
  check '   积分这时候还一分没动（加分只在 confirm 那一支）' '100' "$(user_credit "$M5_liu_ID")"
  check '   而且一条流水都没写' '0' \
    "$(db "SELECT count(*) FROM credit_logs WHERE user_id=$M5_liu_ID OR user_id=$M5_finder_ID")"
fi

# ---------- #24 详情：只有提交人、发帖人、admin 三方看得见 ----------
req GET "/api/returns/$M5_R1" '' "$M5_liu_TOK"
check '#24 提交人看自己那条 → HTTP 200' '200' "$STATUS"
check '   带着凭证图的 URL'             '"proof_image_url":"/uploads/' "$BODY"
check '   带着提交人自己的信用分（发帖人判断真伪的唯一依据）' '"credit_score":100' "$BODY"
check '   还没人审 → review_kind 是 null（键必须在，值必须诚实）' '"review_kind":null' "$BODY"
req GET "/api/returns/$M5_R1" '' "$M5_finder_TOK"
check '   发帖人也能看 → HTTP 200'      '200' "$STATUS"
req GET "/api/returns/$M5_R1" '' "$M5_ownera_TOK"
check '   三方之外（他是配对的失主，但不是这条记录的任何一方）→ HTTP 403' '403' "$STATUS"
# FORBIDDEN 而不是 NOT_FOUND 的取舍和 #22 名单一样：藏起来的代价是提交人拼错一个 id
# 会收到「记录不存在」而明明是没权限，他会去翻自己到底提没提过。
check '     code 是 FORBIDDEN 而不是 NOT_FOUND' '"code":"FORBIDDEN"' "$BODY"
req GET "/api/returns/$M5_R1"
check '   匿名 → HTTP 401'              '401' "$STATUS"

# ---------- 谁能按确认/拒绝这两个按钮：只有发帖人，admin 也不行 ----------
# 定位原则 5：admin 能销毁内容和账号，但**不能制造归属**。
# 替拾主点「确认收到」就是替他做归属判断，所以 #25/#26 对 admin 一律 FORBIDDEN ——
# 「不看 role、只看 submitter != item.user_id」那一句写在 service，这里验它从 HTTP 也成立。
req POST "/api/returns/$M5_R1/confirm" '{}' "$M5_liu_TOK"
check '提交人自己确认自己的归还 → HTTP 403' '403' "$STATUS"
req GET "/api/returns/$M5_R1/confirm" '' "$M5_finder_TOK"
check '确认这条路只有 POST（GET 它 → HTTP 405）' '405' "$STATUS"
req POST "/api/returns/$M5_R1/reject" '{"owner_note":""}' "$M5_finder_TOK"
check '拒绝不带理由 → HTTP 400'   '400'                   "$STATUS"
check '  错误指向 owner_note'     '"field":"owner_note"'  "$BODY"
req POST "/api/returns/$M5_R1/reject" '{"owner_note":"这张照片对不上"}' "$M5_ownera_TOK"
check '  不是发帖人的拒绝 → HTTP 403（权限排在理由校验之前）' '403' "$STATUS"

if [[ "$HAS_PSQL" != 1 ]]; then
  skip 'admin 也不能确认别人的归还（没有 psql 就没法把用户提成 admin）'
else
  db "UPDATE users SET role='admin', updated_at=now() WHERE username='${M5_TAG}o'" >/dev/null
  req POST /api/auth/login "{\"username\":\"${M5_TAG}o\",\"password\":\"$M5_P\"}"
  M5_ADMIN="$(jval "$BODY" '"token":"([^"]{20,})"')"
  req POST "/api/returns/$M5_R1/confirm" '{"owner_note":"admin 替拾主做的判断"}' "$M5_ADMIN"
  check '   admin 确认别人的归还 → HTTP 403（要删的是 #46，不是这里的 #25）' '403' "$STATUS"
  check '     code 是 FORBIDDEN'  '"code":"FORBIDDEN"' "$BODY"
  db "UPDATE users SET role='user', updated_at=now() WHERE username='${M5_TAG}o'" >/dev/null
fi

# ---------- §13 第 6 步：拒绝这一支 ----------
# 这一整段就是这个里程碑最需要从 HTTP 外面验一次的东西：
# **rejected 拒绝的是这条归还确认记录，不是这条帖子。** 帖子必须还是 open。
# 它在第②层是无条件覆盖的（m5_return_reject_test.go），放在这里的理由是那个
# 最坏的实现形状：有人在 Reject 里顺手写一句 UPDATE items SET status='closed'，
# 理由是「都谈崩了就把帖关了吧」—— 那一句话会让失主再也收不到第二个人的归还确认。
req POST "/api/returns/$M5_R1/reject" "{\"owner_note\":\"$M5_REJ_NOTE\"}" "$M5_finder_TOK"
check '拾主拒绝这条归还确认 → HTTP 200'  '200'                 "$STATUS"
check '   记录的状态是 rejected'         '"status":"rejected"' "$BODY"
if [[ "$BODY" == *'"credit_delta"'* ]]; then
  printf '  %sFAIL%s #26 的响应里有 credit_delta —— 拒绝不产生任何分数变化，连 0 都不该返回\n' \
    "$RED" "$RESET"; fail=$((fail+1))
else
  printf '  %sPASS%s   响应里没有 credit_delta（返回 0 会让人以为「本来要加分但没加」）\n' \
    "$GREEN" "$RESET"; pass=$((pass+1))
fi

req GET "/api/returns/$M5_R1" '' "$M5_liu_TOK"
check '   提交人查看详情能读到那句拒绝理由（原样，不改写）' "\"owner_note\":\"$M5_REJ_NOTE\"" "$BODY"

if [[ "$HAS_PSQL" != 1 ]]; then
  skip '拒绝之后帖子仍是 open、零加分、只通知提交人（没有 psql，数不了库）'
else
  check '   ⚠ 帖子 status 必须仍是 open（§13 第 6 步的字面判据）' 'open' \
    "$(db "SELECT status FROM items WHERE id=$M5_FOUND")"
  check '   记录这边是 rejected + review_kind=owner + reviewer=发帖人' '1' \
    "$(db "SELECT (count(*)=1)::int FROM item_returns WHERE id=$M5_R1 AND status='rejected' AND review_kind='owner' AND reviewer_id=$M5_finder_ID")"
  check '   只有提交人收到 1 条 return_rejected' '1' "$(n_count "$M5_liu_ID" return_rejected)"
  check '   发帖人这边一条 return_rejected 都没有（他是做拒绝的那个人）' '0' \
    "$(n_count "$M5_finder_ID" return_rejected)"
  check '   配对的失主们一条提示都没收到（拒绝是他们自己帖子上没发生的事）' '0' \
    "$(n_count "$M5_ownera_ID" item_returned_hint)"
  check '   两个人都还是 100 分' '100100' \
    "$(user_credit "$M5_liu_ID")$(user_credit "$M5_finder_ID")"
  check '   而且一条流水都没写（拒绝不加分这件事在账本上也是干净的）' '0' \
    "$(db "SELECT count(*) FROM credit_logs WHERE user_id=$M5_liu_ID OR user_id=$M5_finder_ID")"
fi

# ---------- 撤销这一支：提交人自己收回来，零副作用 ----------
# 被拒之后同一人还能再提交 —— 那条唯一索引只挡 pending，rejected 那一行不占位。
# 这一条和上面那条 RETURN_DUPLICATE 正好是一枚硬币的两面，所以紧挨着写。
req POST "/api/items/$M5_FOUND/returns" "{\"message\":\"$M5_MSG\",\"proof_image_path\":\"$M5_PROOF\"}" "$M5_liu_TOK"
check '被拒之后重新提交 → HTTP 200（唯一索引只挡 pending）' '200' "$STATUS"
M5_R2="$(jval "$BODY" '"data":\{"id":([0-9]+)')"
if [[ -n "$M5_R2" && "$M5_R2" != "$M5_R1" ]]; then
  printf '  %sPASS%s   重新提交是一条**新记录**（%s → %s），不是把 rejected 那行改回 pending\n' \
    "$GREEN" "$RESET" "$M5_R1" "$M5_R2"; pass=$((pass+1))
else
  printf '  %sFAIL%s 重新提交拿到的 id 是 %s（上一条是 %s）\n' "$RED" "$RESET" "$M5_R2" "$M5_R1"; fail=$((fail+1))
fi

# ⚠ 这里拍的是**快照**而不是字面值，而且必须拍快照：小刘这一节会对同一条帖提交三次
#  （被拒的那条、要撤销的那条、最后确认的那条），每一次提交都给发帖人发一条
#  return_submitted，所以走到撤销这一步时拾主手上已经是 2 条了。
#  这一条要测的是「撤销这一次有没有多发」，那是增量的问题 —— 写死数字会测成假红。
notif_finder_before="$(db "SELECT count(*) FROM notifications WHERE user_id=$M5_finder_ID")"
req POST "/api/returns/$M5_R2/cancel" '' "$M5_liu_TOK"
check '提交人撤销自己那条 pending → HTTP 200' '200'                  "$STATUS"
check '   状态是 cancelled'                   '"status":"cancelled"' "$BODY"
req POST "/api/returns/$M5_R2/cancel" '' "$M5_liu_TOK"
check '   撤销一条已经 cancelled 的 → HTTP 409' '409'                     "$STATUS"
check '     code 是 RETURN_ILLEGAL_TRANSITION'  '"code":"RETURN_ILLEGAL_TRANSITION"' "$BODY"
req POST "/api/returns/$M5_R2/cancel" '' "$M5_finder_TOK"
check '   发帖人不能替别人撤销（那是提交人的记录）→ HTTP 403' '403' "$STATUS"

if [[ "$HAS_PSQL" != 1 ]]; then
  skip '撤销是零副作用的（没有 psql，数不了通知也读不到那三个归属列）'
else
  check '   拾主的通知数一条都没涨（他那两条是两次提交各发的，不是撤销发的）' \
    "$notif_finder_before" \
    "$(db "SELECT count(*) FROM notifications WHERE user_id=$M5_finder_ID")"
  check '   小刘这边也还是只有被拒那一条' '1' \
    "$(db "SELECT count(*) FROM notifications WHERE user_id=$M5_liu_ID")"
  # cancelled 不是「被审」，所以 reviewer_id / review_kind 两列必须是 NULL ——
  # 但 reviewed_at **会填**：它的实际含义是「这条记录退出 pending 的时刻」，
  # 而「谁做的、有没有社区含义」那两问仍然由 review_kind 为 NULL 来回答
  # （repo/item_return.go 的 Cancel 顶上把这条分工写死了）。
  check '   那一行没有 reviewer_id、没有 review_kind，但 reviewed_at 填了' '1' \
    "$(db "SELECT (count(*)=1)::int FROM item_returns WHERE id=$M5_R2 AND reviewer_id IS NULL AND review_kind IS NULL AND reviewed_at IS NOT NULL")"
fi

# ---------- 判据②–⑥：确认这一支，六个后果 ----------
req POST "/api/items/$M5_FOUND/returns" "{\"message\":\"$M5_MSG\",\"proof_image_path\":\"$M5_PROOF\"}" "$M5_liu_TOK"
M5_R3="$(jval "$BODY" '"data":\{"id":([0-9]+)')"
req POST "/api/returns/$M5_R3/confirm" "{\"owner_note\":\"$M5_NOTE\"}" "$M5_finder_TOK"
check '拾主确认归还 → HTTP 200' '200' "$STATUS"
# ⚠ 这里不能用 '"status":' 做子串：响应里有两个 status —— ItemSummary 嵌套的那个
#   是帖子状态（这步之后是 closed），外层这个才是归还记录状态。判据①问的是后者。
check '   ① 记录状态是 confirmed' '"status":"confirmed"' "$BODY"
check '   ① credit_delta 是 10（发帖人自己那一份，实际生效值）' '"credit_delta":10' "$BODY"

if [[ "$HAS_PSQL" != 1 ]]; then
  skip '确认之后那六个后果（没有 psql，只能看见 HTTP 响应，而六个里有五个不在响应里）'
else
  check '   ① 归属写进了数据：review_kind=owner、reviewer_id=发帖人' '1' \
    "$(db "SELECT (count(*)=1)::int FROM item_returns WHERE id=$M5_R3 AND status='confirmed' AND review_kind='owner' AND reviewer_id=$M5_finder_ID")"
  check '   ② 帖子变成 closed（东西已经还回去了，这是事实）' 'closed' \
    "$(db "SELECT status FROM items WHERE id=$M5_FOUND")"
  check '   ③ 发帖人 100 → 110' '110' "$(user_credit "$M5_finder_ID")"
  check '   ③ 提交人 100 → 102' '102' "$(user_credit "$M5_liu_ID")"
  check '   ④ 拾主那条流水 +10、理由带 owner' '1' \
    "$(db "SELECT (count(*)=1)::int FROM credit_logs WHERE user_id=$M5_finder_ID AND delta=10 AND reason='return_confirmed_as_owner' AND ref_type='item_return' AND ref_id=$M5_R3")"
  check '   ④ 小刘那条 +2、理由带 submitter、挂在同一条归还确认上' '1' \
    "$(db "SELECT (count(*)=1)::int FROM credit_logs WHERE user_id=$M5_liu_ID AND delta=2 AND reason='return_confirmed_as_submitter' AND ref_type='item_return' AND ref_id=$M5_R3")"
  check '   ⑤ 提交人收到 1 条 return_confirmed，且挂着这条记录' '1' \
    "$(db "SELECT (count(*)=1)::int FROM notifications WHERE user_id=$M5_liu_ID AND type='return_confirmed' AND return_id=$M5_R3")"
  # ⑥ 是这一节唯一「不在任何响应里」的后果，也是唯一只能靠数库来验的：
  # 配对的 lost 帖作者各收到一条 item_returned_hint。
  # 失主 A 有**两条**帖命中这条 found 帖（M5_A1、M5_A2），但只该收到 **1** 条 ——
  # 台账读的是 DISTINCT user_id，去重按**作者**而不是按帖子。
  # 少了这一条，「同一人两帖发两条骚扰通知」那个实现能一路绿到底。
  check '   ⑥ 失主 A 收到 1 条 item_returned_hint（他命中两条帖，也只发一条）' '1' \
    "$(n_count "$M5_ownera_ID" item_returned_hint)"
  check '   ⑥ 失主 B 收到 1 条' '1' "$(n_count "$M5_ownerb_ID" item_returned_hint)"
  check '   ⑥ 提示挂的是这条 found 帖' '1' \
    "$(db "SELECT (count(*)=1)::int FROM notifications WHERE user_id=$M5_ownera_ID AND type='item_returned_hint' AND item_id=$M5_FOUND")"
  check '   ⑥ 拾主自己不会收到提示（他是把东西还出去的那个人）' '0' \
    "$(n_count "$M5_finder_ID" item_returned_hint)"
  check '   小刘全程只被通知过两次（一次被拒、一次确认）' '2' \
    "$(db "SELECT count(*) FROM notifications WHERE user_id=$M5_liu_ID")"

  # §13 第 10 步那条审计不变式：有终态的记录必须有归属。
  # 它是对**全表**说的，不只本轮 —— 上一轮留下的行同样不能违反它。
  check '   全表没有「已审完却没有 review_kind」的行' '0' \
    "$(db "SELECT count(*) FROM item_returns WHERE status IN ('confirmed','rejected') AND review_kind IS NULL")"
  check '   也没有「还 pending 却已经有 review_kind」的行' '0' \
    "$(db "SELECT count(*) FROM item_returns WHERE status='pending' AND review_kind IS NOT NULL")"
fi

# 再确认一次：同一条记录不能有两个终态。
req POST "/api/returns/$M5_R3/confirm" '{"owner_note":"再点一次"}' "$M5_finder_TOK"
check '对已经 confirmed 的记录再确认 → HTTP 409' '409' "$STATUS"
check '   code 是 RETURN_ILLEGAL_TRANSITION' '"code":"RETURN_ILLEGAL_TRANSITION"' "$BODY"
# ⚠ 不复活：confirm 那条 UPDATE items 带着 `AND status='open'`。
# 这里验它的另一面：确认完成之后这条帖**不再收新的归还确认**。
req POST "/api/items/$M5_FOUND/returns" "{\"message\":\"$M5_MSG\",\"proof_image_path\":\"$M5_PROOF\"}" "$M5_ownerb_TOK"
check '   东西还回去之后别人再提交 → HTTP 409 ITEM_CLOSED' '409' "$STATUS"

# ---------- #28 / #29：同一条 SQL，只差「按谁过滤」 ----------
req GET /api/my/returns/submitted '' "$M5_liu_TOK"
check '#28 小刘查我提交的 → HTTP 200' '200' "$STATUS"
check '   正好三条（被拒、撤销、确认）' '"total":3' "$BODY"
check '   最新那条排第一（刚确认的那条）' '"list":[{"id":'"$M5_R3"',' "$BODY"
# ⚠ 信封**自己**就带一个顶层的 "message":"ok"，所以在整个 BODY 里搜 '"message"'
#   永远搜得到 —— 上一版就是这么假红的。这里不搜「有没有」，而是数「出现几次」：
#   信封恒 1 次，多于 1 次就说明列表元素里混进了 message。
#   （proof_image 和 submitter 不在信封的字段名里，可以直接搜 absence。
#    别用 ${BODY#*'"data":} 这种剥信封的写法：${} 里的单引号会让 bash 的解析整个错位，
#    报出来的是「某某: unbound variable」，和真实原因毫无关系。）
m5_msg_n=$(printf '%s' "$BODY" | grep -o '"message":' | wc -l)
if [[ "$m5_msg_n" -le 1 && "$BODY" != *'"proof_image'* && "$BODY" != *'"submitter"'* ]]; then
  printf '  %sPASS%s   列表里没有 message（信封那 1 条之外）、没有凭证图、也没有提交人\n' "$GREEN" "$RESET"; pass=$((pass+1))
else
  printf '  %sFAIL%s #28 的列表里混进了判断现场才需要的字段（message 出现了 %s 次）\n' "$RED" "$RESET" "$m5_msg_n"
  fail=$((fail+1))
fi
# 列表是索引，#24 才是判断现场。少传这些字段不只是省带宽，它让「列表页不该有
# 确认/拒绝按钮」这件事在前端没有别的选择。
check '   摘要里的 found 帖 contact 是 null（和其他摘要端点同一条规则）' '"contact":null' "$BODY"
check '   摘要带的是那条帖子而不是读者的信息（author 是拾主）' "\"author_id\":$M5_finder_ID," "$BODY"

req GET /api/my/returns/received '' "$M5_finder_TOK"
check '#29 拾主查我收到的 → HTTP 200' '200' "$STATUS"
check '   同样三条（都是提在他那条帖上的）' '"total":3' "$BODY"
req GET "/api/my/returns/received?status=pending" '' "$M5_finder_TOK"
check '   ?status=pending → 空列表（三条都被处理完了）' '"list":[],"total":0' "$BODY"
req GET "/api/my/returns/received?status=rejected" '' "$M5_finder_TOK"
check '   ?status=rejected → 只剩被拒那条' '"total":1' "$BODY"
req GET "/api/my/returns/received?status=blobfish" '' "$M5_finder_TOK"
check '   ?status= 表外的值 → HTTP 400' '400' "$STATUS"
check '     错误指向 status'             '"field":"status"' "$BODY"
# 这两个列表都**只**按当前用户过滤：?user_id= 传谁都无效（同 #19/#30 那条纪律 ——
# 让它可指定就是全站最大的个人记录泄漏面）。判据是小刘换了个 user_id 还是看见自己三条。
req GET "/api/my/returns/submitted?user_id=$M5_ownera_ID&item_id=$M5_FOUND&submitter_id=$M5_finder_ID" '' "$M5_liu_TOK"
check '   ?user_id= / ?item_id= / ?submitter_id= 全部被忽略（还是他那三条）' '"total":3' "$BODY"
req GET /api/my/returns/submitted
check '   不带 token → HTTP 401' '401'                   "$STATUS"
check '     code 是 UNAUTHORIZED' '"code":"UNAUTHORIZED"' "$BODY"

# ---------- #33：让 110 这个数字可解释 ----------
req GET /api/my/credit-logs '' "$M5_finder_TOK"
check '#33 拾主查积分流水 → HTTP 200' '200' "$STATUS"
check '   带着结论（credit_score），前端不必再拉一次 #5' '"credit_score":110' "$BODY"
check '   一条流水：+10、理由是 return_confirmed_as_owner' '"delta":10' "$BODY"
check '   ref_type 是 item_return'   '"ref_type":"item_return"' "$BODY"
check '   ref_id 就是那条归还确认'   "\"ref_id\":$M5_R3"        "$BODY"
# user_id 不在列表元素里：收件人是「你」，不给前端一个可以拿去猜别人 id 的字段。
# 这条可以直接搜整个 BODY —— 信封的四个键（code / message / data / request_id）里
# 没有 user_id，所以不存在 #28 那种「顶层同名键」的假红。
if [[ "$BODY" == *'"user_id"'* ]]; then
  printf '  %sFAIL%s #33 的流水里带了 user_id\n' "$RED" "$RESET"; fail=$((fail+1))
else
  printf '  %sPASS%s   流水元素里没有 user_id\n' "$GREEN" "$RESET"; pass=$((pass+1))
fi
# 这条端点存在的唯一理由就是对账：Σ流水 + 100 == credit_score。
# 从**响应**里算而不是从库里算 —— 用户看到的就是响应，对不上就是他的问题。
sum="$(printf '%s' "$BODY" | grep -oE '"delta":-?[0-9]+' | sed 's/"delta"://' | awk '{s+=$1} END{print s+0}')"
score="$(jval "$BODY" '"credit_score":([0-9-]+)')"
if [[ "$((sum + 100))" == "$score" ]]; then
  printf '  %sPASS%s   Σ流水(%s) + 100 == credit_score(%s)\n' "$GREEN" "$RESET" "$sum" "$score"
  pass=$((pass+1))
else
  printf '  %sFAIL%s   对不上账：Σ流水=%s、credit_score=%s\n' "$RED" "$RESET" "$sum" "$score"
  fail=$((fail+1))
fi

req GET /api/my/credit-logs '' "$M5_liu_TOK"
check '   小刘那边是 +2 那条（提交人一份）' '"reason":"return_confirmed_as_submitter"' "$BODY"
check '   他的分数 102'                     '"credit_score":102' "$BODY"
req GET /api/my/credit-logs
check '   匿名 → HTTP 401'                  '401' "$STATUS"
req GET "/api/my/credit-logs?user_id=$M5_finder_ID" '' "$M5_liu_TOK"
check '   ?user_id= 同样被忽略（只能读自己的）' '"credit_score":102' "$BODY"
req GET "/api/my/credit-logs?page_size=101" '' "$M5_liu_TOK"
check '   page_size 超上限 → HTTP 400'       '400'                   "$STATUS"
check '     错误指向 page_size'              '"field":"page_size"'    "$BODY"

printf '%s本轮 M5 留下的冒烟用户：m5…finder / …liu / …ownera / …ownerb / …out（id=%s）%s\n' \
  "$DIM" "$M5_out_ID" "$RESET"

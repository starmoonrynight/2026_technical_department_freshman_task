# ---------------------------------------------------------------- M2 字典
# deps: m1
section "M2 字典：#7 /api/categories、#8 /api/locations"

req GET /api/categories
check '#7 匿名返回 HTTP 200（发帖表单要能在没有登录时渲染）' '200' "$STATUS"
check '  code 是 OK'                  '"code":"OK"'  "$BODY"
check '  节点带 level'                '"level":'     "$BODY"
check '  节点带 sort_order'           '"sort_order":' "$BODY"
check '  叶子节点的 children 是 []'   '"children":[]' "$BODY"
check '  已知叶子：数码电子 → 手机(id=10,level=2)' '{"id":10,"name":"手机","level":2' "$BODY"
if [[ "$BODY" == *'"children":null'* ]]; then
  printf '  %sFAIL%s 出现了 "children":null —— 前端 data.children.map() 会直接崩\n' "$RED" "$RESET"; fail=$((fail+1))
else
  printf '  %sPASS%s 没有 "children":null\n' "$GREEN" "$RESET"; pass=$((pass+1))
fi
if [[ "$BODY" == *parent_id* ]]; then
  printf '  %sFAIL%s 树形响应里出现了 parent_id —— 嵌套已经把父子关系表达完了\n' "$RED" "$RESET"; fail=$((fail+1))
else
  printf '  %sPASS%s 响应里没有 parent_id\n' "$GREEN" "$RESET"; pass=$((pass+1))
fi

req GET /api/locations
check '#8 匿名返回 HTTP 200'          '200'          "$STATUS"
check '  code 是 OK'                  '"code":"OK"'  "$BODY"
check '  节点带 is_freeform'          '"is_freeform":' "$BODY"
check '  已知叶子：教学区（南侧）→ 场馆与公共建筑 → 图书馆(id=70,level=3)' \
      '{"id":70,"name":"图书馆","level":3' "$BODY"
check '  一级叶子「其他」也可选（§3.6 的特例：level=1 但没有子节点）' \
      '{"id":3,"name":"其他","level":1,"is_freeform":true' "$BODY"

# 发帖后面要用这两个 id。它们在 000001 迁移里是显式 INSERT 的，
# 所以写死是对的 —— 但先确认上面的树里真的有，否则迁移一改这里会静默地发到错的分类上。
CAT_ID=10; LOC_ID=70

# ---------------------------------------------------------------- M2 上传
section "M2 上传：#6 /api/uploads、#40 /uploads/*filepath"

req_upload "" "$png_path"
check '#6 不带 token 返回 HTTP 401'   '401'                 "$STATUS"
check '  code 是 UNAUTHORIZED'        '"code":"UNAUTHORIZED"' "$BODY"

req_upload "$TOKEN" "$png_path"
check '#6 传一张图返回 HTTP 200'      '200'          "$STATUS"
check '  code 是 OK'                  '"code":"OK"' "$BODY"
PIC1="$(jval "$BODY" '"path":"([^"]+\.png)"')"
PIC1_URL="$(jval "$BODY" '"url":"([^"]+)"')"
if [[ -n "$PIC1" ]]; then
  printf '  %sPASS%s 拿到 path（%s）\n' "$GREEN" "$RESET" "$PIC1"; pass=$((pass+1))
else
  printf '  %sFAIL%s 没抠到 path，后面的上传检查会连带失败\n' "$RED" "$RESET"; fail=$((fail+1))
fi
# 按日期分目录 = UTC 的 YYYY/MM；文件名是 32 位十六进制（crypto/rand，猜不到）。
# ⚠ path 是**相对于 UPLOAD_DIR** 的，不带 uploads/ 前缀 —— 前缀属于 url，不属于 path。
#    这两个字段各管一件事：path 给 #13 的 image_paths 用（要落库），
#    url 给 <img src> 用（要过 HTTP）。把它们混成一个字符串的话，
#    落库的那一列就同时编码了「怎么存」和「怎么访问」，改挂载点得回来改数据。
if [[ "$PIC1" =~ ^$(date -u +%Y/%m)/[0-9a-f]{32}\.png$ ]]; then
  printf '  %sPASS%s path 形状正确（年/月/32位十六进制.png）\n' "$GREEN" "$RESET"; pass=$((pass+1))
else
  printf '  %sFAIL%s path 形状不对：%s\n' "$RED" "$RESET" "$PIC1"; fail=$((fail+1))
fi
# url 就是 /uploads/ + path —— #40 的挂载点决定了这个拼法没有别的 Possible
if [[ "$PIC1_URL" == "/uploads/$PIC1" ]]; then
  printf '  %sPASS%s url = /uploads/ + path\n' "$GREEN" "$RESET"; pass=$((pass+1))
else
  printf '  %sFAIL%s url=%s 而 path=%s，两者对不上\n' "$RED" "$RESET" "$PIC1_URL" "$PIC1"; fail=$((fail+1))
fi
[[ -f "$UPLOAD_ABS/$PIC1" ]] && \
  { printf '  %sPASS%s 文件真的落到了磁盘上\n' "$GREEN" "$RESET"; pass=$((pass+1)); } || \
  { printf '  %sFAIL%s 接口说成功了但 %s 不存在\n' "$RED" "$RESET" "$UPLOAD_ABS/$PIC1"; fail=$((fail+1)); }

# 内容嗅探，不看文件名：内容是 PNG、文件叫 shell.php，必须按 PNG 存下来。
# 这一条防的是「上传目录能被写成一个可执行文件」，以及「前端按用户给的后缀渲染」。
req_upload "$TOKEN" "$png_named_php_path"
check_re '  内容是 PNG 但文件名叫 shell.php → 仍然存成 .png' '"path":"[^"]*\.png"' "$BODY"
PIC2="$(jval "$BODY" '"path":"([^"]+\.png)"')"
if [[ "$PIC2" == *shell* ]]; then
  printf '  %sFAIL%s 存下来的文件名沿用了用户给的 shell.php\n' "$RED" "$RESET"; fail=$((fail+1))
else
  printf '  %sPASS%s 文件名是我们生成的，不含用户给的后缀\n' "$GREEN" "$RESET"; pass=$((pass+1))
fi

# 第三张图专门留给 #42 之后的两段：「admin 也不能删图」需要一张还活着的图，
# 而那一节前面已经把前两张删干净了（一张是判据正文，一张是「重复删除不是幂等成功」）。
req_upload "$TOKEN" "$png_path"
PIC3="$(jval "$BODY" '"path":"([^"]+\.png)"')"

req_upload "$TOKEN" "$txt_path"
check '  传一个文本文件返回 HTTP 415' '415'                    "$STATUS"
check '  code 是 FILE_TYPE_UNSUPPORTED' '"code":"FILE_TYPE_UNSUPPORTED"' "$BODY"

req_upload "$TOKEN" "$png_path" image
check '  表单字段名不叫 file → HTTP 400' '400'          "$STATUS"
check '  code 是 VALIDATION'  '"code":"VALIDATION"' "$BODY"

# ---- #40 静态文件 ----
if [[ -n "$PIC1" ]]; then
  out="$(curl -s -D "$tmp_headers" -o "$tmp_dir/downloaded.bin" -w '%{http_code}' "$BASE/uploads/$PIC1")"
  check '#40 匿名 GET 上传文件返回 HTTP 200' '200' "$out"
  check '  Content-Type 是 image/png' 'image/png' "$(cat "$tmp_headers")"
  if cmp -s "$png_path" "$tmp_dir/downloaded.bin"; then
    printf '  %sPASS%s 下载回来的字节和上传的完全一致\n' "$GREEN" "$RESET"; pass=$((pass+1))
  else
    printf '  %sFAIL%s 字节不一致（静态文件被改过了？）\n' "$RED" "$RESET"; fail=$((fail+1))
  fi
  # 首字节必须是 0x89 —— PNG 魔数的第一个字节。
  # 如果这里回来的是 '{'（0x7b），说明拿到的是 JSON 信封而不是图片本身。
  # cmp 已经能抓到这种情况，但那条失败信息会说「字节不一致」，
  # 而这一条会直接说「响应体不是二进制」，后者才是排查时想知道的那句话。
  first_byte="$(head -c 1 "$tmp_dir/downloaded.bin" | od -An -tx1 | tr -d ' \n')"
  if [[ "$first_byte" == "89" ]]; then
    printf '  %sPASS%s 响应体是图片二进制（首字节 0x89），不是 JSON 信封\n' "$GREEN" "$RESET"; pass=$((pass+1))
  else
    printf '  %sFAIL%s 响应体首字节是 0x%s，不是 PNG 的 0x89\n' "$RED" "$RESET" "$first_byte"; fail=$((fail+1))
  fi
fi

# 目录穿越。%2e 是 URL 编码的点：gin 的 Static 会先解码，所以能不能拦住取决于实现，
# 而不是取决于 curl 有没有把 .. 送出去（curl 自己不会归一化 URL 路径）。
for trav in '../backend/.env' '..%2F..%2Fbackend%2F.env' '%2e%2e%2f%2e%2e%2fbackend%2f.env'; do
  out="$(curl -s -o "$tmp_dir/trav.bin" -w '%{http_code}' "$BASE/uploads/$trav")"
  if [[ "$out" == "200" ]] && grep -q 'JWT_SECRET' "$tmp_dir/trav.bin" 2>/dev/null; then
    printf '  %sFAIL%s 穿越到 %s 把 .env 读出来了\n' "$RED" "$RESET" "$trav"; fail=$((fail+1))
  else
    printf '  %sPASS%s 穿越 %s 没有泄漏文件（HTTP %s）\n' "$GREEN" "$RESET" "$trav" "$out"; pass=$((pass+1))
  fi
done

out="$(curl -s "$BASE/uploads/uploads/2026/01/00000000000000000000000000000000.png")"
check '#40 文件不存在也走统一信封（不是 gin 的纯文本 404）' '"code":"NOT_FOUND"' "$out"

# ---------------------------------------------------------------- M2 发帖
section "M2 发帖：#13 POST /api/items"

# 第二个用户：判据里「非本人改他人帖得 FORBIDDEN」「别人删这张图得 FORBIDDEN」都要他。
U2="smoke2$(date +%s)$RANDOM"
req POST /api/auth/register "{\"username\":\"$U2\",\"password\":\"$P\"}"
OTHER_ID="$(jval "$BODY" '"data":\{"id":([0-9]+)')"
req POST /api/auth/login "{\"username\":\"$U2\",\"password\":\"$P\"}"
TOKEN2="$(jval "$BODY" '"token":"([^"]{20,})"')"

req GET /api/auth/me '' "$TOKEN"
MY_ID="$(jval "$BODY" '"data":\{"id":([0-9]+)')"

if [[ -z "$TOKEN2" || -z "$MY_ID" || -z "$OTHER_ID" ]]; then
  printf '  %sFAIL%s 第二个冒烟用户没建起来（me.id=%s other.id=%s），FORBIDDEN 那几条测不了\n' \
    "$RED" "$RESET" "$MY_ID" "$OTHER_ID"; fail=$((fail+1))
else
  printf '  %sPASS%s 冒烟用户 %s(id=%s) 和 %s(id=%s)\n' \
    "$GREEN" "$RESET" "$U" "$MY_ID" "$U2" "$OTHER_ID"; pass=$((pass+1))
fi

lost_json="$(printf '{"item_type":"lost","title":"冒烟：黑色长款钱包","description":"在图书馆三楼丢的","category_id":%d,"location_id":%d,"location_detail":"三楼靠窗第二排","last_seen_at":"2026-10-01T09:00:00+08:00","lost_at":"2026-10-01T12:30:00+08:00","contact":"13800000000","image_paths":["%s","%s","%s"]}' \
  "$CAT_ID" "$LOC_ID" "$PIC1" "$PIC2" "$PIC3")"
req POST /api/items "$lost_json" "$TOKEN"
check '建 lost（带三张图）返回 HTTP 200' '200'          "$STATUS"
LOST_ID="$(itemIDOf "$BODY")"
check '  data.item.status 是 open'    '"status":"open"' "$BODY"
check '  三张图都挂上了，从 images[0] 开始有序' '"images":[{"id":' "$BODY"
# 请求里写的是 +08:00，回来的必须是同一个时刻的 UTC 写法（§3.1「时间一律存 UTC」）
check '  last_seen_at 转成了 UTC'     '"last_seen_at":"2026-10-01T01:00:00Z"' "$BODY"
check '  lost_at 转成了 UTC'          '"lost_at":"2026-10-01T04:30:00Z"'      "$BODY"
check '  lost 帖没有 found_at'        '"found_at":""'  "$BODY"
check '  自己的帖子看得到 contact'    '"contact":"13800000000"' "$BODY"

found_json="$(printf '{"item_type":"found","title":"冒烟：捡到一个卡包","description":"已经交到前台","category_id":%d,"location_id":%d,"found_at":"2026-10-02T15:00:00+08:00","contact":"问图书馆前台"}' \
  "$CAT_ID" "$LOC_ID")"
req POST /api/items "$found_json" "$TOKEN"
check '建 found 返回 HTTP 200'        '200'          "$STATUS"
FOUND_ID="$(itemIDOf "$BODY")"
check '  found 帖没有 lost_at / last_seen_at' '"lost_at":""' "$BODY"

# ---- 判据点名的那几条拒绝 ----
items_before="$(db 'SELECT count(*) FROM items')"
req POST /api/items '{"item_type":"lost","title":"联系方式是空的","contact":"","category_id":10,"location_id":70,"last_seen_at":"2026-10-01T09:00:00+08:00","lost_at":"2026-10-01T12:00:00+08:00"}' "$TOKEN"
check 'contact 为空 → HTTP 400'       '400'                  "$STATUS"
check '  code 是 VALIDATION'          '"code":"VALIDATION"' "$BODY"
check '  错误指向 contact 字段（前端要画红框）' '"field":"contact"' "$BODY"

req POST /api/items '{"item_type":"lost","title":"联系方式是纯空白","contact":"   ","category_id":10,"location_id":70,"last_seen_at":"2026-10-01T09:00:00+08:00","lost_at":"2026-10-01T12:00:00+08:00"}' "$TOKEN"
check 'contact 是纯空白 → 同样是 VALIDATION' '"code":"VALIDATION"' "$BODY"
req POST /api/items '{"item_type":"lost","title":"联系方式是全角空格","contact":"　","category_id":10,"location_id":70,"last_seen_at":"2026-10-01T09:00:00+08:00","lost_at":"2026-10-01T12:00:00+08:00"}' "$TOKEN"
check 'contact 是全角空格 U+3000 → VALIDATION' '"code":"VALIDATION"' "$BODY"

req POST /api/items '{"item_type":"lost","title":"时间填反了","contact":"13800000000","category_id":10,"location_id":70,"last_seen_at":"2026-10-05T09:00:00+08:00","lost_at":"2026-10-01T09:00:00+08:00"}' "$TOKEN"
check 'last_seen_at 晚于 lost_at → HTTP 400' '400'          "$STATUS"
check '  错误同时指向两个时间字段'    '"field":"last_seen_at"' "$BODY"

req POST /api/items '{"item_type":"found","title":"found 帖填了 last_seen_at","contact":"13800000000","category_id":10,"location_id":70,"found_at":"2026-10-02T15:00:00+08:00","last_seen_at":"2026-10-01T09:00:00+08:00"}' "$TOKEN"
check 'found 帖填 last_seen_at → HTTP 400' '400' "$STATUS"
check '  错误指向 last_seen_at'       '"field":"last_seen_at"' "$BODY"

req POST /api/items '{"item_type":"lost","title":"选了大类","contact":"13800000000","category_id":1,"location_id":70,"last_seen_at":"2026-10-01T09:00:00+08:00","lost_at":"2026-10-01T12:00:00+08:00"}' "$TOKEN"
check 'category_id 用一级大类 → HTTP 400' '400' "$STATUS"
check '  错误指向 category_id'        '"field":"category_id"' "$BODY"
req POST /api/items '{"item_type":"lost","title":"选了有子节点的地点","contact":"13800000000","category_id":10,"location_id":9,"last_seen_at":"2026-10-01T09:00:00+08:00","lost_at":"2026-10-01T12:00:00+08:00"}' "$TOKEN"
check 'location_id 用中间层 → HTTP 400' '400'        "$STATUS"

req POST /api/items '{"item_type":"lost","title":"图片路径是编的","contact":"13800000000","category_id":10,"location_id":70,"last_seen_at":"2026-10-01T09:00:00+08:00","lost_at":"2026-10-01T12:00:00+08:00","image_paths":["../../etc/passwd"]}' "$TOKEN"
check 'image_paths 不是 #6 返回的形状 → HTTP 400' '400' "$STATUS"
check '  错误指向 image_paths[0]'     '"field":"image_paths' "$BODY"

req POST /api/items '{"item_type":"lost","title":"没有登录","contact":"13800000000","category_id":10,"location_id":70,"last_seen_at":"2026-10-01T09:00:00+08:00","lost_at":"2026-10-01T12:00:00+08:00"}'
check '发帖不带 token → HTTP 401'     '401'          "$STATUS"

# 校验失败了却还是建出了帖子 —— 这一条只有直接数库能发现，接口上完全看不出来：
# 一个「先插再校验、校验不过就返回错误但忘了回滚」的实现，
# 上面每一条错误码都是对的，而库里已经堆了一堆不该存在的行。
#
# 用「这一段的 before/after 相等」而不是「按标题数某几条」：标题是中文，
# 而 psql 的 -c 参数同样会踩 Git Bash 的代码页转换（变成 ???），
# 那样这条检查会永远匹配到 0 行、永远假绿。数总数不需要任何字符串参数。
if [[ "$HAS_PSQL" == 1 ]]; then
  items_after="$(db 'SELECT count(*) FROM items')"
  if [[ "$items_before" == "$items_after" ]]; then
    printf '  %sPASS%s 上面 %s 条被拒的帖子一条都没进数据库（items 仍是 %s 行）\n' \
      "$GREEN" "$RESET" 9 "$items_before"; pass=$((pass+1))
  else
    printf '  %sFAIL%s 校验失败了却还是建出了帖子：items 从 %s 行变成 %s 行\n' \
      "$RED" "$RESET" "$items_before" "$items_after"; fail=$((fail+1))
  fi
else
  skip '被拒的帖子一条都没进数据库（没有 psql，无法直接数库）'
fi

# ⚠ 这两条是「不校验内容」这条契约的正面证据，缺了上面三条毫无意义：
#    一个「把所有 contact 都拒掉」的实现能让上面全绿。
#
# 刻意放在数库那一段**之后**：它们是真的会建出帖子来的，混在计数窗口里
# 会让上面那条 before/after 白白红一次。契约测试里「成功」和「被拒」两组
# 用例共用一个计数断言时，必须把它们隔开。
req POST /api/items '{"item_type":"found","title":"联系方式写无","contact":"无","category_id":10,"location_id":70,"found_at":"2026-10-02T15:00:00+08:00"}' "$TOKEN"
check 'contact 填「无」→ HTTP 200（平台不校验内容）' '200'          "$STATUS"
check '  code 是 OK'                  '"code":"OK"' "$BODY"
req POST /api/items '{"item_type":"found","title":"联系方式写问宿舍阿姨","contact":"问宿舍阿姨","category_id":10,"location_id":70,"found_at":"2026-10-02T15:00:00+08:00"}' "$TOKEN"
check 'contact 填「问宿舍阿姨」→ HTTP 200' '200'          "$STATUS"

# ---------------------------------------------------------------- M2 列表
section "M2 列表：#14 /api/items（公开）、#19 /api/my/items（JWT）"

req GET /api/items
check '#14 匿名返回 HTTP 200'         '200'          "$STATUS"
check '  分页形状是 {list,total,page,page_size}' '"page_size":20' "$BODY"
check '  摘要里是平铺的 category_name 而不是嵌套对象' '"category_name":"手机"' "$BODY"
# 「默认只看 open」用反面来验：结果里不该出现任何 deleted 的行。
# 正面那句（"status":"open" 出现了）证明不了这件事 —— 一条 open 和一堆 deleted 可以共存。
if [[ "$BODY" == *'"status":"deleted"'* ]]; then
  printf '  %sFAIL%s 广场默认结果里出现了 deleted 的帖子\n' "$RED" "$RESET"; fail=$((fail+1))
else
  printf '  %sPASS%s 默认结果里没有 deleted（软删的帖子对广场来说就是不存在）\n' "$GREEN" "$RESET"; pass=$((pass+1))
fi

# keyword=冒烟 → %E5%86%92%E7%83%9F（见上面 req 的注释：URL 里的中文会被代码页转换弄坏）
req GET '/api/items?keyword=%E5%86%92%E7%83%9F'
if [[ "$BODY" == *"冒烟"* ]]; then
  printf '  %sPASS%s keyword 命中标题\n' "$GREEN" "$RESET"; pass=$((pass+1))
else
  printf '  %sFAIL%s keyword=冒烟 什么都没搜到（本轮的帖子呢？）\n' "$RED" "$RESET"; fail=$((fail+1))
fi
# keyword=冒烟根本不存在
req GET '/api/items?keyword=%E5%86%92%E7%83%9F%E6%A0%B9%E6%9C%AC%E4%B8%8D%E5%AD%98%E5%9C%A8'
check 'keyword 不命中时 list 是 [] 而不是 null' '"list":[]' "$BODY"
check '  且 total 是 0'               '"total":0'    "$BODY"

req GET "/api/items?item_type=found&category_id=$CAT_ID"
check 'item_type + category_id 两个筛选同时用返回 HTTP 200' '200' "$STATUS"
req GET '/api/items?sort=lost_at'
check 'sort=lost_at 返回 HTTP 200'    '200'          "$STATUS"
req GET '/api/items?page=2&page_size=5'
check '分页参数返回 HTTP 200'         '200'          "$STATUS"
check '  回显 page/page_size'         '"page":2,"page_size":5' "$BODY"

req GET '/api/items?status=deleted'
check '#14 不许请求 deleted → HTTP 400' '400'        "$STATUS"
check '  错误指向 status'             '"field":"status"' "$BODY"
req GET '/api/items?page=0'
check 'page=0 → HTTP 400'             '400'          "$STATUS"
req GET '/api/items?page_size=101'
check 'page_size 超上限 → HTTP 400'   '400'          "$STATUS"
req GET '/api/items?sort=id'
check 'sort 不在白名单里 → HTTP 400（不是 500）' '400' "$STATUS"
# 编码过的分号/空格。裸分号会被 url.ParseQuery 整个拒掉，导致 sort 变成空串、
# 请求反而**成功**，那样的话这条检查就成了假绿；裸空格则连请求都发不出去。
req GET '/api/items?sort=created_at%3BDROP%20TABLE%20items'
check 'sort 里塞 SQL → HTTP 400 而不是 500' '400'    "$STATUS"
if [[ "$HAS_PSQL" == 1 ]]; then
  check '  items 表还在（注入没生效）' '1' "$(db 'SELECT (count(*)>0)::int FROM items')"
else
  skip '  items 表还在（没有 psql）'
fi

req GET /api/my/items
check '#19 不带 token → HTTP 401'     '401'          "$STATUS"
req GET /api/my/items '' "$TOKEN"
check '#19 带 token 返回 HTTP 200'    '200'          "$STATUS"
check '  自己的帖子一律带 contact（都是自己的）' '"contact":"13800000000"' "$BODY"
req GET '/api/my/items?status=deleted' '' "$TOKEN"
check '#19 允许显式查 deleted → HTTP 200（和 #14 相反）' '200' "$STATUS"
# 越权：把别人的 id 当查询参数塞进来。#19 的 user_id 来自 JWT，不该读这个参数。
# 匹配时补一个逗号：author_id 后面紧跟 author_name，少了定界符的话
# id=1 会匹配到 id=12 的帖子，那条检查就会因为「另一个人的 id 恰好以我的 id 开头」而假红。
if [[ -n "$MY_ID" ]]; then
  req GET "/api/my/items?user_id=$MY_ID" '' "$TOKEN2"
  # 先确认这个请求真的是「B 带着 token 发的」。少了这一步，一个 401 的空响应
  # 会让下面那条「没读到你的帖子」因为什么都没匹配到而假绿 —— 那是最坏的一种绿。
  if [[ "$STATUS" != "200" ]]; then
    printf '  %sFAIL%s ?user_id= 那条探测没带着 B 的 token 发出去（HTTP %s），判据没法成立\n' \
      "$RED" "$RESET" "$STATUS"; fail=$((fail+1))
  elif [[ "$BODY" == *"\"author_id\":$MY_ID,"* ]]; then
    printf '  %sFAIL%s ?user_id= 让别人的 token 读到了你的帖子（联系方式一起过去了）\n' "$RED" "$RESET"; fail=$((fail+1))
  else
    printf '  %sPASS%s ?user_id= 被忽略，#19 只认 JWT 里的那个人\n' "$GREEN" "$RESET"; pass=$((pass+1))
  fi
fi

# ---------------------------------------------------------------- M2 详情
section "M2 详情：#15 /api/items/:id（公开 + OptionalJWT）"

req GET "/api/items/$LOST_ID"
check '#15 匿名看 lost 帖返回 HTTP 200' '200'          "$STATUS"
check '  lost 帖的 contact 是公开的'  '"contact":"13800000000"' "$BODY"
check '  contact_locked 是 false'     '"contact_locked":false' "$BODY"

req GET "/api/items/$FOUND_ID"
check '  匿名看 found 帖 → contact 是 null' '"contact":null' "$BODY"
check '  contact_locked 是 true'      '"contact_locked":true' "$BODY"
if [[ "$BODY" == *问图书馆前台* ]]; then
  printf '  %sFAIL%s 锁着的联系方式还是出现在响应里了\n' "$RED" "$RESET"; fail=$((fail+1))
else
  printf '  %sPASS%s 锁着的联系方式在整个响应体里都不存在（不是靠前端隐藏）\n' "$GREEN" "$RESET"; pass=$((pass+1))
fi

req GET "/api/items/$FOUND_ID" '' "$TOKEN"
check '  作者本人看自己的 found 帖 → contact 可见' '"contact":"问图书馆前台"' "$BODY"
req GET "/api/items/$FOUND_ID" '' "$TOKEN2"
check '  别人看 → 仍然锁着'           '"contact_locked":true' "$BODY"
req GET "/api/items/$FOUND_ID" '' "not-a-real-token"
check '  无效 token 当匿名处理，不报错（公开接口不该因身份问题拒绝服务）' '200' "$STATUS"

req GET '/api/items/999999999'
check '  不存在的 id → HTTP 404'      '404'                  "$STATUS"
check '  code 是 NOT_FOUND'           '"code":"NOT_FOUND"' "$BODY"
req GET '/api/items/abc'
check '  id 不是数字 → 404 而不是 500' '404'        "$STATUS"

# 摘要里不该有 description：列表接口只回标题，这是刻意的带宽和职责划分
req GET '/api/items?keyword=%E5%86%92%E7%83%9F'
if [[ "$BODY" == *'"description"'* ]]; then
  printf '  %sFAIL%s 广场摘要里出现了 description\n' "$RED" "$RESET"; fail=$((fail+1))
else
  printf '  %sPASS%s 广场摘要里没有 description（详情页才有）\n' "$GREEN" "$RESET"; pass=$((pass+1))
fi

# ---------------------------------------------------------------- M2 改·删·关帖
section "M2 改删：#16 PUT、#17 DELETE、#18 PATCH status"

req PUT "/api/items/$LOST_ID" "{\"item_type\":\"lost\",\"title\":\"被别人改过的标题\",\"contact\":\"13900000000\",\"category_id\":$CAT_ID,\"location_id\":$LOC_ID,\"last_seen_at\":\"2026-10-01T09:00:00+08:00\",\"lost_at\":\"2026-10-01T12:00:00+08:00\"}" "$TOKEN2"
check '非本人改帖 → HTTP 403'         '403'                  "$STATUS"
check '  code 是 FORBIDDEN'           '"code":"FORBIDDEN"' "$BODY"
req GET "/api/items/$LOST_ID"
check '  帖子内容没被改动'            '"title":"冒烟：黑色长款钱包"' "$BODY"

# 改帖**不能**改类型，也不能改作者。请求体里塞了 item_type/user_id 也只会被忽略。
req PUT "/api/items/$LOST_ID" "{\"item_type\":\"found\",\"user_id\":$MY_ID,\"title\":\"冒烟：黑色长款钱包（改过）\",\"contact\":\"13900000000\",\"category_id\":$CAT_ID,\"location_id\":$LOC_ID,\"last_seen_at\":\"2026-10-01T09:00:00+08:00\",\"lost_at\":\"2026-10-01T12:00:00+08:00\"}" "$TOKEN"
check '本人改帖 → HTTP 200'           '200'          "$STATUS"
check '  标题改过来了'                '冒烟：黑色长款钱包（改过）' "$BODY"
check '  item_type 没被改成 found'    '"item_type":"lost"' "$BODY"
check_re '  作者没被改成请求里那个人' '"author":\{"id":'"$MY_ID"',"' "$BODY"

# 判据点名：ITEM_CLOSED 只挡 deleted，closed 的帖子必须还能改
req PATCH "/api/items/$LOST_ID/status" '{"status":"closed"}' "$TOKEN"
check '#18 关帖返回 HTTP 200'         '200'          "$STATUS"
check '  data.status 是 closed'       '"status":"closed"' "$BODY"
req PUT "/api/items/$LOST_ID" "{\"item_type\":\"lost\",\"title\":\"冒烟：已经还回来了\",\"contact\":\"13900000000\",\"category_id\":$CAT_ID,\"location_id\":$LOC_ID,\"last_seen_at\":\"2026-10-01T09:00:00+08:00\",\"lost_at\":\"2026-10-01T12:00:00+08:00\"}" "$TOKEN"
check '改一条 closed 的帖子 → HTTP 200（ITEM_CLOSED 只挡 deleted）' '200' "$STATUS"
check '  改帖不会顺带把状态开回去'    '"status":"closed"' "$BODY"

req PATCH "/api/items/$LOST_ID/status" '{"status":"deleted"}' "$TOKEN"
check '#18 不许把帖子改成 deleted → HTTP 400' '400' "$STATUS"
req PATCH "/api/items/$FOUND_ID/status" '{"status":"closed"}' "$TOKEN2"
check '非本人关帖 → HTTP 403'         '403'                  "$STATUS"
req PATCH "/api/items/$FOUND_ID/status" '{"status":"closed"}'
check '  不带 token → HTTP 401'       '401'                  "$STATUS"

# ---------------------------------------------------------------- M2 删图
section "M2 帖主自删单张图片：#42 DELETE /api/item-images/:id"

IMG1_ID=''; IMG2_ID=''; IMG3_ID=''
if [[ -n "$LOST_ID" ]]; then
  req GET "/api/items/$LOST_ID" '' "$TOKEN"
  # 把 images 数组整段切出来再逐个抠 id。
  # 不用 jval 抠第二个：`.*` 是最长匹配，一次只能拿到**最后一个**，
  # 而这里要的是「按顺序的第 1 / 2 / 3 个」，所以先把数组边界定住（[^]]* 到第一个 ] 为止）。
  imgs="$(printf '%s' "$BODY" | grep -oE '"images":\[[^]]*\]' | grep -oE '"id":[0-9]+' | grep -oE '[0-9]+')"
  IMG1_ID="$(printf '%s\n' "$imgs" | sed -n 1p)"
  IMG2_ID="$(printf '%s\n' "$imgs" | sed -n 2p)"
  IMG3_ID="$(printf '%s\n' "$imgs" | sed -n 3p)"
fi
if [[ -z "$IMG1_ID" || -z "$IMG2_ID" || -z "$IMG3_ID" ]]; then
  printf '  %sFAIL%s 没能从 #15 的响应里取出三张图的 id（%s / %s / %s），#42 测不了\n' \
    "$RED" "$RESET" "$IMG1_ID" "$IMG2_ID" "$IMG3_ID"; fail=$((fail+1))
else
  # 别人和 admin 都不能删 —— 先把拒绝路径验完，此时三张图都还在。
  # ⚠ admin 也不行：#42 的权限列写的是「仅帖主本人」，admin 删图是 #45（M6）。
  #   分成两个端点不是为了麻烦，是因为这两件事的**含义**不同：
  #   帖主删图是「我不想让这张照片挂在这儿」，admin 删图是一次治理动作，要填理由、要落 admin_actions。
  req DELETE "/api/item-images/$IMG1_ID" '' "$TOKEN2"
  check '别人删帖主的图 → HTTP 403'   '403'                  "$STATUS"
  req DELETE "/api/item-images/$IMG1_ID"
  check '  不带 token → HTTP 401'     '401'                  "$STATUS"
  [[ -f "$UPLOAD_ABS/$PIC1" ]] && \
    { printf '  %sPASS%s FORBIDDEN 是真的没删：文件还在\n' "$GREEN" "$RESET"; pass=$((pass+1)); } || \
    { printf '  %sFAIL%s 报了 FORBIDDEN 却把文件删了\n' "$RED" "$RESET"; fail=$((fail+1)); }

  # ---- 判据正文：删中间那张，留下前后两张 ----
  # 留一张在前、一张在后，才能同时验两件事：
  #   ① 「另一张没被牵连」（前一张还在）
  #   ② 封面是 sort_order 最小的那张，删掉它之后封面会跟着换（#14 的 LEFT JOIN LATERAL 现算）
  req DELETE "/api/item-images/$IMG2_ID" '' "$TOKEN"
  check '帖主删自己帖子里的一张图 → HTTP 200' '200'  "$STATUS"
  [[ -f "$UPLOAD_ABS/$PIC2" ]] && \
    { printf '  %sFAIL%s 磁盘文件 %s 还在，没被删\n' "$RED" "$RESET" "$PIC2"; fail=$((fail+1)); } || \
    { printf '  %sPASS%s 磁盘文件消失了\n' "$GREEN" "$RESET"; pass=$((pass+1)); }
  [[ -f "$UPLOAD_ABS/$PIC1" && -f "$UPLOAD_ABS/$PIC3" ]] && \
    { printf '  %sPASS%s 同一帖子里另外两张图的文件没被牵连\n' "$GREEN" "$RESET"; pass=$((pass+1)); } || \
    { printf '  %sFAIL%s 别的图的文件被连带删掉了\n' "$RED" "$RESET"; fail=$((fail+1)); }

  req GET "/api/items/$LOST_ID" '' "$TOKEN"
  check '  帖子本身还在'              '"item_type":"lost"' "$BODY"
  check_re '  只剩两张图'             '"images":\[\{"id":'"$IMG1_ID"'[^}]*\},\{"id":'"$IMG3_ID"'[^}]*\}\]' "$BODY"

  # 广场上的封面自动变成留下来的第一张：#14 用
  # LEFT JOIN LATERAL ... ORDER BY sort_order LIMIT 1 现算封面，
  # 所以这里不需要任何「重算封面」的代码 —— 测的就是那个 JOIN 本身。
  #
  # 必须带 status=closed：上面「改删」那一节把这条帖子关掉了，而 #14 默认只看 open。
  # 少了这个参数，帖子根本不出现在结果里，封面断言就会假红 ——
  # 而且红得像是「封面没跟着换」，指向一个完全不存在的问题。
  req GET '/api/items?status=closed&keyword=%E5%86%92%E7%83%9F'
  if [[ "$BODY" == *"\"id\":$LOST_ID,"* ]]; then
    if [[ "$BODY" == *"/uploads/$PIC1\""* ]]; then
      printf '  %sPASS%s #14 的封面跟着变成了留下来的那张\n' "$GREEN" "$RESET"; pass=$((pass+1))
    else
      printf '  %sFAIL%s 帖子在广场上是这条：\n       %s期望它的封面是: "/uploads/%s"%s\n       %s实际: %s%s\n' \
        "$RED" "$RESET" "$DIM" "$PIC1" "$RESET" "$DIM" "$(printf '%.300s' "$BODY")" "$RESET"; fail=$((fail+1))
    fi
  else
    printf '  %sFAIL%s 这条 closed 的帖子没出现在 status=closed 的结果里，封面没法验\n       %s实际: %s%s\n' \
      "$RED" "$RESET" "$DIM" "$(printf '%.300s' "$BODY")" "$RESET"; fail=$((fail+1))
  fi

  # ---- 重复删除不是幂等成功 ----
  req DELETE "/api/item-images/$IMG1_ID" '' "$TOKEN"
  check '帖主删第一张 → HTTP 200'     '200'          "$STATUS"
  req DELETE "/api/item-images/$IMG1_ID" '' "$TOKEN"
  check '  再删一次同一张 → HTTP 404（不是幂等的 200）' '404' "$STATUS"
fi

# ---------------------------------------------------------------- M2 软删
section "M2 删帖：#17 DELETE /api/items/:id（软删）"

req DELETE "/api/items/$FOUND_ID" '' "$TOKEN"
check '删自己的帖子 → HTTP 200'       '200'          "$STATUS"
req GET "/api/items/$FOUND_ID"
check '  别人/匿名都 404'             '"code":"NOT_FOUND"' "$BODY"
req GET '/api/my/items?status=deleted' '' "$TOKEN"
check_re '  本人在 #19 里还看得见（否则他会以为帖子凭空消失了）' \
  '"id":'"$FOUND_ID"',"' "$BODY"
if [[ "$HAS_PSQL" == 1 ]]; then
  check '  行还在（软删不是物理删除）' '1' "$(db "SELECT (count(*)=1)::int FROM items WHERE id=$FOUND_ID")"
  check '  status 是 deleted'         'deleted' "$(db "SELECT status FROM items WHERE id=$FOUND_ID")"
else
  skip '  行还在 / status 是 deleted（没有 psql，查不到库里的行）'
fi

# 软删之后帖主自己也改不动了 —— 这一条不需要 psql，接口上就能验
req PUT "/api/items/$FOUND_ID" "{\"item_type\":\"found\",\"title\":\"删了还能改？\",\"contact\":\"13800000000\",\"category_id\":$CAT_ID,\"location_id\":$LOC_ID,\"found_at\":\"2026-10-02T15:00:00+08:00\"}" "$TOKEN"
check '  删过的帖子改不了 → HTTP 409' '409'                 "$STATUS"
check '    code 是 ITEM_CLOSED'       '"code":"ITEM_CLOSED"' "$BODY"

# ---------------------------------------------------------------- M2 admin 通道
section "M2 管理员边界：admin 能改别人的帖，但必须给理由"
if [[ "$HAS_PSQL" == 1 ]]; then
  db "UPDATE users SET role='admin', updated_at=now() WHERE username='$U2'" >/dev/null
  req POST /api/auth/login "{\"username\":\"$U2\",\"password\":\"$P\"}"
  ADMIN_TOKEN="$(jval "$BODY" '"token":"([^"]{20,})"')"

  req PUT "/api/items/$LOST_ID" "{\"item_type\":\"lost\",\"title\":\"管理员随手改的\",\"contact\":\"13800000000\",\"category_id\":$CAT_ID,\"location_id\":$LOC_ID,\"last_seen_at\":\"2026-10-01T09:00:00+08:00\",\"lost_at\":\"2026-10-01T12:00:00+08:00\"}" "$ADMIN_TOKEN"
  check 'admin 改别人的帖不带理由 → HTTP 400' '400' "$STATUS"
  check '  错误指向 admin_reason'     '"field":"admin_reason"' "$BODY"

  req PUT "/api/items/$LOST_ID" "{\"item_type\":\"lost\",\"title\":\"管理员下架的违规帖\",\"contact\":\"13800000000\",\"category_id\":$CAT_ID,\"location_id\":$LOC_ID,\"last_seen_at\":\"2026-10-01T09:00:00+08:00\",\"lost_at\":\"2026-10-01T12:00:00+08:00\",\"admin_reason\":\"冒烟脚本：验一下 admin 通道\"}" "$ADMIN_TOKEN"
  check '  带上理由 → HTTP 200'       '200'          "$STATUS"
  check_re '  作者没被改动（admin 改的是内容，不是归属）' '"author":\{"id":'"$MY_ID"',"' "$BODY"

  # #42 的权限是「仅帖主本人」，admin 也不例外 —— 删图对 admin 是一次治理动作，要走 #45
  req GET "/api/items/$LOST_ID" '' "$ADMIN_TOKEN"
  admin_img="$(printf '%s' "$BODY" | grep -oE '"images":\[[^]]*\]' | grep -oE '"id":[0-9]+' | grep -oE '[0-9]+' | sed -n 1p)"
  if [[ -n "$admin_img" ]]; then
    req DELETE "/api/item-images/$admin_img" '' "$ADMIN_TOKEN"
    check 'admin 删别人的图 → HTTP 403（那是 #45，不是 #42）' '403' "$STATUS"
  fi

  db "UPDATE users SET role='user', updated_at=now() WHERE username='$U2'" >/dev/null
else
  skip 'admin 通道全套（没有 psql，无法把用户提成 admin）'
fi


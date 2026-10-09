# ---------------------------------------------------------------- M4 解锁 · 收件箱 · 举报
# deps: m1 m2 m3
section "M4 联系方式解锁 #21·#22、收件箱 #30–#32、举报 #41"

# 这一节要三个角色，谁也合不到谁身上：
#   TOKEN   = 失主 A：发一条 lost 帖，然后去解锁别人那条 found 帖的联系方式
#   TOKEN2  = 失主 B：同样发一条 lost 帖，专门用来验「非排他」和「标记已读只影响自己」
#   TOKEN3  = 拾主  ：发那条被解锁的 found 帖，是 #22 名单唯一合法的读者
# 少了 B，「A 解锁之后 B 还解不解得开」只能靠读代码相信；少了拾主，#21 会走进
# 「作者本人不写行」那一支，名单永远只有一个名字、也就看不出它是不是倒序的。
U3="smoke3$(date +%s)$RANDOM"
M4_TAG="m4$(date +%s)$RANDOM"
M4_CONTACT="wx_smoke_finder_m4"

req POST /api/auth/register "{\"username\":\"$U3\",\"password\":\"$P\"}"
req POST /api/auth/login "{\"username\":\"$U3\",\"password\":\"$P\"}"
TOKEN3="$(jval "$BODY" '"token":"([^"]{20,})"')"
req GET /api/auth/me '' "$TOKEN3"
FINDER_ID="$(jval "$BODY" '"data":\{"id":([0-9]+)')"

if [[ -z "$TOKEN3" || -z "$FINDER_ID" || -z "$MY_ID" || -z "$OTHER_ID" ]]; then
  printf '  %sFAIL%s 第三个冒烟用户（拾主 id=%s）没建起来，M4 这一节整段测不了\n' \
    "$RED" "$RESET" "$FINDER_ID"; fail=$((fail+1))
else
  # ---- 三条帖：两条 lost（两个失主）+ 一条 found（拾主） ----
  #
  # 文本、分类、地点、时间全部照抄 M3 那一组**已经证明过「分数跨得通知线」**的参数，
  # 只把标签换成 M4 的。为什么要在 M4 里重新走一遍匹配：收件箱那一段要有
  # 真实的 new_match 可读，而 M4 自己不写任何通知 —— 唯一的写入方就是这条链。
  # 顺序也是刻意的：先两条 lost（lost 侧一行都不写），最后建 found（这时才通知两个失主）。
  m4_lost_a="$(printf '{"item_type":"lost","title":"%s 黑色长款钱包","description":"%s 夹层里有一张校园卡","category_id":%d,"location_id":%d,"location_detail":"三楼自习室B区","last_seen_at":"%s","lost_at":"%s","contact":"13800007777"}' \
    "$M4_TAG" "$M4_TAG" "$CAT_WALLET" "$LOC_ID" "$T_SEEN" "$T_LOST")"
  req POST /api/items "$m4_lost_a" "$TOKEN"
  check '建失主 A 的 lost 帖返回 HTTP 200' '200' "$STATUS"
  M4_LOST_A="$(itemIDOf "$BODY")"

  m4_lost_b="$(printf '{"item_type":"lost","title":"%s 黑色长款钱包","description":"%s 夹层里有一张校园卡","category_id":%d,"location_id":%d,"location_detail":"三楼自习室B区","last_seen_at":"%s","lost_at":"%s","contact":"13900007777"}' \
    "$M4_TAG" "$M4_TAG" "$CAT_WALLET" "$LOC_ID" "$T_SEEN" "$T_LOST")"
  req POST /api/items "$m4_lost_b" "$TOKEN2"
  check '建失主 B 的 lost 帖返回 HTTP 200' '200' "$STATUS"
  M4_LOST_B="$(itemIDOf "$BODY")"

  m4_found="$(printf '{"item_type":"found","title":"%s 黑色长款钱包","description":"%s 夹层里有一张校园卡","category_id":%d,"location_id":%d,"location_detail":"三楼自习室B区","found_at":"%s","contact":"%s"}' \
    "$M4_TAG" "$M4_TAG" "$CAT_WALLET" "$LOC_ID" "$T_FOUND" "$M4_CONTACT")"
  req POST /api/items "$m4_found" "$TOKEN3"
  check '建拾主的 found 帖返回 HTTP 200' '200' "$STATUS"
  M4_FOUND="$(itemIDOf "$BODY")"
  check_re '  有人被通知了（收件箱才有东西可读）' '"notified_count":[0-9]+' "$BODY"
fi

if [[ -n "$M4_FOUND" && -n "$M4_LOST_A" && -n "$M4_LOST_B" ]]; then
  # ---- 判据①②：解锁之前 #15 给两个人看到的形状 ----
  req GET "/api/items/$M4_FOUND"
  check '① 匿名看 found 帖 → contact 是 null' '"contact":null' "$BODY"
  check '   且 contact_locked 是 true'        '"contact_locked":true' "$BODY"
  req GET "/api/items/$M4_LOST_A"
  check '② 匿名看 lost 帖 → contact 直接可见（解锁这件事只属于 found 侧）' '"contact":"13800007777"' "$BODY"

  # ---- 判据③：解锁 → 拿到 contact，且 notifications 表没有新增行 ----
  # 三个当事人的通知数一起看：只盯解锁者本人的话，「平台给拾主发了一条『有人看了
  # 你的联系方式』」这种实现正好漏掉 —— 而那恰恰是 contact_unlocked 被删掉之后
  # 最不该回来的东西（§16）。
  notif_a0="$(db "SELECT count(*) FROM notifications WHERE user_id=$MY_ID")"
  notif_b0="$(db "SELECT count(*) FROM notifications WHERE user_id=$OTHER_ID")"
  notif_f0="$(db "SELECT count(*) FROM notifications WHERE user_id=$FINDER_ID")"

  req POST "/api/items/$M4_FOUND/unlock-contact" '' "$TOKEN"
  check '③ #21 解锁返回 HTTP 200'        '200' "$STATUS"
  check '   data.contact 就是那条联系方式' "\"contact\":\"$M4_CONTACT\"" "$BODY"
  check '   already_unlocked 是 false（第一次）' '"already_unlocked":false' "$BODY"
  M4_UNLOCKED_AT="$(jval "$BODY" '"unlocked_at":"([^"]+)"')"
  if [[ "$M4_UNLOCKED_AT" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]]; then
    printf '  %sPASS%s   unlocked_at 是 UTC 的 RFC3339（%s）\n' "$GREEN" "$RESET" "$M4_UNLOCKED_AT"
    pass=$((pass+1))
  else
    printf '  %sFAIL%s   unlocked_at 的形状不对：%s\n' "$RED" "$RESET" "$M4_UNLOCKED_AT"; fail=$((fail+1))
  fi

  if [[ "$HAS_PSQL" != 1 ]]; then
    skip '③ 解锁只写一行、且一条通知都不发（本机没有 psql，数不了 contact_views / notifications）'
  else
    check '   contact_views 里正好 1 行' '1' \
      "$(db "SELECT count(*) FROM contact_views WHERE item_id=$M4_FOUND")"
    check '   那一行记的是来解锁的那个人' '1' \
      "$(db "SELECT (count(*)=1)::int FROM contact_views WHERE item_id=$M4_FOUND AND user_id=$MY_ID")"
    if [[ "$(db "SELECT count(*) FROM notifications WHERE user_id=$MY_ID")"     == "$notif_a0" ]] \
    &&  [[ "$(db "SELECT count(*) FROM notifications WHERE user_id=$OTHER_ID")" == "$notif_b0" ]] \
    &&  [[ "$(db "SELECT count(*) FROM notifications WHERE user_id=$FINDER_ID")" == "$notif_f0" ]]; then
      printf '  %sPASS%s   三个人谁都没被通知（contact_unlocked 这个 type 确实不存在了）\n' \
        "$GREEN" "$RESET"; pass=$((pass+1))
    else
      printf '  %sFAIL%s   解锁发出了通知 —— A %s→%s B %s→%s 拾主 %s→%s\n' "$RED" "$RESET" \
        "$notif_a0" "$(db "SELECT count(*) FROM notifications WHERE user_id=$MY_ID")" \
        "$notif_b0" "$(db "SELECT count(*) FROM notifications WHERE user_id=$OTHER_ID")" \
        "$notif_f0" "$(db "SELECT count(*) FROM notifications WHERE user_id=$FINDER_ID")"
      fail=$((fail+1))
    fi
  fi

  req GET "/api/items/$M4_FOUND" '' "$TOKEN"
  check '   同一个人再看 #15 → contact 可见了' "\"contact\":\"$M4_CONTACT\"" "$BODY"
  check '   contact_locked 跟着变成 false'    '"contact_locked":false' "$BODY"

  # ---- 判据④：同一人解锁两次只有一行 ----
  req POST "/api/items/$M4_FOUND/unlock-contact" '' "$TOKEN"
  check '④ 第二次解锁仍然返回 HTTP 200' '200' "$STATUS"
  check '   already_unlocked 变成 true'  '"already_unlocked":true' "$BODY"
  # 回的是**第一次**那个时刻：这张表记的是「他最早什么时候看过」，不是「最后一次」。
  # 一个每次 POST 都写一行的实现也能让 already_unlocked=true，所以光看响应不够，
  # 下面还要数库。
  second_at="$(jval "$BODY" '"unlocked_at":"([^"]+)"')"
  if [[ -n "$second_at" && "$second_at" == "$M4_UNLOCKED_AT" ]]; then
    printf '  %sPASS%s   返回的还是第一次解锁的时刻\n' "$GREEN" "$RESET"; pass=$((pass+1))
  else
    printf '  %sFAIL%s   第二次解锁的时刻变了：%s → %s\n' \
      "$RED" "$RESET" "$M4_UNLOCKED_AT" "$second_at"; fail=$((fail+1))
  fi
  if [[ "$HAS_PSQL" == 1 ]]; then
    check '   库里仍然只有 1 行（幂等不是「再记一次」）' '1' \
      "$(db "SELECT count(*) FROM contact_views WHERE item_id=$M4_FOUND")"
  fi

  # ---- 判据⑤：第二个人也能解锁 —— 这不是排他锁 ----
  # 计划里那句原话是「不需要设一个人认领其他人不允许查看的规则」。
  # 这一条是 M4 最容易在后续改动里被悄悄推翻的一条：只要谁给 contact_views
  # 加一个 status 列、或者在 Unlock 里加一句「已经有人认领就返回 409」，
  # 上面四条判据全都还是绿的，只有这条会红。
  req POST "/api/items/$M4_FOUND/unlock-contact" '' "$TOKEN2"
  check '⑤ 失主 B 也解锁成功（A 没能把这条帖子锁起来）' '200' "$STATUS"
  check '   同样拿到了 contact'                          "\"contact\":\"$M4_CONTACT\"" "$BODY"
  if [[ "$HAS_PSQL" == 1 ]]; then
    check '   库里现在是 2 行（一个人一行，不是两个人抢一行）' '2' \
      "$(db "SELECT count(*) FROM contact_views WHERE item_id=$M4_FOUND")"
  fi

  # ---- 判据⑥：对 lost 帖调解锁接口 → VALIDATION ----
  req POST "/api/items/$M4_LOST_B/unlock-contact" '' "$TOKEN"
  check '⑥ 对 lost 帖调解锁 → HTTP 400' '400'                  "$STATUS"
  check '   code 是 VALIDATION'         '"code":"VALIDATION"' "$BODY"
  if [[ "$HAS_PSQL" == 1 ]]; then
    check '   被拒的那次一行都没写' '0' \
      "$(db "SELECT count(*) FROM contact_views WHERE item_id=$M4_LOST_B")"
  fi

  # ---- closed 的 found 帖：不让新人解锁，但作者本人不受影响 ----
  # 东西已经还回去了，这时候再给拾主添一个「有人来要联系方式」是没有意义的骚扰。
  # 判据顺序（作者那一支排在 closed 之前）在这里正好能测出来。
  m4_closed="$(printf '{"item_type":"found","title":"%s 黑色长款钱包（已经交前台）","description":"%s 已经交到宿管处","category_id":%d,"location_id":%d,"location_detail":"一楼大厅服务台","found_at":"%s","contact":"%s"}' \
    "$M4_TAG" "$M4_TAG" "$CAT_WALLET" "$LOC_ID" "$T_FOUND" "closed_m4_contact")"
  req POST /api/items "$m4_closed" "$TOKEN3"
  check '为 closed 场景再建一条 found 帖返回 HTTP 200' '200' "$STATUS"
  M4_CLOSED="$(itemIDOf "$BODY")"
  req PATCH "/api/items/$M4_CLOSED/status" '{"status":"closed"}' "$TOKEN3"
  check '  拾主把它关掉返回 HTTP 200' '200' "$STATUS"

  req POST "/api/items/$M4_CLOSED/unlock-contact" '' "$TOKEN"
  check '  closed 的 found 帖不让新人解锁 → HTTP 409' '409' "$STATUS"
  check '    code 是 ITEM_CLOSED'                     '"code":"ITEM_CLOSED"' "$BODY"
  req POST "/api/items/$M4_CLOSED/unlock-contact" '' "$TOKEN3"
  check '  作者自己不受影响（他要核对、要改电话）'      '200' "$STATUS"
  if [[ "$HAS_PSQL" == 1 ]]; then
    check '    而作者这一支确实一行都没写' '0' \
      "$(db "SELECT count(*) FROM contact_views WHERE item_id=$M4_CLOSED")"
  fi

  # ---- 判据⑦⑧：#22 解锁名单 ----
  req GET "/api/items/$M4_FOUND/contact-views" '' "$TOKEN3"
  check '⑦ 拾主查名单返回 HTTP 200'   '200'          "$STATUS"
  check '   total 是 2（两个失主各一行）' '"total":2' "$BODY"
  check_re '   形状是 {id,user:{...},created_at} 的嵌套' '"user":\{"id":'"$MY_ID"',' "$BODY"
  check_re '   第二个人也在名单里'     '"user":\{"id":'"$OTHER_ID"',' "$BODY"
  # real_name 在 M8（SSO）之前一直是空的，但**键必须在**：前端按它渲染一行，
  # 键消失和值为空是两种不同的故障，混在一起排查时会分不清是接口漏了字段还是没数据。
  check '   real_name 这个键存在'     '"real_name":'  "$BODY"
  check '   名单一页的默认大小是 20'  '"page_size":20' "$BODY"

  req GET "/api/items/$M4_FOUND/contact-views" '' "$TOKEN"
  check '⑧ 解锁过但不是发帖人 → HTTP 403' '403'                  "$STATUS"
  check '   code 是 FORBIDDEN'            '"code":"FORBIDDEN"' "$BODY"
  req GET "/api/items/$M4_FOUND/contact-views"
  check '   匿名 → HTTP 401（不是 403，也没给出任何一个名字）' '401' "$STATUS"
  if [[ "$BODY" == *"\"user\""* ]]; then
    printf '  %sFAIL%s 401 的响应里居然带了名单\n' "$RED" "$RESET"; fail=$((fail+1))
  else
    printf '  %sPASS%s 401 的响应体里没有名单\n' "$GREEN" "$RESET"; pass=$((pass+1))
  fi
  # 作者查自己 lost 帖的名单：合法请求、空答案。#21 在 lost 上会被拒，
  # 所以这张表对一条 lost 帖永远是空的 —— 但它不该因此变成 404 或 500。
  req GET "/api/items/$M4_LOST_A/contact-views" '' "$TOKEN"
  check '   作者查自己 lost 帖的名单 → 200 且是空列表' '"list":[],"total":0' "$BODY"
  req GET "/api/items/$M4_FOUND/contact-views?page_size=99999" '' "$TOKEN3"
  check '   page_size 超上限 → HTTP 400' '400' "$STATUS"

  # ---- #30 #31 #32 收件箱：这一节读的就是上面那条链写下的通知 ----
  req GET /api/my/notifications '' "$TOKEN"
  check '#30 收件箱返回 HTTP 200'      '200'          "$STATUS"
  check '   分页形状 {list,total,page,page_size}' '"page_size":20' "$BODY"
  check '   里面就是 M3 那一跳写的 new_match' '"type":"new_match"' "$BODY"
  if [[ "$BODY" == *'"user_id"'* ]]; then
    printf '  %sFAIL%s 收件箱里出现了 user_id —— 这一列是归属，不是给用户看的数据\n' "$RED" "$RESET"; fail=$((fail+1))
  else
    printf '  %sPASS%s 响应里没有 user_id\n' "$GREEN" "$RESET"; pass=$((pass+1))
  fi
  # item_id 必须指向那条真的 found 帖：前端靠它跳转，指错了就是点开一条不存在的帖子。
  check '   通知带着能跳过去的 item_id' "\"item_id\":$M4_FOUND" "$BODY"

  # 未读列表里要取**两个** id：第一个给「本人按 id 标记」，第二个留给「别人想标记我的」
  # 那条越权探测 —— 那次探测之后还要回头看它是不是仍然未读，所以两条不能是同一条。
  # NotificationView 的键序是 id、type、title…，所以 `"id":N,"type"` 只可能出现在列表元素开头。
  req GET '/api/my/notifications?is_read=false&page_size=5' '' "$TOKEN"
  check '   is_read=false 只回未读 → HTTP 200' '200' "$STATUS"
  unread_ids="$(printf '%s' "$BODY" | grep -oE '"id":[0-9]+,"type"' | grep -oE '[0-9]+')"
  UNREAD_ID="$(printf '%s\n' "$unread_ids" | sed -n 1p)"
  UNREAD_ID2="$(printf '%s\n' "$unread_ids" | sed -n 2p)"

  req GET '/api/my/notifications/unread-count' '' "$TOKEN"
  check '#31 未读数返回 HTTP 200 且带 count' '"count":' "$BODY"
  unread0="$(jval "$BODY" '"count":([0-9]+)')"
  req GET '/api/my/notifications/unread-count'
  check '   不带 token → HTTP 401'      '401'          "$STATUS"

  if [[ -z "$UNREAD_ID" || -z "$unread0" ]]; then
    printf '  %sFAIL%s 取不到未读通知（id=%s count=%s），#32 测不了\n' \
      "$RED" "$RESET" "$UNREAD_ID" "$unread0"; fail=$((fail+1))
  else
    req PUT /api/my/notifications/read "{\"ids\":[$UNREAD_ID]}" "$TOKEN"
    check '#32 按 id 标记已读返回 HTTP 200' '200' "$STATUS"
    check '   updated_count 是 1'          '"updated_count":1' "$BODY"
    # 第二次点同一个 id：仍然是 200，但 updated_count 必须是 0。
    # 「已读」是一个状态而不是一个计数，用户重复点不该让系统以为发生了什么新事。
    req PUT /api/my/notifications/read "{\"ids\":[$UNREAD_ID]}" "$TOKEN"
    check '   再点一次同一个 id → updated_count 是 0（幂等）' '"updated_count":0' "$BODY"

    req GET '/api/my/notifications/unread-count' '' "$TOKEN"
    unread1="$(jval "$BODY" '"count":([0-9]+)')"
    if [[ "$unread1" == "$((unread0 - 1))" ]]; then
      printf '  %sPASS%s   未读数从 %s 降到 %s\n' "$GREEN" "$RESET" "$unread0" "$unread1"; pass=$((pass+1))
    else
      printf '  %sFAIL%s   标了一条已读，未读数却是 %s → %s\n' \
        "$RED" "$RESET" "$unread0" "$unread1"; fail=$((fail+1))
    fi

    # 越权：B 拿自己的 token 去标记 A 的（另一条）通知。
    # 顺序上排在 all=true 之前，因为 all 会把 A 的未读清空，那样这条判据就没东西可验了。
    if [[ -z "$UNREAD_ID2" ]]; then
      skip '   别人替我标记已读（A 本轮只捞到一条未读，没有第二条可以事后核对）'
    else
      req PUT /api/my/notifications/read "{\"ids\":[$UNREAD_ID2]}" "$TOKEN2"
      check '   别人标记我的通知 → HTTP 403' '403'                  "$STATUS"
      check '     code 是 FORBIDDEN'         '"code":"FORBIDDEN"' "$BODY"
      req GET '/api/my/notifications?is_read=false' '' "$TOKEN"
      if [[ "$BODY" == *"\"id\":$UNREAD_ID2,\"type\""* ]]; then
        printf '  %sPASS%s   被拒的那次真的没改：A 的那条还是未读\n' "$GREEN" "$RESET"; pass=$((pass+1))
      else
        printf '  %sFAIL%s 报了 FORBIDDEN 却还是把 A 的通知改成已读了\n' "$RED" "$RESET"; fail=$((fail+1))
      fi
    fi

    # all=true 的作用域：清掉 A 的所有未读，一条都不能碰到 B。
    req GET '/api/my/notifications/unread-count' '' "$TOKEN2"
    unread_b0="$(jval "$BODY" '"count":([0-9]+)')"
    req PUT /api/my/notifications/read '{"all":true}' "$TOKEN"
    check '   all=true 返回 HTTP 200'   '200'          "$STATUS"
    req GET '/api/my/notifications/unread-count' '' "$TOKEN"
    check '   A 的未读清零'             '"count":0'    "$BODY"
    req GET '/api/my/notifications/unread-count' '' "$TOKEN2"
    if [[ "$(jval "$BODY" '"count":([0-9]+)')" == "$unread_b0" ]]; then
      printf '  %sPASS%s   B 的未读数一条没动（仍是 %s）\n' "$GREEN" "$RESET" "$unread_b0"; pass=$((pass+1))
    else
      printf '  %sFAIL%s 一键已读把 B 的通知也标了：%s → %s\n' \
        "$RED" "$RESET" "$unread_b0" "$(jval "$BODY" '"count":([0-9]+)')"; fail=$((fail+1))
    fi

    # 收件人是 token 决定的，不是查询参数决定的。
    # 这条和 #19 那条同构：能读到别人的收件箱，等于把「谁联系过谁」整份交出去。
    # 判别依据只能用「同一个人带不带这个参数是不是同一份」：B 自己也解锁过这条 found 帖，
    # 他的收件箱里本来就该有一条 item_id 指向它的通知 —— 拿 item_id 当特征是错的。
    req GET /api/my/notifications '' "$TOKEN2"
    total_b_norm="$(jval "$BODY" '"total":([0-9]+)')"
    req GET "/api/my/notifications?user_id=$MY_ID" '' "$TOKEN2"
    total_b_param="$(jval "$BODY" '"total":([0-9]+)')"
    # 两次都必须真的拿到 200：一个 401 的空响应可以匹配「什么都没变」，
    # 而它什么都没证明（上面 #19 那条同构的检查踩过这个坑）。
    if [[ "$STATUS" != "200" || -z "$total_b_norm" || -z "$total_b_param" ]]; then
      printf '  %sFAIL%s 两次收件箱读取没都成功（HTTP %s，total %s→%s），这条判据没成立也没被否证\n' \
        "$RED" "$RESET" "$STATUS" "$total_b_norm" "$total_b_param"; fail=$((fail+1))
    elif [[ "$total_b_param" != "$total_b_norm" ]] || [[ "$BODY" == *"\"id\":$UNREAD_ID,\"type\""* ]]; then
      printf '  %sFAIL%s 传了 ?user_id=%s 就改变了 B 看到的内容（total %s → %s）\n' \
        "$RED" "$RESET" "$MY_ID" "$total_b_norm" "$total_b_param"; fail=$((fail+1))
    else
      printf '  %sPASS%s   ?user_id= 被忽略，B 的收件箱还是 B 的那 %s 条\n' \
        "$GREEN" "$RESET" "$total_b_norm"; pass=$((pass+1))
    fi
  fi

  # ---- 通知是「只读」的：M4 没有任何写通知的端点 ----
  # 基线必须在这里现取，不能复用解锁那一轮的 notif_a0：中间建 closed 那条 found 帖时
  # 合法地又给失主们发过通知，拿旧基线比会假红。
  notif_a_probe="$(db "SELECT count(*) FROM notifications WHERE user_id=$MY_ID")"
  req POST /api/my/notifications '{"type":"new_match"}' "$TOKEN"
  check '   POST 收件箱 → HTTP 405（没有写接口）' '405' "$STATUS"
  req PUT /api/my/notifications/unread '{"all":true}' "$TOKEN"
  check '   PUT /unread（少了一段 /read 的错路径）→ HTTP 404' '404' "$STATUS"
  if [[ "$HAS_PSQL" == 1 ]]; then
    check '   这些探测一条通知都没写' "$notif_a_probe" \
      "$(db "SELECT count(*) FROM notifications WHERE user_id=$MY_ID")"
  fi

  # ---- #41 举报：本里程碑最重要的一条是「它什么都改变不了」 ----
  square_before="$(req GET '/api/items?page_size=100'; printf '%s' "$BODY" \
    | grep -oE '"id":[0-9]+,"item_type"' | tr '\n' ' ')"
  notif_b1="$(db "SELECT count(*) FROM notifications WHERE user_id=$OTHER_ID")"
  credit_b1="$(db "SELECT credit_score FROM users WHERE id=$OTHER_ID")"
  status_b1="$(db "SELECT status FROM items WHERE id=$M4_LOST_B")"

  req POST "/api/items/$M4_LOST_B/report" \
    '{"reason_code":"spam","detail":"冒烟：这个人连发了五条一模一样的广告"}' "$TOKEN"
  check '#41 举报返回 HTTP 200'        '200'          "$STATUS"
  check '   data.status 是 open（M4 只能产生 open）' '"status":"open"' "$BODY"
  check '   data 里有 created_at'      '"created_at":"' "$BODY"
  REPORT_ID="$(jval "$BODY" '"data":\{"id":([0-9]+)')"

  if [[ "$HAS_PSQL" != 1 ]]; then
    skip '举报零自动后果（没有 psql：数不了 reports / notifications / credit_score）'
  elif [[ -z "$REPORT_ID" ]]; then
    skip '举报零自动后果（没从 #41 的响应里取到 id）'
  else
    check '   库里那一行的 status 也是 open（不是只有响应里写着 open）' 'open' \
      "$(db "SELECT status FROM reports WHERE id=$REPORT_ID")"
    check '   举报人记的是 token 里那个人' '1' \
      "$(db "SELECT (count(*)=1)::int FROM reports WHERE id=$REPORT_ID AND reporter_id=$MY_ID")"

    # ②被举报人没收到通知 ③帖子状态没变 ④信用分没动 ⑤广场顺序没动。
    # 这四条合起来才是「只记录，不仲裁」（定位原则 1）。拆开看每一条都像是
    # 「大概不会有人这么写」，合起来才挡得住把举报做成武器的那三种写法：
    # 通知对方（变成骚扰）、自动下架（变成 censorship 的前置）、扣分（变成私刑）。
    if [[ "$(db "SELECT count(*) FROM notifications WHERE user_id=$OTHER_ID")" == "$notif_b1" ]] \
    &&  [[ "$(db "SELECT credit_score FROM users WHERE id=$OTHER_ID")" == "$credit_b1" ]] \
    &&  [[ "$(db "SELECT status FROM items WHERE id=$M4_LOST_B")" == "$status_b1" ]]; then
      printf '  %sPASS%s   被举报人没被通知、信用分没动、帖子状态还是 %s\n' \
        "$GREEN" "$RESET" "$status_b1"; pass=$((pass+1))
    else
      printf '  %sFAIL%s   举报产生了自动后果：通知 %s→%s 信用 %s→%s 状态 %s→%s\n' "$RED" "$RESET" \
        "$notif_b1" "$(db "SELECT count(*) FROM notifications WHERE user_id=$OTHER_ID")" \
        "$credit_b1" "$(db "SELECT credit_score FROM users WHERE id=$OTHER_ID")" \
        "$status_b1" "$(db "SELECT status FROM items WHERE id=$M4_LOST_B")"; fail=$((fail+1))
    fi
    square_after="$(req GET '/api/items?page_size=100'; printf '%s' "$BODY" \
      | grep -oE '"id":[0-9]+,"item_type"' | tr '\n' ' ')"
    if [[ "$square_after" == "$square_before" ]]; then
      printf '  %sPASS%s   广场这一页的 id 顺序一条没变（举报数不是排序信号）\n' \
        "$GREEN" "$RESET"; pass=$((pass+1))
    else
      printf '  %sFAIL%s   举报改变了广场排序\n       %s前: %s%s\n       %s后: %s%s\n' \
        "$RED" "$RESET" "$DIM" "${square_before:0:200}" "$RESET" "$DIM" "${square_after:0:200}" "$RESET"
      fail=$((fail+1))
    fi

    # ③ 同一人对同一帖的待处理举报只有一条（uq_reports_open）
    req POST "/api/items/$M4_LOST_B/report" '{"reason_code":"spam","detail":"再举报一次"}' "$TOKEN"
    check '   同一人重复举报 → HTTP 409'  '409'                     "$STATUS"
    check '     code 是 REPORT_DUPLICATE' '"code":"REPORT_DUPLICATE"' "$BODY"
    # 换个理由也算重复：那条唯一索引管的是「同人对同帖的 open 举报」，不是「同一条理由」。
    req POST "/api/items/$M4_LOST_B/report" '{"reason_code":"fraud","detail":"换个理由再举报"}' "$TOKEN"
    check '     换一个 reason_code 仍然算重复' '"code":"REPORT_DUPLICATE"' "$BODY"
    check '   被拒的两次一共没多写行（仍是 1 行）' '1' \
      "$(db "SELECT count(*) FROM reports WHERE item_id=$M4_LOST_B")"

    # ④ 换一个用户举报同一条帖子是合法的 —— 多人举报是给 admin 的处理信号
    req POST "/api/items/$M4_LOST_B/report" '{"reason_code":"harassment","detail":"我也觉得有问题"}' "$TOKEN3"
    check '   第三个人举报同一条 → HTTP 200' '200' "$STATUS"
    if [[ "$HAS_PSQL" == 1 ]]; then
      check '   库里现在是 2 行（举报不是排他的）' '2' \
        "$(db "SELECT count(*) FROM reports WHERE item_id=$M4_LOST_B")"
    fi

    # ⑤ 表外的 reason_code → VALIDATION，而且不查库、不写行
    req POST "/api/items/$M4_LOST_B/report" '{"reason_code":"inappropriate","detail":"表外的值"}' "$TOKEN"
    check '   reason_code 传表外的值 → HTTP 400' '400' "$STATUS"
    check '     code 是 VALIDATION 而不是 CHECK_VIOLATION' '"code":"VALIDATION"' "$BODY"
  fi

  # 被举报的人连「谁举报了我」都查不到：M4 没有任何读 reports 的端点。
  # 这一条在集成测试里已经无条件覆盖（m4_report_test.go），放在这里的原因是
  # 它同时也是给 M5/M6 看的告示：别为了「让前端显示举报进度」先加一个 GET。
  req GET "/api/items/$M4_LOST_B/reports" '' "$TOKEN2"
  check '   被举报人查这条帖子的举报列表 → HTTP 404（根本没有这个读接口）' '404' "$STATUS"
  req GET "/api/items/$M4_FOUND/report" '' "$TOKEN"
  check '   GET #41 那条路径 → HTTP 405（它只接受 POST）' '405' "$STATUS"
fi


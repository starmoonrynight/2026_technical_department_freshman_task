# ---------------------------------------------------------------- M3 匹配
# deps: m1 m2
section "M3 匹配：#13 发帖时触发 + #20 查询（§12 判据①–⑤）"

# 这一节测的是「发帖之后系统在背后做了什么」，而那些事有一大半在 HTTP 响应里看不见。
# 所以每条判据都是「响应 + 直接数库」两半 —— 这正是 §10 第③层相对第②层的唯一增量。
#
# ⚠ 计数一律用 before/after 的**增量**，不写绝对值：这个脚本打的是开发库，
#   里面有上一轮、上上一轮留下的帖子。而「恰好 1 行」那种判据只有在干净库上才成立
#   （backend/smoketest/m3_match_test.go 里 TruncateAll 之后钉的就是恰好 1）。
#   本轮编号也因此进了标题和描述 —— 让上一轮的帖子尽量别也跨过 0.75 的通知线，
#   否则 notified_count 一轮比一轮大，日志里就看不出这次到底匹配上了什么。
#
# 但有三条判据是**精确**的，和库里有没有残留无关，所以下面直接按字面断言：
#   ④ lost 帖创建一行都不写；⑤ 地点「其他」的帖一行都不写；#20 查询本身一行都不写。
M3_TAG="m3$(date +%s)$RANDOM"
CAT_WALLET=36   # 衣物箱包 → 钱包，和 LOC_ID=70（图书馆）配成「同一个东西」的那一对
LOC_OTHER=3     # 「其他」：level 1 叶子、is_freeform=true、parent_id NULL —— Tier 1 的前提不成立

# 三个时刻都相对 now（UTC），这样「found_at 落在丢失窗口里 → S_time=1.0」
# 是这条链自己保证的，不依赖跑脚本的那一天：
#   last_seen 3 小时前 → found 90 分钟前 → lost_at 2 小时前
# 注意顺序：lost_at 必须**晚于** last_seen_at（M2 那条时间校验会拒反的），
# 而 found_at 夹在中间，才是 §5.3 说的「在你意识到丢之前就被捡到了」。
T_SEEN="$(date -u -d '-3 hours' +%Y-%m-%dT%H:%M:%SZ)"
T_LOST="$(date -u -d '-2 hours' +%Y-%m-%dT%H:%M:%SZ)"
T_FOUND="$(date -u -d '-90 minutes' +%Y-%m-%dT%H:%M:%SZ)"

pairs_before="$(db 'SELECT count(*) FROM match_pairs')"
notif_me_before="$(db "SELECT count(*) FROM notifications WHERE user_id=$MY_ID AND type='new_match'")"
notif_other_before="$(db "SELECT count(*) FROM notifications WHERE user_id=$OTHER_ID")"

# ---- 判据④：建 lost 帖只算不写 ----
lost_m3="$(printf '{"item_type":"lost","title":"%s 黑色长款钱包","description":"%s 夹层里有一张校园卡","category_id":%d,"location_id":%d,"location_detail":"三楼自习室B区","last_seen_at":"%s","lost_at":"%s","contact":"13800000000"}' \
  "$M3_TAG" "$M3_TAG" "$CAT_WALLET" "$LOC_ID" "$T_SEEN" "$T_LOST")"
req POST /api/items "$lost_m3" "$TOKEN"
check '④ 建 lost（钱包@图书馆）返回 HTTP 200' '200' "$STATUS"
M3_LOST_ID="$(itemIDOf "$BODY")"
check '  ④ 响应里有 matches_preview 键（空命中也必须是 []，不能整个消失）' '"matches_preview":[' "$BODY"
if [[ "$BODY" == *'"notified_count"'* ]]; then
  printf '  %sFAIL%s lost 帖的响应里出现了 notified_count —— §4：这两个键按 item_type 互斥\n' "$RED" "$RESET"; fail=$((fail+1))
else
  printf '  %sPASS%s lost 帖的响应里没有 notified_count（lost 方向谁都不通知）\n' "$GREEN" "$RESET"; pass=$((pass+1))
fi

# ---- 判据②：建 found 帖才写台账、才发通知，而且只给 lost 作者发 ----
found_m3="$(printf '{"item_type":"found","title":"%s 黑色长款钱包","description":"%s 夹层里有一张校园卡","category_id":%d,"location_id":%d,"location_detail":"三楼自习室B区","found_at":"%s","contact":"wx_smoke_finder"}' \
  "$M3_TAG" "$M3_TAG" "$CAT_WALLET" "$LOC_ID" "$T_FOUND")"
req POST /api/items "$found_m3" "$TOKEN2"
check '② 建 found（同分类同地点、时间在窗口内）返回 HTTP 200' '200' "$STATUS"
M3_FOUND_ID="$(itemIDOf "$BODY")"
check_re '  ② data.notified_count 出现了（哪怕是 0 也必须出现，那是去重的证据）' \
  '"notified_count":[0-9]+' "$BODY"
if [[ "$BODY" == *'"matches_preview"'* ]]; then
  printf '  %sFAIL%s found 帖的响应里出现了 matches_preview —— §2.3「平台不会通知 found 任何东西」\n' "$RED" "$RESET"; fail=$((fail+1))
else
  printf '  %sPASS%s found 帖的响应里没有 matches_preview\n' "$GREEN" "$RESET"; pass=$((pass+1))
fi

# ---- 判据①：#20 的 tier 1 形状（三个信号、权重之和 1.0、没有 attr、没有 location）----
req GET "/api/items/$M3_LOST_ID/matches" '' "$TOKEN"
check '① 本人查匹配返回 HTTP 200'   '200'          "$STATUS"
check '  code 是 OK'                '"code":"OK"' "$BODY"
check "  ① 本轮那条 found 帖出现在列表里（id=$M3_FOUND_ID）" "\"id\":$M3_FOUND_ID," "$BODY"
check '  ① tier 是 1（同地点 + 时间在窗口内，严格模式的前提成立）' '"tier":1' "$BODY"
check '  ① 每个信号都带 weight 和 score（前端要按它渲染分解表）' '"category":{"weight":' "$BODY"
if [[ "$BODY" == *'"attr"'* ]]; then
  printf '  %sFAIL%s breakdown 里有 attr —— S_attr 已经随 color/brand 两列一起删掉了（§5.4）\n' "$RED" "$RESET"; fail=$((fail+1))
else
  printf '  %sPASS%s breakdown 里没有 attr 信号\n' "$GREEN" "$RESET"; pass=$((pass+1))
fi
# Tier 1 只有三个信号：地点是**硬筛选条件**而不是打分项，所以那个键必须整个不存在。
# 「存在但 weight 为 0」是另一种实现，它会让 breakdown 的加权和对不上总分。
if [[ "$BODY" == *'"location"'* ]]; then
  printf '  %sFAIL%s Tier 1 的 breakdown 里有 location 信号\n' "$RED" "$RESET"; fail=$((fail+1))
else
  printf '  %sPASS%s Tier 1 的 breakdown 里没有 location 信号（0.45/0.40/0.15 三条加起来正好 1.0）\n' "$GREEN" "$RESET"; pass=$((pass+1))
fi
if [[ "$BODY" == *'"notice"'* ]]; then
  printf '  %sFAIL%s Tier 1 带了放宽筛选的横幅文案（那是 Tier 2 才该出现的东西）\n' "$RED" "$RESET"; fail=$((fail+1))
else
  printf '  %sPASS%s Tier 1 没有横幅文案\n' "$GREEN" "$RESET"; pass=$((pass+1))
fi
# 匹配列表里 found 候选的联系方式必须是锁着的 —— 它不能成为 M4 #21 的旁路。
if [[ "$BODY" == *wx_smoke_finder* ]]; then
  printf '  %sFAIL%s 匹配列表里出现了 found 帖的 contact（wx_smoke_finder）—— 认领名单会因此不再完整\n' "$RED" "$RESET"; fail=$((fail+1))
else
  printf '  %sPASS%s 匹配列表里没有 found 帖的 contact（和 #14 同一条规则）\n' "$GREEN" "$RESET"; pass=$((pass+1))
fi

# ---- #20 鉴权与参数 ----
req GET "/api/items/$M3_LOST_ID/matches"
check '#20 不带 token → HTTP 401'   '401'                  "$STATUS"
check '  code 是 UNAUTHORIZED'      '"code":"UNAUTHORIZED"' "$BODY"
req GET "/api/items/$M3_LOST_ID/matches" '' "$TOKEN2"
check '  别人的帖子 → HTTP 403（§4：本人或 Admin）' '403' "$STATUS"
check '  code 是 FORBIDDEN'         '"code":"FORBIDDEN"' "$BODY"
# 三个查询参数都在 handler 里原样收下、在 service 里解析，所以非法值必须变成
# 带字段名的 VALIDATION，而不是「静默用默认值」—— 后者会让用户看到的列表和 URL 不一致。
req GET "/api/items/$M3_LOST_ID/matches?tier=3" '' "$TOKEN"
check '  tier=3 → HTTP 400'         '400'                  "$STATUS"
check '    错误指向 tier'           '"field":"tier"' "$BODY"
req GET "/api/items/$M3_LOST_ID/matches?top=0" '' "$TOKEN"
check '  top=0 → HTTP 400（不是悄悄变成 10）' '400' "$STATUS"
req GET "/api/items/$M3_LOST_ID/matches?min_score=1.5" '' "$TOKEN"
check '  min_score=1.5 → HTTP 400'  '400'                  "$STATUS"

# ---- 判据⑤：地点选「其他」→ Tier 2，四个信号 + 横幅，而且照样一行都不写 ----
freeform_m3="$(printf '{"item_type":"lost","title":"%s 黑色长款钱包","description":"%s 夹层里有一张校园卡","category_id":%d,"location_id":%d,"location_detail":"图书馆三楼","last_seen_at":"%s","lost_at":"%s","contact":"13900000000"}' \
  "$M3_TAG" "$M3_TAG" "$CAT_WALLET" "$LOC_OTHER" "$T_SEEN" "$T_LOST")"
pairs_before_t2="$(db 'SELECT count(*) FROM match_pairs')"
req POST /api/items "$freeform_m3" "$TOKEN"
check '⑤ 建 lost（地点：其他）返回 HTTP 200' '200' "$STATUS"
M3_T2_ID="$(itemIDOf "$BODY")"
# 这条是**精确**的：#13 只跑 Tier 1，而 Tier 1 的前提在这里不成立（没有叶子可比），
# 所以预览一定是空数组 —— 库里有没有别的帖子都改不了这个结论。
check '  ⑤ 发帖时的预览是 []（#13 只做 Tier 1，绝不偷偷放宽）' '"matches_preview":[]' "$BODY"

req GET "/api/items/$M3_T2_ID/matches" '' "$TOKEN"
check '  ⑤ 查匹配时 auto 降到 Tier 2 → HTTP 200' '200' "$STATUS"
check '  ⑤ tier 是 2'               '"tier":2'     "$BODY"
check '  ⑤ 带提示文案（前端靠它显示黄色横幅）' '"notice":"' "$BODY"
check '  ⑤ 四个信号：多出 location 那一档' '"location":{"weight":' "$BODY"
check '  ⑤ 放宽了地点之后确实捞到了那条图书馆的拾物帖' "\"id\":$M3_FOUND_ID," "$BODY"
# 显式要 Tier 1 时**不**偷偷放宽：给空列表才是诚实的答案。
req GET "/api/items/$M3_T2_ID/matches?tier=1" '' "$TOKEN"
check '  ⑤ 显式 tier=1 → 仍然是 1 档、list 为空' '"tier":1,"list":[]' "$BODY"

# ---- 三处「一行都不写」，只能直接数库 ----
# ④ lost 帖创建、⑤ Tier 2 的结果、#20 查询本身。
# 判据③说的「重复不产生第二行」在这里的可执行形式就是最后这一条：
# 用户反复刷新详情页不该往台账里灌数据（§3.7）。
if [[ "$HAS_PSQL" != 1 ]]; then
  skip '台账/通知的增量（本机找不到 psql，或连不上开发库）'
else
  pairs_now="$(db 'SELECT count(*) FROM match_pairs')"
  notif_me="$(db "SELECT count(*) FROM notifications WHERE user_id=$MY_ID AND type='new_match'")"
  notif_other="$(db "SELECT count(*) FROM notifications WHERE user_id=$OTHER_ID")"

  pairs_delta=$((pairs_now - pairs_before))
  if [[ "$pairs_delta" -ge 1 ]]; then
    printf '  %sPASS%s ② match_pairs 新增了 %d 行（found 帖创建这条路径是唯一会写的）\n' \
      "$GREEN" "$RESET" "$pairs_delta"; pass=$((pass+1))
  else
    printf '  %sFAIL%s ② 建了 matched 的 found 帖，match_pairs 却一行都没加（%s → %s）\n' \
      "$RED" "$RESET" "$pairs_before" "$pairs_now"; fail=$((fail+1))
  fi
  if [[ $((notif_me - notif_me_before)) -ge 1 ]]; then
    printf '  %sPASS%s ② lost 作者（我）收到 %d 条 new_match\n' \
      "$GREEN" "$RESET" $((notif_me - notif_me_before)); pass=$((pass+1))
  else
    printf '  %sFAIL%s ② lost 作者一条通知都没收到 —— 整个 M3 的存在理由就是这一条\n' "$RED" "$RESET"; fail=$((fail+1))
  fi
  # 这条是**精确**的：found 作者永远不该因为匹配被通知，和残留数据无关。
  if [[ "$notif_other" == "$notif_other_before" ]]; then
    printf '  %sPASS%s ② found 作者的通知数没变（§2.3：平台不会通知 found 任何东西）\n' "$GREEN" "$RESET"; pass=$((pass+1))
  else
    printf '  %sFAIL%s found 作者被通知了：从 %s 条变成 %s 条\n' \
      "$RED" "$RESET" "$notif_other_before" "$notif_other"; fail=$((fail+1))
  fi

  # ⑤ 之后仍然不写：Tier 2 的结果一律不进台账（§3.7）
  if [[ "$(db 'SELECT count(*) FROM match_pairs')" == "$pairs_before_t2" ]]; then
    printf '  %sPASS%s ⑤ 地点「其他」的帖（Tier 2）一行都没写台账\n' "$GREEN" "$RESET"; pass=$((pass+1))
  else
    printf '  %sFAIL%s Tier 2 的结果被写进了台账\n' "$RED" "$RESET"; fail=$((fail+1))
  fi

  # #20 查询本身：再读两次，台账和通知都必须纹丝不动
  read_only_before="$(db 'SELECT count(*) FROM match_pairs')"
  req GET "/api/items/$M3_LOST_ID/matches" '' "$TOKEN"
  req GET "/api/items/$M3_LOST_ID/matches?tier=2" '' "$TOKEN"
  if [[ "$(db 'SELECT count(*) FROM match_pairs')" == "$read_only_before" ]]; then
    printf '  %sPASS%s ③ 刷了两次 #20，match_pairs 仍是 %s 行（查询接口只读）\n' \
      "$GREEN" "$RESET" "$read_only_before"; pass=$((pass+1))
  else
    printf '  %sFAIL%s 查询接口往台账里灌了数据：%s → %s\n' \
      "$RED" "$RESET" "$read_only_before" "$(db 'SELECT count(*) FROM match_pairs')"; fail=$((fail+1))
  fi

  # 落库的那一行确实带了 breakdown（JSONB 列），这是 §5.7「调试器」的数据源。
  # 数一下本轮这一对有没有行，而不是数某一行——中文标题不能当 psql 参数（代码页转换）。
  if [[ -n "$M3_FOUND_ID" ]]; then
    check '  台账里能按 found_item_id 查到本轮那一对' '1' \
      "$(db "SELECT (count(*)>0)::int FROM match_pairs WHERE found_item_id=$M3_FOUND_ID")"
  fi
fi

# ---- 判据⑥（2026-10-07 补）：改帖重新触发匹配，同一对第二次插入不去重就重复打扰 ----
#
# 计划里判据③写的是「重复建同样的 found 帖 → ON CONFLICT 生效」，但走 HTTP 再发一次帖
# 拿到的是**新的 item id**，台账的键是 (lost_item_id, found_item_id)，永远不构成冲突。
# 真正会让同一对第二次参与匹配的动作是**改帖**（§3.7 用途① 举的正是这个例子），
# 所以 service 里补了 Match.OnUpdated，§9 的 trigger 多一个值 found_updated。
#
# 这条链同时补了一个功能洞：小王随手发「钱包」、后来改成「黑色长款钱包」，
# 以前系统不会重算，小李永远等不到通知 —— 现在会。
lost6="$(printf '{"item_type":"lost","title":"%s 黑色长款钱包","description":"%s 夹层里有一张校园卡","category_id":%d,"location_id":%d,"location_detail":"三楼自习室B区","last_seen_at":"%s","lost_at":"%s","contact":"13800000000"}' \
  "$M3_TAG" "$M3_TAG" "$CAT_WALLET" "$LOC_ID" "$T_SEEN" "$T_LOST")"
req POST /api/items "$lost6" "$TOKEN"
check '⑥ 再建一条同款 lost 返回 HTTP 200' '200' "$STATUS"
M3_6_LOST_ID="$(itemIDOf "$BODY")"

# 弱文本：标题只有「钱包」、描述留空 → 分数落在通知线以下，发帖时一行都不该写。
found6="$(printf '{"item_type":"found","title":"钱包","description":"","category_id":%d,"location_id":%d,"location_detail":"三楼自习室B区","found_at":"%s","contact":"wx_smoke_finder6"}' \
  "$CAT_WALLET" "$LOC_ID" "$T_FOUND")"
req POST /api/items "$found6" "$TOKEN2"
check '  ⑥ 弱文本 found 帖返回 HTTP 200' '200' "$STATUS"
M3_6_FOUND_ID="$(itemIDOf "$BODY")"

if [[ "$HAS_PSQL" != 1 ]]; then
  skip '⑥ 改帖重匹配 + 第二次不重复通知（本机没有 psql 或连不上开发库）'
elif [[ -z "$M3_6_FOUND_ID" || -z "$M3_6_LOST_ID" ]]; then
  skip '⑥ 改帖重匹配（没能取出这一轮的帖子 id）'
else
  rows6_before="$(db "SELECT count(*) FROM match_pairs WHERE found_item_id=$M3_6_FOUND_ID")"
  if [[ "$rows6_before" == "0" ]]; then
    printf '  %sPASS%s ⑥ 弱文本（分数不到 0.75）发帖时一行台账都没写\n' "$GREEN" "$RESET"; pass=$((pass+1))
  else
    printf '  %sFAIL%s ⑥ 弱文本却写了 %s 行台账\n' "$RED" "$RESET" "$rows6_before"; fail=$((fail+1))
  fi

  notif_me6_before="$(db "SELECT count(*) FROM notifications WHERE user_id=$MY_ID AND type='new_match'")"
  notif_finder6="$(db "SELECT count(*) FROM notifications WHERE user_id=$OTHER_ID")"

  # 把帖子改准 → 同一个 found_item_id 第一次够线 → 写一行、通知失主一次。
  strong6="$(printf '{"item_type":"found","title":"%s 黑色长款钱包","description":"%s 夹层里有一张校园卡","category_id":%d,"location_id":%d,"location_detail":"三楼自习室B区","found_at":"%s","contact":"wx_smoke_finder6"}' \
    "$M3_TAG" "$M3_TAG" "$CAT_WALLET" "$LOC_ID" "$T_FOUND")"
  req PUT "/api/items/$M3_6_FOUND_ID" "$strong6" "$TOKEN2"
  check '  ⑥ 改帖（弱文本→强文本）返回 HTTP 200' '200' "$STATUS"
  check '  ⑥ 改准之后这一对进了台账（OnUpdated 真的跑了匹配）' '1' \
    "$(db "SELECT (count(*)>0)::int FROM match_pairs WHERE found_item_id=$M3_6_FOUND_ID AND lost_item_id=$M3_6_LOST_ID")"
  notif_me6="$(db "SELECT count(*) FROM notifications WHERE user_id=$MY_ID AND type='new_match'")"
  rows6_strong="$(db "SELECT count(*) FROM match_pairs WHERE found_item_id=$M3_6_FOUND_ID")"
  if [[ "$notif_me6" -gt "$notif_me6_before" ]]; then
    printf '  %sPASS%s ⑥ 失主因为这次改帖收到了 new_match（%s → %s）\n' \
      "$GREEN" "$RESET" "$notif_me6_before" "$notif_me6"; pass=$((pass+1))
  else
    printf '  %sFAIL%s ⑥ 台账写了行、失主却没收到通知 —— 两者必须在同一个事务里\n' "$RED" "$RESET"; fail=$((fail+1))
  fi

  # **判据③的可执行形式**：同一对第二次参与匹配（再改一次，只动 location_detail）。
  # 少了 ON CONFLICT 的话，小王每改一个字都会把小李通知一遍。
  pairs_total6="$(db 'SELECT count(*) FROM match_pairs')"
  # 和 strong6 只差 location_detail：分数不变，仍然够通知线，于是 RecordMatches
  # 又被调用一次 —— 这次必须被那条唯一约束吃掉。
  strong6b="$(printf '{"item_type":"found","title":"%s 黑色长款钱包","description":"%s 夹层里有一张校园卡","category_id":%d,"location_id":%d,"location_detail":"一楼大厅服务台","found_at":"%s","contact":"wx_smoke_finder6"}' \
    "$M3_TAG" "$M3_TAG" "$CAT_WALLET" "$LOC_ID" "$T_FOUND")"
  req PUT "/api/items/$M3_6_FOUND_ID" "$strong6b" "$TOKEN2"
  check '  ⑥ 第二次改同一条帖返回 HTTP 200' '200' "$STATUS"
  if [[ "$(db "SELECT count(*) FROM match_pairs WHERE found_item_id=$M3_6_FOUND_ID")" == "$rows6_strong" ]] \
     && [[ "$(db 'SELECT count(*) FROM match_pairs')" == "$pairs_total6" ]]; then
    printf '  %sPASS%s ③ 同一对第二次插入被 ON CONFLICT 挡掉，match_pairs 仍是 %s 行\n' \
      "$GREEN" "$RESET" "$pairs_total6"; pass=$((pass+1))
  else
    printf '  %sFAIL%s ③ 第二次改帖多写了台账：%s → %s（重复通知就是这么来的）\n' \
      "$RED" "$RESET" "$pairs_total6" "$(db 'SELECT count(*) FROM match_pairs')"; fail=$((fail+1))
  fi
  if [[ "$(db "SELECT count(*) FROM notifications WHERE user_id=$MY_ID AND type='new_match'")" == "$notif_me6" ]]; then
    printf '  %sPASS%s ③ 第二次改帖没有再通知失主（仍是 %s 条）\n' \
      "$GREEN" "$RESET" "$notif_me6"; pass=$((pass+1))
  else
    printf '  %sFAIL%s ③ 第二次改帖又通知了失主\n' "$RED" "$RESET"; fail=$((fail+1))
  fi
  # found 作者在这一整段里始终一条都没被通知（不对称在改帖上同样成立）。
  if [[ "$(db "SELECT count(*) FROM notifications WHERE user_id=$OTHER_ID")" == "$notif_finder6" ]]; then
    printf '  %sPASS%s ⑥ 拾物作者改帖前后都没收到任何通知\n' "$GREEN" "$RESET"; pass=$((pass+1))
  else
    printf '  %sFAIL%s 拾物作者因为改帖被通知了 —— §2.3 明确禁止\n' "$RED" "$RESET"; fail=$((fail+1))
  fi

  # 改一条 closed 的 found 帖不该再叫醒任何人（§8 允许改 closed 帖，比如改错电话）。
  req PATCH "/api/items/$M3_6_FOUND_ID/status" '{"status":"closed"}' "$TOKEN2"
  check '  ⑥ 关掉这条 found 帖返回 HTTP 200' '200' "$STATUS"
  req PUT "/api/items/$M3_6_FOUND_ID" \
    "$(printf '{"item_type":"found","title":"%s 黑色长款钱包","description":"%s 夹层里有一张校园卡","category_id":%d,"location_id":%d,"location_detail":"宿管阿姨处","found_at":"%s","contact":"wx_smoke_finder6"}' \
      "$M3_TAG" "$M3_TAG" "$CAT_WALLET" "$LOC_ID" "$T_FOUND")" "$TOKEN2"
  check '  ⑥ 改一条 closed 的 found 帖仍然返回 HTTP 200' '200' "$STATUS"
  if [[ "$(db "SELECT count(*) FROM notifications WHERE user_id=$MY_ID AND type='new_match'")" == "$notif_me6" ]]; then
    printf '  %sPASS%s ⑥ closed 的 found 帖改帖没有再通知失主（东西已经还回去了）\n' "$GREEN" "$RESET"; pass=$((pass+1))
  else
    printf '  %sFAIL%s closed 的 found 帖改帖又通知了失主\n' "$RED" "$RESET"; fail=$((fail+1))
  fi
fi


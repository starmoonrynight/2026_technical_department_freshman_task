# ---------------------------------------------------------------- #38 健康检查
# deps:
section "#38 GET /api/health"
body="$(curl -s -D "$tmp_headers" "$BASE/api/health")"
check 'code 是 OK'                    '"code":"OK"'  "$body"
check 'data.status 是 ok'             '"status":"ok"' "$body"
check 'data.db 是 ok'                 '"db":"ok"'    "$body"
check 'data.version 非空'             '"version":"'  "$body"
check '信封里有 request_id'           '"request_id":"' "$body"
check '响应头回写 X-Request-ID'       'X-Request-Id:' "$(cat "$tmp_headers")"

# request_id 的形状：日期-16位十六进制（8 字节随机数）
rid="$(printf '%s' "$body" | sed -n 's/.*"request_id":"\([^"]*\)".*/\1/p')"
if [[ "$rid" =~ ^[0-9]{8}-[0-9a-f]{16}$ ]]; then
  printf '  %sPASS%s request_id 形状正确（%s）\n' "$GREEN" "$RESET" "$rid"
  pass=$((pass + 1))
else
  printf '  %sFAIL%s request_id 形状不对：%s（期望 YYYYMMDD-16位十六进制）\n' "$RED" "$RESET" "$rid"
  fail=$((fail + 1))
fi

# ---------------------------------------------------------------- 信封契约
section "信封契约：非正常路径也必须是同一个形状"

body="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/api/health")"
check 'POST /api/health 返回 405'     '405' "$body"
body="$(curl -s -X POST "$BASE/api/health")"
check '  且 body 是 METHOD_NOT_ALLOWED' '"code":"METHOD_NOT_ALLOWED"' "$body"

body="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/definitely-not-real")"
check '不存在的路径返回 404'          '404' "$body"
body="$(curl -s "$BASE/api/definitely-not-real")"
check '  且 body 是 NOT_FOUND（不是 gin 的纯文本 404）' '"code":"NOT_FOUND"' "$body"
check '  且仍然带 request_id'         '"request_id":"' "$body"

# ---------------------------------------------------------------- RequestID 防线
section "RequestID：沿用客户端传入的，且过滤危险字符"

# 客户端自带的 id 必须原样沿用 —— 这样前端的「一次操作」能和后端日志对上
out="$(curl -s -D "$tmp_headers" -H 'X-Request-ID: 20261007-manual01' "$BASE/api/health")"
check '沿用客户端传入的 request_id'   '20261007-manual01' "$out"
check '  响应头也原样回写'            '20261007-manual01' "$(cat "$tmp_headers")"

# 非白名单字符必须被过滤掉。白名单是 [A-Za-z0-9._-]，
# 换行、制表符这些能把日志撕成两行的字符都在名单外 —— 这是日志注入的防线。
# 这里用 ! @ 空格 来测，而不是真的塞换行符：带换行的 header 值 curl 根本不会发出去。
out="$(curl -s -H 'X-Request-ID: abc!!def@@ ghi' "$BASE/api/health")"
check '非白名单字符被过滤（abc!!def@@ ghi → abcdefghi）' '"request_id":"abcdefghi"' "$out"


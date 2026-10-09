#!/usr/bin/env bash
#
# 手工冒烟脚本（计划 §10 的第③层）—— 运行器。
#
# 它和 backend/smoketest/ 的区别：
#   smoketest/  由 go test 驱动，跑在临时测试库 lostfound_test 上，验证「代码逻辑对」
#   这个脚本    打的是你正在跑的那个 dev server 和它的开发库，验证「环境活着」
# 两个都要跑：前者过了不代表你的 docker / .env / 端口没问题。
#
# 本文件只做四件事：解析参数、按依赖补齐要跑的节、跑连通性前置检查、按顺序 source
# 那些节。判据全在 scripts/smoke/ 下面，一个里程碑一个文件：
#
#   smoke/lib.sh          公共工具（check / req / db / jval…），不含任何判据
#   smoke/m0-baseline.sh  #38 健康检查、信封契约、RequestID 防线
#   smoke/m1-auth.sh      #1–#5 认证 + 封禁
#   smoke/m2-items.sh     #6–#8、#13–#19、#40、#42 上传、字典、帖子
#   smoke/m3-match.sh     #20 匹配
#   smoke/m4-notify.sh    #21、#22、#30–#32、#41 解锁、收件箱、举报
#   smoke/m5-return.sh    #23–#29、#33 归还确认与积分
#
# 用法（Git Bash）：
#   docker compose up -d                          # Docker 路径
#   bash scripts/setup-postgres.sh                # 或：原生 Windows PostgreSQL 路径
#   cd backend && go run ./cmd/server &
#   bash scripts/smoke.sh                         # 全部跑（默认）
#   bash scripts/smoke.sh m3                      # 跑 m3，并自动补上它依赖的 m1 m2
#   bash scripts/smoke.sh m5                      # m5 自包含，真的只跑 m5
#   bash scripts/smoke.sh --list                  # 看有哪些节、谁依赖谁
#
# 换端口：BASE=http://localhost:9090 bash scripts/smoke.sh
#
# ⚠ 「只跑某一个里程碑」是有前提的：后面的节要借用前面节建出来的用户和帖子
#   （$TOKEN、$MY_ID、$LOC_ID 这些全局变量），所以它必须跟着前面的节一起跑。
#   这种借依赖写在每个文件开头的 `# deps:` 一行里，运行器照它自动补齐 ——
#   宁可多跑几节，也不要因为 $TOKEN 是空的而报一堆假的 401。
#   目前只有 m5 做到了自包含（自己注册用户、自己发帖、自己传图），所以只有它能真的单跑。

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SMOKE_DIR="$ROOT/scripts/smoke"
BASE="${BASE:-http://localhost:8080}"

# 唯一的顺序来源：既是可用的里程碑列表，也是执行顺序。
# 新加一节就在这里加一行，同时建一个 <这一行>-<名字>.sh。
ORDER=(m0 m1 m2 m3 m4 m5)

# ---------------------------------------------------------------- 参数
usage() {
  cat <<'EOF'
用法：bash scripts/smoke.sh [节 ...]        不带参数 = 跑全部

  m0 / m1 / ... / m5   按里程碑选节，会自动补上它依赖的前面几节
  --list               列出所有节及其依赖
  -h, --help           这段文字

环境变量：
  BASE=http://localhost:9090   要打的 dev server 地址（默认 8080）
EOF
}

file_of() { # file_of <mN> —— 该节对应的文件名；不存在则返回非 0
  local f
  for f in "$SMOKE_DIR/$1"-*.sh; do
    [[ -e "$f" ]] && { basename "$f"; return 0; }
  done
  return 1
}

deps_of() { # deps_of <文件> —— 读那一行 `# deps:`；没有这行 = 谁都不依赖
  sed -n 's/^# deps:[[:space:]]*//p' "$SMOKE_DIR/$1" | head -1
}

if [[ $# -gt 0 ]]; then
  case "${1:-}" in
    -h|--help) usage; exit 0 ;;
    --list)
      printf '%-18s %s\n' '节' '依赖（空 = 自包含）'
      for m in "${ORDER[@]}"; do
        f="$(file_of "$m")" || { printf '%-18s %s\n' "$m" '（没有对应的文件）'; continue; }
        printf '%-18s %s\n' "$f" "$(deps_of "$f")"
      done
      exit 0 ;;
  esac
fi

requested=()
for arg in "$@"; do
  if [[ "$arg" == "all" ]]; then
    requested=("${ORDER[@]}")
    continue
  fi
  # m5 / m5-return / smoke/m5-return.sh 都算同一个节。
  arg="$(basename "$arg" .sh)"
  arg="m${arg#m}"; arg="${arg%%-*}"
  if [[ "$arg" == "m" || ! " ${ORDER[*]} " == *" $arg "* ]]; then
    printf '不认识的节：%s（可用：%s）\n\n' "$arg" "${ORDER[*]}" >&2
    usage >&2
    exit 2
  fi
  requested+=("$arg")
done
[[ ${#requested[@]} -gt 0 ]] || requested=("${ORDER[@]}")

# 补依赖：反复扫，直到不再产生新的节为止（deps 只允许向前指，所以必然收敛）。
selected=("${requested[@]}")
changed=1
while [[ "$changed" == 1 ]]; do
  changed=0
  for m in "${selected[@]}"; do
    f="$(file_of "$m")" || { printf '找不到 %s 对应的文件：scripts/smoke/%s-*.sh\n' "$m" "$m" >&2; exit 2; }
    for d in $(deps_of "$f"); do
      [[ " ${selected[*]} " == *" $d "* ]] || { selected+=("$d"); changed=1; }
    done
  done
done

# 按 ORDER 的固定顺序执行，而不是按用户输入的先后 —— 借来的全局变量必须先生成。
sorted=()
for m in "${ORDER[@]}"; do
  [[ " ${selected[*]} " == *" $m "* ]] && sorted+=("$m")
done

added=()
for m in "${sorted[@]}"; do
  [[ " ${requested[*]} " == *" $m "* ]] || added+=("$m")
done

# ---------------------------------------------------------------- 公共工具
# shellcheck source=smoke/lib.sh
source "$SMOKE_DIR/lib.sh"

if [[ ${#added[@]} -gt 0 ]]; then
  printf '%s为了跑 %s，运行器先补跑了它依赖的节：%s%s\n' \
    "$DIM" "${requested[*]}" "${added[*]}" "$RESET"
fi
section "本轮跑：${sorted[*]}"

# ---------------------------------------------------------------- 前置检查
section "前置：服务在跑吗"
if ! curl -s -o /dev/null --max-time 5 "$BASE/api/health"; then
  printf '%s连不上 %s%s\n' "$RED" "$BASE" "$RESET"
  cat <<'EOF'

  按顺序检查：
    1. 数据库起来了吗
         Docker 路径：  docker compose up -d        （docker compose ps 看状态）
         原生路径：     pg_isready -h 127.0.0.1 -p 5432
                        没起就 bash scripts/setup-postgres.sh
    2. cd backend && cp .env.example .env
       然后给 JWT_SECRET 填一个随机串：openssl rand -hex 32
    3. cd backend && go run ./cmd/server
       看终端里有没有 startup.failed —— 它会直接告诉你缺哪个变量或连不上哪个库

EOF
  exit 1
fi
printf '  %sPASS%s %s 有响应\n' "$GREEN" "$RESET" "$BASE"
pass=$((pass + 1))

# ---------------------------------------------------------------- 各节
for m in "${sorted[@]}"; do
  # shellcheck source=/dev/null
  source "$SMOKE_DIR/$(file_of "$m")"
done

# ---------------------------------------------------------------- 汇总
printf '\n%s────────────────────────────%s\n' "$DIM" "$RESET"
if [[ $fail -eq 0 ]]; then
  printf '%s全部通过：%d 项%s' "$GREEN" "$pass" "$RESET"
  [[ $skipped -gt 0 ]] && printf '，%s跳过 %d 项%s' "$YELLOW" "$skipped" "$RESET"
  printf '\n'
else
  printf '%s%d 项通过，%d 项失败%s' "$RED" "$pass" "$fail" "$RESET"
  [[ $skipped -gt 0 ]] && printf '，%s%d 项跳过%s' "$YELLOW" "$skipped" "$RESET"
  printf '\n'
fi
# 只有跑到 m1 才有 $U —— 单独跑 m5 时它用的是自己那五个用户，这句就不打印。
[[ -n "${U:-}" ]] && \
  printf '%s开发库里留下了本轮的冒烟用户 %s；要清掉就在 pgAdmin 里删掉这一行。%s\n' "$DIM" "$U" "$RESET"
printf '%s拿上面任意一条的 request_id 去后端日志里 grep，能看到完整链路%s\n\n' "$DIM" "$RESET"
[[ $fail -eq 0 ]] || exit 1
exit 0

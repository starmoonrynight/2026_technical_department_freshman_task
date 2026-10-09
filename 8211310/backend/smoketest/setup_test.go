// Package smoketest 是计划 §10 的第②层：API 冒烟测试。
//
// 它用 httptest + 真实测试库跑完整链路。关键是 router.Setup 和 main.go 调的是同一个函数，
// 所以「测试过了但线上不行」这类事故不会发生。
//
// 断言纪律：只断言 code，不断言 message —— message 是给人看的中文，随时可以改措辞（§8）。
package smoketest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"lostfound/internal/apperr"
	"lostfound/internal/auth"
	"lostfound/internal/config"
	"lostfound/internal/database"
	"lostfound/internal/router"
)

// testDBName 是集成测试专用的库名。它和开发库 lostfound 完全隔离 ——
// 测试里 TRUNCATE 一切，绝不能误伤开发数据。
const testDBName = "lostfound_test"

// harness 在 TestMain 里建一次，全包共享。
// 测试之间靠 TruncateAll 清场，不靠重建整个环境（重建一次要跑完整迁移，太慢）。
var harness *Harness

func TestMain(m *testing.M) {
	dsn := strings.TrimSpace(os.Getenv("TEST_DB_DSN"))

	if dsn == "" {
		// REQUIRE_INTEGRATION=1 时「没配 DSN」直接算失败，而不是静默跳过。
		// 这条兜底存在的唯一理由：防止「全绿但其实一条集成测试都没跑」这种最坏的假象。
		if os.Getenv("REQUIRE_INTEGRATION") == "1" {
			fmt.Fprint(os.Stderr, `
================================================================================
 集成测试被要求必须运行（REQUIRE_INTEGRATION=1），但 TEST_DB_DSN 是空的。
 这是失败，不是跳过 —— 否则「全绿」可能意味着一条集成测试都没跑。

 先确认 PostgreSQL 服务在跑（scripts/setup-postgres.sh 起过的那个），然后：

   cd backend
   TEST_DB_DSN="postgres://lf:lf@127.0.0.1:5432/postgres?sslmode=disable" \
   REQUIRE_INTEGRATION=1 go test ./...

 注意 DSN 指向的是维护库 postgres，测试代码会自己创建并使用 lostfound_test。
================================================================================
`)
			os.Exit(1)
		}

		fmt.Print(`
--------------------------------------------------------------------------------
 跳过 smoketest 包：TEST_DB_DSN 未设置，所以集成测试一条都没跑。
 这不算失败（纯单元测试仍然会跑），但也不代表功能被验证过。

 要真正跑集成测试：
   cd backend
   TEST_DB_DSN="postgres://lf:lf@127.0.0.1:5432/postgres?sslmode=disable" go test ./...
--------------------------------------------------------------------------------
`)
		os.Exit(0)
	}

	h, err := setup(dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n集成测试环境搭建失败: %v\n\n", err)
		os.Exit(1)
	}
	harness = h
	defer h.Close()

	os.Exit(m.Run())
}

// setup 建库 → 迁移 → 装配和生产完全相同的路由 → 起一个 httptest server。
func setup(dsn string) (*Harness, error) {
	ctx := context.Background()

	if err := ensureTestDatabase(ctx, dsn); err != nil {
		return nil, err
	}

	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("解析 TEST_DB_DSN 失败: %w", err)
	}
	poolCfg.ConnConfig.Database = testDBName
	poolCfg.MaxConns = 5

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("连不上测试库 %s: %w", testDBName, err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 先 down 到 0 再 up：保证 schema 是干净的，不会被上一次跑剩的结构污染。
	// 尤其是改过迁移文件之后，只有 down-up 才能暴露「迁移不可重入」这种问题。
	if err := database.MigrateDown(pool, logger); err != nil {
		pool.Close()
		return nil, err
	}
	if err := database.Migrate(pool, logger); err != nil {
		pool.Close()
		return nil, err
	}

	gin.SetMode(gin.TestMode) // 否则 gin 会把路由表打进测试输出，淹没真正的失败信息

	// 图片目录必须是**临时的**：M2 起 #6 上传会真的往磁盘写文件，
	// 而 TruncateAll 只清数据库、不清磁盘。指向仓库里的 ./uploads 的话，
	// 每跑一次测试就多几十个垃圾文件，而且「删帖时磁盘文件消失」这条断言
	// 会被上一次跑剩的同名文件干扰。
	uploadDir, err := os.MkdirTemp("", "lostfound-smoketest-uploads-")
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("建临时上传目录失败: %w", err)
	}

	// cfg 必须是**能通过 Setup 内部校验**的：M1 起 Setup 会真的 new 一个
	// auth.TokenSigner，它拒绝空密钥和非正有效期。
	// 密钥只活在这个测试进程里，和任何环境的真密钥无关。
	//
	// ⚠ M3 起 Match 那四个字段**必须有值，而且必须和 .env.example 的默认值一致**。
	// 留成零值的话 NotifyThreshold 是 0 —— 任何一条候选都会越过通知线，
	// 「只给 score≥0.75 的 lost 作者发通知」这条规则会被静默跳过，
	// 而测试看起来完全正常（通知确实发出去了，只是发得太宽）。
	// 阈值属于运营参数（§9），不写进 cfg 就等于测了一个生产不存在的配置。
	cfg := config.Config{
		Env:           "test",
		JWTSecret:     "smoketest-secret-0123456789abcdef0123456789abcdef",
		JWTExpire:     time.Hour,
		UploadDir:     uploadDir,
		UploadBaseURL: "/uploads",
		Match: config.MatchConfig{
			TimeToleranceHours: 24,
			DecayDays:          14,
			NotifyThreshold:    0.75,
			ShowThreshold:      0.55,
		},
	}

	engine, err := router.Setup(cfg, pool)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("装配路由失败: %w", err)
	}

	return &Harness{
		Pool:   pool,
		Server: httptest.NewServer(engine),
		Cfg:    cfg,
	}, nil
}

// ensureTestDatabase 用 Go 代码建库。
//
// 为什么不直接调 createdb：本机（scoop 装的 PostgreSQL）的 bin 目录不在 PATH 上，
// 测试不该依赖调用方的 shell 环境。而 pgx 本来就连着库，建库也就一句 SQL 的事。
func ensureTestDatabase(ctx context.Context, dsn string) error {
	// 先连到 DSN 里写的那个库（通常是 postgres 维护库），因为 CREATE DATABASE
	// 不能连着「正要被创建的那个库」执行
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("连不上 TEST_DB_DSN 指向的维护库（docker compose up -d 起了吗？）: %w", err)
	}
	defer pool.Close()

	var exists bool
	err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)`, testDBName).Scan(&exists)
	if err != nil {
		return fmt.Errorf("查询 %s 是否存在时失败: %w", testDBName, err)
	}
	if exists {
		return nil
	}

	// CREATE DATABASE 不能用占位符传库名（它是标识符不是值），只能拼字符串。
	// testDBName 是本文件里的常量、不来自任何外部输入，所以这里没有注入风险。
	// 不加 ENCODING / TEMPLATE 选项，让它继承和开发库 lostfound 完全一样的编码与排序规则。
	if _, err := pool.Exec(ctx, `CREATE DATABASE `+testDBName); err != nil {
		return fmt.Errorf("创建测试库 %s 失败: %w", testDBName, err)
	}
	return nil
}

// ---------- Harness ----------

// Harness 是一次集成测试的完整环境：真实测试库 + 真实路由 + 真实 HTTP。
type Harness struct {
	Pool   *pgxpool.Pool
	Server *httptest.Server
	Cfg    config.Config
}

func (h *Harness) Close() {
	h.Server.Close()
	h.Pool.Close()
	// 临时上传目录是我们自己建的，自己删干净 —— 否则每跑一次测试
	// 就在系统临时目录里留一个装满假图片的文件夹。
	if h.Cfg.UploadDir != "" {
		_ = os.RemoveAll(h.Cfg.UploadDir)
	}
}

// TruncateAll 清空全部业务表并重置自增 id，让每个测试从干净状态开始、测试之间零串扰。
//
// ⚠ 这里是 10 张表，不是 12 张 —— categories 和 locations 故意不列。
// 它们装的是迁移种进去的 55 + 91 行字典数据，truncate 掉之后所有测试都建不出帖子
// （items.category_id / location_id 外键找不到目标）。别「补全」这两张表。
//
// 反过来，业务表少列一张就会串味：像 TestEveryAdminWriteIsLogged 那种
// 「断言行数恰好 +N」的测试会被上一个用例留下的行污染。
func (h *Harness) TruncateAll(t *testing.T) {
	t.Helper()
	const q = `TRUNCATE users, items, item_images, contact_views, item_returns,
	                   credit_logs, notifications, match_pairs, reports, admin_actions
	           RESTART IDENTITY CASCADE`
	if _, err := h.Pool.Exec(context.Background(), q); err != nil {
		t.Fatalf("TruncateAll 失败（表名和 §10 的清单对不上？加了新表却忘了往这里加？）: %v", err)
	}
}

// QueryRow 是测试里直接查库的快捷方式 —— 很多断言必须落到数据行上，
// 光看 HTTP 响应是不够的（例如「reject 之后 items.status 仍然是 open」）。
func (h *Harness) QueryRow(t *testing.T, sql string, args ...any) map[string]any {
	t.Helper()
	rows, err := h.Pool.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("查询失败 %q: %v", sql, err)
	}
	defer rows.Close()

	fields := rows.FieldDescriptions()
	if !rows.Next() {
		t.Fatalf("查询没有返回任何行 %q（args=%v）", sql, args)
	}
	raw, err := rows.Values()
	if err != nil {
		t.Fatalf("读取行失败 %q: %v", sql, err)
	}

	out := make(map[string]any, len(fields))
	for i, f := range fields {
		out[string(f.Name)] = raw[i]
	}
	return out
}

// Count 返回一条 SELECT count(*) 的结果。
func (h *Harness) Count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := h.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("计数查询失败 %q: %v", sql, err)
	}
	return n
}

// ---------- HTTP 调用 ----------

// Response 是 §8 统一信封解码后的结果。
type Response struct {
	HTTPStatus int
	Code       string
	Message    string
	RequestID  string
	Data       json.RawMessage

	// Header 留着给需要检查响应头的测试用（例如 X-Request-ID 是否回写）
	Header http.Header
}

// DataInto 把 data 字段解进 v。
func (r Response) DataInto(t *testing.T, v any) {
	t.Helper()
	if len(r.Data) == 0 || string(r.Data) == "null" {
		t.Fatalf("响应的 data 是空的，无法解进 %T；code=%s message=%s", v, r.Code, r.Message)
	}
	if err := json.Unmarshal(r.Data, v); err != nil {
		t.Fatalf("解析 data 失败: %v\n原始 data: %s", err, string(r.Data))
	}
}

// Do 发一个 JSON 请求并解信封。body 为 nil 时不带请求体；token 非空时加 Authorization 头。
//
// 真正的发送/收信/解信封都在 doRaw 里 —— PostMultipart 也走它。
// 拆开的理由和 bindJSON/bindJSONOptional 一样：「一个响应长什么样」只能有一份定义，
// 两份的话其中一份改了另一份不会跟着改，而测试照样全绿。
func (h *Harness) Do(t *testing.T, method, path string, body any, token string) Response {
	t.Helper()

	var reader io.Reader
	var contentType string
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化请求体失败: %v", err)
		}
		reader = bytes.NewReader(raw)
		contentType = "application/json"
	}
	return h.doRaw(t, method, path, reader, contentType, token, true)
}

// Get / Post 是两个最常用的快捷方式。
func (h *Harness) Get(t *testing.T, path, token string) Response {
	t.Helper()
	return h.Do(t, http.MethodGet, path, nil, token)
}

func (h *Harness) Post(t *testing.T, path string, body any, token string) Response {
	t.Helper()
	return h.Do(t, http.MethodPost, path, body, token)
}

// RequireCode 断言响应码，失败时把整条响应打出来。
//
// 只比 code 不比 message：message 是给人看的中文，改措辞不该让测试红（§8）。
func RequireCode(t *testing.T, r Response, want string) {
	t.Helper()
	if r.Code != want {
		t.Fatalf("期望 code=%s，实际 code=%s（HTTP %d）\nmessage: %s\nrequest_id: %s\ndata: %s",
			want, r.Code, r.HTTPStatus, r.Message, r.RequestID, truncate(string(r.Data)))
	}
}

// RequireOK 是 RequireCode 的常用特例。
func RequireOK(t *testing.T, r Response, what string) {
	t.Helper()
	if r.Code != apperr.CodeOK {
		t.Fatalf("%s 失败：code=%s message=%s（HTTP %d）\nrequest_id: %s\ndata: %s",
			what, r.Code, r.Message, r.HTTPStatus, r.RequestID, truncate(string(r.Data)))
	}
}

// ---------- 登录态夹具 ----------

// Session 是一个「已登录用户」。M2 之后几乎每条测试都要先有登录态，
// 所以它和 RegisterAndLogin 放在这个共享文件里，而不是塞进 m1 的测试文件。
type Session struct {
	Token    string
	UserID   int64
	Username string
	Nickname string
	Role     string
}

// RegisterAndLogin 走完 #1 注册 + #2 登录，返回一个可用的登录态。
//
// 它刻意**不断言**这两个端点的契约（那是 m1_auth_test.go 的职责），
// 只在出问题时 fatal 并打出完整响应。作为夹具，它的任务是「给我一个能用的 token」；
// 把断言混进夹具的后果是：注册接口一坏，几十条本来在测别的东西的测试全部变红，
// 而红的那一条真正的原因被埋在第一个失败里。
func (h *Harness) RegisterAndLogin(t *testing.T, username, password string) Session {
	t.Helper()

	RequireOK(t, h.Post(t, "/api/auth/register", map[string]any{
		"username": username,
		"password": password,
	}, ""), "夹具注册 "+username)

	r := h.Post(t, "/api/auth/login", map[string]any{
		"username": username,
		"password": password,
	}, "")
	RequireOK(t, r, "夹具登录 "+username)

	var data struct {
		Token string `json:"token"`
		User  struct {
			ID       int64  `json:"id"`
			Username string `json:"username"`
			Nickname string `json:"nickname"`
			Role     string `json:"role"`
		} `json:"user"`
	}
	r.DataInto(t, &data)
	if data.Token == "" {
		t.Fatalf("夹具登录 %s 成功了，但 token 是空的", username)
	}
	return Session{
		Token:    data.Token,
		UserID:   data.User.ID,
		Username: data.User.Username,
		Nickname: data.User.Nickname,
		Role:     data.User.Role,
	}
}

// TokenFor 给任意 user id 铸一个有效 token，不走 #2 登录接口。
//
// 需要它的场合是「这个用户没法登录，但测试必须让他带着 token 发请求」：
// SSO 账号（没有密码）、已被封禁的账号（登录会被拒）、admin（计划里没有
// 「注册成 admin」的端点）。铸出来的 token 和生产签发的完全同源 ——
// 同一个密钥、同一个 auth.TokenSigner 实现，所以中间件分不出区别，
// 这正是「测的是真链路」的意思。
func (h *Harness) TokenFor(t *testing.T, userID int64) string {
	t.Helper()
	signer, err := auth.NewTokenSigner(h.Cfg.JWTSecret, h.Cfg.JWTExpire)
	if err != nil {
		t.Fatalf("铸 token 失败（建 signer 出错）: %v", err)
	}
	tok, err := signer.Issue(userID)
	if err != nil {
		t.Fatalf("铸 token 失败: %v", err)
	}
	return tok.Value
}

// Ban 把一个用户封禁。
//
// 直接改库而不是走接口：M6 才有封禁端点，而 M1 就必须验证「封号立刻生效」
// （role/status 不进 JWT claims 那个设计决定的验收点）。
func (h *Harness) Ban(t *testing.T, userID int64) {
	t.Helper()
	if _, err := h.Pool.Exec(context.Background(),
		`UPDATE users SET status = 'banned', updated_at = now() WHERE id = $1`, userID); err != nil {
		t.Fatalf("封禁用户 %d 失败: %v", userID, err)
	}
}

// truncate 防止一段长响应把测试输出淹掉。
func truncate(s string) string {
	const max = 800
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("...(截断，共 %d 字节)", len(s))
}

// ---------- 非 JSON 的请求与响应 ----------

// PostMultipart 发一个 multipart/form-data 请求，专门给 #6 上传用。
//
// 不能复用 Do：Do 只会 marshal 成 JSON，而上传的契约（表单字段名必须叫 file、
// Content-Type 必须是 multipart/form-data 带 boundary）恰恰是 JSON 请求表达不出来的。
// 响应仍然是 §8 的信封，所以这里照旧解信封。
func (h *Harness) PostMultipart(t *testing.T, path, field, filename string, content []byte, token string) Response {
	t.Helper()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile(field, filename)
	if err != nil {
		t.Fatalf("构造 multipart 失败: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("写入文件内容失败: %v", err)
	}
	if err := w.Close(); err != nil { // Close 才会写出结尾的 boundary，漏了服务端解析不出来
		t.Fatalf("结束 multipart 失败: %v", err)
	}

	return h.doRaw(t, http.MethodPost, path, &buf, w.FormDataContentType(), token, true)
}

// GetRaw 发一个 GET 并返回**未解析**的响应，给 #40 静态文件用。
//
// #40 是全部 50 个端点里唯一一个不走 JSON 信封的（它直接返回图片二进制），
// 所以它不能用 Do —— Do 解不开信封就会 fatal。
func (h *Harness) GetRaw(t *testing.T, path, token string) (int, []byte, http.Header) {
	t.Helper()
	r := h.doRaw(t, http.MethodGet, path, nil, "", token, false)
	return r.HTTPStatus, r.Data, r.Header
}

// doRaw 是 Do 和 PostMultipart 共用的底层：发请求 → 读响应 → 解信封。
// envelope 为 false 时跳过解信封（调用方自己处理 body）。
func (h *Harness) doRaw(t *testing.T, method, path string, body io.Reader, contentType, token string, envelope bool) Response {
	t.Helper()

	req, err := http.NewRequest(method, h.Server.URL+path, body)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := h.Server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s 请求失败: %v", method, path, err)
	}
	defer resp.Body.Close()

	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("读取响应体失败: %v", err)
	}

	out := Response{
		HTTPStatus: resp.StatusCode,
		Data:       rawBody,
		Header:     resp.Header,
	}
	if !envelope {
		return out
	}

	var env struct {
		Code      string          `json:"code"`
		Message   string          `json:"message"`
		Data      json.RawMessage `json:"data"`
		RequestID string          `json:"request_id"`
	}
	if err := json.Unmarshal(rawBody, &env); err != nil {
		t.Fatalf("%s %s 返回的不是统一信封（§8 被破坏了？）: %v\nHTTP %d\n原始响应: %s",
			method, path, err, resp.StatusCode, truncate(string(rawBody)))
	}
	out.Code = env.Code
	out.Message = env.Message
	out.Data = env.Data
	out.RequestID = env.RequestID
	return out
}

// ---------- 改库夹具 ----------

// MakeAdmin 把一个已存在的用户提升为 admin，返回带**新 token** 的登录态。
//
// 计划里没有「注册成 admin」的端点（也不该有），所以只能直接改库。
// 改完之后必须重新铸 token：不是因为 role 在 claims 里 —— 恰恰相反，
// M1 那个设计决定就是「role/status 不进 JWT」，中间件每个请求都回库查一次。
// 这里重铸只是为了让返回值自洽（Role 字段是新的），旧 token 其实照样能用。
func (h *Harness) MakeAdmin(t *testing.T, s Session) Session {
	t.Helper()
	if _, err := h.Pool.Exec(context.Background(),
		`UPDATE users SET role = 'admin', updated_at = now() WHERE id = $1`, s.UserID); err != nil {
		t.Fatalf("提升 admin 失败: %v", err)
	}
	s.Role = "admin"
	s.Token = h.TokenFor(t, s.UserID)
	return s
}

// SetItemStatus 直接把一条帖子改成某个状态。
//
// 用它而不是走 #18：#18 只允许 open/closed，而很多测试需要一条 deleted 的帖子
// 来验「软删之后广场看不见、本人和管理员还能看见」。
func (h *Harness) SetItemStatus(t *testing.T, itemID int64, status string) {
	t.Helper()
	tag, err := h.Pool.Exec(context.Background(),
		`UPDATE items SET status = $2, updated_at = now() WHERE id = $1`, itemID, status)
	if err != nil {
		t.Fatalf("改帖子 %d 的状态失败: %v", itemID, err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("改帖子 %d 的状态影响了 %d 行，期望 1 行", itemID, tag.RowsAffected())
	}
}

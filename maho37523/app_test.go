package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	db     *sql.DB
	repo   *ItemRepository
	items  *ItemService
	media  *MediaService
	ai     *AIService
	router http.Handler
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	d, err := openDatabaseAt(filepath.Join(dir, "test.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	previous := db
	db = d
	t.Cleanup(func() { db = previous; d.Close() })
	for id := 1; id <= 4; id++ {
		if _, err = d.Exec("INSERT INTO users(id,username,password_hash) VALUES(?,?,?)", id, fmt.Sprintf("user%d", id), "test-only-unused-hash"); err != nil {
			t.Fatal(err)
		}
		if _, err = d.Exec("INSERT INTO sessions VALUES(?,?,?)", fmt.Sprintf("test-session-%d", id), id, time.Now().Add(time.Hour).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	media, err := newMediaService(d, filepath.Join(dir, "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	repo := &ItemRepository{db: d}
	items := &ItemService{repo: repo}
	ai := &AIService{db: d, repo: repo, media: media, config: Config{AIModel: "mock", AIDailyLimit: 100, MatchCandidates: 20}}
	router := newRouter(&ItemHandler{service: items}, &MediaHandler{service: media}, &AIHandler{service: ai}, &NotificationHandler{db: d})
	return &fixture{db: d, repo: repo, items: items, media: media, ai: ai, router: securityHeaders(http.NewCrossOriginProtection().Handler(router))}
}
func (f *fixture) request(t *testing.T, method, path string, user int, body any, status int) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if user > 0 {
		req.AddCookie(&http.Cookie{Name: "session", Value: fmt.Sprintf("test-session-%d", user)})
	}
	res := httptest.NewRecorder()
	f.router.ServeHTTP(res, req)
	if res.Code != status {
		t.Fatalf("%s %s: got %d want %d: %s", method, path, res.Code, status, res.Body.String())
	}
	return res
}
func responseData[T any](t *testing.T, r *httptest.ResponseRecorder) T {
	t.Helper()
	var envelope struct {
		Data T `json:"data"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Data
}
func samplePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 10, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 10; x++ {
			img.Set(x, y, color.RGBA{20, 80, 45, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
func (f *fixture) image(t *testing.T, user int) Media {
	t.Helper()
	m, err := f.media.Save(context.Background(), user, bytes.NewReader(samplePNG(t)))
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func input(kind, name string) CreateItemRequest {
	return CreateItemRequest{ItemType: kind, Name: name, Location: "图书馆", Description: "黑色卡套", Tags: []string{}}
}
func (f *fixture) create(t *testing.T, user int, in CreateItemRequest) Item {
	t.Helper()
	i, err := f.items.Create(context.Background(), user, in)
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func TestMigrationPreservesLegacyAndBacksUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE TABLE items(id INTEGER PRIMARY KEY,type TEXT NOT NULL,name TEXT NOT NULL,location TEXT NOT NULL,description TEXT NOT NULL,status TEXT NOT NULL)",
		"INSERT INTO items VALUES(42,'found','校园卡','图书馆','旧数据','searching')",
	} {
		if _, err = legacy.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	legacy.Close()
	d, err := openDatabaseAt(path, true)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	var revision, allow int
	if err = d.QueryRow("SELECT name,revision,allow_ai FROM items WHERE id=42").Scan(&name, &revision, &allow); err != nil {
		t.Fatal(err)
	}
	if name != "校园卡" || revision != 1 || allow != 0 {
		t.Fatal("legacy changed")
	}
	d.Close()
	backups, err := filepath.Glob(path + ".backup-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("backup: %v %v", backups, err)
	}
	backup, err := sql.Open("sqlite", backups[0])
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var n int
	if err = backup.QueryRow("SELECT count(*) FROM items WHERE id=42").Scan(&n); err != nil || n != 1 {
		t.Fatal("backup invalid", err)
	}
	d, err = openDatabaseAt(path, true)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	backups, _ = filepath.Glob(path + ".backup-*")
	if len(backups) != 1 {
		t.Fatal("migration not idempotent")
	}
}
func TestItemImagesContactsAndPermissions(t *testing.T) {
	f := setup(t)
	in := input("found", "校园卡")
	f.request(t, "POST", "/api/items", 0, in, 401)
	f.request(t, "POST", "/api/items", 1, in, 400)
	bad := input("oops", "校园卡")
	f.request(t, "POST", "/api/items", 1, bad, 400)
	m := f.image(t, 1)
	f.request(t, "GET", m.URL, 0, nil, 404)
	f.request(t, "GET", m.URL, 2, nil, 404)
	f.request(t, "GET", m.URL, 1, nil, 200)
	in.ImageIDs = []int{m.ID}
	in.Contact = "private-contact@example.test"
	in.AllowAI = true
	other := in
	other.ImageIDs = []int{f.image(t, 2).ID}
	f.request(t, "POST", "/api/items", 1, other, 400)
	i := responseData[Item](t, f.request(t, "POST", "/api/items", 1, in, 201))
	if len(i.Images) != 1 || i.Revision != 1 {
		t.Fatalf("%+v", i)
	}
	f.request(t, "GET", m.URL, 0, nil, 200)
	list := f.request(t, "GET", "/api/items", 1, nil, 200)
	if strings.Contains(list.Body.String(), in.Contact) {
		t.Fatal("contact leaked in list")
	}
	detail := f.request(t, "GET", fmt.Sprintf("/api/items/%d", i.ID), 0, nil, 200)
	if strings.Contains(detail.Body.String(), in.Contact) {
		t.Fatal("contact leaked to guest")
	}
	authed := responseData[Item](t, f.request(t, "GET", fmt.Sprintf("/api/items/%d", i.ID), 2, nil, 200))
	if authed.Contact != in.Contact {
		t.Fatal("authenticated contact missing")
	}
	f.request(t, "PUT", fmt.Sprintf("/api/items/%d", i.ID), 2, in, 403)
	f.request(t, "DELETE", fmt.Sprintf("/api/items/%d", i.ID), 2, nil, 403)
	f.request(t, "PATCH", fmt.Sprintf("/api/items/%d/status", i.ID), 2, map[string]string{"status": "found"}, 403)
	f.request(t, "POST", "/api/items", 1, in, 400) // A photo cannot belong to two posts.
	in.Name = "更新后的校园卡"
	in.ImageIDs = nil
	updated := responseData[Item](t, f.request(t, "PUT", fmt.Sprintf("/api/items/%d", i.ID), 1, in, 200))
	if len(updated.Images) != 1 || updated.Revision != 2 {
		t.Fatal("images/revision lost")
	}
	in.ImageIDs = []int{}
	f.request(t, "PUT", fmt.Sprintf("/api/items/%d", i.ID), 1, in, 400)
	f.request(t, "PATCH", fmt.Sprintf("/api/items/%d/status", i.ID), 1, map[string]string{"status": "closed"}, 409)
	f.request(t, "PATCH", fmt.Sprintf("/api/items/%d/status", i.ID), 1, map[string]string{"status": "found"}, 200)
	f.request(t, "PATCH", fmt.Sprintf("/api/items/%d/status", i.ID), 1, map[string]string{"status": "closed"}, 200)
	f.request(t, "DELETE", fmt.Sprintf("/api/items/%d", i.ID), 1, nil, 200)
	f.request(t, "GET", fmt.Sprintf("/api/items/%d", i.ID), 0, nil, 404)
	f.request(t, "GET", m.URL, 0, nil, 404)
	var jobs, posts int
	_ = f.db.QueryRow("SELECT count(*) FROM ai_jobs WHERE kind='match'").Scan(&jobs)
	_ = f.db.QueryRow("SELECT count(*) FROM items").Scan(&posts)
	if jobs != 2 || posts != 1 {
		t.Fatalf("transaction rollback failed: jobs=%d posts=%d", jobs, posts)
	}
}
func TestSearchBreadthsPaginationAndLiteralInput(t *testing.T) {
	f := setup(t)
	exact := f.create(t, 1, input("lost", "校园卡"))
	f.create(t, 1, input("lost", "学生卡"))
	f.create(t, 1, input("lost", "银行卡"))
	f.create(t, 1, input("lost", "雨伞"))
	for _, tc := range []struct {
		breadth string
		total   int
	}{{"strict", 1}, {"standard", 2}, {"broad", 3}} {
		list := responseData[ItemListResponse](t, f.request(t, "GET", "/api/items?q=校园卡&breadth="+tc.breadth, 0, nil, 200))
		if list.Total != tc.total || list.Items[0].ID != exact.ID {
			t.Fatalf("%s: %+v", tc.breadth, list)
		}
	}
	f.request(t, "GET", "/api/items?breadth=unknown", 0, nil, 400)
	f.request(t, "GET", "/api/items?page=0", 0, nil, 400)
	f.request(t, "GET", "/api/items?page_size=101", 0, nil, 400)
	f.request(t, "GET", "/api/items?type=unknown", 0, nil, 400)
	list := responseData[ItemListResponse](t, f.request(t, "GET", "/api/items?q=%25&breadth=strict", 0, nil, 200))
	if list.Total != 0 {
		t.Fatal("percent treated as wildcard")
	}
	list = responseData[ItemListResponse](t, f.request(t, "GET", "/api/items?page=2&page_size=2", 0, nil, 200))
	if list.Total != 4 || len(list.Items) != 2 {
		t.Fatal("pagination failed")
	}
	for n := 0; n < 10; n++ {
		terms := expandSearch("校园卡", "broad")
		if fmt.Sprint(terms) != fmt.Sprint(expandSearch("校园卡", "broad")) {
			t.Fatal("nondeterministic expansion")
		}
	}
}
func TestUploadValidationAndStaticSafety(t *testing.T) {
	f := setup(t)
	for _, data := range [][]byte{[]byte("<script>alert(1)</script>"), make([]byte, maxImageBytes+1)} {
		if _, err := f.media.Save(context.Background(), 1, bytes.NewReader(data)); err == nil {
			t.Fatal("invalid image accepted")
		}
	}
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	part, _ := mw.CreateFormFile("image", "misleading.html")
	_, _ = part.Write(samplePNG(t))
	mw.Close()
	req := httptest.NewRequest("POST", "/api/media", &b)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: "session", Value: "test-session-1"})
	res := httptest.NewRecorder()
	f.router.ServeHTTP(res, req)
	if res.Code != 201 {
		t.Fatal(res.Body.String())
	}
	m := responseData[Media](t, res)
	if m.MIME != "image/jpeg" {
		t.Fatal("not reencoded")
	}
	real, err := f.media.Get(context.Background(), m.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(f.media.dir, real.Path))
	if err != nil {
		t.Fatal(err)
	}
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "jpeg" {
		t.Fatal("stored image invalid")
	}
	root := f.request(t, "GET", "/", 0, nil, 200)
	if !strings.Contains(root.Body.String(), "校园失物招领系统") || root.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("frontend/security absent")
	}
	f.request(t, "GET", "/lostfound.db", 0, nil, 404)
	f.request(t, "GET", "/.env", 0, nil, 404)
	f.request(t, "GET", "/api/nonexistent", 0, nil, 404)
	f.request(t, "GET", "/app.js", 0, nil, 200)
	f.request(t, "POST", "/api/items", 1, map[string]string{"description": strings.Repeat("x", 70*1024)}, 400)
	f.request(t, "POST", "/api/ai/extract", 1, map[string]any{"media_id": m.ID, "consent": true}, 503)
}

type mockVision struct {
	calls     int
	extracts  int
	judgement MatchJudgement
	err       error
	after     func()
}

func (m *mockVision) Extract(context.Context, Media) (ItemSuggestion, error) {
	m.extracts++
	return ItemSuggestion{Name: "校园卡", Category: "卡片", Tags: []string{"黑色"}}, m.err
}
func (m *mockVision) Compare(context.Context, Item, Item) (MatchJudgement, error) {
	m.calls++
	if m.after != nil {
		m.after()
	}
	return m.judgement, m.err
}
func enableMock(f *fixture, m *mockVision) {
	f.ai.client = m
	f.ai.config.AIKey = "test-only"
	f.ai.config.AIModel = "test-vision"
}
func matchInput(kind string, ids ...int) CreateItemRequest {
	in := input(kind, "校园卡")
	in.Category = "卡片"
	in.AllowAI = true
	in.ImageIDs = ids
	return in
}
func positiveMock() *mockVision {
	return &mockVision{judgement: MatchJudgement{Possible: true, Score: .9, Reasons: []string{"同为黑色卡套"}, Conflicts: []string{}, Uncertainty: "仍需人工核实"}}
}
func TestMatchingMultipleCandidatesDedupAndNotificationOwnership(t *testing.T) {
	f := setup(t)
	mock := positiveMock()
	enableMock(f, mock)
	lost := f.create(t, 1, matchInput("lost"))
	foundA := f.create(t, 2, matchInput("found", f.image(t, 2).ID))
	f.create(t, 3, matchInput("found", f.image(t, 3).ID))
	f.create(t, 1, matchInput("found", f.image(t, 1).ID))
	noConsent := matchInput("found", f.image(t, 4).ID)
	noConsent.AllowAI = false
	f.create(t, 4, noConsent)
	job := AIJob{Kind: "match", ItemID: lost.ID, Revision: lost.Revision}
	if _, err := f.ai.process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if mock.calls != 2 {
		t.Fatalf("wanted 2 candidates, got %d", mock.calls)
	}
	if _, err := f.ai.process(context.Background(), AIJob{Kind: "match", ItemID: foundA.ID, Revision: foundA.Revision}); err != nil {
		t.Fatal(err)
	}
	if mock.calls != 2 {
		t.Fatal("duplicate comparison")
	}
	type inbox struct {
		Notifications []Notification `json:"notifications"`
		Unread        int            `json:"unread"`
	}
	data := responseData[inbox](t, f.request(t, "GET", "/api/notifications", 1, nil, 200))
	if len(data.Notifications) != 2 || data.Unread != 2 {
		t.Fatalf("%+v", data)
	}
	for _, n := range data.Notifications {
		if !n.Available || n.LostID != lost.ID || !strings.Contains(n.Uncertainty, "无图片") {
			t.Fatalf("%+v", n)
		}
	}
	other := responseData[inbox](t, f.request(t, "GET", "/api/notifications", 2, nil, 200))
	if len(other.Notifications) != 0 {
		t.Fatal("notified finder rather than owner")
	}
	id := data.Notifications[0].ID
	f.request(t, "PATCH", fmt.Sprintf("/api/notifications/%d/read", id), 2, nil, 404)
	f.request(t, "PATCH", fmt.Sprintf("/api/notifications/%d/read", id), 1, nil, 200)
	data = responseData[inbox](t, f.request(t, "GET", "/api/notifications", 1, nil, 200))
	if data.Unread != 1 {
		t.Fatal("read mark failed")
	}
	updated := matchInput("lost")
	updated.Description = "描述已修改"
	_, err := f.items.Update(context.Background(), lost.ID, 1, updated)
	if err != nil {
		t.Fatal(err)
	}
	data = responseData[inbox](t, f.request(t, "GET", "/api/notifications", 1, nil, 200))
	for _, n := range data.Notifications {
		if n.Available {
			t.Fatal("stale match still available")
		}
	}
	before := mock.calls
	if _, err = f.ai.process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if mock.calls != before {
		t.Fatal("stale job called provider")
	}
}
func TestMatchingRejectsConflictsAndChangesDuringCall(t *testing.T) {
	for _, scenario := range []string{"conflict", "closed-during-call", "consent-revoked"} {
		t.Run(scenario, func(t *testing.T) {
			f := setup(t)
			mock := positiveMock()
			enableMock(f, mock)
			lost := f.create(t, 1, matchInput("lost"))
			f.create(t, 2, matchInput("found", f.image(t, 2).ID))
			switch scenario {
			case "conflict":
				mock.judgement.Conflicts = []string{"颜色矛盾"}
			case "closed-during-call":
				mock.after = func() {
					if _, err := f.items.ChangeStatus(context.Background(), lost.ID, 1, "found"); err != nil {
						t.Fatal(err)
					}
				}
			case "consent-revoked":
				mock.after = func() {
					in := matchInput("lost")
					in.AllowAI = false
					if _, err := f.items.Update(context.Background(), lost.ID, 1, in); err != nil {
						t.Fatal(err)
					}
				}
			}
			job := AIJob{Kind: "match", ItemID: lost.ID, Revision: lost.Revision}
			if _, err := f.ai.process(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			var count int
			_ = f.db.QueryRow("SELECT count(*) FROM notifications").Scan(&count)
			if count != 0 {
				t.Fatal("invalid notification emitted")
			}
			if scenario == "conflict" {
				if _, err := f.ai.process(context.Background(), job); err != nil {
					t.Fatal(err)
				}
				if mock.calls != 1 {
					t.Fatal("negative match not cached")
				}
			}
		})
	}
}
func TestDurableJobsConsentLeaseAndBudget(t *testing.T) {
	f := setup(t)
	mock := positiveMock()
	enableMock(f, mock)
	m := f.image(t, 1)
	f.request(t, "POST", "/api/ai/extract", 1, map[string]any{"media_id": m.ID}, 400)
	f.request(t, "POST", "/api/ai/extract", 2, map[string]any{"media_id": m.ID, "consent": true}, 400)
	j := responseData[AIJob](t, f.request(t, "POST", "/api/ai/extract", 1, map[string]any{"media_id": m.ID, "consent": true}, 202))
	again, err := f.ai.queueExtract(context.Background(), 1, m.ID)
	if err != nil || again.ID != j.ID {
		t.Fatal("extract not deduplicated", err)
	}
	f.request(t, "GET", fmt.Sprintf("/api/ai/jobs/%d", j.ID), 2, nil, 404)
	claimed, err := f.ai.claim(context.Background())
	if err != nil || claimed.ID != j.ID || claimed.Attempts != 1 {
		t.Fatal("claim failed", err)
	}
	if _, err = f.ai.claim(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("running job claimed twice", err)
	}
	result, err := f.ai.process(context.Background(), claimed)
	if err != nil || !strings.Contains(result, "校园卡") {
		t.Fatal("mock extraction failed", err)
	}
	_, _ = f.db.Exec("UPDATE ai_jobs SET lease_until=0 WHERE id=?", j.ID)
	reclaimed, err := f.ai.claim(context.Background())
	if err != nil || reclaimed.Attempts != 2 {
		t.Fatal("lease recovery failed", err)
	}
	_, _ = f.db.Exec("UPDATE ai_jobs SET attempts=3,lease_until=0 WHERE id=?", j.ID)
	if _, err = f.ai.claim(context.Background()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("too many attempts", err)
	}
	terminal := responseData[struct {
		Job AIJob `json:"job"`
	}](t, f.request(t, "GET", fmt.Sprintf("/api/ai/jobs/%d", j.ID), 1, nil, 200))
	if terminal.Job.Status != "failed" {
		t.Fatal("not terminal")
	}
	f.ai.config.AIDailyLimit = 2
	if err = f.ai.budget(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = f.ai.budget(context.Background()); !errors.Is(err, ErrAIBudget) {
		t.Fatal("daily limit bypassed", err)
	}
	var calls int
	_ = f.db.QueryRow("SELECT calls FROM ai_usage").Scan(&calls)
	if calls != 2 {
		t.Fatal("overcharged budget")
	}
}
func TestDeepSeekVisionHTTPContractNoContact(t *testing.T) {
	f := setup(t)
	m := f.image(t, 1)
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer test-only" {
			t.Error("invalid request")
		}
		raw, _ := io.ReadAll(r.Body)
		if !bytes.Contains(raw, []byte("data:image/jpeg;base64,")) {
			t.Error("vision image missing")
		}
		if !bytes.Contains(raw, []byte(`"thinking":{"type":"disabled"}`)) {
			t.Error("thinking mode not explicitly bounded")
		}
		if bytes.Contains(raw, []byte("never-send-this-contact")) {
			t.Error("contact sent to provider")
		}
		if calls == 1 {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"name\":\"校园卡 123456789\",\"category\":\"卡片\",\"tags\":[\"黑色\"]}"}}]}`)
		} else {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"possible\":true,\"score\":0.9,\"reasons\":[\"外观相近\"],\"conflicts\":[],\"uncertainty\":\"需核实\"}"}}]}`)
		}
	}))
	defer server.Close()
	c, err := newDeepSeekClient(Config{AIKey: "test-only", AIBaseURL: server.URL, AIModel: "deepseek-flash"}, f.media)
	if err != nil {
		t.Fatal(err)
	}
	suggestion, err := c.Extract(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(suggestion.Name, "123456789") {
		t.Fatal("identifier not redacted")
	}
	lost := Item{Name: "校园卡", Contact: "never-send-this-contact"}
	found := lost
	found.Images = []Media{m}
	judgement, err := c.Compare(context.Background(), lost, found)
	if err != nil || !judgement.Possible {
		t.Fatal("compare failed", err)
	}
	if calls != 2 {
		t.Fatal("unexpected calls")
	}
}

func TestAuthenticationCookieAndCrossOriginProtection(t *testing.T) {
	f := setup(t)
	credentials := map[string]string{"username": "new-test-user", "password": "test-password-only"}
	f.request(t, "POST", "/api/register", 0, credentials, 200)
	f.request(t, "POST", "/api/register", 0, credentials, 409)
	f.request(t, "POST", "/api/login", 0, map[string]string{"username": "new-test-user", "password": "wrong"}, 401)
	login := f.request(t, "POST", "/api/login", 0, credentials, 200)
	cookies := login.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal("session cookie unsafe")
	}
	request := func(method, path string, cookie *http.Cookie, status int) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		res := httptest.NewRecorder()
		f.router.ServeHTTP(res, req)
		if res.Code != status {
			t.Fatalf("%s got %d: %s", path, res.Code, res.Body.String())
		}
		return res
	}
	me := responseData[User](t, request("GET", "/api/me", cookies[0], 200))
	if me.Username != "new-test-user" {
		t.Fatal("wrong authenticated user")
	}
	csrf := httptest.NewRequest("POST", "/api/logout", nil)
	csrf.AddCookie(cookies[0])
	csrf.Header.Set("Sec-Fetch-Site", "cross-site")
	csrf.Header.Set("Origin", "https://example.test")
	rejected := httptest.NewRecorder()
	f.router.ServeHTTP(rejected, csrf)
	if rejected.Code != 403 {
		t.Fatal("cross-site mutation allowed")
	}
	request("POST", "/api/logout", cookies[0], 200)
	request("GET", "/api/me", cookies[0], 401)
	t.Setenv("APP_COOKIE_SECURE", "true")
	secured := f.request(t, "POST", "/api/login", 0, credentials, 200)
	if !secured.Result().Cookies()[0].Secure {
		t.Fatal("production Secure flag missing")
	}
}

func TestWorkerCompletesPersistedExtractJob(t *testing.T) {
	f := setup(t)
	mock := positiveMock()
	enableMock(f, mock)
	m := f.image(t, 1)
	j, err := f.ai.queueExtract(context.Background(), 1, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.ai.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		job, e := scanJob(f.db.QueryRow("SELECT "+jobColumns+" FROM ai_jobs WHERE id=?", j.ID))
		if e != nil {
			t.Fatal(e)
		}
		if job.Status == "succeeded" {
			if job.Attempts != 1 || !strings.Contains(job.Result, "校园卡") {
				t.Fatal("worker result not durable")
			}
			return
		}
		if job.Status == "failed" {
			t.Fatal(job.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("worker did not finish queued task")
}

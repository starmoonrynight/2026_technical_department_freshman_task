package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// These are human-authored test fixtures, NOT recognition results from DeepSeek.
// Similar product photos are possible leads; they do not establish physical identity.
var providedCases = []struct{ file, name, category string }{
	{"1.jpg", "银色充电宝", "电子产品"},
	{"2.jpg", "白色有线鼠标", "电子产品"},
	{"4.jpg", "白色 OPPO 耳机盒", "电子产品"},
	{"5.jpg", "待辨认的黑色长条物品", "其他"},
	{"23.png", "OPPO 无线耳机和充电盒", "电子产品"},
	{"231.jpg", "银灰色充电宝", "电子产品"},
	{"4341.jpg", "银灰色充电宝", "电子产品"},
	{"43951.jpg", "笔记本电脑", "电子产品"},
}

type caseUploadResult struct {
	File             string `json:"file"`
	OriginalBytes    int    `json:"original_bytes"`
	OriginalWidth    int    `json:"original_width"`
	OriginalHeight   int    `json:"original_height"`
	MediaID          int    `json:"media_id"`
	StoredWidth      int    `json:"stored_width"`
	StoredHeight     int    `json:"stored_height"`
	StoredMIME       string `json:"stored_mime"`
	UploadHTTPStatus int    `json:"upload_http_status"`
}
type caseTestReport struct {
	Mode             string             `json:"mode"`
	RealModelCalled  bool               `json:"real_model_called"`
	Uploads          []caseUploadResult `json:"uploads"`
	MockExtractTasks int                `json:"mock_extract_tasks"`
	MockComparisons  int                `json:"mock_comparisons"`
	Notifications    int                `json:"notifications"`
	Note             string             `json:"note"`
}
type caseVision struct {
	mediaNames         map[int]string
	suggestions        map[int]ItemSuggestion
	extracts, compares int
}

func (c *caseVision) Extract(_ context.Context, m Media) (ItemSuggestion, error) {
	c.extracts++
	return c.suggestions[m.ID], nil
}
func (c *caseVision) Compare(_ context.Context, lost, found Item) (MatchJudgement, error) {
	c.compares++
	a, b := c.mediaNames[lost.Images[0].ID], c.mediaNames[found.Images[0].ID]
	possible := (a == "1.jpg" && (b == "231.jpg" || b == "4341.jpg")) || (a == "23.png" && b == "4.jpg")
	if possible {
		return MatchJudgement{Possible: true, Score: .9, Reasons: []string{"人工设置的模拟相似候选"}, Conflicts: []string{}, Uncertainty: "仅验证软件链路；不是模型识别，也未确认同一件实物"}, nil
	}
	return MatchJudgement{Possible: false, Score: .1, Reasons: []string{}, Conflicts: []string{"人工设置的模拟非匹配候选"}, Uncertainty: "模拟对照"}, nil
}
func providedCaseDir(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("CASE_IMAGE_DIR")
	if dir == "" {
		t.Skip("set CASE_IMAGE_DIR to test the user's external image fixtures")
	}
	return dir
}
func uploadProvidedCase(t *testing.T, f *fixture, dir, name string) (Media, caseUploadResult) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	part, err := mw.CreateFormFile("image", name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err = mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/media", &b)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: "session", Value: "test-session-1"})
	res := httptest.NewRecorder()
	f.router.ServeHTTP(res, req)
	if res.Code != 201 {
		t.Fatalf("%s upload failed: %d %s", name, res.Code, res.Body.String())
	}
	m := responseData[Media](t, res)
	if m.Width > 1600 || m.Height > 1600 || m.MIME != "image/jpeg" {
		t.Fatalf("%s unexpected processed image: %+v", name, m)
	}
	f.request(t, "GET", m.URL, 0, nil, 404)
	f.request(t, "GET", m.URL, 2, nil, 404)
	f.request(t, "GET", m.URL, 1, nil, 200)
	result := caseUploadResult{File: name, OriginalBytes: len(raw), OriginalWidth: cfg.Width, OriginalHeight: cfg.Height, MediaID: m.ID, StoredWidth: m.Width, StoredHeight: m.Height, StoredMIME: m.MIME, UploadHTTPStatus: res.Code}
	t.Logf("%s: %d bytes, %dx%d -> %dx%d JPEG; upload 201; draft owner-only", name, len(raw), cfg.Width, cfg.Height, m.Width, m.Height)
	return m, result
}
func TestProvidedCaseImagesLocalPipeline(t *testing.T) {
	dir := providedCaseDir(t)
	f := setup(t)
	report := caseTestReport{Mode: "local-upload-and-deterministic-mock", RealModelCalled: false, Uploads: []caseUploadResult{}, Note: "Labels and matching decisions are manually assigned fixtures. This test cannot establish recognition accuracy or physical item identity."}
	mock := &caseVision{mediaNames: map[int]string{}, suggestions: map[int]ItemSuggestion{}}
	enableMock(f, positiveMock())
	f.ai.client = mock
	media := map[string]Media{}
	for _, c := range providedCases {
		m, result := uploadProvidedCase(t, f, dir, c.file)
		media[c.file] = m
		report.Uploads = append(report.Uploads, result)
		mock.mediaNames[m.ID] = c.file
		mock.suggestions[m.ID] = ItemSuggestion{Name: c.name, Category: c.category, Tags: []string{"模拟测试"}}
		j := responseData[AIJob](t, f.request(t, "POST", "/api/ai/extract", 1, map[string]any{"media_id": m.ID, "consent": true}, 202))
		resultJSON, err := f.ai.process(context.Background(), j)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.db.Exec("UPDATE ai_jobs SET status='succeeded',result=? WHERE id=?", resultJSON, j.ID); err != nil {
			t.Fatal(err)
		}
		response := responseData[struct {
			Job    AIJob          `json:"job"`
			Result ItemSuggestion `json:"result"`
		}](t, f.request(t, "GET", fmt.Sprintf("/api/ai/jobs/%d", j.ID), 1, nil, 200))
		if response.Job.Status != "succeeded" || response.Result.Name != c.name {
			t.Fatal("mock draft job failed")
		}
	}
	// Different users publish found posts. Own photographs are never borrowed via another user.
	for _, name := range []string{"2.jpg", "4.jpg", "5.jpg", "231.jpg", "4341.jpg", "43951.jpg"} {
		source, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		m, err := f.media.Save(context.Background(), 2, source)
		source.Close()
		if err != nil {
			t.Fatal(err)
		}
		mock.mediaNames[m.ID] = name
		suggestion := mock.suggestions[media[name].ID]
		in := input("found", suggestion.Name)
		in.Description = "人为设定的本地测试资料"
		in.Category = suggestion.Category
		in.ImageIDs = []int{m.ID}
		in.AllowAI = true
		f.create(t, 2, in)
	}
	for _, name := range []string{"1.jpg", "23.png"} {
		suggestion := mock.suggestions[media[name].ID]
		in := input("lost", suggestion.Name)
		in.Description = "人为设定的本地测试资料"
		in.Category = suggestion.Category
		in.AllowAI = true
		in.ImageIDs = []int{media[name].ID}
		lost := f.create(t, 1, in)
		j := AIJob{Kind: "match", ItemID: lost.ID, Revision: lost.Revision}
		if _, err := f.ai.process(context.Background(), j); err != nil {
			t.Fatal(err)
		}
		before := mock.compares
		if _, err := f.ai.process(context.Background(), j); err != nil {
			t.Fatal(err)
		}
		if mock.compares != before {
			t.Fatal("repeated matching called mock again")
		}
	}
	inbox := responseData[struct {
		Notifications []Notification `json:"notifications"`
		Unread        int            `json:"unread"`
	}](t, f.request(t, "GET", "/api/notifications", 1, nil, 200))
	if len(inbox.Notifications) != 3 || inbox.Unread != 3 {
		t.Fatalf("expected three controlled mock notifications, got %+v", inbox)
	}
	for _, n := range inbox.Notifications {
		if !n.Available {
			t.Fatal("mock notification unavailable")
		}
		f.request(t, "GET", fmt.Sprintf("/api/items/%d", n.FoundID), 1, nil, 200)
	}
	report.MockExtractTasks = mock.extracts
	report.MockComparisons = mock.compares
	report.Notifications = len(inbox.Notifications)
	t.Logf("mock extraction=%d, comparisons=%d, notifications=%d; real model was NOT called", mock.extracts, mock.compares, len(inbox.Notifications))
	if output := os.Getenv("CASE_REPORT_PATH"); output != "" {
		// Only a caller-selected new file; never overwrite a previous report.
		out, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			t.Fatal(err)
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		err = enc.Encode(report)
		closeErr := out.Close()
		if err != nil {
			t.Fatal(err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}
}

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type liveExtractionResult struct {
	File       string         `json:"file"`
	Suggestion ItemSuggestion `json:"suggestion"`
	Error      string         `json:"error,omitempty"`
}
type livePairResult struct {
	LostFile            string         `json:"lost_file"`
	FoundFile           string         `json:"found_file"`
	Control             string         `json:"control"`
	Judgement           MatchJudgement `json:"judgement"`
	NotificationCreated bool           `json:"notification_created"`
	Error               string         `json:"error,omitempty"`
}
type liveCaseReport struct {
	Mode        string                 `json:"mode"`
	StartedAt   string                 `json:"started_at"`
	Model       string                 `json:"configured_model"`
	APIRequests int                    `json:"api_request_attempts"`
	Extractions []liveExtractionResult `json:"extractions"`
	Pairs       []livePairResult       `json:"pairs"`
	Note        string                 `json:"note"`
}

// Only the opt-in live test reads this file. The application still does NOT
// auto-load .env. Parse data, never execute shell commands from an env file.
func liveConfig(t *testing.T) Config {
	t.Helper()
	if os.Getenv("RUN_LIVE_AI_TESTS") != "1" {
		t.Skip("live AI tests require explicit RUN_LIVE_AI_TESTS=1")
	}
	c, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("LIVE_AI_ENV_FILE")
	if path == "" {
		path = ".env"
	}
	file, err := os.Open(path)
	if err == nil {
		defer file.Close()
		scan := bufio.NewScanner(file)
		for scan.Scan() {
			line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(scan.Text()), "export "))
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			value = strings.TrimSpace(value)
			if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
				value = value[1 : len(value)-1]
			}
			switch key {
			case "DEEPSEEK_API_KEY":
				if c.AIKey == "" {
					c.AIKey = value
				}
			case "DEEPSEEK_BASE_URL":
				if os.Getenv(key) == "" && value != "" {
					c.AIBaseURL = value
				}
			case "DEEPSEEK_MODEL":
				if os.Getenv(key) == "" && value != "" {
					c.AIModel = value
				}
			}
		}
		if scan.Err() != nil {
			t.Fatal("cannot read local AI configuration")
		}
	} else if !os.IsNotExist(err) {
		t.Fatal("cannot open local AI configuration")
	}
	if c.AIKey == "" {
		t.Fatal("no DeepSeek key: configure DEEPSEEK_API_KEY locally; do not paste it into chat")
	}
	u, e := url.Parse(c.AIBaseURL)
	if e != nil || u.Scheme != "https" || u.Hostname() != "api.deepseek.com" {
		t.Fatal("live image test only sends credentials/photos to the official DeepSeek HTTPS endpoint")
	}
	// Explicit bounded batch, isolated daily budget; no automatic retries.
	c.AIDailyLimit = 14
	c.MatchCandidates = 20
	return c
}
func TestProvidedCaseLiveAI(t *testing.T) {
	c := liveConfig(t)
	dir := providedCaseDir(t)
	f := setup(t)
	client, err := newDeepSeekClient(c, f.media)
	if err != nil {
		t.Fatal(err)
	}
	f.ai.config = c
	f.ai.client = client
	report := liveCaseReport{Mode: "live-provider-extraction-and-six-controlled-pairs", StartedAt: time.Now().Format(time.RFC3339), Model: c.AIModel, Extractions: []liveExtractionResult{}, Pairs: []livePairResult{}, Note: "At most 14 calls, no retry, temporary database. Pair labels describe visual hypotheses, not verified ownership. This test directly invokes the provider/service and notification transaction; it is not a full browser or exhaustive automatic-candidate-recall evaluation."}
	output := os.Getenv("LIVE_CASE_REPORT_PATH")
	if output == "" {
		t.Fatal("set LIVE_CASE_REPORT_PATH to a NEW result file")
	}
	if _, e := os.Stat(output); e == nil {
		t.Fatal("result file already exists; refusing to overwrite")
	} else if !os.IsNotExist(e) {
		t.Fatal(e)
	}
	defer func() {
		out, e := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			t.Error(e)
			return
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if e = enc.Encode(report); e != nil {
			t.Error(e)
		}
		if e = out.Close(); e != nil {
			t.Error(e)
		}
	}()
	media := map[string]Media{}
	suggestions := map[string]ItemSuggestion{}
	for _, sample := range providedCases {
		source, e := os.Open(filepath.Join(dir, sample.file))
		if e != nil {
			t.Fatal(e)
		}
		m, e := f.media.Save(context.Background(), 1, source)
		source.Close()
		if e != nil {
			t.Fatal(e)
		}
		media[sample.file] = m
		if e = f.ai.budget(context.Background()); e != nil {
			t.Fatal(e)
		}
		report.APIRequests++
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
		s, e := client.Extract(ctx, m)
		cancel()
		result := liveExtractionResult{File: sample.file, Suggestion: s}
		if e != nil {
			result.Error = e.Error()
			t.Errorf("%s extraction: %s", sample.file, e)
		} else {
			suggestions[sample.file] = s
			t.Logf("%s -> name=%q category=%q color=%q brand=%q", sample.file, s.Name, s.Category, s.Color, s.Brand)
		}
		report.Extractions = append(report.Extractions, result)
		if e != nil && (strings.Contains(e.Error(), "HTTP 401") || strings.Contains(e.Error(), "HTTP 402") || strings.Contains(e.Error(), "HTTP 403")) {
			t.Fatal("authentication/balance/access failed; stopped remaining paid calls")
		}
	}
	items := map[string]Item{}
	for _, sample := range providedCases {
		s, ok := suggestions[sample.file]
		if !ok {
			continue
		}
		owner, kind := 2, "found"
		if sample.file == "1.jpg" || sample.file == "23.png" {
			owner, kind = 1, "lost"
		}
		m := media[sample.file]
		if owner == 2 {
			source, e := os.Open(filepath.Join(dir, sample.file))
			if e != nil {
				t.Fatal(e)
			}
			m, e = f.media.Save(context.Background(), owner, source)
			source.Close()
			if e != nil {
				t.Fatal(e)
			}
		}
		// Empty names remain a model limitation; a manual placeholder allows testing
		// the pair without claiming the model filled a name it did not produce.
		name := s.Name
		if name == "" {
			name = "待确认物品（人工占位）"
		}
		in := CreateItemRequest{ItemType: kind, Name: name, Location: "测试用虚构地点", Description: s.Description, Category: s.Category, Color: s.Color, Brand: s.Brand, Tags: s.Tags, ImageIDs: []int{m.ID}, AllowAI: true}
		i, e := f.items.Create(context.Background(), owner, in)
		if e != nil {
			t.Errorf("%s model draft cannot publish: %s", sample.file, e)
			continue
		}
		i.Images = []Media{m}
		items[sample.file] = i
	}
	for _, pair := range []struct{ a, b, control string }{
		{"1.jpg", "231.jpg", "visual-candidate-unverified"},
		{"1.jpg", "4341.jpg", "visual-candidate-unverified"},
		{"23.png", "4.jpg", "visual-candidate-unverified"},
		{"1.jpg", "2.jpg", "different-object-category"},
		{"1.jpg", "43951.jpg", "different-object-category"},
		{"23.png", "2.jpg", "different-object-category"},
	} {
		lost, lostOK := items[pair.a]
		found, foundOK := items[pair.b]
		result := livePairResult{LostFile: pair.a, FoundFile: pair.b, Control: pair.control}
		if !lostOK || !foundOK {
			result.Error = "识图/草稿失败，此组未调用模型"
			report.Pairs = append(report.Pairs, result)
			continue
		}
		if e := f.ai.budget(context.Background()); e != nil {
			t.Fatal(e)
		}
		report.APIRequests++
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
		j, e := client.Compare(ctx, lost, found)
		cancel()
		result.Judgement = j
		if e != nil {
			result.Error = e.Error()
			t.Errorf("%s / %s compare: %s", pair.a, pair.b, e)
		} else {
			qualifies := j.Possible && j.Score >= .8 && len(j.Reasons) > 0 && len(j.Conflicts) == 0
			if qualifies {
				result.NotificationCreated, e = f.ai.saveMatch(context.Background(), lost, found, j)
				if e != nil {
					result.Error = e.Error()
					t.Error(e)
				}
			}
			if pair.control == "different-object-category" && qualifies {
				t.Errorf("false-positive control: %s / %s would notify", pair.a, pair.b)
			}
			t.Logf("%s / %s: possible=%t score=%.3f conflicts=%d notify=%t", pair.a, pair.b, j.Possible, j.Score, len(j.Conflicts), result.NotificationCreated)
		}
		report.Pairs = append(report.Pairs, result)
	}
	t.Logf("live requests attempted=%d; report saved without key", report.APIRequests)
}

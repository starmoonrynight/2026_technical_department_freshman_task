package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type AIService struct {
	db     *sql.DB
	repo   *ItemRepository
	media  *MediaService
	client VisionClient
	config Config
}
type AIHandler struct{ service *AIService }

const jobColumns = "id,kind,user_id,COALESCE(item_id,0),COALESCE(media_id,0),revision,status,attempts,result,error"

func scanJob(row scanner) (AIJob, error) {
	var j AIJob
	err := row.Scan(&j.ID, &j.Kind, &j.UserID, &j.ItemID, &j.MediaID, &j.Revision, &j.Status, &j.Attempts, &j.Result, &j.Error)
	return j, err
}
func (s *AIService) enabled() bool { return s.config.AIKey != "" && s.client != nil }
func (s *AIService) queueExtract(ctx context.Context, user, mediaID int) (AIJob, error) {
	if !s.enabled() {
		return AIJob{}, ErrAIDisabled
	}
	m, err := s.media.Get(ctx, mediaID)
	if err != nil || m.UserID != user {
		return AIJob{}, ErrInvalidMedia
	}
	key := fmt.Sprintf("extract:%d", mediaID)
	now := time.Now().Unix()
	// Re-use a successful/in-progress draft; a new upload can retry a terminal failure.
	j, err := scanJob(s.db.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM ai_jobs WHERE dedup_key=?", key))
	if err == nil {
		return j, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return AIJob{}, err
	}
	var count int
	if err = s.db.QueryRowContext(ctx, "SELECT count(*) FROM ai_jobs WHERE user_id=? AND kind='extract' AND created_at>?", user, time.Now().Add(-time.Hour).Unix()).Scan(&count); err != nil {
		return AIJob{}, err
	}
	if count >= 20 {
		return AIJob{}, rateLimitError("每小时最多识图 20 次")
	}
	_, err = s.db.ExecContext(ctx, "INSERT OR IGNORE INTO ai_jobs(kind,user_id,media_id,dedup_key,run_after,created_at) SELECT 'extract',?,?,?,?,? WHERE (SELECT count(*) FROM ai_jobs WHERE user_id=? AND kind='extract' AND created_at>?)<20", user, mediaID, key, now, now, user, now-3600)
	if err != nil {
		return AIJob{}, err
	}
	j, err = scanJob(s.db.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM ai_jobs WHERE dedup_key=?", key))
	if errors.Is(err, sql.ErrNoRows) {
		return AIJob{}, rateLimitError("每小时最多识图 20 次")
	}
	return j, err
}
func (h *AIHandler) Config(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		methodNotAllowed(w, "GET")
		return
	}
	writeJSON(w, 200, Response{Code: 0, Message: "功能配置", Data: map[string]any{"ai_enabled": h.service.enabled(), "ai_model": h.service.config.AIModel, "image_max_bytes": maxImageBytes, "max_images": 3, "search_breadths": []string{"strict", "standard", "broad"}}})
}
func (h *AIHandler) Extract(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		methodNotAllowed(w, "POST")
		return
	}
	u, ok := requireLogin(w, r)
	if !ok {
		return
	}
	var in struct {
		MediaID int  `json:"media_id"`
		Consent bool `json:"consent"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !in.Consent {
		writeJSON(w, 400, Response{Code: 400, Message: "请明确同意将图片发送给 AI 服务，并提供 media_id"})
		return
	}
	j, err := h.service.queueExtract(r.Context(), u.ID, in.MediaID)
	if errors.Is(err, ErrAIDisabled) {
		writeJSON(w, 503, Response{Code: 503, Message: err.Error()})
		return
	}
	if err != nil {
		writeItemError(w, err)
		return
	}
	writeJSON(w, 202, Response{Code: 0, Message: "识图任务已提交，结果需要你确认", Data: j})
}
func (h *AIHandler) Job(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		methodNotAllowed(w, "GET")
		return
	}
	u, ok := requireLogin(w, r)
	if !ok {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 1 {
		writeItemError(w, ErrItemNotFound)
		return
	}
	j, err := scanJob(h.service.db.QueryRowContext(r.Context(), "SELECT "+jobColumns+" FROM ai_jobs WHERE id=? AND user_id=?", id, u.ID))
	if errors.Is(err, sql.ErrNoRows) {
		writeItemError(w, ErrItemNotFound)
		return
	}
	if err != nil {
		writeItemError(w, err)
		return
	}
	var result any
	if j.Result != "" {
		_ = json.Unmarshal([]byte(j.Result), &result)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, Response{Code: 0, Message: "任务状态", Data: map[string]any{"job": j, "result": result, "ai_enabled": h.service.enabled()}})
}
func (s *AIService) claim(ctx context.Context) (AIJob, error) {
	now := time.Now().Unix()
	// A crashed worker has a finite lease. Third failed/crashed attempts are terminal.
	if _, err := s.db.ExecContext(ctx, "UPDATE ai_jobs SET status='failed',error='任务重试次数已用完' WHERE status='running' AND lease_until<? AND attempts>=3", now); err != nil {
		return AIJob{}, err
	}
	return scanJob(s.db.QueryRowContext(ctx, "UPDATE ai_jobs SET status='running',attempts=attempts+1,lease_until=? WHERE id=(SELECT id FROM ai_jobs WHERE ((status='pending' AND run_after<=?) OR (status='running' AND lease_until<?)) AND attempts<3 ORDER BY CASE kind WHEN 'extract' THEN 0 ELSE 1 END,id LIMIT 1) RETURNING "+jobColumns, now+300, now, now))
}
func (s *AIService) budget(ctx context.Context) error {
	day := time.Now().UTC().Format("2006-01-02")
	var calls int
	err := s.db.QueryRowContext(ctx, "INSERT INTO ai_usage(day,calls) VALUES(?,1) ON CONFLICT(day) DO UPDATE SET calls=calls+1 WHERE calls<? RETURNING calls", day, s.config.AIDailyLimit).Scan(&calls)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrAIBudget
	}
	return err
}
func (s *AIService) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !s.enabled() {
				continue
			}
			j, err := s.claim(ctx)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				continue
			}
			taskCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			result, err := s.process(taskCtx, j)
			cancel()
			if ctx.Err() != nil {
				return
			} // Leave the lease for restart recovery.
			status, message := "succeeded", ""
			runAfter := time.Now().Unix()
			attempts := j.Attempts
			if err != nil {
				status = "pending"
				message = err.Error()
				runAfter += int64(5 * (1 << j.Attempts))
				if j.Attempts >= 3 {
					status = "failed"
				}
				if errors.Is(err, ErrAIBudget) {
					status = "pending"
					attempts--
					runAfter = time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour).Unix()
				}
			}
			_, _ = s.db.ExecContext(ctx, "UPDATE ai_jobs SET status=?,result=?,error=?,run_after=?,lease_until=0,attempts=? WHERE id=? AND status='running'", status, result, message, runAfter, attempts, j.ID)
		}
	}
}
func (s *AIService) process(ctx context.Context, j AIJob) (string, error) {
	if j.Kind == "extract" {
		m, err := s.media.Get(ctx, j.MediaID)
		if err != nil {
			return "", errors.New("识图图片不可用")
		}
		if err = s.budget(ctx); err != nil {
			return "", err
		}
		result, err := s.client.Extract(ctx, m)
		if err != nil {
			return "", err
		}
		raw, _ := json.Marshal(result)
		return string(raw), nil
	}
	if j.Kind != "match" {
		return "", errors.New("未知任务类型")
	}
	origin, ok, err := s.repo.GetByID(ctx, j.ItemID)
	if err != nil {
		return "", err
	}
	if !ok || origin.Revision != j.Revision || origin.Status != "searching" || !origin.AllowAI {
		return `{"skipped":true}`, nil
	}
	opposite := "found"
	if origin.ItemType == "found" {
		opposite = "lost"
	}
	// Structured category is used for broad recall; an image-derived draft supplies it naturally.
	keyword := origin.Category
	if keyword == "" {
		keyword = origin.Name
	}
	if keyword == "" {
		keyword = strings.Join(origin.Tags, " ")
	}
	candidates, _, err := s.repo.Search(ctx, SearchQuery{Keyword: keyword, Breadth: "broad", ItemType: opposite, Status: "searching", Page: 1, PageSize: s.config.MatchCandidates, ForMatching: true, ExcludeUserID: origin.UserID})
	if err != nil {
		return "", err
	}
	compared, matched := 0, 0
	for _, candidate := range candidates {
		if compared >= s.config.MatchCandidates {
			break
		}
		if !candidate.AllowAI || candidate.UserID == origin.UserID {
			continue
		}
		current, active, e := s.repo.GetByID(ctx, origin.ID)
		if e != nil {
			return "", e
		}
		if !active || current.Revision != origin.Revision || current.Status != "searching" || !current.AllowAI {
			break
		}
		candidate, active, e = s.repo.GetByID(ctx, candidate.ID)
		if e != nil {
			return "", e
		}
		if !active || !candidate.AllowAI || candidate.Status != "searching" {
			continue
		}
		lost, found := origin, candidate
		if origin.ItemType == "found" {
			lost, found = candidate, origin
		}
		if len(found.Images) == 0 {
			continue
		}
		var existing int
		if e = s.db.QueryRowContext(ctx, "SELECT count(*) FROM match_reviews WHERE lost_id=? AND found_id=? AND lost_revision=? AND found_revision=?", lost.ID, found.ID, lost.Revision, found.Revision).Scan(&existing); e != nil {
			return "", e
		}
		if existing > 0 {
			continue
		}
		// Load private filesystem metadata only for the provider, never into JSON responses.
		for _, item := range []*Item{&lost, &found} {
			for n := range item.Images {
				m, e := s.media.Get(ctx, item.Images[n].ID)
				if e != nil {
					return "", e
				}
				item.Images[n] = m
			}
		}
		if e = s.budget(ctx); e != nil {
			return "", e
		}
		judgement, e := s.client.Compare(ctx, lost, found)
		if e != nil {
			return "", e
		}
		compared++
		if !judgement.Possible || judgement.Score < 0.8 || len(judgement.Reasons) == 0 || len(judgement.Conflicts) > 0 {
			raw, _ := json.Marshal(judgement)
			if _, e = s.db.ExecContext(ctx, "INSERT OR IGNORE INTO match_reviews VALUES(?,?,?,?,?,?)", lost.ID, found.ID, lost.Revision, found.Revision, string(raw), time.Now().Unix()); e != nil {
				return "", e
			}
			continue
		}
		if len(lost.Images) == 0 {
			judgement.Uncertainty = "丢失帖无图片，本结果仅依据文字与找到帖图片，需人工核实。 " + judgement.Uncertainty
		}
		added, e := s.saveMatch(ctx, lost, found, judgement)
		if e != nil {
			return "", e
		}
		if added {
			matched++
		}
	}
	raw, _ := json.Marshal(map[string]any{"compared": compared, "matched": matched})
	return string(raw), nil
}
func (s *AIService) saveMatch(ctx context.Context, lost, found Item, j MatchJudgement) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var active int
	err = tx.QueryRowContext(ctx, "SELECT count(*) FROM items WHERE deleted_at=0 AND status='searching' AND allow_ai=1 AND ((id=? AND revision=? AND type='lost' AND user_id=?) OR (id=? AND revision=? AND type='found' AND user_id=?))", lost.ID, lost.Revision, lost.UserID, found.ID, found.Revision, found.UserID).Scan(&active)
	if err != nil {
		return false, err
	}
	if active != 2 || lost.UserID == found.UserID {
		return false, nil
	}
	reasons, _ := json.Marshal(j.Reasons)
	conflicts, _ := json.Marshal(j.Conflicts)
	now := time.Now().Unix()
	var matchID int
	err = tx.QueryRowContext(ctx, "INSERT OR IGNORE INTO matches(lost_id,found_id,lost_revision,found_revision,score,reasons,conflicts,uncertainty,model,created_at) VALUES(?,?,?,?,?,?,?,?,?,?) RETURNING id", lost.ID, found.ID, lost.Revision, found.Revision, j.Score, string(reasons), string(conflicts), j.Uncertainty, s.config.AIModel, now).Scan(&matchID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO notifications(user_id,match_id,created_at) VALUES(?,?,?)", lost.UserID, matchID, now); err != nil {
		return false, err
	}
	raw, _ := json.Marshal(j)
	if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO match_reviews VALUES(?,?,?,?,?,?)", lost.ID, found.ID, lost.Revision, found.Revision, string(raw), now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

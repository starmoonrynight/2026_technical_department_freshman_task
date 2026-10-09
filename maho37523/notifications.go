package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"
)

type NotificationHandler struct{ db *sql.DB }

func (h *NotificationHandler) List(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		methodNotAllowed(w, "GET")
		return
	}
	u, ok := requireLogin(w, r)
	if !ok {
		return
	}
	rows, err := h.db.QueryContext(r.Context(), `SELECT n.id,m.lost_id,m.found_id,f.name,m.score,m.reasons,m.conflicts,m.uncertainty,n.read_at,n.created_at,
 CASE WHEN l.deleted_at=0 AND f.deleted_at=0 AND l.status='searching' AND f.status='searching' AND l.allow_ai=1 AND f.allow_ai=1 AND l.revision=m.lost_revision AND f.revision=m.found_revision THEN 1 ELSE 0 END
 FROM notifications n JOIN matches m ON m.id=n.match_id JOIN items l ON l.id=m.lost_id JOIN items f ON f.id=m.found_id WHERE n.user_id=? ORDER BY n.id DESC LIMIT 100`, u.ID)
	if err != nil {
		writeItemError(w, err)
		return
	}
	out := []Notification{}
	for rows.Next() {
		var n Notification
		var reasons, conflicts string
		if err = rows.Scan(&n.ID, &n.LostID, &n.FoundID, &n.FoundName, &n.Score, &reasons, &conflicts, &n.Uncertainty, &n.ReadAt, &n.CreatedAt, &n.Available); err != nil {
			rows.Close()
			writeItemError(w, err)
			return
		}
		_ = json.Unmarshal([]byte(reasons), &n.Reasons)
		_ = json.Unmarshal([]byte(conflicts), &n.Conflicts)
		out = append(out, n)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		writeItemError(w, err)
		return
	}
	var unread int
	if err = h.db.QueryRowContext(r.Context(), "SELECT count(*) FROM notifications WHERE user_id=? AND read_at=0", u.ID).Scan(&unread); err != nil {
		writeItemError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, Response{Code: 0, Message: "可能的物品匹配（请人工核实）", Data: map[string]any{"notifications": out, "unread": unread}})
}
func (h *NotificationHandler) Read(w http.ResponseWriter, r *http.Request) {
	if r.Method != "PATCH" {
		methodNotAllowed(w, "PATCH")
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
	var updated int
	err = h.db.QueryRowContext(r.Context(), "UPDATE notifications SET read_at=CASE WHEN read_at=0 THEN ? ELSE read_at END WHERE id=? AND user_id=? RETURNING id", time.Now().Unix(), id, u.ID).Scan(&updated)
	if errors.Is(err, sql.ErrNoRows) {
		writeItemError(w, ErrItemNotFound)
		return
	}
	if err != nil {
		writeItemError(w, err)
		return
	}
	writeJSON(w, 200, Response{Code: 0, Message: "已标记为已读"})
}

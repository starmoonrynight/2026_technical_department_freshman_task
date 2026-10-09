package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"lostfound/internal/database"
)

// SessionRow 是 sessions 表的一行。
type SessionRow struct {
	ID        string
	UserID    int64
	CreatedAt string
	ExpiresAt string
}

// CreateSession 生成随机会话 ID 并落库。
func (s *Store) CreateSession(userID int64, ttl time.Duration) (*SessionRow, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}

	id := hex.EncodeToString(buf)
	created := time.Now().UTC()
	expires := created.Add(ttl)

	_, err := s.DB.Exec(
		`INSERT INTO sessions (id, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		id, userID, formatISO(created), formatISO(expires),
	)
	if err != nil {
		return nil, wrap(err)
	}

	return &SessionRow{ID: id, UserID: userID, CreatedAt: formatISO(created), ExpiresAt: formatISO(expires)}, nil
}

// GetSession 取出有效会话；已过期则顺手删除并返回 nil。
func (s *Store) GetSession(id string) (*SessionRow, error) {
	if id == "" {
		return nil, nil
	}

	var row SessionRow
	err := s.DB.QueryRow(
		`SELECT id, user_id, created_at, expires_at FROM sessions WHERE id = ?`, id,
	).Scan(&row.ID, &row.UserID, &row.CreatedAt, &row.ExpiresAt)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, wrap(err)
	}

	expires, perr := time.Parse(time.RFC3339Nano, row.ExpiresAt)
	if perr != nil || !expires.After(time.Now()) {
		_ = s.DestroySession(id)
		return nil, nil
	}
	return &row, nil
}

// DestroySession 删除单个会话。
func (s *Store) DestroySession(id string) error {
	if id == "" {
		return nil
	}
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	return wrap(err)
}

// DestroyUserSessions 踢掉某用户的全部会话。
func (s *Store) DestroyUserSessions(userID int64) error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE user_id = ?`, userID)
	return wrap(err)
}

// CleanupExpired 清理过期会话，返回删除条数。
func (s *Store) CleanupExpired() (int64, error) {
	res, err := s.DB.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, database.Now())
	if err != nil {
		return 0, wrap(err)
	}
	n, err := res.RowsAffected()
	return n, wrap(err)
}

func formatISO(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

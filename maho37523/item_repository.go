package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type ItemRepository struct{ db *sql.DB }

const itemColumns = "id,type,name,location,description,status,user_id,category,color,brand,tags_json,contact,occurred_at,created_at,updated_at,revision,allow_ai"

type scanner interface{ Scan(...any) error }

func scanItem(row scanner) (Item, error) {
	var i Item
	var tags string
	err := row.Scan(&i.ID, &i.ItemType, &i.Name, &i.Location, &i.Description, &i.Status, &i.UserID, &i.Category, &i.Color, &i.Brand, &tags, &i.Contact, &i.OccurredAt, &i.CreatedAt, &i.UpdatedAt, &i.Revision, &i.AllowAI)
	if err != nil {
		return i, err
	}
	i.Tags = []string{}
	i.Images = []Media{}
	if err = json.Unmarshal([]byte(tags), &i.Tags); err != nil {
		return i, err
	}
	return i, nil
}
func (repo *ItemRepository) images(ctx context.Context, id int) ([]Media, error) {
	rows, err := repo.db.QueryContext(ctx, "SELECT m.id,m.mime,m.width,m.height,m.created_at FROM item_images x JOIN media m ON m.id=x.media_id WHERE x.item_id=? ORDER BY x.position", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Media{}
	for rows.Next() {
		var m Media
		if err = rows.Scan(&m.ID, &m.MIME, &m.Width, &m.Height, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.URL = fmt.Sprintf("/api/media/%d/content", m.ID)
		out = append(out, m)
	}
	return out, rows.Err()
}
func (repo *ItemRepository) GetByID(ctx context.Context, id int) (Item, bool, error) {
	i, err := scanItem(repo.db.QueryRowContext(ctx, "SELECT "+itemColumns+" FROM items WHERE id=? AND deleted_at=0", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, false, nil
	}
	if err != nil {
		return i, false, err
	}
	i.Images, err = repo.images(ctx, id)
	return i, true, err
}
func (repo *ItemRepository) GetOwnerID(ctx context.Context, id int) (int, bool, error) {
	var owner int
	err := repo.db.QueryRowContext(ctx, "SELECT user_id FROM items WHERE id=? AND deleted_at=0", id).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return owner, err == nil, err
}

// Images, the post and its durable job commit together, or none of them do.
func attachImages(ctx context.Context, tx *sql.Tx, id, owner int, ids []int) error {
	for pos, mediaID := range ids {
		var count int
		err := tx.QueryRowContext(ctx, "SELECT count(*) FROM media m WHERE m.id=? AND m.user_id=? AND NOT EXISTS(SELECT 1 FROM item_images x WHERE x.media_id=m.id)", mediaID, owner).Scan(&count)
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrInvalidMedia
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO item_images VALUES(?,?,?)", id, mediaID, pos); err != nil {
			return err
		}
	}
	return nil
}
func enqueueMatch(ctx context.Context, tx *sql.Tx, i Item) error {
	if !i.AllowAI || i.Status != "searching" {
		return nil
	}
	now := time.Now().Unix()
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM ai_jobs WHERE kind='match' AND user_id=? AND created_at>?", i.UserID, now-3600).Scan(&count); err != nil {
		return err
	}
	if count >= 20 {
		return rateLimitError("每小时最多触发 20 次自动匹配，请稍后再试或暂时关闭 AI 匹配")
	}
	_, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO ai_jobs(kind,user_id,item_id,revision,dedup_key,run_after,created_at) VALUES('match',?,?,?,?,?,?)", i.UserID, i.ID, i.Revision, fmt.Sprintf("match:%d:%d", i.ID, i.Revision), now, now)
	return err
}
func (repo *ItemRepository) Create(ctx context.Context, owner int, input CreateItemRequest) (Item, error) {
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		return Item{}, err
	}
	defer tx.Rollback()
	tags, _ := json.Marshal(input.Tags)
	now := time.Now().Unix()
	var count int
	if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM items WHERE user_id=? AND created_at>?", owner, now-3600).Scan(&count); err != nil {
		return Item{}, err
	}
	if count >= 30 {
		return Item{}, rateLimitError("每小时最多发布 30 条线索")
	}
	i, err := scanItem(tx.QueryRowContext(ctx, "INSERT INTO items(type,name,location,description,status,user_id,category,color,brand,tags_json,contact,occurred_at,created_at,updated_at,revision,allow_ai) VALUES(?,?,?,?,'searching',?,?,?,?,?,?,?,?,?,1,?) RETURNING "+itemColumns, input.ItemType, input.Name, input.Location, input.Description, owner, input.Category, input.Color, input.Brand, string(tags), input.Contact, input.OccurredAt, now, now, input.AllowAI))
	if err != nil {
		return Item{}, err
	}
	if err = attachImages(ctx, tx, i.ID, owner, input.ImageIDs); err != nil {
		return Item{}, err
	}
	if err = enqueueMatch(ctx, tx, i); err != nil {
		return Item{}, err
	}
	if err = tx.Commit(); err != nil {
		return Item{}, err
	}
	i.Images, err = repo.images(ctx, i.ID)
	return i, err
}
func (repo *ItemRepository) Update(ctx context.Context, id, owner int, input CreateItemRequest) (Item, bool, error) {
	tx, err := repo.db.BeginTx(ctx, nil)
	if err != nil {
		return Item{}, false, err
	}
	defer tx.Rollback()
	tags, _ := json.Marshal(input.Tags)
	i, err := scanItem(tx.QueryRowContext(ctx, "UPDATE items SET type=?,name=?,location=?,description=?,category=?,color=?,brand=?,tags_json=?,contact=?,occurred_at=?,allow_ai=?,updated_at=?,revision=revision+1 WHERE id=? AND user_id=? AND deleted_at=0 RETURNING "+itemColumns, input.ItemType, input.Name, input.Location, input.Description, input.Category, input.Color, input.Brand, string(tags), input.Contact, input.OccurredAt, input.AllowAI, time.Now().Unix(), id, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, err
	}
	if input.ImageIDs != nil {
		if _, err = tx.ExecContext(ctx, "DELETE FROM item_images WHERE item_id=?", id); err != nil {
			return Item{}, false, err
		}
		if err = attachImages(ctx, tx, id, owner, input.ImageIDs); err != nil {
			return Item{}, false, err
		}
	}
	if i.ItemType == "found" {
		var n int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM item_images WHERE item_id=?", id).Scan(&n); err != nil {
			return Item{}, false, err
		}
		if n == 0 {
			return Item{}, false, validationError("找到帖至少需要一张图片")
		}
	}
	if err = enqueueMatch(ctx, tx, i); err != nil {
		return Item{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return Item{}, false, err
	}
	i.Images, err = repo.images(ctx, id)
	return i, true, err
}
func (repo *ItemRepository) UpdateStatus(ctx context.Context, id, owner int, previous, target string) (Item, bool, error) {
	i, err := scanItem(repo.db.QueryRowContext(ctx, "UPDATE items SET status=?,revision=revision+1,updated_at=? WHERE id=? AND user_id=? AND status=? AND deleted_at=0 RETURNING "+itemColumns, target, time.Now().Unix(), id, owner, previous))
	if errors.Is(err, sql.ErrNoRows) {
		return Item{}, false, nil
	}
	if err != nil {
		return Item{}, false, err
	}
	i.Images, err = repo.images(ctx, id)
	return i, true, err
}
func (repo *ItemRepository) Delete(ctx context.Context, id, owner int) (bool, error) {
	now := time.Now().Unix()
	result, err := repo.db.ExecContext(ctx, "UPDATE items SET deleted_at=?,updated_at=?,revision=revision+1,status='closed' WHERE id=? AND user_id=? AND deleted_at=0", now, now, id, owner)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}
func (repo *ItemRepository) Search(ctx context.Context, q SearchQuery) ([]Item, int, error) {
	terms := expandSearch(q.Keyword, q.Breadth)
	// INSTR uses literal strings: '%' and '_' supplied by users are not SQL wildcards.
	field := "lower(name||' '||location||' '||description||' '||category||' '||color||' '||brand||' '||tags_json)"
	where := "deleted_at=0"
	args := []any{}
	if q.ForMatching {
		where += " AND allow_ai=1 AND user_id<>?"
		args = append(args, q.ExcludeUserID)
	}
	for _, f := range []struct{ column, value string }{{"type", q.ItemType}, {"status", q.Status}} {
		if f.value != "" {
			where += " AND " + f.column + "=?"
			args = append(args, f.value)
		}
	}
	conditions := []string{}
	scores := []string{}
	scoreArgs := []any{}
	for _, t := range terms {
		conditions = append(conditions, "instr("+field+",?)>0")
		args = append(args, t.text)
		scores = append(scores, fmt.Sprintf("CASE WHEN instr(%s,?)>0 THEN %d ELSE 0 END", field, t.weight))
		scoreArgs = append(scoreArgs, t.text)
	}
	if len(conditions) > 0 {
		where += " AND (" + strings.Join(conditions, " OR ") + ")"
	}
	var total int
	if err := repo.db.QueryRowContext(ctx, "SELECT count(*) FROM items WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	order := "id DESC"
	if len(scores) > 0 {
		order = "(" + strings.Join(scores, "+") + ") DESC,id DESC"
	}
	allArgs := append(append([]any{}, args...), scoreArgs...)
	allArgs = append(allArgs, q.PageSize, (q.Page-1)*q.PageSize)
	rows, err := repo.db.QueryContext(ctx, "SELECT "+itemColumns+" FROM items WHERE "+where+" ORDER BY "+order+" LIMIT ? OFFSET ?", allArgs...)
	if err != nil {
		return nil, 0, err
	}
	items := []Item{}
	for rows.Next() {
		i, e := scanItem(rows)
		if e != nil {
			rows.Close()
			return nil, 0, e
		}
		i.Contact = ""
		items = append(items, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	// Release the rows before any nested query: SQLite deliberately has one connection.
	for idx := range items {
		items[idx].Images, err = repo.images(ctx, items[idx].ID)
		if err != nil {
			return nil, 0, err
		}
	}
	return items, total, nil
}

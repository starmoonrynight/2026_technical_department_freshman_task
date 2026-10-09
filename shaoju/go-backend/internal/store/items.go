package store

import (
	"database/sql"
	"errors"
	"math"

	"lostfound/internal/database"
	"lostfound/internal/httpx"
	"lostfound/internal/models"
)

const (
	itemSelectFields = `i.id, i.type, i.title, i.category, i.description, i.location, i.storage_place,
		i.happened_at, i.contact, i.image_url, i.status, i.audit_status, i.audit_remark,
		i.user_id, i.created_at, i.updated_at,
		u.student_id AS owner_student_id, u.name AS owner_name,
		u.username AS owner_username, u.nickname AS owner_nickname`

	itemFrom = `FROM items i JOIN users u ON u.id = i.user_id`
)

// ItemRow 是 items 表 join users 后的一行。
type ItemRow struct {
	ID           int64
	Type         string
	Title        string
	Category     string
	Description  string
	Location     string
	StoragePlace string
	HappenedAt   string
	Contact      string
	ImageURL     string
	Status       string
	AuditStatus  string
	AuditRemark  string
	UserID       int64
	CreatedAt    string
	UpdatedAt    string

	OwnerStudentID string
	OwnerName      string
	OwnerUser      string // 兼容列
	OwnerNick      string // 兼容列
}

// ShapeItem 数据库行 -> 前端使用的驼峰结构。
func ShapeItem(row *ItemRow) *models.Item {
	if row == nil {
		return nil
	}

	studentID := firstNonEmpty(row.OwnerStudentID, row.OwnerUser)
	name := firstNonEmpty(row.OwnerName, row.OwnerNick)

	return &models.Item{
		ID:           row.ID,
		Type:         row.Type,
		Title:        row.Title,
		Category:     row.Category,
		Description:  row.Description,
		Location:     row.Location,
		StoragePlace: row.StoragePlace,
		HappenedAt:   row.HappenedAt,
		Contact:      row.Contact,
		ImageURL:     row.ImageURL,
		Status:       row.Status,
		AuditStatus:  row.AuditStatus,
		AuditRemark:  row.AuditRemark,
		UserID:       row.UserID,
		Owner: models.Owner{
			ID:        row.UserID,
			StudentID: studentID,
			Name:      name,
			Username:  studentID,
			Nickname:  name,
		},
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}

func scanItem(row interface{ Scan(...any) error }) (*ItemRow, error) {
	var it ItemRow
	err := row.Scan(
		&it.ID, &it.Type, &it.Title, &it.Category, &it.Description, &it.Location, &it.StoragePlace, &it.HappenedAt,
		&it.Contact, &it.ImageURL, &it.Status, &it.AuditStatus, &it.AuditRemark,
		&it.UserID, &it.CreatedAt, &it.UpdatedAt,
		&it.OwnerStudentID, &it.OwnerName, &it.OwnerUser, &it.OwnerNick,
	)
	if err != nil {
		return nil, err
	}
	return &it, nil
}

// FindItemByID 按主键查询。
func (s *Store) FindItemByID(id int64) (*ItemRow, error) {
	row, err := scanItem(s.DB.QueryRow(`SELECT `+itemSelectFields+` `+itemFrom+` WHERE i.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return row, wrap(err)
}

// ListOptions 列表 / 搜索的查询条件，指针字段为 nil 表示不过滤。
type ListOptions struct {
	Type        *string
	Category    *string
	Keyword     *string
	Status      *string
	AuditStatus *string
	UserID      *int64
	PublicOnly  bool

	// Sort 排序字段（itemSortColumns 的 key），Order 取 asc / desc。
	// 两者都会经过白名单校验，非法值退回缺省排序。
	Sort  string
	Order string

	Page     int
	PageSize int
}

// ListItems 分页查询，返回结构与 Node 版 items.list 一致。
//
// 完整 SQL 形态：
//
//	SELECT COUNT(*) FROM items i JOIN users u ON u.id = i.user_id [WHERE …]
//	SELECT <字段>    FROM items i JOIN users u ON u.id = i.user_id [WHERE …] [ORDER BY …] LIMIT ? OFFSET ?
//
// WHERE 与 ORDER BY 的拼装逻辑分别在 buildItemWhere / buildItemOrderBy 里（见 item_query.go）。
func (s *Store) ListItems(o ListOptions) (*models.Page, error) {
	where, args := buildItemWhere(o)
	orderBy := buildItemOrderBy(o)
	page, pageSize, offset := normalizePaging(o.Page, o.PageSize)

	// 先数总数：用来算总页数，前端的分页按钮依赖它。
	var total int64
	if err := s.DB.QueryRow(`SELECT COUNT(*) `+itemFrom+where, args...).Scan(&total); err != nil {
		return nil, wrap(err)
	}

	// 再取当前页。LIMIT / OFFSET 也走占位符，避免手工拼数字。
	rows, err := s.DB.Query(
		`SELECT `+itemSelectFields+` `+itemFrom+where+orderBy+` LIMIT ? OFFSET ?`,
		append(args, pageSize, offset)...,
	)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()

	items := make([]models.Item, 0)
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, wrap(err)
		}
		items = append(items, *ShapeItem(it))
	}
	if err := rows.Err(); err != nil {
		return nil, wrap(err)
	}

	totalPages := int(math.Max(1, math.Ceil(float64(total)/float64(pageSize))))

	return &models.Page{
		Items:      items,
		Page:       page,
		PageSize:   pageSize,
		Total:      total,
		TotalPages: totalPages,
	}, nil
}

// ItemInput 新建 / 修改时接收的字段。
type ItemInput struct {
	Type         string
	Title        string
	Category     string
	Description  string
	Location     string
	StoragePlace string
	HappenedAt   string
	Contact      string
	ImageURL     string
	Status       *string // 仅更新时可能出现
}

// CreateItem 新建条目，默认进入待审核状态。
func (s *Store) CreateItem(userID int64, in ItemInput) (*ItemRow, error) {
	ts := database.Now()
	res, err := s.DB.Exec(`
		INSERT INTO items (type, title, category, description, location, storage_place, happened_at,
		                   contact, image_url, status, audit_status, user_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'open', 'pending', ?, ?, ?)`,
		in.Type, in.Title, in.Category, in.Description, in.Location, in.StoragePlace, in.HappenedAt,
		in.Contact, in.ImageURL, userID, ts, ts,
	)
	if err != nil {
		return nil, wrap(err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, wrap(err)
	}
	return s.FindItemByID(id)
}

// AssertCanModify 判断某用户能否改动该条目：本人或管理员。
func AssertCanModify(row *ItemRow, user *UserRow) error {
	if row == nil {
		return httpx.NotFound("该信息不存在或已被删除")
	}
	if user.Role == "admin" {
		return nil
	}
	if row.UserID != user.ID {
		return httpx.Forbidden("只能修改自己发布的信息")
	}
	return nil
}

// UpdateItem 修改条目；普通用户改完后需要重新审核。
func (s *Store) UpdateItem(id int64, user *UserRow, in ItemInput) (*ItemRow, error) {
	row, err := s.FindItemByID(id)
	if err != nil {
		return nil, err
	}
	if err := AssertCanModify(row, user); err != nil {
		return nil, err
	}

	status := row.Status
	if in.Status != nil {
		status = *in.Status
	}

	if _, err := s.DB.Exec(`
		UPDATE items
		   SET type = ?, title = ?, category = ?, description = ?, location = ?, storage_place = ?,
		       happened_at = ?, contact = ?, image_url = ?, status = ?, updated_at = ?
		 WHERE id = ?`,
		in.Type, in.Title, in.Category, in.Description, in.Location, in.StoragePlace, in.HappenedAt,
		in.Contact, in.ImageURL, status, database.Now(), id,
	); err != nil {
		return nil, wrap(err)
	}

	if user.Role != "admin" {
		if _, err := s.DB.Exec(
			`UPDATE items SET audit_status = ?, audit_remark = ?, updated_at = ? WHERE id = ?`,
			"pending", "", database.Now(), id,
		); err != nil {
			return nil, wrap(err)
		}
	}
	return s.FindItemByID(id)
}

// SetItemStatus 标记已解决 / 重新开放。
func (s *Store) SetItemStatus(id int64, user *UserRow, status string) (*ItemRow, error) {
	row, err := s.FindItemByID(id)
	if err != nil {
		return nil, err
	}
	if err := AssertCanModify(row, user); err != nil {
		return nil, err
	}

	if _, err := s.DB.Exec(`UPDATE items SET status = ?, updated_at = ? WHERE id = ?`, status, database.Now(), id); err != nil {
		return nil, wrap(err)
	}
	return s.FindItemByID(id)
}

// AuditItem 管理员审核：action = approve / reject。
func (s *Store) AuditItem(id int64, action, remark string) (*ItemRow, error) {
	row, err := s.FindItemByID(id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, httpx.NotFound("该信息不存在或已被删除")
	}

	auditStatus := "rejected"
	storedRemark := remark
	if action == "approve" {
		auditStatus = "approved"
		storedRemark = ""
	}

	if _, err := s.DB.Exec(
		`UPDATE items SET audit_status = ?, audit_remark = ?, updated_at = ? WHERE id = ?`,
		auditStatus, storedRemark, database.Now(), id,
	); err != nil {
		return nil, wrap(err)
	}
	return s.FindItemByID(id)
}

// RemoveItem 删除条目（本人或管理员）。
func (s *Store) RemoveItem(id int64, user *UserRow) error {
	row, err := s.FindItemByID(id)
	if err != nil {
		return err
	}
	if err := AssertCanModify(row, user); err != nil {
		return err
	}
	_, err = s.DB.Exec(`DELETE FROM items WHERE id = ?`, id)
	return wrap(err)
}

// ItemStatuses items.status 的合法取值，顺序即前端标签顺序。
//
//	open   = 寻找中（默认）
//	found  = 已找到
//	closed = 已结束
var ItemStatuses = []string{"open", "found", "closed"}

// ItemStats 后台数据概览。
func (s *Store) ItemStats() (*models.Stats, error) {
	byType := map[string]int64{}
	byStatus := map[string]int64{}

	rows, err := s.DB.Query(`SELECT type, COUNT(*) FROM items GROUP BY type`)
	if err != nil {
		return nil, wrap(err)
	}
	for rows.Next() {
		var k string
		var c int64
		if err := rows.Scan(&k, &c); err != nil {
			rows.Close()
			return nil, wrap(err)
		}
		byType[k] = c
	}
	rows.Close()

	rows, err = s.DB.Query(`SELECT status, COUNT(*) FROM items GROUP BY status`)
	if err != nil {
		return nil, wrap(err)
	}
	for rows.Next() {
		var k string
		var c int64
		if err := rows.Scan(&k, &c); err != nil {
			rows.Close()
			return nil, wrap(err)
		}
		byStatus[k] = c
	}
	rows.Close()

	// 按状态输出时固定顺序、缺的补 0，前端可以直接按顺序渲染卡片，
	// 两版后端给出的顺序也完全一致。
	statusCounts := make([]models.StatusCount, 0, len(ItemStatuses))
	for _, status := range ItemStatuses {
		statusCounts = append(statusCounts, models.StatusCount{Status: status, Count: byStatus[status]})
	}

	var total, pending int64
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM items`).Scan(&total); err != nil {
		return nil, wrap(err)
	}
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM items WHERE audit_status = 'pending'`).Scan(&pending); err != nil {
		return nil, wrap(err)
	}

	byCategory := make([]models.CategoryCount, 0)
	rows, err = s.DB.Query(`SELECT category, COUNT(*) AS c FROM items GROUP BY category ORDER BY c DESC`)
	if err != nil {
		return nil, wrap(err)
	}
	for rows.Next() {
		var cc models.CategoryCount
		if err := rows.Scan(&cc.Category, &cc.Count); err != nil {
			rows.Close()
			return nil, wrap(err)
		}
		byCategory = append(byCategory, cc)
	}
	rows.Close()

	// 解决率 = （已找到 + 已结束）/ 总数：两种终态都算「有结论」。
	resolved := byStatus["found"] + byStatus["closed"]
	resolvedRate := 0
	if total > 0 {
		resolvedRate = int(math.Round(float64(resolved) / float64(total) * 100))
	}

	return &models.Stats{
		Total:        total,
		Lost:         byType["lost"],
		Found:        byType["found"],
		Open:         byStatus["open"],
		Closed:       byStatus["closed"],
		Pending:      pending,
		ResolvedRate: resolvedRate,
		ByCategory:   byCategory,
		ByStatus:     statusCounts,
	}, nil
}

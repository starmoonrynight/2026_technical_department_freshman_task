package store

import (
	"database/sql"
	"errors"

	"lostfound/internal/database"
	"lostfound/internal/httpx"
	"lostfound/internal/models"
	"lostfound/internal/secure"
)

// UserRow 是 users 表的一行。
//
// StudentID / Name 是正式字段；Username / Nickname 是兼容列
// （与 Node 版后端共用数据库时，两边都要写，见 database/schema.sql 的说明）。
type UserRow struct {
	ID           int64
	StudentID    string
	PasswordHash string
	Name         string
	Contact      string
	Role         string
	Status       string
	CreatedAt    string

	Username string // 兼容列，等于 StudentID
	Nickname string // 兼容列，等于 Name

	ItemCount *int64
}

const userColumns = `
	u.id, u.student_id, u.password_hash, u.name, u.contact, u.role, u.status, u.created_at,
	u.username, u.nickname`

// ToPublic 去掉口令散列，转为可直接返回前端的结构。
func ToPublic(row *UserRow) *models.User {
	if row == nil {
		return nil
	}

	// Node 版写入的历史数据可能只有 username / nickname。
	studentID := firstNonEmpty(row.StudentID, row.Username)
	name := firstNonEmpty(row.Name, row.Nickname)

	return &models.User{
		ID:        row.ID,
		StudentID: studentID,
		Name:      name,
		Contact:   row.Contact,
		Role:      row.Role,
		Status:    row.Status,
		CreatedAt: row.CreatedAt,
		ItemCount: row.ItemCount,
		Username:  studentID,
		Nickname:  name,
	}
}

func scanUser(row interface{ Scan(...any) error }) (*UserRow, error) {
	var u UserRow
	err := row.Scan(
		&u.ID, &u.StudentID, &u.PasswordHash, &u.Name, &u.Contact, &u.Role, &u.Status, &u.CreatedAt,
		&u.Username, &u.Nickname,
	)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// FindUserByID 按主键查询。
func (s *Store) FindUserByID(id int64) (*UserRow, error) {
	row, err := scanUser(s.DB.QueryRow(`SELECT `+userColumns+` FROM users u WHERE u.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return row, wrap(err)
}

// FindUserByStudentID 按学号查询。
// 如果学号列为空（历史数据由 Node 版写入），退回用兼容列 username 匹配一次。
func (s *Store) FindUserByStudentID(studentID string) (*UserRow, error) {
	if studentID == "" {
		return nil, nil
	}

	row, err := scanUser(s.DB.QueryRow(`SELECT `+userColumns+` FROM users u WHERE u.student_id = ?`, studentID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, wrap(err)
	}
	if row != nil {
		return row, nil
	}

	row, err = scanUser(s.DB.QueryRow(
		`SELECT `+userColumns+` FROM users u WHERE u.student_id = '' AND u.username = ?`, studentID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return row, wrap(err)
}

// CreateUser 创建用户；学号重复时返回 409。
// student_id / name 与兼容列 username / nickname 一次性写入相同取值。
func (s *Store) CreateUser(studentID, password, name, contact, role string) (*UserRow, error) {
	existing, err := s.FindUserByStudentID(studentID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, httpx.Conflict("该学号已被注册")
	}

	if name == "" {
		name = studentID
	}

	res, err := s.DB.Exec(
		`INSERT INTO users (student_id, password_hash, name, contact, role, status, created_at, username, nickname)
		 VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?)`,
		studentID, secure.Hash(password), name, contact, role, database.Now(), studentID, name,
	)
	if err != nil {
		return nil, wrap(err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, wrap(err)
	}
	return s.FindUserByID(id)
}

// Register 注册普通用户。
func (s *Store) Register(studentID, password, name, contact string) (*UserRow, error) {
	return s.CreateUser(studentID, password, name, contact, "user")
}

// Authenticate 校验学号与口令，失败时返回对应的业务异常。
func (s *Store) Authenticate(studentID, password string) (*UserRow, error) {
	user, err := s.FindUserByStudentID(studentID)
	if err != nil {
		return nil, err
	}
	if user == nil || !secure.Verify(password, user.PasswordHash) {
		return nil, httpx.Unauthorized("学号或密码错误")
	}
	if user.Status != "active" {
		return nil, httpx.Forbidden("该账号已被停用，请联系管理员")
	}
	return user, nil
}

// UpdateProfile 修改姓名与联系方式，同时同步兼容列。
func (s *Store) UpdateProfile(userID int64, name, contact string) (*UserRow, error) {
	if _, err := s.DB.Exec(
		`UPDATE users SET name = ?, nickname = ?, contact = ? WHERE id = ?`, name, name, contact, userID,
	); err != nil {
		return nil, wrap(err)
	}
	return s.FindUserByID(userID)
}

// ChangePassword 校验原口令后写入新口令。
func (s *Store) ChangePassword(userID int64, oldPassword, newPassword string) error {
	user, err := s.FindUserByID(userID)
	if err != nil {
		return err
	}
	if user == nil {
		return httpx.Unauthorized("")
	}
	if !secure.Verify(oldPassword, user.PasswordHash) {
		return httpx.Unauthorized("原密码不正确")
	}
	_, err = s.DB.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, secure.Hash(newPassword), userID)
	return wrap(err)
}

// countActiveAdmins 统计可用的管理员数量。
func (s *Store) countActiveAdmins() (int64, error) {
	var c int64
	err := s.DB.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin' AND status = 'active'`).Scan(&c)
	return c, wrap(err)
}

// UpdateUserStatus 启用 / 停用账号；不允许停用最后一个可用管理员。
func (s *Store) UpdateUserStatus(userID int64, status string) (*UserRow, error) {
	user, err := s.FindUserByID(userID)
	if err != nil || user == nil {
		return nil, err
	}

	if status == "disabled" && user.Role == "admin" {
		count, err := s.countActiveAdmins()
		if err != nil {
			return nil, err
		}
		if count <= 1 {
			return nil, httpx.Forbidden("至少需要保留一个可用的管理员账号")
		}
	}

	if _, err := s.DB.Exec(`UPDATE users SET status = ? WHERE id = ?`, status, userID); err != nil {
		return nil, wrap(err)
	}
	return s.FindUserByID(userID)
}

// UpdateUserRole 调整角色；不允许把最后一个可用管理员降级。
func (s *Store) UpdateUserRole(userID int64, role string) (*UserRow, error) {
	user, err := s.FindUserByID(userID)
	if err != nil || user == nil {
		return nil, err
	}

	if user.Role == "admin" && role != "admin" {
		count, err := s.countActiveAdmins()
		if err != nil {
			return nil, err
		}
		if count <= 1 {
			return nil, httpx.Forbidden("至少需要保留一个可用的管理员账号")
		}
	}

	if _, err := s.DB.Exec(`UPDATE users SET role = ? WHERE id = ?`, role, userID); err != nil {
		return nil, wrap(err)
	}
	return s.FindUserByID(userID)
}

// ListUsers 后台用户列表，附带每人发布条数。
func (s *Store) ListUsers() ([]models.User, error) {
	rows, err := s.DB.Query(`
		SELECT ` + userColumns + `, (SELECT COUNT(*) FROM items i WHERE i.user_id = u.id) AS item_count
		FROM users u
		ORDER BY u.id ASC`)
	if err != nil {
		return nil, wrap(err)
	}
	defer rows.Close()

	list := make([]models.User, 0)
	for rows.Next() {
		var u UserRow
		var itemCount int64
		if err := rows.Scan(
			&u.ID, &u.StudentID, &u.PasswordHash, &u.Name, &u.Contact, &u.Role, &u.Status, &u.CreatedAt,
			&u.Username, &u.Nickname, &itemCount,
		); err != nil {
			return nil, wrap(err)
		}
		u.ItemCount = &itemCount
		list = append(list, *ToPublic(&u))
	}
	return list, wrap(rows.Err())
}

// EnsureActiveAdminRole 保证默认账号是管理员。
func (s *Store) EnsureActiveAdminRole(userID int64) error {
	_, err := s.DB.Exec(`UPDATE users SET role = 'admin' WHERE id = ?`, userID)
	return wrap(err)
}

// BackfillIdentity 把只有兼容列的历史行补成学号 / 姓名。
// 由 Node 版后端写入的用户会落在这里。
func (s *Store) BackfillIdentity() (int64, error) {
	res, err := s.DB.Exec(`UPDATE users SET student_id = username WHERE student_id = ''`)
	if err != nil {
		return 0, wrap(err)
	}
	studentFixed, err := res.RowsAffected()
	if err != nil {
		return 0, wrap(err)
	}

	if _, err := s.DB.Exec(`UPDATE users SET name = nickname WHERE name = ''`); err != nil {
		return studentFixed, wrap(err)
	}
	return studentFixed, nil
}

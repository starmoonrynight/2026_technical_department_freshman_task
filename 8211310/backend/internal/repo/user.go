// Package repo 是唯一允许出现 SQL 的一层（计划 §6 分层规则）。
//
// 约定：一个函数 ≈ 一条 SQL。不 import gin，不写业务 if ——
// 「密码错三次要不要锁」这种规则住在 service，这里只负责把行取出来、写进去。
package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
)

// userCols 是 users 表的取列清单。
//
// 抽成一个常量而不是每条 SQL 各写一遍：SELECT 的列顺序必须和 scanUser 里的
// Scan 参数顺序严格一致，写三遍就意味着改一处要记得改三处 —— 而漏改的症状是
// 「扫描类型不匹配」或者更糟的「两个同为 string 的列悄悄对调」，编译期一声不响。
const userCols = `id, username, password_hash, auth_source, sso_user_id, real_name,
	student_id, avatar_url, nickname, role, status, phone, email, credit_score,
	created_at, updated_at`

// User 是 users 表的数据访问对象。
type User struct {
	pool *pgxpool.Pool
}

// NewUser 造一个 User repo。依赖在 main.go 里手工 new（§6：不用 wire/fx）。
func NewUser(pool *pgxpool.Pool) *User { return &User{pool: pool} }

// NewLocalUser 是注册时要写入的字段。
//
// 刻意只包含本地注册需要的三样，不预留 SSO 参数：M8 接杭电助手时会加一个
// 兄弟方法（CreateFromSSO），而不是把这里撑成一堆永远传 nil 的可选字段。
// 预留用不上的参数等于让每个调用点都要回答「这个我是不是该填」。
type NewLocalUser struct {
	Username     string
	PasswordHash string // bcrypt 哈希，永不是明文
	Nickname     string
}

// CreateLocal 插入一个本地账号并返回完整的一行。
//
// 用户名撞车（部分唯一索引 uq_users_username）转成 USER_ALREADY_EXISTS。
// 这件事在 repo 做而不是在 service 先 SELECT 一次再 INSERT：先查后插是竞态的，
// 两个请求同时通过检查就会有一个在 INSERT 时炸掉，而那条错误如果没被翻译，
// 用户会收到一个 INTERNAL 500 —— 明明只是重名。让唯一索引当裁判才是原子的。
func (r *User) CreateLocal(ctx context.Context, p NewLocalUser) (*model.User, error) {
	row := r.pool.QueryRow(ctx, `
		INSERT INTO users (username, password_hash, nickname, auth_source)
		VALUES ($1, $2, $3, $4)
		RETURNING `+userCols,
		p.Username, p.PasswordHash, p.Nickname, model.AuthSourceLocal)

	u, err := scanUser(row)
	if err != nil {
		var pge *pgconn.PgError
		if errors.As(err, &pge) && pge.Code == pgUniqueViolation {
			// Err 保留原始错误供日志用，但响应体里只会出现「用户名已被占用」
			return nil, apperr.WrapMsg(err, apperr.CodeUserAlreadyExists, "用户名已被占用")
		}
		return nil, fmt.Errorf("repo.User.CreateLocal(%q): %w", p.Username, err)
	}
	return u, nil
}

// GetByID 按主键取一行。查无此行返回 apperr NOT_FOUND（Err 里仍是 pgx.ErrNoRows，
// 上层可以用 errors.Is 继续穿透判断）。
func (r *User) GetByID(ctx context.Context, id int64) (*model.User, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id)
	u, err := scanUser(row)
	if err != nil {
		return nil, r.wrapNotFound(err, "GetByID", fmt.Sprint(id))
	}
	return u, nil
}

// GetByUsername 按用户名取一行，登录走的就是它（uq_users_username 是唯一索引）。
//
// ⚠ 调用方注意：登录路径上「查无此人」绝不能原样变成 NOT_FOUND 返给用户 ——
// 那等于开了一个账号枚举接口（挨个试用户名，看谁返回 404 谁返回 401，就能把
// 全部注册用户名爬出来）。auth.LocalProvider 会把它和「密码错」合并成同一个
// INVALID_CREDENTIALS。这里保持诚实返回 NOT_FOUND，翻译的责任在认得语境的那一层。
func (r *User) GetByUsername(ctx context.Context, username string) (*model.User, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE username = $1`, username)
	u, err := scanUser(row)
	if err != nil {
		return nil, r.wrapNotFound(err, "GetByUsername", username)
	}
	return u, nil
}

// UpdateProfile 覆写资料三件套并返回新的一行。
//
// 这里是**无条件覆写**，不用 COALESCE($2, nickname) 那种「传 NULL 就不改」的写法。
// 因为 COALESCE 分不清「我没传这个字段」和「我要把它清空」—— 而 phone/email
// 恰恰是用户会想清空的（填错了、换号了）。所以「没传就保持原值」的合并逻辑放在
// service 层用 Go 代码做（那里能区分 *string 的 nil 和指向空串的指针），
// 到 repo 时三个值都已经是最终值了。
//
// updated_at 必须显式写 now()：这个库没有 trigger（000001 迁移里一个都没有），
// 漏写的话这一列会永远停在注册时间，而它唯一的用途就是「这行最后被动过是什么时候」。
func (r *User) UpdateProfile(ctx context.Context, id int64, nickname string, phone, email *string) (*model.User, error) {
	row := r.pool.QueryRow(ctx, `
		UPDATE users
		   SET nickname = $2, phone = $3, email = $4, updated_at = now()
		 WHERE id = $1
		RETURNING `+userCols,
		id, nickname, phone, email)

	u, err := scanUser(row)
	if err != nil {
		return nil, r.wrapNotFound(err, "UpdateProfile", fmt.Sprint(id))
	}
	return u, nil
}

// UpdatePasswordHash 换密码哈希。旧哈希直接丢弃，不留历史 ——
// 密码历史对这个系统没有任何用途，留着只是一份泄漏面。
func (r *User) UpdatePasswordHash(ctx context.Context, id int64, hash string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1`, id, hash)
	if err != nil {
		return fmt.Errorf("repo.User.UpdatePasswordHash(%d): %w", id, err)
	}
	// 影响 0 行说明 id 不存在。不当成成功放过：调用方以为改好了，用户下次登录却失败，
	// 而日志里一片绿 —— 这种「静默没生效」是最难查的一类 bug。
	if tag.RowsAffected() == 0 {
		return apperr.WrapMsg(pgx.ErrNoRows, apperr.CodeNotFound, "用户不存在")
	}
	return nil
}

// wrapNotFound 把 pgx.ErrNoRows 翻译成 apperr NOT_FOUND，其它错误原样包装。
//
// 这个区分很重要：连接断了不该报「用户不存在」，那会把一次数据库故障
// 伪装成一个正常的业务结果，排查时会一路往「数据是不是没了」的方向查。
func (r *User) wrapNotFound(err error, fn, arg string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apperr.WrapMsg(err, apperr.CodeNotFound, "用户不存在")
	}
	return fmt.Errorf("repo.User.%s(%s): %w", fn, arg, err)
}

// scanUser 按 userCols 的顺序扫一行。列顺序改动时这里必须同步改 ——
// 这也是为什么 userCols 只写一遍：它和这个函数是同一个约定的两半。
func scanUser(row pgx.Row) (*model.User, error) {
	var u model.User
	err := row.Scan(
		&u.ID, &u.Username, &u.PasswordHash, &u.AuthSource, &u.SSOUserID, &u.RealName,
		&u.StudentID, &u.AvatarURL, &u.Nickname, &u.Role, &u.Status, &u.Phone, &u.Email,
		&u.CreditScore, &u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

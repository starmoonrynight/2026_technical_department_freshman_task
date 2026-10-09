package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
)

// Dict 是字典表（categories / locations）的数据访问对象。
//
// M2 只需要**读**：两张表的内容是 000001 迁移种进去的 55 + 91 行，
// 增删端点（#9–#12）在 M6，改名压根没有端点（走 Adminer，计划 §4）。
type Dict struct {
	pool *pgxpool.Pool
}

func NewDict(pool *pgxpool.Pool) *Dict { return &Dict{pool: pool} }

// CategoryRows 取全部**启用中**的分类，按 (level, sort_order, id) 排好序。
//
// 排序里的 level 是建树的前提：父节点的 level 一定比子节点小，
// 所以「父行先出现」这件事由 ORDER BY 保证，model.BuildCategoryTree 才能
// 一遍扫过去就把子节点挂到已经建好的父节点上。
//
// 只取 is_active=true：停用一个小类之后它就不该再出现在发帖的下拉框里。
// 已经引用了它的老帖子不受影响 —— items 那边是 JOIN categories 而不是
// 「JOIN 且要求 active」，所以老帖子的分类名照样显示得出来。
func (r *Dict) CategoryRows(ctx context.Context) ([]model.Category, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, parent_id, name, level, sort_order, is_active
		  FROM categories
		 WHERE is_active
	  ORDER BY level, sort_order, id`)
	if err != nil {
		return nil, fmt.Errorf("repo.Dict.CategoryRows: %w", err)
	}
	defer rows.Close()

	out := []model.Category{}
	for rows.Next() {
		var c model.Category
		if err := rows.Scan(&c.ID, &c.ParentID, &c.Name, &c.Level, &c.SortOrder, &c.IsActive); err != nil {
			return nil, fmt.Errorf("repo.Dict.CategoryRows 扫描一行: %w", err)
		}
		out = append(out, c)
	}
	// rows.Err() 必须查：迭代中途连接断了的话，Next() 只是返回 false，
	// 不检查就会把「半份数据」当成「完整数据」交上去 —— 前端表现为
	// 「分类少了一半」，而日志里一片绿。
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo.Dict.CategoryRows 迭代: %w", err)
	}
	return out, nil
}

// LocationRows 取全部启用中的地点，规则同 CategoryRows。
func (r *Dict) LocationRows(ctx context.Context) ([]model.Location, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, parent_id, name, level, sort_order, is_active, is_freeform
		  FROM locations
		 WHERE is_active
	  ORDER BY level, sort_order, id`)
	if err != nil {
		return nil, fmt.Errorf("repo.Dict.LocationRows: %w", err)
	}
	defer rows.Close()

	out := []model.Location{}
	for rows.Next() {
		var l model.Location
		if err := rows.Scan(&l.ID, &l.ParentID, &l.Name, &l.Level, &l.SortOrder, &l.IsActive, &l.IsFreeform); err != nil {
			return nil, fmt.Errorf("repo.Dict.LocationRows 扫描一行: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("repo.Dict.LocationRows 迭代: %w", err)
	}
	return out, nil
}

// GetCategory 按主键取一行分类，**不管 is_active** —— 判断「这个分类还能不能用」
// 是 service 的业务规则，repo 只负责诚实地把行取回来。
func (r *Dict) GetCategory(ctx context.Context, id int64) (*model.Category, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, parent_id, name, level, sort_order, is_active
		  FROM categories WHERE id = $1`, id)

	var c model.Category
	err := row.Scan(&c.ID, &c.ParentID, &c.Name, &c.Level, &c.SortOrder, &c.IsActive)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.WrapMsg(err, apperr.CodeNotFound, "分类不存在")
		}
		return nil, fmt.Errorf("repo.Dict.GetCategory(%d): %w", id, err)
	}
	return &c, nil
}

// GetLocation 按主键取一行地点，规则同 GetCategory。
func (r *Dict) GetLocation(ctx context.Context, id int64) (*model.Location, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT id, parent_id, name, level, sort_order, is_active, is_freeform
		  FROM locations WHERE id = $1`, id)

	var l model.Location
	err := row.Scan(&l.ID, &l.ParentID, &l.Name, &l.Level, &l.SortOrder, &l.IsActive, &l.IsFreeform)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, apperr.WrapMsg(err, apperr.CodeNotFound, "地点不存在")
		}
		return nil, fmt.Errorf("repo.Dict.GetLocation(%d): %w", id, err)
	}
	return &l, nil
}

// CountActiveChildren 数一个地点下面还有几个启用中的子节点。
//
// 发帖时校验「location_id 必须是叶子」用的就是它（计划 §3.2）。
// 为什么不直接判 level=3：「其他」是 level=1 且 is_freeform=true 的**一级叶子**，
// 它没有子节点、用户直接选它。所以「叶子」的准确定义是「没有子节点」，
// 不是「level 最大」—— 按 level 判会把「其他」整个拒掉，而它恰恰是必须能选的。
func (r *Dict) CountActiveChildren(ctx context.Context, id int64) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		SELECT count(*) FROM locations WHERE parent_id = $1 AND is_active`, id).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("repo.Dict.CountActiveChildren(%d): %w", id, err)
	}
	return n, nil
}

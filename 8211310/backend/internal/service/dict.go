package service

import (
	"context"

	"lostfound/internal/model"
)

// DictTrees 是「查字典树」需要的全部持久化能力。
//
// 定成窄接口而不是直接用 *repo.Dict，理由和 auth.UserLookup / middleware.UserByID /
// service.UserStore 完全一样：依赖面写在类型上，单元测试能塞内存实现。
// 这个接口只有两个方法，因为 #7 和 #8 是**只读**接口，不碰任何单行查询。
type DictTrees interface {
	CategoryRows(ctx context.Context) ([]model.Category, error)
	LocationRows(ctx context.Context) ([]model.Location, error)
}

// Dict 是字典的业务层。
//
// 它薄到几乎没有逻辑（取行 → 建树），但仍然单独一层：建树是 model 里的纯函数，
// 「哪些行该出现在树里」是业务规则（目前只有 is_active 一条，住在 repo 的 SQL 里），
// 而 handler 不该同时知道这两件事。
type Dict struct {
	store DictTrees
}

func NewDict(store DictTrees) *Dict { return &Dict{store: store} }

// CategoryTree 返回 #7 GET /api/categories 的 data。
//
// 全量返回、不分页：55 行分类和 91 行地点加起来也就几 KB，前端进页面时拉一次
// 缓存在内存里，之后所有级联下拉和筛选器都从这份数据里取。
// 做成「按父节点懒加载」会多出十几次请求，而省下来的字节数不值得。
func (s *Dict) CategoryTree(ctx context.Context) ([]*model.CategoryNode, error) {
	rows, err := s.store.CategoryRows(ctx)
	if err != nil {
		return nil, err
	}
	return model.BuildCategoryTree(rows), nil
}

// LocationTree 返回 #8 GET /api/locations 的 data。
func (s *Dict) LocationTree(ctx context.Context) ([]*model.LocationNode, error) {
	rows, err := s.store.LocationRows(ctx)
	if err != nil {
		return nil, err
	}
	return model.BuildLocationTree(rows), nil
}

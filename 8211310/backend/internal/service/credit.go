package service

import (
	"context"
	"log/slog"

	"lostfound/internal/model"
)

// 这个文件是 M5 的最后一块，也是全项目**最短**的一个服务：#33 把积分流水读出来给人看。
//
// 它存在的理由写在计划 §3.6：credit_score 是一个被夹过的累计值，单看那个数字永远
// 解释不了「我为什么是 110」。有这条流水，用户问、客服查、测试对账三件事才都有依据。
//
// ⚠ 这个服务**只读**，而且比看起来更瘦：
//   - 没有 CreditStore 之外的依赖，没有 UserLookup —— 当前用户的 credit_score 直接取自
//     actor，因为 JWT 中间件每个请求都从库里重新读了一行 users（middleware/jwt.go 顶部
//     那条「token 里不放 role/status」的决定顺带保证了这里的值是新鲜的）。
//     再加一次 GetByID 是第二次读同一行，除了多一个依赖和一次往返什么也不换来。
//   - 没有任何写方法。加分唯一的路径是 repo.Confirm 那个事务里的 applyCredit，
//     所以这个 struct 连「能写」的可能性都没有（见 repo/credit.go 顶部同一条纪律）。

// CreditStore 是流水表需要的全部能力：一个分页读。
//
// 一个方法的接口不是形式主义：它是「积分只能被赚、不能被改」这句话在类型层面的写法。
// 想在这里加 Set / Adjust，就得先去回答「谁能调它、调完流水怎么对齐」，
// 而那个问题在 M5 的答案是「不开放，要改数据走 Adminer，且 review_kind 会露馅」。
type CreditStore interface {
	List(ctx context.Context, userID int64, page, pageSize int) ([]model.CreditLog, int, error)
}

// Credit 是积分流水的业务规则（只有解析分页参数这一条）。
type Credit struct {
	logs   CreditStore
	logger *slog.Logger
}

func NewCredit(logs CreditStore, logger *slog.Logger) *Credit {
	if logger == nil {
		logger = slog.Default()
	}
	return &Credit{logs: logs, logger: logger}
}

// CreditHistory 是 #33 GET /api/my/credit-logs 的 data（计划 §4 第 33 行）。
//
// 形状是 {credit_score, list, total, page, page_size} —— 分页四件套之外多一个
// credit_score，而不是让前端再去 #5 拉一次自己。理由：这一页的用户意图是
// 「看看我的分数是怎么来的」，把结论和明细放在同一个响应里，前端就不会
// 出现「分数来自 A 接口、流水来自 B 接口、两者中间隔着一次并发 confirm」的对不齐。
//
// 字段显式列出来而没有内嵌 Page[T]：内嵌结构体在 encoding/json 里的键顺序
// 由提升规则决定，读代码的人看不出 `{credit_score, list, ...}` 到底是不是计划里那个顺序。
// 五个字面字段换可读性，值。
type CreditHistory struct {
	CreditScore int                   `json:"credit_score"`
	List        []model.CreditLogView `json:"list"`
	Total       int                   `json:"total"`
	Page        int                   `json:"page"`
	PageSize    int                   `json:"page_size"`
}

// MyLogs 返回当前用户的积分流水。
//
// userID 一律取自 JWT 而不是查询参数（同 #19/#30 那条纪律）：这张表里躺着的是
// 「谁在什么时候因为哪条归还确认加了几分」，让它能被 ?user_id= 指定就是全站
// 最大的个人记录泄漏面。
func (s *Credit) MyLogs(ctx context.Context, actor *model.User, q PageQuery) (*CreditHistory, error) {
	page, pageSize, err := parsePageQuery(q)
	if err != nil {
		return nil, err
	}

	rows, total, err := s.logs.List(ctx, actor.ID, page, pageSize)
	if err != nil {
		return nil, err
	}

	list := make([]model.CreditLogView, 0, len(rows))
	for i := range rows {
		list = append(list, rows[i].View())
	}

	return &CreditHistory{
		CreditScore: actor.CreditScore,
		List:        list,
		Total:       total,
		Page:        page,
		PageSize:    pageSize,
	}, nil
}

package repo

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"lostfound/internal/apperr"
)

// PostgreSQL 的 SQLSTATE 错误码。
//
// 为什么按码判断而不是按错误文本：文本会随 PG 版本和 locale 变化，码是稳定的。
// 这条纪律和 §7.6 里「匹配杭电助手错误按 error/code 判断，不按 msg」是同一条。
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	pgNotNullViolation    = "23502"
	pgCheckViolation      = "23514"
)

// TranslateConstraint 把数据库的约束违规翻译成一个能给用户看的 apperr。
//
// 返回 nil 表示「这不是一个我们认识的约束违规」，调用方应当按普通错误继续包装
// （最终由 handler 层变成 INTERNAL 500）。
//
// **为什么要有这一层**：约束是数据库在替我们兜底。service 已经校验过一遍了，
// 正常情况下永远走不到这里 —— 可一旦 service 漏了一条规则，没有这层翻译的话
// 用户会收到一句 "value too long for type character varying(100)" 的 INTERNAL 500：
// 既是英文技术细节（看不懂），又暴露了表结构（§8 明确禁止），还让一次
// 「用户填错了」的普通事故在日志里表现成服务器故障。
//
// 翻译的方向是「4xx + 字段名」，而不是「藏起来」：用户能改的就让他改。
func TranslateConstraint(err error) *apperr.Error {
	var pge *pgconn.PgError
	if !errors.As(err, &pge) {
		return nil
	}

	switch pge.Code {
	case pgCheckViolation:
		if e, ok := checkViolations[pge.ConstraintName]; ok {
			return apperr.WrapMsg(err, apperr.CodeValidation, e.msg).WithField(e.field, e.detail)
		}
	case pgNotNullViolation:
		// NOT NULL 被撞到说明 service 少校验了一个必填字段。
		// 列名是 pge.ColumnName，它和 JSON 字段名在本项目里恰好同名
		// （items 表的列名就是 API 的字段名），可以直接用。
		return apperr.WrapMsg(err, apperr.CodeValidation, "缺少必填字段").
			WithField(pge.ColumnName, "这个字段是必填的")
	case pgForeignKeyViolation:
		if e, ok := fkViolations[pge.ConstraintName]; ok {
			return apperr.WrapMsg(err, apperr.CodeValidation, e.msg).WithField(e.field, e.detail)
		}
		// 不认识的外键：给一个不泄漏表名的通用文案，仍然是 4xx 而不是 500 ——
		// 引用完整性失败几乎总是「用户传了一个不存在的 id」。
		return apperr.WrapMsg(err, apperr.CodeValidation, "引用了一个不存在的资源")
	case pgUniqueViolation:
		return apperr.WrapMsg(err, apperr.CodeConflict, "内容和已有的记录冲突了")
	}
	return nil
}

// fieldHint 是一条约束违规对应给前端的字段级提示。
type fieldHint struct {
	field  string
	msg    string // 整条响应的 message
	detail string // data.errors 里那一条的 msg
}

// checkViolations 是 CHECK 约束名 → 用户能看懂的提示。
//
// 约束名在 000001 迁移里都是显式命名的（CONSTRAINT items_time_semantics CHECK ...），
// 不是 PG 自动生成的 items_xxx_check，所以这张表能稳定对上。
// 加新约束时记得往这里加一行 —— 漏加的后果是回落到下面那个通用分支，
// 用户会看到一句没有字段名的「数据不符合约束」，虽然不至于 500，但帮不上忙。
var checkViolations = map[string]fieldHint{
	"items_time_semantics": {
		field:  "lost_at",
		msg:    "时间和帖子类型对不上",
		detail: "lost 帖必须同时填 last_seen_at 和 lost_at（前者不晚于后者），且不能填 found_at；found 帖只填 found_at",
	},
	"items_contact_not_blank": {
		field:  "contact",
		msg:    "联系方式不能为空",
		detail: "填一个对方能联系到你的方式，内容不做校验",
	},
	"items_title_not_blank": {
		field:  "title",
		msg:    "标题不能为空",
		detail: "标题不能只有空白字符",
	},
	"items_item_type_check": {
		field:  "item_type",
		msg:    "帖子类型不对",
		detail: "只能是 lost 或 found",
	},
	"items_status_check": {
		field:  "status",
		msg:    "帖子状态不对",
		detail: "只能是 open、closed 或 deleted",
	},
	"categories_parent_level": {
		field:  "parent_id",
		msg:    "分类层级和父节点对不上",
		detail: "一级分类不能有父节点，二级分类必须有",
	},
	"locations_parent_level": {
		field:  "parent_id",
		msg:    "地点层级和父节点对不上",
		detail: "一级地点不能有父节点，二三级必须有",
	},
}

// fkViolations 是外键约束名 → 提示。PG 自动生成的外键名是 <表>_<列>_fkey，同样稳定。
var fkViolations = map[string]fieldHint{
	"items_category_id_fkey": {
		field:  "category_id",
		msg:    "分类不存在",
		detail: "请从 GET /api/categories 返回的列表里选一个小类",
	},
	"items_location_id_fkey": {
		field:  "location_id",
		msg:    "地点不存在",
		detail: "请从 GET /api/locations 返回的列表里选一个具体地点",
	},
	"items_user_id_fkey": {
		field:  "user_id",
		msg:    "发布者不存在",
		detail: "登录状态可能已经失效，请重新登录",
	},
}

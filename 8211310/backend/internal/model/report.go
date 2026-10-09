package model

// 举报的六个 reason_code 和三种状态。
//
// ⚠ 这两组字面量不是我们自己定的，它们是 000001 迁移第 12 节里 reports 表
// 两条 CHECK 约束的内容。写错一个字母不会编译报错，也不会有一个清楚的运行时提示，
// 得到的是 SQLSTATE 23514（check_violation）—— 那是 §10 里最难查的一类失败：
// 看起来是数据库坏了，实际是代码里少了一个字母。
// 所以这里一次列全，当作那两条 CHECK 的镜像，service 按它做**前置**校验。
//
// 状态机只有三个值，而且**没有任何自动转移**：
//   - open     —— #41 写入时唯一的取值
//   - resolved —— admin 采纳了（#49，M6）
//   - dismissed —— admin 没采纳（#49，M6）
//
// M4 只产生 open。之所以把后两个也列在这里，是因为 #41 的响应形状里有 `status` 字段，
// 而它的值域是这三者之一 —— 少列一个，将来读响应的人就会以为只有两种。
const (
	ReportReasonSpam       = "spam"
	ReportReasonPrivacy    = "privacy"
	ReportReasonFraud      = "fraud"
	ReportReasonHarassment = "harassment"
	ReportReasonIllegal    = "illegal"
	ReportReasonOther      = "other"

	ReportStatusOpen      = "open"
	ReportStatusResolved  = "resolved"
	ReportStatusDismissed = "dismissed"
)

// ReportReasonCodes 是 reason_code 的合法值集合，顺序和迁移里的 CHECK 一致。
//
// 用它做校验而不是写六个 case：校验点和错误文案（「只能是 …」）都要列一遍合法值，
// 两处各写一遍的话，将来 CHECK 加一个码就得记得改两处。
// 这个切片是那条 CHECK 的第二镜像，而**镜像只该有一份**。
var ReportReasonCodes = []string{
	ReportReasonSpam,
	ReportReasonPrivacy,
	ReportReasonFraud,
	ReportReasonHarassment,
	ReportReasonIllegal,
	ReportReasonOther,
}

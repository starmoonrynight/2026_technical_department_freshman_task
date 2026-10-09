package handler

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"lostfound/internal/apperr"
)

// pathID 取一个路径参数并转成正整数 id。
//
// ⚠ 解析失败返回 **NOT_FOUND** 而不是 VALIDATION。
// 计划 §4 里 #15/#16/#17/#18/#42 的专属错误码都列了 NOT_FOUND，没有一条列 VALIDATION。
// 语义上也更对：`/api/items/abc` 指的是一条不存在的帖子，
// 而「参数格式不对」暗示「换个格式就能查」—— 但没有任何格式能让 abc 变成一条帖子。
// 顺带的好处是不向外透露「我们对 id 做了什么校验」。
func pathID(c *gin.Context, param, what string) (int64, error) {
	id, err := strconv.ParseInt(c.Param(param), 10, 64)
	if err != nil || id <= 0 {
		return 0, apperr.NotFound(what)
	}
	return id, nil
}

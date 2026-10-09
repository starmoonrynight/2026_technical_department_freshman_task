package middleware

import (
	"context"
	"log/slog"
	"strings"

	"github.com/gin-gonic/gin"

	"lostfound/internal/apperr"
	"lostfound/internal/auth"
	"lostfound/internal/model"
)

// UserByID 是 JWT 中间件需要的**全部**数据库能力：按主键取一行 users。
//
// 和 auth.UserLookup 同一个套路 —— 定成窄接口而不是直接依赖 *repo.User：
// 中间件的单元测试可以塞一个内存实现，不用起数据库；同时「中间件到底碰了
// repo 的哪些方法」被写在类型上，将来 repo 长大也不会悄悄扩大依赖面。
type UserByID interface {
	GetByID(ctx context.Context, id int64) (*model.User, error)
}

// headerAuthorization 是承载 token 的标准请求头。
const headerAuthorization = "Authorization"

// bearerPrefix 是 RFC 6750 定义的 token 前缀。
const bearerPrefix = "Bearer "

// JWT 校验登录态，通过后把用户塞进 context，不通过就直接写错误响应并中止。
//
// ⚠ 每个请求都拿 sub 去库里读一遍 users，**不是**从 token 的 claims 里读 role/status。
// 这是刻意的，理由写在 auth.TokenSigner.Issue 的注释里，一句话版本：
// JWT 签出去就改不了，把 role 放进 claims 意味着 admin 降权/封号之后，
// 那个人手上的旧 token 还能继续用满 24 小时 —— 而封号的整个意义就是立刻生效。
// 代价是每个已登录请求多一次主键查询（走 PK 索引，微秒级）。
func JWT(signer auth.TokenSigner, users UserByID) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := bearerToken(c)
		if !ok {
			// 没带 token 和 token 无效都是 UNAUTHORIZED（§8）：
			// 对前端来说处理方式完全一样（跳登录页），分成两个码只会让它多写一套分支。
			reject(c, apperr.NewMsg(apperr.CodeUnauthorized, "请先登录"))
			return
		}

		u, err := resolveUser(c.Request.Context(), signer, users, raw)
		if err != nil {
			// 错误里已经区分好了三种情况的文案，且内部原因包在 apperr.Error.Err 里 ——
			// 访问日志会打出来，但响应体只有「登录已过期」这类通用文案（§8）。
			reject(c, err)
			return
		}

		setIdentity(c, u)
		c.Next()
	}
}

// OptionalJWT 是「认得出身份就认，认不出就当匿名」的 JWT 中间件。
//
// 存在它的唯一理由是 #15 GET /api/items/:id：那是个**公开**接口（帖子内容对未登录
// 用户完全公开，§2.4），但 contact 的可见性规则里有「当前用户是发帖人」和
// 「已解锁过」两个分支，所以它必须知道「现在是谁在看」。
//
// ⚠ 任何失败都**不返回 401**，包括 token 过期、签名不对、用户被封、用户已被删。
// 公开接口因为一个过期 token 而拒绝服务是荒谬的 —— 用户该看到的是帖子内容
// 和一个「登录后查看联系方式」的按钮，而不是一句「请先登录」然后被踢去登录页。
// 最坏的结果只是他看不到 contact，而这恰好就是未登录用户本该得到的结果。
//
// 失败时记一条 debug 日志而不是彻底静默：前端报「我明明登录了怎么还让我解锁」
// 这类问题时，那条日志是唯一能区分「token 真过期了」和「前端忘了带头」的证据。
func OptionalJWT(signer auth.TokenSigner, users UserByID) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw, ok := bearerToken(c)
		if !ok {
			c.Next()
			return
		}

		u, err := resolveUser(c.Request.Context(), signer, users, raw)
		if err != nil {
			apperr.Log(c).DebugContext(c.Request.Context(), "auth.optional_ignored",
				slog.String("path", c.Request.URL.Path),
				slog.String("err", err.Error()))
			c.Next()
			return
		}

		setIdentity(c, u)
		c.Next()
	}
}

// resolveUser 把 token 换成一行 users，是两个 JWT 中间件共用的那一段。
//
// 抽出来不只是省重复：这两条路径对「身份」的定义必须是**同一个**，
// 否则就会出现「带这个 token 能发帖但不能看详情页的联系方式」这种自相矛盾的行为，
// 而那种 bug 从现象往回查到「原来两个中间件各写了一遍判断」要花很久。
func resolveUser(ctx context.Context, signer auth.TokenSigner, users UserByID, raw string) (*model.User, error) {
	userID, err := signer.Parse(raw)
	if err != nil {
		return nil, err
	}

	u, err := users.GetByID(ctx, userID)
	if err != nil {
		if apperr.IsCode(err, apperr.CodeNotFound) {
			// token 签名有效，但对应的用户已经不在库里了。
			// 现实成因：admin 从 Adminer 里删了这行（§3.4 允许），或者测试库被 truncate 过。
			// 对用户表现为 401 让他重新登录，比 500 合理 —— 他确实没有有效身份了。
			return nil, apperr.WrapMsg(err, apperr.CodeUnauthorized, "账号不存在，请重新登录")
		}
		// 数据库故障 → 让 apperr.Respond 转成 INTERNAL 500。
		// 绝不能伪装成 401：那会把一次宕机表现成「所有人都登录失效」，
		// 排查时会去查 JWT 密钥是不是换了，而真正的原因是连接断了。
		return nil, err
	}

	// 封禁拦截。§8 对 USER_BANNED 的定义是「被封禁用户登录/**操作**」——
	// 登录那一半在 auth.LocalProvider 里，操作这一半就在这里。
	// 少了这里，admin 封号之后那个人手上的旧 token 还能继续发帖、删帖、解锁联系方式。
	if u.IsBanned() {
		return nil, apperr.NewMsg(apperr.CodeUserBanned, "账号已被封禁，请联系管理员")
	}
	return u, nil
}

// setIdentity 把认出来的用户塞进 context。
//
// ⚠ 这里**不要**把 user_id 再预绑定进 logger（apperr.SetLogger(c, log.With(...))）。
// AccessLog 已经把 user_id 作为一个显式字段打进 http.request 那行了，
// 而它在 c.Next() 之后才执行，那时 UserID(c) 已经被下面这两行设好。
// 再绑定一次会让同一条日志里出现两个 user_id 键 —— 这正是 accesslog.go
// 里对 request_id 特别注明「不在这里写，因为已经预绑定过了」的同一个坑。
//
// 业务日志（比如 service 里的 item.create）需要 user_id 时自己显式带上，
// 那样每条日志的字段是写出来的、看得见的，不依赖中间件的绑定顺序。
func setIdentity(c *gin.Context, u *model.User) {
	apperr.SetUserID(c, u.ID)
	apperr.SetUser(c, u)
}

// bearerToken 从 Authorization 头里取出 token。
//
// 接受的形状：`Bearer <token>`。scheme 大小写不敏感（RFC 7235 规定 auth-scheme
// 是大小写不敏感的），所以 `bearer xxx` 也放行 —— 严格按大小写匹配会让
// 某些 HTTP 客户端（和历史上的 curl 脚本）莫名其妙地拿到 401，而放行它没有任何安全代价。
func bearerToken(c *gin.Context) (string, bool) {
	h := c.GetHeader(headerAuthorization)
	if len(h) < len(bearerPrefix) {
		return "", false
	}
	if !strings.EqualFold(h[:len(bearerPrefix)], bearerPrefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(bearerPrefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// reject 写错误响应并中止链路。
//
// c.Abort() 是必须的，而且漏掉它不会立刻出错 —— gin 会继续调用后续的 handler，
// 于是客户端收到两个响应体拼在一起的畸形 JSON，或者 handler 在一个
// 「其实没登录」的 context 上继续跑（UserID(c) 是 0）。这类 bug 的现象
// 离原因很远，所以这里把它和写响应绑成一个函数，不给「忘了 Abort」留机会。
func reject(c *gin.Context, err error) {
	apperr.Respond(c, err)
	c.Abort()
}

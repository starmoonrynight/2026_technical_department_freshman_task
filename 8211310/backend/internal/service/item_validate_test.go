package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"lostfound/internal/apperr"
	"lostfound/internal/model"
)

// 本文件覆盖的是 items 的**纯校验规则**（item_validate.go）。
//
// M2 的验收判据里有一大半是「某种输入必须得 VALIDATION」。把它们放在这一层测，
// 而不是放到集成测试里，理由很具体：集成测试每验一条都得先建用户、造分类、起事务，
// 慢、脆，而且失败信息里混着一堆和「校验对不对」无关的东西。
//
// ⚠ 断言只看 code 和 field，**不看 message** —— message 是给人看的中文，
// 随时可以改措辞（计划 §8）。

// requireValidation 断言 err 是 VALIDATION，并且它的字段级细节里包含 wantField。
//
// 为什么连 field 一起断言：前端靠 field 把红框定位到具体输入框。
// 只断言 code 的话，「所有校验都报 title 字段」这种退化不会让任何测试变红，
// 而它的表现是用户改哪个框都没用。
func requireValidation(t *testing.T, err error, wantField string) {
	t.Helper()
	if err == nil {
		t.Fatalf("期望 VALIDATION（field=%s），实际没有错误", wantField)
	}
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		t.Fatalf("错误不是 *apperr.Error：%v", err)
	}
	if ae.Code != apperr.CodeValidation {
		t.Fatalf("期望 code=%s，实际 code=%s（%v）", apperr.CodeValidation, ae.Code, err)
	}
	for _, f := range ae.Fields {
		if f.Field == wantField {
			return
		}
	}
	t.Fatalf("VALIDATION 的字段细节里没有 %q，实际是 %+v", wantField, ae.Fields)
}

// baseLost / baseFound 是两份**一定合法**的输入，每条用例只改自己关心的那一个字段。
//
// 从零值开始构造的话，一条「title 超长」的用例会先撞上「contact 为空」，
// 于是它测的其实是 contact —— 而且是在将来 contact 规则变化时静默地改测别的东西。
func baseLost() ItemFields {
	return ItemFields{
		Title:          "黑色长款钱包",
		Description:    "皮质，内有一张校园卡",
		CategoryID:     36, // 迁移里「衣物箱包 → 钱包」的真实 id
		LocationID:     50,
		LocationDetail: "3楼自习室B区靠窗",
		LastSeenAt:     "2026-10-01T08:00:00+08:00",
		LostAt:         "2026-10-03T20:00:00+08:00",
		Contact:        "微信 lost-wallet-001",
	}
}

func baseFound() ItemFields {
	f := baseLost()
	f.Title = "捡到黑色长款钱包"
	f.LastSeenAt = ""
	f.LostAt = ""
	f.FoundAt = "2026-10-04T09:30:00+08:00"
	f.Contact = "13800000000"
	return f
}

// ---------- normalizeItemFields ----------

func TestNormalizeItemFieldsHappyPath(t *testing.T) {
	got, err := normalizeItemFields(model.ItemTypeLost, baseLost())
	if err != nil {
		t.Fatalf("合法输入不该报错：%v", err)
	}
	if got.Title != "黑色长款钱包" || got.Contact != "微信 lost-wallet-001" {
		t.Errorf("字段没有原样带过来：%+v", got)
	}
	if got.LastSeenAt == nil || got.LostAt == nil || got.FoundAt != nil {
		t.Fatalf("lost 帖的时间列不对：lastSeen=%v lost=%v found=%v",
			got.LastSeenAt, got.LostAt, got.FoundAt)
	}

	// 存 UTC（计划 §3.1）。timestamptz 存的是绝对时刻，转不转不影响库里的值，
	// 但转了之后日志和 SQL tracer 打出来的参数和库里一致，排查时不用在脑子里换算时区。
	if loc := got.LostAt.Location(); loc != time.UTC {
		t.Errorf("时间应该转成 UTC，实际是 %v", loc)
	}
	if h := got.LostAt.Hour(); h != 12 { // 20:00+08:00 == 12:00Z
		t.Errorf("时区换算错了：期望 12 点（UTC），实际 %d 点", h)
	}

	got, err = normalizeItemFields(model.ItemTypeFound, baseFound())
	if err != nil {
		t.Fatalf("合法的 found 输入不该报错：%v", err)
	}
	if got.FoundAt == nil || got.LastSeenAt != nil || got.LostAt != nil {
		t.Errorf("found 帖的时间列不对：lastSeen=%v lost=%v found=%v",
			got.LastSeenAt, got.LostAt, got.FoundAt)
	}
}

// TestNormalizeTrimsWhitespace 钉住「入库前 trim」。
//
// 不 trim 的话，一个标题是 "钱包 " 的帖子在广场上会显示成带尾随空格的样子，
// 而 keyword 搜索「钱包」照样能搜到它 —— 于是没人会发现，直到某天排序或去重出问题。
func TestNormalizeTrimsWhitespace(t *testing.T) {
	f := baseLost()
	f.Title = "  黑色钱包 \n\t "
	f.Description = "  皮质  "
	f.LocationDetail = "  3楼  "
	f.Contact = "  13800000000  "
	f.LastSeenAt = "  2026-10-01T08:00:00+08:00  "

	got, err := normalizeItemFields(model.ItemTypeLost, f)
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if got.Title != "黑色钱包" || got.Description != "皮质" ||
		got.LocationDetail != "3楼" || got.Contact != "13800000000" {
		t.Errorf("没有 trim：%+v", got)
	}
}

func TestNormalizeTitleRules(t *testing.T) {
	cases := []struct {
		name  string
		title string
		field string // "" 表示期望成功
	}{
		{"空", "", "title"},
		{"纯空格", "     ", "title"},
		{"纯全角空格", "\u3000\u3000", "title"},
		{"刚好 100 个汉字", strings.Repeat("钱", 100), ""},
		// 101 个汉字 = 303 字节。用 len() 数长度的实现会在 34 个汉字处就报错，
		// 于是「标题最长 100」变成「最长 33 个汉字」，和数据库 VARCHAR(100) 的行为不一致。
		{"101 个汉字", strings.Repeat("钱", 101), "title"},
		{"刚好 100 个 ASCII", strings.Repeat("a", 100), ""},
		{"101 个 ASCII", strings.Repeat("a", 101), "title"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := baseLost()
			f.Title = tc.title
			_, err := normalizeItemFields(model.ItemTypeLost, f)
			if tc.field == "" {
				if err != nil {
					t.Fatalf("不该报错：%v", err)
				}
				return
			}
			requireValidation(t, err, tc.field)
		})
	}
}

// TestNormalizeContactIsNeverValidated 是 M2 验收判据里最重要的一条：
// **contact 只校验非空和长度，内容一律不校验。**
//
// 这条契约之所以要写成一个专门的测试，是因为它天生违反直觉 ——
// 任何看到「联系方式」字段的人都会想「至少校验一下手机号格式吧」。
// 而计划 §3.2 明确不做，理由见 normalizeItemFields 里那段注释（定位原则 1：
// 平台一旦校验就要为校验结果背书）。
//
// 测试的作用是让「顺手加个校验」这个动作**立刻变红**，
// 逼着改的人回来读一遍这条判据，而不是悄悄把契约改掉。
func TestNormalizeContactIsNeverValidated(t *testing.T) {
	accepted := []string{
		"无",
		"问宿舍阿姨",
		"图书馆前台",
		"13800000000",
		"1380000",            // 位数不对也收
		"abc",                // 完全不像联系方式也收
		"微信：lost+found_2026", // 带符号也收
		"10086",
		"请扫码加我 qq 群 123456789",
		"!!!",
		"<script>alert(1)</script>", // 内容不做过滤；转义是前端渲染时的责任（§8）
		strings.Repeat("联", 100),    // 刚好 100 字符
	}
	for _, c := range accepted {
		t.Run("接受 "+truncateForName(c), func(t *testing.T) {
			f := baseLost()
			f.Contact = c
			got, err := normalizeItemFields(model.ItemTypeLost, f)
			if err != nil {
				t.Fatalf("contact=%q 应该被接受（计划 §3.2：不校验内容），实际报错 %v", c, err)
			}
			if got.Contact != c {
				t.Errorf("contact 应该原样保留，期望 %q 实际 %q", c, got.Contact)
			}
		})
	}

	rejected := []struct {
		name    string
		contact string
	}{
		{"空串", ""},
		{"纯半角空格", "     "},
		{"纯制表符换行", "\t\n "},
		// 下面两个是数据库 CHECK items_contact_not_blank 的字符集：
		// 它 btrim 掉的除了 ASCII 空白还有 chr(160) 和 chr(12288)。
		// Go 的 strings.TrimSpace 也认这两个，所以两边行为一致 ——
		// 如果不一致，就会出现「service 放过去了，数据库 CHECK 拦下来」的 500。
		{"纯不换行空格 U+00A0", "\u00a0\u00a0"},
		{"纯全角空格 U+3000", "\u3000\u3000"},
		{"101 个字符", strings.Repeat("联", 101)},
	}
	for _, tc := range rejected {
		t.Run("拒绝 "+tc.name, func(t *testing.T) {
			f := baseLost()
			f.Contact = tc.contact
			_, err := normalizeItemFields(model.ItemTypeLost, f)
			requireValidation(t, err, "contact")
		})
	}
}

func TestNormalizeOtherLengthLimits(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*ItemFields)
		field string
	}{
		{"description 5001 字符", func(f *ItemFields) { f.Description = strings.Repeat("描", 5001) }, "description"},
		{"location_detail 201 字符", func(f *ItemFields) { f.LocationDetail = strings.Repeat("位", 201) }, "location_detail"},
		{"category_id 为 0", func(f *ItemFields) { f.CategoryID = 0 }, "category_id"},
		{"category_id 为负", func(f *ItemFields) { f.CategoryID = -1 }, "category_id"},
		{"location_id 为 0", func(f *ItemFields) { f.LocationID = 0 }, "location_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := baseLost()
			tc.mut(&f)
			_, err := normalizeItemFields(model.ItemTypeLost, f)
			requireValidation(t, err, tc.field)
		})
	}
}

// TestNormalizeTimeSemantics 覆盖三个时间列的组合规则。
//
// 数据库有一条 items_time_semantics CHECK 做兜底，但**必须在这里先查一遍**：
// CHECK 被撞到时 PG 抛的是一句英文技术细节，用户看不出该改哪个字段。
// 集成测试里另外验一次 CHECK 本身，两边合起来才是完整的。
func TestNormalizeTimeSemantics(t *testing.T) {
	cases := []struct {
		name     string
		itemType string
		mut      func(*ItemFields)
		field    string // "" = 期望成功
	}{
		// ---- lost ----
		{"lost 缺 last_seen_at", model.ItemTypeLost, func(f *ItemFields) { f.LastSeenAt = "" }, "last_seen_at"},
		{"lost 缺 lost_at", model.ItemTypeLost, func(f *ItemFields) { f.LostAt = "" }, "lost_at"},
		{"lost 两个时间都缺", model.ItemTypeLost, func(f *ItemFields) { f.LastSeenAt = ""; f.LostAt = "" }, "last_seen_at"},
		{"lost 多填了 found_at", model.ItemTypeLost, func(f *ItemFields) { f.FoundAt = "2026-10-04T09:00:00+08:00" }, "found_at"},
		{"lost last_seen 晚于 lost", model.ItemTypeLost, func(f *ItemFields) {
			f.LastSeenAt = "2026-10-05T08:00:00+08:00"
			f.LostAt = "2026-10-03T08:00:00+08:00"
		}, "last_seen_at"},
		// 相等是合法的：「8 点还拿着，8 点发现没了」完全说得通（课间十分钟）。
		// 写成 < 而不是 <= 的话这条会误伤，而误伤的表现是用户怎么改都提交不了。
		{"lost 两个时间相等", model.ItemTypeLost, func(f *ItemFields) {
			f.LastSeenAt = "2026-10-03T08:00:00+08:00"
			f.LostAt = "2026-10-03T08:00:00+08:00"
		}, ""},
		// 跨时区比较：09:00+08:00 == 01:00Z，早于 10:00Z，所以合法。
		// 如果实现里比的是「墙上时间的字符串」而不是时刻，这条会挂。
		{"lost 跨时区仍然正确", model.ItemTypeLost, func(f *ItemFields) {
			f.LastSeenAt = "2026-10-03T09:00:00+08:00"
			f.LostAt = "2026-10-03T18:00:00+08:00"
		}, ""},

		// ---- found ----
		{"found 缺 found_at", model.ItemTypeFound, func(f *ItemFields) { f.FoundAt = "" }, "found_at"},
		// 这条正是 M2 验收判据里「found 帖填 last_seen_at」的那一半：
		// API 层要拦下它（本用例），数据库 CHECK 也要拦下它（集成测试里直接 INSERT 验）。
		{"found 多填了 last_seen_at", model.ItemTypeFound, func(f *ItemFields) {
			f.FoundAt = "2026-10-04T09:00:00+08:00"
			f.LastSeenAt = "2026-10-01T08:00:00+08:00"
		}, "last_seen_at"},
		{"found 多填了 lost_at", model.ItemTypeFound, func(f *ItemFields) {
			f.FoundAt = "2026-10-04T09:00:00+08:00"
			f.LostAt = "2026-10-03T08:00:00+08:00"
		}, "lost_at"},
		{"found 合法", model.ItemTypeFound, func(f *ItemFields) {
			f.LastSeenAt = ""
			f.LostAt = ""
			f.FoundAt = "2026-10-04T09:00:00+08:00"
		}, ""},

		// ---- 类型本身 ----
		{"item_type 为空", "", func(f *ItemFields) {}, "item_type"},
		{"item_type 是别的", "stolen", func(f *ItemFields) {}, "item_type"},
		{"item_type 大小写不对", "LOST", func(f *ItemFields) {}, "item_type"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := baseLost()
			tc.mut(&f)
			_, err := normalizeItemFields(tc.itemType, f)
			if tc.field == "" {
				if err != nil {
					t.Fatalf("不该报错：%v", err)
				}
				return
			}
			requireValidation(t, err, tc.field)
		})
	}
}

func TestParseItemTime(t *testing.T) {
	bad := []string{
		"2026-13-01T00:00:00Z",          // 没有 13 月
		"2026-10-07T15:04",              // ⚠ <input type="datetime-local"> 的原样输出，缺秒和时区
		"2026-10-07 15:04:05",           // 空格分隔，不是 RFC3339
		"2026-10-07",                    // 只有日期
		"昨天",                            // 自然语言
		"1759820645",                    // Unix 时间戳
		"2026-10-07T15:04:05+08:00 UTC", // 尾巴多了东西
	}
	for _, raw := range bad {
		t.Run("拒绝 "+raw, func(t *testing.T) {
			_, err := parseItemTime("lost_at", raw)
			requireValidation(t, err, "lost_at")
		})
	}

	// 空串是「这一列写 NULL」，不是错误。found 帖的 lost_at 就是空串。
	got, err := parseItemTime("lost_at", "")
	if err != nil || got != nil {
		t.Fatalf("空串应该返回 (nil, nil)，实际 (%v, %v)", got, err)
	}
	got, err = parseItemTime("lost_at", "   ")
	if err != nil || got != nil {
		t.Fatalf("纯空白应该等同空串，实际 (%v, %v)", got, err)
	}

	// 刻意**不做**宽容解析：接受多种格式意味着同一个时刻有多种写法，
	// 而写错时区的那一种会静默地把时间挪 8 小时，匹配算法随后给出完全错误的结果。
	t.Run("datetime-local 的原样输出必须被拒", func(t *testing.T) {
		if _, err := parseItemTime("found_at", "2026-10-07T15:04"); err == nil {
			t.Fatal("M7 的前端必须先 new Date(v).toISOString()；这里放过去就会静默产生错误时刻")
		}
	})
}

// ---------- validateImagePaths ----------

// validUploadPath / anotherUploadPath 是两条**形状合法且互不相同**的路径。
// 手写字面量而不是调 newPath()：newPath 用 crypto/rand，
// 而测试要的是确定的输入 —— 随机路径会让失败信息每次都不一样。
const (
	validUploadPath   = "2026/10/0123456789abcdef0123456789abcdef.jpg"
	anotherUploadPath = "2026/10/1123456789abcdef0123456789abcdef.jpg"
)

func TestValidateImagePaths(t *testing.T) {
	ok := []struct {
		name  string
		paths []string
	}{
		{"nil", nil},
		{"空数组", []string{}},
		{"一张", []string{validUploadPath}},
		{"九张（上限）", ninePaths()},
		{"png/webp/gif 都收", []string{
			"2026/10/0123456789abcdef0123456789abcdef.png",
			"2026/01/0123456789abcdef0123456789abcdef.webp",
			"2025/12/0123456789abcdef0123456789abcdef.gif",
		}},
	}
	for _, tc := range ok {
		t.Run("接受 "+tc.name, func(t *testing.T) {
			if err := validateImagePaths(tc.paths); err != nil {
				t.Fatalf("不该报错：%v", err)
			}
		})
	}

	// field 一列是「错误应该指向哪个下标」：数量超限是数组整体的问题（image_paths），
	// 形状不合法和重复则必须指到具体那一个 —— 指错了前端会把红框画在合法的图上。
	bad := []struct {
		name  string
		paths []string
		field string
	}{
		{"十张", append(ninePaths(), anotherUploadPath), "image_paths"},
		{"重复路径", []string{validUploadPath, validUploadPath}, "image_paths[1]"},
		// 下面几条是 IsUploadPath 这道防线的意义所在：
		// 这些字符串会被原样写进 item_images.path，之后又被拼成 /uploads/<path>。
		{"路径穿越", []string{"../../.env"}, "image_paths[0]"},
		{"绝对路径", []string{"/etc/passwd"}, "image_paths[0]"},
		{"Windows 反斜杠", []string{"..\\..\\secret.txt"}, "image_paths[0]"},
		{"HTML 注入", []string{"<script>alert(1)</script>"}, "image_paths[0]"},
		{"可执行扩展名", []string{"2026/10/0123456789abcdef0123456789abcdef.php"}, "image_paths[0]"},
		{"大写十六进制", []string{"2026/10/0123456789ABCDEF0123456789ABCDEF.jpg"}, "image_paths[0]"},
		{"少一位十六进制", []string{"2026/10/0123456789abcdef0123456789abcde.jpg"}, "image_paths[0]"},
		{"月份只有一位", []string{"2026/1/0123456789abcdef0123456789abcdef.jpg"}, "image_paths[0]"},
		{"没有日期目录", []string{"0123456789abcdef0123456789abcdef.jpg"}, "image_paths[0]"},
		{"只有扩展名", []string{".jpg"}, "image_paths[0]"},
		{"空串", []string{""}, "image_paths[0]"},
	}
	for _, tc := range bad {
		t.Run("拒绝 "+tc.name, func(t *testing.T) {
			requireValidation(t, validateImagePaths(tc.paths), tc.field)
		})
	}

	// 重复的那条错误要指向**第二个**下标，否则前端会把红框画在第一个（合法的那个）上。
	t.Run("重复时指向后一个下标", func(t *testing.T) {
		err := validateImagePaths([]string{
			"2026/10/0123456789abcdef0123456789abcdef.jpg",
			"2026/10/1123456789abcdef0123456789abcdef.png",
			"2026/10/0123456789abcdef0123456789abcdef.jpg",
		})
		requireValidation(t, err, "image_paths[2]")
	})
}

func ninePaths() []string {
	out := make([]string, 0, maxItemImages)
	for i := 0; i < maxItemImages; i++ {
		out = append(out, validUploadPath[:len(validUploadPath)-len(".jpg")-1]+string(rune('0'+i))+".jpg")
	}
	return out
}

// ---------- contactLocked ----------

// TestContactLocked 钉住计划 §4「#15 的 contact 可见性规则」的前两条。
//
// 这条逻辑是全系统最容易写错的地方之一：写错的后果不是报错，
// 而是**把别人的联系方式公开出去**，而且没有任何测试会红 ——
// 除非它本身就被测到。所以它必须是一个能一眼读完的纯函数 + 一张穷举表。
func TestContactLocked(t *testing.T) {
	owner := &model.User{ID: 7, Role: model.RoleUser}
	other := &model.User{ID: 8, Role: model.RoleUser}
	admin := &model.User{ID: 9, Role: model.RoleAdmin}

	found := &model.Item{ID: 1, ItemType: model.ItemTypeFound, UserID: 7}
	lost := &model.Item{ID: 2, ItemType: model.ItemTypeLost, UserID: 7}

	cases := []struct {
		name   string
		item   *model.Item
		viewer *model.User
		locked bool
	}{
		// lost 帖的联系方式**直接公开**：丢东西的人巴不得被联系上，
		// 而捡到东西的人不想被骚扰。所以只有 found 帖需要解锁。
		{"lost + 未登录", lost, nil, false},
		{"lost + 路人", lost, other, false},
		{"lost + 本人", lost, owner, false},

		{"found + 未登录", found, nil, true},
		{"found + 路人", found, other, true},
		// admin 也不能白看：他要看联系方式得走 #22 的解锁名单，
		// 或者像普通用户一样点「认领并查看」（留下 contact_views 一行）。
		// 「admin 例外」听起来合理，但它会让 admin 的浏览行为不留痕迹，
		// 而定位原则 5 的约束方式恰恰是「每个动作都要查得到」。
		{"found + admin（非本人）", found, admin, true},
		{"found + 本人", found, owner, false},
		// M4 会在这里补上第三条：已登录且 contact_views 有记录 → 不锁。
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := contactLocked(tc.item, tc.viewer); got != tc.locked {
				t.Fatalf("contactLocked = %v，期望 %v", got, tc.locked)
			}
		})
	}
}

// ---------- authorizeItemWrite ----------

func TestAuthorizeItemWrite(t *testing.T) {
	owner := &model.User{ID: 7, Role: model.RoleUser}
	other := &model.User{ID: 8, Role: model.RoleUser}
	admin := &model.User{ID: 9, Role: model.RoleAdmin}
	const ownerID = 7

	cases := []struct {
		name      string
		actor     *model.User
		ownerID   int64
		reason    string
		asAdmin   bool
		wantCode  string // "" = 期望放行
		wantField string
	}{
		{"本人改自己的帖子，不带理由", owner, ownerID, "", false, "", ""},
		// 本人带了理由也**不算** admin 操作：asAdmin=true 会让 M6 往 admin_actions 里写一行
		// 「管理员修改了自己的帖子」，那是一行毫无信息量的噪声。
		{"本人改自己的帖子，带了理由", owner, ownerID, "我改个错别字", false, "", ""},
		// admin 编辑**自己发**的帖子不算 admin 操作：判断的是「帖子是谁的」，
		// 不是「操作者是什么角色」。所以它不需要理由，也不会往 admin_actions 里写一行。
		{"admin 改自己的帖子", admin, admin.ID, "", false, "", ""},
		{"admin 改别人的帖子，有理由", admin, 8, "spam", true, "", ""},
		{"admin 改别人的帖子，没理由", admin, 8, "", false, apperr.CodeValidation, "admin_reason"},
		{"admin 改别人的帖子，理由是纯空白", admin, 8, "   ", false, apperr.CodeValidation, "admin_reason"},
		// 普通用户带上 admin_reason 也**不能**变成有权：
		// 顺序必须是「先判身份再判理由」。反过来的话，一个普通用户随便带上理由
		// 就会收到「理由不合格」而不是「你没权限」，那等于向他确认了这个端点存在 admin 通道。
		{"路人改别人的帖子，带了理由", other, ownerID, "我要改", false, apperr.CodeForbidden, ""},
		{"路人改别人的帖子，不带理由", other, ownerID, "", false, apperr.CodeForbidden, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			asAdmin, err := authorizeItemWrite(tc.actor, tc.ownerID, tc.reason)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("不该报错：%v", err)
				}
				if asAdmin != tc.asAdmin {
					t.Fatalf("asAdmin = %v，期望 %v", asAdmin, tc.asAdmin)
				}
				return
			}
			if err == nil {
				t.Fatalf("期望 %s（asAdmin=%v），实际放行且 asAdmin=%v", tc.wantCode, tc.asAdmin, asAdmin)
			}
			if !apperr.IsCode(err, tc.wantCode) {
				t.Fatalf("期望 code=%s，实际 %v", tc.wantCode, err)
			}
			if asAdmin {
				t.Error("被拒绝的操作不该返回 asAdmin=true")
			}
			if tc.wantField != "" {
				requireValidation(t, err, tc.wantField)
			}
		})
	}
}

// ---------- 列表查询参数 ----------

func TestParseListQueryDefaults(t *testing.T) {
	got, err := parseListQuery(ListQuery{}, publicListStatus)
	if err != nil {
		t.Fatalf("全空参数应该走默认值：%v", err)
	}
	if got.ItemType != "" || got.Keyword != "" {
		t.Errorf("ItemType/Keyword 应该是空的（表示不加筛选）：%+v", got)
	}
	if got.CategoryID != nil || got.LocationID != nil || got.UserID != nil {
		t.Errorf("三个 id 筛选应该是 nil：%+v", got)
	}
	if !equalStrings(got.Statuses, []string{model.ItemStatusOpen}) {
		t.Errorf("广场默认只该看 open，实际 %v", got.Statuses)
	}
	if got.Sort != "created_at" || got.Page != 1 || got.PageSize != 20 {
		t.Errorf("排序/分页默认值不对：%+v", got)
	}

	// #19 我的发布默认要带上 closed：自己的帖子归还之后不能从「我的发布」里消失，
	// 否则用户会以为帖子被删了。
	got, err = parseListQuery(ListQuery{}, mineListStatus)
	if err != nil {
		t.Fatalf("全空参数应该走默认值：%v", err)
	}
	if !equalStrings(got.Statuses, []string{model.ItemStatusOpen, model.ItemStatusClosed}) {
		t.Errorf("我的发布默认该是 open+closed，实际 %v", got.Statuses)
	}
}

func TestParseListQueryFilters(t *testing.T) {
	catID := int64(36)
	q := ListQuery{
		ItemType:   "found",
		Keyword:    "  钱包  ",
		CategoryID: "36",
		LocationID: "50",
		Status:     "closed",
		Sort:       "found_at",
		Page:       "3",
		PageSize:   "50",
	}
	got, err := parseListQuery(q, publicListStatus)
	if err != nil {
		t.Fatalf("不该报错：%v", err)
	}
	if got.ItemType != "found" {
		t.Errorf("ItemType = %q", got.ItemType)
	}
	// keyword 要 trim：用户从别处复制粘贴时带上的空格不该让搜索空手而归。
	if got.Keyword != "钱包" {
		t.Errorf("Keyword 没有 trim：%q", got.Keyword)
	}
	if got.CategoryID == nil || *got.CategoryID != catID {
		t.Errorf("CategoryID = %v", got.CategoryID)
	}
	if got.LocationID == nil || *got.LocationID != 50 {
		t.Errorf("LocationID = %v", got.LocationID)
	}
	if !equalStrings(got.Statuses, []string{model.ItemStatusClosed}) {
		t.Errorf("Statuses = %v", got.Statuses)
	}
	if got.Sort != "found_at" || got.Page != 3 || got.PageSize != 50 {
		t.Errorf("排序/分页不对：%+v", got)
	}
}

func TestParseListQueryRejects(t *testing.T) {
	cases := []struct {
		name   string
		q      ListQuery
		policy statusPolicy
		field  string
	}{
		{"item_type 是别的", ListQuery{ItemType: "stolen"}, publicListStatus, "item_type"},
		{"item_type 大小写不对", ListQuery{ItemType: "FOUND"}, publicListStatus, "item_type"},
		{"category_id 不是数字", ListQuery{CategoryID: "abc"}, publicListStatus, "category_id"},
		{"category_id 是 0", ListQuery{CategoryID: "0"}, publicListStatus, "category_id"},
		{"category_id 是负数", ListQuery{CategoryID: "-3"}, publicListStatus, "category_id"},
		{"location_id 不是数字", ListQuery{LocationID: "x"}, publicListStatus, "location_id"},
		{"sort 是别的", ListQuery{Sort: "title"}, publicListStatus, "sort"},
		// sort 会被拼进 SQL 的 ORDER BY，所以它必须走白名单。
		// 拒绝一个注入尝试时返回的是 VALIDATION 而不是 500 —— 用户输入永远不该产生 500。
		{"sort 是注入尝试", ListQuery{Sort: "created_at; DROP TABLE items"}, publicListStatus, "sort"},
		{"page 是 0", ListQuery{Page: "0"}, publicListStatus, "page"},
		{"page 是负数", ListQuery{Page: "-1"}, publicListStatus, "page"},
		{"page 不是数字", ListQuery{Page: "abc"}, publicListStatus, "page"},
		{"page_size 是 0", ListQuery{PageSize: "0"}, publicListStatus, "page_size"},
		{"page_size 超过上限", ListQuery{PageSize: "101"}, publicListStatus, "page_size"},
		// page_size 不设上限的话，一个人请求 page_size=1000000 就能让数据库
		// 一次性把整张表连表查出来 —— 那是最便宜的 DoS。
		{"page_size 是天文数字", ListQuery{PageSize: "99999999"}, publicListStatus, "page_size"},

		// deleted 在广场**一律不允许**：软删的帖子对广场来说就是不存在。
		// 让任何人（包括发帖人自己）通过广场列表看到「这里有一条被删的帖子」，
		// 等于把治理动作公开化了。
		{"广场请求 deleted", ListQuery{Status: "deleted"}, publicListStatus, "status"},
		{"广场请求 bogus 状态", ListQuery{Status: "archived"}, publicListStatus, "status"},
		// 但「我的发布」允许：自己的帖子被 admin 下架之后应该看得见，
		// 否则用户会以为帖子凭空消失了。
		{"我的发布请求 deleted（应该允许）", ListQuery{Status: "deleted"}, mineListStatus, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseListQuery(tc.q, tc.policy)
			if tc.field == "" {
				if err != nil {
					t.Fatalf("不该报错：%v", err)
				}
				return
			}
			requireValidation(t, err, tc.field)
		})
	}
}

func TestParseListQueryStatusesAreNotShared(t *testing.T) {
	// slices.Clone 的存在理由：Default 是包级变量，如果不 clone 就把它交给
	// repo，那么任何一次对返回切片的 append/sort 都会污染这个变量 ——
	// 于是「第一个请求之后广场的默认状态变成了别的」，而这类 bug 只在
	// 请求顺序特定时才复现，是最难查的一类。
	a, err := parseListQuery(ListQuery{}, publicListStatus)
	if err != nil {
		t.Fatal(err)
	}
	a.Statuses[0] = "hacked"

	b, err := parseListQuery(ListQuery{}, publicListStatus)
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(b.Statuses, []string{model.ItemStatusOpen}) {
		t.Fatalf("包级的默认值被上一次调用污染了：%v", b.Statuses)
	}
}

func TestParseIntParam(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		def         int
		min, max    int
		want        int
		wantInvalid bool
	}{
		{"空串走默认值", "", 20, 1, 100, 20, false},
		{"纯空白走默认值", "   ", 20, 1, 100, 20, false},
		{"下界", "1", 20, 1, 100, 1, false},
		{"上界", "100", 20, 1, 100, 100, false},
		{"低于下界", "0", 20, 1, 100, 0, true},
		{"高于上界", "101", 20, 1, 100, 0, true},
		{"带空格的正整数", " 7 ", 20, 1, 100, 7, false},
		{"小数", "1.5", 20, 1, 100, 0, true},
		// strconv.Atoi 接受前导 +，所以 ?page_size=+7 和 ?page_size=7 等价。
		// 这不是 bug，也不值得专门去禁 —— 但行为要被记录下来，
		// 否则将来有人「顺手严格化」时不会意识到自己改了一个既有契约。
		{"带加号", "+7", 20, 1, 100, 7, false},
		{"溢出", "99999999999999999999", 20, 1, 100, 0, true},
		{"负号", "-7", 20, 1, 100, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseIntParam("page_size", tc.raw, tc.def, tc.min, tc.max)
			if tc.wantInvalid {
				requireValidation(t, err, "page_size")
				return
			}
			if err != nil {
				t.Fatalf("不该报错：%v", err)
			}
			if got != tc.want {
				t.Fatalf("得到 %d，期望 %d", got, tc.want)
			}
		})
	}
}

// ---------- 辅助 ----------

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// truncateForName 把过长的用例名截短，否则 t.Run 的名字会占满一屏。
// 按 **rune** 截而不是按字节：用例名里有中文，s[:24] 可能正好切在一个汉字的中间，
// 产出一段无效 UTF-8，测试输出里就是一片问号。
func truncateForName(s string) string {
	const max = 24
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}

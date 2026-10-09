// 写入演示数据：与 Node 版 scripts/seed.js 等价，幂等可重复执行。
package main

import (
	"fmt"
	"os"

	"lostfound/internal/bootstrap"
	"lostfound/internal/config"
	"lostfound/internal/database"
	"lostfound/internal/store"
)

type demoUser struct {
	StudentID string
	Password  string
	Name      string
	Contact   string
}

type demoItem struct {
	Owner       string
	Audit       bool
	Type        string
	Title       string
	Category    string
	Location    string
	HappenedAt  string
	Description string
}

var demoUsers = []demoUser{
	// 学号固定 8 位数字，与 Node 版 scripts/seed.js 完全一致，保证两版指向同一个库时不会出现两套用户。
	{StudentID: "20230001", Password: "123456", Name: "张三", Contact: "zhangsan@example.com"},
	{StudentID: "20230002", Password: "123456", Name: "李四", Contact: "138-0000-0002"},
	{StudentID: "20230003", Password: "123456", Name: "王五", Contact: "wangwu@example.com"},
}

var demoItems = []demoItem{
	{
		Owner: "20230001", Audit: true, Type: "lost", Title: "丢失一个黑色长款钱包", Category: "钱包箱包",
		Location: "图书馆三楼自习区", HappenedAt: "2026-09-12 15:30",
		Description: "黑色皮质长款钱包，内有一张校园卡、一张银行卡和少量现金。捡到请联系，必有酬谢。",
	},
	{
		Owner: "20230002", Audit: true, Type: "found", Title: "拾到学生证一张（姓名李四）", Category: "证件卡类",
		Location: "第二食堂门口", HappenedAt: "2026-09-13 12:10",
		Description: "在第二食堂门口台阶上拾到学生证一张，已交至食堂前台，请失主携带身份证件前往认领。",
	},
	{
		Owner: "20230003", Audit: true, Type: "lost", Title: "寻一副白色蓝牙耳机", Category: "电子产品",
		Location: "体育馆篮球场", HappenedAt: "2026-09-14 19:00",
		Description: "白色入耳式蓝牙耳机，充电盒背面贴有一张卡通贴纸，大概是打球时落在场地边的。",
	},
	{
		Owner: "20230001", Audit: true, Type: "found", Title: "捡到一串钥匙（带小熊挂件）", Category: "钥匙",
		Location: "校医院一楼走廊", HappenedAt: "2026-09-15 09:20",
		Description: "银色钥匙串，上面挂着一个棕色小熊挂件，共三把钥匙。现暂存于校医院导诊台。",
	},
	{
		Owner: "20230002", Audit: true, Type: "lost", Title: "丢失蓝色折叠雨伞", Category: "其他",
		Location: "教学楼B座201教室", HappenedAt: "2026-09-16 17:40",
		Description: "深蓝色折叠雨伞，伞柄处有明显褪色划痕，伞面内侧写有名字缩写。",
	},
	{
		Owner: "20230003", Audit: true, Type: "found", Title: "拾到笔记本电脑充电器一个", Category: "电子产品",
		Location: "图书馆一楼自习室", HappenedAt: "2026-09-17 20:05",
		Description: "65W 笔记本充电器，接口为 Type-C，线材上缠有黑色魔术贴。已暂存图书馆一楼服务台。",
	},
	{
		Owner: "20230001", Audit: true, Type: "lost", Title: "寻找一条灰色针织围巾", Category: "衣物鞋帽",
		Location: "北门公交站", HappenedAt: "2026-09-18 08:15",
		Description: "浅灰色针织围巾，一端有手工缝制的姓名标签，天气转凉急用，请好心人联系。",
	},
	{
		Owner: "20230002", Audit: true, Type: "found", Title: "看台上遗落的运动水杯", Category: "运动器材",
		Location: "田径场东侧看台", HappenedAt: "2026-09-19 16:30",
		Description: "不锈钢保温运动水杯，杯身贴有校运会贴纸，容量约 600ml。",
	},
	{
		Owner: "20230003", Audit: false, Type: "lost", Title: "丢失一个白色移动电源", Category: "电子产品",
		Location: "实验楼四楼机房", HappenedAt: "2026-09-20 14:00",
		Description: "白色 10000mAh 移动电源，背面有轻微磕碰痕迹。（此条为待审核示例）",
	},
}

func main() {
	cfg := config.Load()

	db, err := database.Open(cfg.DBFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[fatal]", err)
		os.Exit(1)
	}
	defer db.Close()

	s := store.New(db)

	fmt.Println("开始写入演示数据...")
	fmt.Println()

	admin, err := bootstrap.EnsureDefaultAdmin(s, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[fatal]", err)
		os.Exit(1)
	}
	state := "已存在"
	if admin.Created {
		state = "已创建"
	}
	fmt.Printf("  管理员账号 %s %s\n", admin.StudentID, state)

	owners := map[string]*store.UserRow{}
	for _, demo := range demoUsers {
		user, err := s.FindUserByStudentID(demo.StudentID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "[fatal]", err)
			os.Exit(1)
		}
		if user == nil {
			user, err = s.CreateUser(demo.StudentID, demo.Password, demo.Name, demo.Contact, "user")
			if err != nil {
				fmt.Fprintln(os.Stderr, "[fatal]", err)
				os.Exit(1)
			}
			fmt.Printf("  创建用户 %s（%s，口令 %s）\n", demo.StudentID, demo.Name, demo.Password)
		}
		owners[demo.StudentID] = user
	}

	existing := map[string]bool{}
	page, err := s.ListItems(store.ListOptions{Page: 1, PageSize: 200})
	if err != nil {
		fmt.Fprintln(os.Stderr, "[fatal]", err)
		os.Exit(1)
	}
	for _, item := range page.Items {
		existing[item.Title] = true
	}

	created := 0
	for _, demo := range demoItems {
		if existing[demo.Title] {
			continue
		}
		owner := owners[demo.Owner]

		item, err := s.CreateItem(owner.ID, store.ItemInput{
			Type:        demo.Type,
			Title:       demo.Title,
			Category:    demo.Category,
			Description: demo.Description,
			Location:    demo.Location,
			HappenedAt:  demo.HappenedAt,
			Contact:     owner.Contact,
			ImageURL:    "",
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "[fatal]", err)
			os.Exit(1)
		}
		if demo.Audit {
			if _, err := s.AuditItem(item.ID, "approve", ""); err != nil {
				fmt.Fprintln(os.Stderr, "[fatal]", err)
				os.Exit(1)
			}
		}
		created++
	}

	fmt.Println()
	fmt.Printf("完成：新增 %d 条失物/招领信息。\n", created)
	fmt.Println("管理员后台：/admin（10000000 / admin123）")
}

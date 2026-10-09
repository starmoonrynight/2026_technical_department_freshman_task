# 架构与学习说明

## 谁负责什么

```text
浏览器（web/）
  → 路由与安全头（routes.go）
    → Handler：解析 HTTP、判断登录、组织 JSON
      → ItemService：类型、图片、状态等业务规则
        → ItemRepository：SQL、事务、图片关联、创建任务

后台 worker（ai_jobs.go）
  → 宽泛搜索（search.go）
  → VisionClient 接口
    → DeepSeekClient（ai_client.go）：HTTP、base64 图片、JSON 响应
  → 比较结果缓存 / matches / notifications
    → NotificationHandler → 浏览器消息中心
```

没有添加新的 Go 外部依赖，继续使用已有 SQLite 驱动和 bcrypt。新增功能用标准库实现；也没有为每个文件创建一个 package。全部 `package main` 的文件仍由 `go run .` 一起编译，文件边界先按职责拆分。

| 文件 | 职责 |
| --- | --- |
| main.go | 装配对象、启动 HTTP 与后台 worker、优雅停止 |
| config.go | 从环境变量读取配置；Key 不进入前端 |
| database.go | 数据库连接、版本迁移、迁移前一致性备份 |
| models.go | HTTP 请求/响应、帖子、图片、任务、通知的数据形状 |
| auth.go | 注册、bcrypt 密码验证、Cookie 会话、登录校验 |
| item_handler.go | 帖子 API 与 HTTP 错误映射 |
| item_service.go | 校验、权限和状态转换规则 |
| item_repository.go | 帖子读写、图片关联、事务、软删除 |
| search.go | 三档搜索词扩展及权重 |
| media.go | 实际图片解码、限制、元数据清除、草稿访问权限 |
| ai_client.go | 第三方模型协议；不参与数据库业务 |
| ai_jobs.go | 持久化任务、有限重试、初筛、模型精筛、通知入库 |
| notifications.go | 用户自己的消息列表与已读标记 |
| web/ | 同源原生前端；动态用户内容用 textContent，不插入 HTML |

## main 的运行顺序

main 顺序执行配置读取、打开数据库、构建图片服务/帖子服务/模型适配器、注册路由。然后启动两个受控 goroutine：HTTP 服务与后台 worker。HTTP 处理函数不会因为“写在文件里”就一直运行，只有相应请求到达时才被调用。

worker 每秒检查是否有待处理任务；没有 Key 时不会认领任务。Ctrl+C 后 HTTP 停止接收请求，worker 的 context 被取消，等待 goroutine 退出后才关闭数据库。

## 两条异步流程

识图：上传图片 → 校验归属与授权 → 写入 extract 任务 → 返回任务 ID → 浏览器轮询 → 识图结果保存为建议 → 用户修改并确认发布。

自动匹配：用户发布/编辑 → 一个事务内保存帖子、关联图片、写入 match 任务 → 提交后返回 → worker 初筛候选 → 调用 AI → 再检查双方版本/状态/授权 → 一个事务内保存匹配与通知。

写入任务与帖子是一个事务，因此不会出现帖子已成功保存、任务却因进程崩溃彻底丢失的中间状态。模型请求一定在事务外执行，防止 SQLite 连接被长时间占住。

## SQLite 和任务可靠性

- 只有一个数据库连接。读出多行后必须关闭 rows，再执行查询图片等嵌套查询，否则连接可能一直等自己释放。
- 数据表从 schema_migrations 版本管理开始升级。旧数据不会自动被发送给 AI。
- 帖子的 revision 在编辑、状态变化、删除时递增。模型等待期间帖子改变，旧结果不能写入有效通知。
- ai_jobs 的 running 任务有 5 分钟租约；单次执行限制 2 分钟。进程崩溃后租约到期可以重领，最多 3 次尝试。
- 正负比较结果按“双方帖子 ID + 双方版本”缓存，通知由数据库唯一约束防重复。重试最多只能重复外部调用，不能保证供应商端 exactly-once 计费。
- 日额度通过数据库原子 UPSERT 累加，不仅是进程内变量；重启不能重置额度。按照保守原则，发出调用前就占用一次额度。
- 删除帖子采用软删除，保留历史匹配引用，但 API/搜索不再返回已删除帖子。

## 为什么使用接口

`VisionClient` 只规定 `Extract` 与 `Compare` 两个方法。任务流程只需要知道“能识图、能比较”，不需要知道 DeepSeek 的 URL、请求头或图片编码方法。正式运行交给 DeepSeekClient，测试交给 mockVision；因此可以测试匹配业务而不付模型费用。更换供应商也不需要重写帖子的权限与 SQL。

## 当前刻意保持简单的地方

单服务、单 SQLite、单 worker、本地图片、前端每 30 秒轮询通知；没有引入微服务、Redis、消息队列或向量库。只有数据量、并发或部署需求真的增长后，再分别替换存储、任务实现和搜索索引。

本地词表只覆盖常见校园物品，不承诺任意语义召回；真实 AI 判断需要样本验证。图片目前保留在本地，没有自动清理，以免删除用户仍需使用的数据。

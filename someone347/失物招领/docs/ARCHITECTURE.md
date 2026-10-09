# 代码与架构说明
## 架构
浏览器通过同源 HTTP 调用后端。前端原生 ES Modules；后端 Node.js 原生 HTTP；SQLite 保存用户、帖子、会话、消息和 Session，图片文件放在服务器本地。没有运行时第三方依赖。
```text
浏览器 frontend/app.js
  └─ lib/api.js：Cookie + CSRF + 统一异常
       └─ backend/app.mjs：路由、JSON、来源检查
            ├─ AuthService：scrypt密码、Session、登录限流
            ├─ PostService：搜索、发布、版本检查、状态机
            ├─ ImageService：文件签名、大小、上传与读取权限
            └─ MessageService：参与者权限、去重、游标与未读
                 └─ SQLite app.sqlite + uploads/
```
## 数据关系
| 表 | 关键字段/约束 | 用途 |
| --- | --- | --- |
| users | account UNIQUE | 账号、昵称、带盐密码哈希 |
| sessions | token_hash PRIMARY KEY，user_id FK | 服务端会话、CSRF、过期时间 |
| campuses | id PRIMARY KEY | 校区字典 |
| posts | owner_id/campus_id FK，version | 寻物/招领、状态、软删除 |
| images / post_images | owner_id FK；post_id+position UNIQUE | 图片归属和排列 |
| post_requests | user_id+request_key PRIMARY KEY | 发帖幂等与内容摘要 |
| conversations | post_id+initiator_id+owner_id UNIQUE | 一个物品对应双方的唯一会话 |
| messages | sender_id+client_message_id UNIQUE | 消息去重，整数递增ID作为游标 |
| conversation_reads | conversation_id+user_id PRIMARY KEY | 双方各自的已读位置 |
| schema_migrations | version PRIMARY KEY | 初始化与旧数据导入记录 |
完整 DDL 见 [schema.sql](../backend/db/schema.sql)。帖子与消息保留关联，删除帖子只设置 deleted_at。关联图片在帖子删除后不再公开，上传者仍能访问。
## 核心流程
**发布：** 校验登录/CSRF→验证字段与图片归属→BEGIN IMMEDIATE→检查幂等键及内容摘要→写帖子和图片关联→保存幂等记录→COMMIT。相同键不同内容返回409。
**编辑/状态：** 在同一事务内校验作者和version→执行更新→version递增。两次并发编辑只有一个成功，另一个409。状态仅允许 OPEN→RESOLVED/CLOSED，RESOLVED/CLOSED→OPEN。
**搜索：** 参数化 SQL，WHERE 组合类型、关键词、类别、校区、状态、日期；名称/描述用 LIKE；按 created_at DESC,id DESC 排序；LIMIT/OFFSET 分页。转义LIKE通配符，避免用户输入%匹配所有内容。
**聊天：** 会话双方权限检查→sender_id取Session→按客户端消息ID去重→消息和最近活动时间在事务内写入。每5秒拉取after_id后的新消息；before_id查询更早记录。所有结果按ID升序展示。
**已读：** 只有属于当前会话的消息ID才有效；数据库用MAX保持只前移。未读为“对方发送、ID大于我的已读位置”的消息数。
## 前端
Hash路由：首页 /、详情 /posts/:id、发布 /posts/new、编辑 /posts/:id/edit、登录 /login、注册 /register、我的 /me、会话 /messages、聊天 /messages/:id。地址栏实际为 /#/...。
renderId 防止旧请求覆盖新页面；列表按ID去重；聊天支持失败重试和历史分页。发布草稿在当前标签页按用户隔离存入sessionStorage，编辑草稿保存在内存。失效登录保留草稿，登录相同账号后恢复。刷新未保存的编辑内容仍可能丢失，离开前会提示。
## 认证与安全
- scrypt+随机盐存储密码；异步计算，接口不返回哈希。
- Session令牌只存SHA-256摘要，Cookie为HttpOnly、SameSite=Lax；生产模式加Secure。
- 写接口校验Origin（浏览器提供时）和X-CSRF-Token。JSON体最大15MiB。
- 所有作者/参与者权限在后端检查，owner_id和sender_id取会话身份。
- 图片检查JPEG/PNG签名及10MiB限制；随机文件名，未发布图片只给上传者读取。
- 所有用户文本经HTML转义；静态目录限制路径边界；数据库使用参数绑定。
- 登录/注册同IP每10分钟最多15次；重启会重置此计数。
## 配置与运维
| 变量 | 默认值 | 含义 |
| --- | --- | --- |
| HOST / PORT | 127.0.0.1 / 4173 | 监听地址及端口 |
| DATA_DIR | 项目/data | 数据库和上传文件目录 |
| NODE_ENV | 非production | production禁用自动演示种子并启用Secure Cookie |
| SEED_DEMO | true | 开发环境是否生成示例账号 |
| PUBLIC_ORIGIN | 当前HTTP Host | HTTPS反向代理需设置为外部完整Origin |
| SECURE_COOKIE | false（开发） | true强制Secure Cookie |
| LOGIN_LIMIT | 15 | 每IP十分钟认证请求上限 |
不使用反向代理传来的任意IP作为限流依据；代理部署的限流需在入口按真实客户端配置。本项目使用同步SQLite短事务，适合单进程作业服务；高并发场景应另做负载测试与架构调整。
## 数据初始化与迁移
首次启动创建表、外键及索引，开启WAL。旧JSON仅在数据库空且未导入时迁移，原文件保持不变。后续注册、帖子、图片、消息和Session均写SQLite。重启测试验证不会重复导入。数据库结构升级应新增编号迁移，不能靠覆盖schema.sql修改已有数据库。

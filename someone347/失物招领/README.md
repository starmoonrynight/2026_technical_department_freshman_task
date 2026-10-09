# 杭电失物招领
按杭助作业和最新 PRD 实现的前后端项目。支持电脑和手机，数据真实保存，两个账号可互相聊天。
## 启动
需要 **Node.js 24.14 或更新版本**，无需安装运行时依赖或单独配置数据库。
```powershell
cd someone347/失物招领
npm start
```
访问 http://127.0.0.1:4173 。Windows 可双击「启动Demo.cmd」。首次启动自动创建 SQLite 表和演示数据；不要直接双击 HTML。
| 账号 | 密码 | 昵称 |
| --- | --- | --- |
| demo | demo12345 | 小杭 |
| xiaoming | demo12345 | 小明 |
也可注册新账号。使用普通窗口和无痕窗口分别登录，演示双方聊天。
## 已实现
- P0：注册、登录、退出；发布、详情、编辑、软删除；关键词搜索、校区/类别/状态筛选、分页；完成、关闭、重新开启；站内消息、历史分页、未读数、失败重试。
- P1：最多3张 JPG/PNG、图片预览、日期范围筛选、保管地点。
- 首页保留「招领信息/寻物信息」标签，搜索在标签下方。暂不接学校认证，未实现 AI 匹配与订阅提醒。
## 文档
| 文件 | 内容 |
| --- | --- |
| [产品 PRD](docs/PRD.md) | 最新飞书快照、页面结构、功能优先级及验收标准 |
| [作业对照](docs/ASSIGNMENT.md) | 每项作业要求对应代码和验证方式 |
| [接口说明](docs/API.md) | 认证、请求示例、错误及业务约束 |
| [OpenAPI](docs/openapi.json) | 可导入 Apifox/Postman 的完整接口定义 |
| [代码与架构说明](docs/ARCHITECTURE.md) | 分层、表关系、关键流程、运行配置 |
| [演示步骤](docs/DEMO.md) | 双账号演示及测试方式 |
## 目录
```text
frontend/                HTML、CSS、原生 JavaScript ES Modules
  app.js                 路由、页面渲染和交互
  lib/                   API封装、状态、工具、图标和物品插图
backend/
  app.mjs                HTTP路由、请求校验、静态文件服务
  config.mjs             环境变量配置
  services/              auth / posts / images / messages
  db/schema.sql          表结构、外键、索引和唯一约束
  db/connection.mjs      事务、数据库初始化、旧数据导入
  db/seed.mjs            演示数据
  lib/                   错误、校验、密码工具
server.mjs               启动入口
tests/                   接口、边界和浏览器流程测试
docs/                    PRD、接口、架构、验收和设计图
data/                    运行数据（不提交版本库）
```
## 测试
```powershell
npm test
```
使用隔离临时目录，覆盖账号与权限、SQL搜索、并发版本、幂等、消息游标与已读、图片权限、迁移和重启持久化。浏览器测试见 [演示步骤](docs/DEMO.md)。
## 数据与配置
数据位于 `data/app.sqlite`，图片位于 `data/uploads/`。旧版 `data/store.json` 首次自动导入且保留原文件；之后以 SQLite 为准。登录会话保存于数据库，重启后仍有效，默认24小时。
复制 `.env.example` 为 `.env` 可改端口、数据目录等。默认只监听本机。正式环境使用 `NODE_ENV=production`、全新数据目录、HTTPS 和正确的 `PUBLIC_ORIGIN`；生产模式不生成演示账号。启用 Secure Cookie 后必须通过 HTTPS 访问。
备份时先停服务，再复制整个 data 目录（含 SQLite 文件及图片）。请勿提交 data、.env 或真实账号数据。Node 24 的内置 SQLite 可能显示实验性提示，本项目已在 Node 24.14.0 验证。
## 本次范围
面向作业与小规模单实例运行；图片存本地、消息每5秒轮询。登录限流为进程内计数，未上传成功发布的图片暂无自动清理；尚未提供管理后台、找回密码、内容审核、学校认证及多实例部署。

页面图片统一在 docs/images：页面设计、桌面首页、手机首页、站内聊天，共4张。浏览器测试截图默认临时生成，测试结束自动清理。

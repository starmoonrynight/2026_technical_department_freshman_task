# 作业要求与实现对照
依据：[杭助作业](https://hdu-help.feishu.cn/wiki/OQsiwXHufi0GECkEHIZcoVDqnje)。原文快照见 [assignment-source.md](assignment-source.md)。
| 要求 | 实现位置 | 验证 |
| --- | --- | --- |
| 注册、登录、当前用户、退出 | backend/services/auth.mjs；前端登录/注册页 | API测试；登录后返回原操作 |
| 发布、查看、编辑、删除自己的帖子 | backend/services/posts.mjs；发布/详情/我的页 | 完整生命周期；越权403；软删除404 |
| 查看和搜索他人信息 | PostService.list；首页标签及搜索栏 | SQL WHERE/LIKE/ORDER BY/LIMIT/OFFSET |
| 标记已找回/已归还 | mutate status；状态弹窗 | 完成、关闭、重新开启；版本冲突409 |
| 图片（加分） | images.mjs；上传及预览 | 大小/类型校验、归属校验、删除后不可公开读 |
| 前后端代码 | frontend/、backend/、server.mjs | npm start 启动 |
| API文档 | docs/openapi.json、docs/API.md | 可导入Apifox；路径与实际路由检查 |
| 简单代码文档 | README.md、docs/ARCHITECTURE.md | 分层、数据库、核心流程、配置 |
| 演示视频（可选） | docs/DEMO.md提供脚本 | 未录制视频 |
| AI推荐（可选） | P2后续范围 | 未实现 |
PRD新增的站内对话已实现：双方权限、发送去重、未读数、历史分页及5秒增量轮询。学校统一认证依用户最新决定暂不接入。
提交时包含源代码、tests、docs、package.json、启动脚本及.env.example；排除data、.env、node_modules。通过个人Fork向作业仓库提交PR；未部署公网服务。

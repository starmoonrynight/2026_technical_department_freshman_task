# API 说明

成功响应为 `{"code":0,"message":"...","data":...}`；失败使用标准 HTTP 状态码及对应 code。登录 Cookie 名为 session；跨源前端不是本项目当前部署方式。

| 方法 | 路径 | 登录 | 用途 |
| --- | --- | --- | --- |
| POST | /api/register | 否 | username、password 注册 |
| POST | /api/login | 否 | 登录并设置 HttpOnly Cookie |
| POST | /api/logout | 否 | 撤销当前会话 |
| GET | /api/me | 是 | 当前用户 |
| GET | /api/config | 否 | AI 是否启用、图片限制、搜索档位 |
| GET | /api/health | 否 | 服务健康 |
| POST | /api/media | 是 | multipart/form-data，字段 image，一次一张 |
| GET / HEAD | /api/media/{id}/content | 见下文 | 图片内容 |
| POST | /api/items | 是 | 创建帖子 |
| GET | /api/items | 否 | 搜索、筛选和分页；不含联系方式 |
| GET | /api/items/{id} | 否 | 详情；登录才返回 contact |
| PUT | /api/items/{id} | 发布者 | 更新帖子 |
| DELETE | /api/items/{id} | 发布者 | 软删除 |
| PATCH | /api/items/{id}/status | 发布者 | 状态变更 |
| POST | /api/ai/extract | 是 | 识图任务，需要 consent=true |
| GET | /api/ai/jobs/{id} | 任务所属用户 | 任务状态与结果 |
| GET | /api/notifications | 是 | 自己的最新 100 条消息与全部未读数量 |
| PATCH | /api/notifications/{id}/read | 消息所属用户 | 已读 |

草稿图片只有上传者可以读取；关联到未删除帖子后的图片公开。图片 ID 只能由上传者关联，且同一图片只能属于一个帖子。最多三张，新建找到帖至少一张。

## 发帖 JSON

```json
{
  "type": "found",
  "name": "校园卡",
  "location": "图书馆二楼",
  "description": "黑色卡套，带金属扣",
  "category": "卡片",
  "color": "黑色",
  "brand": "",
  "tags": ["卡套", "金属扣"],
  "contact": "",
  "occurred_at": 0,
  "image_ids": [1],
  "allow_ai": true
}
```

type 只能是 lost / found。occurred_at 使用 Unix 秒，0 表示未知。allow_ai=true 表示已经获得用户向 DeepSeek 发送图片与物品描述的明确同意；不通过请求提供 user_id。

PUT 使用同样结构，所有文本/特征/授权字段整体替换；省略 image_ids 可保留旧图片，空数组表示移除全部图片（找到帖不允许）。status 通过单独接口修改，不能在 PUT 中跳级。

状态请求：

```json
{"status":"found"}
```

状态链：searching → found → closed。type=found 是“找到帖”，status=found 是“已取得联系”，二者不同。

## 搜索

```text
GET /api/items?q=校园卡&breadth=broad&type=found&status=searching&page=1&page_size=12
```

默认 breadth=standard、page=1、page_size=20；省略 type/status 表示不筛选。breadth 可为 strict / standard / broad。page_size 最大100，page 最大1000000。关键词是字面文本，% / _ 不是 SQL 通配符。返回 items、total、page、page_size。

## AI

```json
{"media_id":1,"consent":true}
```

POST /api/ai/extract 返回 HTTP 202 和任务 ID。相同图片复用现有任务；终止失败时可重新上传照片创建新任务。

GET /api/ai/jobs/{id} 的 data 含 job、result、ai_enabled。job.status 为 pending / running / succeeded / failed；识图成功 result 含 name/category/color/brand/description/tags。等待额度时 status 仍为 pending，error 说明原因。

没有配置 Key 时，识图返回 503。配置无效或供应商暂时故障时由后台有限重试；不会返回伪造成功结果。真实接口连接仍需部署者提供 Key 联调。

## 消息

返回 notifications 数组和 unread。每条通知包含 lost_id、found_id、found_name、score、reasons、conflicts、uncertainty、available、read_at、created_at。可用入口为 `/?item=found_id`，前端自动打开帖子详情。多个候选为多条消息。

score 是相关度，不是归属概率。available=false 表示任一帖子已删除、结束、改版或撤销 AI 授权；消息保留为历史，不把过时结果作为有效匹配。

## 常见状态码

400：JSON/字段/图片/分页等校验失败；401：未登录或会话过期；403：操作他人的帖子；404：不存在或无权查看的草稿/任务/通知；405：方法不支持；409：用户名冲突或状态转换冲突；429：上传/发布/AI 任务过于频繁，或图片处理繁忙；503：AI 未配置；500：内部错误。

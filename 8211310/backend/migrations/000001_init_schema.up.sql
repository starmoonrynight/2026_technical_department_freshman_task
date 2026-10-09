-- 失物招领系统 全量建表迁移
-- 12 张表 + 索引 + 55 行分类 + 91 行地点
--
-- 约定（见计划 §3.1）：
--   主键     BIGSERIAL 自增整数
--   时间     timestamptz，一律存 UTC
--   枚举     VARCHAR + CHECK（不用 PG 原生 ENUM，加值要 ALTER TYPE 太麻烦）
--   软删除   status='deleted'（不物理删，保留历史）
--   字典树   自引用（parent_id 指向同表）

CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ============================================================
-- 1. users
-- ============================================================
CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    username      VARCHAR(32)  NULL,                       -- 本地账号才有；SSO 用户为 NULL
    password_hash VARCHAR(100) NULL,                       -- 本地账号才有；SSO 用户没有密码
    auth_source   VARCHAR(16)  NOT NULL DEFAULT 'local'
                  CHECK (auth_source IN ('local','hduhelp')),
    sso_user_id   VARCHAR(64)  NULL,                       -- 杭电助手全局 userId
    real_name     VARCHAR(32)  NULL,
    student_id    VARCHAR(32)  NULL,
    avatar_url    VARCHAR(500) NULL,
    nickname      VARCHAR(32)  NOT NULL DEFAULT '',
    role          VARCHAR(16)  NOT NULL DEFAULT 'user'
                  CHECK (role IN ('user','admin')),
    status        VARCHAR(16)  NOT NULL DEFAULT 'active'
                  CHECK (status IN ('active','banned')),
    phone         VARCHAR(20)  NULL,
    email         VARCHAR(128) NULL,
    credit_score  INT          NOT NULL DEFAULT 100
                  CHECK (credit_score BETWEEN 0 AND 200),
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),

    -- 本地账号必须有用户名和密码；SSO 账号必须有 sso_user_id
    CONSTRAINT users_auth_local CHECK (
        auth_source <> 'local' OR (username IS NOT NULL AND password_hash IS NOT NULL)
    ),
    CONSTRAINT users_auth_sso CHECK (
        auth_source <> 'hduhelp' OR sso_user_id IS NOT NULL
    ),
    -- 不存杭电助手的 accessToken/refreshToken（计划 §7.4）
    CONSTRAINT users_username_len  CHECK (username    IS NULL OR char_length(username)    BETWEEN 3 AND 32),
    CONSTRAINT users_nickname_len  CHECK (char_length(nickname) <= 32)
);

-- 部分唯一索引：NULL 本来就不参与唯一性判断，加 WHERE 是为了让意图显式
CREATE UNIQUE INDEX uq_users_username  ON users(username)    WHERE username    IS NOT NULL;
CREATE UNIQUE INDEX uq_users_sso       ON users(sso_user_id) WHERE sso_user_id IS NOT NULL;

-- ============================================================
-- 2. categories —— 两级自引用树
-- ============================================================
CREATE TABLE categories (
    id         BIGSERIAL PRIMARY KEY,
    parent_id  BIGINT      NULL REFERENCES categories(id),
    name       VARCHAR(32) NOT NULL,
    level      INT         NOT NULL CHECK (level IN (1,2)),
    sort_order INT         NOT NULL DEFAULT 0,
    is_active  BOOLEAN     NOT NULL DEFAULT true,
    CONSTRAINT categories_parent_level CHECK (
        (level = 1 AND parent_id IS NULL) OR
        (level = 2 AND parent_id IS NOT NULL)
    ),
    CONSTRAINT categories_name_len CHECK (char_length(name) >= 1)
);

CREATE UNIQUE INDEX uq_categories_sibling ON categories(COALESCE(parent_id, 0), name);
CREATE INDEX idx_categories_parent ON categories(parent_id);

-- ============================================================
-- 3. locations —— 三级自引用树
--    is_freeform=true 标记「其他」这类没有叶子、要用户自己输入文本的节点
-- ============================================================
CREATE TABLE locations (
    id          BIGSERIAL PRIMARY KEY,
    parent_id   BIGINT      NULL REFERENCES locations(id),
    name        VARCHAR(64) NOT NULL,
    level       INT         NOT NULL CHECK (level IN (1,2,3)),
    sort_order  INT         NOT NULL DEFAULT 0,
    is_active   BOOLEAN     NOT NULL DEFAULT true,
    is_freeform BOOLEAN     NOT NULL DEFAULT false,
    CONSTRAINT locations_parent_level CHECK (
        (level = 1 AND parent_id IS NULL) OR
        (level > 1 AND parent_id IS NOT NULL)
    ),
    CONSTRAINT locations_name_len CHECK (char_length(name) >= 1)
);

CREATE UNIQUE INDEX uq_locations_sibling ON locations(COALESCE(parent_id, 0), name);
CREATE INDEX idx_locations_parent ON locations(parent_id);

-- ============================================================
-- 4. items —— 核心。lost 和 found 共用一张表，用 item_type 区分
--    ⚠ 没有 color / brand 两列（计划 §3.2，第 5 版删除）
-- ============================================================
CREATE TABLE items (
    id              BIGSERIAL PRIMARY KEY,
    item_type       VARCHAR(8)   NOT NULL CHECK (item_type IN ('lost','found')),
    user_id         BIGINT       NOT NULL REFERENCES users(id),
    title           VARCHAR(100) NOT NULL,
    description     TEXT         NOT NULL DEFAULT '',
    category_id     BIGINT       NOT NULL REFERENCES categories(id),
    location_id     BIGINT       NOT NULL REFERENCES locations(id),
    location_detail VARCHAR(200) NOT NULL DEFAULT '',
    last_seen_at    TIMESTAMPTZ  NULL,   -- 仅 lost：最后一次确认还拥有它的时间
    lost_at         TIMESTAMPTZ  NULL,   -- 仅 lost：发现它不见了的时间
    found_at        TIMESTAMPTZ  NULL,   -- 仅 found：实际拾获时间（不是上传时间）
    contact         VARCHAR(100) NOT NULL,
    status          VARCHAR(16)  NOT NULL DEFAULT 'open'
                    CHECK (status IN ('open','closed','deleted')),
    view_count      INT          NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),

    -- 三个具名时间列把「哪种类型填哪列」钉死在数据库层
    CONSTRAINT items_time_semantics CHECK (
        (item_type = 'lost'  AND last_seen_at IS NOT NULL AND lost_at IS NOT NULL
                             AND last_seen_at <= lost_at AND found_at IS NULL)
        OR
        (item_type = 'found' AND found_at IS NOT NULL
                             AND last_seen_at IS NULL AND lost_at IS NULL)
    ),
    -- contact 只约束「非空 + 长度」，内容一律不校验（计划 §3.2）。
    --
    -- ⚠ btrim(x) 的**单参数版本只删空格**，制表符和换行原样保留 —— 那样 contact="\t\n"
    --   就能通过校验，而它在语义上和空字符串没有任何区别。必须显式给出要删的字符集。
    -- 字符集和 Go 侧的 strings.TrimSpace 对齐（app 层是第一道防线，这里是兜底）：
    --   E' \t\n\v\f\r'  六种 ASCII 空白
    --   chr(160)        U+00A0 不换行空格，从网页粘贴时会带进来
    --   chr(12288)      U+3000 全角空格 —— **中文输入法下按空格出来的就是它**，最容易漏
    -- 后两个用 chr() 而不是直接写字符本身：它们在编辑器里看起来就是一个空格，
    -- 谁「顺手清理一下行尾空白」就会静默地破坏这条约束，而且从 diff 上完全看不出来。
    CONSTRAINT items_contact_not_blank
        CHECK (char_length(btrim(contact, E' \t\n\v\f\r' || chr(160) || chr(12288))) >= 1),
    CONSTRAINT items_title_not_blank
        CHECK (char_length(btrim(title,   E' \t\n\v\f\r' || chr(160) || chr(12288))) >= 1)
);

CREATE INDEX idx_items_type_status_created ON items(item_type, status, created_at DESC);
CREATE INDEX idx_items_match               ON items(item_type, status, location_id);
CREATE INDEX idx_items_user                ON items(user_id);
CREATE INDEX idx_items_category            ON items(category_id);
CREATE INDEX idx_items_location            ON items(location_id);
CREATE INDEX idx_items_title_trgm          ON items USING gin(title       gin_trgm_ops);
CREATE INDEX idx_items_desc_trgm           ON items USING gin(description gin_trgm_ops);

-- ============================================================
-- 5. item_images —— 1:N，一个帖子多张图
-- ============================================================
CREATE TABLE item_images (
    id         BIGSERIAL PRIMARY KEY,
    item_id    BIGINT       NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    path       VARCHAR(255) NOT NULL,          -- uploads 相对路径，DB 不存二进制
    sort_order INT          NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX idx_item_images_item ON item_images(item_id);

-- ============================================================
-- 6. contact_views —— 纯审计日志，不是锁
--    ⚠ 表里没有任何「状态」列：它只表达「这个人此刻看过了联系方式」
-- ============================================================
CREATE TABLE contact_views (
    id         BIGSERIAL PRIMARY KEY,
    item_id    BIGINT      NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    user_id    BIGINT      NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_contact_views UNIQUE (item_id, user_id)   -- 重复点不产生第二行，接口天然幂等
);

CREATE INDEX idx_contact_views_item ON contact_views(item_id, created_at DESC);
CREATE INDEX idx_contact_views_user ON contact_views(user_id);

-- ============================================================
-- 7. item_returns —— 归还确认（事后记录，不是事前审批）
-- ============================================================
CREATE TABLE item_returns (
    id               BIGSERIAL PRIMARY KEY,
    item_id          BIGINT       NOT NULL REFERENCES items(id),
    submitter_id     BIGINT       NOT NULL REFERENCES users(id),
    message          TEXT         NOT NULL
                     CHECK (char_length(message) BETWEEN 5 AND 1000),
    proof_image_path VARCHAR(255) NOT NULL,     -- 凭证图必填
    status           VARCHAR(16)  NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','confirmed','rejected','cancelled')),
    owner_note       VARCHAR(500) NOT NULL DEFAULT '',
    -- 归属写进数据，不靠权限规则假装（计划 §3.4）
    reviewer_id      BIGINT       NULL REFERENCES users(id),
    review_kind      VARCHAR(16)  NULL CHECK (review_kind IN ('owner','admin_data_fix')),
    submitted_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    reviewed_at      TIMESTAMPTZ  NULL
);

-- 同一人对同一帖只能有一个待处理的确认；被拒后可以重新提交
CREATE UNIQUE INDEX uq_item_returns_pending ON item_returns(item_id, submitter_id) WHERE status = 'pending';
CREATE INDEX idx_item_returns_item      ON item_returns(item_id);
CREATE INDEX idx_item_returns_submitter ON item_returns(submitter_id);

-- ============================================================
-- 8. credit_logs —— 积分流水，让分数可解释、可 debug
-- ============================================================
CREATE TABLE credit_logs (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT      NOT NULL REFERENCES users(id),
    delta      INT         NOT NULL,
    reason     VARCHAR(32) NOT NULL,
    ref_type   VARCHAR(16) NULL,
    ref_id     BIGINT      NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_credit_logs_user ON credit_logs(user_id, created_at DESC);

-- ============================================================
-- 9. notifications —— 站内通知，7 种 type
--    ⚠ 不存在任何「发给 found 帖作者」的匹配类通知（计划 §3.7）
-- ============================================================
CREATE TABLE notifications (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT       NOT NULL REFERENCES users(id),
    type       VARCHAR(32)  NOT NULL CHECK (type IN (
                   'new_match','return_submitted','return_confirmed','return_rejected',
                   'item_returned_hint','admin_action','report_resolved')),
    title      VARCHAR(100) NOT NULL,
    content    VARCHAR(500) NOT NULL DEFAULT '',
    item_id    BIGINT       NULL,      -- 跳转用，批量下架时为 NULL
    return_id  BIGINT       NULL,
    is_read    BOOLEAN      NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX idx_notifications_user ON notifications(user_id, is_read, created_at DESC);

-- ============================================================
-- 10. match_pairs —— 通知台账，不是匹配结果表
--     唯一写入路径：found 帖创建时 Tier 1 命中 score >= 0.75
-- ============================================================
CREATE TABLE match_pairs (
    id            BIGSERIAL PRIMARY KEY,
    lost_item_id  BIGINT       NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    found_item_id BIGINT       NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    score         NUMERIC(5,4) NOT NULL CHECK (score >= 0 AND score <= 1),
    breakdown     JSONB        NOT NULL,
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT uq_match_pairs UNIQUE (lost_item_id, found_item_id)
);

CREATE INDEX idx_match_pairs_found ON match_pairs(found_item_id);
CREATE INDEX idx_match_pairs_lost  ON match_pairs(lost_item_id);

-- ============================================================
-- 11. admin_actions —— 治理留痕（第 6 版新增）
--     reason 必填：不给理由接口直接返回 VALIDATION
-- ============================================================
CREATE TABLE admin_actions (
    id          BIGSERIAL PRIMARY KEY,
    admin_id    BIGINT       NOT NULL REFERENCES users(id),
    action      VARCHAR(32)  NOT NULL CHECK (action IN (
                    'item_takedown','item_restore','image_takedown','return_takedown',
                    'item_edit','user_ban','user_unban','user_role_change',
                    'warning_sent','report_resolved','dict_create','dict_delete')),
    target_type VARCHAR(16)  NOT NULL CHECK (target_type IN (
                    'item','item_image','item_return','user','report','category','location')),
    target_id   BIGINT       NOT NULL,     -- 批量时存第一个 id，完整列表进 detail.ids
    -- reason 是整个治理模型唯一的问责依据：这串文字会原样进被处置人的通知，
    -- 也是「谁在什么时候因为什么删了哪些帖子」的唯一记录（计划 §3.7、定位原则 5）。
    -- 所以「非空」必须是真的非空 —— 单参数 btrim 只删空格，reason="\t\n" 能通过校验，
    -- 而用户收到的通知会变成「你的帖子因『』被下架」。字符集说明见 items 表那处注释。
    reason      VARCHAR(500) NOT NULL
                CHECK (char_length(btrim(reason, E' \t\n\v\f\r' || chr(160) || chr(12288))) >= 1),
    detail      JSONB        NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX idx_admin_actions_admin  ON admin_actions(admin_id, created_at DESC);
CREATE INDEX idx_admin_actions_target ON admin_actions(target_type, target_id);

-- ============================================================
-- 12. reports —— 举报（第 6 版新增，只记录，不仲裁）
--     ⚠ 这张表刻意没有任何自动后果：不下架、不扣分、不影响排序、不通知被举报人
-- ============================================================
CREATE TABLE reports (
    id            BIGSERIAL PRIMARY KEY,
    item_id       BIGINT       NOT NULL REFERENCES items(id) ON DELETE CASCADE,
    reporter_id   BIGINT       NOT NULL REFERENCES users(id),   -- 除 admin 外对任何人不可见
    reason_code   VARCHAR(24)  NOT NULL CHECK (reason_code IN (
                      'spam','privacy','fraud','harassment','illegal','other')),
    detail        VARCHAR(500) NOT NULL DEFAULT '',
    status        VARCHAR(16)  NOT NULL DEFAULT 'open'
                  CHECK (status IN ('open','resolved','dismissed')),
    resolved_by   BIGINT       NULL REFERENCES users(id),
    resolved_note VARCHAR(500) NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    resolved_at   TIMESTAMPTZ  NULL
);

-- 防刷：同一人对同一帖只能有一条待处理举报
CREATE UNIQUE INDEX uq_reports_open ON reports(item_id, reporter_id) WHERE status = 'open';
CREATE INDEX idx_reports_status ON reports(status, created_at);
CREATE INDEX idx_reports_item   ON reports(item_id);

-- ============================================================
-- 种子数据：分类 55 行（9 大类 + 46 小类）
-- id 写死，方便迁移可重复执行、方便前端调试时对号
-- ============================================================
INSERT INTO categories (id, parent_id, name, level, sort_order) VALUES
-- 9 个大类
( 1, NULL, '数码电子', 1, 1),
( 2, NULL, '证件卡片', 1, 2),
( 3, NULL, '钥匙',     1, 3),
( 4, NULL, '书籍文具', 1, 4),
( 5, NULL, '衣物箱包', 1, 5),
( 6, NULL, '生活用品', 1, 6),
( 7, NULL, '运动器材', 1, 7),
( 8, NULL, '首饰配饰', 1, 8),
( 9, NULL, '其他',     1, 9),
-- 数码电子 (9)
(10, 1, '手机',           2,  1),
(11, 1, '笔记本电脑',     2,  2),
(12, 1, '平板',           2,  3),
(13, 1, '耳机音响',       2,  4),
(14, 1, '相机',           2,  5),
(15, 1, '智能穿戴',       2,  6),
(16, 1, '充电器与数据线', 2,  7),
(17, 1, 'U盘移动硬盘',    2,  8),
(18, 1, '其他数码',       2,  9),
-- 证件卡片 (5)
(19, 2, '身份证',       2, 1),
(20, 2, '学生证校园卡', 2, 2),
(21, 2, '银行卡',       2, 3),
(22, 2, '驾驶证',       2, 4),
(23, 2, '其他证件',     2, 5),
-- 钥匙 (5)
(24, 3, '宿舍钥匙',    2, 1),
(25, 3, '家门钥匙',    2, 2),
(26, 3, '车钥匙',      2, 3),
(27, 3, '钥匙串(多把)', 2, 4),
(28, 3, '其他钥匙',    2, 5),
-- 书籍文具 (5)
(29, 4, '书籍教材',   2, 1),
(30, 4, '笔记本文档', 2, 2),
(31, 4, '文具笔类',   2, 3),
(32, 4, '画具',       2, 4),
(33, 4, '其他文具',   2, 5),
-- 衣物箱包 (7)
(34, 5, '背包书包',     2, 1),
(35, 5, '手提袋',       2, 2),
(36, 5, '钱包',         2, 3),
(37, 5, '外套衣物',     2, 4),
(38, 5, '帽子围巾手套', 2, 5),
(39, 5, '眼镜墨镜',     2, 6),
(40, 5, '鞋子',         2, 7),
-- 生活用品 (5)
(41, 6, '雨伞',       2, 1),
(42, 6, '水杯保温杯', 2, 2),
(43, 6, '餐具',       2, 3),
(44, 6, '洗漱用品',   2, 4),
(45, 6, '其他生活',   2, 5),
-- 运动器材 (5)
(46, 7, '球类',     2, 1),
(47, 7, '球拍',     2, 2),
(48, 7, '运动护具', 2, 3),
(49, 7, '健身器材', 2, 4),
(50, 7, '其他运动', 2, 5),
-- 首饰配饰 (4)
(51, 8, '戒指项链手链', 2, 1),
(52, 8, '耳环',         2, 2),
(53, 8, '胸针发饰',     2, 3),
(54, 8, '其他首饰',     2, 4),
-- 其他 (1)
(55, 9, '其他物品', 2, 1);

SELECT setval(pg_get_serial_sequence('categories','id'), 55);

-- ============================================================
-- 种子数据：地点 91 行（3 个一级 + 8 个二级 + 80 个三级）
-- ⚠ 计划里曾写成 92，那是把「其他」数了两遍。逐行数是 91。
-- ============================================================
INSERT INTO locations (id, parent_id, name, level, sort_order, is_freeform) VALUES
-- 3 个一级区
(1, NULL, '生活区（北侧）', 1, 1, false),
(2, NULL, '教学区（南侧）', 1, 2, false),
(3, NULL, '其他',           1, 3, true),    -- 无子节点，用户自己填「最近的建筑」
-- 8 个二级子类
( 4, 1, '生活区校门',       2, 1, false),
( 5, 1, '宿舍楼宇',         2, 2, false),
( 6, 1, '餐饮配套',         2, 3, false),
( 7, 2, '校门',             2, 1, false),
( 8, 2, '教学楼',           2, 2, false),
( 9, 2, '场馆与公共建筑',   2, 3, false),
(10, 2, '广场湖泊',         2, 4, false),
(11, 2, '体育区域',         2, 5, false),
-- 生活区校门 (4)
(12, 4, '生活区北门', 3,  1, false),
(13, 4, '生活区东门', 3,  2, false),
(14, 4, '南一门',     3,  3, false),
(15, 4, '南二门',     3,  4, false),
-- 宿舍楼宇 (27)
(16, 5, '2楼',              3,  1, false),
(17, 5, '3楼',              3,  2, false),
(18, 5, '4楼北',            3,  3, false),
(19, 5, '4楼南',            3,  4, false),
(20, 5, '5楼北',            3,  5, false),
(21, 5, '5楼南(女生寝室)',  3,  6, false),
(22, 5, '6楼北',            3,  7, false),
(23, 5, '6楼南',            3,  8, false),
(24, 5, '8楼',              3,  9, false),
(25, 5, '10楼北',           3, 10, false),
(26, 5, '10楼南',           3, 11, false),
(27, 5, '11楼南',           3, 12, false),
(28, 5, '11楼北(男生寝室)', 3, 13, false),
(29, 5, '12楼南',           3, 14, false),
(30, 5, '13楼',             3, 15, false),
(31, 5, '14楼',             3, 16, false),
(32, 5, '15楼',             3, 17, false),
(33, 5, '16楼',             3, 18, false),
(34, 5, '18楼',             3, 19, false),
(35, 5, '21楼',             3, 20, false),
(36, 5, '22楼',             3, 21, false),
(37, 5, '27楼',             3, 22, false),
(38, 5, '28楼',             3, 23, false),
(39, 5, '29楼',             3, 24, false),
(40, 5, '30楼',             3, 25, false),
(41, 5, '31楼',             3, 26, false),
(42, 5, '32楼(公主楼)',     3, 27, false),
-- 餐饮配套 (10)
(43, 6, '一餐厅',             3,  1, false),
(44, 6, '清真餐厅',           3,  2, false),
(45, 6, '一餐健身房',         3,  3, false),
(46, 6, '五餐厅',             3,  4, false),
(47, 6, '六餐厅',             3,  5, false),
(48, 6, '五六餐厅健身房',     3,  6, false),
(49, 6, '三餐厅',             3,  7, false),
(50, 6, '美食城',             3,  8, false),
(51, 6, '教工餐厅',           3,  9, false),
(52, 6, '师生(后勤)事务大厅', 3, 10, false),
-- 校门 (5)
(53, 7, '南大门(教学区南门)', 3, 1, false),
(54, 7, '东门',               3, 2, false),
(55, 7, '东南门',             3, 3, false),
(56, 7, '北一门',             3, 4, false),
(57, 7, '北二门',             3, 5, false),
-- 教学楼 (12)
(58, 8, '1教信仁楼',  3,  1, false),
(59, 8, '2教信义楼',  3,  2, false),
(60, 8, '3教信礼楼',  3,  3, false),
(61, 8, '4教信智楼',  3,  4, false),
(62, 8, '5教草坪',    3,  5, false),
(63, 8, '6教信诚楼',  3,  6, false),
(64, 8, '7教信博楼',  3,  7, false),
(65, 8, '8教信达楼',  3,  8, false),
(66, 8, '9教力行楼',  3,  9, false),
(67, 8, '10教笃学楼', 3, 10, false),
(68, 8, '11教求索楼', 3, 11, false),
(69, 8, '12教守正楼', 3, 12, false),
-- 场馆与公共建筑 (8)
(70, 9, '图书馆',       3, 1, false),
(71, 9, '行政楼',       3, 2, false),
(72, 9, '科技馆',       3, 3, false),
(73, 9, '国际教育中心', 3, 4, false),
(74, 9, '学生活动中心', 3, 5, false),
(75, 9, '校医院',       3, 6, false),
(76, 9, '保卫处',       3, 7, false),
(77, 9, 'EMS收发室',    3, 8, false),
-- 广场湖泊 (3)
(78, 10, '同鼎广场', 3, 1, false),
(79, 10, '五四广场', 3, 2, false),
(80, 10, '月雅湖',   3, 3, false),
-- 体育区域 (11)
(81, 11, '体育场',     3,  1, false),
(82, 11, '体育馆',     3,  2, false),
(83, 11, '体育馆副馆', 3,  3, false),
(84, 11, '足球场',     3,  4, false),
(85, 11, '风雨操场',   3,  5, false),
(86, 11, '羽毛球馆',   3,  6, false),
(87, 11, '西篮球场',   3,  7, false),
(88, 11, '网球场',     3,  8, false),
(89, 11, '排球场',     3,  9, false),
(90, 11, '休读园',     3, 10, false),
(91, 11, '东操场',     3, 11, false);

SELECT setval(pg_get_serial_sequence('locations','id'), 91);

-- 回滚 000001。按外键依赖的反序删。
-- CASCADE 是因为字典表被 items 引用，测试库里 down 到 0 再 up 时需要一次清干净。

DROP TABLE IF EXISTS reports         CASCADE;
DROP TABLE IF EXISTS admin_actions   CASCADE;
DROP TABLE IF EXISTS match_pairs     CASCADE;
DROP TABLE IF EXISTS notifications   CASCADE;
DROP TABLE IF EXISTS credit_logs     CASCADE;
DROP TABLE IF EXISTS item_returns    CASCADE;
DROP TABLE IF EXISTS contact_views   CASCADE;
DROP TABLE IF EXISTS item_images     CASCADE;
DROP TABLE IF EXISTS items           CASCADE;
DROP TABLE IF EXISTS locations       CASCADE;
DROP TABLE IF EXISTS categories      CASCADE;
DROP TABLE IF EXISTS users           CASCADE;

-- pg_trgm 故意不 DROP EXTENSION：它是实例级的，删了会影响同实例上的其他库

-- 样例数据集（T1002）：订单 10 万、商品 1 万、店铺 1000、退款 2 万。
--
-- 与 examples/seed.sql 的区别：这里刻意保留了「看起来像真的」脏数据。
-- 全部是业务层异常，不破坏表约束——灌数必须成功，否则演示一开始就崩，
-- 而这些异常正好会被质量规则抓到，用来演示门禁与隔离。
--
-- 脏数据清单：
--   1. 2% 订单金额落在 5 万~20 万（异常大额，正常客单 20~1200）；
--   2. 1% 订单 status 为空串（枚举校验会抓）；
--   3. 0.5% 订单 user_id = 0（疑似埋点缺失）；
--   4. 部分退款金额大于订单金额（业务上不该发生）。
--
-- 可重复执行：先 DROP 再建，保证每次 demo 的基线完全一致。

CREATE SCHEMA IF NOT EXISTS ods;

-- 固定随机种子：demo 重跑必须得到同一份数据，否则「结果一致」无法验证。
-- setseed 只影响当前会话后续的 random()，seed 命令全程用同一个连接。
SELECT setseed(0.42);

-- 清空上一轮演示的产出表，保证每次 demo 的基线完全一致。
--
-- 为什么必须清：DWD 是 upsert 写入，旧数据里 order_id 超出本轮 10 万行范围的
-- 记录不会被覆盖；只要其中有一行金额为负，质量规则就会在某个日期分区失败，
-- 表现为「demo 时好时坏」。这不是规则的问题，是基线不干净。
-- 用 to_regclass 判断存在性，首次运行时这些表还不存在，跳过即可。
DO $$
BEGIN
  IF to_regclass('dwd.dwd_order') IS NOT NULL THEN TRUNCATE dwd.dwd_order; END IF;
  IF to_regclass('dwd.dwd_refund') IS NOT NULL THEN TRUNCATE dwd.dwd_refund; END IF;
  IF to_regclass('dws.dws_order_daily') IS NOT NULL THEN TRUNCATE dws.dws_order_daily; END IF;
  IF to_regclass('dws.dws_refund_daily') IS NOT NULL THEN TRUNCATE dws.dws_refund_daily; END IF;
  IF to_regclass('ads.ads_order_dashboard') IS NOT NULL THEN TRUNCATE ads.ads_order_dashboard; END IF;
  IF to_regclass('ads.ads_refund_dashboard') IS NOT NULL THEN TRUNCATE ads.ads_refund_dashboard; END IF;
END $$;

DROP TABLE IF EXISTS ods.refund;
DROP TABLE IF EXISTS ods.product;
DROP TABLE IF EXISTS ods.shop;
DROP TABLE IF EXISTS ods."order";

CREATE TABLE ods."order" (
    order_id BIGINT PRIMARY KEY,
    user_id  BIGINT NOT NULL,
    amount   NUMERIC(20,4) NOT NULL,
    status   TEXT NOT NULL,
    dt       DATE NOT NULL
);

CREATE TABLE ods.shop (
    shop_id    INT PRIMARY KEY,
    shop_name  TEXT NOT NULL,
    level      TEXT NOT NULL,
    created_at DATE NOT NULL
);

CREATE TABLE ods.product (
    product_id INT PRIMARY KEY,
    shop_id    INT NOT NULL REFERENCES ods.shop(shop_id),
    name       TEXT NOT NULL,
    category   TEXT NOT NULL,
    price      NUMERIC(12,2) NOT NULL
);

CREATE TABLE ods.refund (
    refund_id BIGINT PRIMARY KEY,
    order_id  BIGINT NOT NULL,
    amount    NUMERIC(20,4) NOT NULL,
    reason    TEXT NOT NULL,
    status    TEXT NOT NULL,
    dt        DATE NOT NULL
);

-- 店铺：4 个等级，创建时间分散在两年内，便于演示维度下钻。
INSERT INTO ods.shop
SELECT g,
       '店铺' || g,
       (ARRAY['S', 'A', 'B', 'C'])[1 + (g % 4)],
       DATE '2024-01-01' + (g % 600)
FROM generate_series(1, 1000) g;

-- 商品：5 个类目，价格 10~910。
INSERT INTO ods.product
SELECT g,
       1 + (g % 1000),
       '商品' || g,
       (ARRAY['数码', '服饰', '食品', '家居', '美妆'])[1 + (g % 5)],
       round((random() * 900 + 10)::numeric, 2)
FROM generate_series(1, 10000) g;

-- 订单：30 天，每天约 3300 单。金额随日期缓慢上抬，趋势图才有形状。
--
-- 脏数据用 random() 而不是 g % N：dt 是 g % 30 轮转出来的，任何基于 g 的
-- 取模条件都会固定在某一两天，导致「质检某天通过、某天不通过」，演示时
-- 看起来像门禁时灵时不灵。改成概率后脏数据均匀落在每一天。
INSERT INTO ods."order"
SELECT g,
       CASE WHEN random() < 0.005 THEN 0 ELSE (random() * 20000)::bigint END,
       round((CASE WHEN random() < 0.02
                   THEN random() * 150000 + 50000            -- 异常大额（质量规则会抓）
                   ELSE random() * 800 + 20 + (g % 30) * 12  -- 正常区间，带日增长
              END)::numeric, 2),
       -- 退款状态占比与 ods.refund 的 2 万笔对齐（2万/10万 = 20%），
       -- 否则「订单看板」和「退款看板」会对同一个退款率给出两个数。
       CASE WHEN random() < 0.01 THEN ''                      -- 枚举缺失
            WHEN random() < 0.20 THEN 'refunded'
            WHEN random() < 0.35 THEN 'created'
            ELSE 'paid'
       END,
       DATE '2026-10-11' + (g % 30)
FROM generate_series(1, 100000) g;

-- 退款：2 万单，原因分布不均（质量问题最多），便于画占比。
INSERT INTO ods.refund
SELECT g,
       g,
       round((random() * 200 + 10)::numeric, 2),
       -- 原因用 (g/30) 取模：dt 是 g%30，两者互质才不会出现
       -- 「某一天只有一种退款原因」，否则占比图会退化成一整块。
       (ARRAY['质量问题', '质量问题', '尺码不符', '物流慢', '不喜欢', '商家发错'])[1 + ((g / 30) % 6)],
       CASE WHEN random() < 0.02 THEN 'closed' ELSE 'done' END,
       -- 退款日随机分布：订单按 g%30 均匀轮转，退款若也均匀，30 天曲线会是一条直线。
       DATE '2026-10-11' + floor(random() * 30)::int
FROM generate_series(1, 20000) g;

CREATE INDEX IF NOT EXISTS idx_order_dt ON ods."order"(dt);
CREATE INDEX IF NOT EXISTS idx_refund_dt ON ods.refund(dt);
CREATE INDEX IF NOT EXISTS idx_product_shop ON ods.product(shop_id);

ANALYZE ods."order";
ANALYZE ods.refund;
ANALYZE ods.product;
ANALYZE ods.shop;

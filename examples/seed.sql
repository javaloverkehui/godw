-- 样例数据：30 天订单明细。可重复执行（先清空再灌）。
-- 日期范围 2026-10-11 ~ 2026-11-09，用于演示趋势与补数。
CREATE SCHEMA IF NOT EXISTS ods;

CREATE TABLE IF NOT EXISTS ods."order" (
    order_id BIGINT PRIMARY KEY,
    user_id  BIGINT NOT NULL,
    amount   NUMERIC(20,4) NOT NULL,
    status   TEXT NOT NULL,
    dt       DATE NOT NULL
);

TRUNCATE ods."order";

INSERT INTO ods."order"
SELECT g,
       (random() * 20000)::bigint,
       round((random() * 800 + 20 + (g % 30) * 12)::numeric, 2),
       (ARRAY['paid','refunded','created'])[1 + floor(random() * 3)::int],
       DATE '2026-10-11' + (g % 30)
FROM generate_series(1, 150000) g;

CREATE INDEX IF NOT EXISTS idx_order_dt ON ods."order"(dt);
ANALYZE ods."order";

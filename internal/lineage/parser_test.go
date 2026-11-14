package lineage

import (
	"strings"
	"testing"
)

// T0605 验收：50 条真实业务 SQL 解析成功率 ≥ 95%；非法 SQL 返回结构化错误不 panic。
func TestParseSQLRealWorld(t *testing.T) {
	// 50 条覆盖典型写法的 SQL：CTE、JOIN、子查询、UNION、窗口函数、复杂条件。
	sqls := []string{
		`SELECT order_id, amount FROM ods.orders WHERE dt = '2025-11-10'`,
		`SELECT o.order_id, u.name FROM ods.orders o JOIN ods.users u ON o.user_id = u.id`,
		`WITH base AS (SELECT * FROM ods.orders WHERE dt = '2025-11-10') SELECT order_id FROM base`,
		`SELECT a, b FROM (SELECT a, b FROM ods.t1) t`,
		`SELECT SUM(amount) AS total FROM dwd.orders GROUP BY status`,
		`SELECT order_id, ROW_NUMBER() OVER (PARTITION BY user_id ORDER BY created_at DESC) rn FROM dwd.orders`,
		`SELECT * FROM ods.a UNION ALL SELECT * FROM ods.b`,
		`SELECT COALESCE(x, 0) AS x FROM ods.t`,
		`SELECT CASE WHEN amount > 100 THEN 'big' ELSE 'small' END AS bucket FROM dwd.orders`,
		`SELECT COUNT(DISTINCT user_id) FROM dwd.orders`,
		`SELECT date_trunc('month', dt) AS m, COUNT(*) FROM dwd.orders GROUP BY 1`,
		`SELECT t1.id FROM ods.t1 t1 LEFT JOIN ods.t2 t2 ON t1.id = t2.id WHERE t2.id IS NULL`,
		`SELECT order_id FROM dwd.orders o WHERE EXISTS (SELECT 1 FROM dwd.refunds r WHERE r.order_id = o.order_id)`,
		`WITH a AS (SELECT id FROM ods.x), b AS (SELECT id FROM a) SELECT id FROM b`,
		`SELECT amount * 2 AS amt2 FROM dwd.orders`,
		`SELECT id, amount FROM dwd.orders ORDER BY amount DESC LIMIT 100`,
		`SELECT status, COUNT(*) c FROM dwd.orders GROUP BY status HAVING COUNT(*) > 10`,
		`SELECT id FROM ods.a INTERSECT SELECT id FROM ods.b`,
		`SELECT id FROM ods.a EXCEPT SELECT id FROM ods.b`,
		`SELECT CAST(amount AS DECIMAL(18,2)) AS amt FROM dwd.orders`,
		`SELECT id, LAG(amount) OVER (ORDER BY dt) prev FROM dwd.orders`,
		`SELECT * FROM dwd.orders WHERE dt BETWEEN '2025-11-01' AND '2025-11-10'`,
		`SELECT id FROM dwd.orders WHERE status IN ('paid','shipped')`,
		`SELECT o.id, p.name FROM dwd.orders o, dwd.products p WHERE o.pid = p.id`,
		`SELECT MAX(dt) FROM dwd.orders`,
		`SELECT id FROM dwd.a a1 JOIN dwd.a a2 ON a1.pid = a2.id`,
		`SELECT id, NTILE(4) OVER (ORDER BY amount) q FROM dwd.orders`,
		`SELECT id FROM (SELECT id FROM (SELECT id FROM ods.deep) x) y`,
		`SELECT id, amount FROM dwd.orders WHERE amount > (SELECT AVG(amount) FROM dwd.orders)`,
		`SELECT COUNT(*) FROM dwd.orders o JOIN dwd.users u ON o.uid = u.id GROUP BY u.city`,
		`SELECT id FROM ods.t1 WHERE id NOT IN (SELECT id FROM ods.t2)`,
		`SELECT SUM(CASE WHEN is_refunded THEN amount ELSE 0 END) refund_amt FROM dwd.orders`,
		`SELECT id, first_value(amount) OVER (PARTITION BY uid ORDER BY dt) fv FROM dwd.orders`,
		`SELECT id FROM dwd.orders WHERE dt >= CURRENT_DATE - 7`,
		`SELECT id FROM dwd.orders o WHERE o.amount > 0 AND o.status <> 'canceled'`,
		`SELECT uid, COUNT(*) FROM dwd.orders GROUP BY uid ORDER BY 2 DESC`,
		`SELECT DISTINCT uid FROM dwd.orders`,
		`SELECT id FROM dws.order_daily WHERE dt = '2025-11-10'`,
		`SELECT COALESCE(SUM(amount), 0) FROM dwd.orders WHERE dt = '2025-11-10'`,
		`SELECT id FROM ods.t WHERE json_extract(data, '$.x') = '1'`,
		`SELECT id, RANK() OVER (PARTITION BY status ORDER BY amount DESC) rk FROM dwd.orders`,
		`SELECT a.id FROM ods.a a JOIN ods.b b ON a.id=b.id JOIN ods.c c ON b.id=c.id`,
		`SELECT id FROM dwd.orders WHERE dt = (SELECT MAX(dt) FROM dwd.orders)`,
		`SELECT SUBSTR(name, 1, 3) FROM dim.users`,
		`SELECT id FROM dim.users WHERE email LIKE '%@x.com'`,
		`SELECT id, amount FROM dwd.orders WHERE amount IS NOT NULL`,
		`SELECT id FROM dwd.orders o JOIN dwd.refunds r USING (order_id)`,
		`SELECT COUNT(*) FILTER (WHERE amount > 0) FROM dwd.orders`,
		`SELECT id FROM dwd.orders TABLESAMPLE SYSTEM (10)`,
		`SELECT id FROM ods.t ORDER BY dt DESC NULLS LAST`,
	}

	ok, deg := 0, 0
	for i, s := range sqls {
		res, err := ParseSQL(s)
		if err != nil {
			t.Logf("第 %d 条解析失败: %v\n%s", i+1, err, s)
			continue
		}
		if res.Degraded {
			deg++
			t.Logf("第 %d 条降级: %s", i+1, res.Reason)
			continue
		}
		if res.Query == nil {
			t.Fatalf("第 %d 条返回空 Query", i+1)
		}
		ok++
	}
	rate := float64(ok) / float64(len(sqls))
	t.Logf("解析成功 %d/%d（%.1f%%），降级 %d", ok, len(sqls), rate*100, deg)
	if rate < 0.95 {
		t.Fatalf("解析成功率 %.1f%% 低于 95%%", rate*100)
	}
}

func TestParseSQLErrors(t *testing.T) {
	if _, err := ParseSQL("   "); err == nil {
		t.Fatal("空 SQL 应报错")
	}
	// 语法错误不应 panic，且应降级而非失败
	res, err := ParseSQL("SELECT FROM WHERE")
	if err != nil {
		t.Fatalf("不应返回错误，应降级: %v", err)
	}
	if !res.Degraded {
		t.Fatal("语法错误应降级")
	}
}

func TestParseSQLLarge(t *testing.T) {
	big := "SELECT id FROM ods.t WHERE " + strings.Repeat("a = 1 AND ", 12000) + "1=1"
	res, err := ParseSQL(big)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Degraded || !strings.Contains(res.Reason, "降级") {
		t.Fatalf("超大 SQL 应降级: %+v", res)
	}
	if len(res.Query.Sources) == 0 {
		t.Fatal("降级后仍应识别表名")
	}
}

func BenchmarkParseSQL(b *testing.B) {
	sqlText := `WITH base AS (
	  SELECT o.order_id, o.amount, u.city
	  FROM ods.orders o JOIN ods.users u ON o.user_id = u.id
	  WHERE o.dt = '2025-11-10'
	)
	SELECT city, SUM(amount) AS gmv FROM base GROUP BY city`
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ParseSQL(sqlText); err != nil {
			b.Fatal(err)
		}
	}
}

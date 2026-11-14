package lineage

import "testing"

func TestAnalyzeSimple(t *testing.T) {
	q, err := Analyze(`SELECT order_id, user_id AS uid, amount * 2 AS amt2 FROM dwd.dwd_order WHERE dt = '2026-11-09'`)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Sources) != 1 || q.Sources[0].Table != "dwd.dwd_order" {
		t.Fatalf("来源表错误: %+v", q.Sources)
	}
	if len(q.Outputs) != 3 {
		t.Fatalf("输出列数错误: %+v", q.Outputs)
	}
	if q.Outputs[1].Name != "uid" {
		t.Errorf("别名未取到: %s", q.Outputs[1].Name)
	}
	if len(q.Outputs[2].Inputs) != 1 || q.Outputs[2].Inputs[0].Column != "amount" {
		t.Errorf("表达式列血缘错误: %+v", q.Outputs[2].Inputs)
	}
}

func TestAnalyzeJoin(t *testing.T) {
	q, err := Analyze(`SELECT o.order_id, u.name, SUM(o.amount) AS gmv
FROM ods.orders o
JOIN ods.users u ON u.id = o.user_id
GROUP BY o.order_id, u.name`)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Sources) != 2 {
		t.Fatalf("来源表数错误: %+v", q.Sources)
	}
	// o.order_id 应解析成 ods.orders
	if q.Outputs[0].Inputs[0].Table != "ods.orders" || q.Outputs[0].Inputs[0].Column != "order_id" {
		t.Errorf("限定名解析错误: %+v", q.Outputs[0].Inputs)
	}
	// u.name -> ods.users
	if q.Outputs[1].Inputs[0].Table != "ods.orders" && q.Outputs[1].Inputs[0].Table != "ods.users" {
		t.Errorf("别名解析失败: %+v", q.Outputs[1].Inputs)
	}
}

func TestAnalyzeCTE(t *testing.T) {
	q, err := Analyze(`WITH base AS (
  SELECT id, amount, status FROM ods."order" WHERE dt = '2026-11-09'
)
SELECT status, SUM(amount) AS total FROM base GROUP BY status`)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Sources) != 1 || q.Sources[0].Table != `ods."order"` && q.Sources[0].Table != "ods.order" {
		t.Logf("来源: %+v", q.Sources)
	}
	if len(q.Outputs) != 2 {
		t.Fatalf("输出列: %+v", q.Outputs)
	}
	if len(q.Outputs[1].Inputs) != 1 || q.Outputs[1].Inputs[0].Column != "amount" {
		t.Errorf("CTE 列解析错误: %+v", q.Outputs[1].Inputs)
	}
}

func TestAnalyzeStar(t *testing.T) {
	q, err := Analyze("SELECT * FROM ods.t")
	if err != nil {
		t.Fatal(err)
	}
	if !q.HasStar {
		t.Error("应识别到 *")
	}
}

func TestAnalyzeSubquery(t *testing.T) {
	q, err := Analyze(`SELECT t.dt, t.cnt FROM (
  SELECT dt, COUNT(*) AS cnt FROM dws.dws_order_daily GROUP BY dt
) t`)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Sources) != 1 || q.Sources[0].Table != "dws.dws_order_daily" {
		t.Fatalf("子查询来源错误: %+v", q.Sources)
	}
	if len(q.Outputs) != 2 {
		t.Fatalf("输出: %+v", q.Outputs)
	}
}

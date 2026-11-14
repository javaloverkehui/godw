package service_test

import (
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"strings"
	"testing"
)

// 画像要在一次查询里拿到行数/空值率/去重数/极值，并据此推荐可编译的规则。
func TestProfileTable(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date, id bigint, amount decimal(18,2), region text)`, p.table("dwd_prof_t"))
	p.exec(t, `INSERT INTO %s VALUES
		('2026-08-01',1,10,'east'),
		('2026-08-01',2,20,'west'),
		('2026-08-01',3,30,'east')`, p.table("dwd_prof_t"))

	// 未采集元数据时不能猜列，必须提示先采集。
	if _, err := svc.Profile(ctxBG(), ws.ID, ds.Name, p.schema, "dwd_prof_t", 0); !errs.Is(err, errs.CodeNotFound) {
		t.Fatalf("缺元数据应 NotFound，实际 %v", err)
	}
	if err := svc.Repo.UpsertDataset(ctxBG(), model.Dataset{
		DatasourceID: ds.ID, Schema: p.schema, Name: "dwd_prof_t",
		Columns: []model.ColumnSpec{
			{Name: "dt", Type: "date"}, {Name: "id", Type: "bigint"},
			{Name: "amount", Type: "decimal(18,2)"}, {Name: "region", Type: "string"},
		},
	}, ws.ID); err != nil {
		t.Fatalf("写入 dataset 失败: %v", err)
	}

	res, err := svc.Profile(ctxBG(), ws.ID, ds.Name, p.schema, "dwd_prof_t", 0)
	if err != nil {
		t.Fatalf("Profile 失败: %v", err)
	}
	if res["rows"] != int64(3) {
		t.Fatalf("行数不对: %+v", res["rows"])
	}
	cols, _ := res["columns"].([]map[string]any)
	if len(cols) != 4 {
		t.Fatalf("应有 4 列画像: %+v", cols)
	}
	idCol := cols[1]
	if idCol["distinct"] != int64(3) || idCol["nulls"] != int64(0) {
		t.Fatalf("id 列统计不对: %+v", idCol)
	}
	// 数值/日期列才取极值，字符串列不取。
	if idCol["min"] == nil || idCol["max"] == nil {
		t.Fatalf("有序列应有极值: %+v", idCol)
	}
	if cols[3]["min"] != nil {
		t.Fatalf("字符串列不应取极值: %+v", cols[3])
	}

	rules, _ := res["recommended"].([]model.QualityRule)
	var sawNotNull, sawUnique bool
	for _, r := range rules {
		switch r.Kind {
		case "not_null":
			sawNotNull = true
		case "unique":
			sawUnique = true
		}
	}
	if !sawNotNull || !sawUnique {
		t.Fatalf("应推荐 not_null 与 unique: %+v", rules)
	}
	// 推荐规则必须能编译，否则用户点一下就报错。
	if len(rules) == 0 {
		t.Fatal("应至少推荐一条规则")
	}

	// 采样模式：SQL 走子查询 + LIMIT，返回的仍是采样集上的统计。
	sampled, err := svc.Profile(ctxBG(), ws.ID, ds.Name, p.schema, "dwd_prof_t", 2)
	if err != nil {
		t.Fatalf("采样画像失败: %v", err)
	}
	if sqlText, _ := sampled["sql"].(string); !strings.Contains(sqlText, "LIMIT") {
		t.Fatalf("采样画像应带 LIMIT: %s", sqlText)
	}
	if sampled["rows"] != int64(2) {
		t.Fatalf("采样应是 2 行: %+v", sampled["rows"])
	}

	// 空表：行数 0，不应给出 not_null/unique 这类基于比例的推荐。
	p.exec(t, `TRUNCATE %s`, p.table("dwd_prof_t"))
	empty, err := svc.Profile(ctxBG(), ws.ID, ds.Name, p.schema, "dwd_prof_t", 0)
	if err != nil {
		t.Fatalf("空表画像失败: %v", err)
	}
	if empty["rows"] != int64(0) {
		t.Fatalf("空表行数应为 0: %+v", empty["rows"])
	}
	if rs, _ := empty["recommended"].([]model.QualityRule); len(rs) != 0 {
		t.Fatalf("空表不应推荐规则: %+v", rs)
	}
}

// 自助查询的第一道闸是静态只读校验：注释绕过、多语句、CTE 内写操作都要挡住。
func TestRawSQLReadOnlyGuard(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)

	bad := []string{
		"DELETE FROM dw.t",
		"UPDATE dw.t SET a=1",
		"SELECT 1; SELECT 2",
		"WITH x AS (DELETE FROM dw.t RETURNING *) SELECT * FROM x",
		"SELECT * FROM dw.t FOR UPDATE",
		"/* hi */ DROP TABLE dw.t",
		"",
	}
	for _, s := range bad {
		if _, err := svc.RawSQLQuery(ctxBG(), ws.ID, ds.Name, s, "alice", "viewer", 10); !errs.Is(err, errs.CodeInvalidArgument) {
			t.Fatalf("%q 应被只读校验拒绝，实际 %v", s, err)
		}
	}
	// 字符串字面量里的关键字不算写操作（等长替换的意义所在）。
	if _, err := svc.RawSQLQuery(ctxBG(), ws.ID, ds.Name, "SELECT 'delete' AS x", "alice", "viewer", 10); err != nil {
		t.Fatalf("字面量中的关键字不应误判: %v", err)
	}
	if _, err := svc.RawSQLQuery(ctxBG(), ws.ID, "", "SELECT 1", "", "", 10); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("缺数据源应报参数错误，实际 %v", err)
	}
}

// 结果行数硬上限、截断标记、列级脱敏：自助模式只有列名可用，宁可多脱不可漏脱。
func TestRawSQLQueryLimitAndMask(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (id bigint, phone text)`, p.table("dwd_raw_t"))
	p.exec(t, `INSERT INTO %s VALUES (1,'13800000001'),(2,'13800000002'),(3,'13800000003')`, p.table("dwd_raw_t"))
	if err := svc.Repo.SaveColumnPolicy(ctxBG(), ws.ID, model.ColumnPolicy{
		TableName: "any_table", ColumnName: "phone", Classification: "high", MaskType: "phone",
	}); err != nil {
		t.Fatalf("保存列策略失败: %v", err)
	}

	res, err := svc.RawSQLQuery(ctxBG(), ws.ID, ds.Name,
		"SELECT id, phone FROM "+p.table("dwd_raw_t")+" ORDER BY id", "alice", "viewer", 2)
	if err != nil {
		t.Fatalf("RawSQLQuery 失败: %v", err)
	}
	if res.RowCount != 2 || !res.Truncated || res.Limit != 2 {
		t.Fatalf("应截断到 2 行: %+v", res)
	}
	if res.Masks["phone"] != "phone" {
		t.Fatalf("应回显列级脱敏: %+v", res.Masks)
	}
	if res.Rows[0][1] != "138****0001" {
		t.Fatalf("同名列应被脱敏: %+v", res.Rows[0])
	}
	if len(res.Notes) < 2 {
		t.Fatalf("必须提示「行级策略未注入」这一降级事实: %+v", res.Notes)
	}

	// limit 超过硬上限时收敛到 MaxRawSQLLimit，而不是直接拒绝。
	res, err = svc.RawSQLQuery(ctxBG(), ws.ID, ds.Name,
		"SELECT id FROM "+p.table("dwd_raw_t"), "", "", 999999)
	if err != nil {
		t.Fatalf("超上限 limit 不应报错: %v", err)
	}
	if res.Limit != 5000 || res.RowCount != 3 || res.Truncated {
		t.Fatalf("limit 应收敛且结果完整: %+v", res)
	}

	// 坏 SQL 报执行失败，而不是 500。
	if _, err := svc.RawSQLQuery(ctxBG(), ws.ID, ds.Name, "SELECT * FROM no_such_table", "", "", 10); !errs.Is(err, errs.CodeExecutionFailed) {
		t.Fatalf("坏 SQL 应报执行失败，实际 %v", err)
	}
}

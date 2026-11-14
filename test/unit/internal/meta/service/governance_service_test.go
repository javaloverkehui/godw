package service_test

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/pkg/errs"
)

// 展示层脱敏是最后一道闸：类型覆盖要全，短值不能「脱了等于没脱」。
func TestMaskByType(t *testing.T) {
	cases := []struct {
		mask string
		in   any
		want string
	}{
		{"", "plain", "plain"},
		{"none", "plain", "plain"},
		{"full", "13800000000", "***"},
		{"partial", "123456789", "123***789"},
		{"partial", "ab", "***"},   // 太短时整体遮蔽，不能露出全部
		{"partial", "abc", "a*c"},  // 3 位保留首尾各 1
		{"hash", "alice", "sha1:"}, // 只暴露前缀，可关联不可逆
		{"email", "alice@example.com", "al***@example.com"},
		{"email", "a@b.com", "*@b.com"},
		{"email", "not-an-email", "***"},
		{"phone", "13800001234", "138****1234"},
		{"phone", "123", "***"},
		{"unknown", "keep", "keep"},
	}
	for _, c := range cases {
		got := service.Mask(c.mask, c.in)
		s := got.(string)
		if c.mask == "hash" {
			if !strings.HasPrefix(s, c.want) || len(s) != len("sha1:")+8 {
				t.Fatalf("service.Mask(hash)=%q 期望 %q+8 位", s, c.want)
			}
			continue
		}
		if s != c.want {
			t.Fatalf("service.Mask(%s,%v)=%q 期望 %q", c.mask, c.in, s, c.want)
		}
	}
	if service.Mask("full", nil) != nil {
		t.Fatal("nil 应原样返回，避免把 NULL 变成字符串 ***")
	}
}

// 列策略的键既要支持裸列名也要支持 table.column；表名过滤用于「只脱这张表」。
func TestMaskRowsAndColumnMasks(t *testing.T) {
	rows := [][]any{{"alice", "13800001234"}, {"bob", "13900001234"}}
	service.MaskRows([]string{"name", "phone"}, rows, map[string]string{"phone": "partial"})
	if rows[0][1] != "138*****234" || rows[0][0] != "alice" {
		t.Fatalf("按列脱敏不对: %+v", rows)
	}
	// 没有策略的列保持原值；空策略直接返回。
	service.MaskRows([]string{"name"}, rows, nil)
	if rows[0][0] != "alice" {
		t.Fatalf("无策略时不应改动: %+v", rows)
	}

	policies := []model.ColumnPolicy{
		{TableName: "dwd_order", ColumnName: "phone", MaskType: "partial"},
		{TableName: "dwd_other", ColumnName: "email", MaskType: "full"},
	}
	m := service.ColumnMasks(policies, "dwd_order")
	if m["phone"] != "partial" || m["dwd_order.phone"] != "partial" {
		t.Fatalf("应同时提供裸列名与限定键: %+v", m)
	}
	if _, ok := m["email"]; ok {
		t.Fatalf("表名过滤应排除其它表的策略: %+v", m)
	}
	// table 为空时不过滤，用于原始 SQL 模式（只有输出列名可用）。
	all := service.ColumnMasks(policies, "")
	if all["phone"] != "partial" || all["email"] != "full" {
		t.Fatalf("未限定表时应包含全部策略: %+v", all)
	}
}

// PreviewTable 是治理真正生效的路径：行级过滤 + 列级脱敏 + 生效策略回显。
func TestPreviewTableAppliesPolicies(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	p := newPhys(t)
	p.exec(t, `CREATE TABLE %s (dt date, id bigint, amount decimal(18,2), region text)`, p.table("dwd_prev_t"))
	p.exec(t, `INSERT INTO %s VALUES ('2026-08-01',1,100,'alice'),('2026-08-01',2,200,'bob')`, p.table("dwd_prev_t"))

	if err := svc.Repo.SaveColumnPolicy(ctxBG(), ws.ID, model.ColumnPolicy{
		TableName: "dwd_prev_t", ColumnName: "amount", Classification: "confidential", MaskType: "full",
	}); err != nil {
		t.Fatalf("保存列策略失败: %v", err)
	}
	if _, err := svc.Repo.SaveRowPolicy(ctxBG(), ws.ID, model.RowPolicy{
		TableName: "dwd_prev_t", Role: "viewer", FilterExpr: "region = '${user}'",
	}); err != nil {
		t.Fatalf("保存行策略失败: %v", err)
	}

	res, err := svc.PreviewTable(ctxBG(), ws.ID, ds.Name, p.schema, "dwd_prev_t", "alice", "viewer", 10)
	if err != nil {
		t.Fatalf("PreviewTable 失败: %v", err)
	}
	rows, _ := res["rows"].([][]any)
	if len(rows) != 1 {
		t.Fatalf("行级策略应只放行 alice 的行: %+v", rows)
	}
	if rows[0][2] != "***" {
		t.Fatalf("amount 应被完全脱敏: %+v", rows[0])
	}
	masks, _ := res["masks"].(map[string]string)
	if masks["amount"] != "full" {
		t.Fatalf("应回显实际生效的脱敏列: %+v", masks)
	}
	if filter, _ := res["filter"].(string); !strings.Contains(filter, "region = 'alice'") {
		t.Fatalf("行过滤未注入: %q", filter)
	}

	// 没有角色时不注入行策略，但列脱敏仍然生效。
	res, err = svc.PreviewTable(ctxBG(), ws.ID, ds.Name, p.schema, "dwd_prev_t", "", "", 10)
	if err != nil {
		t.Fatalf("无角色预览失败: %v", err)
	}
	rows, _ = res["rows"].([][]any)
	if len(rows) != 2 {
		t.Fatalf("无角色应看到全部行: %+v", rows)
	}
	// limit 越界回到默认，不报错。
	if _, err := svc.PreviewTable(ctxBG(), ws.ID, ds.Name, p.schema, "no_such_table", "", "", 99999); !errs.Is(err, errs.CodeExecutionFailed) {
		t.Fatalf("表不存在应报执行失败，实际 %v", err)
	}
}

// 策略命中测试的意义：配完当场能看到「到底会不会生效」，而不是等导出被拒。
func TestSimulatePolicies(t *testing.T) {
	svc, ws := newSvc(t)
	for _, p := range []model.ColumnPolicy{
		{TableName: "dwd.sim_t", ColumnName: "phone", Classification: "secret", MaskType: "phone"},
		{TableName: "dwd.sim_t", ColumnName: "amount", Classification: "internal", MaskType: "none"},
		{TableName: "other_t", ColumnName: "x", Classification: "secret", MaskType: "full"},
	} {
		if err := svc.Repo.SaveColumnPolicy(ctxBG(), ws.ID, p); err != nil {
			t.Fatalf("保存列策略失败: %v", err)
		}
	}
	if _, err := svc.Repo.SaveRowPolicy(ctxBG(), ws.ID, model.RowPolicy{
		TableName: "sim_t", Role: "viewer", FilterExpr: "region = '${user}'", Description: "只看本区域",
	}); err != nil {
		t.Fatalf("保存行策略失败: %v", err)
	}

	// 用裸表名命中 dwd.sim_t：全名与末段必须互相匹配，否则「配了却没命中」。
	sim, err := svc.SimulatePolicies(ctxBG(), ws.ID, "sim_t", "alice", "viewer")
	if err != nil {
		t.Fatalf("SimulatePolicies 失败: %v", err)
	}
	if sim.MaxLevel != "high" {
		t.Fatalf("最高分级应为 high（secret 归一化）: %+v", sim.MaxLevel)
	}
	if len(sim.Columns) != 2 || sim.Columns[0].Column != "amount" || sim.Columns[1].Column != "phone" {
		t.Fatalf("列应按名字排序且只含命中表: %+v", sim.Columns)
	}
	if !sim.Columns[1].Enforced || sim.Columns[1].Mask != "phone" {
		t.Fatalf("phone 应标记为已脱敏: %+v", sim.Columns[1])
	}
	if sim.Columns[0].Enforced {
		t.Fatalf("mask=none 不应标记为已脱敏: %+v", sim.Columns[0])
	}
	if len(sim.Rows) != 1 || !strings.Contains(sim.Rows[0].Rendered, "'alice'") {
		t.Fatalf("行策略应渲染占位符: %+v", sim.Rows)
	}
	if !strings.Contains(sim.Where, "region = 'alice'") {
		t.Fatalf("Where 不对: %q", sim.Where)
	}

	// 没有策略命中的表要给出可操作的提示，而不是空结果。
	empty, err := svc.SimulatePolicies(ctxBG(), ws.ID, "unknown_t", "bob", "viewer")
	if err != nil {
		t.Fatalf("SimulatePolicies 失败: %v", err)
	}
	if len(empty.Columns) != 0 || empty.MaxLevel != "none" || len(empty.Notes) < 2 {
		t.Fatalf("无策略时应给出提示: %+v", empty)
	}
	if _, err := svc.SimulatePolicies(ctxBG(), ws.ID, " ", "bob", "viewer"); !errs.Is(err, errs.CodeInvalidArgument) {
		t.Fatalf("空表名应被拒绝，实际 %v", err)
	}
}

// 列级血缘要在管道发布时自动建立，并能沿链路算出影响面。
func TestRecordColumnLineageAndImpact(t *testing.T) {
	svc, ws := newSvc(t)
	ds := newPgDatasource(t, svc, ws.ID)
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_src_line", "dw", "dwd_src_line"))
	publishModel(t, svc, ws.ID, modelSpec(ds.Name, "dwd_down_line", "dw", "dwd_down_line"))

	spec := model.PipelineSpec{
		Kind: "Pipeline", Name: "line_pipe",
		Nodes: []model.NodeSpec{
			{ID: "w1", Type: model.NodeSQL, Model: "dwd_down_line",
				SQL: "SELECT o.id AS id, o.amount AS amount FROM dw.dwd_src_line o"},
		},
	}
	publishPipeline(t, svc, ws.ID, spec)
	svc.RecordColumnLineage(ctxBG(), ws.ID, spec)

	edges, err := svc.Repo.ColumnLineage(ctxBG(), ws.ID, "dwd_down_line", "")
	if err != nil {
		t.Fatalf("ColumnLineage 失败: %v", err)
	}
	if len(edges) < 2 {
		t.Fatalf("应建立 id/amount 两条列级血缘: %+v", edges)
	}
	for _, e := range edges {
		// 物理表名必须被归一化成模型名，否则图接不起来。
		if e.UpstreamTable != "dwd_src_line" {
			t.Fatalf("上游表应归一化为模型名: %+v", e)
		}
	}

	impact, err := svc.Impact(ctxBG(), ws.ID, "dwd_src_line", "id", 5)
	if err != nil {
		t.Fatalf("Impact 失败: %v", err)
	}
	if impact["origin"] != "dwd_src_line.id" {
		t.Fatalf("origin 不对: %+v", impact)
	}
	tables, _ := impact["tables"].([]string)
	if !hasStr(tables, "dwd_down_line") {
		t.Fatalf("影响面应包含下游模型: %+v", impact)
	}
	if n, _ := impact["count"].(int); n < 1 {
		t.Fatalf("受影响列数应 >0: %+v", impact)
	}

	// 自引用不算血缘：dt 取自同名表同名列会形成自环。
	self := model.PipelineSpec{Kind: "Pipeline", Name: "self_pipe", Nodes: []model.NodeSpec{
		{ID: "w2", Type: model.NodeSQL, Model: "dwd_src_line",
			SQL: "SELECT s.dt AS dt, s.id AS id FROM dw.dwd_src_line s"},
	}}
	svc.RecordColumnLineage(ctxBG(), ws.ID, self)
	edges, _ = svc.Repo.ColumnLineage(ctxBG(), ws.ID, "dwd_src_line", "")
	for _, e := range edges {
		if e.UpstreamTable == e.DownstreamTable && e.UpstreamColumn == e.DownstreamColumn {
			t.Fatalf("自引用不应写入血缘: %+v", e)
		}
	}
}

// 服务方法版 service.MaskRows：读库里的策略并返回实际生效的列映射。
func TestMaskRowsFromRepo(t *testing.T) {
	svc, ws := newSvc(t)
	if err := svc.Repo.SaveColumnPolicy(ctxBG(), ws.ID, model.ColumnPolicy{
		TableName: "dwd_mask_t", ColumnName: "phone", Classification: "high", MaskType: "phone",
	}); err != nil {
		t.Fatalf("保存列策略失败: %v", err)
	}
	rows := [][]any{{"alice", "13800001234"}}
	out, applied := svc.MaskRows(ctxBG(), ws.ID, "dwd_mask_t", []string{"name", "phone"}, rows)
	if out[0][1] != "138****1234" || applied["phone"] != "phone" {
		t.Fatalf("脱敏结果不对: %+v %+v", out, applied)
	}
	// 没有策略的表原样返回，且不报错。
	rows2 := [][]any{{"x"}}
	out2, applied2 := svc.MaskRows(ctxBG(), ws.ID, "none_t", []string{"a"}, rows2)
	if out2[0][0] != "x" || len(applied2) != 0 {
		t.Fatalf("无策略应原样返回: %+v %+v", out2, applied2)
	}
}

// service.ApplyRowFilters（服务版）把策略渲染成带括号的条件列表，供指标/数据集注入。
func TestApplyRowFiltersService(t *testing.T) {
	svc, ws := newSvc(t)
	if _, err := svc.Repo.SaveRowPolicy(ctxBG(), ws.ID, model.RowPolicy{
		TableName: "dwd_rls_t", Role: "viewer", FilterExpr: "region = '${user}'",
	}); err != nil {
		t.Fatalf("保存行策略失败: %v", err)
	}
	conds, err := svc.ApplyRowFilters(ctxBG(), ws.ID, "dwd_rls_t", "alice", "viewer")
	if err != nil || len(conds) != 1 {
		t.Fatalf("应返回一条条件: %v %v", conds, err)
	}
	if conds[0] != "((region = 'alice'))" {
		t.Fatalf("条件应双层括号且渲染占位符: %q", conds[0])
	}
	// 通配角色 '*' 对任意角色生效，而不匹配的角色看不到任何条件。
	if _, err := svc.Repo.SaveRowPolicy(ctxBG(), ws.ID, model.RowPolicy{
		TableName: "dwd_rls_wild", Role: "*", FilterExpr: "region = '${user}'",
	}); err != nil {
		t.Fatalf("保存通配行策略失败: %v", err)
	}
	conds, err = svc.ApplyRowFilters(ctxBG(), ws.ID, "dwd_rls_wild", "bob", "admin")
	if err != nil || len(conds) != 1 || !strings.Contains(conds[0], "'bob'") {
		t.Fatalf("通配角色应命中: %v %v", conds, err)
	}
	if got, err := svc.ApplyRowFilters(ctxBG(), ws.ID, "dwd_rls_t", "bob", "admin"); err != nil || len(got) != 0 {
		t.Fatalf("角色不匹配时不应有过滤: %v %v", got, err)
	}
	// 没有角色时直接返回 nil，不查库。
	if got, err := svc.ApplyRowFilters(ctxBG(), ws.ID, "dwd_rls_t", "u", ""); err != nil || got != nil {
		t.Fatalf("空角色应返回 nil: %v %v", got, err)
	}
}

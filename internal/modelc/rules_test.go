package modelc

import (
	"strings"
	"testing"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
)

// T0303 验收：构造 12 种违规引用，全部被拒且错误信息说明「哪条规则、哪个字段」。
func TestLayerRulesReject(t *testing.T) {
	rules := DefaultRules()
	dwd := model.ModelSpec{
		Kind: "Model", Name: "dwd_order", Layer: "dwd", Datasource: "pg",
		Columns: []model.ColumnSpec{
			{Name: "order_id", Type: "bigint"},
			{Name: "dt", Type: "date"},
		},
		PrimaryKey: []string{"order_id"},
	}

	cases := []struct {
		name string
		spec model.ModelSpec
		refs []string
		up   map[string]string
		rule string
	}{
		{
			name: "表名缺层前缀",
			spec: func() model.ModelSpec { s := dwd; s.Name = "order_detail"; return s }(),
			rule: "layer_name",
		},
		{
			name: "表名大驼峰",
			spec: func() model.ModelSpec { s := dwd; s.Name = "dwd_Order"; return s }(),
			rule: "layer_name",
		},
		{
			name: "缺 dt 列",
			spec: func() model.ModelSpec {
				s := dwd
				s.Columns = []model.ColumnSpec{{Name: "order_id", Type: "bigint"}}
				return s
			}(),
			rule: "required_column",
		},
		{
			name: "dt 类型错误",
			spec: func() model.ModelSpec {
				s := dwd
				s.Columns = []model.ColumnSpec{
					{Name: "order_id", Type: "bigint"},
					{Name: "dt", Type: "string"},
				}
				return s
			}(),
			rule: "dt_type",
		},
		{
			name: "缺粒度声明",
			spec: func() model.ModelSpec { s := dwd; s.PrimaryKey = nil; return s }(),
			rule: "require_key",
		},
		{
			name: "dwd 引用 dws（反向依赖）",
			spec: dwd, refs: []string{"dws_order_daily"},
			up:   map[string]string{"dws_order_daily": "dws"},
			rule: "reverse_dependency",
		},
		{
			name: "dwd 引用 ads（反向依赖）",
			spec: dwd, refs: []string{"ads_dashboard"},
			up:   map[string]string{"ads_dashboard": "ads"},
			rule: "reverse_dependency",
		},
		{
			name: "dws 引用 ods（跨层）",
			spec: func() model.ModelSpec { s := dwd; s.Name = "dws_x"; s.Layer = "dws"; return s }(),
			refs: []string{"ods_order"},
			up:   map[string]string{"ods_order": "ods"},
			rule: "layer_ref",
		},
		{
			name: "引用未知模型",
			spec: dwd, refs: []string{"mystery_table"},
			up:   map[string]string{},
			rule: "unknown_ref",
		},
		{
			name: "ads 引用 ods",
			spec: func() model.ModelSpec { s := dwd; s.Name = "ads_x"; s.Layer = "ads"; return s }(),
			refs: []string{"ods_order"},
			up:   map[string]string{"ods_order": "ods"},
			rule: "layer_ref",
		},
		{
			name: "dim 缺主键",
			spec: func() model.ModelSpec {
				s := dwd
				s.Name = "dim_user"
				s.Layer = "dim"
				s.PrimaryKey = nil
				return s
			}(),
			rule: "require_key",
		},
		{
			name: "ods 引用 ods 之上的层",
			spec: func() model.ModelSpec {
				return model.ModelSpec{
					Kind: "Model", Name: "ods_order", Layer: "ods", Datasource: "pg",
					Columns: []model.ColumnSpec{
						{Name: "id", Type: "bigint"},
						{Name: "dt", Type: "date"},
						{Name: "_godw_src", Type: "string"},
						{Name: "_godw_load_at", Type: "datetime"},
					},
				}
			}(),
			refs: []string{"dwd_order"},
			up:   map[string]string{"dwd_order": "dwd"},
			rule: "reverse_dependency",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var vs []RuleViolation
			vs = append(vs, rules.Check(c.spec)...)
			if c.refs != nil {
				vs = append(vs, rules.CheckRefs(c.spec, c.refs, c.up)...)
			}
			found := false
			for _, v := range vs {
				if v.Rule == c.rule {
					found = true
					if v.Field == "" {
						t.Errorf("违规项缺少字段定位: %+v", v)
					}
				}
			}
			if !found {
				t.Fatalf("期望命中规则 %s，实际 %+v", c.rule, vs)
			}
			if err := Enforce(vs); err == nil {
				t.Fatal("Enforce 应该返回错误")
			}
		})
	}
}

func TestLayerRulesPass(t *testing.T) {
	spec := model.ModelSpec{
		Kind: "Model", Name: "dwd_order", Layer: "dwd", Datasource: "pg",
		Columns: []model.ColumnSpec{
			{Name: "order_id", Type: "bigint"},
			{Name: "dt", Type: "date"},
		},
		PrimaryKey: []string{"order_id"},
	}
	if vs := DefaultRules().Check(spec); len(vs) != 0 {
		t.Fatalf("应通过，实际 %+v", vs)
	}
	vs := DefaultRules().CheckRefs(spec, []string{"ods_order"}, map[string]string{"ods_order": "ods"})
	if len(vs) != 0 {
		t.Fatalf("dwd←ods 应放行，实际 %+v", vs)
	}
}

// T0304 验收：分区表达式跨引擎翻译；粒度不得细于小时；TTL 格式。
func TestPartitionCompile(t *testing.T) {
	cases := []struct {
		raw    string
		kind   string
		column string
		ch     string
		pg     string
	}{
		{"dt", "column", "dt", "`dt`", `"dt"`},
		{"date_trunc('month', dt)", "month", "dt", "toYYYYMM(`dt`)", `date_trunc('month', "dt")`},
		{"toYYYYMM(dt)", "month", "dt", "toYYYYMM(`dt`)", `date_trunc('month', "dt")`},
		{"toDate(created_at)", "day", "created_at", "toDate(`created_at`)", `date_trunc('day', "created_at")`},
	}
	for _, c := range cases {
		p, err := ParsePartition(c.raw)
		if err != nil {
			t.Fatalf("%s 解析失败: %v", c.raw, err)
		}
		if p.Kind != c.kind || p.Column != c.column {
			t.Fatalf("%s 解析结果 %+v", c.raw, p)
		}
		ch, err := CompilePartition(p, dialect.ClickHouse{})
		if err != nil || ch != c.ch {
			t.Fatalf("%s CH 编译 %q err=%v", c.raw, ch, err)
		}
		pg, err := CompilePartition(p, dialect.Postgres{})
		if err != nil || pg != c.pg {
			t.Fatalf("%s PG 编译 %q err=%v", c.raw, pg, err)
		}
	}
}

func TestPartitionRejects(t *testing.T) {
	if _, err := ParsePartition("date_trunc('week', dt)"); err == nil {
		t.Fatal("week 粒度应被拒绝")
	}
	if _, err := ParsePartition("dt + 1"); err == nil {
		t.Fatal("表达式应被拒绝")
	}
	if _, err := ParsePartition("date_trunc('month', dt)"); err != nil {
		t.Fatal(err)
	}
	// MySQL 不支持表达式分区
	p, _ := ParsePartition("date_trunc('month', dt)")
	if _, err := CompilePartition(p, dialect.MySQL{}); err == nil {
		t.Fatal("MySQL 表达式分区应报错")
	}
}

func TestTTL(t *testing.T) {
	for _, c := range []struct {
		raw  string
		unit string
		val  int
	}{
		{"30d", "day", 30}, {"12m", "month", 12}, {"3y", "year", 3},
	} {
		ttl, err := ParseTTL(c.raw)
		if err != nil || ttl.Unit != c.unit || ttl.Value != c.val {
			t.Fatalf("%s -> %+v err=%v", c.raw, ttl, err)
		}
	}
	if _, err := ParseTTL("30 days"); err == nil {
		t.Fatal("非法 TTL 应报错")
	}
	ttl, _ := ParseTTL("30d")
	if got := CompileTTL(ttl, "dt", dialect.ClickHouse{}); got != "TTL `dt` + INTERVAL 30 DAY" {
		t.Fatalf("CH TTL 编译 %q", got)
	}
	if got := CompileTTL(ttl, "dt", dialect.Postgres{}); got != "" {
		t.Fatalf("PG 不应有原生 TTL，实际 %q", got)
	}
}

func TestValidateStorage(t *testing.T) {
	spec := model.ModelSpec{
		Name: "dwd_x", Layer: "dwd",
		Columns: []model.ColumnSpec{
			{Name: "id", Type: "bigint"},
			{Name: "dt", Type: "date"},
			{Name: "hour", Type: "string", Nullable: true},
		},
		PartitionBy: []string{"date_trunc('hour', dt)", "hour", "missing"},
		TTL:         "5x",
	}
	vs := ValidateStorage(spec)
	rules := map[string]bool{}
	for _, v := range vs {
		rules[v.Rule] = true
	}
	for _, want := range []string{"partition_too_fine", "partition_nullable", "partition_column", "ttl_format"} {
		if !rules[want] {
			t.Fatalf("缺少规则 %s，实际 %+v", want, vs)
		}
	}
}

// T0305 验收：矩阵每个单元格都有映射；有损转换未声明时拒绝。
func TestMatrixComplete(t *testing.T) {
	for _, engine := range dialect.MatrixEngines() {
		for _, lt := range dialect.MatrixLogicalTypes() {
			ts := dialect.TypeSpec{Logical: lt, Elem: dialect.LString, Precision: 20, Scale: 4}
			m, err := dialect.MapType(engine, ts)
			if err != nil {
				t.Fatalf("%s/%s 映射失败: %v", engine, lt, err)
			}
			if m.Physical == "" {
				t.Fatalf("%s/%s 物理类型为空", engine, lt)
			}
		}
	}
}

func TestMatrixLossy(t *testing.T) {
	m, err := dialect.MapType("mysql", dialect.TypeSpec{Logical: dialect.LArray, Elem: dialect.LString})
	if err != nil || !m.Lossy {
		t.Fatalf("MySQL array 应标记有损: %+v err=%v", m, err)
	}
	m, err = dialect.MapType("postgres", dialect.TypeSpec{Logical: dialect.LDecimal, Precision: 50, Scale: 10})
	if err != nil || !m.Lossy {
		t.Fatalf("PG decimal(50,10) 应标记有损: %+v err=%v", m, err)
	}

	spec := model.ModelSpec{
		Name: "dwd_x", Layer: "dwd",
		Columns: []model.ColumnSpec{
			{Name: "id", Type: "bigint"},
			{Name: "attrs", Type: "array<string>"},
			{Name: "dt", Type: "date"},
		},
	}
	vs := ValidateTypes(spec, "mysql")
	if len(vs) == 0 || vs[0].Rule != "lossy_type" {
		t.Fatalf("未声明 allow_lossy 应报 lossy_type，实际 %+v", vs)
	}
	spec.AllowLossy = true
	if vs := ValidateTypes(spec, "mysql"); len(vs) != 0 {
		t.Fatalf("声明后应放行，实际 %+v", vs)
	}
}

func TestReverseMap(t *testing.T) {
	cases := []struct{ engine, physical, logical string }{
		{"postgres", "varchar(64)", "string"},
		{"postgres", "numeric(18,2)", "decimal"},
		{"postgres", "timestamptz", "datetime"},
		{"postgres", "bigint", "bigint"},
		{"mysql", "tinyint(1)", "bool"},
		{"mysql", "datetime", "datetime"},
		{"clickhouse", "Nullable(String)", "string"},
		{"clickhouse", "Decimal(20, 4)", "decimal"},
		{"clickhouse", "DateTime64(3)", "datetime"},
		{"duckdb", "BIGINT", "bigint"},
	}
	for _, c := range cases {
		got := dialect.ReverseMap(c.engine, c.physical)
		if got.Logical != c.logical {
			t.Fatalf("%s %s → %s，期望 %s", c.engine, c.physical, got.Logical, c.logical)
		}
	}
}

// T0306 验收：删列判 breaking；加列生成正确 ALTER。
func TestDiffAndPlan(t *testing.T) {
	old := model.ModelSpec{
		Name: "dwd_order", Layer: "dwd", Schema: "dwd",
		Columns: []model.ColumnSpec{
			{Name: "order_id", Type: "bigint"},
			{Name: "amount", Type: "decimal"},
			{Name: "dt", Type: "date"},
		},
		PrimaryKey:  []string{"order_id"},
		PartitionBy: []string{"dt"},
	}
	newSpec := model.ModelSpec{
		Name: "dwd_order", Layer: "dwd", Schema: "dwd",
		Columns: []model.ColumnSpec{
			{Name: "order_id", Type: "bigint"},
			{Name: "dt", Type: "date"},
			{Name: "channel", Type: "string", Nullable: true, Comment: "渠道"},
		},
		PrimaryKey:  []string{"order_id"},
		PartitionBy: []string{"dt"},
	}

	d := Diff(old, newSpec)
	if !d.HasBreaking() {
		t.Fatal("删列应判为 breaking")
	}
	var drop, add *Change
	for i := range d.Changes {
		switch d.Changes[i].Kind {
		case ChangeDropColumn:
			drop = &d.Changes[i]
		case ChangeAddColumn:
			add = &d.Changes[i]
		}
	}
	if drop == nil || !drop.Breaking {
		t.Fatalf("缺少删列 breaking: %+v", d.Changes)
	}
	if add == nil || add.Breaking {
		t.Fatalf("可空加列应为 safe: %+v", d.Changes)
	}

	plan := PlanMigration(old, newSpec, dialect.Postgres{})
	joined := strings.Join(plan.Statements, "\n")
	if !strings.Contains(joined, `ADD COLUMN "channel"`) {
		t.Fatalf("缺少 ADD COLUMN: %s", joined)
	}
	if !strings.Contains(joined, `DROP COLUMN "amount"`) {
		t.Fatalf("缺少 DROP COLUMN: %s", joined)
	}
	if !strings.Contains(joined, `COMMENT ON COLUMN "dwd"."dwd_order"."channel"`) {
		t.Fatalf("缺少注释同步: %s", joined)
	}

	// 非空无默认值加列 → breaking。
	hard := newSpec
	hard.Columns = append([]model.ColumnSpec{}, newSpec.Columns...)
	hard.Columns = append(hard.Columns, model.ColumnSpec{Name: "must", Type: "string"})
	d2 := Diff(old, hard)
	found := false
	for _, c := range d2.Changes {
		if c.Column == "must" && c.Kind == ChangeAddColumn && c.Breaking {
			found = true
		}
	}
	if !found {
		t.Fatal("非空无默认值加列应 breaking")
	}

	// 主键变更 → breaking；partition 变更 → 需要重建。
	pk := old
	pk.PrimaryKey = []string{"order_id", "dt"}
	if d3 := Diff(old, pk); !d3.HasBreaking() {
		t.Fatal("主键变更应 breaking")
	}
	part := old
	part.PartitionBy = []string{"date_trunc('month', dt)"}
	plan3 := PlanMigration(old, part, dialect.Postgres{})
	if !plan3.Rebuild {
		t.Fatal("分区变更应要求重建")
	}
}

func TestCompileMaterialization(t *testing.T) {
	base := model.ModelSpec{Name: "dws_x", Layer: "dws", Query: "SELECT 1"}
	base.Materialization = "view"
	if _, err := CompileMaterialization(base, dialect.Postgres{}, MaterializationCaps{SupportsMV: true}); err != nil {
		t.Fatalf("view 应通过: %v", err)
	}
	base.PartitionBy = []string{"dt"}
	if _, err := CompileMaterialization(base, dialect.Postgres{}, MaterializationCaps{SupportsMV: true}); err == nil {
		t.Fatal("view 配分区应报错")
	}

	mv := model.ModelSpec{Name: "dws_x", Layer: "dws", Materialization: "mv", Query: "SELECT 1"}
	if _, err := CompileMaterialization(mv, dialect.MySQL{}, MaterializationCaps{SupportsMV: false}); err == nil {
		t.Fatal("MySQL mv 应给出明确错误")
	} else if !strings.Contains(err.Error(), "建议") {
		t.Fatalf("错误应包含替代方案: %v", err)
	}

	inc := model.ModelSpec{Name: "dws_x", Layer: "dws", Materialization: "incremental"}
	if _, err := CompileMaterialization(inc, dialect.Postgres{}, MaterializationCaps{}); err == nil {
		t.Fatal("incremental 无分区应报错")
	}
}

// T0309 验收：从源表生成 YAML 后 Validate 直接通过；字段顺序与源表一致。
func TestScaffold(t *testing.T) {
	res, err := Scaffold(ScaffoldOptions{
		Layer: "dwd", Name: "order_detail", Datasource: "pg_dw", Engine: "postgres",
		Upstream: []model.ColumnSpec{
			{Name: "order_id", Type: "bigint"},
			{Name: "user_id", Type: "bigint"},
			{Name: "amount", Type: "numeric(18,2)"},
			{Name: "status", Type: "varchar(32)"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Spec.Name != "dwd_order_detail" {
		t.Fatalf("应自动补层前缀，实际 %s", res.Spec.Name)
	}
	if len(res.Spec.PrimaryKey) == 0 || res.Spec.PrimaryKey[0] != "order_id" {
		t.Fatalf("主键推断错误: %+v", res.Spec.PrimaryKey)
	}
	// 字段顺序：dt 在最前，其余保持源表顺序。
	if res.Spec.Columns[0].Name != "dt" {
		t.Fatalf("dt 应在首位，实际 %+v", res.Spec.Columns[0])
	}
	if res.Spec.Columns[1].Name != "order_id" || res.Spec.Columns[3].Name != "amount" {
		t.Fatalf("源表顺序被打乱: %+v", res.Spec.Columns)
	}
	if err := Validate(res.Spec); err != nil {
		t.Fatalf("脚手架产物应通过 Validate: %v", err)
	}
	if vs := DefaultRules().Check(res.Spec); len(vs) != 0 {
		t.Fatalf("脚手架产物应合规: %+v", vs)
	}
	if !strings.Contains(res.YAML, "kind: Model") {
		t.Fatalf("YAML 输出异常: %s", res.YAML)
	}
}

// 源表 DDL 已经写了 PRIMARY KEY，脚手架就该直接用它，
// 而不是拿列名启发式盖过去（ref_id 的列名更像主键，但源表主键是 contract_sn + tenant_id）。
func TestScaffoldUsesSourcePrimaryKey(t *testing.T) {
	res, err := Scaffold(ScaffoldOptions{
		Layer: "dwd", Name: "contract", Datasource: "dw", Engine: "mysql",
		Upstream: []model.ColumnSpec{
			{Name: "ref_id", Type: "bigint"},
			{Name: "contract_sn", Type: "varchar(64)", PrimaryKey: true},
			{Name: "tenant_id", Type: "bigint", PrimaryKey: true},
			{Name: "amount", Type: "decimal(18,2)"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(res.Spec.PrimaryKey, ","); got != "contract_sn,tenant_id" {
		t.Fatalf("应采用源表主键 contract_sn,tenant_id，实际 %v", res.Spec.PrimaryKey)
	}
	if got := strings.Join(res.Spec.UniqueKey, ","); got != "contract_sn,tenant_id" {
		t.Fatalf("unique_key 应与主键一致，实际 %v", res.Spec.UniqueKey)
	}
	if len(res.KeyCandidates) == 0 || res.KeyCandidates[0] != "contract_sn" {
		t.Fatalf("源表主键应排在候选首位，实际 %v", res.KeyCandidates)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("有源表主键时不该再告警: %v", res.Warnings)
	}
}

// ODS 保原始、不建主键约束，所以「没有主键」对 ODS 不是问题，不该告警。
// 平台自己注入的审计列（_godw_ 前缀）也不该逼用户去点 allow_lossy。
func TestScaffoldODSHasNoKeyNoise(t *testing.T) {
	res, err := Scaffold(ScaffoldOptions{
		Layer: "ods", Name: "contract", Datasource: "dw", Engine: "mysql",
		Upstream: []model.ColumnSpec{
			{Name: "contract_no", Type: "varchar(64)", PrimaryKey: true},
			{Name: "amount", Type: "decimal(18,2)"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Spec.PrimaryKey) != 0 {
		t.Fatalf("ODS 不应带主键约束，实际 %v", res.Spec.PrimaryKey)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("ODS 脚手架不应产生告警: %v", res.Warnings)
	}
}

// T0310：字典导出包含类型、口径、上下游。
func TestDictionary(t *testing.T) {
	specs := []model.ModelSpec{
		{
			Name: "dwd_order", Layer: "dwd", Schema: "dwd", Description: "订单明细",
			Owner: "kenny", Tags: []string{"core"}, PrimaryKey: []string{"order_id"},
			Columns: []model.ColumnSpec{
				{Name: "order_id", Type: "bigint", Comment: "订单号"},
				{Name: "dt", Type: "date", Comment: "分区日期"},
			},
		},
	}
	md := DictionaryMarkdown(BuildDictionary(DictInput{
		Specs:      specs,
		Upstream:   map[string][]string{"dwd_order": {"ods_order"}},
		Downstream: map[string][]string{"dwd_order": {"dws_order_daily"}},
		Policies: map[string]map[string]model.ColumnPolicy{
			"dwd_order": {"order_id": {Classification: "low", MaskType: "hash"}},
		},
		Engine: "postgres",
	}))
	for _, want := range []string{"# 数据字典", "dwd_order", "订单号", "bigint", "上游：ods_order", "下游：dws_order_daily", "hash"} {
		if !strings.Contains(md, want) {
			t.Fatalf("字典缺少 %q:\n%s", want, md)
		}
	}
}

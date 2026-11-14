package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/godw/godw/internal/config"
	"github.com/godw/godw/internal/connector"
	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/meta/repo"
	"github.com/godw/godw/internal/meta/service"
	"github.com/godw/godw/internal/modelc"
	"github.com/godw/godw/internal/obs/log"
	pipelinecompile "github.com/godw/godw/internal/pipeline/compile"
	"github.com/godw/godw/internal/semantic"

	_ "github.com/godw/godw/internal/connector/clickhouse"
	_ "github.com/godw/godw/internal/connector/duckdb"
	_ "github.com/godw/godw/internal/connector/mysql"
	_ "github.com/godw/godw/internal/connector/postgres"
	"gopkg.in/yaml.v3"
)

const usage = `godw-cli — GoDW 运维命令行

用法:
  godw-cli <command> [flags]

命令:
  migrate        执行数据库迁移
  doctor         检查配置、数据库、连接器与 schema 状态
  apply -f FILE  提交模型/管道/指标/看板定义（按 kind 自动识别）
  run            触发一条管道
  init           从源表生成模型脚手架（init model）
  diff           对比模型当前发布版本与本地文件
  export-doc     导出数据字典（markdown）
  lineage        查看表级/列级血缘与影响分析
  profile        对表做画像并给出推荐质量规则
  export         导出指标或看板为 CSV
  lint           校验目录下所有 YAML 定义（不落库）
  worker         作为数据面进程运行（--once 只跑一轮）
  version        打印版本

示例:
  godw-cli migrate --config config.yaml
  godw-cli apply -f examples/model_dwd_order.yaml --publish --apply
  godw-cli run --pipeline dwd_order_daily --date 2025-01-01
  godw-cli init model --layer dwd --from order --name order_detail --datasource pg_dw
  godw-cli diff -f examples/02_model_dwd_order.yaml
  godw-cli export-doc -o docs/dictionary.md
`

// 发布构建用 -ldflags "-X main.version=v1.0.0" 覆盖，源码里保留当前发布号。
var version = "1.0.0"

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "migrate":
		err = cmdMigrate(args)
	case "doctor":
		err = cmdDoctor(args)
	case "apply":
		err = cmdApply(args)
	case "run":
		err = cmdRun(args)
	case "worker":
		err = cmdWorker(args)
	case "seed":
		err = cmdSeed(args)
	case "backfill":
		err = cmdBackfill(args)
	case "init":
		err = cmdInit(args)
	case "diff":
		err = cmdDiff(args)
	case "export-doc":
		err = cmdExportDoc(args)
	case "lineage":
		err = cmdLineage(args)
	case "profile":
		err = cmdProfile(args)
	case "collect":
		err = cmdCollect(args)
	case "export":
		err = cmdExport(args)
	case "lint":
		err = cmdLint(args)
	case "version", "-v", "--version":
		fmt.Println("godw-cli", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func setup(args []string) (*service.Service, config.Config, func(), error) {
	fs := flag.NewFlagSet("godw", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return nil, config.Config{}, nil, err
	}
	res, err := config.Load(config.Loader{Path: *cfgPath})
	if err != nil {
		return nil, config.Config{}, nil, err
	}
	lg := log.New(res.Config.Log)
	db, err := repo.Open(context.Background(), res.Config.DB)
	if err != nil {
		return nil, config.Config{}, nil, err
	}
	svc, err := service.New(db, res.Config, lg)
	if err != nil {
		db.Close()
		return nil, config.Config{}, nil, err
	}
	return svc, res.Config, db.Close, nil
}

func cmdMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	checkOnly := fs.Bool("check", false, "只检查是否有待执行的迁移，不执行")
	if err := fs.Parse(args); err != nil {
		return err
	}
	res, err := config.Load(config.Loader{Path: *cfgPath})
	if err != nil {
		return err
	}
	db, err := repo.Open(context.Background(), res.Config.DB)
	if err != nil {
		return err
	}
	defer db.Close()
	if *checkOnly {
		pending, err := db.PendingMigrations(context.Background())
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			fmt.Println("schema 已是最新")
			return nil
		}
		fmt.Printf("待执行迁移 %d 个:\n", len(pending))
		for _, p := range pending {
			fmt.Println("  -", p)
		}
		return fmt.Errorf("存在待执行的迁移")
	}
	applied, err := db.Migrate(context.Background())
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		fmt.Println("schema 已是最新，无需迁移")
		return nil
	}
	fmt.Printf("已应用 %d 个迁移: %s\n", len(applied), strings.Join(applied, ", "))
	return nil
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	if err := fs.Parse(args); err != nil {
		return err
	}
	res, err := config.Load(config.Loader{Path: *cfgPath})
	if err != nil {
		return err
	}
	fmt.Println("✓ 配置校验通过")
	fmt.Printf("  env=%s db=%s\n", res.Config.Env, res.Config.DB.Redacted())

	db, err := repo.Open(context.Background(), res.Config.DB)
	if err != nil {
		return err
	}
	defer db.Close()
	fmt.Println("✓ 元数据库可连接")

	v, n, err := db.Version(context.Background())
	if err != nil {
		return err
	}
	if n == 0 {
		fmt.Println("! schema 未初始化，请执行 godw-cli migrate")
	} else {
		fmt.Printf("✓ schema 版本 %s（%d 个迁移）\n", v, n)
	}

	fmt.Printf("✓ 已注册连接器: %s\n", strings.Join(connector.Types(), ", "))

	svc, err := service.New(db, res.Config, log.New(res.Config.Log))
	if err != nil {
		return err
	}
	list, err := svc.Repo.ListDatasources(context.Background(), "default")
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("! 尚未接入任何数据源")
		return nil
	}
	fmt.Printf("✓ 数据源 %d 个\n", len(list))
	fail := 0
	for _, ds := range list {
		if err := svc.TestDatasource(context.Background(), ds.WorkspaceID, ds.Name); err != nil {
			fmt.Printf("  ✗ %s (%s): %v\n", ds.Name, ds.Type, err)
			fail++
			continue
		}
		fmt.Printf("  ✓ %s (%s)\n", ds.Name, ds.Type)
	}
	if fail > 0 {
		return fmt.Errorf("%d 个数据源连接失败", fail)
	}
	return nil
}

func cmdApply(args []string) error {
	fs := flag.NewFlagSet("apply", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	file := fs.String("f", "", "YAML 文件路径")
	publish := fs.Bool("publish", true, "是否立即发布")
	applyDDL := fs.Bool("apply", false, "模型是否执行建表 DDL")
	force := fs.Bool("force", false, "允许破坏性结构变更（删列/改类型/改分区）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("缺少 -f 参数")
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}

	var head struct {
		Kind string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(raw, &head); err != nil {
		return err
	}

	svc, cfg, closeFn, err := setup([]string{"--config", *cfgPath})
	if err != nil {
		return err
	}
	defer closeFn()

	ws, err := svc.Bootstrap(context.Background())
	if err != nil {
		return err
	}

	switch head.Kind {
	case "Model":
		m, err := svc.ApplyModel(context.Background(), ws.ID, "cli", raw, *publish, *applyDDL, *force)
		if err != nil {
			return err
		}
		fmt.Printf("模型 %s 已保存，当前版本 v%d\n", m.Name, m.ActiveVersion)
		if m.DDL != "" && !*applyDDL {
			fmt.Println("\n生成的 DDL（未执行）:")
			fmt.Println(m.DDL)
		}
	case "Metric":
		var m semantic.Metric
		if err := yaml.Unmarshal(raw, &m); err != nil {
			return err
		}
		saved, err := svc.ApplyMetric(context.Background(), ws.ID, m)
		if err != nil {
			return err
		}
		fmt.Printf("指标 %s 已保存（%s）\n", saved.Name, saved.DisplayName)
	case "Dashboard":
		var d semantic.Dashboard
		if err := yaml.Unmarshal(raw, &d); err != nil {
			return err
		}
		saved, err := svc.ApplyDashboard(context.Background(), ws.ID, d)
		if err != nil {
			return err
		}
		fmt.Printf("看板 %s 已保存（%d 个卡片）\n", saved.Name, len(saved.Cards))
	case "Pipeline":
		p, err := svc.ApplyPipeline(context.Background(), ws.ID, "cli", raw, *publish)
		if err != nil {
			return err
		}
		fmt.Printf("管道 %s 已保存，当前版本 v%d\n", p.Name, p.ActiveVersion)
	case "Dataset":
		var d semantic.Dataset
		if err := yaml.Unmarshal(raw, &d); err != nil {
			return err
		}
		saved, err := svc.ApplyDataset(context.Background(), ws.ID, d)
		if err != nil {
			return err
		}
		fmt.Printf("数据集 %s 已保存（%d 维度 / %d 度量）\n", saved.Name, len(saved.Dimensions), len(saved.Measures))
	case "Chart":
		var c semantic.Chart
		if err := yaml.Unmarshal(raw, &c); err != nil {
			return err
		}
		saved, warnings, err := svc.ApplyChart(context.Background(), ws.ID, c)
		if err != nil {
			return err
		}
		fmt.Printf("图表 %s 已保存（%s）\n", saved.Name, saved.Type)
		// 影响面提醒：改图表不用重跑数据，但会改变业务看到的数字口径。
		for _, w := range warnings {
			fmt.Println("  注意:", w)
		}
	default:
		return fmt.Errorf("不支持的 kind: %q（应为 Model / Pipeline / Metric / Dashboard / Dataset / Chart）", head.Kind)
	}
	_ = cfg
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	pipeline := fs.String("pipeline", "", "管道名")
	bizDate := fs.String("date", "", "业务日期 YYYY-MM-DD，默认今天")
	wait := fs.Bool("wait", false, "等待执行完成")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pipeline == "" {
		return fmt.Errorf("缺少 --pipeline 参数")
	}
	svc, cfg, closeFn, err := setup([]string{"--config", *cfgPath})
	if err != nil {
		return err
	}
	defer closeFn()
	ws, err := svc.Bootstrap(context.Background())
	if err != nil {
		return err
	}
	inst, created, err := svc.Trigger(context.Background(), ws.ID, *pipeline, *bizDate, "manual", nil)
	if err != nil {
		return err
	}
	if created {
		fmt.Printf("已创建实例 %s（业务日期 %s）\n", inst.ID, inst.BizDate)
	} else {
		fmt.Printf("复用已有实例 %s\n", inst.ID)
	}
	if !*wait {
		return nil
	}
	ran, err := svc.RunOnce(context.Background(), "cli")
	if err != nil {
		return err
	}
	if !ran {
		fmt.Println("没有可领取的实例")
		return nil
	}
	final, err := svc.Repo.GetInstance(context.Background(), inst.ID)
	if err != nil {
		return err
	}
	fmt.Printf("执行结束: %s\n", final.Status)
	for _, t := range final.Tasks {
		fmt.Printf("  %-24s %-10s %s\n", t.NodeID, t.Status, t.Error)
	}
	_ = cfg
	return nil
}

func cmdWorker(args []string) error {
	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	once := fs.Bool("once", false, "只执行一轮")
	name := fs.String("name", "cli-worker", "worker 名称")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, _, closeFn, err := setup([]string{"--config", *cfgPath})
	if err != nil {
		return err
	}
	defer closeFn()
	for {
		ran, err := svc.RunOnce(context.Background(), *name)
		if err != nil {
			fmt.Fprintln(os.Stderr, "执行失败:", err)
		}
		if *once || !ran {
			if !ran {
				fmt.Println("没有待执行实例")
			}
			return nil
		}
	}
}

var _ = model.NodeSource

// cmdSeed 在指定数据源执行样例数据脚本，用于本地一键复现。
func cmdSeed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	dsName := fs.String("datasource", "pg_dw", "目标数据源名称")
	file := fs.String("f", "examples/seed.sql", "SQL 文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	svc, _, closeFn, err := setup([]string{"--config", *cfgPath})
	if err != nil {
		return err
	}
	defer closeFn()

	ws, err := svc.Bootstrap(context.Background())
	if err != nil {
		return err
	}
	// 数据源不存在时按配置里的元数据库信息自动接入，省去手工一步。
	if _, _, err := svc.Repo.DatasourceByName(context.Background(), ws.ID, *dsName); err != nil {
		_, cerr := svc.CreateDatasource(context.Background(), ws.ID, *dsName, "postgres", map[string]any{
			"host": svc.Cfg.DB.Host, "port": svc.Cfg.DB.Port,
			"database": svc.Cfg.DB.Database, "user": svc.Cfg.DB.User,
		}, map[string]string{"password": svc.Cfg.DB.Password})
		if cerr != nil {
			return cerr
		}
		fmt.Println("已接入数据源", *dsName)
	}

	spec, err := svc.DatasourceSpec(context.Background(), ws.ID, *dsName)
	if err != nil {
		return err
	}
	c, err := svc.Pool.Acquire(context.Background(), spec)
	if err != nil {
		return err
	}
	// 用状态机切分而不是按 ';' 硬切：注释与字符串里的分号都可能导致语句错位，
	// 更糟的是「以注释开头的整条语句被静默跳过」——那会让人以为灌数成功了。
	for _, stmt := range splitSQLScript(string(raw)) {
		if _, err := c.Exec(context.Background(), stmt); err != nil {
			return fmt.Errorf("执行失败: %w\n%s", err, stmt)
		}
	}
	fmt.Println("样例数据已灌入", *dsName)
	return nil
}

// cmdBackfill 按日期区间逐日补数。幂等键保证重复执行不会产生重复实例。
func cmdBackfill(args []string) error {
	fs := flag.NewFlagSet("backfill", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	pipeline := fs.String("pipeline", "", "管道名")
	from := fs.String("from", "", "起始日期 YYYY-MM-DD")
	to := fs.String("to", "", "结束日期 YYYY-MM-DD（含）")
	exec := fs.Bool("exec", true, "是否顺带执行")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pipeline == "" || *from == "" || *to == "" {
		return fmt.Errorf("需要 --pipeline --from --to")
	}
	start, err := time.Parse("2006-01-02", *from)
	if err != nil {
		return fmt.Errorf("起始日期格式错误: %w", err)
	}
	end, err := time.Parse("2006-01-02", *to)
	if err != nil {
		return fmt.Errorf("结束日期格式错误: %w", err)
	}
	if end.Before(start) {
		return fmt.Errorf("结束日期不能早于起始日期")
	}

	svc, _, closeFn, err := setup([]string{"--config", *cfgPath})
	if err != nil {
		return err
	}
	defer closeFn()
	ws, err := svc.Bootstrap(context.Background())
	if err != nil {
		return err
	}

	total := 0
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		date := d.Format("2006-01-02")
		inst, created, err := svc.Trigger(context.Background(), ws.ID, *pipeline, date, "backfill", nil)
		if err != nil {
			return fmt.Errorf("%s: %w", date, err)
		}
		if created {
			total++
			fmt.Printf("  %s 已排队 %s\n", date, inst.ID[len(inst.ID)-8:])
		} else {
			fmt.Printf("  %s 已存在，跳过\n", date)
		}
	}
	fmt.Printf("共创建 %d 个补数实例\n", total)

	if !*exec {
		return nil
	}
	for {
		ran, err := svc.RunOnce(context.Background(), "backfill")
		if err != nil {
			fmt.Fprintln(os.Stderr, "执行失败:", err)
		}
		if !ran {
			break
		}
	}
	fmt.Println("补数执行完成")
	return nil
}

// cmdLineage 查询血缘：默认表级，--column 时看列级，--impact 时看影响面。
func cmdLineage(args []string) error {
	fs := flag.NewFlagSet("lineage", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	table := fs.String("table", "", "表或模型名")
	column := fs.String("column", "", "列名（可选）")
	impact := fs.Bool("impact", false, "查看该列的下游影响")
	depth := fs.Int("depth", 5, "影响分析深度")
	format := fs.String("o", "text", "输出格式：text/json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *table == "" {
		return fmt.Errorf("需要 --table")
	}
	svc, _, closeFn, err := setup([]string{"--config", *cfgPath})
	if err != nil {
		return err
	}
	defer closeFn()
	ws, err := svc.Bootstrap(context.Background())
	if err != nil {
		return err
	}

	if *impact {
		res, err := svc.Impact(context.Background(), ws.ID, *table, *column, *depth)
		if err != nil {
			return err
		}
		if *format == "json" {
			raw, _ := json.MarshalIndent(res, "", "  ")
			fmt.Println(string(raw))
			return nil
		}
		edges, _ := res["edges"].([]model.ColumnEdge)
		fmt.Printf("影响 %v 个下游列，涉及 %d 张表\n", res["count"], len(res["tables"].([]string)))
		for _, e := range edges {
			fmt.Printf("  %s.%s -> %s.%s\n", e.UpstreamTable, e.UpstreamColumn, e.DownstreamTable, e.DownstreamColumn)
		}
		return nil
	}

	edges, err := svc.Repo.ColumnLineage(context.Background(), ws.ID, *table, *column)
	if err != nil {
		return err
	}
	if *format == "json" {
		raw, _ := json.MarshalIndent(edges, "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	if len(edges) == 0 {
		fmt.Println("没有列级血缘（可能用了 SELECT *，或尚未发布管道）")
		return nil
	}
	for _, e := range edges {
		fmt.Printf("  %s.%s -> %s.%s\n", e.UpstreamTable, e.UpstreamColumn, e.DownstreamTable, e.DownstreamColumn)
	}
	return nil
}

// cmdProfile 对表做画像，可选直接把推荐规则写进模型 YAML 供人工确认。
// cmdCollect 采集数据源元数据，并输出与上次的差异。
// demo 与日常巡检都用它：没有采集，表结构页就是空的。
func cmdCollect(args []string) error {
	fs := flag.NewFlagSet("collect", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	ds := fs.String("datasource", "pg_dw", "数据源名称")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, _, closeFn, err := setup([]string{"--config", *cfgPath})
	if err != nil {
		return err
	}
	defer closeFn()
	ws, err := svc.Bootstrap(context.Background())
	if err != nil {
		return err
	}
	res, err := svc.CollectWithDiff(context.Background(), ws.ID, *ds)
	if err != nil {
		return err
	}
	fmt.Printf("采集完成：%s，%d 张表，%d 项结构变更，耗时 %dms\n",
		res.Datasource, res.Tables, len(res.Changes), res.DurationMS)
	// 变更超过 10 条时只列前 10，避免刷屏；完整清单在「表结构 → 结构变更」里。
	for i, c := range res.Changes {
		if i >= 10 {
			fmt.Printf("  …… 另有 %d 项\n", len(res.Changes)-10)
			break
		}
		fmt.Printf("  %-12s %s.%s %s\n", c.Kind, c.Schema, c.Table, c.Column)
	}
	return nil
}

func cmdProfile(args []string) error {
	fs := flag.NewFlagSet("profile", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	ds := fs.String("datasource", "pg_dw", "数据源名称")
	schema := fs.String("schema", "", "schema")
	table := fs.String("table", "", "表名")
	format := fs.String("o", "text", "输出格式：text/json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *table == "" {
		return fmt.Errorf("需要 --table")
	}
	svc, _, closeFn, err := setup([]string{"--config", *cfgPath})
	if err != nil {
		return err
	}
	defer closeFn()
	ws, err := svc.Bootstrap(context.Background())
	if err != nil {
		return err
	}
	res, err := svc.Profile(context.Background(), ws.ID, *ds, *schema, *table, 0)
	if err != nil {
		return err
	}
	if *format == "json" {
		raw, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(raw))
		return nil
	}
	fmt.Printf("%s.%s 共 %v 行\n", *schema, *table, res["rows"])
	cols, _ := res["columns"].([]map[string]any)
	for _, c := range cols {
		fmt.Printf("  %-18s %-10s 空值率 %.2f%%  去重 %v\n",
			c["column"], c["type"], toFloat(c["nullRate"])*100, c["distinct"])
	}
	rules, _ := res["recommended"].([]model.QualityRule)
	if len(rules) > 0 {
		fmt.Println("推荐规则：")
		for _, r := range rules {
			fmt.Printf("  - name: %s\n    kind: %s\n    column: %s\n    severity: %s\n", r.Name, r.Kind, r.Column, r.Severity)
		}
	}
	return nil
}

func toFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// cmdExport 把指标/看板导出为 CSV 并等待完成。
func cmdExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	kind := fs.String("kind", "metric", "metric 或 dashboard")
	target := fs.String("target", "", "指标/看板名")
	from := fs.String("from", "", "起始日期")
	to := fs.String("to", "", "结束日期")
	out := fs.String("o", "", "输出文件路径（默认写到导出目录）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *target == "" {
		return fmt.Errorf("需要 --target")
	}
	svc, _, closeFn, err := setup([]string{"--config", *cfgPath})
	if err != nil {
		return err
	}
	defer closeFn()
	ws, err := svc.Bootstrap(context.Background())
	if err != nil {
		return err
	}
	job, err := svc.CreateExport(context.Background(), ws.ID, *kind, *target,
		map[string]any{"from": *from, "to": *to, "granularity": "day"}, "cli")
	if err != nil {
		return err
	}
	fmt.Println("导出任务", job.ID, "已创建，等待完成…")
	for i := 0; i < 60; i++ {
		time.Sleep(500 * time.Millisecond)
		j, err := svc.Repo.GetExport(context.Background(), ws.ID, job.ID)
		if err != nil {
			return err
		}
		switch j.Status {
		case "success":
			fmt.Printf("完成：%d 行 -> %s\n", j.RowCount, j.FilePath)
			if *out != "" {
				raw, err := os.ReadFile(j.FilePath)
				if err != nil {
					return err
				}
				if err := os.WriteFile(*out, raw, 0o644); err != nil {
					return err
				}
				fmt.Println("已另存到", *out)
			}
			return nil
		case "failed":
			return fmt.Errorf("导出失败: %s", j.Error)
		}
	}
	return fmt.Errorf("导出超时")
}

// cmdLint 只校验不写库：适合放进 CI，防止非法 YAML 合入。
func cmdLint(args []string) error {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	dir := fs.String("dir", "examples", "要扫描的目录")
	fs.Bool("strict", false, "把警告也当作失败")
	if err := fs.Parse(args); err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(*dir, "*.yaml"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("目录下没有 YAML: %s", *dir)
	}
	bad := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		kind, err := detectKind(raw)
		if err != nil {
			fmt.Printf("✗ %-48s %v\n", f, err)
			bad++
			continue
		}
		var verr error
		switch kind {
		case "Model":
			var spec model.ModelSpec
			if err := yaml.Unmarshal(raw, &spec); err != nil {
				verr = err
			} else {
				pg, _ := dialect.Get("postgres")
				_, verr = modelc.Compile(spec, pg)
			}
		case "Pipeline":
			var spec model.PipelineSpec
			if err := yaml.Unmarshal(raw, &spec); err != nil {
				verr = err
			} else {
				verr = pipelinecompile.Validate(&spec)
			}
		case "Metric":
			var m semantic.Metric
			if err := yaml.Unmarshal(raw, &m); err != nil {
				verr = err
			} else {
				verr = semantic.Validate(m)
			}
		case "Dashboard":
			var d semantic.Dashboard
			if err := yaml.Unmarshal(raw, &d); err != nil {
				verr = err
			} else {
				verr = semantic.ValidateDashboard(d)
			}
		}
		if verr != nil {
			fmt.Printf("✗ %-48s %v\n", f, verr)
			bad++
		} else {
			fmt.Printf("✓ %-48s %s\n", f, kind)
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d 个文件校验失败", bad)
	}
	fmt.Printf("全部通过（%d 个文件）\n", len(files))
	return nil
}

// detectKind 读取 YAML 顶层 kind。apply 与 lint 共用同一套判定。
func detectKind(raw []byte) (string, error) {
	var head struct {
		Kind string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(raw, &head); err != nil {
		return "", err
	}
	if head.Kind == "" {
		return "", fmt.Errorf("缺少 kind 字段")
	}
	return head.Kind, nil
}

// cmdInit 生成模型脚手架。子命令只有一个 model，但保留层级便于后续扩展。
func cmdInit(args []string) error {
	if len(args) == 0 || args[0] != "model" {
		return fmt.Errorf("用法: godw-cli init model --layer dwd --from <源表> --name <模型名>")
	}
	fs := flag.NewFlagSet("init model", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	layer := fs.String("layer", "dwd", "分层：ods/dwd/dws/ads/dim")
	from := fs.String("from", "", "源表名（用于读取列定义）")
	name := fs.String("name", "", "模型名，默认取源表名")
	ds := fs.String("datasource", "pg_dw", "目标数据源")
	schema := fs.String("schema", "", "目标 schema，默认与层同名")
	out := fs.String("o", "", "输出文件，留空打印到 stdout")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *from == "" {
		return fmt.Errorf("必须指定 --from")
	}
	svc, _, cleanup, err := setup([]string{"--config", *cfgPath})
	if err != nil {
		return err
	}
	defer cleanup()

	ws, err := svc.Repo.EnsureWorkspace(context.Background(), "default")
	if err != nil {
		return err
	}
	res, err := svc.ScaffoldModel(context.Background(), ws.ID, modelc.ScaffoldOptions{
		Layer: *layer, Name: *name, SourceName: *from,
		Datasource: *ds, Schema: *schema,
	})
	if err != nil {
		return err
	}
	for _, w := range res.Warnings {
		fmt.Fprintln(os.Stderr, "提示:", w)
	}
	if *out != "" {
		if err := os.WriteFile(*out, []byte(res.YAML), 0o644); err != nil {
			return err
		}
		fmt.Println("已写入", *out)
		return nil
	}
	fmt.Print(res.YAML)
	return nil
}

// cmdDiff 输出当前发布版本与本地文件的差异，breaking 时以非零码退出，
// 便于 CI 在 PR 阶段就拦住破坏性变更。
func cmdDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	file := fs.String("f", "", "模型 YAML 文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return fmt.Errorf("缺少 -f 参数")
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	var spec model.ModelSpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		return err
	}
	svc, _, cleanup, err := setup([]string{"--config", *cfgPath})
	if err != nil {
		return err
	}
	defer cleanup()
	ws, err := svc.Repo.EnsureWorkspace(context.Background(), "default")
	if err != nil {
		return err
	}
	res, err := svc.ModelDiff(context.Background(), ws.ID, spec)
	if err != nil {
		return err
	}
	diff, _ := res["diff"].(modelc.DiffResult)
	if len(diff.Changes) == 0 {
		fmt.Println("无结构变更")
		return nil
	}
	for _, c := range diff.Changes {
		flagText := "safe"
		if c.Breaking {
			flagText = "BREAKING"
		}
		fmt.Printf("[%s] %s %s %s → %s  %s\n", flagText, c.Kind, c.Column, c.From, c.To, c.Reason)
	}
	fmt.Printf("\n共 %d 项变更，其中破坏性 %d 项\n", len(diff.Changes), diff.Breaking)
	if plan, okc := res["plan"].(modelc.MigrationPlan); okc {
		if len(plan.Statements) > 0 {
			fmt.Println("\n-- 迁移语句 --")
			for _, s := range plan.Statements {
				fmt.Println(s)
			}
		}
		for _, m := range plan.Manual {
			fmt.Println("需人工处理:", m)
		}
	}
	if diff.HasBreaking() {
		return fmt.Errorf("存在破坏性变更，发布需显式 --force")
	}
	return nil
}

// cmdExportDoc 导出数据字典。
func cmdExportDoc(args []string) error {
	fs := flag.NewFlagSet("export-doc", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "配置文件路径")
	out := fs.String("o", "", "输出文件，留空打印到 stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, _, cleanup, err := setup([]string{"--config", *cfgPath})
	if err != nil {
		return err
	}
	defer cleanup()
	ws, err := svc.Repo.EnsureWorkspace(context.Background(), "default")
	if err != nil {
		return err
	}
	md, err := svc.DictionaryMarkdown(context.Background(), ws.ID)
	if err != nil {
		return err
	}
	if *out != "" {
		if err := os.WriteFile(*out, []byte(md), 0o644); err != nil {
			return err
		}
		fmt.Println("已写入", *out)
		return nil
	}
	fmt.Print(md)
	return nil
}

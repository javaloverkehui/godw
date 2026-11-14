package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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
  run            触发管道（--pipeline 单条，或 --select 按模型选择）
  init           从源表生成模型脚手架（init model）
  diff           对比模型当前发布版本与本地文件
  export-doc     导出数据字典（--html 导出静态文档站）
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
  godw-cli run --select "dwd_order+" --date 2025-01-01
  godw-cli run --select "tag:core" --dry-run
  godw-cli init model --layer dwd --from order --name order_detail --datasource pg_dw
  godw-cli diff -f examples/02_model_dwd_order.yaml
  godw-cli export-doc -o docs/dictionary.md
  godw-cli export-doc --html -o docs/catalog.html
`

// Stdout / Stderr 供测试替换，业务代码只经由它们输出，保证 Run 可被单测驱动。
var (
	Stdout io.Writer = os.Stdout
	Stderr io.Writer = os.Stderr
)

// Run 只负责参数分发、打印 usage / 错误并返回退出码；
// 退出码语义与原 main 保持一致：0 成功、1 业务错误、2 用法错误。
func Run(args []string, version string) int {
	if len(args) == 0 {
		fmt.Fprint(Stdout, usage)
		return 2
	}
	cmd := args[0]
	rest := args[1:]

	var err error
	switch cmd {
	case "migrate":
		err = cmdMigrate(rest)
	case "doctor":
		err = cmdDoctor(rest)
	case "apply":
		err = cmdApply(rest)
	case "run":
		err = cmdRun(rest)
	case "worker":
		err = cmdWorker(rest)
	case "seed":
		err = cmdSeed(rest)
	case "backfill":
		err = cmdBackfill(rest)
	case "init":
		err = cmdInit(rest)
	case "diff":
		err = cmdDiff(rest)
	case "export-doc":
		err = cmdExportDoc(rest)
	case "lineage":
		err = cmdLineage(rest)
	case "profile":
		err = cmdProfile(rest)
	case "collect":
		err = cmdCollect(rest)
	case "export":
		err = cmdExport(rest)
	case "lint":
		err = cmdLint(rest)
	case "version", "-v", "--version":
		fmt.Fprintln(Stdout, "godw-cli", version)
	case "help", "-h", "--help":
		fmt.Fprint(Stdout, usage)
	default:
		fmt.Fprintf(Stderr, "未知命令: %s\n\n%s", cmd, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(Stderr, "错误:", err)
		return 1
	}
	return 0
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
			fmt.Fprintln(Stdout, "schema 已是最新")
			return nil
		}
		fmt.Fprintf(Stdout, "待执行迁移 %d 个:\n", len(pending))
		for _, p := range pending {
			fmt.Fprintln(Stdout, "  -", p)
		}
		return fmt.Errorf("存在待执行的迁移")
	}
	applied, err := db.Migrate(context.Background())
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		fmt.Fprintln(Stdout, "schema 已是最新，无需迁移")
		return nil
	}
	fmt.Fprintf(Stdout, "已应用 %d 个迁移: %s\n", len(applied), strings.Join(applied, ", "))
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
	fmt.Fprintln(Stdout, "✓ 配置校验通过")
	fmt.Fprintf(Stdout, "  env=%s db=%s\n", res.Config.Env, res.Config.DB.Redacted())

	db, err := repo.Open(context.Background(), res.Config.DB)
	if err != nil {
		return err
	}
	defer db.Close()
	fmt.Fprintln(Stdout, "✓ 元数据库可连接")

	v, n, err := db.Version(context.Background())
	if err != nil {
		return err
	}
	if n == 0 {
		fmt.Fprintln(Stdout, "! schema 未初始化，请执行 godw-cli migrate")
	} else {
		fmt.Fprintf(Stdout, "✓ schema 版本 %s（%d 个迁移）\n", v, n)
	}

	fmt.Fprintf(Stdout, "✓ 已注册连接器: %s\n", strings.Join(connector.Types(), ", "))

	svc, err := service.New(db, res.Config, log.New(res.Config.Log))
	if err != nil {
		return err
	}
	list, err := svc.Repo.ListDatasources(context.Background(), "default")
	if err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Fprintln(Stdout, "! 尚未接入任何数据源")
		return nil
	}
	fmt.Fprintf(Stdout, "✓ 数据源 %d 个\n", len(list))
	fail := 0
	for _, ds := range list {
		if err := svc.TestDatasource(context.Background(), ds.WorkspaceID, ds.Name); err != nil {
			fmt.Fprintf(Stdout, "  ✗ %s (%s): %v\n", ds.Name, ds.Type, err)
			fail++
			continue
		}
		fmt.Fprintf(Stdout, "  ✓ %s (%s)\n", ds.Name, ds.Type)
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
		fmt.Fprintf(Stdout, "模型 %s 已保存，当前版本 v%d\n", m.Name, m.ActiveVersion)
		if m.DDL != "" && !*applyDDL {
			fmt.Fprintln(Stdout, "\n生成的 DDL（未执行）:")
			fmt.Fprintln(Stdout, m.DDL)
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
		fmt.Fprintf(Stdout, "指标 %s 已保存（%s）\n", saved.Name, saved.DisplayName)
	case "Dashboard":
		var d semantic.Dashboard
		if err := yaml.Unmarshal(raw, &d); err != nil {
			return err
		}
		saved, err := svc.ApplyDashboard(context.Background(), ws.ID, d)
		if err != nil {
			return err
		}
		fmt.Fprintf(Stdout, "看板 %s 已保存（%d 个卡片）\n", saved.Name, len(saved.Cards))
	case "Pipeline":
		p, err := svc.ApplyPipeline(context.Background(), ws.ID, "cli", raw, *publish)
		if err != nil {
			return err
		}
		fmt.Fprintf(Stdout, "管道 %s 已保存，当前版本 v%d\n", p.Name, p.ActiveVersion)
	case "Dataset":
		var d semantic.Dataset
		if err := yaml.Unmarshal(raw, &d); err != nil {
			return err
		}
		saved, err := svc.ApplyDataset(context.Background(), ws.ID, d)
		if err != nil {
			return err
		}
		fmt.Fprintf(Stdout, "数据集 %s 已保存（%d 维度 / %d 度量）\n", saved.Name, len(saved.Dimensions), len(saved.Measures))
	case "Chart":
		var c semantic.Chart
		if err := yaml.Unmarshal(raw, &c); err != nil {
			return err
		}
		saved, warnings, err := svc.ApplyChart(context.Background(), ws.ID, c)
		if err != nil {
			return err
		}
		fmt.Fprintf(Stdout, "图表 %s 已保存（%s）\n", saved.Name, saved.Type)
		// 影响面提醒：改图表不用重跑数据，但会改变业务看到的数字口径。
		for _, w := range warnings {
			fmt.Fprintln(Stdout, "  注意:", w)
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
	sel := fs.String("select", "", "选择器：dwd_order+ / +dwd_order / tag:core / layer:dwd / pipeline:name")
	bizDate := fs.String("date", "", "业务日期 YYYY-MM-DD，默认今天")
	wait := fs.Bool("wait", false, "等待执行完成")
	dryRun := fs.Bool("dry-run", false, "只解析选择器并打印命中范围，不触发")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *pipeline == "" && *sel == "" {
		return fmt.Errorf("需要 --pipeline 或 --select")
	}
	if *pipeline != "" && *sel != "" {
		return fmt.Errorf("--pipeline 与 --select 只能选一个")
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
	_ = cfg

	if *sel != "" {
		return runBySelect(svc, ws.ID, *sel, *bizDate, *wait, *dryRun)
	}

	inst, created, err := svc.Trigger(context.Background(), ws.ID, *pipeline, *bizDate, "manual", nil)
	if err != nil {
		return err
	}
	if created {
		fmt.Fprintf(Stdout, "已创建实例 %s（业务日期 %s）\n", inst.ID, inst.BizDate)
	} else {
		fmt.Fprintf(Stdout, "复用已有实例 %s\n", inst.ID)
	}
	if !*wait {
		return nil
	}
	return runAndReport(svc, []string{inst.ID})
}

// runBySelect 按选择器触发：先打印命中范围，再逐条触发。
// 「命中 3 个模型却排了 5 条管道」这种放大必须让人看见，否则会以为只跑了选中的模型。
func runBySelect(svc *service.Service, wsID, expr, bizDate string, wait, dryRun bool) error {
	ctx := context.Background()
	res, err := svc.ResolveSelect(ctx, wsID, expr)
	if err != nil {
		return err
	}
	fmt.Fprintf(Stdout, "选择器 %s → %d 个模型、%d 条管道\n", expr, len(res.Models), len(res.Pipelines))
	for _, m := range res.Models {
		fmt.Fprintf(Stdout, "  模型  %s\n", m)
	}
	for _, p := range res.Pipelines {
		fmt.Fprintf(Stdout, "  管道  %s\n", p)
	}
	if len(res.Uncovered) > 0 {
		fmt.Fprintf(Stdout, "  未覆盖（没有管道写这些模型）: %s\n", strings.Join(res.Uncovered, ", "))
	}
	if dryRun {
		return nil
	}
	if len(res.Pipelines) == 0 {
		return fmt.Errorf("选择器没有命中任何已发布管道")
	}

	runs, _, err := svc.TriggerSelect(ctx, wsID, expr, bizDate, nil)
	if err != nil {
		return err
	}
	var ids []string
	failed := 0
	for _, x := range runs {
		switch {
		case x.Error != "":
			failed++
			fmt.Fprintf(Stdout, "  ✗ %-24s %s\n", x.Pipeline, x.Error)
		case x.Created:
			fmt.Fprintf(Stdout, "  ✓ %-24s 已排队 %s\n", x.Pipeline, x.InstanceID)
			ids = append(ids, x.InstanceID)
		default:
			fmt.Fprintf(Stdout, "  = %-24s 复用 %s\n", x.Pipeline, x.InstanceID)
		}
	}
	if !wait || len(ids) == 0 {
		if failed > 0 {
			return fmt.Errorf("%d 条管道触发失败", failed)
		}
		return nil
	}
	return runAndReport(svc, ids)
}

// runAndReport 依次执行队列，并打印指定实例的最终状态。
func runAndReport(svc *service.Service, ids []string) error {
	ctx := context.Background()
	for {
		ran, err := svc.RunOnce(ctx, "cli")
		if err != nil {
			return err
		}
		if !ran {
			break
		}
	}
	bad := 0
	for _, id := range ids {
		final, err := svc.Repo.GetInstance(ctx, id)
		if err != nil {
			return err
		}
		fmt.Fprintf(Stdout, "%s 执行结束: %s\n", id, final.Status)
		if final.Status != "success" {
			bad++
		}
		for _, t := range final.Tasks {
			fmt.Fprintf(Stdout, "  %-24s %-10s %s\n", t.NodeID, t.Status, t.Error)
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d 个实例未成功", bad)
	}
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
			fmt.Fprintln(Stderr, "执行失败:", err)
		}
		if *once || !ran {
			if !ran {
				fmt.Fprintln(Stdout, "没有待执行实例")
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
		fmt.Fprintln(Stdout, "已接入数据源", *dsName)
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
	for _, stmt := range SplitSQLScript(string(raw)) {
		if _, err := c.Exec(context.Background(), stmt); err != nil {
			return fmt.Errorf("执行失败: %w\n%s", err, stmt)
		}
	}
	fmt.Fprintln(Stdout, "样例数据已灌入", *dsName)
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
			fmt.Fprintf(Stdout, "  %s 已排队 %s\n", date, inst.ID[len(inst.ID)-8:])
		} else {
			fmt.Fprintf(Stdout, "  %s 已存在，跳过\n", date)
		}
	}
	fmt.Fprintf(Stdout, "共创建 %d 个补数实例\n", total)

	if !*exec {
		return nil
	}
	for {
		ran, err := svc.RunOnce(context.Background(), "backfill")
		if err != nil {
			fmt.Fprintln(Stderr, "执行失败:", err)
		}
		if !ran {
			break
		}
	}
	fmt.Fprintln(Stdout, "补数执行完成")
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
			fmt.Fprintln(Stdout, string(raw))
			return nil
		}
		edges, _ := res["edges"].([]model.ColumnEdge)
		fmt.Fprintf(Stdout, "影响 %v 个下游列，涉及 %d 张表\n", res["count"], len(res["tables"].([]string)))
		for _, e := range edges {
			fmt.Fprintf(Stdout, "  %s.%s -> %s.%s\n", e.UpstreamTable, e.UpstreamColumn, e.DownstreamTable, e.DownstreamColumn)
		}
		return nil
	}

	edges, err := svc.Repo.ColumnLineage(context.Background(), ws.ID, *table, *column)
	if err != nil {
		return err
	}
	if *format == "json" {
		raw, _ := json.MarshalIndent(edges, "", "  ")
		fmt.Fprintln(Stdout, string(raw))
		return nil
	}
	if len(edges) == 0 {
		fmt.Fprintln(Stdout, "没有列级血缘（可能用了 SELECT *，或尚未发布管道）")
		return nil
	}
	for _, e := range edges {
		fmt.Fprintf(Stdout, "  %s.%s -> %s.%s\n", e.UpstreamTable, e.UpstreamColumn, e.DownstreamTable, e.DownstreamColumn)
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
	fmt.Fprintf(Stdout, "采集完成：%s，%d 张表，%d 项结构变更，耗时 %dms\n",
		res.Datasource, res.Tables, len(res.Changes), res.DurationMS)
	// 变更超过 10 条时只列前 10，避免刷屏；完整清单在「表结构 → 结构变更」里。
	for i, c := range res.Changes {
		if i >= 10 {
			fmt.Fprintf(Stdout, "  …… 另有 %d 项\n", len(res.Changes)-10)
			break
		}
		fmt.Fprintf(Stdout, "  %-12s %s.%s %s\n", c.Kind, c.Schema, c.Table, c.Column)
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
		fmt.Fprintln(Stdout, string(raw))
		return nil
	}
	fmt.Fprintf(Stdout, "%s.%s 共 %v 行\n", *schema, *table, res["rows"])
	cols, _ := res["columns"].([]map[string]any)
	for _, c := range cols {
		fmt.Fprintf(Stdout, "  %-18s %-10s 空值率 %.2f%%  去重 %v\n",
			c["column"], c["type"], ToFloat(c["nullRate"])*100, c["distinct"])
	}
	rules, _ := res["recommended"].([]model.QualityRule)
	if len(rules) > 0 {
		fmt.Fprintln(Stdout, "推荐规则：")
		for _, r := range rules {
			fmt.Fprintf(Stdout, "  - name: %s\n    kind: %s\n    column: %s\n    severity: %s\n", r.Name, r.Kind, r.Column, r.Severity)
		}
	}
	return nil
}

func ToFloat(v any) float64 {
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
	fmt.Fprintln(Stdout, "导出任务", job.ID, "已创建，等待完成…")
	for i := 0; i < 60; i++ {
		time.Sleep(500 * time.Millisecond)
		j, err := svc.Repo.GetExport(context.Background(), ws.ID, job.ID)
		if err != nil {
			return err
		}
		switch j.Status {
		case "success":
			fmt.Fprintf(Stdout, "完成：%d 行 -> %s\n", j.RowCount, j.FilePath)
			if *out != "" {
				raw, err := os.ReadFile(j.FilePath)
				if err != nil {
					return err
				}
				if err := os.WriteFile(*out, raw, 0o644); err != nil {
					return err
				}
				fmt.Fprintln(Stdout, "已另存到", *out)
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
		kind, err := DetectKind(raw)
		if err != nil {
			fmt.Fprintf(Stdout, "✗ %-48s %v\n", f, err)
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
			fmt.Fprintf(Stdout, "✗ %-48s %v\n", f, verr)
			bad++
		} else {
			fmt.Fprintf(Stdout, "✓ %-48s %s\n", f, kind)
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d 个文件校验失败", bad)
	}
	fmt.Fprintf(Stdout, "全部通过（%d 个文件）\n", len(files))
	return nil
}

// DetectKind 读取 YAML 顶层 kind。apply 与 lint 共用同一套判定。
func DetectKind(raw []byte) (string, error) {
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
	out := fs.String("o", "", "输出文件，留空打印到 Stdout")
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
		fmt.Fprintln(Stderr, "提示:", w)
	}
	if *out != "" {
		if err := os.WriteFile(*out, []byte(res.YAML), 0o644); err != nil {
			return err
		}
		fmt.Fprintln(Stdout, "已写入", *out)
		return nil
	}
	fmt.Fprint(Stdout, res.YAML)
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
		fmt.Fprintln(Stdout, "无结构变更")
		return nil
	}
	for _, c := range diff.Changes {
		flagText := "safe"
		if c.Breaking {
			flagText = "BREAKING"
		}
		fmt.Fprintf(Stdout, "[%s] %s %s %s → %s  %s\n", flagText, c.Kind, c.Column, c.From, c.To, c.Reason)
	}
	fmt.Fprintf(Stdout, "\n共 %d 项变更，其中破坏性 %d 项\n", len(diff.Changes), diff.Breaking)
	if plan, okc := res["plan"].(modelc.MigrationPlan); okc {
		if len(plan.Statements) > 0 {
			fmt.Fprintln(Stdout, "\n-- 迁移语句 --")
			for _, s := range plan.Statements {
				fmt.Fprintln(Stdout, s)
			}
		}
		for _, m := range plan.Manual {
			fmt.Fprintln(Stdout, "需人工处理:", m)
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
	out := fs.String("o", "", "输出文件，留空打印到 Stdout")
	asHTML := fs.Bool("html", false, "导出单文件文档站（HTML），默认导出 Markdown 字典")
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
	var body string
	if *asHTML {
		body, err = svc.CatalogHTML(context.Background(), ws.ID)
	} else {
		body, err = svc.DictionaryMarkdown(context.Background(), ws.ID)
	}
	if err != nil {
		return err
	}
	// 写文件时连带报字节数：空文档和成功长得一样，不给大小很容易被当成没问题。
	if *out != "" {
		if err := os.WriteFile(*out, []byte(body), 0o644); err != nil {
			return err
		}
		fmt.Fprintln(Stdout, "已写入", *out, fmt.Sprintf("(%d 字节)", len(body)))
		return nil
	}
	fmt.Fprint(Stdout, body)
	return nil
}

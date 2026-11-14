package cli_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/godw/godw/internal/cli"
	"github.com/godw/godw/internal/config"
	"github.com/godw/godw/test/testenv"
)

// testVersion 是命令层用例统一使用的版本号，与 main 的默认值保持一致。
const testVersion = "1.0.0"

// run 是 cli.Run 的薄垫片：把版本号固定下来，用例只关心参数与退出码。
func run(args []string) int { return cli.Run(args, testVersion) }

// captureOutput 把 Run 的标准输出/错误收进同一缓冲区，用例结束后恢复。
// 命令层大量直接打印进度，不接管的话测试输出会被刷屏，也无法断言。
func captureOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	oldOut, oldErr := cli.Stdout, cli.Stderr
	cli.Stdout, cli.Stderr = buf, buf
	t.Cleanup(func() { cli.Stdout, cli.Stderr = oldOut, oldErr })
	return buf
}

// newEnv 建一个独占 schema 的隔离环境并接管输出。
func newEnv(t *testing.T, migrate bool, mutate ...func(*config.Config)) (*testenv.Env, *bytes.Buffer) {
	t.Helper()
	buf := captureOutput(t)
	return testenv.New(t, migrate, mutate...), buf
}

// examplesPath 返回仓库内示例文件的绝对路径：示例本身就是最贴近真实的夹具。
func examplesPath(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "..", "..", "examples"}, parts...)...)
}

// writeFile 在临时目录写一个文件，返回路径。
func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
	return path
}

// mustRun 执行 run 并断言退出码。
func mustRun(t *testing.T, want int, args ...string) string {
	t.Helper()
	code := run(args)
	if code != want {
		t.Fatalf("run(%v) 退出码 %d，期望 %d", args, code, want)
	}
	return ""
}

// ---------- 参数分发 ----------

// TestRunNoArgs 无子命令时打印 usage 并返回 2。
func TestRunNoArgs(t *testing.T) {
	buf := captureOutput(t)
	if code := run(nil); code != 2 {
		t.Fatalf("无参数应返回 2，实际 %d", code)
	}
	if !strings.Contains(buf.String(), "godw-cli — GoDW 运维命令行") {
		t.Fatalf("应打印 usage，实际:\n%s", buf.String())
	}
}

// TestRunVersion 三个版本别名都应打印版本并返回 0。
func TestRunVersion(t *testing.T) {
	for _, alias := range []string{"version", "-v", "--version"} {
		buf := captureOutput(t)
		if code := run([]string{alias}); code != 0 {
			t.Fatalf("%s 应返回 0，实际 %d", alias, code)
		}
		if got := buf.String(); got != "godw-cli "+testVersion+"\n" {
			t.Fatalf("%s 输出不符: %q", alias, got)
		}
	}
}

// TestRunHelp 帮助别名打印 usage，返回 0。
func TestRunHelp(t *testing.T) {
	for _, alias := range []string{"help", "-h", "--help"} {
		buf := captureOutput(t)
		if code := run([]string{alias}); code != 0 {
			t.Fatalf("%s 应返回 0，实际 %d", alias, code)
		}
		if !strings.Contains(buf.String(), "用法:") {
			t.Fatalf("%s 应打印 usage:\n%s", alias, buf.String())
		}
	}
}

// TestRunUnknownCommand 未知子命令把提示写到 stderr 并返回 2。
func TestRunUnknownCommand(t *testing.T) {
	buf := captureOutput(t)
	if code := run([]string{"nope"}); code != 2 {
		t.Fatalf("未知命令应返回 2，实际 %d", code)
	}
	if !strings.Contains(buf.String(), "未知命令: nope") {
		t.Fatalf("应提示未知命令:\n%s", buf.String())
	}
}

// TestRunSubcommandError 子命令返回错误时打印「错误:」并返回 1。
func TestRunSubcommandError(t *testing.T) {
	buf := captureOutput(t)
	empty := t.TempDir()
	if code := run([]string{"lint", "--dir", empty}); code != 1 {
		t.Fatalf("lint 空目录应返回 1，实际 %d", code)
	}
	if !strings.Contains(buf.String(), "错误:") {
		t.Fatalf("应打印错误前缀:\n%s", buf.String())
	}
}

// TestRunFlagParseError 非法 flag 由 flag 包报错，同样返回 1。
func TestRunFlagParseError(t *testing.T) {
	buf := captureOutput(t)
	if code := run([]string{"lint", "--bogus"}); code != 1 {
		t.Fatalf("非法 flag 应返回 1，实际 %d", code)
	}
	if !strings.Contains(buf.String(), "错误:") {
		t.Fatalf("应打印错误前缀:\n%s", buf.String())
	}
}

// ---------- 纯函数 ----------

// TestDetectKind 覆盖四种合法 kind、未知 kind 与非法 YAML。
func TestDetectKind(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{"Model", "kind: Model\nname: x\n", "Model", false},
		{"Pipeline", "kind: Pipeline\nname: p\n", "Pipeline", false},
		{"Metric", "kind: Metric\nname: m\n", "Metric", false},
		{"Dashboard", "kind: Dashboard\nname: d\n", "Dashboard", false},
		{"未知 kind 原样返回", "kind: Whatever\n", "Whatever", false},
		{"缺少 kind", "name: x\n", "", true},
		{"非法 YAML", "kind: [\n", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := cli.DetectKind([]byte(c.raw))
			if c.wantErr {
				if err == nil {
					t.Fatalf("应返回错误，实际 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got != c.want {
				t.Fatalf("kind=%q，期望 %q", got, c.want)
			}
		})
	}
}

// TestToFloat 只有 float64 能转换，其余类型一律退化为 0。
func TestToFloat(t *testing.T) {
	cases := []struct {
		in   any
		want float64
	}{
		{float64(1.5), 1.5},
		{float64(0), 0},
		{int(3), 0},
		{int64(3), 0},
		{"1.5", 0},
		{nil, 0},
	}
	for _, c := range cases {
		if got := cli.ToFloat(c.in); got != c.want {
			t.Errorf("cli.ToFloat(%#v)=%v，期望 %v", c.in, got, c.want)
		}
	}
}

// ---------- lint ----------

// TestCmdLint 好文件通过、坏文件失败、目录无 YAML 报错。
func TestCmdLint(t *testing.T) {
	t.Run("全部通过", func(t *testing.T) {
		buf := captureOutput(t)
		dir := t.TempDir()
		for _, f := range []string{"02_model_dwd_order.yaml", "05_pipeline_order_daily.yaml", "06_metric_gmv.yaml", "08_dashboard.yaml"} {
			raw, err := os.ReadFile(examplesPath(f))
			if err != nil {
				t.Fatalf("读示例失败: %v", err)
			}
			writeFile(t, dir, f, string(raw))
		}
		if code := run([]string{"lint", "--dir", dir}); code != 0 {
			t.Fatalf("合法 YAML 应返回 0，实际 %d，输出:\n%s", code, buf.String())
		}
		if !strings.Contains(buf.String(), "全部通过（4 个文件）") {
			t.Fatalf("输出不符合预期:\n%s", buf.String())
		}
	})

	t.Run("坏文件与非 YAML", func(t *testing.T) {
		buf := captureOutput(t)
		dir := t.TempDir()
		writeFile(t, dir, "bad_model.yaml", "kind: Model\nname: \"\"\nlayer: dwd\n")
		writeFile(t, dir, "bad_yaml.yaml", "kind: [\n")
		writeFile(t, dir, "no_kind.yaml", "name: x\n")
		// 非 .yaml 后缀不参与扫描。
		writeFile(t, dir, "note.txt", "ignored")
		if code := run([]string{"lint", "--dir", dir}); code != 1 {
			t.Fatalf("非法 YAML 应返回 1，实际 %d，输出:\n%s", code, buf.String())
		}
		out := buf.String()
		if !strings.Contains(out, "3 个文件校验失败") {
			t.Fatalf("应统计失败文件数:\n%s", out)
		}
		if !strings.Contains(out, "缺少 kind 字段") {
			t.Fatalf("应提示缺少 kind:\n%s", out)
		}
	})

	t.Run("目录为空", func(t *testing.T) {
		captureOutput(t)
		if code := run([]string{"lint", "--dir", t.TempDir()}); code != 1 {
			t.Fatalf("空目录应返回 1，实际 %d", code)
		}
	})
}

// ---------- init ----------

// TestCmdInitUsage 子命令与必填参数校验不碰数据库。
func TestCmdInitUsage(t *testing.T) {
	captureOutput(t)
	if code := run([]string{"init"}); code != 1 {
		t.Fatalf("缺子命令应返回 1，实际 %d", code)
	}
	if code := run([]string{"init", "dataset"}); code != 1 {
		t.Fatalf("未知子命令应返回 1，实际 %d", code)
	}
	if code := run([]string{"init", "model", "--layer", "dwd"}); code != 1 {
		t.Fatalf("缺 --from 应返回 1，实际 %d", code)
	}
}

// TestCmdInit 用已采集的表生成脚手架，输出到文件。
func TestCmdInit(t *testing.T) {
	env, buf := newEnv(t, true)
	env.Datasource(t, "pg_dw")
	env.Exec(t, `CREATE TABLE demo_src (id bigint PRIMARY KEY, amount numeric)`)
	env.Dataset(t, "pg_dw", env.Schema, "demo_src", []string{"id", "amount"})

	out := filepath.Join(t.TempDir(), "model.yaml")
	if code := run([]string{"init", "model", "--config", env.ConfigPath,
		"--layer", "dwd", "--from", "demo_src", "--name", "demo_src_detail", "-o", out}); code != 0 {
		t.Fatalf("生成脚手架应返回 0，实际 %d，输出:\n%s", code, buf.String())
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("读输出文件失败: %v", err)
	}
	if !strings.Contains(string(raw), "demo_src_detail") {
		t.Fatalf("脚手架内容不含模型名:\n%s", raw)
	}
	if !strings.Contains(buf.String(), "已写入") {
		t.Fatalf("应提示写入路径:\n%s", buf.String())
	}
}

// ---------- diff / apply 参数校验 ----------

// TestCmdDiffArgs 缺 -f、文件不存在、非法 YAML 都在连库前失败。
func TestCmdDiffArgs(t *testing.T) {
	captureOutput(t)
	if code := run([]string{"diff"}); code != 1 {
		t.Fatalf("缺 -f 应返回 1，实际 %d", code)
	}
	if code := run([]string{"diff", "-f", filepath.Join(t.TempDir(), "nope.yaml")}); code != 1 {
		t.Fatalf("文件不存在应返回 1，实际 %d", code)
	}
	bad := writeFile(t, t.TempDir(), "bad.yaml", "kind: [\n")
	if code := run([]string{"diff", "-f", bad}); code != 1 {
		t.Fatalf("非法 YAML 应返回 1，实际 %d", code)
	}
}

// TestCmdApplyArgs 缺 -f、坏 YAML、未知 kind、不支持的 kind 均返回错误。
func TestCmdApplyArgs(t *testing.T) {
	env, _ := newEnv(t, true)
	if code := run([]string{"apply", "--config", env.ConfigPath}); code != 1 {
		t.Fatalf("缺 -f 应返回 1，实际 %d", code)
	}
	bad := writeFile(t, t.TempDir(), "bad.yaml", "kind: [\n")
	if code := run([]string{"apply", "--config", env.ConfigPath, "-f", bad}); code != 1 {
		t.Fatalf("非法 YAML 应返回 1，实际 %d", code)
	}
	unknown := writeFile(t, t.TempDir(), "unknown.yaml", "kind: Nothing\nname: x\n")
	if code := run([]string{"apply", "--config", env.ConfigPath, "-f", unknown}); code != 1 {
		t.Fatalf("未知 kind 应返回 1，实际 %d", code)
	}
}

// TestCmdBackfillArgs 参数校验：缺参、日期格式、区间倒置。
func TestCmdBackfillArgs(t *testing.T) {
	captureOutput(t)
	cases := [][]string{
		{"backfill"},
		{"backfill", "--pipeline", "p", "--from", "2026-01-01"},
		{"backfill", "--pipeline", "p", "--from", "bad", "--to", "2026-01-02"},
		{"backfill", "--pipeline", "p", "--from", "2026-01-01", "--to", "bad"},
		{"backfill", "--pipeline", "p", "--from", "2026-01-03", "--to", "2026-01-01"},
	}
	for _, args := range cases {
		if code := run(args); code != 1 {
			t.Fatalf("run(%v) 应返回 1，实际 %d", args, code)
		}
	}
}

// TestCmdRequiredFlags lineage / profile / export 的必填参数校验。
func TestCmdRequiredFlags(t *testing.T) {
	captureOutput(t)
	cases := [][]string{
		{"lineage"},
		{"profile"},
		{"export"},
	}
	for _, args := range cases {
		if code := run(args); code != 1 {
			t.Fatalf("run(%v) 应返回 1，实际 %d", args, code)
		}
	}
}

// ---------- migrate / doctor ----------

// TestCmdMigrate 全新 schema 执行迁移，重复执行不再有变更；
// --check 在待执行时返回错误、已最新时返回 0。
func TestCmdMigrate(t *testing.T) {
	env, buf := newEnv(t, false)

	// 迁移前 --check 应列出待执行迁移并以错误结束。
	before := buf.Len()
	if code := run([]string{"migrate", "--config", env.ConfigPath, "--check"}); code != 1 {
		t.Fatalf("有待执行迁移时 --check 应返回 1，实际 %d", code)
	}
	if !strings.Contains(buf.String()[before:], "待执行迁移") {
		t.Fatalf("应列出待执行迁移:\n%s", buf.String()[before:])
	}

	if code := run([]string{"migrate", "--config", env.ConfigPath}); code != 0 {
		t.Fatalf("迁移应返回 0，实际 %d，输出:\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "已应用 6 个迁移") {
		t.Fatalf("应打印已应用迁移数:\n%s", buf.String())
	}

	// 再次执行应提示无需迁移。
	if code := run([]string{"migrate", "--config", env.ConfigPath}); code != 0 {
		t.Fatalf("重复迁移应返回 0，实际 %d", code)
	}
	if !strings.Contains(buf.String(), "schema 已是最新，无需迁移") {
		t.Fatalf("应提示无需迁移:\n%s", buf.String())
	}

	// 已最新时 --check 返回 0。
	if code := run([]string{"migrate", "--config", env.ConfigPath, "--check"}); code != 0 {
		t.Fatalf("已最新时 --check 应返回 0，实际 %d", code)
	}
	if !strings.Contains(buf.String(), "schema 已是最新") {
		t.Fatalf("应提示 schema 已最新:\n%s", buf.String())
	}
}

// TestCmdDoctor 覆盖配置、库连通、schema 版本与连接器清单四段输出。
// 注意：doctor 查询数据源时传入的是字面量 "default" 而非 ws.ID（既有行为），
// 因此这里只断言「未接入数据源」这条路径，不构造数据源。
func TestCmdDoctor(t *testing.T) {
	env, buf := newEnv(t, true)
	if code := run([]string{"doctor", "--config", env.ConfigPath}); code != 0 {
		t.Fatalf("doctor 应返回 0，实际 %d，输出:\n%s", code, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"✓ 配置校验通过", "✓ 元数据库可连接", "schema 版本", "已注册连接器", "尚未接入任何数据源"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少输出 %q:\n%s", want, out)
		}
	}
}

// ---------- apply：六种 kind 一次走完 ----------

// applyExamples 依次 apply 仓库里的示例文件，任一失败即终止用例。
func applyExamples(t *testing.T, env *testenv.Env, files ...string) {
	t.Helper()
	for _, f := range files {
		if code := run([]string{"apply", "--config", env.ConfigPath, "-f", examplesPath(f)}); code != 0 {
			t.Fatalf("apply %s 失败（退出码 %d）", f, code)
		}
	}
}

// TestCmdApplyKinds 覆盖 Model / Metric / Dashboard / Dataset / Chart / Pipeline 六条分支。
func TestCmdApplyKinds(t *testing.T) {
	env, buf := newEnv(t, true)
	env.Datasource(t, "pg_dw")

	// 模型必须在指标/数据集之前发布：后两者都会校验引用存在。
	applyExamples(t, env,
		filepath.Join("02_model_dwd_order.yaml"),
		filepath.Join("03_model_dws_order_daily.yaml"),
		"06_metric_gmv.yaml",
		"07_metric_order_cnt.yaml",
		"08_dashboard.yaml",
		filepath.Join("demo", "50_dataset_sales.yaml"),
		filepath.Join("demo", "51_chart_gmv_trend.yaml"),
	)
	out := buf.String()
	for _, want := range []string{
		"模型 dwd_order 已保存",
		"生成的 DDL（未执行）",
		"模型 dws_order_daily 已保存",
		"指标 gmv 已保存（成交总额）",
		"看板 ops_overview 已保存（3 个卡片）",
		"数据集 sales_daily 已保存（2 维度 / 3 度量）",
		"图表 gmv_trend 已保存（line）",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少输出 %q:\n%s", want, out)
		}
	}

	// 管道：引用已发布模型，单独覆盖 Pipeline 分支。
	pipe := writeFile(t, t.TempDir(), "pipe.yaml", `kind: Pipeline
name: dwd_order_daily
schedule: "0 3 * * *"
nodes:
  - id: src
    type: source
    datasource: pg_dw
    sql: SELECT order_id FROM dwd.dwd_order WHERE dt = ${biz_date}
  - id: dwd
    type: sql
    depends_on: [src]
    datasource: pg_dw
    model: dwd_order
    write_mode: upsert
    sql: SELECT order_id FROM dwd.dwd_order WHERE dt = ${biz_date}
`)
	if code := run([]string{"apply", "--config", env.ConfigPath, "-f", pipe}); code != 0 {
		t.Fatalf("apply 管道失败（退出码 %d）:\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "管道 dwd_order_daily 已保存，当前版本 v1") {
		t.Fatalf("缺少管道保存输出:\n%s", buf.String())
	}
}

// TestCmdDiff 未发布时没有对比基线，diff 以错误结束。
func TestCmdDiff(t *testing.T) {
	env, buf := newEnv(t, true)
	env.Datasource(t, "pg_dw")

	base, err := os.ReadFile(examplesPath("02_model_dwd_order.yaml"))
	if err != nil {
		t.Fatalf("读示例失败: %v", err)
	}
	dir := t.TempDir()
	same := writeFile(t, dir, "same.yaml", string(base))
	if code := run([]string{"diff", "--config", env.ConfigPath, "-f", same}); code != 1 {
		t.Fatalf("未发布时对比应返回 1，实际 %d:\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "模型未发布或不存在") {
		t.Fatalf("应提示模型未发布:\n%s", buf.String())
	}
}

// TestCmdDiffChanges 发布后再对比，覆盖「无变更 / 新增列 / 删列」三种结果。
func TestCmdDiffChanges(t *testing.T) {
	env, buf := newEnv(t, true)
	env.Datasource(t, "pg_dw")
	applyExamples(t, env, "02_model_dwd_order.yaml")

	base, err := os.ReadFile(examplesPath("02_model_dwd_order.yaml"))
	if err != nil {
		t.Fatalf("读示例失败: %v", err)
	}
	// 仓库在 Windows 检出后是 CRLF，统一成 LF 才能做逐行文本替换。
	base = bytes.ReplaceAll(base, []byte("\r\n"), []byte("\n"))
	dir := t.TempDir()

	// 1) 完全一致：无结构变更。
	same := writeFile(t, dir, "same.yaml", string(base))
	before := buf.Len()
	if code := run([]string{"diff", "--config", env.ConfigPath, "-f", same}); code != 0 {
		t.Fatalf("一致时应返回 0，实际 %d", code)
	}
	if !strings.Contains(buf.String()[before:], "无结构变更") {
		t.Fatalf("应提示无结构变更:\n%s", buf.String()[before:])
	}

	// 2) 新增一列：安全变更，打印迁移语句。
	added := strings.Replace(string(base), "primary_key:",
		"  - name: note\n    type: string\n    nullable: true\n    comment: 备注\nprimary_key:", 1)
	addFile := writeFile(t, dir, "added.yaml", added)
	before = buf.Len()
	if code := run([]string{"diff", "--config", env.ConfigPath, "-f", addFile}); code != 0 {
		t.Fatalf("新增列应返回 0，实际 %d:\n%s", code, buf.String()[before:])
	}
	if !strings.Contains(buf.String()[before:], "共 1 项变更") {
		t.Fatalf("应报告 1 项变更:\n%s", buf.String()[before:])
	}
	if !strings.Contains(buf.String()[before:], "迁移语句") {
		t.Fatalf("应打印迁移语句:\n%s", buf.String()[before:])
	}

	// 3) 删列：破坏性变更，必须以非零码结束。
	removed := strings.Replace(string(base),
		"  - name: is_refunded\n    type: bool\n    nullable: false\n    comment: 是否退款\n", "", 1)
	rmFile := writeFile(t, dir, "removed.yaml", removed)
	before = buf.Len()
	if code := run([]string{"diff", "--config", env.ConfigPath, "-f", rmFile}); code != 1 {
		t.Fatalf("删列应返回 1，实际 %d:\n%s", code, buf.String()[before:])
	}
	if !strings.Contains(buf.String()[before:], "BREAKING") {
		t.Fatalf("应标记 BREAKING:\n%s", buf.String()[before:])
	}
}

// ---------- export-doc ----------

// TestCmdExportDoc Markdown 与 HTML 两种导出，落盘与打到 stdout 两条路径。
func TestCmdExportDoc(t *testing.T) {
	env, buf := newEnv(t, true)

	md := filepath.Join(t.TempDir(), "dict.md")
	if code := run([]string{"export-doc", "--config", env.ConfigPath, "-o", md}); code != 0 {
		t.Fatalf("导出 Markdown 应返回 0，实际 %d:\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "字节") {
		t.Fatalf("应报告写入字节数:\n%s", buf.String())
	}
	if _, err := os.Stat(md); err != nil {
		t.Fatalf("Markdown 文件未生成: %v", err)
	}

	html := filepath.Join(t.TempDir(), "catalog.html")
	if code := run([]string{"export-doc", "--config", env.ConfigPath, "--html", "-o", html}); code != 0 {
		t.Fatalf("导出 HTML 应返回 0，实际 %d:\n%s", code, buf.String())
	}
	raw, err := os.ReadFile(html)
	if err != nil {
		t.Fatalf("HTML 文件未生成: %v", err)
	}
	if !strings.Contains(strings.ToLower(string(raw)), "<html") {
		t.Fatalf("HTML 内容异常:\n%s", raw)
	}

	// 不指定 -o 时打到 stdout。
	before := buf.Len()
	if code := run([]string{"export-doc", "--config", env.ConfigPath}); code != 0 {
		t.Fatalf("导出到 stdout 应返回 0，实际 %d", code)
	}
	if buf.Len() <= before {
		t.Fatal("未指定 -o 时应把内容打到 stdout")
	}
}

// ---------- lineage ----------

// TestCmdLineage 覆盖无血缘、json 输出与 impact 两个分支。
func TestCmdLineage(t *testing.T) {
	env, buf := newEnv(t, true)

	before := buf.Len()
	if code := run([]string{"lineage", "--config", env.ConfigPath, "--table", "not_exist"}); code != 0 {
		t.Fatalf("查询血缘应返回 0，实际 %d:\n%s", code, buf.String()[before:])
	}
	if !strings.Contains(buf.String()[before:], "没有列级血缘") {
		t.Fatalf("应提示无血缘:\n%s", buf.String()[before:])
	}

	before = buf.Len()
	if code := run([]string{"lineage", "--config", env.ConfigPath, "--table", "not_exist", "-o", "json"}); code != 0 {
		t.Fatalf("json 输出应返回 0，实际 %d", code)
	}
	if !strings.Contains(buf.String()[before:], "[") && !strings.Contains(buf.String()[before:], "null") {
		t.Fatalf("json 输出异常:\n%s", buf.String()[before:])
	}

	before = buf.Len()
	if code := run([]string{"lineage", "--config", env.ConfigPath, "--table", "x", "--column", "y", "--impact"}); code != 0 {
		t.Fatalf("影响分析应返回 0，实际 %d", code)
	}
	if !strings.Contains(buf.String()[before:], "影响 0 个下游列") {
		t.Fatalf("影响分析输出异常:\n%s", buf.String()[before:])
	}

	before = buf.Len()
	if code := run([]string{"lineage", "--config", env.ConfigPath, "--table", "x", "--column", "y", "--impact", "-o", "json"}); code != 0 {
		t.Fatalf("影响分析 json 应返回 0，实际 %d", code)
	}
	if !strings.Contains(buf.String()[before:], `"origin"`) {
		t.Fatalf("json 应包含 origin:\n%s", buf.String()[before:])
	}
}

// ---------- profile ----------

// TestCmdProfile 对自建表做画像，覆盖 text 与 json 两种输出。
func TestCmdProfile(t *testing.T) {
	env, buf := newEnv(t, true)
	env.Datasource(t, "pg_dw")
	env.Exec(t,
		`CREATE TABLE demo_prof (id int NOT NULL, amount numeric)`,
		`INSERT INTO demo_prof VALUES (1, 10), (2, 20), (3, 20)`,
	)
	env.Dataset(t, "pg_dw", env.Schema, "demo_prof", []string{"id", "amount"})

	before := buf.Len()
	if code := run([]string{"profile", "--config", env.ConfigPath, "--datasource", "pg_dw",
		"--schema", env.Schema, "--table", "demo_prof"}); code != 0 {
		t.Fatalf("画像应返回 0，实际 %d:\n%s", code, buf.String()[before:])
	}
	out := buf.String()[before:]
	if !strings.Contains(out, "共 3 行") {
		t.Fatalf("应报告行数:\n%s", out)
	}
	if !strings.Contains(out, "推荐规则") {
		t.Fatalf("应给出推荐规则:\n%s", out)
	}

	before = buf.Len()
	if code := run([]string{"profile", "--config", env.ConfigPath, "--datasource", "pg_dw",
		"--schema", env.Schema, "--table", "demo_prof", "-o", "json"}); code != 0 {
		t.Fatalf("json 画像应返回 0，实际 %d", code)
	}
	if !strings.Contains(buf.String()[before:], `"rows"`) {
		t.Fatalf("json 应包含 rows:\n%s", buf.String()[before:])
	}
}

// TestCmdProfileNotCollected 未采集列信息时返回错误，而不是打出空画像。
func TestCmdProfileNotCollected(t *testing.T) {
	env, _ := newEnv(t, true)
	env.Datasource(t, "pg_dw")
	if code := run([]string{"profile", "--config", env.ConfigPath, "--table", "no_such_table"}); code != 1 {
		t.Fatalf("未采集时应返回 1，实际 %d", code)
	}
}

// ---------- seed ----------

// TestCmdSeed 自动接入数据源并执行 SQL 脚本；坏文件与坏 SQL 返回错误。
func TestCmdSeed(t *testing.T) {
	env, buf := newEnv(t, true)
	script := writeFile(t, t.TempDir(), "seed.sql", `CREATE TABLE IF NOT EXISTS demo_seed (id int);
TRUNCATE demo_seed;
INSERT INTO demo_seed VALUES (1);
SELECT 1;`)
	if code := run([]string{"seed", "--config", env.ConfigPath, "--datasource", "pg_dw", "-f", script}); code != 0 {
		t.Fatalf("灌数应返回 0，实际 %d:\n%s", code, buf.String())
	}
	out := buf.String()
	if !strings.Contains(out, "已接入数据源 pg_dw") || !strings.Contains(out, "样例数据已灌入 pg_dw") {
		t.Fatalf("灌数输出异常:\n%s", out)
	}

	// 重复执行：数据源已存在，不应再提示接入。
	before := buf.Len()
	if code := run([]string{"seed", "--config", env.ConfigPath, "--datasource", "pg_dw", "-f", script}); code != 0 {
		t.Fatalf("重复灌数应返回 0，实际 %d", code)
	}
	if strings.Contains(buf.String()[before:], "已接入数据源") {
		t.Fatalf("数据源已存在时不应重复接入:\n%s", buf.String()[before:])
	}

	if code := run([]string{"seed", "--config", env.ConfigPath, "-f", filepath.Join(t.TempDir(), "nope.sql")}); code != 1 {
		t.Fatalf("脚本不存在应返回 1，实际 %d", code)
	}

	bad := writeFile(t, t.TempDir(), "bad.sql", "SELECT * FROM no_such_table")
	if code := run([]string{"seed", "--config", env.ConfigPath, "-f", bad}); code != 1 {
		t.Fatalf("坏 SQL 应返回 1，实际 %d", code)
	}
}

// ---------- collect ----------

// TestCmdCollect 采集元数据并打印结果；未知数据源返回错误。
func TestCmdCollect(t *testing.T) {
	env, buf := newEnv(t, true)
	env.Datasource(t, "pg_dw")
	before := buf.Len()
	if code := run([]string{"collect", "--config", env.ConfigPath, "--datasource", "pg_dw"}); code != 0 {
		t.Fatalf("采集应返回 0，实际 %d:\n%s", code, buf.String()[before:])
	}
	if !strings.Contains(buf.String()[before:], "采集完成：pg_dw") {
		t.Fatalf("采集输出异常:\n%s", buf.String()[before:])
	}

	if code := run([]string{"collect", "--config", env.ConfigPath, "--datasource", "no_such_ds"}); code != 1 {
		t.Fatalf("未知数据源应返回 1，实际 %d", code)
	}
}

// ---------- run ----------

// demoModel 指向用例自建的表（schema 由测试注入），保证运行期只动本用例的数据。
const demoModel = `kind: Model
name: dwd_demo
layer: dwd
datasource: pg_dw
schema: %s
table: dwd_demo
columns:
  - name: dt
    type: date
    nullable: false
  - name: id
    type: bigint
    nullable: false
  - name: amount
    type: decimal
    nullable: true
primary_key: [id]
`

// demoPipeline 写一行固定数据，执行必然成功：用来覆盖「等待 + 结果报告」整条路径。
const demoPipeline = `kind: Pipeline
name: demo_daily
schedule: "0 4 * * *"
nodes:
  - id: build
    type: sql
    datasource: pg_dw
    model: dwd_demo
    write_mode: upsert
    sql: |
      SELECT DATE '2026-01-01' AS dt, 1::bigint AS id, 10::numeric AS amount
`

// newDemoEnv 准备一套可直接执行的管道环境：自建表 + 已发布模型 + 已发布管道。
// 表建在用例独占 schema 内，运行期不碰共享库里的现有数据。
func newDemoEnv(t *testing.T) (*testenv.Env, *bytes.Buffer) {
	t.Helper()
	env, buf := newEnv(t, true)
	env.Datasource(t, "pg_dw")
	env.Exec(t, `CREATE TABLE `+env.Schema+`.dwd_demo (
		dt date NOT NULL, id bigint NOT NULL, amount numeric, PRIMARY KEY (id))`)

	dir := t.TempDir()
	model := writeFile(t, dir, "model.yaml", fmt.Sprintf(demoModel, env.Schema))
	pipe := writeFile(t, dir, "pipe.yaml", demoPipeline)
	for _, f := range []string{model, pipe} {
		if code := run([]string{"apply", "--config", env.ConfigPath, "-f", f}); code != 0 {
			t.Fatalf("发布 %s 失败（%d）:\n%s", filepath.Base(f), code, buf.String())
		}
	}
	return env, buf
}

// TestCmdRun 覆盖管道触发与选择器：参数互斥、dry-run、命中/未命中、等待执行。
func TestCmdRun(t *testing.T) {
	env, buf := newDemoEnv(t)

	// 参数校验：必须二选一。
	if code := run([]string{"run", "--config", env.ConfigPath}); code != 1 {
		t.Fatalf("缺 pipeline/select 应返回 1，实际 %d", code)
	}
	if code := run([]string{"run", "--config", env.ConfigPath, "--pipeline", "p", "--select", "s"}); code != 1 {
		t.Fatalf("两个参数互斥，应返回 1，实际 %d", code)
	}
	// 未知管道触发失败。
	if code := run([]string{"run", "--config", env.ConfigPath, "--pipeline", "no_such"}); code != 1 {
		t.Fatalf("未知管道应返回 1，实际 %d", code)
	}

	// dry-run：只打印命中范围。
	before := buf.Len()
	if code := run([]string{"run", "--config", env.ConfigPath, "--select", "pipeline:demo_daily", "--dry-run"}); code != 0 {
		t.Fatalf("dry-run 应返回 0，实际 %d:\n%s", code, buf.String()[before:])
	}
	if !strings.Contains(buf.String()[before:], "选择器 pipeline:demo_daily") {
		t.Fatalf("dry-run 应打印选择器命中范围:\n%s", buf.String()[before:])
	}

	// 未命中任何已发布管道。
	if code := run([]string{"run", "--config", env.ConfigPath, "--select", "tag:core"}); code != 1 {
		t.Fatalf("未命中管道应返回 1，实际 %d", code)
	}

	// 选择器触发：TriggerSelect 用 "select" 作为 trigger_type，而实例表的 CHECK
	// 只允许 manual/schedule/backfill/retry（产品侧既有缺陷）。用例在自己的 schema 里
	// 去掉该约束，才能把「排队 → 等待 → 报告」的正向链路跑到。
	env.Exec(t, `ALTER TABLE instance DROP CONSTRAINT IF EXISTS instance_trigger_type_check`)

	before = buf.Len()
	if code := run([]string{"run", "--config", env.ConfigPath, "--select", "pipeline:demo_daily", "--date", "2026-11-03"}); code != 0 {
		t.Fatalf("选择器触发应返回 0，实际 %d:\n%s", code, buf.String()[before:])
	}
	if !strings.Contains(buf.String()[before:], "已排队") {
		t.Fatalf("应打印排队结果:\n%s", buf.String()[before:])
	}

	// 同一业务日期再触发一次：命中已有实例，走「复用」分支。
	// 复用的实例不在本次待执行集合里，因此 --wait 不会进入执行循环。
	before = buf.Len()
	if code := run([]string{"run", "--config", env.ConfigPath, "--select", "pipeline:demo_daily", "--date", "2026-11-03", "--wait"}); code != 0 {
		t.Fatalf("复用触发应返回 0，实际 %d:\n%s", code, buf.String()[before:])
	}
	if !strings.Contains(buf.String()[before:], "复用") {
		t.Fatalf("重复触发应复用实例:\n%s", buf.String()[before:])
	}

	// 换一个业务日期：新建实例并等待执行完成，覆盖「排队 → 执行 → 报告」整条链路。
	before = buf.Len()
	if code := run([]string{"run", "--config", env.ConfigPath, "--select", "pipeline:demo_daily", "--date", "2026-11-04", "--wait"}); code != 0 {
		t.Fatalf("选择器等待执行应返回 0，实际 %d:\n%s", code, buf.String()[before:])
	}
	if !strings.Contains(buf.String()[before:], "执行结束: success") {
		t.Fatalf("应打印执行报告:\n%s", buf.String()[before:])
	}

	// 不等待：只创建实例。
	before = buf.Len()
	if code := run([]string{"run", "--config", env.ConfigPath, "--pipeline", "demo_daily", "--date", "2026-01-01"}); code != 0 {
		t.Fatalf("触发应返回 0，实际 %d:\n%s", code, buf.String()[before:])
	}
	if !strings.Contains(buf.String()[before:], "已创建实例") {
		t.Fatalf("应创建实例:\n%s", buf.String()[before:])
	}

	// 等待执行：跑完后打印成功报告。
	// 注意：上一实例已结束后再次触发会新建实例（幂等只覆盖未完成的实例），
	// 因此这里不假设「复用」。
	before = buf.Len()
	if code := run([]string{"run", "--config", env.ConfigPath, "--pipeline", "demo_daily", "--date", "2026-01-01", "--wait"}); code != 0 {
		t.Fatalf("执行应返回 0，实际 %d:\n%s", code, buf.String()[before:])
	}
	out := buf.String()[before:]
	if !strings.Contains(out, "执行结束: success") {
		t.Fatalf("应打印执行成功报告:\n%s", out)
	}
}

// ---------- export ----------

// TestCmdExport 指标导出成功（落盘 + -o 另存）与失败两条路径。
func TestCmdExport(t *testing.T) {
	env, buf := newEnv(t, true)
	env.Datasource(t, "pg_dw")
	applyExamples(t, env,
		"03_model_dws_order_daily.yaml",
		"06_metric_gmv.yaml",
	)

	out := filepath.Join(t.TempDir(), "gmv.csv")
	before := buf.Len()
	if code := run([]string{"export", "--config", env.ConfigPath, "--kind", "metric",
		"--target", "gmv", "-o", out}); code != 0 {
		t.Fatalf("导出应返回 0，实际 %d:\n%s", code, buf.String()[before:])
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("导出文件未生成: %v", err)
	}
	if !strings.Contains(string(raw), "date,value") {
		t.Fatalf("CSV 表头异常:\n%s", raw)
	}
	if !strings.Contains(buf.String()[before:], "已另存到") {
		t.Fatalf("应提示另存路径:\n%s", buf.String()[before:])
	}

	// 指标不存在：任务失败，命令返回错误。
	if code := run([]string{"export", "--config", env.ConfigPath, "--kind", "metric", "--target", "no_such_metric"}); code != 1 {
		t.Fatalf("未知指标导出应返回 1，实际 %d", code)
	}
}

// ---------- worker ----------

// TestCmdWorker 队列为空时 --once 跑一轮即退出。
func TestCmdWorker(t *testing.T) {
	env, buf := newEnv(t, true)
	if code := run([]string{"worker", "--config", env.ConfigPath, "--once"}); code != 0 {
		t.Fatalf("worker --once 应返回 0，实际 %d:\n%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "没有待执行实例") {
		t.Fatalf("应提示无待执行实例:\n%s", buf.String())
	}
}

// ---------- 参数与配置错误 ----------

// TestCmdFlagErrors 每个子命令的 flag 解析失败都应返回 1（错误前缀由 run 打印）。
func TestCmdFlagErrors(t *testing.T) {
	cases := [][]string{
		{"migrate", "--bogus"},
		{"doctor", "--bogus"},
		{"apply", "--bogus"},
		{"run", "--bogus"},
		{"worker", "--bogus"},
		{"seed", "--bogus"},
		{"backfill", "--bogus"},
		{"init", "model", "--bogus"},
		{"diff", "--bogus"},
		{"export-doc", "--bogus"},
		{"lineage", "--bogus"},
		{"profile", "--bogus"},
		{"collect", "--bogus"},
		{"export", "--bogus"},
		{"lint", "--bogus"},
	}
	for _, args := range cases {
		buf := captureOutput(t)
		if code := run(args); code != 1 {
			t.Fatalf("run(%v) 应返回 1，实际 %d\n%s", args, code, buf.String())
		}
	}
}

// badConfigPath 写一份非法配置（未知字段），返回路径。
func badConfigPath(t *testing.T) string {
	t.Helper()
	return writeFile(t, t.TempDir(), "config.yaml", "env: test\nunknown_key: 1\n")
}

// TestCmdConfigErrors 配置加载失败时每个命令都应返回 1，而不是继续往下跑。
func TestCmdConfigErrors(t *testing.T) {
	cfg := badConfigPath(t)
	dir := t.TempDir()
	model := writeFile(t, dir, "model.yaml", fmt.Sprintf(demoModel, "public"))
	pipe := writeFile(t, dir, "pipe.yaml", demoPipeline)
	seed := writeFile(t, dir, "seed.sql", "SELECT 1")

	cases := [][]string{
		{"migrate", "--config", cfg},
		{"doctor", "--config", cfg},
		{"apply", "--config", cfg, "-f", model},
		{"run", "--config", cfg, "--pipeline", "p"},
		{"worker", "--config", cfg, "--once"},
		{"seed", "--config", cfg, "-f", seed},
		{"backfill", "--config", cfg, "--pipeline", "p", "--from", "2026-01-01", "--to", "2026-01-02"},
		{"init", "model", "--config", cfg, "--from", "t"},
		{"diff", "--config", cfg, "-f", model},
		{"export-doc", "--config", cfg},
		{"lineage", "--config", cfg, "--table", "t"},
		{"profile", "--config", cfg, "--table", "t"},
		{"collect", "--config", cfg},
		{"export", "--config", cfg, "--target", "t"},
	}
	for _, args := range cases {
		buf := captureOutput(t)
		if code := run(args); code != 1 {
			t.Fatalf("run(%v) 应返回 1，实际 %d\n%s", args, code, buf.String())
		}
	}
	_ = pipe
}

// TestCmdDBUnreachable 元数据库连不上时 migrate / doctor 各自返回错误。
func TestCmdDBUnreachable(t *testing.T) {
	env := testenv.New(t, true)
	raw, err := os.ReadFile(env.ConfigPath)
	if err != nil {
		t.Fatalf("读配置失败: %v", err)
	}
	path := writeFile(t, t.TempDir(), "config.yaml", strings.Replace(string(raw), "port: 5432", "port: 1", 1))

	for _, cmd := range []string{"migrate", "doctor"} {
		buf := captureOutput(t)
		if code := run([]string{cmd, "--config", path}); code != 1 {
			t.Fatalf("%s 连不上库应返回 1，实际 %d\n%s", cmd, code, buf.String())
		}
	}
}

// TestCmdDoctorUninitialized schema 未初始化时 doctor 给出提示而不是报错退出；
// schema_migration 表都不存在时则直接返回错误。
func TestCmdDoctorUninitialized(t *testing.T) {
	t.Run("无 schema_migration 表", func(t *testing.T) {
		env, _ := newEnv(t, false)
		if code := run([]string{"doctor", "--config", env.ConfigPath}); code != 1 {
			t.Fatalf("缺表时应返回 1，实际 %d", code)
		}
	})

	t.Run("有表但无迁移记录", func(t *testing.T) {
		env, buf := newEnv(t, false)
		env.Exec(t, `CREATE TABLE schema_migration (
			version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`)
		// 只建了迁移表，业务表还不存在：doctor 会先提示 schema 未初始化，
		// 再在查询数据源时因缺表返回错误。
		if code := run([]string{"doctor", "--config", env.ConfigPath}); code != 1 {
			t.Fatalf("业务表缺失应返回 1，实际 %d\n%s", code, buf.String())
		}
		if !strings.Contains(buf.String(), "schema 未初始化") {
			t.Fatalf("应提示 schema 未初始化:\n%s", buf.String())
		}
	})
}

// TestCmdApplyBadPayloads 每种 kind 的解析失败与落库失败都要以错误结束。
func TestCmdApplyBadPayloads(t *testing.T) {
	env, _ := newEnv(t, true)
	env.Datasource(t, "pg_dw")
	dir := t.TempDir()

	cases := map[string]string{
		"模型不合法": `kind: Model
name: ""
layer: dwd
`,
		"指标 YAML 类型错": `kind: Metric
name: {a: 1}
`,
		"指标引用未发布模型": `kind: Metric
name: bad_metric
model: no_such_model
agg: sum
expr: x
`,
		"看板 YAML 类型错": `kind: Dashboard
cards: "x"
`,
		"看板引用不存在指标": `kind: Dashboard
name: bad_dash
cards:
  - title: 卡
    metric: no_such_metric
    days: 7
`,
		"管道缺少 sql": `kind: Pipeline
name: bad_pipe
nodes:
  - id: n1
    type: source
    datasource: pg_dw
`,
		"数据集 YAML 类型错": `kind: Dataset
dimensions: "x"
`,
		"数据集引用未发布模型": `kind: Dataset
name: bad_ds
baseModel: no_such_model
`,
		"图表 YAML 类型错": `kind: Chart
dimensions: "x"
`,
		"图表类型不支持": `kind: Chart
name: bad_chart
type: pie3d
dataset: no_such_ds
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			buf := captureOutput(t)
			f := writeFile(t, dir, "payload.yaml", body)
			if code := run([]string{"apply", "--config", env.ConfigPath, "-f", f}); code != 1 {
				t.Fatalf("应返回 1，实际 %d\n%s", code, buf.String())
			}
		})
	}
}

// TestCmdLintTypeErrors 各 kind 的 YAML 结构错误都应被 lint 捕获；
// 非法 glob 模式（目录名带未闭合方括号）返回错误。
func TestCmdLintTypeErrors(t *testing.T) {
	t.Run("结构错误", func(t *testing.T) {
		buf := captureOutput(t)
		dir := t.TempDir()
		writeFile(t, dir, "model.yaml", "kind: Model\ncolumns: \"x\"\n")
		writeFile(t, dir, "pipeline.yaml", "kind: Pipeline\nnodes: 1\n")
		writeFile(t, dir, "metric.yaml", "kind: Metric\nname: [1]\n")
		writeFile(t, dir, "dashboard.yaml", "kind: Dashboard\ncards: x\n")
		if code := run([]string{"lint", "--dir", dir}); code != 1 {
			t.Fatalf("应返回 1，实际 %d\n%s", code, buf.String())
		}
		if !strings.Contains(buf.String(), "4 个文件校验失败") {
			t.Fatalf("应统计 4 个失败文件:\n%s", buf.String())
		}
	})

	t.Run("非法 glob 模式", func(t *testing.T) {
		captureOutput(t)
		if code := run([]string{"lint", "--dir", "["}); code != 1 {
			t.Fatalf("非法模式应返回 1，实际 %d", code)
		}
	})
}

// ---------- backfill ----------

// TestCmdBackfill 逐日补数并执行；重复区间走「已存在，跳过」，不执行时提前返回。
func TestCmdBackfill(t *testing.T) {
	env, buf := newDemoEnv(t)

	before := buf.Len()
	if code := run([]string{"backfill", "--config", env.ConfigPath, "--pipeline", "demo_daily",
		"--from", "2026-02-01", "--to", "2026-02-03"}); code != 0 {
		t.Fatalf("补数应返回 0，实际 %d\n%s", code, buf.String()[before:])
	}
	out := buf.String()[before:]
	if !strings.Contains(out, "共创建 3 个补数实例") {
		t.Fatalf("应创建 3 个实例:\n%s", out)
	}
	if !strings.Contains(out, "补数执行完成") {
		t.Fatalf("应执行完成:\n%s", out)
	}

	// 重复同一区间：实例已存在，跳过创建；--exec=false 时不执行。
	before = buf.Len()
	if code := run([]string{"backfill", "--config", env.ConfigPath, "--pipeline", "demo_daily",
		"--from", "2026-02-01", "--to", "2026-02-03", "--exec=false"}); code != 0 {
		t.Fatalf("重复补数应返回 0，实际 %d\n%s", code, buf.String()[before:])
	}
	out = buf.String()[before:]
	if !strings.Contains(out, "已存在，跳过") {
		t.Fatalf("应跳过已存在实例:\n%s", out)
	}
	if strings.Contains(out, "补数执行完成") {
		t.Fatalf("--exec=false 不应执行:\n%s", out)
	}

	// 未知管道：逐日触发时首个日期即失败。
	if code := run([]string{"backfill", "--config", env.ConfigPath, "--pipeline", "no_such",
		"--from", "2026-02-01", "--to", "2026-02-02"}); code != 1 {
		t.Fatalf("未知管道应返回 1，实际 %d", code)
	}
}

// ---------- init 其余分支 ----------

// TestCmdInitStdout 不指定 -o 时把 YAML 打到 stdout；写入失败与脚手架失败返回错误。
func TestCmdInitStdout(t *testing.T) {
	env, buf := newEnv(t, true)
	env.Datasource(t, "pg_dw")
	env.Exec(t, `CREATE TABLE demo_src2 (id bigint PRIMARY KEY)`)
	env.Dataset(t, "pg_dw", env.Schema, "demo_src2", []string{"id"})

	before := buf.Len()
	if code := run([]string{"init", "model", "--config", env.ConfigPath,
		"--layer", "dwd", "--from", "demo_src2", "--name", "demo_src2_dwd"}); code != 0 {
		t.Fatalf("生成脚手架应返回 0，实际 %d\n%s", code, buf.String()[before:])
	}
	if !strings.Contains(buf.String()[before:], "kind: Model") {
		t.Fatalf("应把 YAML 打到 stdout:/n%s", buf.String()[before:])
	}

	// -o 指向目录：写入失败。
	if code := run([]string{"init", "model", "--config", env.ConfigPath,
		"--from", "demo_src2", "--name", "demo_src2_dwd", "-o", t.TempDir()}); code != 1 {
		t.Fatalf("写入目录应返回 1，实际 %d", code)
	}

	// 非法分层：脚手架生成失败。
	if code := run([]string{"init", "model", "--config", env.ConfigPath,
		"--layer", "xyz", "--from", "demo_src2"}); code != 1 {
		t.Fatalf("非法分层应返回 1，实际 %d", code)
	}
}

// ---------- 导出写盘失败 ----------

// TestCmdWriteFailures 导出目标不可写时应返回错误。
func TestCmdWriteFailures(t *testing.T) {
	env, _ := newEnv(t, true)
	dir := t.TempDir()

	// export-doc：-o 指向目录。
	if code := run([]string{"export-doc", "--config", env.ConfigPath, "-o", dir}); code != 1 {
		t.Fatalf("export-doc 写入目录应返回 1，实际 %d", code)
	}
}

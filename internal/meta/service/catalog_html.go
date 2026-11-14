package service

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"strings"
	"time"
)

// 静态文档站导出：一份 HTML，双击就能看，也能直接丢进内网共享目录。
//
// 为什么不是「前端截图」或「依赖服务端」：文档站的读者常常是外部协作方或审计，
// 他们没有平台账号。导出的文件必须自带全部样式与数据，断网也能读。
// 因此这里把样式内联、不引用任何外部资源，唯一的脚本只有搜索过滤。

type docPage struct {
	Title       string
	GeneratedAt string
	Layers      []docLayer
	TotalCols   int
	TotalRules  int
}

type docLayer struct {
	Name   string
	Docs   []docView
	Anchor string
}

type docView struct {
	CatalogDoc
	Anchor     string
	LayerLabel string
	FreshText  string
	FreshClass string
	LastRunTxt string
	QualityTxt string
}

var layerLabel = map[string]string{
	"ods": "ODS 接入层", "dim": "DIM 维度层", "dwd": "DWD 明细层",
	"dws": "DWS 汇总层", "ads": "ADS 应用层",
}

// CatalogHTML 把文档站渲染成单文件 HTML。
func (s *Service) CatalogHTML(ctx context.Context, workspaceID string) (string, error) {
	page, err := s.CatalogDocs(ctx, workspaceID)
	if err != nil {
		return "", err
	}
	out := docPage{
		Title:       "数据文档",
		GeneratedAt: time.Now().Format("2006-01-02 15:04"),
	}
	byLayer := map[string][]docView{}
	for _, d := range page.Entries {
		v := docView{CatalogDoc: d, Anchor: docAnchor(d.Name)}
		v.LayerLabel = layerLabel[d.Layer]
		if v.LayerLabel == "" {
			v.LayerLabel = strings.ToUpper(d.Layer) + " 层"
		}
		v.FreshText, v.FreshClass = freshnessText(d.Freshness)
		if d.LastRun != nil {
			v.LastRunTxt = fmt.Sprintf("%s · %s（业务日期 %s）",
				statusLabel(d.LastRun.Status), shortTime(d.LastRun.FinishedAt), d.LastRun.BizDate)
		} else {
			v.LastRunTxt = "还没有运行记录"
		}
		v.QualityTxt = fmt.Sprintf("%d 条规则", len(d.Quality))
		out.TotalCols += len(d.Columns)
		out.TotalRules += len(d.Quality)
		byLayer[d.Layer] = append(byLayer[d.Layer], v)
	}
	for _, l := range page.Layers {
		out.Layers = append(out.Layers, docLayer{
			Name: l, Docs: byLayer[l], Anchor: "layer-" + l,
		})
	}
	var buf bytes.Buffer
	if err := docTemplate.Execute(&buf, out); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func docAnchor(name string) string { return "m-" + strings.ReplaceAll(name, ".", "-") }

func freshnessText(f CatalogFreshness) (string, string) {
	byRule := f.Basis == "quality_rule"
	switch f.Status {
	case "fresh":
		if byRule {
			return fmt.Sprintf("数据新鲜（质量规则判定，阈值 %s）", humanAge(f.ThresholdMin)), "ok"
		}
		return fmt.Sprintf("新鲜（%s 前成功，阈值 %s）", humanAge(f.AgeMinutes), humanAge(f.ThresholdMin)), "ok"
	case "stale":
		if byRule {
			return fmt.Sprintf("数据过期（质量规则未通过，阈值 %s）", humanAge(f.ThresholdMin)), "err"
		}
		return fmt.Sprintf("过期（%s 前成功，阈值 %s）", humanAge(f.AgeMinutes), humanAge(f.ThresholdMin)), "err"
	}
	if f.Basis == "no_run" {
		return "未运行", "mute"
	}
	return fmt.Sprintf("未知（最近成功 %s 前，未配新鲜度规则）", humanAge(f.AgeMinutes)), "warn"
}

func humanAge(min float64) string {
	switch {
	case min < 1:
		return "不到 1 分钟"
	case min < 60:
		return fmt.Sprintf("%.0f 分钟", min)
	case min < 1440:
		return fmt.Sprintf("%.1f 小时", min/60)
	default:
		return fmt.Sprintf("%.1f 天", min/1440)
	}
}

func statusLabel(s string) string {
	switch s {
	case "success":
		return "成功"
	case "failed":
		return "失败"
	case "running":
		return "运行中"
	case "queued":
		return "排队中"
	}
	return s
}

func shortTime(rfc string) string {
	t, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		return rfc
	}
	return t.Local().Format("2006-01-02 15:04")
}

var docTemplate = template.Must(template.New("doc").Funcs(template.FuncMap{
	"join": strings.Join,
}).Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<title>{{.Title}} · GoDW</title>
<style>
:root{
  --bg:#f6f7f9; --panel:#ffffff; --line:#e4e7ec; --text:#1a2330; --dim:#5c6b80;
  --accent:#2563eb; --ok:#0f7b3f; --err:#b42318; --warn:#9a6700;
  --mono:ui-monospace,SFMono-Regular,Menlo,Consolas,"Liberation Mono",monospace;
}
@media (prefers-color-scheme: dark){
  :root{ --bg:#0f1319; --panel:#161b23; --line:#252c37; --text:#e6ebf2; --dim:#93a1b5;
         --accent:#4f8cff; --ok:#3fb950; --err:#f85149; --warn:#d29922; }
}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);
  font:14px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif}
a{color:var(--accent);text-decoration:none}
.layout{display:grid;grid-template-columns:260px 1fr;min-height:100vh}
aside{position:sticky;top:0;height:100vh;overflow:auto;border-right:1px solid var(--line);
  background:var(--panel);padding:18px 14px}
.brand{font-weight:700;font-size:15px;margin-bottom:2px}
.brand-sub{color:var(--dim);font-size:12px;margin-bottom:14px}
.search{width:100%;padding:7px 9px;border:1px solid var(--line);border-radius:7px;
  background:var(--bg);color:var(--text);font-size:12.5px;margin-bottom:12px}
.nav-layer{font-size:11px;letter-spacing:.06em;text-transform:uppercase;color:var(--dim);
  margin:14px 0 4px;font-weight:600}
.nav-item{display:block;padding:3px 6px;border-radius:5px;font-family:var(--mono);font-size:12.5px;color:var(--text)}
.nav-item:hover{background:var(--bg)}
main{padding:32px 40px 80px;max-width:1080px}
h1{font-size:22px;margin:0 0 4px}
.sub{color:var(--dim);font-size:13px;margin-bottom:26px}
.model{background:var(--panel);border:1px solid var(--line);border-radius:10px;
  padding:20px 22px;margin-bottom:22px;scroll-margin-top:20px}
.model h2{font-size:17px;margin:0;font-family:var(--mono)}
.head{display:flex;align-items:baseline;gap:10px;flex-wrap:wrap}
.desc{color:var(--dim);margin:8px 0 14px;font-size:13px}
.pill{display:inline-block;padding:1px 8px;border-radius:999px;font-size:11.5px;
  border:1px solid var(--line);color:var(--dim)}
.pill.ok{color:var(--ok);border-color:var(--ok)}
.pill.err{color:var(--err);border-color:var(--err)}
.pill.warn{color:var(--warn);border-color:var(--warn)}
.meta{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:6px 22px;
  font-size:12.5px;margin:12px 0 18px}
.meta div{display:flex;gap:8px;min-width:0}
.meta b{color:var(--dim);font-weight:500;flex:0 0 76px}
.meta span{min-width:0;overflow-wrap:anywhere}
h3{font-size:12px;letter-spacing:.05em;text-transform:uppercase;color:var(--dim);
  margin:18px 0 8px;font-weight:600}
table{width:100%;border-collapse:collapse;font-size:12.5px}
th,td{text-align:left;padding:5px 8px;border-bottom:1px solid var(--line);vertical-align:top}
th{color:var(--dim);font-weight:500;font-size:11.5px}
td.mono,.mono{font-family:var(--mono);font-size:12px}
.chips{display:flex;gap:6px;flex-wrap:wrap}
pre{background:var(--bg);border:1px solid var(--line);border-radius:7px;padding:12px;
  overflow:auto;font-family:var(--mono);font-size:12px;margin:0;max-height:320px}
.empty{color:var(--dim);font-size:12.5px}
@media (max-width:820px){
  .layout{grid-template-columns:1fr}
  aside{position:static;height:auto;border-right:0;border-bottom:1px solid var(--line)}
  main{padding:20px}
}
@media print{
  aside{display:none}.layout{display:block}.model{break-inside:avoid;border-color:#ccc}
}
</style>
</head>
<body>
<div class="layout">
<aside>
  <div class="brand">数据文档</div>
  <div class="brand-sub">{{.GeneratedAt}} · {{len .Layers}} 个分层 · {{.TotalCols}} 个字段 · {{.TotalRules}} 条规则</div>
  <input class="search" id="q" placeholder="搜索模型 / 字段 / 注释" />
  {{range .Layers}}
    <div class="nav-layer">{{.Name}}</div>
    {{range .Docs}}<a class="nav-item" data-name="{{.Name}} {{.Table}}" href="#{{.Anchor}}">{{.Name}}</a>{{end}}
  {{end}}
</aside>
<main>
  <h1>{{.Title}}</h1>
  <div class="sub">由 GoDW 导出 · 生成于 {{.GeneratedAt}} · 只读快照，不含运行时数据</div>
  {{range .Layers}}{{range .Docs}}
  <section class="model" id="{{.Anchor}}" data-name="{{.Name}} {{.Table}} {{.Description}}">
    <div class="head">
      <h2>{{.Name}}</h2>
      <span class="pill">{{.LayerLabel}}</span>
      {{if .Version}}<span class="pill">v{{.Version}} / 共 {{.Versions}} 版</span>{{end}}
      {{range .Tags}}<span class="pill">{{.}}</span>{{end}}
      <span class="pill {{.FreshClass}}">{{.FreshText}}</span>
    </div>
    {{if .Description}}<div class="desc">{{.Description}}</div>{{end}}
    <div class="meta">
      <div><b>物理表</b><span class="mono">{{if .Table}}{{.Table}}{{else}}—{{end}}</span></div>
      <div><b>Owner</b><span>{{if .Owner}}{{.Owner}}{{else}}未指定{{end}}</span></div>
      <div><b>最近运行</b><span>{{.LastRunTxt}}</span></div>
      <div><b>质量</b><span>{{.QualityTxt}}</span></div>
    </div>

    <h3>字段（{{len .Columns}}）</h3>
    {{if .Columns}}
    <table>
      <thead><tr><th style="width:22%">名称</th><th style="width:14%">类型</th><th style="width:8%">可空</th><th>注释</th><th style="width:16%">分级 / 脱敏</th></tr></thead>
      <tbody>
      {{range .Columns}}
        <tr>
          <td class="mono">{{.Name}}</td>
          <td class="mono">{{.Type}}</td>
          <td>{{if .Nullable}}是{{else}}否{{end}}</td>
          <td>{{.Comment}}</td>
          <td>{{if .Classification}}<span class="pill">{{.Classification}}</span>{{end}}{{if .Mask}}<span class="pill">{{.Mask}}</span>{{end}}</td>
        </tr>
      {{end}}
      </tbody>
    </table>
    {{else}}<div class="empty">没有字段定义</div>{{end}}

    <h3>上下游</h3>
    {{if or .Upstream .Downstream}}
    <div class="chips">
      {{range .Upstream}}<span class="pill">← {{.}}</span>{{end}}
      {{range .Downstream}}<span class="pill">→ {{.}}</span>{{end}}
    </div>
    {{else}}<div class="empty">没有血缘记录</div>{{end}}

    <h3>质量规则</h3>
    {{if .Quality}}
    <table>
      <thead><tr><th style="width:30%">规则</th><th style="width:14%">类型</th><th style="width:14%">列</th><th style="width:10%">级别</th><th>最近结果</th></tr></thead>
      <tbody>
      {{range .Quality}}
        <tr>
          <td class="mono">{{.Name}}</td>
          <td class="mono">{{.Kind}}</td>
          <td class="mono">{{if .Column}}{{.Column}}{{else}}—{{end}}</td>
          <td>{{.Severity}}</td>
          <td>{{if .LastPassed}}<span class="pill ok">通过</span>{{else if .LastAt}}<span class="pill err">未通过</span>{{else}}<span class="empty">未运行</span>{{end}}</td>
        </tr>
      {{end}}
      </tbody>
    </table>
    {{else}}<div class="empty">还没有质量规则（在管道的质量节点里配置）</div>{{end}}

    {{if .DDL}}
    <h3>当前版本 DDL</h3>
    <pre>{{.DDL}}</pre>
    {{end}}
  </section>
  {{end}}{{end}}
</main>
</div>
<script>
// 只做过滤：输入时隐藏不匹配的模型。搜索框在静态页里是刚需——
// 一屏几十个模型，靠滚动找名字比搜索慢得多。
(function(){
  var q = document.getElementById('q');
  if (!q) return;
  var sections = [].slice.call(document.querySelectorAll('.model'));
  var links = [].slice.call(document.querySelectorAll('.nav-item'));
  q.addEventListener('input', function(){
    var s = q.value.trim().toLowerCase();
    sections.forEach(function(el){
      var hit = !s || (el.dataset.name || '').toLowerCase().indexOf(s) >= 0;
      el.style.display = hit ? '' : 'none';
    });
    links.forEach(function(a){
      var hit = !s || (a.dataset.name || '').toLowerCase().indexOf(s) >= 0;
      a.style.display = hit ? '' : 'none';
    });
  });
})();
</script>
</body>
</html>
`))

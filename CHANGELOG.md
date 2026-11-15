# 变更日志

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [未发布]

### Changed
- **「开发」菜单改为向导驱动**：新建 ODS / 模型 / 管道 / 指标不再以 YAML 文本框为入口，
  统一走「选 → 看 → 确认」三步向导（`assets/wizard.js`）。YAML 退到「编辑定义」里作为高级视图，
  仍可手工改，但不再是必经之路。
  - **接入 ODS**：看源表前 8 行真实数据 → 勾选字段（源类型/落库类型/可空/注释并排，带 PII 与必填标记）
    → 选分区/TTL/写入方式并实时预览 DDL。名字按 `ods_` 前缀预填，全程只需三次「下一步」。
    完成后直接给出「继续做 DWD / 打开设计器 / 建管道」三个出口。
  - **派生模型**：从上游模型带出列、主键候选与 SQL，支持「直接带过去」与「SUM/COUNT/去重计数/AVG/MAX/MIN」
    两类列，自动生成 `GROUP BY`；目标层默认取下一层，名字按层前缀改写。
  - **管道**：勾选要产出的模型 → 表格里确认每个节点的数据来源与写入方式（默认按「同后缀上游」猜，
    如 `dwd_order ← ods_order`）→ 用「每天 02:00 / 每周一 / 每小时 / 手动」选调度，cron 由选项翻译，
    同时给出执行图与节点 SQL；可勾选「发布后立刻试跑」。
  - **指标**：选模型与时间列 → 聚合方式与字段、可拆维度 → 命名并**用真实数据试算 30 天曲线**。
- **写接口接受结构化 spec**：`POST /api/v1/models` 与 `POST /api/v1/pipelines` 现在接受 `{spec}`，
  与 `{yaml}` 等价（服务端序列化后再走同一条解析链路）。前端不再手工拼 YAML 字符串，
  避免引号/换行转义错误把校验报错指向「第 N 行」而不是「哪个选择错了」。
- **分层规范前置到界面**：向导读取 `GET /api/v1/layer-rules` 的 `requiredColumns`，
  必填列（如 `dt`）在选择列表里锁定并标注「必填」。此前用户可以取消勾选、一路走到提交才被
  `[required_column]` 拒掉。

### Added
- `POST /api/v1/metrics/preview`：用未保存的指标定义直接跑一次聚合，不落库、不读写缓存。
  指标向导「改口径立刻看曲线」依赖它。
- **运行日志可追溯**：每行新增 `instanceId` / `taskId`（迁移 `0008_run_log_task.sql` 增加
  `run_log.task_id`）。节点级日志由执行器直接带上 `task_instance.id`，实例级日志带实例 ID；
  详情页显示「实例…尾号 · 节点」，悬停看完整标识，并支持按当前筛选复制（TSV，含完整 ID）。
- 执行日志覆盖完整生命周期：实例开始（管道/版本/业务日期/触发方式/资源组/参数）→ 执行计划
  （阶段数/节点数/已完成数）→ 阶段开始 → 节点开始/成功/失败/重试/跳过/阻断 → 实例结束
  （状态/耗时/总行数/错误）。
- 单测：`test/unit/internal/api/wizard_test.go` 覆盖 spec 入参、管道 spec 回读与指标试算；
  `TestRunOnceWritesStructuredLogs` / `TestRunOnceFailureLogsCarryIDs` 覆盖日志完整性与标识。

### Fixed
- **执行日志「没人看就不记」**：`Service.Logs_` 原先在第一次读日志时才初始化，而 `AppendLog`
  在 nil 时静默返回——实例执行完如果没人打开过日志抽屉，`run_log` 里一行都没有。
  现在日志管理器随 `Service` 构造即就绪，实例开始/结束都能落库。
- **日志被截断导致「筛选 ERROR 显示暂无日志」**：快照接口写死 5000、`Buffer.Since` 又把
  limit>1000 回退成 200，且只取最早的 200 行；前端只拉 `limit=200`。ERROR 往往在日志末尾，
  自然永远筛不到。现在快照按 2000 行分页读到耗尽（上限 5 万），前端游标从快照最大 seq 续拉。
- **自动滚动关不掉**：轮询每秒重绘整个日志盒，浏览器重排把滚动位置带回底部。改为增量追加
  DOM；关闭自动滚动时保持 `scrollTop` 不变，新日志只显示「↓ N 条新日志」按钮。
- **日志级别对不上**：下拉值是 `err`，存储层归一成 `error`，筛选永远不匹配。现在选项与
  归一值一致，并在选项与计数行直接显示各级别条数（打开就知道有没有 ERROR）。
- **重跑实例日志 seq 撞车**：实例结束后缓冲释放，重跑时新缓冲从 1 重新编号，和库里的旧
  seq 冲突会让游标读漏行。新增 `SeqReader`，缓冲首行前从 `MAX(seq)` 续号。
- **MySQL 目标的模型建表必失败**：DDL 编译对 MySQL 跳过了 `CREATE DATABASE`（把「SCHEMA 等同
  DATABASE」当成了「无需创建」），模型只要写了 `schema: dws`，建表和执行写入都会撞上
  `Error 1049 (42000): Unknown database 'dws'`——报错看起来像数据源配错，实际是平台少发了一句建库。
  现在 MySQL 与 ClickHouse 一样先发 `CREATE DATABASE IF NOT EXISTS`。
  → `internal/modelc/compile.go`、`TestCompile`（`mysql 先建库` 子用例）。
- **脚手架产出的模型自己过不了校验**：原逻辑「优先用上游第一个日期列当分区，没有才补 dt」，
  遇到只有 `created_at` 的表就不会生成 `dt`，界面弹一条「层 ods 缺少必填列 dt」——
  提示缺列却不补，照着建还一定发布失败。现在分区列默认 `dt`、缺失即补；
  只有显式传 `BizDateColumn` 才会改用别的列，且此时仍保证 `dt` 存在。
  行为变化见 `TestScaffoldPartition`（三例：默认 dt / 显式指定 / 上游已有不重复）。
- **ODS 同步管道的 SQL 引用了不存在的列**：`dt` 与 `_godw_src`/`_godw_load_at` 是平台列、
  源表没有，原来直接 `SELECT <模型全部列>` 会一跑就报错。现在按「源列照搬（改名加 AS）、
  `dt` 取 `${biz_date}`、来源写成字面量、入库时间取 `now()`」拼装，分区条件只在源表真的有该列时才加。
  已用 `ods.shop`（1000 行，只有 `created_at`）实测：建表 → 触发管道 → 1000 行落进 `ods.ods_shop`。

## [1.3.0] - 2026-11-14

### Added
- **看板支持 BI 图表卡**：`semantic.Card` 新增 `chart`（与 `metric` 二选一）。看板按图表自身的
  `type` 渲染折线 / 柱 / 堆叠柱 / 饼 / 环形 / 指标卡 / 表格，`RunCards` 增加图表解析回调，
  单卡失败仍只挂在自己身上。页头全局「时间范围」在数据集存在 `filterable` 的 time 维度时落成
  `>=` 条件，「维度」切片收敛到对应系列。
- **看板可视化编辑**：原来的 YAML 文本框改为「卡片列表 + 高级 YAML 页签」——类型、引用对象、
  标题、天数、宽度、上下移都在行内完成。`POST /api/v1/dashboards` 同时接受 `{dashboard}` 与
  `{yaml}`，两种写法等价。
- `GET /api/v1/dashboards/{name}` 的卡片新增 `kind`（metric/chart）、`chartType`、`series`、
  `chartPoints`；指标卡的 `points` 与 `unit` 保持不变，老前端不受影响。
- **自助 SQL 控制台**：新增 `#/sql` 页面与 `POST /api/v1/sql/query`。
  - 只读守卫（`internal/sqlx/guard.go`）：先跳过字符串字面量与注释再判断，避免 `action='delete'`
    这类内容被误伤；只允许 SELECT / WITH、单语句，拦住 INSERT/UPDATE/DELETE/DROP 等写关键字
    （含 CTE 内的数据修改）与 `FOR UPDATE` 行锁。
  - 执行边界：15 秒超时、行数上限（默认 500，硬上限 5000）、明确标记是否截断。
  - 结果值归一化：`[]byte` 转字符串、`time.Time` 统一格式、pgx 的 `NUMERIC` 转数字
    （否则前端拿到的是 base64 或 `{"Int":..}`）。
  - 列级脱敏按输出列名匹配工作区内全部列策略；行级策略不注入，响应与界面都写明这一点。
  - 权限：admin / editor 可用，viewer 拒绝。审计记录 SQL 指纹 + 前 200 字符预览 + 耗时 / 行数 / 命中列数。
- **跨实例运行矩阵**：新增 `GET /api/v1/instances/matrix?pipeline=&limit=`，返回
  「节点 × 最近 N 次运行」的状态矩阵；运行实例页新增「列表 / 矩阵」切换，两者共用管道筛选。
  点格子或列头打开该次实例的抽屉。
  - `Cells[nodeID]` 与 `Runs` 等长同序、缺节点处为 `null`，某次运行少节点时整列不会错位。
  - 行按节点名排序（不按分层）：矩阵要回答「这个节点最近稳不稳」，行序稳定比分组更重要。
- **数据文档站**：新增 `#/catalog` 与 `GET /api/v1/catalog`，按模型聚合字段（含分级/脱敏）、
  质量规则与最近结果、上下游、版本历史、最近一次运行与新鲜度；`GET /api/v1/catalog/export?format=html`
  与 `godw-cli export-doc --html` 导出**单文件 HTML**（样式内联、无外部资源，断网可读，带搜索过滤）。
  - 服务端一次聚合：字典 + 版本数 + 质量规则 + 最近结果 + 最近运行各一条查询，避免一屏几十个模型
    打出几十次往返。
  - 新鲜度以 `freshness` 规则结果为准；没有规则时退化为「最近一次成功距今」，并明确标注「未配规则」。
- **模型选择器 `--select`**：`internal/meta/service/selector.go` 支持 `dwd_order` / `dwd_order+` /
  `+dwd_order` / `+dwd_order+` / `tag:core` / `layer:dwd` / `pipeline:name` 与 `*` 通配、逗号或空格并集。
  - CLI：`godw-cli run --select "dwd_order+" [--dry-run] [--wait]`。
  - 接口：`GET /api/v1/select/preview?select=`、`POST /api/v1/runs/select`（`dryRun` 只解析）。
  - 界面：运维中心「手动触发」弹窗新增选择器输入与「预览命中」。
  - 响应同时给出 `models` 与 `pipelines`：不支持节点级部分运行，`+model+` 会带出整条管道，必须让人看见放大效应。
- **`freshness` 质量规则**：新规则类型 `kind: freshness`，配置 `column` + `max_age_min|hours|days`（默认 1 天）。
  SQL 只取 `MAX(时间列)`，时间差在 Go 里算——跨引擎写 `EXTRACT(EPOCH)/TIMESTAMPDIFF/dateDiff` 写错不会报错，
  只会永远通过。空表判失败；分区收窄对它不生效。
- **策略命中测试**：`GET /api/v1/policies/simulate?table=&user=&role=` 返回该表在指定身份下**实际生效**的
  脱敏列（原始分级 + 归一化等级）与行过滤（原始表达式 + 占位符渲染结果），治理页新增「策略命中」页签。
  表名支持 `dwd.dwd_order` 与 `dwd_order` 互相命中。
- **500 节点夹具与画布性能**：`scripts/gen_dag.py N` 生成确定性 DAG（默认 500 节点 / 627 条边，自检无环）；
  `scripts/bench_canvas.mjs` 用无头 Chrome 做规模回归（首屏、拖动、缩放、平移、进出 8 次，8 条断言）。
  `make dag-fixture` / `make bench-canvas` 两个目标串起来。

### Changed
- 侧边栏「看板」之后新增「自助查询」入口。
- **图表配色收口到设计 token**：`chart.js` / `bi.js` 的系列色改读 `--chart-1..8`，坐标轴与图例
  文字改读 `--text-mute` / `--text-dim`。亮色主题不再需要单独分支，切换主题会按原数据重放绘制。
- **看板查询带上访问者身份**：新增 `QueryDashboardFor`，把 user/role 透传到图表卡的数据集查询，
  行级与列级策略对图表卡同样生效（此前这条路径没有身份）。

### Fixed
- `bi.js` 用 `svg.clientWidth` 测量宽度，而 SVG 未设宽度时默认只有 300px → viewBox 按 300 计算，
  整行卡片里的图被压到左侧、字号被缩小约 35%。改为先撑满容器再量 `getBoundingClientRect()`，
  `viewBox` 与实际像素一致，窗口缩放时重绘。
- `ChartQuery` 的系列顺序取自 map 遍历，不稳定：前端按索引配色，导致同一条线每次刷新可能换色。
  改为按首次出现顺序输出。
- 环形图挖洞 `fill="var(--bg-elev)"`、网格线 `stroke="var(--line-soft)"` 写在 SVG 表现属性上，
  CSS 变量不解析（洞变黑、网格线消失）。改为运行时取值后写入。
- 饼图扇区名称为空时图例渲染成空白，回落为「(空值)」。
- **行级策略占位符被加了双层引号**：策略写 `region = '${user}'`（最自然的写法）时，`ApplyRowFilters`
  无条件替换成 `'alice'`，渲染结果是 `region = ''alice''`，SQL 语法错误——而且只有该策略真的命中才暴露。
  改为先匹配「被引号包住」的形式再匹配裸占位符，`O'Brien` 之类的取值仍正确转义。
- **空切片序列化成 `null`**：选择器无「未覆盖模型」时返回 `"uncovered": null`，前端 `.length` 直接抛
  `Cannot read properties of null`。服务端改为显式初始化空切片，前端补 `|| []`。
- **脚手架拿不到源表主键，只能靠列名猜**：`connector.TableMeta` 与 `model.ColumnSpec` 都没有主键字段，
  采集落库这一步就把源表声明的 PRIMARY KEY 丢了，于是「源表明明写了主键」却提示「未能推断主键」。
  现在四个连接器都读真实主键：PG 走 `pg_index`（按索引列序）、MySQL 用 `column_key='PRI'`、
  DuckDB 查 `duckdb_constraints()`、ClickHouse 取 `system.tables.primary_key`（跳过表达式排序键），
  随 `dataset.columns` 一起落库；脚手架优先级改为「显式指定 > 源表主键 > 名称启发式」，列名被归一改名时
  主键跟着换名，`key_candidates` 把源表主键排在首位。**老数据集需重新采集一次**才有主键。
- **ODS 因为没有主键被告警**：ODS 本就不建主键约束（规则里 `RequireKey=false`），提示却要求「手动指定
  primary_key，否则无法幂等写入」，自相矛盾。现在 ODS 不再出这条告警，源表主键只作为候选展示。
- **平台自己的审计列反过来逼用户确认有损**：MySQL 上 `_godw_load_at` 的 `datetime → datetime(3)`
  让每张 ODS 都得点一次 `allow_lossy`。`_godw_` 前缀列由平台写入、口径固定 UTC，不再索要确认；
  用户自己声明的列照旧必须显式确认。

### 测试
- `internal/semantic/dashboard_test.go`：卡片二选一校验、图表卡分派、维度切片、单卡失败隔离、
  缺解析器时的报错。
- headless：三卡混排渲染、暗→亮换色、编辑器行控件、BI 页与全屏的 viewBox 与实际像素一致、
  无页面错误。
- `internal/quality/freshness_test.go`：阈值解析（分钟/小时/天/缺省）、通过/过期/空表/字符串时间、
  分区收窄不作用于 freshness。
- `internal/meta/service/selector_test.go`：精确、上下闭包、双向、tag、layer、pipeline、通配、并集、
  未知前缀与空表达式。
- `internal/meta/service/governance_test.go`：表名全名/末段互相命中；占位符单层引号（含 `O'Brien`）。
- `internal/modelc/rules_test.go`：源表主键优先于名称启发式（复合主键、`unique_key` 同步、候选排序）、
  ODS 无主键与平台审计列不再产生告警。
- `scripts/bench_canvas.mjs`：500 节点首屏 40.6ms、DOM 88/500、边 1 条 path、拖动单帧 p95 1.1ms、
  平移/缩放 p95 0.5ms、进出 8 次 52ms→28.8ms。

## [1.2.0] - 2026-11-14

第二轮「补齐」：把审计文档 §4 剩下的 8 条 P1 全部关掉，并补上血缘图的四项验收
（分层色 / 双击展开 / 导出 PNG / 图内搜索）与实例详情的「渲染 SQL 快照」。

### Added
- **实例节点渲染 SQL**：新增 `GET /api/v1/instances/{id}/sql?node=`。用实例创建时的**版本快照**
  加该实例参数渲染（`sqlx.Render` + 模型增量推导的 where，与执行器同一套规则），返回可看 SQL 的
  节点清单、变量、增量说明，以及**上一实例同节点的 SQL**。抽屉里可切节点、复制、对比上次
  （复用 `SqlDiff` 做左右 diff）；版本快照被清理时降级为当前定义并在头部标「非快照」。
- **血缘图分层与读图能力**：节点左侧 4px 层色条 + 层名（ODS/DWD/DWS/ADS/源），图例带数量；
  `Ctrl+F` 落到图内搜索（命中描边、其余淡出到 0.22）；**双击节点聚焦 N 跳邻域**（1/2/3 可选），
  双击空白复原；表级与列级都能导出 PNG（2 倍像素、内联计算样式、按主题补底色）。
- 血缘边改正交折线，多上游汇聚不再互相穿线。
- 表结构树：`public` 下 45 张系统表默认收起（开关显示数量），新增「名称 / 行数 / 大小 / 最近采集」
  四档排序，空态说明「已隐藏 N 张系统表」。
- 设计 token：`--sp-1..6`、`--fs-1..5`、`--r-sm/md/lg`、语义色 `--layer-*` 与交互态
  `--hover/--nav-active/--row-hover/--code-bg/--topbar-bg/--edge`。

### Changed
- 血缘图与管道执行图改为**真实像素**渲染（`viewBox` 宽 = `width` 属性 = CSS 宽），容器窄了
  横向滚动而不是整体缩放——此前 700px 宽时 11.5px 的字实际只有 6.5px。
- 亮色主题不再逐条打补丁：交互态与图色都走变量，删掉 5 条 `:root[data-theme="light"]` 覆盖。
- 顶栏响应式：≤1180 按钮只留图标（`title` 兜底）、搜索框 340→200；≤900 侧栏折叠 56px 图标条；
  ≤760 隐藏搜索、单列网格。按钮统一 30px 高，1024 宽不再出现「刷/新」竖排折断。
- 质量趋势在采样点 < 2 时给文案（说明「至少 2 天」）而不是画空坐标系；模型页空态写清分层顺序。
- 切路由时自动收掉抽屉与弹窗（此前点侧栏跳转后遮罩仍盖着页面）。
- SQL diff 换行由 `break-all` 改为 `overflow-wrap: anywhere`，长表达式不再从中间被切断。

### Fixed
- 全站没有 `:focus-visible`、输入框写死 `outline:none`：统一 2px 焦点环，鼠标点击不画环、键盘必画。
- 血缘图单击下钻会抢在双击之前触发整页重渲染，导致「双击聚焦」永远进不去：单击延迟 240ms
  并自行数连击（不依赖浏览器合成的 `dblclick`）。
- `.dag-wrap` 里的 SVG 未设 `display:block`，`margin:auto` 不生效导致图偏在左边。
- 快速连点侧栏时，上一页的慢查询晚到会把新页面盖回去（`render()` 改为每次换一个新的 `#view`
  容器，旧 render 的写入落在已脱离文档的节点上）。

### 测试
- `audit2.js`（headless）31 项断言全 PASS：系统表默认 10 张 / 展开 55 张、搜索收敛、四档排序；
  血缘 7 节点 4 层、图例 4 项、搜索命中 2 淡出 5、双击聚焦 3/7 并复原、导出 PNG 成功；
  抽屉渲染 SQL 带版本与变量、切节点重新渲染、对比上次出现左右 diff；质量趋势 <2 点断言 `#q-trend` 不存在；
  1024 宽三个顶栏按钮高 30px 且单行、侧栏仍 216px；Tab 焦点环 2px；亮色顶栏按钮非深底。

## [1.1.0] - 2026-11-14

本次补齐的是「交互密度」——对标 Airflow Graph / DolphinScheduler / Metabase 时最欠的那一层。
后端能力没动，全部改动集中在控制台的编排、运行态与实例详情。

### Added
- **编排画布重写**（`assets/canvas.js`）：视口模型 `{tx, ty, scale}`，滚轮以指针为锚点缩放、
  空格/中键平移、空白框选、Shift 加选、多选整体拖动、方向键微调、`0` 适应视图。
- 左侧节点面板（7 类节点，指针拖入画布落点或点击落在视口中心）、右侧属性面板
  （ID/类型/目标模型/数据源/写入模式/依赖/SQL 实时生效，改 ID 自动重连依赖边）。
- 撤销/重做命令栈（上限 60 步，`Ctrl+Z` / `Ctrl+Shift+Z` / `Ctrl+Y`），`Delete` 删除、
  `Ctrl+A` 全选、`Ctrl+S` 保存；连线前在客户端拦成环并给出口语化提示。
- **运行态叠加**：工具栏可选某个实例，节点上显示状态点（成功绿 / 运行中呼吸灯 / 失败红）、
  耗时与影响行数、失败角标；属性面板同步显示该节点的尝试次数，可一键跳转完整日志。
- 编排工具栏新增「校验」：走 `/pipelines/dry-run`，弹出分层、将写入的表、逐节点渲染结果与
  可展开的渲染 SQL；失败节点高亮原因。
- 边改为正交折线（间距不足才绕行），选中节点时相连的边高亮；节点左侧竖条按分层着色
  （ODS 灰 / DWD 蓝 / DWS 青 / ADS 橙），与血缘图同一套语义。
- 侧边导航新增「编排」，管道详情新增「可视化编排」入口（此前 `#/canvas` 只能手输地址进入）。
- 实例详情统一走运维中心的抽屉：参数快照、节点耗时条（按最慢节点归一）、质量结果、
  级别/关键字过滤的实时日志，一次点击到细节；节点按失败优先排序。
- **交互式图表**（新 `assets/chart.js`）：hover 十字参考线 + 一次显示该时间点全部系列的值、
  图例点击隐藏/恢复系列、拖拽刷选时间区间缩放 + 双击复位；尺寸改为按容器实际宽度出图，
  字号回到真实 12px（此前 viewBox 固定 960 被压缩，700px 宽时字只有 7.7px）。
- **看板全局筛选**：页头时间范围（跟随卡片 / 7 / 14 / 30 / 90 天）与维度值切片，服务端按
  `?days=&dim=` 重算每张卡片；卡片可切整行/半宽（本地偏好）与全屏放大。
- **画布小地图**：右下角缩略图按分层着色，视口框随缩放/平移实时跟随，点击或拖动可定位。
- 质量趋势图改用同一图表引擎，hover 能直接看到当天通过率。
- **实例列表重做**：管道/状态/业务日期区间筛选（筛选条件在页面间保留）、服务端分页
  （10/20/50/100）、行多选与批量重跑/取消、失败行可展开错误原文、点整行打开详情。
- **节点级重跑**：`POST /api/v1/instances/{id}/retry` 支持 `{"node": "..."}`，
  原地重置该节点及其全部下游（上游成功结果保留），用于「只有一段算错」的返工场景；
  抽屉与编排画布都提供入口。
- `GET /api/v1/instances` 支持 `pipeline/status/from/to/limit/offset`，返回 `{items,total}`；
  新增 `POST /api/v1/instances/batch`（批量 retry/cancel，逐条容错并返回成功/失败清单）。

### Fixed
- 实例详情显示 `版本 vundefined`：接口字段是 `pipelineVersion`，前端读的是 `pipelineVer`。
- 运维抽屉在 `tasks` 为空时抛 `ReferenceError`（模板里引用了未声明的变量）。
- 抽屉「重跑」后强制跳转运维页，改为刷新当前页面。
- `assets/canvas.js` 的 `renderCanvas` 与 `bi.js` 同名，被后加载的 `bi.js` 覆盖，
  导致编排页白屏（改为 `cvRender` 并统一 `cv` 前缀）。
- 控制台 404（缺 favicon），补内联 SVG 图标。
- 明暗主题下原生控件（日期选择器、复选框、滚动条）不跟随：`:root` 加 `color-scheme`。
- 折线图 y 轴刻度曾堆成一堵墙（KPI 类指标 3044~3101 波动 57，却按 57 算步长得到 2，
  再把下界归零 → 1550 条刻度）。改为先定域（波动小于量级 50% 时不从 0 起）再算步长，并兜底不超过 6 条刻度。
- 图表配色写死在 JS 里，亮色主题下网格线仍是暗色：改为读 CSS 变量，主题切换时重绘。

### 测试
- 用 Chrome headless 对画布做了真操作回归：滚轮缩放（71% → 114% → 44%）、框选 5 个节点、
  删除后 `Ctrl+Z` 恢复到 5 个、拖拽新增节点（5 → 6）、失败实例叠加 1 个角标、
  dry-run 弹出 5 层/将写 3 张表、实例抽屉显示节点耗时与日志。
- 图表：看板 4 张图 hover 出多系列 tooltip（空值/created/paid/refunded 同屏）、点图例 path 4→3 且文字划掉、
  刷选后出现「重置缩放」、程序化 dblclick 复位成功；指标页 tooltip 正常。
- 看板筛选：切「7 天」后卡片标签与数据同步变成 7 天（12 个点），选维度 paid 后只剩 paid 序列、
  无该维度的卡片显示「该维度值下没有数据」。
- 小地图：5 个缩略块，点击左侧后 `tx 12 → 258`，视口框随之移动。
- 实例列表：18 条 → 筛选 `failed` 得 2 条 → 每页 10 条翻页得 10/8 条 → 勾选后批量条出现；
  `q_strict` 节点重跑后实例回 `queued`、两节点重置为 `pending`，下一轮执行再次按质量门禁失败
  （符合预期），批量接口对不存在的 ID 单独返回失败而不影响其余。

## [1.0.0] - 2026-11-14

### Added
- 血缘页支持列级下钻：表级 DAG 点节点进入字段视图，字段按表分泳道，可切双向/仅上游/仅下游，
  点任意字段可跨表继续追；同时给出该字段的下游影响面。
- `GET /api/v1/lineage/column-graph`：列级子图查询，带深度限制、节点上限与 TTL 缓存。
- `scripts/release.sh`：一键打出 macOS（Intel / Apple Silicon）与 Linux 的压缩包，
  包内含二进制、配置、迁移、示例与文档，附 `SHA256SUMS.txt`；使用说明见
  `docs/12-离线包使用说明.md`，拿到包的人不需要 Go 环境。
- 三个二进制统一支持版本输出：`godw-server -version`、`godw-worker --version`、`godw-cli version`。

### Fixed
- `deploy/init/01_bootstrap.sql` 使用了 MySQL 的 `CREATE DATABASE IF NOT EXISTS`，
  在全新数据卷上会让 PostgreSQL 容器初始化失败并退出（`make up` 首次启动即复现）。
  改为 `\gexec` 条件建库，并把授权切到 `dw` 库执行。
- 开发环境数据库账号不一致：compose 建的是 `godw/godw`，而 `config.dev.yaml`、`scripts/seed.sh`
  与 Makefile 默认 `postgres/postgres`，导致 `make migrate` / `make demo` 认证失败。
- 列级血缘把「同表同列」的自引用写入存储，血缘图上出现自环；记录与查询两侧都做了过滤。
- 列级血缘默认聚焦列会选中 `dt`（每张表都有、被引用最多），改为优先普通字段。
- 「模型设计 → 血缘」tab 报 `Cannot read properties of null (reading 'forEach')`：
  空图时 `Graph.Edges` 为 nil，序列化成 `"edges":null` 打崩前端。空图一律返回 `[]`；
  `/lineage/focus` 的 `direction` 缺省改为双向（ODS 表只有上游，只看下游永远是空图），
  前端同时修正了节点形态（对象 → id）与上下游计数（原先读不存在的 `g.upstream`，恒显示 0）。

### 测试与 CI
- 新增 `test/integration`：对 PG/MySQL/ClickHouse/DuckDB 各建一张表、插数、`COUNT/SUM`、
  采集列信息、删表，真正连库执行（`make test-integration`）。
- CI 的 Go 版本改为跟随 `go.mod`（原先写死 1.22，模块要求 1.25 会直接构建失败）；
  数据库账号与 compose/配置对齐为 `godw/godw`，并新增 `integration` job。

### 引擎验证（真实实例）
在 PostgreSQL 16 / MySQL 8.4 / ClickHouse 24.10 / DuckDB 1.1 上跑通建表、写入、质量、指标、
采集与列级血缘，并修复以下只在真实引擎上暴露的问题（完整记录见 `docs/11-引擎验证记录.md`）：

- ClickHouse 建库语句误用 `CREATE SCHEMA`（应为 `CREATE DATABASE`），真实实例直接语法错误；
- ClickHouse 能力位误声明 `InsertOverwrite`，导致分区覆写生成 Hive 语法的 `INSERT OVERWRITE`；
- `REPLACE PARTITION` 的临时表名拼在反引号外（`` `dw`.`tbl`__tmp ``），生成的 SQL 非法；
- 文本协议引擎返回的日期字符串无法扫入 `time.Time`，指标时间轴全为 `0001-01-01`；
- DuckDB 连接器从未被编入任何二进制，`-tags duckdb` 也因缺少依赖失败；默认二进制的
  「未启用 DuckDB」提示同时被外层包装吞掉；
- DuckDB 事务内 `DELETE`+`INSERT` 相同主键会被唯一索引拒绝（known index limitations），
  分区覆写改为两段提交并给出降级告警；
- DuckDB `column_default` 为 NULL 时元数据采集整表失败（采集结果为 0 张表）；
- DuckDB `DECIMAL` 以结构体返回，接口输出 `{"Width":18,"Scale":2,...}`，指标查询类型转换报错。

### 计划
- 看板拖拽布局（当前用 YAML 的 `span` 控制）
- 指标预聚合表与自动回填
- 列级血缘的窗口函数支持

## [0.1.0] - 2026-11-11

首个可用版本：ETL → ODS → DWD → DWS → ADS → BI 全链路打通。

### Added

**接入**
- PostgreSQL / MySQL / ClickHouse 连接器，`Capability` 能力矩阵
- 元数据采集：schema → table → columns，逐表失败隔离
- 数据源凭证 AES-256-GCM 加密存储，接口不回传明文

**建模**
- YAML 模型定义 → 校验 → DDL 编译（4 方言）
- 版本化发布，发布版本不可变，支持回滚
- 表、列、分区、唯一键、质量的完整校验，问题一次性收集

**管道**
- 严格 YAML 解析，成环/依赖/参数/节点字段校验
- 7 类节点：source / sql / quality / sink / shell / http / branch
- 4 种写入策略：append / overwrite / upsert / overwrite_partition
- 有 `Transactor` 的引擎包事务，失败整体回滚
- SQL 模板 `${var}` 渲染，值按字面量转义，杜绝注入

**调度**
- 5 段 cron，T-1 派发，重启补偿窗口
- 幂等键（schedule/backfill 为 `pipeline_id:biz_date`）
- `SKIP LOCKED` 领取 + 租约心跳 + 僵尸回收
- 补数 CLI，按日期区间批量建实例

**质量与血缘**
- 6 类规则：not_null / unique / accepted_values / range / row_count / custom
- error 级门禁阻断下游，warn 只记录
- 表级血缘（depends_on）+ 列级血缘（SQL 解析）
- 影响分析：从某列向下追溯所有受影响的下游列

**语义层**
- 指标定义（聚合白名单 + 维度校验），不开放任意 SQL
- 时间序列查询，按天/周/月聚合，支持维度拆分
- 多指标看板，`span` 控制布局
- 指标结果缓存，实例结束后自动失效

**治理**
- JWT 登录 + RBAC（admin/editor/viewer）
- 列级脱敏（partial/hash/email/phone/full）与数据分级
- 行级权限，支持 `${user}` / `${role}` 占位符
- 审计日志，写操作全留痕
- 多租户：工作区隔离、成员管理、工作区切换
- 公开分享链接，可设过期时间

**交付**
- 单二进制 + `go:embed` 内嵌控制台，10 个页面
- CLI：migrate / doctor / apply / run / seed / backfill / lineage / profile / export / lint
- 管道可视化编排：拖拽、连线、双击编辑，保存即写回 YAML
- Docker Compose、多阶段镜像、Helm Chart
- GitHub Actions：lint / test / build / e2e
- Prometheus `/metrics`，手写导出器无额外依赖
- 文档站生成脚本（`scripts/build_docs.py`）

### 设计取舍

| 项 | 选择 | 理由 |
|---|---|---|
| 调度中间件 | 不引入 Kafka/Temporal | PG 的 `SKIP LOCKED` + 租约，少一个组件 |
| SQL 访问层 | pgx 手写 SQL | 血缘递归、实例分页用生成器反而难维护 |
| 前端 | 原生 JS + `go:embed` | 单二进制可用，无 npm 构建链 |
| 指标表达式 | 只允许列名 + 聚合白名单 | 指标是业务口径，不该成为注入面 |
| 日志 | `log/slog` | 标准库够用，少一个依赖 |
| 指标导出 | 手写 Prometheus 文本格式 | 指标集小，不值得引入 client_golang |

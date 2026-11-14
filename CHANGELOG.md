# 变更日志

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

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

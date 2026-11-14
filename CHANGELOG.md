# 变更日志

格式遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

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

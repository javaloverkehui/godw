# 贡献指南

## 1. 开发环境

```bash
# 需要 Go 1.22+、PostgreSQL 16（或用 docker compose）
go version
make up          # 起 PG
make migrate     # 建表
make seed        # 灌样例数据
make run         # 起服务，http://localhost:8080
```

没有 Docker 时，本机装 PostgreSQL 后：

```bash
createdb godw
GODW_DB_PASSWORD=postgres GODW_AUTH_JWT_SECRET=dev-secret go run ./cmd/godw-server --config config.dev.yaml
```

## 2. 提交前必做

```bash
gofmt -w cmd internal
go vet ./...
go test ./...
godw-cli lint --dir examples     # YAML 定义校验
```

CI 会跑同样四项，任一失败即拒。

## 3. 代码约定

### 目录

```
cmd/            可执行入口：server / worker / cli / bench
internal/       全部业务代码，不对外暴露
  api/          HTTP 层：路由、中间件、handler
  meta/         元数据领域：model / repo / service
  connector/    数据源连接器，每个引擎一个子包
  dialect/      SQL 方言
  modelc/       模型编译器（YAML → DDL）
  pipeline/     管道 DSL、编译、节点执行
  semantic/     语义层：指标、看板
  scheduler/    cron 与派发
  quality/      质量规则
  lineage/      SQL 解析与血缘
migrations/     数据库迁移，只增不删
web/            （预留）独立前端工程
docs/           设计文档
examples/       可直接 apply 的样例定义
```

### 命名

- 包名小写单词，不用下划线：`modelc` 而非 `model_compile`
- 接口名不加 `I` 前缀：`Connector` 而非 `IConnector`
- 错误用 `errs.New(code, msg)`，不要裸 `errors.New`
- 日志用 `slog` 的键值对，键名小写下划线：`"node_id"`

### 注释

只写「为什么」，不写「是什么」：

```go
// ✅ 幂等键带随机后缀：manual 触发允许重复执行，便于人工调试
key := fmt.Sprintf("%s:%s:%s", pipelineID, bizDate, rand.String(6))

// ❌ 生成幂等键
key := ...
```

导出的函数必须有注释，且以函数名开头。

## 4. 测试要求

| 改动类型 | 必须补的测试 |
|---|---|
| 新增纯函数/算法 | 表驱动单测，覆盖边界 |
| 新增 API | 至少一个 handler 测试（成功 + 参数错误） |
| 改 SQL | 在本地 PG 上跑通，且 `EXPLAIN` 无全表扫描（大表） |
| 改迁移 | `migrate --check` 在两个版本间往返验证 |
| 改前端 | 截图对比，确认布局无回归 |

测试统一放在根目录 `test/` 下，目录按被测包镜像，包名用 `<pkg>_test` 黑盒：

```
test/unit/internal/meta/repo/     ← internal/meta/repo
test/unit/internal/meta/service/  ← internal/meta/service
test/unit/internal/api/           ← internal/api/http
test/unit/cmd/cli/                ← internal/cli（命令实现与入口分离）
test/testenv/                     命令层夹具（独占 schema + 临时配置）
```

源码包内不再放 `_test.go`；需要驱动未导出的逻辑时，优先从导出的公开入口构造场景，
确有必要再把该能力提升为导出函数，而不是把测试放回源码目录。

覆盖率门禁：`bash scripts/cover.sh`（需要本机 PostgreSQL），整体语句覆盖率 ≥ 95%；
逐块定位缺口用 `PKG=internal/meta/repo bash scripts/cover-gaps.sh cover.out`。

## 5. 提交信息

```
<type>(<scope>): <subject>

<body 可选>

<footer 可选>
```

| type | 用途 |
|---|---|
| `feat` | 新功能 |
| `fix` | 修 bug |
| `refactor` | 重构，不改行为 |
| `perf` | 性能优化 |
| `docs` | 文档 |
| `test` | 测试 |
| `chore` | 构建、依赖、脚本 |

例：

```
feat(lineage): 支持 CTE 与子查询的列级血缘

之前 CTE 别名被当成物理表，导致血缘断链。改为在解析时
把虚拟表输出展开成基表列，无法归属时丢弃而不是猜测。

Closes #42
```

## 6. 提 PR

1. 从 `main` 切分支：`feat/xxx` 或 `fix/xxx`
2. 一个 PR 只做一件事，改动控制在 400 行以内
3. 描述里写清：**改了什么、为什么、怎么验证**
4. 涉及 UI 的附截图；涉及性能的附压测数据
5. 等 CI 全绿再请人 review

## 7. 报告问题

请附：

- 版本（`godw-cli version`）
- 复现步骤（能贴 YAML 就贴 YAML）
- 实际结果 vs 期望结果
- 相关日志（带 `request_id` 最好）
- `godw-cli doctor` 的输出（脱敏后）

## 8. 安全

发现漏洞**不要**开公开 issue。发邮件到 security@example.com，48 小时内会回复。

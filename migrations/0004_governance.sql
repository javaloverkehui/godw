-- +goose Up
-- 列级血缘：每条记录一个「上游表.列 → 下游表.列」的依赖。
-- 表级血缘仍在 lineage_edge，列级是其细化；两者都保留，便于不同粒度的查询。
CREATE TABLE IF NOT EXISTS lineage_column (
    workspace_id      TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    downstream_table  TEXT NOT NULL,
    downstream_column TEXT NOT NULL,
    upstream_table    TEXT NOT NULL,
    upstream_column   TEXT NOT NULL,
    node_id           TEXT NOT NULL DEFAULT '',
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, downstream_table, downstream_column, upstream_table, upstream_column)
);
CREATE INDEX IF NOT EXISTS idx_lineage_col_up ON lineage_column(workspace_id, upstream_table, upstream_column);

-- 列策略：数据分级 + 脱敏方式。查询结果返回前按此表处理。
CREATE TABLE IF NOT EXISTS column_policy (
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    table_name   TEXT NOT NULL,
    column_name  TEXT NOT NULL,
    classification TEXT NOT NULL DEFAULT 'internal', -- public/internal/confidential/secret
    mask_type    TEXT NOT NULL DEFAULT 'none',       -- none/partial/hash/email/phone/full
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, table_name, column_name)
);

-- 行级权限：按角色给某张表加过滤条件，查询改写时注入。
CREATE TABLE IF NOT EXISTS row_policy (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    table_name   TEXT NOT NULL,
    role         TEXT NOT NULL DEFAULT '*',
    filter_expr  TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, table_name, role)
);

-- 指标/看板查询结果缓存。key 由「工作区+指标+参数」哈希得到。
CREATE TABLE IF NOT EXISTS query_cache (
    cache_key   TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    payload     JSONB NOT NULL,
    hit_count   BIGINT NOT NULL DEFAULT 0,
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_cache_expire ON query_cache(expires_at);

-- 导出任务：结果文件落盘，前端轮询状态后下载。
CREATE TABLE IF NOT EXISTS export_job (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    kind         TEXT NOT NULL,             -- metric/table/instance
    target       TEXT NOT NULL,
    params       JSONB NOT NULL DEFAULT '{}'::jsonb,
    status       TEXT NOT NULL DEFAULT 'queued', -- queued/running/success/failed
    file_path    TEXT NOT NULL DEFAULT '',
    row_count    BIGINT NOT NULL DEFAULT 0,
    error        TEXT NOT NULL DEFAULT '',
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_export_ws ON export_job(workspace_id, created_at DESC);

-- 订阅：定时把指标/看板结果推送到 webhook 或落盘。
CREATE TABLE IF NOT EXISTS subscription (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    kind         TEXT NOT NULL DEFAULT 'metric', -- metric/dashboard
    target       TEXT NOT NULL,                  -- 指标或看板名
    channel      TEXT NOT NULL DEFAULT 'webhook',-- webhook/file
    endpoint     TEXT NOT NULL DEFAULT '',
    schedule     TEXT NOT NULL DEFAULT '',
    config       JSONB NOT NULL DEFAULT '{}'::jsonb,
    enabled      BOOLEAN NOT NULL DEFAULT true,
    last_run_at  TIMESTAMPTZ,
    last_status  TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

-- 分享链接：一次性 token，可设过期时间；访问时不要求登录。
CREATE TABLE IF NOT EXISTS share_link (
    token        TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    kind         TEXT NOT NULL,  -- dashboard/metric
    target       TEXT NOT NULL,
    expires_at   TIMESTAMPTZ,
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- 多租户：工作区即租户，成员表记录谁属于哪个租户、什么角色。
CREATE TABLE IF NOT EXISTS workspace_member (
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    user_id      TEXT NOT NULL,
    role         TEXT NOT NULL DEFAULT 'viewer',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (workspace_id, user_id)
);

-- +goose Down
DROP TABLE IF EXISTS workspace_member;
DROP TABLE IF EXISTS share_link;
DROP TABLE IF EXISTS subscription;
DROP TABLE IF EXISTS export_job;
DROP TABLE IF EXISTS query_cache;
DROP TABLE IF EXISTS row_policy;
DROP TABLE IF EXISTS column_policy;
DROP TABLE IF EXISTS lineage_column;

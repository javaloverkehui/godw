-- +goose Up
-- 元数据库：只存平台自身的元数据，业务数据留在用户的数据源里。
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS workspace (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS app_user (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    email         TEXT NOT NULL,
    display_name  TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'viewer' CHECK (role IN ('admin','editor','viewer')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, email)
);

CREATE TABLE IF NOT EXISTS datasource (
    id          TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    type        TEXT NOT NULL,
    config      JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- 凭证单独加密存放（AES-GCM），API 永远不返回明文
    secret_enc  BYTEA,
    status      TEXT NOT NULL DEFAULT 'unknown' CHECK (status IN ('unknown','ok','error')),
    last_error  TEXT NOT NULL DEFAULT '',
    checked_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

-- 采集到的表结构快照。按 (datasource, schema, table) 唯一，重复采集覆盖。
CREATE TABLE IF NOT EXISTS dataset (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    datasource_id TEXT NOT NULL REFERENCES datasource(id) ON DELETE CASCADE,
    schema_name   TEXT NOT NULL,
    table_name    TEXT NOT NULL,
    table_type    TEXT NOT NULL DEFAULT 'table',
    comment       TEXT NOT NULL DEFAULT '',
    row_count     BIGINT NOT NULL DEFAULT 0,
    size_bytes    BIGINT NOT NULL DEFAULT 0,
    columns       JSONB NOT NULL DEFAULT '[]'::jsonb,
    collected_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (datasource_id, schema_name, table_name)
);
CREATE INDEX IF NOT EXISTS idx_dataset_ws ON dataset(workspace_id, datasource_id);

CREATE TABLE IF NOT EXISTS model (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    layer        TEXT NOT NULL CHECK (layer IN ('ods','dwd','dws','ads','dim')),
    description  TEXT NOT NULL DEFAULT '',
    datasource_id TEXT REFERENCES datasource(id) ON DELETE SET NULL,
    -- 当前生效版本；发布新版本时整体切换，回滚只需指回旧版本
    active_version INT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

CREATE TABLE IF NOT EXISTS model_version (
    id           TEXT PRIMARY KEY,
    model_id     TEXT NOT NULL REFERENCES model(id) ON DELETE CASCADE,
    version      INT  NOT NULL,
    spec         JSONB NOT NULL,
    -- 编译后的 DDL 快照：模型改了也不会让历史执行漂移
    ddl          TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','published','archived')),
    published_by TEXT NOT NULL DEFAULT '',
    published_at TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (model_id, version)
);

CREATE TABLE IF NOT EXISTS pipeline (
    id            TEXT PRIMARY KEY,
    workspace_id  TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    schedule      TEXT NOT NULL DEFAULT '',
    timezone      TEXT NOT NULL DEFAULT 'Asia/Shanghai',
    concurrency   INT NOT NULL DEFAULT 1,
    enabled       BOOLEAN NOT NULL DEFAULT true,
    active_version INT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

CREATE TABLE IF NOT EXISTS pipeline_version (
    id          TEXT PRIMARY KEY,
    pipeline_id TEXT NOT NULL REFERENCES pipeline(id) ON DELETE CASCADE,
    version     INT NOT NULL,
    spec        JSONB NOT NULL,
    status      TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','published','archived')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (pipeline_id, version)
);

-- 一次调度产生一个 instance；幂等键保证同一业务时间只跑一次。
CREATE TABLE IF NOT EXISTS instance (
    id             TEXT PRIMARY KEY,
    workspace_id   TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    pipeline_id    TEXT NOT NULL REFERENCES pipeline(id) ON DELETE CASCADE,
    pipeline_ver   INT NOT NULL,
    biz_date       DATE NOT NULL,
    trigger_type   TEXT NOT NULL DEFAULT 'manual' CHECK (trigger_type IN ('manual','schedule','backfill','retry')),
    status         TEXT NOT NULL DEFAULT 'queued'
                   CHECK (status IN ('queued','running','success','failed','canceled','waiting')),
    idempotency_key TEXT NOT NULL,
    params         JSONB NOT NULL DEFAULT '{}'::jsonb,
    error          TEXT NOT NULL DEFAULT '',
    -- instance 级租约：worker 认领后定期续期，超时由 recover 循环退回 queued
    worker_owner   TEXT NOT NULL DEFAULT '',
    lease_until    TIMESTAMPTZ,
    started_at     TIMESTAMPTZ,
    finished_at    TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_instance_pipeline ON instance(pipeline_id, biz_date DESC);
CREATE INDEX IF NOT EXISTS idx_instance_status ON instance(status, created_at) WHERE status IN ('queued','running');
CREATE INDEX IF NOT EXISTS idx_instance_lease ON instance(lease_until) WHERE status='running';

CREATE TABLE IF NOT EXISTS task_instance (
    id           TEXT PRIMARY KEY,
    instance_id  TEXT NOT NULL REFERENCES instance(id) ON DELETE CASCADE,
    node_id      TEXT NOT NULL,
    node_type    TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','running','success','failed','skipped','blocked')),
    attempt      INT NOT NULL DEFAULT 0,
    -- 租约：worker 定期续期，超时即视为死亡并可被接管
    lease_owner  TEXT NOT NULL DEFAULT '',
    lease_until  TIMESTAMPTZ,
    rows_affected BIGINT NOT NULL DEFAULT 0,
    duration_ms  BIGINT NOT NULL DEFAULT 0,
    error        TEXT NOT NULL DEFAULT '',
    started_at   TIMESTAMPTZ,
    finished_at  TIMESTAMPTZ,
    UNIQUE (instance_id, node_id)
);
CREATE INDEX IF NOT EXISTS idx_task_lease ON task_instance(status, lease_until) WHERE status IN ('pending','running');

CREATE TABLE IF NOT EXISTS quality_rule (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    model_name   TEXT NOT NULL,
    name         TEXT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('not_null','unique','accepted_values','range','row_count','custom_sql')),
    column_name  TEXT NOT NULL DEFAULT '',
    config       JSONB NOT NULL DEFAULT '{}'::jsonb,
    severity     TEXT NOT NULL DEFAULT 'error' CHECK (severity IN ('warn','error')),
    enabled      BOOLEAN NOT NULL DEFAULT true,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, model_name, name)
);

CREATE TABLE IF NOT EXISTS quality_result (
    id          TEXT PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instance(id) ON DELETE CASCADE,
    node_id     TEXT NOT NULL,
    rule_name   TEXT NOT NULL,
    passed      BOOLEAN NOT NULL,
    observed    TEXT NOT NULL DEFAULT '',
    expected    TEXT NOT NULL DEFAULT '',
    severity    TEXT NOT NULL DEFAULT 'error',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS lineage_edge (
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    upstream     TEXT NOT NULL,
    downstream   TEXT NOT NULL,
    node_id      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (workspace_id, upstream, downstream, node_id)
);

CREATE TABLE IF NOT EXISTS audit_log (
    id           BIGSERIAL PRIMARY KEY,
    workspace_id TEXT NOT NULL,
    actor        TEXT NOT NULL DEFAULT '',
    action       TEXT NOT NULL,
    resource     TEXT NOT NULL DEFAULT '',
    detail       JSONB NOT NULL DEFAULT '{}'::jsonb,
    request_id   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_audit_ws_time ON audit_log(workspace_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS lineage_edge;
DROP TABLE IF EXISTS quality_result;
DROP TABLE IF EXISTS quality_rule;
DROP TABLE IF EXISTS task_instance;
DROP TABLE IF EXISTS instance;
DROP TABLE IF EXISTS pipeline_version;
DROP TABLE IF EXISTS pipeline;
DROP TABLE IF EXISTS model_version;
DROP TABLE IF EXISTS model;
DROP TABLE IF EXISTS dataset;
DROP TABLE IF EXISTS datasource;
DROP TABLE IF EXISTS app_user;
DROP TABLE IF EXISTS workspace;

-- 0005：执行面（队列/资源组/日志/告警）+ 语义层（数据集/图表）+ 治理（RBAC/生命周期）+ 元数据（标签/搜索/PII/变更）
-- 设计取舍：所有新表都带 workspace_id 并建复合索引，多租户过滤走索引而不是全表扫。

-- ---------- 资源组与队列 ----------
CREATE TABLE IF NOT EXISTS resource_group (
    id              TEXT PRIMARY KEY,
    workspace_id    TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    name            TEXT NOT NULL,
    max_concurrency INT NOT NULL DEFAULT 4,
    max_timeout_sec INT NOT NULL DEFAULT 3600,
    max_pool        INT NOT NULL DEFAULT 10,
    engines         TEXT[] NOT NULL DEFAULT '{}',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

ALTER TABLE pipeline ADD COLUMN IF NOT EXISTS resource_group TEXT NOT NULL DEFAULT 'default';
ALTER TABLE pipeline ADD COLUMN IF NOT EXISTS priority INT NOT NULL DEFAULT 5;
ALTER TABLE instance ADD COLUMN IF NOT EXISTS resource_group TEXT NOT NULL DEFAULT 'default';
ALTER TABLE instance ADD COLUMN IF NOT EXISTS priority INT NOT NULL DEFAULT 5;
ALTER TABLE instance ADD COLUMN IF NOT EXISTS queued_at TIMESTAMPTZ;
ALTER TABLE instance ADD COLUMN IF NOT EXISTS cpu_ms BIGINT NOT NULL DEFAULT 0;
ALTER TABLE instance ADD COLUMN IF NOT EXISTS scanned_bytes BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_instance_queue ON instance(priority, queued_at)
    WHERE status IN ('queued','running');

-- ---------- 运行日志 ----------
CREATE TABLE IF NOT EXISTS run_log (
    id          BIGSERIAL PRIMARY KEY,
    instance_id TEXT NOT NULL REFERENCES instance(id) ON DELETE CASCADE,
    node_id     TEXT NOT NULL DEFAULT '',
    seq         INT NOT NULL DEFAULT 0,
    level       TEXT NOT NULL DEFAULT 'info',
    message     TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_run_log_instance ON run_log(instance_id, id);
-- 归档后 DB 只留索引：archived_at 非空的记录 message 已被裁剪
ALTER TABLE run_log ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;

-- ---------- 告警 ----------
CREATE TABLE IF NOT EXISTS alert_rule (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('sla_deadline','duration','failure','row_count_zero','volume_drop','sensor_timeout')),
    -- 目标：管道名或 * 表示全部
    target       TEXT NOT NULL DEFAULT '*',
    threshold    DOUBLE PRECISION NOT NULL DEFAULT 0,
    -- cron 或 HH:MM 形式，视 kind 而定
    deadline     TEXT NOT NULL DEFAULT '',
    severity     TEXT NOT NULL DEFAULT 'warning' CHECK (severity IN ('info','warning','critical')),
    channels     TEXT[] NOT NULL DEFAULT '{}',
    enabled      BOOLEAN NOT NULL DEFAULT true,
    silence_from TEXT NOT NULL DEFAULT '',
    silence_to   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

CREATE TABLE IF NOT EXISTS alert_event (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    rule_id      TEXT NOT NULL REFERENCES alert_rule(id) ON DELETE CASCADE,
    rule_name    TEXT NOT NULL DEFAULT '',
    pipeline     TEXT NOT NULL DEFAULT '',
    instance_id  TEXT NOT NULL DEFAULT '',
    severity     TEXT NOT NULL DEFAULT 'warning',
    title        TEXT NOT NULL DEFAULT '',
    detail       TEXT NOT NULL DEFAULT '',
    -- open / acknowledged / resolved
    status       TEXT NOT NULL DEFAULT 'open',
    -- 去重键：同管道同规则同窗口合并
    dedupe_key   TEXT NOT NULL DEFAULT '',
    notified     BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    handled_at   TIMESTAMPTZ,
    handled_by   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_alert_open ON alert_event(workspace_id, status, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS uq_alert_dedupe ON alert_event(workspace_id, dedupe_key) WHERE dedupe_key <> '';

-- ---------- 传感器与事件 ----------
CREATE TABLE IF NOT EXISTS sensor_state (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    pipeline     TEXT NOT NULL,
    node_id      TEXT NOT NULL,
    kind         TEXT NOT NULL,
    last_check   TIMESTAMPTZ,
    last_result  TEXT NOT NULL DEFAULT '',
    satisfied    BOOLEAN NOT NULL DEFAULT false,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, pipeline, node_id)
);

CREATE TABLE IF NOT EXISTS event_delivery (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    source       TEXT NOT NULL DEFAULT '',
    event_id     TEXT NOT NULL,
    pipeline     TEXT NOT NULL DEFAULT '',
    instance_id  TEXT NOT NULL DEFAULT '',
    payload      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, source, event_id)
);

-- ---------- 语义层：数据集与图表 ----------
CREATE TABLE IF NOT EXISTS semantic_dataset (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    spec         JSONB NOT NULL DEFAULT '{}'::jsonb,
    version      INT NOT NULL DEFAULT 1,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

CREATE TABLE IF NOT EXISTS chart (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    dataset      TEXT NOT NULL DEFAULT '',
    spec         JSONB NOT NULL DEFAULT '{}'::jsonb,
    version      INT NOT NULL DEFAULT 1,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

-- ---------- RBAC ----------
CREATE TABLE IF NOT EXISTS role_permission (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    role         TEXT NOT NULL,
    resource     TEXT NOT NULL,
    action       TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, role, resource, action)
);

CREATE TABLE IF NOT EXISTS resource_grant (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    subject      TEXT NOT NULL,
    resource     TEXT NOT NULL,
    resource_id  TEXT NOT NULL,
    action       TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, subject, resource, resource_id, action)
);

-- ---------- 生命周期与合规 ----------
CREATE TABLE IF NOT EXISTS retention_policy (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    model_name   TEXT NOT NULL,
    retain_days  INT NOT NULL DEFAULT 365,
    archive_to   TEXT NOT NULL DEFAULT '',
    enabled      BOOLEAN NOT NULL DEFAULT true,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, model_name)
);

CREATE TABLE IF NOT EXISTS erasure_job (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    model_name   TEXT NOT NULL,
    subject_key  TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','running','done','failed')),
    reason       TEXT NOT NULL DEFAULT '',
    rows_deleted BIGINT NOT NULL DEFAULT 0,
    error        TEXT NOT NULL DEFAULT '',
    requested_by TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ
);

-- ---------- 元数据：标签 / PII / 结构变更 ----------
CREATE TABLE IF NOT EXISTS tag (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    color        TEXT NOT NULL DEFAULT '',
    description  TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

CREATE TABLE IF NOT EXISTS entity_tag (
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    entity_type  TEXT NOT NULL CHECK (entity_type IN ('model','dataset','chart','pipeline')),
    entity_name  TEXT NOT NULL,
    tag          TEXT NOT NULL,
    PRIMARY KEY (workspace_id, entity_type, entity_name, tag)
);

ALTER TABLE dataset ADD COLUMN IF NOT EXISTS pii TEXT NOT NULL DEFAULT 'none';
ALTER TABLE dataset ADD COLUMN IF NOT EXISTS pii_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE dataset ADD COLUMN IF NOT EXISTS pii_override BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE dataset ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}';

CREATE TABLE IF NOT EXISTS schema_change (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    datasource   TEXT NOT NULL,
    schema_name  TEXT NOT NULL DEFAULT '',
    table_name   TEXT NOT NULL,
    kind         TEXT NOT NULL CHECK (kind IN ('add_column','drop_column','modify_column','add_table','drop_table')),
    column_name  TEXT NOT NULL DEFAULT '',
    detail       TEXT NOT NULL DEFAULT '',
    breaking     BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_schema_change ON schema_change(workspace_id, created_at DESC);

CREATE TABLE IF NOT EXISTS collect_job (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    datasource   TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'running' CHECK (status IN ('running','success','failed')),
    tables_seen  INT NOT NULL DEFAULT 0,
    changes      INT NOT NULL DEFAULT 0,
    error        TEXT NOT NULL DEFAULT '',
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ
);

-- ---------- 质量门禁放行 ----------
CREATE TABLE IF NOT EXISTS quality_waiver (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    instance_id  TEXT NOT NULL,
    node_id      TEXT NOT NULL DEFAULT '',
    reason       TEXT NOT NULL DEFAULT '',
    actor        TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------- 环境绑定 ----------
CREATE TABLE IF NOT EXISTS environment (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

CREATE TABLE IF NOT EXISTS env_binding (
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    env          TEXT NOT NULL,
    alias        TEXT NOT NULL,
    datasource_id TEXT NOT NULL,
    PRIMARY KEY (workspace_id, env, alias)
);

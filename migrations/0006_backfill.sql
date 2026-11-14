-- T0906 补数批次：记录「谁在什么时候补了哪段日期」，
-- 进度不落库，按 instance 反查，保证实例表是唯一事实源。
CREATE TABLE IF NOT EXISTS backfill_batch (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    pipeline     TEXT NOT NULL,
    date_from    TEXT NOT NULL,
    date_to      TEXT NOT NULL,
    total        INT  NOT NULL DEFAULT 0,
    created      INT  NOT NULL DEFAULT 0,
    skipped      INT  NOT NULL DEFAULT 0,
    reason       TEXT NOT NULL DEFAULT '',
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_backfill_ws ON backfill_batch(workspace_id, created_at DESC);

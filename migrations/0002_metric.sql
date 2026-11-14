-- +goose Up
-- 语义层指标：把「业务口径」固化成可复用对象，避免每个看板各写一遍 SQL。
CREATE TABLE IF NOT EXISTS metric (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    model_name   TEXT NOT NULL,
    agg          TEXT NOT NULL CHECK (agg IN ('sum','avg','count','count_distinct','min','max')),
    expr         TEXT NOT NULL,
    time_column  TEXT NOT NULL DEFAULT 'dt',
    dimensions   JSONB NOT NULL DEFAULT '[]'::jsonb,
    unit         TEXT NOT NULL DEFAULT '',
    description  TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

-- +goose Down
DROP TABLE IF EXISTS metric;

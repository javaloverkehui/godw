-- +goose Up
-- 看板：把指标组合成一页，卡片只存引用与展示参数，口径仍在 metric 上。
CREATE TABLE IF NOT EXISTS dashboard (
    id           TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspace(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    description  TEXT NOT NULL DEFAULT '',
    cards        JSONB NOT NULL DEFAULT '[]'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

-- +goose Down
DROP TABLE IF EXISTS dashboard;

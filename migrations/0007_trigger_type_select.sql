-- 选择器触发（/runs/select）会把 trigger_type 记为 select，
-- 原 CHECK 白名单漏了这一项，导致按模型 / tag / 分层批量触发时全部被库拒绝。
ALTER TABLE instance DROP CONSTRAINT IF EXISTS instance_trigger_type_check;
ALTER TABLE instance ADD CONSTRAINT instance_trigger_type_check
    CHECK (trigger_type IN ('manual', 'schedule', 'backfill', 'retry', 'select'));

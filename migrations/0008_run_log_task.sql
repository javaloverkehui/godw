-- 0008：运行日志补 task_id。
--
-- 背景：日志原先只有 node_id，节点重试/并行时无法和 task_instance 对上，
-- 前端也没法把「这条日志属于哪个任务」说清楚。task_id 直接落库后，
-- 日志可以按任务导出、和节点耗时表对齐，排查时不用再靠时间猜。

ALTER TABLE run_log ADD COLUMN IF NOT EXISTS task_id TEXT NOT NULL DEFAULT '';

-- 一个实例的日志读取永远是 (instance_id, seq) 的有序扫描；
-- 0005 只建了 (instance_id, id)，seq 回读在长日志下会多一次排序。
CREATE INDEX IF NOT EXISTS idx_run_log_instance_seq ON run_log(instance_id, seq);

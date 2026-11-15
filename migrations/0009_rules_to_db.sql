-- 0009：把「只存在于代码或管道 YAML」的两类规则搬进库。
--
-- 背景：产品要卖给多个数据团队，规范与质量规则必须按工作区配置、可复用、可审计。
-- 此前建模规范写死在 DefaultRules()，质量规则藏在下游管道的 nodes[].config.rules 里；
-- quality_rule 表虽然早就建了，但没有任何读写代码，属于空表。
--
-- +goose Up

-- 工作区级配置。建模规范放 settings.modelingRules，
-- 用 JSONB 而不是宽表：层数固定为 5，但每层字段还会长（后续可能要加负责人、审批要求）。
ALTER TABLE workspace ADD COLUMN IF NOT EXISTS settings JSONB NOT NULL DEFAULT '{}'::jsonb;

-- quality_rule 原先的 kind 白名单只有 6 种，而编译器实际支持 11 种，
-- freshness / regex / unique_combination 等规则一旦落库就会被 CHECK 拒掉。
ALTER TABLE quality_rule DROP CONSTRAINT IF EXISTS quality_rule_kind_check;
ALTER TABLE quality_rule ADD CONSTRAINT quality_rule_kind_check
    CHECK (kind IN ('not_null','unique','unique_combination','accepted_values','range','regex',
                    'referential','row_count','row_count_delta','custom_sql','freshness'));
ALTER TABLE quality_rule ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT '';
ALTER TABLE quality_rule ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT now();
-- 规则的唯一读取路径是「某工作区下某模型的全部规则」。
CREATE INDEX IF NOT EXISTS idx_quality_rule_model ON quality_rule(workspace_id, model_name);

-- semantic_dataset.version / chart.version 只是原地递增的计数器：既没有历史行，
-- 也没有回滚入口，接口却把它当「版本」返回。留着比删掉更危险——用户会以为改坏了能退回去。
-- 真正要版本时按 model_version / pipeline_version 的模式补快照表，而不是一个自增数字。
ALTER TABLE semantic_dataset DROP COLUMN IF EXISTS version;
ALTER TABLE chart DROP COLUMN IF EXISTS version;

-- +goose Down

ALTER TABLE chart ADD COLUMN IF NOT EXISTS version INT NOT NULL DEFAULT 1;
ALTER TABLE semantic_dataset ADD COLUMN IF NOT EXISTS version INT NOT NULL DEFAULT 1;
DROP INDEX IF EXISTS idx_quality_rule_model;
ALTER TABLE quality_rule DROP COLUMN IF EXISTS updated_at;
ALTER TABLE quality_rule DROP COLUMN IF EXISTS description;
ALTER TABLE quality_rule DROP CONSTRAINT IF EXISTS quality_rule_kind_check;
ALTER TABLE quality_rule ADD CONSTRAINT quality_rule_kind_check
    CHECK (kind IN ('not_null','unique','accepted_values','range','row_count','custom_sql'));
ALTER TABLE workspace DROP COLUMN IF EXISTS settings;

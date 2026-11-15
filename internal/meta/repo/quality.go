package repo

import (
	"context"
	"encoding/json"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/pkg/ids"
	"github.com/jackc/pgx/v5"
)

// 质量规则仓储。
//
// 规则挂在模型上：一个模型一套规则集，任何产出该模型的管道都复用同一套。
// 这样「改阈值」只动一条规则，不必重发整条管道；quality_result / 豁免
// 也能通过稳定的规则名对上历史结果。

// AssertModelExists 确认模型存在（任意版本）。规则指向不存在的模型就是死配置，
// 但这里不要求模型已发布——先定义模型、再配规则、最后发布是正常顺序。
func (r *Repo) AssertModelExists(ctx context.Context, workspaceID, name string) error {
	var exists bool
	if err := r.DB.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM model WHERE workspace_id=$1 AND name=$2)`,
		workspaceID, name).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errs.New(errs.CodeNotFound, "模型不存在: "+name)
	}
	return nil
}

const qualityRuleCols = `id,workspace_id,model_name,name,kind,column_name,config,severity,description,enabled,created_at`

// scanQualityRule 统一一处扫描，避免单模型/全工作区两条查询的列顺序漂移。
func scanQualityRule(rows pgx.Rows) (model.QualityRule, error) {
	var q model.QualityRule
	var cfg []byte
	var enabled bool
	if err := rows.Scan(&q.ID, &q.WorkspaceID, &q.ModelName, &q.Name, &q.Kind,
		&q.Column, &cfg, &q.Severity, &q.Description, &enabled, &q.CreatedAt); err != nil {
		return q, err
	}
	if len(cfg) > 0 {
		if err := json.Unmarshal(cfg, &q.Config); err != nil {
			return q, err
		}
	}
	q.Enabled = &enabled
	return q, nil
}

func (r *Repo) ListQualityRules(ctx context.Context, workspaceID, modelName string) ([]model.QualityRule, error) {
	rows, err := r.DB.Pool.Query(ctx, `
		SELECT `+qualityRuleCols+`
		  FROM quality_rule
		 WHERE workspace_id=$1 AND model_name=$2
		 ORDER BY name`, workspaceID, modelName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]model.QualityRule, 0, 8)
	for rows.Next() {
		q, err := scanQualityRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// ListQualityRulesByWorkspace 一次取全工作区的规则，按模型分组。
// 文档站要按模型展示规则，逐个模型查会变成 N+1。
func (r *Repo) ListQualityRulesByWorkspace(ctx context.Context, workspaceID string) (map[string][]model.QualityRule, error) {
	rows, err := r.DB.Pool.Query(ctx, `
		SELECT `+qualityRuleCols+`
		  FROM quality_rule
		 WHERE workspace_id=$1
		 ORDER BY model_name, name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]model.QualityRule{}
	for rows.Next() {
		q, err := scanQualityRule(rows)
		if err != nil {
			return nil, err
		}
		out[q.ModelName] = append(out[q.ModelName], q)
	}
	return out, rows.Err()
}

// ReplaceQualityRules 以 name 为稳定标识做增量同步：出现的更新/插入，未出现的删除。
//
// 不做「先全删再全插」：规则 id 是结果与豁免的引用锚点，每次保存都换 id
// 等于让历史记录失去指向，也看不出某条规则是「改过」还是「删了重建」。
func (r *Repo) ReplaceQualityRules(ctx context.Context, workspaceID, modelName string, rules []model.QualityRule) error {
	return r.DB.WithTx(ctx, func(tx pgx.Tx) error {
		keep := make([]string, 0, len(rules))
		for _, q := range rules {
			keep = append(keep, q.Name)
			cfg, err := json.Marshal(q.Config)
			if err != nil {
				return err
			}
			if len(q.Config) == 0 {
				cfg = []byte("{}")
			}
			enabled := q.Enabled == nil || *q.Enabled
			if _, err := tx.Exec(ctx, `
				INSERT INTO quality_rule(id,workspace_id,model_name,name,kind,column_name,config,severity,description,enabled)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
				ON CONFLICT (workspace_id,model_name,name) DO UPDATE SET
					kind        = EXCLUDED.kind,
					column_name = EXCLUDED.column_name,
					config      = EXCLUDED.config,
					severity    = EXCLUDED.severity,
					description = EXCLUDED.description,
					enabled     = EXCLUDED.enabled,
					updated_at  = now()`,
				ids.New("qr"), workspaceID, modelName, q.Name, q.Kind, q.Column,
				cfg, q.Severity, q.Description, enabled); err != nil {
				return err
			}
		}
		// name <> ALL('{}') 为真，因此传空集合时会清空该模型的全部规则。
		_, err := tx.Exec(ctx,
			`DELETE FROM quality_rule WHERE workspace_id=$1 AND model_name=$2 AND name <> ALL($3)`,
			workspaceID, modelName, keep)
		return err
	})
}

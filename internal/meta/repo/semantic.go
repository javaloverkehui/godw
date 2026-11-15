package repo

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/pkg/ids"
	"github.com/godw/godw/internal/semantic"
	"github.com/jackc/pgx/v5"
)

// 语义层仓储：数据集与图表。只保留最新定义，不存历史。
//
// 这里曾经有一个 version 计数列：每次保存 +1，但没有历史行、没有回滚入口，
// 接口却把它当版本号返回，容易让人以为改坏了能退回去。0009 已删除该列。
// 真正需要版本时按 model_version 的模式补快照表，而不是一个自增数字。

func (r *Repo) SaveDataset(ctx context.Context, workspaceID string, d semantic.Dataset) (*semantic.Dataset, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var existing string
	err = r.DB.Pool.QueryRow(ctx,
		`SELECT id FROM semantic_dataset WHERE workspace_id=$1 AND name=$2`,
		workspaceID, d.Name).Scan(&existing)
	if errors.Is(err, pgx.ErrNoRows) {
		d.ID = ids.New("dsx")
		if _, err := r.DB.Pool.Exec(ctx,
			`INSERT INTO semantic_dataset(id,workspace_id,name,description,spec)
			 VALUES($1,$2,$3,$4,$5)`, d.ID, workspaceID, d.Name, d.Description, raw); err != nil {
			return nil, err
		}
		return &d, nil
	}
	if err != nil {
		return nil, err
	}
	d.ID = existing
	if _, err := r.DB.Pool.Exec(ctx,
		`UPDATE semantic_dataset SET description=$3,spec=$4,updated_at=now()
		 WHERE workspace_id=$1 AND name=$2`, workspaceID, d.Name, d.Description, raw); err != nil {
		return nil, err
	}
	return &d, nil
}

func (r *Repo) GetDataset(ctx context.Context, workspaceID, name string) (*semantic.Dataset, error) {
	var raw []byte
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT spec FROM semantic_dataset WHERE workspace_id=$1 AND name=$2`, workspaceID, name).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.New(errs.CodeNotFound, "数据集不存在: "+name)
	}
	if err != nil {
		return nil, err
	}
	var d semantic.Dataset
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (r *Repo) ListDatasetsScoped(ctx context.Context, workspaceID string) ([]semantic.Dataset, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT spec FROM semantic_dataset WHERE workspace_id=$1 ORDER BY name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []semantic.Dataset{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var d semantic.Dataset
		if err := json.Unmarshal(raw, &d); err != nil {
			continue
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *Repo) DeleteDataset(ctx context.Context, workspaceID, name string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`DELETE FROM semantic_dataset WHERE workspace_id=$1 AND name=$2`, workspaceID, name)
	return err
}

func (r *Repo) SaveChart(ctx context.Context, workspaceID string, c semantic.Chart) (*semantic.Chart, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	var existing string
	err = r.DB.Pool.QueryRow(ctx,
		`SELECT id FROM chart WHERE workspace_id=$1 AND name=$2`, workspaceID, c.Name).
		Scan(&existing)
	if errors.Is(err, pgx.ErrNoRows) {
		c.ID = ids.New("cht")
		if _, err := r.DB.Pool.Exec(ctx,
			`INSERT INTO chart(id,workspace_id,name,dataset,spec) VALUES($1,$2,$3,$4,$5)`,
			c.ID, workspaceID, c.Name, c.Dataset, raw); err != nil {
			return nil, err
		}
		return &c, nil
	}
	if err != nil {
		return nil, err
	}
	c.ID = existing
	if _, err := r.DB.Pool.Exec(ctx,
		`UPDATE chart SET dataset=$3,spec=$4,updated_at=now()
		 WHERE workspace_id=$1 AND name=$2`, workspaceID, c.Name, c.Dataset, raw); err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *Repo) GetChart(ctx context.Context, workspaceID, name string) (*semantic.Chart, error) {
	var raw []byte
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT spec FROM chart WHERE workspace_id=$1 AND name=$2`, workspaceID, name).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.New(errs.CodeNotFound, "图表不存在: "+name)
	}
	if err != nil {
		return nil, err
	}
	var c semantic.Chart
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *Repo) ListCharts(ctx context.Context, workspaceID string) ([]semantic.Chart, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT spec FROM chart WHERE workspace_id=$1 ORDER BY name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []semantic.Chart{}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var c semantic.Chart
		if err := json.Unmarshal(raw, &c); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Repo) DeleteChart(ctx context.Context, workspaceID, name string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`DELETE FROM chart WHERE workspace_id=$1 AND name=$2`, workspaceID, name)
	return err
}

// CountChartsUsingDataset 返回引用某数据集的图表数，用于删除前的影响检查。
func (r *Repo) CountChartsUsingDataset(ctx context.Context, workspaceID, dataset string) (int, error) {
	var n int
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT count(*) FROM chart WHERE workspace_id=$1 AND dataset=$2`, workspaceID, dataset).Scan(&n)
	return n, err
}

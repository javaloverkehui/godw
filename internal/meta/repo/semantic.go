package repo

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/pkg/ids"
	"github.com/godw/godw/internal/semantic"
	"github.com/jackc/pgx/v5"
)

// 语义层仓储：数据集与图表。两者都是「定义 + 版本」，但当前只保留最新版本，
// 因为数据集/图表的变更是弱破坏性的（不像物理表 DDL），保留全量历史价值不高。

func (r *Repo) SaveDataset(ctx context.Context, workspaceID string, d semantic.Dataset) (*semantic.Dataset, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var existing string
	var version int
	err = r.DB.Pool.QueryRow(ctx,
		`SELECT id,version FROM semantic_dataset WHERE workspace_id=$1 AND name=$2`,
		workspaceID, d.Name).Scan(&existing, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		d.ID = ids.New("dsx")
		d.Version = 1
		if _, err := r.DB.Pool.Exec(ctx,
			`INSERT INTO semantic_dataset(id,workspace_id,name,description,spec,version)
			 VALUES($1,$2,$3,$4,$5,1)`, d.ID, workspaceID, d.Name, d.Description, raw); err != nil {
			return nil, err
		}
		return &d, nil
	}
	if err != nil {
		return nil, err
	}
	d.ID = existing
	d.Version = version + 1
	if _, err := r.DB.Pool.Exec(ctx,
		`UPDATE semantic_dataset SET description=$3,spec=$4,version=$5,updated_at=now()
		 WHERE workspace_id=$1 AND name=$2`, workspaceID, d.Name, d.Description, raw, d.Version); err != nil {
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
		`SELECT spec,version,updated_at FROM semantic_dataset WHERE workspace_id=$1 ORDER BY name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []semantic.Dataset{}
	for rows.Next() {
		var raw []byte
		var version int
		var at time.Time
		if err := rows.Scan(&raw, &version, &at); err != nil {
			return nil, err
		}
		var d semantic.Dataset
		if err := json.Unmarshal(raw, &d); err != nil {
			continue
		}
		d.Version = version
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
	var version int
	err = r.DB.Pool.QueryRow(ctx,
		`SELECT id,version FROM chart WHERE workspace_id=$1 AND name=$2`, workspaceID, c.Name).
		Scan(&existing, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		c.ID = ids.New("cht")
		c.Version = 1
		if _, err := r.DB.Pool.Exec(ctx,
			`INSERT INTO chart(id,workspace_id,name,dataset,spec,version) VALUES($1,$2,$3,$4,$5,1)`,
			c.ID, workspaceID, c.Name, c.Dataset, raw); err != nil {
			return nil, err
		}
		return &c, nil
	}
	if err != nil {
		return nil, err
	}
	c.ID = existing
	c.Version = version + 1
	if _, err := r.DB.Pool.Exec(ctx,
		`UPDATE chart SET dataset=$3,spec=$4,version=$5,updated_at=now()
		 WHERE workspace_id=$1 AND name=$2`, workspaceID, c.Name, c.Dataset, raw, c.Version); err != nil {
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
		`SELECT spec,version FROM chart WHERE workspace_id=$1 ORDER BY name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []semantic.Chart{}
	for rows.Next() {
		var raw []byte
		var version int
		if err := rows.Scan(&raw, &version); err != nil {
			return nil, err
		}
		var c semantic.Chart
		if err := json.Unmarshal(raw, &c); err != nil {
			continue
		}
		c.Version = version
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

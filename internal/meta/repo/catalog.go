package repo

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
	"github.com/godw/godw/internal/pkg/ids"
	"github.com/godw/godw/internal/semantic"
	"github.com/jackc/pgx/v5"
)

// Repo 是元数据仓储。所有 SQL 集中在此包，service 层不写 SQL。
type Repo struct {
	DB *DB
}

func New(db *DB) *Repo { return &Repo{DB: db} }

func nowPtr(t time.Time) *time.Time { return &t }

// ---------- workspace / user ----------

func (r *Repo) EnsureWorkspace(ctx context.Context, name string) (*model.Workspace, error) {
	var w model.Workspace
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT id, name, description, created_at FROM workspace WHERE name=$1`, name).
		Scan(&w.ID, &w.Name, &w.Description, &w.CreatedAt)
	if err == nil {
		return &w, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	w = model.Workspace{ID: ids.New("ws"), Name: name, CreatedAt: time.Now()}
	_, err = r.DB.Pool.Exec(ctx,
		`INSERT INTO workspace(id,name,description) VALUES($1,$2,'') ON CONFLICT (name) DO NOTHING`,
		w.ID, w.Name)
	if err != nil {
		return nil, err
	}
	if err := r.DB.Pool.QueryRow(ctx, `SELECT id, created_at FROM workspace WHERE name=$1`, name).
		Scan(&w.ID, &w.CreatedAt); err != nil {
		return nil, err
	}
	return &w, nil
}

func (r *Repo) CreateUser(ctx context.Context, u model.User) error {
	_, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO app_user(id,workspace_id,email,display_name,password_hash,role)
		 VALUES($1,$2,$3,$4,$5,$6)`,
		u.ID, u.WorkspaceID, u.Email, u.DisplayName, u.PasswordHash, u.Role)
	return err
}

func (r *Repo) UserByEmail(ctx context.Context, workspaceID, email string) (*model.User, error) {
	var u model.User
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT id,workspace_id,email,display_name,password_hash,role,created_at
		 FROM app_user WHERE workspace_id=$1 AND email=$2`, workspaceID, email).
		Scan(&u.ID, &u.WorkspaceID, &u.Email, &u.DisplayName, &u.PasswordHash, &u.Role, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.New(errs.CodeUnauthorized, "邮箱或密码错误")
	}
	return &u, err
}

// ---------- datasource ----------

func (r *Repo) CreateDatasource(ctx context.Context, ds model.Datasource, secretEnc []byte) (*model.Datasource, error) {
	ds.ID = ids.New("ds")
	ds.CreatedAt = time.Now()
	ds.UpdatedAt = ds.CreatedAt
	cfg, err := json.Marshal(ds.Config)
	if err != nil {
		return nil, err
	}
	_, err = r.DB.Pool.Exec(ctx,
		`INSERT INTO datasource(id,workspace_id,name,type,config,secret_enc)
		 VALUES($1,$2,$3,$4,$5,$6)`,
		ds.ID, ds.WorkspaceID, ds.Name, ds.Type, cfg, secretEnc)
	if err != nil {
		if isUnique(err) {
			return nil, errs.New(errs.CodeDatasourceDuplicate, "数据源名称已存在: "+ds.Name)
		}
		return nil, err
	}
	return &ds, nil
}

func (r *Repo) ListDatasources(ctx context.Context, workspaceID string) ([]model.Datasource, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT id,workspace_id,name,type,config,status,last_error,checked_at,created_at,updated_at
		 FROM datasource WHERE workspace_id=$1 ORDER BY name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Datasource{}
	for rows.Next() {
		var d model.Datasource
		var cfg []byte
		if err := rows.Scan(&d.ID, &d.WorkspaceID, &d.Name, &d.Type, &cfg, &d.Status,
			&d.LastError, &d.CheckedAt, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(cfg, &d.Config)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *Repo) DatasourceByName(ctx context.Context, workspaceID, name string) (*model.Datasource, []byte, error) {
	var d model.Datasource
	var cfg, sec []byte
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT id,workspace_id,name,type,config,secret_enc,status,last_error,checked_at,created_at,updated_at
		 FROM datasource WHERE workspace_id=$1 AND name=$2`, workspaceID, name).
		Scan(&d.ID, &d.WorkspaceID, &d.Name, &d.Type, &cfg, &sec, &d.Status,
			&d.LastError, &d.CheckedAt, &d.CreatedAt, &d.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, errs.New(errs.CodeNotFound, "数据源不存在: "+name)
	}
	if err != nil {
		return nil, nil, err
	}
	_ = json.Unmarshal(cfg, &d.Config)
	return &d, sec, nil
}

func (r *Repo) UpdateDatasourceStatus(ctx context.Context, id, status, lastErr string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`UPDATE datasource SET status=$2,last_error=$3,checked_at=now(),updated_at=now() WHERE id=$1`,
		id, status, lastErr)
	return err
}

func (r *Repo) DeleteDatasource(ctx context.Context, workspaceID, name string) error {
	tag, err := r.DB.Pool.Exec(ctx, `DELETE FROM datasource WHERE workspace_id=$1 AND name=$2`, workspaceID, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.CodeNotFound, "数据源不存在: "+name)
	}
	return nil
}

// ---------- dataset ----------

func (r *Repo) UpsertDataset(ctx context.Context, ds model.Dataset, workspaceID string) error {
	cols, err := json.Marshal(ds.Columns)
	if err != nil {
		return err
	}
	_, err = r.DB.Pool.Exec(ctx,
		`INSERT INTO dataset(id,workspace_id,datasource_id,schema_name,table_name,table_type,comment,row_count,size_bytes,columns,collected_at)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,now())
		 ON CONFLICT (datasource_id,schema_name,table_name) DO UPDATE SET
		   table_type=EXCLUDED.table_type, comment=EXCLUDED.comment, row_count=EXCLUDED.row_count,
		   size_bytes=EXCLUDED.size_bytes, columns=EXCLUDED.columns, collected_at=now()`,
		ids.New("dsat"), workspaceID, ds.DatasourceID, ds.Schema, ds.Name, ds.Type,
		ds.Comment, ds.RowCount, ds.SizeBytes, cols)
	return err
}

func (r *Repo) ListDatasets(ctx context.Context, workspaceID, datasourceID string) ([]model.Dataset, error) {
	q := `SELECT id,datasource_id,schema_name,table_name,table_type,comment,row_count,size_bytes,columns,collected_at,
	             COALESCE(pii,'none'), COALESCE(pii_reason,''), COALESCE(tags,'{}')
	      FROM dataset WHERE workspace_id=$1`
	args := []any{workspaceID}
	if datasourceID != "" {
		q += ` AND datasource_id=$2`
		args = append(args, datasourceID)
	}
	q += ` ORDER BY schema_name,table_name`
	rows, err := r.DB.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Dataset{}
	for rows.Next() {
		var d model.Dataset
		var cols []byte
		if err := rows.Scan(&d.ID, &d.DatasourceID, &d.Schema, &d.Name, &d.Type, &d.Comment,
			&d.RowCount, &d.SizeBytes, &cols, &d.CollectedAt, &d.PII, &d.PIIReason, &d.Tags); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(cols, &d.Columns)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------- model ----------

// SaveModel 保存模型定义并生成新版本。publish=true 时立即切换 active_version。
// 已发布的版本不可修改：改模型只会新增版本，历史执行的快照因此保持稳定。
func (r *Repo) SaveModel(ctx context.Context, workspaceID string, spec model.ModelSpec, datasourceID, ddl, actor string, publish bool) (*model.Model, error) {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}

	var m model.Model
	err = r.DB.WithTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx,
			`SELECT id,workspace_id,name,layer,description,COALESCE(datasource_id,''),active_version,created_at,updated_at
			 FROM model WHERE workspace_id=$1 AND name=$2`, workspaceID, spec.Name).
			Scan(&m.ID, &m.WorkspaceID, &m.Name, &m.Layer, &m.Description, &m.DatasourceID, &m.ActiveVersion, &m.CreatedAt, &m.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			m = model.Model{
				ID: ids.New("mdl"), WorkspaceID: workspaceID, Name: spec.Name,
				Layer: spec.Layer, Description: spec.Description, DatasourceID: datasourceID,
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO model(id,workspace_id,name,layer,description,datasource_id,active_version)
				 VALUES($1,$2,$3,$4,$5,NULLIF($6,''),0)`,
				m.ID, workspaceID, m.Name, m.Layer, m.Description, datasourceID); err != nil {
				return err
			}
			m.ActiveVersion = 0
		} else if err != nil {
			return err
		} else {
			// 更新描述与分层，但不覆盖 active_version
			if _, err := tx.Exec(ctx,
				`UPDATE model SET description=$2, layer=$3, datasource_id=NULLIF($4,''), updated_at=now() WHERE id=$1`,
				m.ID, spec.Description, spec.Layer, datasourceID); err != nil {
				return err
			}
		}

		var next int
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(version),0)+1 FROM model_version WHERE model_id=$1`, m.ID).Scan(&next); err != nil {
			return err
		}

		status := "draft"
		var publishedAt *time.Time
		if publish {
			status = "published"
			publishedAt = nowPtr(time.Now())
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO model_version(id,model_id,version,spec,ddl,status,published_by,published_at)
			 VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			ids.New("mv"), m.ID, next, specJSON, ddl, status, actor, publishedAt); err != nil {
			return err
		}

		if publish {
			if _, err := tx.Exec(ctx,
				`UPDATE model_version SET status='archived' WHERE model_id=$1 AND version<>$2 AND status='published'`,
				m.ID, next); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE model SET active_version=$2, updated_at=now() WHERE id=$1`, m.ID, next); err != nil {
				return err
			}
			m.ActiveVersion = next
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	m.Spec = &spec
	m.DDL = ddl
	return &m, nil
}

func (r *Repo) ListModels(ctx context.Context, workspaceID string) ([]model.Model, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT m.id,m.workspace_id,m.name,m.layer,m.description,COALESCE(m.datasource_id,''),
		        m.active_version,m.created_at,m.updated_at,
		        COALESCE(v.spec,'{}'::jsonb), COALESCE(v.ddl,'')
		 FROM model m
		 LEFT JOIN model_version v ON v.model_id=m.id AND v.version=m.active_version
		 WHERE m.workspace_id=$1
		 ORDER BY CASE m.layer WHEN 'ods' THEN 0 WHEN 'dim' THEN 1 WHEN 'dwd' THEN 2
		                       WHEN 'dws' THEN 3 WHEN 'ads' THEN 4 ELSE 5 END, m.name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Model{}
	for rows.Next() {
		var m model.Model
		var specJSON []byte
		if err := rows.Scan(&m.ID, &m.WorkspaceID, &m.Name, &m.Layer, &m.Description,
			&m.DatasourceID, &m.ActiveVersion, &m.CreatedAt, &m.UpdatedAt, &specJSON, &m.DDL); err != nil {
			return nil, err
		}
		if m.ActiveVersion > 0 {
			var spec model.ModelSpec
			if json.Unmarshal(specJSON, &spec) == nil {
				m.Spec = &spec
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// PublishedModelSpec 返回当前生效版本的模型定义，执行器只认这个口径。
func (r *Repo) PublishedModelSpec(ctx context.Context, workspaceID, name string) (*model.ModelSpec, error) {
	var specJSON []byte
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT v.spec FROM model m
		 JOIN model_version v ON v.model_id=m.id AND v.version=m.active_version
		 WHERE m.workspace_id=$1 AND m.name=$2 AND m.active_version>0`, workspaceID, name).Scan(&specJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.New(errs.CodeNotFound, "模型未发布或不存在: "+name)
	}
	if err != nil {
		return nil, err
	}
	var spec model.ModelSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

func (r *Repo) ModelVersions(ctx context.Context, workspaceID, name string) ([]model.ModelVersion, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT v.id,v.model_id,v.version,v.spec,v.ddl,v.status,COALESCE(v.published_by,''),v.published_at,v.created_at
		 FROM model_version v JOIN model m ON m.id=v.model_id
		 WHERE m.workspace_id=$1 AND m.name=$2 ORDER BY v.version DESC`, workspaceID, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ModelVersion{}
	for rows.Next() {
		var v model.ModelVersion
		var specJSON []byte
		if err := rows.Scan(&v.ID, &v.ModelID, &v.Version, &specJSON, &v.DDL, &v.Status,
			&v.PublishedBy, &v.PublishedAt, &v.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(specJSON, &v.Spec)
		out = append(out, v)
	}
	return out, rows.Err()
}

// RollbackModel 把 active_version 指回历史版本，不修改任何 DDL。
func (r *Repo) RollbackModel(ctx context.Context, workspaceID, name string, version int) error {
	return r.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var modelID string
		if err := tx.QueryRow(ctx, `SELECT id FROM model WHERE workspace_id=$1 AND name=$2`, workspaceID, name).Scan(&modelID); err != nil {
			return errs.New(errs.CodeNotFound, "模型不存在: "+name)
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_version WHERE model_id=$1 AND version=$2)`, modelID, version).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errs.New(errs.CodeNotFound, "版本不存在")
		}
		if _, err := tx.Exec(ctx, `UPDATE model_version SET status='archived' WHERE model_id=$1 AND status='published'`, modelID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE model_version SET status='published' WHERE model_id=$1 AND version=$2`, modelID, version); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE model SET active_version=$2, updated_at=now() WHERE id=$1`, modelID, version)
		return err
	})
}

// ---------- pipeline ----------

func (r *Repo) SavePipeline(ctx context.Context, workspaceID string, spec model.PipelineSpec, actor string, publish bool) (*model.Pipeline, error) {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}
	p := model.Pipeline{
		ID: ids.New("pl"), WorkspaceID: workspaceID, Name: spec.Name,
		Description: spec.Description, Schedule: spec.Schedule, Timezone: spec.Timezone,
		Concurrency: spec.Concurrency, Enabled: true,
		ResourceGroup: spec.ResourceGroup, Priority: spec.Priority,
	}
	if p.Timezone == "" {
		p.Timezone = "Asia/Shanghai"
	}
	if p.Concurrency <= 0 {
		p.Concurrency = 1
	}
	if p.ResourceGroup == "" {
		p.ResourceGroup = "default"
	}
	if p.Priority <= 0 {
		p.Priority = 5
	}

	err = r.DB.WithTx(ctx, func(tx pgx.Tx) error {
		var existing string
		err := tx.QueryRow(ctx, `SELECT id FROM pipeline WHERE workspace_id=$1 AND name=$2`, workspaceID, spec.Name).Scan(&existing)
		if errors.Is(err, pgx.ErrNoRows) {
			if _, err := tx.Exec(ctx,
				`INSERT INTO pipeline(id,workspace_id,name,description,schedule,timezone,concurrency,enabled,active_version,resource_group,priority)
				 VALUES($1,$2,$3,$4,$5,$6,$7,true,0,$8,$9)`,
				p.ID, workspaceID, p.Name, p.Description, p.Schedule, p.Timezone, p.Concurrency, p.ResourceGroup, p.Priority); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			p.ID = existing
			if _, err := tx.Exec(ctx,
				`UPDATE pipeline SET description=$2,schedule=$3,timezone=$4,concurrency=$5,
				        resource_group=$6,priority=$7,updated_at=now() WHERE id=$1`,
				p.ID, p.Description, p.Schedule, p.Timezone, p.Concurrency, p.ResourceGroup, p.Priority); err != nil {
				return err
			}
		}

		var next int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM pipeline_version WHERE pipeline_id=$1`, p.ID).Scan(&next); err != nil {
			return err
		}
		status := "draft"
		if publish {
			status = "published"
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO pipeline_version(id,pipeline_id,version,spec,status) VALUES($1,$2,$3,$4,$5)`,
			ids.New("pv"), p.ID, next, specJSON, status); err != nil {
			return err
		}
		if publish {
			if _, err := tx.Exec(ctx,
				`UPDATE pipeline_version SET status='archived' WHERE pipeline_id=$1 AND version<>$2 AND status='published'`,
				p.ID, next); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE pipeline SET active_version=$2,updated_at=now() WHERE id=$1`, p.ID, next); err != nil {
				return err
			}
			p.ActiveVersion = next
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	p.Spec = &spec
	return &p, nil
}

func (r *Repo) ListPipelines(ctx context.Context, workspaceID string) ([]model.Pipeline, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT p.id,p.workspace_id,p.name,p.description,p.schedule,p.timezone,p.concurrency,p.enabled,
		        p.active_version,p.created_at,p.updated_at, COALESCE(v.spec,'{}'::jsonb)
		 FROM pipeline p
		 LEFT JOIN pipeline_version v ON v.pipeline_id=p.id AND v.version=p.active_version
		 WHERE p.workspace_id=$1 ORDER BY p.name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Pipeline{}
	for rows.Next() {
		var p model.Pipeline
		var specJSON []byte
		if err := rows.Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Description, &p.Schedule, &p.Timezone,
			&p.Concurrency, &p.Enabled, &p.ActiveVersion, &p.CreatedAt, &p.UpdatedAt, &specJSON); err != nil {
			return nil, err
		}
		if p.ActiveVersion > 0 {
			var spec model.PipelineSpec
			if json.Unmarshal(specJSON, &spec) == nil {
				p.Spec = &spec
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repo) PipelineByName(ctx context.Context, workspaceID, name string) (*model.Pipeline, *model.PipelineSpec, int, error) {
	var p model.Pipeline
	var specJSON []byte
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT p.id,p.workspace_id,p.name,p.description,p.schedule,p.timezone,p.concurrency,p.enabled,
		        p.active_version,p.created_at,p.updated_at, COALESCE(v.spec,'{}'::jsonb)
		 FROM pipeline p
		 LEFT JOIN pipeline_version v ON v.pipeline_id=p.id AND v.version=p.active_version
		 WHERE p.workspace_id=$1 AND p.name=$2`, workspaceID, name).
		Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Description, &p.Schedule, &p.Timezone,
			&p.Concurrency, &p.Enabled, &p.ActiveVersion, &p.CreatedAt, &p.UpdatedAt, &specJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, 0, errs.New(errs.CodeNotFound, "管道不存在: "+name)
	}
	if err != nil {
		return nil, nil, 0, err
	}
	var spec model.PipelineSpec
	if p.ActiveVersion > 0 {
		if err := json.Unmarshal(specJSON, &spec); err != nil {
			return nil, nil, 0, err
		}
		p.Spec = &spec
	}
	return &p, &spec, p.ActiveVersion, nil
}

// PipelineVersionSpec 取指定版本的快照；重跑历史实例必须用它，不能用当前版本。
func (r *Repo) PipelineVersionSpec(ctx context.Context, pipelineID string, version int) (*model.PipelineSpec, error) {
	var specJSON []byte
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT spec FROM pipeline_version WHERE pipeline_id=$1 AND version=$2`, pipelineID, version).Scan(&specJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.New(errs.CodeNotFound, "管道版本不存在")
	}
	if err != nil {
		return nil, err
	}
	var spec model.PipelineSpec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

func (r *Repo) SetPipelineEnabled(ctx context.Context, workspaceID, name string, enabled bool) error {
	tag, err := r.DB.Pool.Exec(ctx,
		`UPDATE pipeline SET enabled=$3,updated_at=now() WHERE workspace_id=$1 AND name=$2`,
		workspaceID, name, enabled)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.CodeNotFound, "管道不存在: "+name)
	}
	return nil
}

func (r *Repo) ListEnabledPipelines(ctx context.Context) ([]model.Pipeline, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT id,workspace_id,name,schedule,timezone,concurrency,active_version,
		        COALESCE(resource_group,'default'),COALESCE(priority,5) FROM pipeline
		 WHERE enabled=true AND schedule<>'' AND active_version>0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Pipeline{}
	for rows.Next() {
		var p model.Pipeline
		if err := rows.Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Schedule, &p.Timezone,
			&p.Concurrency, &p.ActiveVersion, &p.ResourceGroup, &p.Priority); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func isUnique(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "duplicate key") || strings.Contains(msg, "unique constraint")
}

// ---------- metric ----------

func (r *Repo) SaveMetric(ctx context.Context, wsID string, m semantic.Metric) (*semantic.Metric, error) {
	dims, err := json.Marshal(m.Dimensions)
	if err != nil {
		return nil, err
	}
	m.ID = ids.New("mtc")
	m.WorkspaceID = wsID
	_, err = r.DB.Pool.Exec(ctx,
		`INSERT INTO metric(id,workspace_id,name,display_name,model_name,agg,expr,time_column,dimensions,unit,description)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 ON CONFLICT (workspace_id,name) DO UPDATE SET
		   display_name=EXCLUDED.display_name, model_name=EXCLUDED.model_name, agg=EXCLUDED.agg,
		   expr=EXCLUDED.expr, time_column=EXCLUDED.time_column, dimensions=EXCLUDED.dimensions,
		   unit=EXCLUDED.unit, description=EXCLUDED.description, updated_at=now()`,
		m.ID, wsID, m.Name, m.DisplayName, m.ModelName, m.Agg, m.Expr, m.TimeColumn, dims, m.Unit, m.Description)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *Repo) ListMetrics(ctx context.Context, wsID string) ([]semantic.Metric, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT id,workspace_id,name,display_name,model_name,agg,expr,time_column,dimensions,unit,description
		 FROM metric WHERE workspace_id=$1 ORDER BY name`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []semantic.Metric{}
	for rows.Next() {
		var m semantic.Metric
		var dims []byte
		if err := rows.Scan(&m.ID, &m.WorkspaceID, &m.Name, &m.DisplayName, &m.ModelName, &m.Agg,
			&m.Expr, &m.TimeColumn, &dims, &m.Unit, &m.Description); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(dims, &m.Dimensions)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *Repo) MetricByName(ctx context.Context, wsID, name string) (*semantic.Metric, error) {
	var m semantic.Metric
	var dims []byte
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT id,workspace_id,name,display_name,model_name,agg,expr,time_column,dimensions,unit,description
		 FROM metric WHERE workspace_id=$1 AND name=$2`, wsID, name).
		Scan(&m.ID, &m.WorkspaceID, &m.Name, &m.DisplayName, &m.ModelName, &m.Agg,
			&m.Expr, &m.TimeColumn, &dims, &m.Unit, &m.Description)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.New(errs.CodeNotFound, "指标不存在: "+name)
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(dims, &m.Dimensions)
	return &m, nil
}

// ---------- dashboard ----------

func (r *Repo) SaveDashboard(ctx context.Context, wsID string, d semantic.Dashboard) (*semantic.Dashboard, error) {
	cards, err := json.Marshal(d.Cards)
	if err != nil {
		return nil, err
	}
	d.ID = ids.New("dsh")
	d.WorkspaceID = wsID
	_, err = r.DB.Pool.Exec(ctx,
		`INSERT INTO dashboard(id,workspace_id,name,description,cards)
		 VALUES($1,$2,$3,$4,$5)
		 ON CONFLICT (workspace_id,name) DO UPDATE SET
		   description=EXCLUDED.description, cards=EXCLUDED.cards, updated_at=now()`,
		d.ID, wsID, d.Name, d.Description, cards)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (r *Repo) ListDashboards(ctx context.Context, wsID string) ([]semantic.Dashboard, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT id,workspace_id,name,description,cards FROM dashboard WHERE workspace_id=$1 ORDER BY name`, wsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []semantic.Dashboard{}
	for rows.Next() {
		var d semantic.Dashboard
		var cards []byte
		if err := rows.Scan(&d.ID, &d.WorkspaceID, &d.Name, &d.Description, &cards); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(cards, &d.Cards)
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *Repo) DashboardByName(ctx context.Context, wsID, name string) (*semantic.Dashboard, error) {
	var d semantic.Dashboard
	var cards []byte
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT id,workspace_id,name,description,cards FROM dashboard WHERE workspace_id=$1 AND name=$2`, wsID, name).
		Scan(&d.ID, &d.WorkspaceID, &d.Name, &d.Description, &cards)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.New(errs.CodeNotFound, "看板不存在: "+name)
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal(cards, &d.Cards)
	return &d, nil
}

// DatasetColumnsByName 按表名（忽略 schema）查询最近采集到的列定义，
// 供建模脚手架自动填充字段。同名表取最近采集的一条。
func (r *Repo) DatasetColumnsByName(ctx context.Context, workspaceID, table string) ([]model.ColumnSpec, error) {
	var cols []byte
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT columns FROM dataset
		 WHERE workspace_id=$1 AND table_name=$2
		 ORDER BY collected_at DESC LIMIT 1`, workspaceID, table).Scan(&cols)
	if err != nil {
		return nil, err
	}
	var specs []model.ColumnSpec
	if err := json.Unmarshal(cols, &specs); err != nil {
		return nil, err
	}
	return specs, nil
}

// LineageEdge 是表级血缘的一条边。
type LineageEdge struct {
	Upstream   string `json:"upstream"`
	Downstream string `json:"downstream"`
	NodeID     string `json:"nodeId,omitempty"`
}

// ListLineage 返回工作空间内的全部表级血缘边，用于整图渲染与字典生成。
func (r *Repo) ListLineage(ctx context.Context, workspaceID string) ([]LineageEdge, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT upstream, downstream, node_id FROM lineage_edge
		 WHERE workspace_id=$1 ORDER BY upstream, downstream`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LineageEdge{}
	for rows.Next() {
		var e LineageEdge
		if err := rows.Scan(&e.Upstream, &e.Downstream, &e.NodeID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- 环境与绑定（T0203） ----------

func (r *Repo) EnsureEnvironment(ctx context.Context, workspaceID, name, desc string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO environment(id,workspace_id,name,description) VALUES($1,$2,$3,$4)
		 ON CONFLICT (workspace_id,name) DO NOTHING`,
		ids.New("env"), workspaceID, name, desc)
	return err
}

func (r *Repo) ListEnvironments(ctx context.Context, workspaceID string) ([]model.Environment, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT id,name,description,created_at FROM environment WHERE workspace_id=$1 ORDER BY name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Environment{}
	for rows.Next() {
		var e model.Environment
		e.WorkspaceID = workspaceID
		if err := rows.Scan(&e.ID, &e.Name, &e.Description, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r *Repo) BindEnvDatasource(ctx context.Context, workspaceID, env, alias, datasourceID string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO env_binding(workspace_id,env,alias,datasource_id) VALUES($1,$2,$3,$4)
		 ON CONFLICT (workspace_id,env,alias) DO UPDATE SET datasource_id=EXCLUDED.datasource_id`,
		workspaceID, env, alias, datasourceID)
	return err
}

// EnvBinding 返回某环境某别名的数据源 ID，未绑定时返回空串。
func (r *Repo) EnvBinding(ctx context.Context, workspaceID, env, alias string) (string, error) {
	var id string
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT datasource_id FROM env_binding WHERE workspace_id=$1 AND env=$2 AND alias=$3`,
		workspaceID, env, alias).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (r *Repo) ListEnvBindings(ctx context.Context, workspaceID, env string) (map[string]string, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT b.alias, d.name FROM env_binding b JOIN datasource d ON d.id=b.datasource_id
		 WHERE b.workspace_id=$1 AND b.env=$2 ORDER BY b.alias`, workspaceID, env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var alias, name string
		if err := rows.Scan(&alias, &name); err != nil {
			return nil, err
		}
		out[alias] = name
	}
	return out, rows.Err()
}

func (r *Repo) DatasourceNameByID(ctx context.Context, workspaceID, id string) (string, error) {
	var name string
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT name FROM datasource WHERE workspace_id=$1 AND id=$2`, workspaceID, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errs.New(errs.CodeNotFound, "数据源不存在")
	}
	return name, err
}

// RunningInstances 返回工作空间内运行中的实例数。
func (r *Repo) RunningInstances(ctx context.Context, workspaceID string) (int, error) {
	var n int
	err := r.DB.Pool.QueryRow(ctx,
		`SELECT count(*) FROM instance WHERE workspace_id=$1 AND status IN ('running','queued','waiting')`,
		workspaceID).Scan(&n)
	return n, err
}

// SoftDeleteWorkspace 软删工作空间：改名并标记，保留数据便于审计。
func (r *Repo) SoftDeleteWorkspace(ctx context.Context, workspaceID string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`UPDATE workspace SET name = name || '__deleted_' || to_char(now(),'YYYYMMDDHH24MISS')
		 WHERE id=$1`, workspaceID)
	return err
}

// ---------- 结构变更与采集任务（T0208） ----------

func (r *Repo) SaveSchemaChange(ctx context.Context, workspaceID, datasource string, c model.SchemaChange) error {
	_, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO schema_change(id,workspace_id,datasource,schema_name,table_name,kind,column_name,detail,breaking)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		ids.New("sch"), workspaceID, datasource, c.Schema, c.Table, c.Kind, c.Column,
		c.From+" -> "+c.To, c.Breaking)
	return err
}

// ListSchemaChanges 返回近期结构变更。
func (r *Repo) ListSchemaChanges(ctx context.Context, workspaceID string, limit int) ([]model.SchemaChange, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT datasource,schema_name,table_name,kind,column_name,detail,breaking,created_at
		 FROM schema_change WHERE workspace_id=$1 ORDER BY created_at DESC LIMIT `+itoa(limit), workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.SchemaChange{}
	for rows.Next() {
		var c model.SchemaChange
		var detail string
		if err := rows.Scan(&c.Datasource, &c.Schema, &c.Table, &c.Kind, &c.Column, &detail, &c.Breaking, &c.CreatedAt); err != nil {
			return nil, err
		}
		if parts := strings.SplitN(detail, " -> ", 2); len(parts) == 2 {
			c.From, c.To = parts[0], parts[1]
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateDatasetPII 把敏感级别写回 dataset 表。
func (r *Repo) UpdateDatasetPII(ctx context.Context, workspaceID, table, level, reason string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`UPDATE dataset SET pii=$3, pii_reason=$4 WHERE workspace_id=$1 AND table_name=$2`,
		workspaceID, table, level, reason)
	return err
}

// ListPIIOverrides 返回人工覆盖的敏感级别。
func (r *Repo) ListPIIOverrides(ctx context.Context, workspaceID, datasource string) ([]model.PIIOverride, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT table_name, pii, pii_reason FROM dataset
		 WHERE workspace_id=$1 AND pii_override=true`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.PIIOverride{}
	for rows.Next() {
		var o model.PIIOverride
		if err := rows.Scan(&o.Table, &o.Level, &o.Reason); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (r *Repo) RecordCollectJob(ctx context.Context, workspaceID, datasource, status string, tables, changes int, errMsg string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`INSERT INTO collect_job(id,workspace_id,datasource,status,tables_seen,changes,error,finished_at)
		 VALUES($1,$2,$3,$4,$5,$6,$7,now())`,
		ids.New("cjb"), workspaceID, datasource, status, tables, changes, errMsg)
	return err
}

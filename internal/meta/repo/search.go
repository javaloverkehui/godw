package repo

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/godw/godw/internal/pkg/ids"
)

// =============================================================================
// T0210 元数据搜索与标签
//
// 设计取舍（中文场景是主要矛盾）：
//
//	PostgreSQL 的 simple 分词器不切中文，to_tsvector('simple','订单明细') 会得到
//	一个整体 token，搜「订单」永远命中不了。所以搜索分两层：
//	  主分 —— 名称匹配档位（全等 / 前缀 / 包含 / 注释包含），保证中文子串一定命中；
//	  副分 —— ts_rank_cd，只在英文标识符（dwd_order_detail）之间有区分度。
//	不用 trigram 扩展：pg_trgm 需要 CREATE EXTENSION 权限，运维成本换不来中文子串
//	能力（ILIKE 已经覆盖），故不引入。
//
//	标签统一存 entity_tag，责任人用约定标签 owner:<user> 表达，避免为每类实体加列。
// =============================================================================

// SearchFilter 是仓储层搜索条件。repo 不能依赖 service 的 DTO（会循环导入），
// 两边的字段一一对应，由 service 做转换。
type SearchFilter struct {
	Q          string
	Types      []string
	Layer      string
	Tags       []string
	Datasource string
	Owner      string
	Limit      int
}

// SearchRow 是一条命中记录。
type SearchRow struct {
	Type    string
	Name    string
	Layer   string
	Comment string
	Owner   string
	Score   float64
	Tags    []string
}

// searchEntity 描述一类可搜索实体如何映射到 SQL 列。
// 所有分支必须产出相同列数，才能 UNION ALL。
type searchEntity struct {
	kind    string
	from    string // 例如 "model m"
	name    string // 名称表达式
	layer   string // 分层表达式，没有就写 "''"
	comment string // 描述表达式
	// tags 是「该实体已挂标签」的数组表达式。
	tags string
	// tagHit 是「某个标签是否命中该实体」的判定模板，%s 为标签参数占位符。
	tagHit string
}

const emptyTags = `'{}'::text[]`

func tagsSubquery(kind, nameExpr string) string {
	return fmt.Sprintf(`COALESCE((SELECT array_agg(et.tag ORDER BY et.tag) FROM entity_tag et
	         WHERE et.workspace_id = $1 AND et.entity_type = '%s' AND et.entity_name = %s), %s)`,
		kind, nameExpr, emptyTags)
}

func searchEntities() []searchEntity {
	return []searchEntity{
		{
			kind: "model", from: "model m",
			name: "m.name", layer: "m.layer", comment: "m.description",
			tags: tagsSubquery("model", "m.name"),
			tagHit: "EXISTS (SELECT 1 FROM entity_tag et2 WHERE et2.workspace_id = $1 " +
				"AND et2.entity_type = 'model' AND et2.entity_name = m.name AND et2.tag = %s)",
		},
		{
			// 数据集是采集来的物理表，没有分层概念；它的标签既可能来自打标，
			// 也可能来自 0005 迁移加的 dataset.tags 列（PII 扫描时写入），两者合并。
			kind: "dataset", from: "dataset d",
			name: "d.table_name", layer: "''", comment: "d.comment",
			tags: "(" + tagsSubquery("dataset", "d.table_name") + " || d.tags)",
			tagHit: "(EXISTS (SELECT 1 FROM entity_tag et2 WHERE et2.workspace_id = $1 " +
				"AND et2.entity_type = 'dataset' AND et2.entity_name = d.table_name AND et2.tag = %s) " +
				"OR %s = ANY(d.tags))",
		},
		{
			kind: "pipeline", from: "pipeline p",
			name: "p.name", layer: "''", comment: "p.description",
			tags: tagsSubquery("pipeline", "p.name"),
			tagHit: "EXISTS (SELECT 1 FROM entity_tag et2 WHERE et2.workspace_id = $1 " +
				"AND et2.entity_type = 'pipeline' AND et2.entity_name = p.name AND et2.tag = %s)",
		},
		{
			kind: "chart", from: "chart c",
			name: "c.name", layer: "''", comment: "''",
			tags: tagsSubquery("chart", "c.name"),
			tagHit: "EXISTS (SELECT 1 FROM entity_tag et2 WHERE et2.workspace_id = $1 " +
				"AND et2.entity_type = 'chart' AND et2.entity_name = c.name AND et2.tag = %s)",
		},
	}
}

// SearchMetadata 执行元数据搜索。
//
// 参数顺序固定为：$1 workspace、$2 关键词（仅搜索模式）、其后为标签/责任人过滤，
// 最后是 limit。拼 SQL 时只拼占位符，值一律走参数，杜绝注入。
func (r *Repo) SearchMetadata(ctx context.Context, workspaceID string, f SearchFilter) ([]SearchRow, error) {
	if f.Limit <= 0 {
		f.Limit = 50
	}
	keyword := strings.TrimSpace(f.Q)
	args := []any{workspaceID}
	var kwArg string
	if keyword != "" {
		args = append(args, keyword)
		kwArg = fmt.Sprintf("$%d", len(args))
	}

	// 类型白名单过滤下推到 SQL，减少无谓的行搬运。
	want := map[string]bool{}
	for _, t := range f.Types {
		want[strings.ToLower(strings.TrimSpace(t))] = true
	}

	var branches []string
	for _, e := range searchEntities() {
		if len(want) > 0 && !want[e.kind] {
			continue
		}
		if f.Layer != "" && e.layer == "''" {
			continue // 无分层的实体不可能命中分层过滤
		}
		var conds []string
		conds = append(conds, fmt.Sprintf("%s.workspace_id = $1", aliasOf(e.from)))

		if keyword != "" {
			// 双通道：ILIKE 保证中文子串，tsquery 保证英文分词与词干。
			conds = append(conds, fmt.Sprintf(`(%s ILIKE '%%' || %s || '%%'
				OR %s ILIKE '%%' || %s || '%%'
				OR to_tsvector('simple', %s || ' ' || %s) @@ plainto_tsquery('simple', %s))`,
				e.name, kwArg, e.comment, kwArg, e.name, e.comment, kwArg))
		}
		if f.Layer != "" {
			conds = append(conds, fmt.Sprintf("%s = ", e.layer)+func() string {
				args = append(args, f.Layer)
				return fmt.Sprintf("$%d", len(args))
			}())
		}
		for _, tag := range f.Tags {
			tag = strings.TrimSpace(tag)
			if tag == "" {
				continue
			}
			args = append(args, tag)
			ph := fmt.Sprintf("$%d", len(args))
			hit := e.tagHit
			if strings.Count(hit, "%s") == 2 {
				hit = fmt.Sprintf(hit, ph, ph)
			} else {
				hit = fmt.Sprintf(hit, ph)
			}
			conds = append(conds, hit)
		}
		if f.Owner != "" {
			args = append(args, "owner:"+f.Owner)
			conds = append(conds, fmt.Sprintf(`EXISTS (SELECT 1 FROM entity_tag et3
				WHERE et3.workspace_id = $1 AND et3.entity_type = '%s'
				  AND et3.entity_name = %s AND et3.tag = $%d)`, e.kind, e.name, len(args)))
		}
		if f.Datasource != "" && e.kind == "dataset" {
			args = append(args, f.Datasource)
			conds = append(conds, fmt.Sprintf("d.datasource_id = $%d", len(args)))
		} else if f.Datasource != "" && e.kind != "dataset" {
			continue // 数据源过滤只对数据集有意义
		}

		score := "0.0"
		if keyword != "" {
			score = fmt.Sprintf(`(
				CASE WHEN lower(%s) = lower(%s) THEN 6.0
				     WHEN %s ILIKE %s || '%%' THEN 4.0
				     WHEN %s ILIKE '%%' || %s || '%%' THEN 3.0
				     WHEN %s ILIKE '%%' || %s || '%%' THEN 1.0
				     ELSE 0.0 END
				+ COALESCE(ts_rank_cd(to_tsvector('simple', %s || ' ' || %s),
				                      plainto_tsquery('simple', %s)), 0))`,
				e.name, kwArg, e.name, kwArg, e.name, kwArg, e.comment, kwArg,
				e.name, e.comment, kwArg)
		}

		branches = append(branches, fmt.Sprintf(
			`SELECT '%s'::text AS etype, %s::text AS name, %s::text AS layer,
			        %s::text AS comment, %s AS tags, %s::float8 AS score
			   FROM %s WHERE %s`,
			e.kind, e.name, e.layer, e.comment, e.tags, score, e.from,
			strings.Join(conds, "\n   AND ")))
	}

	if len(branches) == 0 {
		return nil, nil
	}

	args = append(args, f.Limit)
	sql := fmt.Sprintf(`SELECT * FROM (
%s
) hit ORDER BY score DESC, name ASC LIMIT $%d`,
		strings.Join(branches, "\nUNION ALL\n"), len(args))

	rows, err := r.DB.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []SearchRow{}
	for rows.Next() {
		var s SearchRow
		var tags []string
		if err := rows.Scan(&s.Type, &s.Name, &s.Layer, &s.Comment, &tags, &s.Score); err != nil {
			return nil, err
		}
		s.Tags = []string{}
		for _, t := range tags {
			if strings.HasPrefix(t, "owner:") {
				s.Owner = strings.TrimPrefix(t, "owner:")
				continue
			}
			s.Tags = append(s.Tags, t)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// aliasOf 从 "model m" 取出 "m"，用于拼 workspace 条件。
func aliasOf(from string) string {
	if i := strings.LastIndex(from, " "); i >= 0 {
		return from[i+1:]
	}
	return from
}

// ---------- 标签 ----------

// TagRow 是一个标签及其挂载数量。
type TagRow struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Color       string    `json:"color"`
	Description string    `json:"description"`
	Count       int       `json:"count"`
	CreatedAt   time.Time `json:"created_at"`
}

// SaveTag 新建或更新标签（同名 upsert）。颜色与描述允许后续修改。
func (r *Repo) SaveTag(ctx context.Context, workspaceID, name, color, desc string) (*TagRow, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("标签名不能为空")
	}
	var t TagRow
	err := r.DB.Pool.QueryRow(ctx,
		`INSERT INTO tag(id, workspace_id, name, color, description)
		 VALUES($1,$2,$3,$4,$5)
		 ON CONFLICT (workspace_id, name)
		 DO UPDATE SET color = EXCLUDED.color, description = EXCLUDED.description
		 RETURNING id, name, color, description, created_at`,
		ids.New("tag"), workspaceID, name, color, desc).
		Scan(&t.ID, &t.Name, &t.Color, &t.Description, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ListTags 列出标签，并带上当前挂载次数（用于前端排序与「空标签」清理）。
func (r *Repo) ListTags(ctx context.Context, workspaceID string) ([]TagRow, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT t.id, t.name, t.color, t.description, t.created_at,
		        COALESCE((SELECT count(*) FROM entity_tag et
		                   WHERE et.workspace_id = t.workspace_id AND et.tag = t.name), 0)
		   FROM tag t WHERE t.workspace_id = $1 ORDER BY t.name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TagRow{}
	for rows.Next() {
		var t TagRow
		if err := rows.Scan(&t.ID, &t.Name, &t.Color, &t.Description, &t.CreatedAt, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTag 删除标签定义并摘掉所有挂载，避免留下孤儿标签。
func (r *Repo) DeleteTag(ctx context.Context, workspaceID, name string) error {
	tx, err := r.DB.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`DELETE FROM entity_tag WHERE workspace_id=$1 AND tag=$2`, workspaceID, name); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM tag WHERE workspace_id=$1 AND name=$2`, workspaceID, name)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TagEntity 给实体挂多个标签。标签必须已在 tag 表定义，否则视为手误直接拒绝
// —— 静默建标签会让标签体系迅速退化成自由文本。
func (r *Repo) TagEntity(ctx context.Context, workspaceID, etype, ename string, tags []string) error {
	etype = strings.TrimSpace(etype)
	ename = strings.TrimSpace(ename)
	if !validEntityType(etype) {
		return fmt.Errorf("不支持的实体类型 %q（可选 model/dataset/chart/pipeline）", etype)
	}
	if ename == "" {
		return fmt.Errorf("实体名不能为空")
	}
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		var exists bool
		if err := r.DB.Pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM tag WHERE workspace_id=$1 AND name=$2)`,
			workspaceID, tag).Scan(&exists); err != nil {
			return err
		}
		if !exists && !strings.HasPrefix(tag, "owner:") {
			return fmt.Errorf("标签 %q 未定义，请先创建", tag)
		}
		if _, err := r.DB.Pool.Exec(ctx,
			`INSERT INTO entity_tag(workspace_id, entity_type, entity_name, tag)
			 VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
			workspaceID, etype, ename, tag); err != nil {
			return err
		}
	}
	return nil
}

// UntagEntity 摘掉一个标签。
func (r *Repo) UntagEntity(ctx context.Context, workspaceID, etype, ename, tag string) error {
	_, err := r.DB.Pool.Exec(ctx,
		`DELETE FROM entity_tag WHERE workspace_id=$1 AND entity_type=$2 AND entity_name=$3 AND tag=$4`,
		workspaceID, etype, ename, tag)
	return err
}

// EntityTags 返回某实体的标签。
func (r *Repo) EntityTags(ctx context.Context, workspaceID, etype, ename string) ([]string, error) {
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT tag FROM entity_tag WHERE workspace_id=$1 AND entity_type=$2 AND entity_name=$3
		 ORDER BY tag`, workspaceID, etype, ename)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// EntitiesByTag 反查：某标签下挂了哪些实体。
func (r *Repo) EntitiesByTag(ctx context.Context, workspaceID, tag string, limit int) ([]SearchRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := r.DB.Pool.Query(ctx,
		`SELECT entity_type, entity_name FROM entity_tag
		  WHERE workspace_id=$1 AND tag=$2 ORDER BY entity_type, entity_name LIMIT $3`,
		workspaceID, tag, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SearchRow{}
	for rows.Next() {
		var s SearchRow
		if err := rows.Scan(&s.Type, &s.Name); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func validEntityType(t string) bool {
	switch t {
	case "model", "dataset", "chart", "pipeline":
		return true
	}
	return false
}

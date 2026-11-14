package modelc

import (
	"fmt"
	"strings"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
)

// T0306 Schema 变更管理。
//
// 核心判定只有一条：这次变更会不会让现有下游查询读不到数据或读错数据。
// 会 → Breaking，必须走审批；不会 → Safe，直接执行。
// 平台不接受「先发布再发现问题」，所以 Diff 是发布前的强制关口。

// ChangeKind 是变更类型。
type ChangeKind string

const (
	ChangeAddColumn     ChangeKind = "add_column"
	ChangeDropColumn    ChangeKind = "drop_column"
	ChangeModifyType    ChangeKind = "modify_type"
	ChangeModifyNull    ChangeKind = "modify_nullable"
	ChangeModifyPK      ChangeKind = "modify_primary_key"
	ChangeModifyPart    ChangeKind = "modify_partition"
	ChangeModifyTTL     ChangeKind = "modify_ttl"
	ChangeRenameColumn  ChangeKind = "rename_column"
	ChangeModifyMat     ChangeKind = "modify_materialization"
	ChangeModifyComment ChangeKind = "modify_comment"
)

// Change 是一条结构差异。
type Change struct {
	Kind     ChangeKind `json:"kind"`
	Column   string     `json:"column,omitempty"`
	From     string     `json:"from,omitempty"`
	To       string     `json:"to,omitempty"`
	Breaking bool       `json:"breaking"`
	// Reason 说明为什么是 breaking，直接展示给用户。
	Reason string `json:"reason,omitempty"`
}

// DiffResult 是一次结构对比的完整结论。
type DiffResult struct {
	Changes  []Change `json:"changes"`
	Breaking int      `json:"breakingCount"`
	Safe     int      `json:"safeCount"`
}

// HasBreaking 判断是否需要审批。
func (d DiffResult) HasBreaking() bool { return d.Breaking > 0 }

// Diff 对比两个版本的模型定义。
func Diff(old, new model.ModelSpec) DiffResult {
	var out DiffResult
	add := func(c Change) {
		out.Changes = append(out.Changes, c)
		if c.Breaking {
			out.Breaking++
		} else {
			out.Safe++
		}
	}

	oldCols := map[string]model.ColumnSpec{}
	for _, c := range old.Columns {
		oldCols[strings.ToLower(c.Name)] = c
	}
	newCols := map[string]model.ColumnSpec{}
	for _, c := range new.Columns {
		newCols[strings.ToLower(c.Name)] = c
	}

	// 删列：下游 SELECT * 或显式引用都会断，直接 breaking。
	for name, c := range oldCols {
		if _, ok := newCols[name]; !ok {
			add(Change{Kind: ChangeDropColumn, Column: c.Name, From: c.Type, Breaking: true,
				Reason: "删除列会导致下游引用该列的查询失败"})
		}
	}

	// 加列：可空 / 有默认值 → safe；非空无默认值 → breaking（存量行无法填充）。
	for name, c := range newCols {
		if _, ok := oldCols[name]; ok {
			continue
		}
		breaking := !c.Nullable && c.Default == ""
		reason := ""
		if breaking {
			reason = "新增非空且无默认值的列，存量数据无法满足约束"
		}
		add(Change{Kind: ChangeAddColumn, Column: c.Name, To: c.Type, Breaking: breaking, Reason: reason})
	}

	// 改类型：一律 breaking。即使「看起来兼容」（int → bigint），
	// 下游的 CAST 与索引都可能失效，不值得赌。
	for name, nc := range newCols {
		oc, ok := oldCols[name]
		if !ok {
			continue
		}
		if !strings.EqualFold(oc.Type, nc.Type) {
			add(Change{Kind: ChangeModifyType, Column: nc.Name, From: oc.Type, To: nc.Type, Breaking: true,
				Reason: "修改列类型可能导致下游 CAST 失败或精度丢失"})
		}
		if oc.Nullable != nc.Nullable {
			// 可空 → 非空：存量 NULL 会让 DDL 失败或写入被拒，breaking。
			breaking := oc.Nullable && !nc.Nullable
			reason := ""
			if breaking {
				reason = "由可空改为非空，存量 NULL 值会导致 DDL 或写入失败"
			}
			add(Change{Kind: ChangeModifyNull, Column: nc.Name,
				From: boolText(oc.Nullable), To: boolText(nc.Nullable), Breaking: breaking, Reason: reason})
		}
		if oc.Comment != nc.Comment {
			add(Change{Kind: ChangeModifyComment, Column: nc.Name, From: oc.Comment, To: nc.Comment})
		}
	}

	// 主键 / 唯一键：影响幂等写入与去重语义，breaking。
	if !sameStrings(old.PrimaryKey, new.PrimaryKey) {
		add(Change{Kind: ChangeModifyPK, From: strings.Join(old.PrimaryKey, ","), To: strings.Join(new.PrimaryKey, ","),
			Breaking: true, Reason: "主键变更会让幂等写入的判定条件改变，历史数据可能重复"})
	}
	// 分区：改了要重建表，breaking。
	if !sameStrings(old.PartitionBy, new.PartitionBy) {
		add(Change{Kind: ChangeModifyPart, From: strings.Join(old.PartitionBy, ","), To: strings.Join(new.PartitionBy, ","),
			Breaking: true, Reason: "分区键变更需要重建表并迁移数据"})
	}
	if old.TTL != new.TTL {
		add(Change{Kind: ChangeModifyTTL, From: old.TTL, To: new.TTL,
			Breaking: false, Reason: "TTL 变更只影响后续清理，不影响存量查询"})
	}
	if old.Materialization != new.Materialization {
		add(Change{Kind: ChangeModifyMat, From: old.Materialization, To: new.Materialization,
			Breaking: true, Reason: "物化方式变更会替换整个对象，需重建"})
	}
	return out
}

// MigrationPlan 是可执行的变更方案。
type MigrationPlan struct {
	Statements []string `json:"statements"`
	// Manual 是需要人工处理的步骤（如类型变更的重建建议）。
	Manual []string `json:"manual,omitempty"`
	// Rebuild 表示必须走「新建表 + 双写 + 切换」流程。
	Rebuild bool `json:"rebuild"`
}

// PlanMigration 生成迁移 SQL。只处理能安全 ALTER 的部分，
// 剩下的明确列为 Manual，不假装能自动完成。
func PlanMigration(old, new model.ModelSpec, d dialect.Dialect) MigrationPlan {
	var p MigrationPlan
	diff := Diff(old, new)
	table := qualify(new.Schema, new.TableName(), d)

	oldCols := map[string]model.ColumnSpec{}
	for _, c := range old.Columns {
		oldCols[strings.ToLower(c.Name)] = c
	}

	for _, ch := range diff.Changes {
		switch ch.Kind {
		case ChangeAddColumn:
			nc := findCol(new, ch.Column)
			sql := "ALTER TABLE " + table + " ADD COLUMN " + d.QuoteIdent(nc.Name) + " " + physType(nc.Type, d)
			if !nc.Nullable {
				if nc.Default != "" {
					sql += " NOT NULL DEFAULT " + nc.Default
				} else {
					// 非空无默认值：先加可空列，回填后再改非空。这里给出两步。
					sql += " " // 加可空
					p.Manual = append(p.Manual,
						fmt.Sprintf("列 %s 为非空且无默认值：先以可空列加入并回填，再执行 ALTER COLUMN SET NOT NULL", nc.Name))
				}
			}
			if nc.Comment != "" && d.Name() == "postgres" {
				sql += ";\nCOMMENT ON COLUMN " + table + "." + d.QuoteIdent(nc.Name) + " IS " + lit(nc.Comment)
			}
			if d.Name() == "clickhouse" {
				// ClickHouse 的 ADD COLUMN 支持 AFTER，用它保持列序与 YAML 一致。
				if after := prevColumn(new, nc.Name); after != "" {
					sql += " AFTER " + d.QuoteIdent(after)
				}
			}
			p.Statements = append(p.Statements, sql+";")

		case ChangeDropColumn:
			if d.Name() == "clickhouse" {
				p.Statements = append(p.Statements,
					"ALTER TABLE "+table+" DROP COLUMN "+d.QuoteIdent(ch.Column)+";")
			} else {
				p.Statements = append(p.Statements,
					"ALTER TABLE "+table+" DROP COLUMN "+d.QuoteIdent(ch.Column)+";")
			}

		case ChangeModifyNull:
			if ch.To == "false" {
				p.Statements = append(p.Statements,
					"ALTER TABLE "+table+" ALTER COLUMN "+d.QuoteIdent(ch.Column)+" SET NOT NULL;")
			} else {
				p.Statements = append(p.Statements,
					"ALTER TABLE "+table+" ALTER COLUMN "+d.QuoteIdent(ch.Column)+" DROP NOT NULL;")
			}

		case ChangeModifyType:
			if d.Name() == "clickhouse" {
				nc := findCol(new, ch.Column)
				p.Statements = append(p.Statements,
					"ALTER TABLE "+table+" MODIFY COLUMN "+d.QuoteIdent(ch.Column)+" "+physType(nc.Type, d)+";")
			} else {
				p.Manual = append(p.Manual,
					fmt.Sprintf("%s 不支持直接改列类型（%s → %s），建议新建表 + 双写 + 切换", d.Name(), ch.From, ch.To))
			}

		case ChangeModifyPart, ChangeModifyMat:
			p.Rebuild = true
			p.Manual = append(p.Manual, ch.Reason+"；请使用「新建表 + 双写 + 切换」流程")
		}
	}

	if d.Name() == "postgres" && p.Rebuild {
		p.Manual = append(p.Manual, "重建步骤：1) 建 v2 表 2) 双写 3) 校验行数 4) 原子 RENAME 5) 保留旧表 7 天")
	}

	// 注释同步：独立于结构变更，任何变更都补一遍。
	if d.Name() == "postgres" {
		if new.Description != old.Description && new.Description != "" {
			p.Statements = append(p.Statements, "COMMENT ON TABLE "+table+" IS "+lit(new.Description)+";")
		}
		for _, c := range new.Columns {
			oc, ok := oldCols[strings.ToLower(c.Name)]
			if ok && oc.Comment == c.Comment {
				continue
			}
			if c.Comment != "" {
				p.Statements = append(p.Statements,
					"COMMENT ON COLUMN "+table+"."+d.QuoteIdent(c.Name)+" IS "+lit(c.Comment)+";")
			}
		}
	}
	return p
}

func findCol(spec model.ModelSpec, name string) model.ColumnSpec {
	for _, c := range spec.Columns {
		if strings.EqualFold(c.Name, name) {
			return c
		}
	}
	return model.ColumnSpec{Name: name, Type: "string"}
}

// prevColumn 返回某列在定义中的前一列名，用于 ClickHouse 的 AFTER。
func prevColumn(spec model.ModelSpec, name string) string {
	for i, c := range spec.Columns {
		if strings.EqualFold(c.Name, name) {
			if i == 0 {
				return ""
			}
			return spec.Columns[i-1].Name
		}
	}
	return ""
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

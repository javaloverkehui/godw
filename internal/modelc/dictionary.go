package modelc

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/godw/godw/internal/dialect"
	"github.com/godw/godw/internal/meta/model"
)

// MarshalSpec 以稳定格式输出模型 YAML：字段顺序固定、缩进 2 空格。
// 稳定输出让「版本 diff」可比，也让脚手架产物可以直接进 Git。
func MarshalSpec(spec model.ModelSpec) (string, error) {
	var b strings.Builder
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(spec); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return strings.TrimSpace(b.String()) + "\n", nil
}

// T0310 数据字典导出。
//
// 字典不是「给人看的文档」，而是「契约的快照」：
// 字段类型、口径、上下游、负责人，任何一项缺失都会让排查变慢。

// DictEntry 是数据字典中的一张表。
type DictEntry struct {
	Layer       string       `json:"layer"`
	Name        string       `json:"name"`
	Table       string       `json:"table"`
	Description string       `json:"description"`
	Owner       string       `json:"owner"`
	Tags        []string     `json:"tags"`
	Columns     []DictColumn `json:"columns"`
	Upstream    []string     `json:"upstream"`
	Downstream  []string     `json:"downstream"`
}

// DictColumn 是字典中的一列。
type DictColumn struct {
	Name           string `json:"name"`
	Type           string `json:"type"`
	Physical       string `json:"physical"`
	Nullable       bool   `json:"nullable"`
	Comment        string `json:"comment"`
	Classification string `json:"classification,omitempty"`
	Mask           string `json:"mask,omitempty"`
}

// DictInput 是导出字典所需的全部数据，由 service 层组装。
type DictInput struct {
	Specs      []model.ModelSpec
	Upstream   map[string][]string
	Downstream map[string][]string
	Policies   map[string]map[string]model.ColumnPolicy // table → column → policy
	Engine     string
}

// BuildDictionary 按层排序生成字典。
func BuildDictionary(in DictInput) []DictEntry {
	rank := map[string]int{"ods": 0, "dim": 1, "dwd": 2, "dws": 3, "ads": 4}
	specs := make([]model.ModelSpec, len(in.Specs))
	copy(specs, in.Specs)
	sort.SliceStable(specs, func(i, j int) bool {
		if rank[specs[i].Layer] != rank[specs[j].Layer] {
			return rank[specs[i].Layer] < rank[specs[j].Layer]
		}
		return specs[i].Name < specs[j].Name
	})

	out := make([]DictEntry, 0, len(specs))
	for _, s := range specs {
		e := DictEntry{
			Layer: s.Layer, Name: s.Name, Table: s.TableName(),
			Description: s.Description, Owner: s.Owner, Tags: s.Tags,
			Upstream: in.Upstream[s.Name], Downstream: in.Downstream[s.Name],
		}
		pol := in.Policies[s.TableName()]
		for _, c := range s.Columns {
			dc := DictColumn{Name: c.Name, Type: c.Type, Nullable: c.Nullable, Comment: c.Comment}
			if ts, err := parseTypeSpec(c.Type); err == nil {
				if m, err := dialect.MapType(in.Engine, ts); err == nil {
					dc.Physical = m.Physical
				}
			}
			if p, ok := pol[c.Name]; ok {
				dc.Classification = p.Classification
				dc.Mask = p.MaskType
			}
			e.Columns = append(e.Columns, dc)
		}
		out = append(out, e)
	}
	return out
}

// DictionaryMarkdown 输出 Markdown 表格，供文档站与 Git 归档。
func DictionaryMarkdown(entries []DictEntry) string {
	var b strings.Builder
	b.WriteString("# 数据字典\n\n")
	b.WriteString(fmt.Sprintf("共 %d 张表。本文档由 `godw export-doc` 生成，请勿手工编辑。\n\n", len(entries)))

	// 目录：按层分组，方便大仓库定位。
	byLayer := map[string][]DictEntry{}
	for _, e := range entries {
		byLayer[e.Layer] = append(byLayer[e.Layer], e)
	}
	b.WriteString("## 目录\n\n")
	for _, layer := range []string{"ods", "dim", "dwd", "dws", "ads"} {
		list := byLayer[layer]
		if len(list) == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("- **%s**（%d）：", strings.ToUpper(layer), len(list)))
		names := make([]string, len(list))
		for i, e := range list {
			names[i] = e.Name
		}
		b.WriteString(strings.Join(names, "、") + "\n")
	}
	b.WriteString("\n")

	for _, layer := range []string{"ods", "dim", "dwd", "dws", "ads"} {
		list := byLayer[layer]
		if len(list) == 0 {
			continue
		}
		b.WriteString("## " + strings.ToUpper(layer) + " 层\n\n")
		for _, e := range list {
			b.WriteString("### " + e.Name + "\n\n")
			if e.Description != "" {
				b.WriteString(e.Description + "\n\n")
			}
			meta := []string{}
			meta = append(meta, "物理表 `"+e.Layer+"."+e.Table+"`")
			if e.Owner != "" {
				meta = append(meta, "负责人 "+e.Owner)
			}
			if len(e.Tags) > 0 {
				meta = append(meta, "标签 "+strings.Join(e.Tags, "/"))
			}
			b.WriteString(strings.Join(meta, "　|　") + "\n\n")
			b.WriteString("| 字段 | 逻辑类型 | 物理类型 | 可空 | 说明 | 分级 | 脱敏 |\n")
			b.WriteString("|---|---|---|---|---|---|---|\n")
			for _, c := range e.Columns {
				b.WriteString(fmt.Sprintf("| %s | %s | %s | %s | %s | %s | %s |\n",
					c.Name, c.Type, c.Physical, boolText(c.Nullable), dash(c.Comment),
					dash(c.Classification), dash(c.Mask)))
			}
			if len(e.Upstream) > 0 {
				b.WriteString("\n上游：" + strings.Join(e.Upstream, "、") + "\n")
			}
			if len(e.Downstream) > 0 {
				b.WriteString("\n下游：" + strings.Join(e.Downstream, "、") + "\n")
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

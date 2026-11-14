package compile

import (
	"fmt"
	"strings"
	"time"

	"github.com/godw/godw/internal/meta/model"
	"github.com/godw/godw/internal/pkg/errs"
)

// T0407 增量条件推导。
//
// 原则：能推导就推导，推导不出来就报错让人写 where。
// 「猜一个条件然后全表扫描」比报错更糟——它不会失败，只会慢慢拖垮库。

// InferenceKind 说明条件是怎么来的，前端要展示给用户看。
type InferenceKind string

const (
	InferPartition  InferenceKind = "partition"   // 依据模型分区键
	InferTimeColumn InferenceKind = "time_column" // 依据 updated_at 等时间列
	InferExplicit   InferenceKind = "explicit"    // 用户手写 where
	InferNone       InferenceKind = "none"        // 全量
)

// Inference 是一次增量推导的结果。
type Inference struct {
	Kind     InferenceKind `json:"kind"`
	Column   string        `json:"column,omitempty"`
	Where    string        `json:"where,omitempty"`
	Describe string        `json:"describe"`
	// ScannedPartitions 是本次预计扫描的分区，用于前端「影响面」展示。
	ScannedPartitions []string `json:"scannedPartitions,omitempty"`
	// Warn 是降级提示（例如按时间列推导精度不如分区）。
	Warn []string `json:"warn,omitempty"`
}

// 常见的时间列候选，按优先级排列。
var timeColumnCandidates = []string{
	"updated_at", "update_time", "modified_at", "gmt_modified",
	"created_at", "create_time", "gmt_create", "event_time", "ts",
}

// InferIncremental 根据目标模型与业务日期推导增量条件。
//
// explicit 非空时直接使用（用户手写优先，平台不覆盖人的判断）。
func InferIncremental(target model.ModelSpec, bizDate string, explicit string) (Inference, error) {
	if strings.TrimSpace(explicit) != "" {
		return Inference{
			Kind:     InferExplicit,
			Where:    strings.TrimSpace(explicit),
			Describe: "使用节点显式声明的增量条件",
		}, nil
	}

	d, err := time.Parse("2006-01-02", strings.TrimSpace(bizDate))
	if err != nil {
		return Inference{}, errs.New(errs.CodeInvalidArgument, "biz_date 必须是 YYYY-MM-DD，实际 "+bizDate)
	}

	// 1) 分区键优先：分区裁剪成本最低，且语义最明确。
	if len(target.PartitionBy) > 0 {
		col := partitionColumnOf(target)
		if col != "" {
			if !hasColumn(target, col) {
				return Inference{}, errs.New(errs.CodeModelInvalid,
					fmt.Sprintf("模型 %s 声明的分区列 %s 不存在于列定义中", target.Name, col))
			}
			return Inference{
				Kind:              InferPartition,
				Column:            col,
				Where:             fmt.Sprintf("%s = '%s'", col, d.Format("2006-01-02")),
				Describe:          fmt.Sprintf("按分区列 %s 裁剪，仅扫描 %s 一个分区", col, d.Format("2006-01-02")),
				ScannedPartitions: []string{d.Format("2006-01-02")},
			}, nil
		}
	}

	// 2) 时间列推导：半开区间 [bizdate, bizdate+1)，避免边界重复。
	for _, cand := range timeColumnCandidates {
		if !hasColumn(target, cand) {
			continue
		}
		next := d.AddDate(0, 0, 1).Format("2006-01-02")
		return Inference{
			Kind:   InferTimeColumn,
			Column: cand,
			Where: fmt.Sprintf("%s >= '%s' AND %s < '%s'",
				cand, d.Format("2006-01-02"), cand, next),
			Describe: fmt.Sprintf("无分区键，按时间列 %s 推导半开区间 [%s, %s)", cand, d.Format("2006-01-02"), next),
			Warn:     []string{"按时间列过滤无法分区裁剪，大表上可能较慢；建议给模型增加分区键"},
		}, nil
	}

	// 3) 推导不出来：明确报错，不猜。
	return Inference{}, errs.New(errs.CodeModelInvalid,
		fmt.Sprintf("模型 %s 既无分区键，也没有可识别的时间列（%s），无法推导增量条件",
			target.Name, strings.Join(timeColumnCandidates[:5], "/"))).
		WithMeta("model", target.Name).
		WithMeta("hint", "请在节点的 config.where 中显式声明增量条件，或给模型加分区键").
		WithMeta("candidates", timeColumnCandidates)
}

// ApplyWhere 把推导出的条件注入 SQL。
//
// 注入点选择：包一层子查询而不是改写 WHERE。
// 理由：原 SQL 可能有 GROUP BY / UNION / LIMIT，直接找 WHERE 位置做字符串插入极易出错；
// 包一层 `SELECT * FROM (...) t WHERE cond` 在所有引擎上都成立。
func ApplyWhere(sqlText string, inf Inference) string {
	if strings.TrimSpace(inf.Where) == "" {
		return sqlText
	}
	body := strings.TrimRight(strings.TrimSpace(sqlText), ";")
	return fmt.Sprintf("SELECT * FROM (\n%s\n) godw_incremental WHERE %s", body, inf.Where)
}

// ValidateIncremental 校验增量配置是否会退化成全表扫描。
// 用于发布前的静态检查，不依赖实际执行。
func ValidateIncremental(target model.ModelSpec, inf Inference) []string {
	var warns []string
	if inf.Kind == InferNone || inf.Where == "" {
		warns = append(warns, fmt.Sprintf("模型 %s 将全量扫描，生产环境请确认数据量可控", target.Name))
	}
	if inf.Kind == InferTimeColumn {
		warns = append(warns, inf.Warn...)
	}
	if inf.Kind == InferPartition {
		// 分区列必须非空，否则 NULL 行会永远不被增量覆盖。
		for _, c := range target.Columns {
			if strings.EqualFold(c.Name, inf.Column) && c.Nullable {
				warns = append(warns, fmt.Sprintf("分区列 %s 可空，NULL 行不会被任何一次增量覆盖", inf.Column))
			}
		}
	}
	return warns
}

func partitionColumnOf(spec model.ModelSpec) string {
	if len(spec.PartitionBy) == 0 {
		return ""
	}
	raw := strings.TrimSpace(spec.PartitionBy[0])
	if i := strings.Index(raw, "("); i > 0 {
		inner := strings.TrimSuffix(raw[i+1:], ")")
		if j := strings.LastIndex(inner, ","); j >= 0 {
			inner = inner[j+1:]
		}
		return strings.Trim(strings.TrimSpace(inner), `"'`)
	}
	return raw
}

func hasColumn(spec model.ModelSpec, name string) bool {
	for _, c := range spec.Columns {
		if strings.EqualFold(c.Name, name) {
			return true
		}
	}
	return false
}

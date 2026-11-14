package model

// PipelineSpec 是管道 DSL 的根结构。YAML 是唯一事实来源，画布只是它的图形化编辑态。
//
//	kind: Pipeline
//	name: dwd_order_daily
//	schedule: "0 2 * * *"
//	nodes:
//	  - id: src_order
//	    type: source
//	    datasource: pg_main
//	    sql: SELECT * FROM ods."order" WHERE dt = '${biz_date}'
//	  - id: dwd_order
//	    type: sql
//	    depends_on: [src_order]
//	    model: dwd_order
//	    write_mode: upsert
//	    sql: SELECT ...
type PipelineSpec struct {
	Kind        string `json:"kind" yaml:"kind"`
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Schedule    string `json:"schedule,omitempty" yaml:"schedule,omitempty"`
	Timezone    string `json:"timezone,omitempty" yaml:"timezone,omitempty"`
	Concurrency int    `json:"concurrency,omitempty" yaml:"concurrency,omitempty"`
	// ResourceGroup 决定该管道占用哪个资源组配额，默认 default。
	ResourceGroup string `json:"resourceGroup,omitempty" yaml:"resource_group,omitempty"`
	// Priority 1–9，1 最高。默认 5。
	Priority int `json:"priority,omitempty" yaml:"priority,omitempty"`
	// SLA 声明期望完成时间（HH:mm），超时由告警规则消费。
	SLA      string       `json:"sla,omitempty" yaml:"sla,omitempty"`
	Params   []ParamSpec  `json:"params,omitempty" yaml:"params,omitempty"`
	Defaults NodeDefaults `json:"defaults,omitempty" yaml:"defaults,omitempty"`
	Nodes    []NodeSpec   `json:"nodes" yaml:"nodes"`
}

type ParamSpec struct {
	Name        string `json:"name" yaml:"name"`
	Type        string `json:"type" yaml:"type"`
	Required    bool   `json:"required,omitempty" yaml:"required,omitempty"`
	Default     any    `json:"default,omitempty" yaml:"default,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

type NodeDefaults struct {
	Retry      int    `json:"retry,omitempty" yaml:"retry,omitempty"`
	TimeoutSec int    `json:"timeout_sec,omitempty" yaml:"timeout_sec,omitempty"`
	WriteMode  string `json:"write_mode,omitempty" yaml:"write_mode,omitempty"`
	Datasource string `json:"datasource,omitempty" yaml:"datasource,omitempty"`
}

// NodeSpec 是管道中的一个算子。Type 决定执行器与必需字段。
type NodeSpec struct {
	ID         string         `json:"id" yaml:"id"`
	Type       string         `json:"type" yaml:"type"`
	Name       string         `json:"name,omitempty" yaml:"name,omitempty"`
	DependsOn  []string       `json:"dependsOn,omitempty" yaml:"depends_on,omitempty"`
	Datasource string         `json:"datasource,omitempty" yaml:"datasource,omitempty"`
	Model      string         `json:"model,omitempty" yaml:"model,omitempty"`
	SQL        string         `json:"sql,omitempty" yaml:"sql,omitempty"`
	WriteMode  string         `json:"writeMode,omitempty" yaml:"write_mode,omitempty"`
	Config     map[string]any `json:"config,omitempty" yaml:"config,omitempty"`
	Retry      int            `json:"retry,omitempty" yaml:"retry,omitempty"`
	TimeoutSec int            `json:"timeoutSec,omitempty" yaml:"timeout_sec,omitempty"`
	// ContinueOnFailure 仅对质量节点有效：失败是否阻断下游。
	ContinueOnFailure bool `json:"continueOnFailure,omitempty" yaml:"continue_on_failure,omitempty"`
}

// NodeType 常量。
const (
	NodeSource  = "source"
	NodeSQL     = "sql"
	NodeQuality = "quality"
	NodeBranch  = "branch"
	NodeSink    = "sink"
	NodeShell   = "shell"
	NodeHTTP    = "http"
)

// WriteMode 常量。
const (
	WriteAppend             = "append"
	WriteOverwrite          = "overwrite"
	WriteUpsert             = "upsert"
	WriteOverwritePartition = "overwrite_partition"
)

// ValidNodeType 校验节点类型。
func ValidNodeType(t string) bool {
	switch t {
	case NodeSource, NodeSQL, NodeQuality, NodeBranch, NodeSink, NodeShell, NodeHTTP:
		return true
	}
	return false
}

// ValidWriteMode 校验写入模式。
func ValidWriteMode(m string) bool {
	switch m {
	case "", WriteAppend, WriteOverwrite, WriteUpsert, WriteOverwritePartition:
		return true
	}
	return false
}

package model

import (
	"time"
)

// Layer 是数据分层。ODS 保原始、DWD 做清洗、DWS 做汇总、ADS 面向应用。
type Layer string

const (
	LayerODS Layer = "ods"
	LayerDWD Layer = "dwd"
	LayerDWS Layer = "dws"
	LayerADS Layer = "ads"
	LayerDIM Layer = "dim"
)

// ValidLayer 校验分层取值。
func ValidLayer(l string) bool {
	switch Layer(l) {
	case LayerODS, LayerDWD, LayerDWS, LayerADS, LayerDIM:
		return true
	}
	return false
}

type Workspace struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"createdAt"`
}

type User struct {
	ID           string    `json:"id"`
	WorkspaceID  string    `json:"workspaceId"`
	Email        string    `json:"email"`
	DisplayName  string    `json:"displayName"`
	PasswordHash string    `json:"-"`
	Role         string    `json:"role"`
	CreatedAt    time.Time `json:"createdAt"`
}

type Datasource struct {
	ID          string         `json:"id"`
	WorkspaceID string         `json:"workspaceId"`
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Config      map[string]any `json:"config"`
	Status      string         `json:"status"`
	LastError   string         `json:"lastError,omitempty"`
	CheckedAt   *time.Time     `json:"checkedAt,omitempty"`
	CreatedAt   time.Time      `json:"createdAt"`
	UpdatedAt   time.Time      `json:"updatedAt"`
}

type Dataset struct {
	ID           string       `json:"id"`
	DatasourceID string       `json:"datasourceId"`
	Schema       string       `json:"schema"`
	Name         string       `json:"name"`
	Type         string       `json:"type"`
	Comment      string       `json:"comment"`
	RowCount     int64        `json:"rowCount"`
	SizeBytes    int64        `json:"sizeBytes"`
	Columns      []ColumnSpec `json:"columns"`
	CollectedAt  time.Time    `json:"collectedAt"`
	// PII 是敏感级别（none/low/medium/high），由采集后的识别任务写入。
	PII string `json:"pii,omitempty"`
	// PIIReason 说明为什么判定为该级别，便于人工复核。
	PIIReason string `json:"piiReason,omitempty"`
	// Tags 是挂在该表上的标签，表详情与搜索都用得到。
	Tags []string `json:"tags,omitempty"`
}

type ColumnSpec struct {
	Name     string `json:"name" yaml:"name"`
	Type     string `json:"type" yaml:"type"`
	Nullable bool   `json:"nullable" yaml:"nullable"`
	Comment  string `json:"comment,omitempty" yaml:"comment,omitempty"`
	Default  string `json:"default,omitempty" yaml:"default,omitempty"`
}

// ModelSpec 是模型 YAML 的完整定义，也是版本快照的内容。
type ModelSpec struct {
	Kind        string       `json:"kind" yaml:"kind"`
	Name        string       `json:"name" yaml:"name"`
	Layer       string       `json:"layer" yaml:"layer"`
	Description string       `json:"description,omitempty" yaml:"description,omitempty"`
	Datasource  string       `json:"datasource" yaml:"datasource"`
	Schema      string       `json:"schema,omitempty" yaml:"schema,omitempty"`
	Table       string       `json:"table,omitempty" yaml:"table,omitempty"`
	Columns     []ColumnSpec `json:"columns" yaml:"columns"`
	PrimaryKey  []string     `json:"primaryKey,omitempty" yaml:"primary_key,omitempty"`
	UniqueKey   []string     `json:"uniqueKey,omitempty" yaml:"unique_key,omitempty"`
	PartitionBy []string     `json:"partitionBy,omitempty" yaml:"partition_by,omitempty"`
	Indexes     []IndexSpec  `json:"indexes,omitempty" yaml:"indexes,omitempty"`
	Tags        []string     `json:"tags,omitempty" yaml:"tags,omitempty"`
	Owner       string       `json:"owner,omitempty" yaml:"owner,omitempty"`

	// TTL 是数据生命周期，格式 30d/12m/3y。
	TTL string `json:"ttl,omitempty" yaml:"ttl,omitempty"`
	// Materialization 取 table|view|mv|incremental，默认 table。
	Materialization string `json:"materialization,omitempty" yaml:"materialization,omitempty"`
	// Query 是 view/mv 的定义查询。
	Query string `json:"query,omitempty" yaml:"query,omitempty"`
	// AllowLossy 表示显式接受有损类型映射。
	AllowLossy bool `json:"allowLossy,omitempty" yaml:"allow_lossy,omitempty"`
	// BucketBy / Buckets 用于分桶（ClickHouse 分片键、Hive 分桶）。当前仅记录，供引擎适配层使用。
	BucketBy []string `json:"bucketBy,omitempty" yaml:"bucket_by,omitempty"`
	Buckets  int      `json:"buckets,omitempty" yaml:"buckets,omitempty"`
}

type IndexSpec struct {
	Name    string   `json:"name,omitempty" yaml:"name,omitempty"`
	Columns []string `json:"columns" yaml:"columns"`
	Unique  bool     `json:"unique,omitempty" yaml:"unique,omitempty"`
}

// TableName 返回物理表名，未显式指定时与模型同名。
func (s ModelSpec) TableName() string {
	if s.Table != "" {
		return s.Table
	}
	return s.Name
}

type Model struct {
	ID            string     `json:"id"`
	WorkspaceID   string     `json:"workspaceId"`
	Name          string     `json:"name"`
	Layer         string     `json:"layer"`
	Description   string     `json:"description"`
	DatasourceID  string     `json:"datasourceId,omitempty"`
	ActiveVersion int        `json:"activeVersion"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	Spec          *ModelSpec `json:"spec,omitempty"`
	DDL           string     `json:"ddl,omitempty"`
}

type ModelVersion struct {
	ID          string     `json:"id"`
	ModelID     string     `json:"modelId"`
	Version     int        `json:"version"`
	Spec        ModelSpec  `json:"spec"`
	DDL         string     `json:"ddl"`
	Status      string     `json:"status"`
	PublishedBy string     `json:"publishedBy,omitempty"`
	PublishedAt *time.Time `json:"publishedAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
}

type Pipeline struct {
	ID            string        `json:"id"`
	WorkspaceID   string        `json:"workspaceId"`
	Name          string        `json:"name"`
	Description   string        `json:"description"`
	Schedule      string        `json:"schedule"`
	Timezone      string        `json:"timezone"`
	Concurrency   int           `json:"concurrency"`
	ResourceGroup string        `json:"resourceGroup,omitempty"`
	Priority      int           `json:"priority,omitempty"`
	Enabled       bool          `json:"enabled"`
	ActiveVersion int           `json:"activeVersion"`
	CreatedAt     time.Time     `json:"createdAt"`
	UpdatedAt     time.Time     `json:"updatedAt"`
	Spec          *PipelineSpec `json:"spec,omitempty"`
}

type PipelineVersion struct {
	ID         string       `json:"id"`
	PipelineID string       `json:"pipelineId"`
	Version    int          `json:"version"`
	Spec       PipelineSpec `json:"spec"`
	Status     string       `json:"status"`
	CreatedAt  time.Time    `json:"createdAt"`
}

type Instance struct {
	ID             string         `json:"id"`
	WorkspaceID    string         `json:"workspaceId"`
	PipelineID     string         `json:"pipelineId"`
	ResourceGroup  string         `json:"resourceGroup,omitempty"`
	Priority       int            `json:"priority,omitempty"`
	QueuedAt       *time.Time     `json:"queuedAt,omitempty"`
	PipelineName   string         `json:"pipelineName,omitempty"`
	PipelineVer    int            `json:"pipelineVersion"`
	BizDate        string         `json:"bizDate"`
	TriggerType    string         `json:"triggerType"`
	Status         string         `json:"status"`
	IdempotencyKey string         `json:"idempotencyKey"`
	Params         map[string]any `json:"params"`
	Error          string         `json:"error,omitempty"`
	WorkerOwner    string         `json:"workerOwner,omitempty"`
	LeaseUntil     *time.Time     `json:"leaseUntil,omitempty"`
	StartedAt      *time.Time     `json:"startedAt,omitempty"`
	FinishedAt     *time.Time     `json:"finishedAt,omitempty"`
	CreatedAt      time.Time      `json:"createdAt"`
	Tasks          []TaskInstance `json:"tasks,omitempty"`
}

type TaskInstance struct {
	ID           string     `json:"id"`
	InstanceID   string     `json:"instanceId"`
	NodeID       string     `json:"nodeId"`
	NodeType     string     `json:"nodeType"`
	Status       string     `json:"status"`
	Attempt      int        `json:"attempt"`
	LeaseOwner   string     `json:"leaseOwner,omitempty"`
	LeaseUntil   *time.Time `json:"leaseUntil,omitempty"`
	RowsAffected int64      `json:"rowsAffected"`
	DurationMS   int64      `json:"durationMs"`
	Error        string     `json:"error,omitempty"`
	StartedAt    *time.Time `json:"startedAt,omitempty"`
	FinishedAt   *time.Time `json:"finishedAt,omitempty"`
}

type QualityRule struct {
	ID          string         `json:"id"`
	WorkspaceID string         `json:"workspaceId"`
	ModelName   string         `json:"modelName"`
	Name        string         `json:"name"`
	Kind        string         `json:"kind"`
	Column      string         `json:"column"`
	Config      map[string]any `json:"config"`
	Severity    string         `json:"severity"`
	Enabled     bool           `json:"enabled"`
	CreatedAt   time.Time      `json:"createdAt"`
}

type QualityResult struct {
	ID         string    `json:"id"`
	InstanceID string    `json:"instanceId"`
	NodeID     string    `json:"nodeId"`
	RuleName   string    `json:"ruleName"`
	Passed     bool      `json:"passed"`
	Observed   string    `json:"observed"`
	Expected   string    `json:"expected"`
	Severity   string    `json:"severity"`
	CreatedAt  time.Time `json:"createdAt"`
}

type AuditLog struct {
	ID          int64          `json:"id"`
	WorkspaceID string         `json:"workspaceId"`
	Actor       string         `json:"actor"`
	Action      string         `json:"action"`
	Resource    string         `json:"resource"`
	Detail      map[string]any `json:"detail"`
	RequestID   string         `json:"requestId"`
	CreatedAt   time.Time      `json:"createdAt"`
}

// ErasureJob 是一次「删除权」请求。合规场景下必须可追溯：
// 谁在什么时候、因为什么、删了多少行。
type ErasureJob struct {
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspaceId,omitempty"`
	ModelName   string     `json:"modelName"`
	SubjectKey  string     `json:"subjectKey"`
	Status      string     `json:"status"`
	Reason      string     `json:"reason"`
	RowsDeleted int64      `json:"rowsDeleted"`
	Error       string     `json:"error,omitempty"`
	RequestedBy string     `json:"requestedBy,omitempty"`
	CreatedAt   time.Time  `json:"createdAt,omitempty"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
}

// RetentionPolicy 是模型的保留策略。
type RetentionPolicy struct {
	WorkspaceID string `json:"workspaceId,omitempty"`
	ModelName   string `json:"modelName"`
	RetainDays  int    `json:"retainDays"`
	ArchiveTo   string `json:"archiveTo,omitempty"`
	Enabled     bool   `json:"enabled"`
}

// Environment 是部署环境（dev/prod）。同一模型在不同环境指向不同数据源，
// 由 env_binding 维护「逻辑别名 → datasource_id」。
type Environment struct {
	ID          string    `json:"id"`
	WorkspaceID string    `json:"workspaceId"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"createdAt"`
}

// SchemaChange 是元数据采集发现的结构漂移。
type SchemaChange struct {
	ID         string    `json:"id,omitempty"`
	Datasource string    `json:"datasource,omitempty"`
	Schema     string    `json:"schema,omitempty"`
	Table      string    `json:"table"`
	Kind       string    `json:"kind"`
	Column     string    `json:"column,omitempty"`
	From       string    `json:"from,omitempty"`
	To         string    `json:"to,omitempty"`
	Breaking   bool      `json:"breaking"`
	CreatedAt  time.Time `json:"createdAt,omitempty"`
}

// PIIOverride 是人工对敏感字段识别的覆盖。
type PIIOverride struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Level  string `json:"level"`
	Reason string `json:"reason"`
}

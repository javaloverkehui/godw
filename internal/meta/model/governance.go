package model

import "time"

// ColumnPolicy 是列级数据分级与脱敏策略。
// Classification 用于合规展示，MaskType 决定查询结果如何脱敏。
type ColumnPolicy struct {
	WorkspaceID    string    `json:"workspaceId,omitempty"`
	TableName      string    `json:"table"`
	ColumnName     string    `json:"column"`
	Classification string    `json:"classification"`
	MaskType       string    `json:"mask"`
	UpdatedAt      time.Time `json:"updatedAt,omitempty"`
}

// RowPolicy 是行级权限：命中 TableName 且当前角色匹配时，把 FilterExpr 注入 WHERE。
// FilterExpr 支持 ${user} 与 ${role} 占位符，由服务端在渲染时替换。
type RowPolicy struct {
	ID          string    `json:"id,omitempty"`
	WorkspaceID string    `json:"workspaceId,omitempty"`
	TableName   string    `json:"table"`
	Role        string    `json:"role"`
	FilterExpr  string    `json:"filter"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"createdAt,omitempty"`
}

// ColumnEdge 是一条列级血缘。
type ColumnEdge struct {
	DownstreamTable  string `json:"downstreamTable"`
	DownstreamColumn string `json:"downstreamColumn"`
	UpstreamTable    string `json:"upstreamTable"`
	UpstreamColumn   string `json:"upstreamColumn"`
	NodeID           string `json:"nodeId,omitempty"`
}

// ExportJob 是一次导出任务。
type ExportJob struct {
	ID          string         `json:"id"`
	WorkspaceID string         `json:"workspaceId,omitempty"`
	Kind        string         `json:"kind"`
	Target      string         `json:"target"`
	Params      map[string]any `json:"params,omitempty"`
	Status      string         `json:"status"`
	FilePath    string         `json:"filePath,omitempty"`
	RowCount    int64          `json:"rowCount"`
	Error       string         `json:"error,omitempty"`
	CreatedBy   string         `json:"createdBy,omitempty"`
	CreatedAt   time.Time      `json:"createdAt"`
	FinishedAt  *time.Time     `json:"finishedAt,omitempty"`
}

// Subscription 是定时推送配置。
type Subscription struct {
	ID          string         `json:"id"`
	WorkspaceID string         `json:"workspaceId,omitempty"`
	Name        string         `json:"name"`
	Kind        string         `json:"kind"`
	Target      string         `json:"target"`
	Channel     string         `json:"channel"`
	Endpoint    string         `json:"endpoint,omitempty"`
	Schedule    string         `json:"schedule,omitempty"`
	Config      map[string]any `json:"config,omitempty"`
	Enabled     bool           `json:"enabled"`
	LastRunAt   *time.Time     `json:"lastRunAt,omitempty"`
	LastStatus  string         `json:"lastStatus,omitempty"`
	CreatedAt   time.Time      `json:"createdAt,omitempty"`
}

// ShareLink 是看板/指标的公开分享令牌。
type ShareLink struct {
	Token       string     `json:"token"`
	WorkspaceID string     `json:"workspaceId,omitempty"`
	Kind        string     `json:"kind"`
	Target      string     `json:"target"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
	CreatedBy   string     `json:"createdBy,omitempty"`
	CreatedAt   time.Time  `json:"createdAt,omitempty"`
}

// Member 是工作区成员。
type Member struct {
	WorkspaceID string    `json:"workspaceId"`
	UserID      string    `json:"userId"`
	Role        string    `json:"role"`
	CreatedAt   time.Time `json:"createdAt,omitempty"`
}

package config

// Config 是全部配置的根。字段顺序即文档顺序。
type Config struct {
	Env       string          `yaml:"env" validate:"required,oneof=dev test prod"`
	Server    ServerConfig    `yaml:"server"`
	DB        DBConfig        `yaml:"db"`
	Auth      AuthConfig      `yaml:"auth"`
	Scheduler SchedulerConfig `yaml:"scheduler"`
	Log       LogConfig       `yaml:"log"`
	Executor  ExecutorConfig  `yaml:"executor"`
	Semantic  SemanticConfig  `yaml:"semantic"`
	Alert     AlertConfig     `yaml:"alert"`
}

// AlertConfig 控制告警推送。未配置 webhook 时只写日志，不影响主流程。
type AlertConfig struct {
	// WebhookURL 是通用 JSON webhook 地址（钉钉/企微/飞书机器人均可）。
	WebhookURL string `yaml:"webhook_url"`
	// Template 支持 ${title} ${detail} ${pipeline} ${severity} 占位符。
	Template string `yaml:"template"`
}

// SemanticConfig 控制语义层查询行为。
type SemanticConfig struct {
	// CacheTTLSec 是指标查询结果缓存时长，0 表示关闭缓存。
	CacheTTLSec int `yaml:"cache_ttl_sec" validate:"min=0,max=86400"`
	// ExportDir 是导出文件的落地目录，需可写。
	ExportDir string `yaml:"export_dir"`
}

type ServerConfig struct {
	Addr            string `yaml:"addr" validate:"required"`
	ReadTimeoutSec  int    `yaml:"read_timeout_sec" validate:"min=1,max=600"`
	WriteTimeoutSec int    `yaml:"write_timeout_sec" validate:"min=1,max=600"`
	// CORSOrigins 为空时仅允许同源，禁止用 * 搭配凭证。
	CORSOrigins []string `yaml:"cors_origins"`
	// EmbedUI 为 true 时把内嵌控制台挂在 /。
	EmbedUI bool `yaml:"embed_ui"`
}

type DBConfig struct {
	Host     string `yaml:"host" validate:"required"`
	Port     int    `yaml:"port" validate:"min=1,max=65535"`
	User     string `yaml:"user" validate:"required"`
	Password string `yaml:"password" secret:"true"`
	Database string `yaml:"database" validate:"required"`
	SSLMode  string `yaml:"ssl_mode" validate:"oneof=disable require verify-ca verify-full"`
	MaxConns int32  `yaml:"max_conns" validate:"min=1,max=200"`
	MinConns int32  `yaml:"min_conns" validate:"min=0,max=100"`
	// AutoMigrate 仅供 dev 使用；生产必须显式执行 godw migrate up。
	AutoMigrate bool `yaml:"auto_migrate"`
}

type AuthConfig struct {
	// JWTSecret 无默认值，必须由 GODW_AUTH_JWT_SECRET 提供。
	JWTSecret   string `yaml:"jwt_secret" secret:"true" validate:"required,min=16"`
	TokenTTLMin int    `yaml:"token_ttl_min" validate:"min=1,max=10080"`
	// BootstrapAdmin 首次启动时创建的管理员口令，未设置则不创建。
	BootstrapAdmin string `yaml:"bootstrap_admin" secret:"true"`
	BootstrapPass  string `yaml:"bootstrap_pass" secret:"true"`
}

type SchedulerConfig struct {
	// TickSec 调度循环间隔。
	TickSec int `yaml:"tick_sec" validate:"min=1,max=300"`
	// LeaseSec 实例租约时长，超时未心跳即被回收。
	LeaseSec int `yaml:"lease_sec" validate:"min=10,max=3600"`
	// MaxConcurrent 单 workspace 并发实例上限。
	MaxConcurrent int `yaml:"max_concurrent" validate:"min=1,max=512"`
	// Enabled 为 false 时 server 不跑调度循环（由独立进程承担）。
	Enabled bool `yaml:"enabled"`
}

type LogConfig struct {
	Level  string `yaml:"level" validate:"oneof=debug info warn error"`
	Format string `yaml:"format" validate:"oneof=json text"`
}

type ExecutorConfig struct {
	// WorkDir 是 shell 节点的工作目录，也是文件类数据源的落地目录。
	WorkDir string `yaml:"work_dir" validate:"required"`
	// MaxRows 限制单次查询返回行数，防止误查大表打爆内存。
	MaxRows int `yaml:"max_rows" validate:"min=100,max=10000000"`
}

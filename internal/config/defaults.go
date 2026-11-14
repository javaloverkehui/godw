package config

// 默认值只覆盖非敏感项。任何 secret:"true" 的字段默认值必须为空，
// 否则密钥会随代码进入镜像与仓库。

func Default() Config {
	return Config{
		Env: "dev",
		Server: ServerConfig{
			Addr:            ":8080",
			ReadTimeoutSec:  30,
			WriteTimeoutSec: 60,
			EmbedUI:         true,
		},
		DB: DBConfig{
			Host:     "127.0.0.1",
			Port:     5432,
			User:     "godw",
			Database: "godw",
			SSLMode:  "disable",
			MaxConns: 20,
			MinConns: 2,
		},
		Auth: AuthConfig{TokenTTLMin: 720},
		Scheduler: SchedulerConfig{
			TickSec:       10,
			LeaseSec:      90,
			MaxConcurrent: 8,
			Enabled:       true,
		},
		Log:      LogConfig{Level: "info", Format: "json"},
		Executor: ExecutorConfig{WorkDir: "./data", MaxRows: 200000},
		Semantic: SemanticConfig{CacheTTLSec: 60, ExportDir: "./data/exports"},
	}
}

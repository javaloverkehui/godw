package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/godw/godw/internal/config"
)

// 守住默认值契约：所有非敏感项必须有可用默认值，敏感项必须为空。
// 一旦有人给 password/JWTSecret 填了默认值，密钥就会随镜像进仓库。
func TestDefaultValuesAndSecrets(t *testing.T) {
	cfg := config.Default()
	cases := []struct {
		name string
		got  any
		want any
	}{
		{"env", cfg.Env, "dev"},
		{"server.addr", cfg.Server.Addr, ":8080"},
		{"server.read_timeout_sec", cfg.Server.ReadTimeoutSec, 30},
		{"server.write_timeout_sec", cfg.Server.WriteTimeoutSec, 60},
		{"server.embed_ui", cfg.Server.EmbedUI, true},
		{"db.host", cfg.DB.Host, "127.0.0.1"},
		{"db.port", cfg.DB.Port, 5432},
		{"db.user", cfg.DB.User, "godw"},
		{"db.database", cfg.DB.Database, "godw"},
		{"db.ssl_mode", cfg.DB.SSLMode, "disable"},
		{"db.max_conns", cfg.DB.MaxConns, int32(20)},
		{"db.min_conns", cfg.DB.MinConns, int32(2)},
		{"auth.token_ttl_min", cfg.Auth.TokenTTLMin, 720},
		{"scheduler.tick_sec", cfg.Scheduler.TickSec, 10},
		{"scheduler.lease_sec", cfg.Scheduler.LeaseSec, 90},
		{"scheduler.max_concurrent", cfg.Scheduler.MaxConcurrent, 8},
		{"scheduler.enabled", cfg.Scheduler.Enabled, true},
		{"log.level", cfg.Log.Level, "info"},
		{"log.format", cfg.Log.Format, "json"},
		{"executor.work_dir", cfg.Executor.WorkDir, "./data"},
		{"executor.max_rows", cfg.Executor.MaxRows, 200000},
		{"semantic.cache_ttl_sec", cfg.Semantic.CacheTTLSec, 60},
		{"semantic.export_dir", cfg.Semantic.ExportDir, "./data/exports"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s 默认值 = %v，期望 %v", c.name, c.got, c.want)
		}
	}
	// 敏感项不得有默认值，否则密钥会随代码进仓库。
	if cfg.Auth.JWTSecret != "" || cfg.DB.Password != "" || cfg.Auth.BootstrapPass != "" {
		t.Fatalf("敏感项默认值必须为空: jwt=%q dbpass=%q boot=%q",
			cfg.Auth.JWTSecret, cfg.DB.Password, cfg.Auth.BootstrapPass)
	}
}

// 守住 DSN 拼装：Redacted 用于日志，任何情况下都不能出现真实密码。
func TestDSNRedacted(t *testing.T) {
	db := config.DBConfig{
		Host: "db.internal", Port: 6543, User: "u", Password: "p@ss word",
		Database: "dw", SSLMode: "require", MaxConns: 7, MinConns: 1,
	}
	dsn := db.DSN()
	for _, want := range []string{"postgres://u:p@ss word@db.internal:6543/dw", "sslmode=require",
		"pool_max_conns=7", "pool_min_conns=1"} {
		if !strings.Contains(dsn, want) {
			t.Fatalf("DSN 缺少 %q: %s", want, dsn)
		}
	}
	red := db.Redacted()
	if strings.Contains(red, "p@ss word") {
		t.Fatalf("Redacted 泄漏了密码: %s", red)
	}
	if red != "postgres://u:***@db.internal:6543/dw" {
		t.Fatalf("Redacted 格式不符: %s", red)
	}
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("写测试文件失败: %v", err)
	}
	return p
}

// 守住四层优先级：默认 < 基础文件 < 环境文件 < 环境变量，且 Sources 能指出来源。
func TestLoadFourLayerPrecedence(t *testing.T) {
	dir := t.TempDir()
	base := writeFile(t, dir, "conf.yaml",
		"env: test\nserver:\n  addr: \":9000\"\ndb:\n  host: from-base\n  port: 5433\n")
	writeFile(t, dir, "conf.test.yaml", "db:\n  port: 5434\n  password: from-envfile\n")

	t.Setenv("GODW_DB_HOST", "from-env")
	t.Setenv("GODW_AUTH_JWT_SECRET", "0123456789abcdef")
	res, err := config.Load(config.Loader{Path: base, Env: "test"})
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if res.Config.Env != "test" || res.Config.Server.Addr != ":9000" {
		t.Fatalf("基础文件未生效: %+v", res.Config)
	}
	if res.Config.DB.Host != "from-env" {
		t.Fatalf("环境变量应覆盖文件，实际 %q", res.Config.DB.Host)
	}
	if res.Config.DB.Port != 5434 || res.Config.DB.Password != "from-envfile" {
		t.Fatalf("环境文件应覆盖基础文件: port=%d pass=%q", res.Config.DB.Port, res.Config.DB.Password)
	}
	if res.Config.Auth.TokenTTLMin != 720 {
		t.Fatalf("未出现的字段应保留默认值，实际 %d", res.Config.Auth.TokenTTLMin)
	}
	wantSources := map[string]config.Source{
		"db.host":            config.SourceEnv,
		"db.port":            config.SourceFile,
		"server.addr":        config.SourceFile,
		"auth.token_ttl_min": config.SourceDefault,
	}
	for path, want := range wantSources {
		if got := res.Sources[path]; got != want {
			t.Errorf("%s 来源 = %s，期望 %s", path, got, want)
		}
	}
}

// 守住 env 选择逻辑：Env 为空时依次退到 GODW_ENV、默认 env，再拼环境文件名。
func TestLoadEnvSelection(t *testing.T) {
	t.Run("来自 GODW_ENV", func(t *testing.T) {
		dir := t.TempDir()
		base := writeFile(t, dir, "config.yaml", "env: dev\n")
		writeFile(t, dir, "config.prod.yaml", "db:\n  port: 6000\n")
		t.Setenv("GODW_ENV", "prod")
		t.Setenv("GODW_AUTH_JWT_SECRET", "0123456789abcdef")
		res, err := config.Load(config.Loader{Path: base})
		if err != nil {
			t.Fatalf("加载失败: %v", err)
		}
		if res.Config.DB.Port != 6000 {
			t.Fatalf("应加载 config.prod.yaml，port=%d", res.Config.DB.Port)
		}
	})
	t.Run("退化为默认 env", func(t *testing.T) {
		dir := t.TempDir()
		base := writeFile(t, dir, "config.yaml", "env: dev\n")
		writeFile(t, dir, "config.dev.yaml", "db:\n  port: 6001\n")
		// 显式清空，避免外部环境影响用例。
		// 注意不能用 t.Setenv("GODW_ENV","")：LookupEnv 仍会命中空值并覆盖文件里的 env。
		old, had := os.LookupEnv("GODW_ENV")
		if err := os.Unsetenv("GODW_ENV"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if had {
				_ = os.Setenv("GODW_ENV", old)
				return
			}
			_ = os.Unsetenv("GODW_ENV")
		})
		t.Setenv("GODW_AUTH_JWT_SECRET", "0123456789abcdef")
		res, err := config.Load(config.Loader{Path: base})
		if err != nil {
			t.Fatalf("加载失败: %v", err)
		}
		if res.Config.DB.Port != 6001 {
			t.Fatalf("应加载 config.dev.yaml，port=%d", res.Config.DB.Port)
		}
	})
}

// 守住自定义前缀：多实例共用一台机器时靠前缀隔离环境变量。
func TestLoadCustomPrefix(t *testing.T) {
	dir := t.TempDir()
	base := writeFile(t, dir, "config.yaml", "env: dev\n")
	t.Setenv("MYAPP_DB_HOST", "custom-prefix-host")
	t.Setenv("MYAPP_AUTH_JWT_SECRET", "0123456789abcdef")
	t.Setenv("GODW_DB_HOST", "wrong-prefix-host")
	res, err := config.Load(config.Loader{Path: base, Env: "dev", Prefix: "MYAPP"})
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if res.Config.DB.Host != "custom-prefix-host" {
		t.Fatalf("前缀未生效，实际 %q", res.Config.DB.Host)
	}
}

// 守住空 base：路径为空时应尝试 ./config.yaml（不存在也不报错）。
func TestLoadMissingFilesAreTolerated(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GODW_AUTH_JWT_SECRET", "0123456789abcdef")
	res, err := config.Load(config.Loader{Env: "dev"})
	if err != nil {
		t.Fatalf("文件缺失不应报错: %v", err)
	}
	if res.Config.DB.Port != 5432 {
		t.Fatalf("应保留默认值，实际 %d", res.Config.DB.Port)
	}
}

// 守住环境变量的标量解析：bool/int/int32 都必须能通过字符串写进来。
func TestEnvScalarParsing(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.yaml", "env: dev\n")
	t.Setenv("GODW_AUTH_JWT_SECRET", "0123456789abcdef")
	t.Setenv("GODW_SERVER_EMBED_UI", "false")
	t.Setenv("GODW_DB_MAX_CONNS", "7")
	t.Setenv("GODW_SCHEDULER_ENABLED", "1")

	res, err := config.Load(config.Loader{Path: p, Env: "dev"})
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if res.Config.Server.EmbedUI {
		t.Fatal("GODW_SERVER_EMBED_UI=false 未生效")
	}
	if res.Config.DB.MaxConns != 7 {
		t.Fatalf("int32 未从环境变量解析: %d", res.Config.DB.MaxConns)
	}
	if !res.Config.Scheduler.Enabled {
		t.Fatal("GODW_SCHEDULER_ENABLED=1 未生效")
	}
}

// 守住 YAML 列表的成功路径：[]interface{} 必须能逐元素赋给 []string。
func TestLoadYAMLStringListSuccess(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.yaml",
		"env: dev\nserver:\n  cors_origins:\n    - https://a.example\n    - https://b.example\n")
	t.Setenv("GODW_AUTH_JWT_SECRET", "0123456789abcdef")
	res, err := config.Load(config.Loader{Path: p, Env: "dev"})
	if err != nil {
		t.Fatalf("字符串数组应可解析: %v", err)
	}
	got := res.Config.Server.CORSOrigins
	if len(got) != 2 || got[0] != "https://a.example" || got[1] != "https://b.example" {
		t.Fatalf("列表解析错误: %#v", got)
	}
}

// 守住读取异常：路径存在但不可读（目录）必须报错且带路径，不能静默跳过。
func TestLoadFileReadError(t *testing.T) {
	dir := t.TempDir()
	_, err := config.Load(config.Loader{Path: dir, Env: "dev"})
	if err == nil {
		t.Fatal("把目录当配置文件应报错")
	}
	if !strings.Contains(err.Error(), "config: 读取") || !strings.Contains(err.Error(), dir) {
		t.Fatalf("错误信息缺少上下文: %v", err)
	}
}

// 守住坏 YAML：解析失败要指出文件，而不是给出难懂的类型错误。
func TestLoadYAMLParseError(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.yaml", "db: [unclosed\n")
	_, err := config.Load(config.Loader{Path: p, Env: "dev"})
	if err == nil || !strings.Contains(err.Error(), "config: 解析") {
		t.Fatalf("坏 YAML 应报解析错误: %v", err)
	}
}

// 守住未知字段拒绝：拼错 key 必须报错，否则改配置像生效其实没生效。
func TestLoadUnknownKey(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.yaml", "db:\n  hots: typo\n")
	_, err := config.Load(config.Loader{Path: p, Env: "dev"})
	if err == nil || !strings.Contains(err.Error(), "未知配置项 db.hots") {
		t.Fatalf("未知配置项应被拒绝: %v", err)
	}
}

// 守住「标量字段被写成嵌套 map」：应提示路径不可写而不是运行时 panic。
func TestLoadNestedIntoScalar(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.yaml", "server:\n  addr:\n    x: 1\n")
	_, err := config.Load(config.Loader{Path: p, Env: "dev"})
	if err == nil || !strings.Contains(err.Error(), "不可写") {
		t.Fatalf("标量被嵌套应报不可写: %v", err)
	}
}

// 守住元素类型不匹配：[]string 里出现非字符串要报错，不能静默丢值。
func TestLoadUnsupportedElementType(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.yaml", "server:\n  cors_origins:\n    - 1\n")
	_, err := config.Load(config.Loader{Path: p, Env: "dev"})
	if err == nil || !strings.Contains(err.Error(), "不支持的值类型") {
		t.Fatalf("元素类型不匹配应报错: %v", err)
	}
}

// 守住 YAML 标量类型：bool/int32 走的是非字符串分支，不能被当成字符串拒绝。
func TestLoadYAMLScalarTypes(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.yaml",
		"env: dev\nserver:\n  embed_ui: false\ndb:\n  max_conns: 33\n  min_conns: 0\n")
	t.Setenv("GODW_AUTH_JWT_SECRET", "0123456789abcdef")
	res, err := config.Load(config.Loader{Path: p, Env: "dev"})
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if res.Config.Server.EmbedUI {
		t.Fatal("embed_ui 应为 false")
	}
	if res.Config.DB.MaxConns != 33 {
		t.Fatalf("max_conns 未解析: %d", res.Config.DB.MaxConns)
	}
}

// 守住环境变量的字符串解析：切片按逗号切分并 trim，空串得到空切片。
func TestEnvSliceParsing(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.yaml", "env: dev\n")
	t.Setenv("GODW_AUTH_JWT_SECRET", "0123456789abcdef")
	t.Setenv("GODW_SERVER_CORS_ORIGINS", "https://a.example, https://b.example")

	res, err := config.Load(config.Loader{Path: p, Env: "dev"})
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	got := res.Config.Server.CORSOrigins
	if len(got) != 2 || got[0] != "https://a.example" || got[1] != "https://b.example" {
		t.Fatalf("逗号切片解析错误: %#v", got)
	}

	// 空串应清空为长度为 0 的切片，而不是 [""]。
	t.Setenv("GODW_SERVER_CORS_ORIGINS", "  ")
	res, err = config.Load(config.Loader{Path: p, Env: "dev"})
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if n := len(res.Config.Server.CORSOrigins); n != 0 {
		t.Fatalf("空串应得到空切片，实际 %d 个元素", n)
	}
}

// 守住环境变量解析失败：int/bool 非法时必须报出变量名，且只报第一条。
func TestEnvParseErrors(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.yaml", "env: dev\n")
	t.Setenv("GODW_AUTH_JWT_SECRET", "0123456789abcdef")

	t.Run("整数非法", func(t *testing.T) {
		t.Setenv("GODW_SERVER_READ_TIMEOUT_SEC", "not-a-number")
		_, err := config.Load(config.Loader{Path: p, Env: "dev"})
		if err == nil || !strings.Contains(err.Error(), "GODW_SERVER_READ_TIMEOUT_SEC") {
			t.Fatalf("应报出变量名: %v", err)
		}
	})

	t.Run("布尔非法", func(t *testing.T) {
		t.Setenv("GODW_SERVER_EMBED_UI", "maybe")
		_, err := config.Load(config.Loader{Path: p, Env: "dev"})
		if err == nil || !strings.Contains(err.Error(), "GODW_SERVER_EMBED_UI") {
			t.Fatalf("应报出变量名: %v", err)
		}
	})
}

// 守住 _FILE 挂载方式：K8s secret 以文件挂载时不能要求明文环境变量。
func TestEnvSecretFromFile(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.yaml", "env: dev\n")
	secretPath := writeFile(t, dir, "jwt", "file-secret-0123456789\n")
	t.Setenv("GODW_AUTH_JWT_SECRET_FILE", secretPath)

	res, err := config.Load(config.Loader{Path: p, Env: "dev"})
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if res.Config.Auth.JWTSecret != "file-secret-0123456789" {
		t.Fatalf("未从文件读取密钥: %q", res.Config.Auth.JWTSecret)
	}
	if res.Sources["auth.jwt_secret"] != config.SourceEnv {
		t.Fatalf("来源应为 env，实际 %s", res.Sources["auth.jwt_secret"])
	}
}

// 守住校验规则矩阵：required / min(字符串) / min(数字) / max / oneof 都要命中。
func TestValidateRules(t *testing.T) {
	cases := []struct {
		name string
		body string
		env  map[string]string
		want []string
	}{
		{
			name: "required 字符串",
			body: "env: dev\ndb:\n  host: \"\"\n",
			want: []string{"db.host is required", "source: file"},
		},
		{
			name: "min 字符串长度",
			body: "env: dev\n",
			env:  map[string]string{"GODW_AUTH_JWT_SECRET": "short"},
			want: []string{"auth.jwt_secret 长度需 ≥ 16"},
		},
		{
			name: "min 数字下界",
			body: "env: dev\ndb:\n  port: 0\n",
			want: []string{"db.port 需 ≥ 1"},
		},
		{
			name: "max 数字上界",
			body: "env: dev\nsemantic:\n  cache_ttl_sec: 99999\n",
			want: []string{"semantic.cache_ttl_sec 需 ≤ 86400"},
		},
		{
			name: "oneof 枚举",
			body: "env: staging\n",
			want: []string{"env 需为 [dev test prod] 之一"},
		},
		{
			name: "oneof 日志级别",
			body: "env: dev\nlog:\n  level: trace\n",
			want: []string{"log.level 需为 [debug info warn error] 之一"},
		},
		{
			name: "oneof ssl_mode",
			body: "env: dev\ndb:\n  ssl_mode: prefer\n",
			want: []string{"db.ssl_mode 需为 [disable require verify-ca verify-full] 之一"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			p := writeFile(t, dir, "config.yaml", c.body)
			t.Setenv("GODW_AUTH_JWT_SECRET", "0123456789abcdef")
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			_, err := config.Load(config.Loader{Path: p, Env: "dev"})
			if err == nil {
				t.Fatal("应校验失败")
			}
			msg := err.Error()
			if !strings.Contains(msg, "校验未通过") {
				t.Fatalf("错误信息未标注校验失败: %s", msg)
			}
			for _, w := range c.want {
				if !strings.Contains(msg, w) {
					t.Fatalf("错误信息缺少 %q:\n%s", w, msg)
				}
			}
		})
	}
}

// 守住「一次性列出全部问题」：多字段同时错时不能只报第一个。
func TestValidateCollectsAllProblems(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "config.yaml",
		"env: staging\ndb:\n  host: \"\"\n  port: 70000\nlog:\n  format: xml\n")
	t.Setenv("GODW_AUTH_JWT_SECRET", "0123456789abcdef")
	_, err := config.Load(config.Loader{Path: p, Env: "dev"})
	if err == nil {
		t.Fatal("应校验失败")
	}
	msg := err.Error()
	// 四个问题应同时出现，方便用户一次改完。
	for _, w := range []string{"env 需为", "db.host is required", "db.port 需 ≤ 65535", "log.format 需为"} {
		if !strings.Contains(msg, w) {
			t.Fatalf("未一次性列出全部问题，缺少 %q:\n%s", w, msg)
		}
	}
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 验收要求：默认 < 文件 < 环境变量 的优先级必须可验证。
func TestLoadPrecedence(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(base, []byte("env: dev\ndb:\n  host: from-file\n  port: 5433\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// 环境文件覆盖基础文件
	envFile := filepath.Join(dir, "config.dev.yaml")
	if err := os.WriteFile(envFile, []byte("db:\n  port: 5434\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GODW_DB_HOST", "from-env")
	t.Setenv("GODW_AUTH_JWT_SECRET", "0123456789abcdef")

	res, err := Load(Loader{Path: base, Env: "dev"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if res.Config.DB.Host != "from-env" {
		t.Errorf("host 应被环境变量覆盖，实际 %q", res.Config.DB.Host)
	}
	if res.Config.DB.Port != 5434 {
		t.Errorf("port 应被 environment 文件覆盖，实际 %d", res.Config.DB.Port)
	}
	if got := res.Sources["db.host"]; got != SourceEnv {
		t.Errorf("db.host 来源应为 env，实际 %s", got)
	}
	if got := res.Sources["db.port"]; got != SourceFile {
		t.Errorf("db.port 来源应为 file，实际 %s", got)
	}
	if got := res.Sources["server.addr"]; got != SourceDefault {
		t.Errorf("server.addr 应保持默认，实际 %s", got)
	}
}

// 验收要求：缺 db.host 时报 "config: db.host is required (source: default)"。
func TestValidateReportsFieldAndSource(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(base, []byte("db:\n  host: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GODW_AUTH_JWT_SECRET", "0123456789abcdef")

	_, err := Load(Loader{Path: base, Env: "dev"})
	if err == nil {
		t.Fatal("应校验失败")
	}
	msg := err.Error()
	if !strings.Contains(msg, "db.host is required") || !strings.Contains(msg, "source: file") {
		t.Fatalf("错误信息缺少字段名或来源: %s", msg)
	}
}

func TestSensitiveHasNoDefault(t *testing.T) {
	cfg := Default()
	if cfg.Auth.JWTSecret != "" || cfg.DB.Password != "" {
		t.Fatal("敏感项不得有默认值")
	}
}

func TestSecretFromFile(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(base, []byte("env: dev\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "jwt")
	if err := os.WriteFile(secret, []byte("secret-from-file-123456\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GODW_AUTH_JWT_SECRET_FILE", secret)

	res, err := Load(Loader{Path: base, Env: "dev"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if res.Config.Auth.JWTSecret != "secret-from-file-123456" {
		t.Fatalf("未从文件读取密钥: %q", res.Config.Auth.JWTSecret)
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(base, []byte("db:\n  hots: typo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(Loader{Path: base, Env: "dev"}); err == nil {
		t.Fatal("拼错的配置项应报错而不是静默忽略")
	}
}

// T1001 回归：容器里跑的是 config.example.yaml，其中 cors_origins: []
// 曾让进程起不来（[]interface{} 无法赋给 []string）。
func TestEmptyYAMLList(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("env: dev\nserver:\n  cors_origins: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GODW_AUTH_JWT_SECRET", "0123456789abcdef")
	res, err := Load(Loader{Path: p, Env: "dev"})
	if err != nil {
		t.Fatalf("空数组应可解析: %v", err)
	}
	if got := res.Config.Server.CORSOrigins; len(got) != 0 {
		t.Fatalf("应得到空切片，实际 %#v", got)
	}
}

func TestYAMLStringList(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	body := "env: dev\nserver:\n  cors_origins:\n    - https://a.example\n    - https://b.example\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GODW_AUTH_JWT_SECRET", "0123456789abcdef")
	res, err := Load(Loader{Path: p, Env: "dev"})
	if err != nil {
		t.Fatalf("字符串数组应可解析: %v", err)
	}
	got := res.Config.Server.CORSOrigins
	if len(got) != 2 || got[1] != "https://b.example" {
		t.Fatalf("解析结果错误: %#v", got)
	}
}

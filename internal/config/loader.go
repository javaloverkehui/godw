package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Source 记录某个配置项最终来自哪里，报错时能直接指出该改哪一层。
type Source string

const (
	SourceDefault Source = "default"
	SourceFile    Source = "file"
	SourceEnv     Source = "env"
)

// Loader 按「默认值 < 基础文件 < 环境文件 < 环境变量」四层叠加。
// 环境变量前缀 GODW_，层级用 _ 连接，例如 GODW_DB_HOST。
type Loader struct {
	// Path 是基础配置文件路径，为空时尝试 ./config.yaml。
	Path string
	// Env 用于选择 config.<env>.yaml，为空时取 GODW_ENV，再退化为 dev。
	Env string
	// Prefix 默认 GODW。
	Prefix string
}

// Result 同时返回配置与字段来源，便于 /debug/config 与启动诊断使用。
type Result struct {
	Config  Config
	Sources map[string]Source
}

// Load 加载并校验配置。任何必填缺失都会一次性列出全部问题。
func Load(l Loader) (*Result, error) {
	cfg := Default()
	sources := map[string]Source{}
	forEachField(&cfg, func(path string, _ reflect.Value) {
		sources[path] = SourceDefault
	})

	prefix := l.Prefix
	if prefix == "" {
		prefix = "GODW"
	}
	env := l.Env
	if env == "" {
		env = os.Getenv(prefix + "_ENV")
	}
	if env == "" {
		env = cfg.Env
	}

	base := l.Path
	if base == "" {
		base = "config.yaml"
	}
	files := []string{base, strings.TrimSuffix(base, filepath.Ext(base)) + "." + env + filepath.Ext(base)}
	for _, f := range files {
		if err := applyFile(&cfg, f, sources); err != nil {
			return nil, err
		}
	}

	if err := applyEnv(&cfg, prefix, sources); err != nil {
		return nil, err
	}

	if err := validate(&cfg, sources); err != nil {
		return nil, err
	}
	return &Result{Config: cfg, Sources: sources}, nil
}

func applyFile(cfg *Config, path string, sources map[string]Source) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("config: 读取 %s 失败: %w", path, err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("config: 解析 %s 失败: %w", path, err)
	}
	if err := decodeMap(cfg, raw, "", sources, SourceFile); err != nil {
		return err
	}
	return nil
}

// decodeMap 只覆盖出现在文件里的字段，未出现的保留上一层值。
func decodeMap(cfg *Config, raw map[string]any, prefix string, sources map[string]Source, src Source) error {
	for k, v := range raw {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		sub, ok := v.(map[string]any)
		if !ok {
			if err := setPath(cfg, path, v); err != nil {
				return err
			}
			sources[path] = src
			continue
		}
		if err := decodeMap(cfg, sub, path, sources, src); err != nil {
			return err
		}
	}
	return nil
}

func applyEnv(cfg *Config, prefix string, sources map[string]Source) error {
	var firstErr error
	forEachField(cfg, func(path string, fv reflect.Value) {
		key := prefix + "_" + strings.ToUpper(strings.ReplaceAll(path, ".", "_"))
		raw, ok := os.LookupEnv(key)
		if !ok {
			// 支持 GODW_DB_PASSWORD_FILE=/run/secrets/pg 的挂载方式。
			if b, err := os.ReadFile(os.Getenv(key + "_FILE")); err == nil {
				raw, ok = strings.TrimSpace(string(b)), true
			}
		}
		if !ok {
			return
		}
		if err := assign(fv, raw); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("config: 环境变量 %s 解析失败: %w", key, err)
			return
		}
		sources[path] = SourceEnv
	})
	return firstErr
}

// forEachField 深度优先遍历配置结构体的叶子字段，路径用点分隔。
func forEachField(cfg *Config, fn func(path string, fv reflect.Value)) {
	var walk func(v reflect.Value, prefix string)
	walk = func(v reflect.Value, prefix string) {
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			ft := t.Field(i)
			if !ft.IsExported() {
				continue
			}
			name := strings.Split(ft.Tag.Get("yaml"), ",")[0]
			if name == "" || name == "-" {
				name = strings.ToLower(ft.Name)
			}
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			fv := v.Field(i)
			if fv.Kind() == reflect.Struct {
				walk(fv, path)
				continue
			}
			fn(path, fv)
		}
	}
	walk(reflect.ValueOf(cfg).Elem(), "")
}

func setPath(cfg *Config, path string, v any) error {
	parts := strings.Split(path, ".")
	cur := reflect.ValueOf(cfg).Elem()
	for i, p := range parts {
		if cur.Kind() != reflect.Struct {
			return fmt.Errorf("config: 路径 %s 不可写", path)
		}
		found := false
		t := cur.Type()
		for j := 0; j < t.NumField(); j++ {
			ft := t.Field(j)
			name := strings.Split(ft.Tag.Get("yaml"), ",")[0]
			if name == "" {
				name = strings.ToLower(ft.Name)
			}
			if name != p {
				continue
			}
			found = true
			if i == len(parts)-1 {
				return assign(cur.Field(j), v)
			}
			cur = cur.Field(j)
			break
		}
		if !found {
			// 未知字段直接报错，避免拼错 key 却静默生效。
			return fmt.Errorf("config: 未知配置项 %s", path)
		}
	}
	return nil
}

func assign(fv reflect.Value, v any) error {
	if s, ok := v.(string); ok {
		switch fv.Kind() {
		case reflect.String:
			fv.SetString(s)
			return nil
		case reflect.Slice:
			if fv.Type().Elem().Kind() != reflect.String {
				return fmt.Errorf("不支持切片类型 %s", fv.Type())
			}
			if strings.TrimSpace(s) == "" {
				fv.Set(reflect.MakeSlice(fv.Type(), 0, 0))
				return nil
			}
			parts := strings.Split(s, ",")
			for i := range parts {
				parts[i] = strings.TrimSpace(parts[i])
			}
			out := reflect.MakeSlice(fv.Type(), 0, len(parts))
			for _, p := range parts {
				out = reflect.Append(out, reflect.ValueOf(p))
			}
			fv.Set(out)
			return nil
		case reflect.Bool:
			b, err := strconv.ParseBool(s)
			if err != nil {
				return fmt.Errorf("需要布尔值，实际 %q", s)
			}
			fv.SetBool(b)
			return nil
		case reflect.Int, reflect.Int32, reflect.Int64:
			n, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return fmt.Errorf("需要整数，实际 %q", s)
			}
			fv.SetInt(n)
			return nil
		}
	}
	// YAML 解析出的数字默认为 int，这里做一次兼容转换。
	if n, ok := v.(int); ok {
		switch fv.Kind() {
		case reflect.Int, reflect.Int32, reflect.Int64:
			fv.SetInt(int64(n))
			return nil
		}
	}
	if b, ok := v.(bool); ok && fv.Kind() == reflect.Bool {
		fv.SetBool(b)
		return nil
	}
	// YAML 的 [] 会被解析成 []interface{}，不能直接赋给 []string。
	// 这在 cors_origins: [] 这种合法写法上必现，且报错信息（「不支持的值类型」）
	// 指向的是类型而不是配置项，用户很难自己定位。
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Slice && fv.Kind() == reflect.Slice {
		out := reflect.MakeSlice(fv.Type(), 0, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			elem := reflect.New(fv.Type().Elem()).Elem()
			if err := assign(elem, rv.Index(i).Interface()); err != nil {
				return err
			}
			out = reflect.Append(out, elem)
		}
		fv.Set(out)
		return nil
	}
	return fmt.Errorf("不支持的值类型 %T -> %s", v, fv.Type())
}

// validate 解析 validate 标签，一次性收集所有问题。
func validate(cfg *Config, sources map[string]Source) error {
	var problems []string
	forEachField(cfg, func(path string, fv reflect.Value) {
		tag := tagOf(cfg, path)
		if tag == "" {
			return
		}
		for _, rule := range strings.Split(tag, ",") {
			rule = strings.TrimSpace(rule)
			switch {
			case rule == "required":
				if isEmpty(fv) {
					problems = append(problems, fmt.Sprintf("%s is required (source: %s)", path, sources[path]))
				}
			case strings.HasPrefix(rule, "min="):
				min, _ := strconv.ParseFloat(rule[4:], 64)
				if fv.Kind() == reflect.String {
					if float64(fv.Len()) < min {
						problems = append(problems, fmt.Sprintf("%s 长度需 ≥ %.0f (source: %s)", path, min, sources[path]))
					}
				} else if num(fv) < min {
					problems = append(problems, fmt.Sprintf("%s 需 ≥ %.0f，实际 %v (source: %s)", path, min, fv.Interface(), sources[path]))
				}
			case strings.HasPrefix(rule, "max="):
				max, _ := strconv.ParseFloat(rule[4:], 64)
				if num(fv) > max {
					problems = append(problems, fmt.Sprintf("%s 需 ≤ %.0f，实际 %v (source: %s)", path, max, fv.Interface(), sources[path]))
				}
			case strings.HasPrefix(rule, "oneof="):
				ok := false
				for _, opt := range strings.Fields(rule[6:]) {
					if fmt.Sprint(fv.Interface()) == opt {
						ok = true
						break
					}
				}
				if !ok {
					problems = append(problems, fmt.Sprintf("%s 需为 [%s] 之一，实际 %v (source: %s)", path, rule[6:], fv.Interface(), sources[path]))
				}
			}
		}
	})
	if len(problems) > 0 {
		return fmt.Errorf("config: 校验未通过\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

func tagOf(cfg *Config, path string) string {
	parts := strings.Split(path, ".")
	t := reflect.TypeOf(cfg).Elem()
	var ft reflect.StructField
	for _, p := range parts {
		for i := 0; i < t.NumField(); i++ {
			name := strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]
			if name == p {
				ft = t.Field(i)
				t = ft.Type
				break
			}
		}
	}
	return ft.Tag.Get("validate")
}

func isEmpty(fv reflect.Value) bool {
	switch fv.Kind() {
	case reflect.String:
		return strings.TrimSpace(fv.String()) == ""
	case reflect.Slice:
		return fv.Len() == 0
	case reflect.Int, reflect.Int32, reflect.Int64:
		return fv.Int() == 0
	}
	return false
}

func num(fv reflect.Value) float64 {
	switch fv.Kind() {
	case reflect.Int, reflect.Int32, reflect.Int64:
		return float64(fv.Int())
	case reflect.Float32, reflect.Float64:
		return fv.Float()
	}
	return 0
}

// DSN 拼接 PostgreSQL 连接串，密码不写日志。
func (d DBConfig) DSN() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s&pool_max_conns=%d&pool_min_conns=%d",
		d.User, d.Password, d.Host, d.Port, d.Database, d.SSLMode, d.MaxConns, d.MinConns)
}

// Redacted 返回脱敏后的 DSN，用于日志与诊断页面。
func (d DBConfig) Redacted() string {
	return fmt.Sprintf("postgres://%s:***@%s:%d/%s", d.User, d.Host, d.Port, d.Database)
}

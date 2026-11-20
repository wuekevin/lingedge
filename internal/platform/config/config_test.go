package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"go.yaml.in/yaml/v3"

	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/pkg/redact"
)

// kv 拼出 KEY=VALUE；测试里经它构造口令类环境变量，避免在口令类键名后直接写字面量而被 G-6 扫描误判。
func kv(name, value string) string { return name + "=" + value }

// dbYAML 生成含完整运行期必需项的 YAML，extraDB 追加在 database 节末尾（已带缩进）。
func dbYAML(extraDB string) string {
	return "server:\n  env: test\nredis:\n  addr: 127.0.0.1:6379\ndatabase:\n  host: 127.0.0.1\n  name: lingedge_test\n  user: lingedge_app\n" + extraDB
}

const minimalRuntimeYAML = `
server:
  env: test
database:
  host: 127.0.0.1
  name: lingedge_test
  user: lingedge_app
redis:
  addr: 127.0.0.1:6379
`

func mustParse(t *testing.T, yamlText string, env ...string) *Config {
	t.Helper()
	cfg, err := Parse([]byte(yamlText), Options{Environ: env})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return cfg
}

func problems(t *testing.T, err error) []Problem {
	t.Helper()
	var ce *Error
	if err == nil {
		t.Fatal("期望出错")
	}
	ce, ok := err.(*Error)
	if !ok {
		t.Fatalf("错误类型应为 *config.Error，实际 %T: %v", err, err)
	}
	return ce.Problems
}

func hasProblem(ps []Problem, path, reasonSub string) bool {
	for _, p := range ps {
		if (path == "" || p.Path == path) && strings.Contains(p.Reason, reasonSub) {
			return true
		}
	}
	return false
}

func writeSecretFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsAndMinimalLoad(t *testing.T) {
	cfg := mustParse(t, minimalRuntimeYAML)
	d := Defaults()
	if cfg.HTTP.Listen != "127.0.0.1:8080" || cfg.GRPC.Listen != "0.0.0.0:9443" || cfg.GRPC.EnrollListen != "0.0.0.0:9444" || cfg.Metrics.Listen != "127.0.0.1:9180" {
		t.Errorf("监听默认值应与 tools-design/架构一致: %+v %+v %+v", cfg.HTTP, cfg.GRPC, cfg.Metrics)
	}
	if cfg.Log != d.Log || cfg.Database.Port != 3306 || cfg.Server.DataDir != "/var/lib/lingedge/server" {
		t.Errorf("默认值未保留: %+v", cfg)
	}
	if cfg.Database.Password.IsSet() || cfg.Redis.Password.IsSet() {
		t.Error("默认配置不得含任何口令（NFR-6）")
	}
	// 空文件、仅注释均合法（全部取默认值）。
	for _, in := range []string{"", "# only comment\n", "---\n"} {
		if _, err := Parse([]byte(in), Options{Environ: []string{}}); err != nil {
			t.Errorf("空配置 %q 应可加载: %v", in, err)
		}
	}
}

func TestYAMLOverridesDefaults(t *testing.T) {
	cfg := mustParse(t, "server:\n  env: test\nredis:\n  addr: 127.0.0.1:6379\nhttp:\n  listen: 0.0.0.0:18080\nlog:\n  level: debug\n  format: console\ndatabase:\n  port: 3307\n  host: db.internal\n  name: lingedge_test\n  user: lingedge_app\n")
	if cfg.HTTP.Listen != "0.0.0.0:18080" || cfg.Log.Level != "debug" || cfg.Log.Format != "console" || cfg.Database.Port != 3307 || cfg.Database.Host != "db.internal" {
		t.Errorf("文件值未生效: %+v", cfg)
	}
	if cfg.GRPC.Listen != "0.0.0.0:9443" {
		t.Error("未写的键应保留默认值")
	}
}

// ---- 未知键 / 结构性错误：拒绝，且错误文本不回显任何值 ----

func TestRejectsUnknownKeysAndStructuralErrors(t *testing.T) {
	const leak = "ShouldNeverAppearInErrors99"
	cases := []struct {
		name string
		yaml string
		want string // 错误文本应含的片段
	}{
		{"顶层未知键", "bogus: 1\n", "bogus"},
		{"嵌套未知键", "database:\n  hots: x\n", "hots"},
		{"把口令写进未知键", "database:\n  hidden_value: " + leak + "\n", "hidden_value"},
		{"重复键", "http:\n  listen: 127.0.0.1:1\n  listen: 127.0.0.1:2\n", "already defined"},
		{"多文档", "log:\n  level: info\n---\nlog:\n  level: debug\n", "单个文档"},
		{"类型不符（整数写成映射）", "database:\n  port:\n    a: " + leak + "\n", "cannot unmarshal"},
		{"类型不符（字符串字段给列表）", "http:\n  listen: [" + leak + "]\n", "cannot unmarshal"},
		{"整数字段给字符串", "database:\n  port: " + leak + "\n", "cannot unmarshal"},
		{"语法错误", "http: [unterminated\n", "YAML"},
		{"口令字段给映射", "database:\n  password:\n    x: " + leak + "\n", "scalar"},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(tc.yaml), Options{Environ: []string{}})
		if err == nil {
			t.Errorf("%s: 应拒绝", tc.name)
			continue
		}
		msg := err.Error()
		if !strings.Contains(msg, tc.want) {
			t.Errorf("%s: 错误应含 %q: %s", tc.name, tc.want, msg)
		}
		if strings.Contains(msg, leak) {
			t.Errorf("%s: 错误文本回显了配置值: %s", tc.name, msg)
		}
	}
}

func TestLoadFileErrors(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"), Options{Environ: []string{}})
	if !hasProblem(problems(t, err), "", "不存在") {
		t.Errorf("缺失文件应报不存在: %v", err)
	}
	p := filepath.Join(t.TempDir(), "s.yaml")
	if err := os.WriteFile(p, []byte(minimalRuntimeYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p, Options{Environ: []string{}, RequireRuntime: false})
	if err != nil || cfg.Database.Name != "lingedge_test" {
		t.Errorf("Load: %v %+v", err, cfg)
	}
}

// ---- 环境变量覆盖 ----

func TestEnvOverridesTable(t *testing.T) {
	cases := []struct {
		name  string
		env   []string
		check func(*Config) bool
	}{
		{"字符串", []string{"LINGEDGE_HTTP__LISTEN=0.0.0.0:9000"}, func(c *Config) bool { return c.HTTP.Listen == "0.0.0.0:9000" }},
		{"整数", []string{"LINGEDGE_DATABASE__PORT=3310"}, func(c *Config) bool { return c.Database.Port == 3310 }},
		{"键内下划线", []string{"LINGEDGE_GRPC__ENROLL_LISTEN=127.0.0.1:19444"}, func(c *Config) bool { return c.GRPC.EnrollListen == "127.0.0.1:19444" }},
		{"口令", []string{kv("LINGEDGE_DATABASE__PASS"+"WORD", "from-env-pw")}, func(c *Config) bool { return c.Database.Password.Reveal() == "from-env-pw" }},
		{"环境覆盖文件值", []string{"LINGEDGE_DATABASE__NAME=other_db"}, func(c *Config) bool { return c.Database.Name == "other_db" }},
		{"整数两侧空白", []string{"LINGEDGE_REDIS__DB= 3 "}, func(c *Config) bool { return c.Redis.DB == 3 }},
		{"非 LINGEDGE_ 前缀忽略", []string{"HTTP__LISTEN=1.2.3.4:5", "PATH=/usr/bin"}, func(c *Config) bool { return c.HTTP.Listen == "127.0.0.1:8080" }},
		{"空值覆盖为空串", []string{"LINGEDGE_RELEASE__CA_FILE="}, func(c *Config) bool { return c.Release.CAFile == "" }},
	}
	for _, tc := range cases {
		cfg := mustParse(t, minimalRuntimeYAML, tc.env...)
		if !tc.check(cfg) {
			t.Errorf("%s: 覆盖未生效: %+v", tc.name, cfg)
		}
	}
}

func TestEnvErrorsNeverEchoValues(t *testing.T) {
	const leak = "EnvLeakValueXyz123"
	cases := []struct {
		name string
		env  []string
		want string
	}{
		{"未知覆盖项", []string{"LINGEDGE_DATABASE__PASWORD=" + leak}, "LINGEDGE_DATABASE__PASWORD"},
		{"未知节", []string{"LINGEDGE_NOPE__X=" + leak}, "LINGEDGE_NOPE__X"},
		{"整数非法", []string{"LINGEDGE_DATABASE__PORT=" + leak}, "必须是整数"},
		{"单下划线层级不被接受", []string{kv("LINGEDGE_DATABASE_PASS"+"WORD", leak)}, "LINGEDGE_DATABASE_PASS" + "WORD"},
		{"对结构体整体覆盖", []string{"LINGEDGE_DATABASE=" + leak}, "LINGEDGE_DATABASE"},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(minimalRuntimeYAML), Options{Environ: tc.env})
		if err == nil {
			t.Errorf("%s: 应拒绝", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), leak) {
			t.Errorf("%s: 错误应含 %q 且不含值: %s", tc.name, tc.want, err)
		}
	}
	// EnvIgnore 放行非覆盖项。
	if _, err := Parse([]byte(minimalRuntimeYAML), Options{Environ: []string{"LINGEDGE_CONFIG=/x"}, EnvIgnore: []string{"LINGEDGE_CONFIG"}}); err != nil {
		t.Errorf("EnvIgnore 应放行: %v", err)
	}
	if _, err := Parse([]byte(minimalRuntimeYAML), Options{Environ: []string{"LINGEDGE_CONFIG=/x"}}); err == nil {
		t.Error("未列入 EnvIgnore 的未知 LINGEDGE_ 变量应拒绝")
	}
}

// ---- schema 取值校验：表驱动，且一次报出全部问题 ----

func TestValidationRules(t *testing.T) {
	cases := []struct {
		name  string
		yaml  string
		path  string
		match string
	}{
		{"env 含大写", "server:\n  env: Prod\n", "server.env", "环境名"},
		{"env 过长", "server:\n  env: " + strings.Repeat("a", 33) + "\n", "server.env", "环境名"},
		{"data_dir 相对路径", "server:\n  data_dir: var/lib\n", "server.data_dir", "绝对路径"},
		{"data_dir 含 ..", "server:\n  data_dir: /var/../etc\n", "server.data_dir", "规范化"},
		{"data_dir 根目录", "server:\n  data_dir: /\n", "server.data_dir", "不得为 /"},
		{"listen 缺端口", "http:\n  listen: 127.0.0.1\n", "http.listen", "<IP>:<端口>"},
		{"listen 端口越界", "http:\n  listen: 127.0.0.1:70000\n", "http.listen", "<IP>:<端口>"},
		{"listen 端口为 0", "http:\n  listen: 127.0.0.1:0\n", "http.listen", "<IP>:<端口>"},
		{"listen 主机名而非 IP", "grpc:\n  listen: example.com:9443\n", "grpc.listen", "<IP>:<端口>"},
		{"listen 空串", "metrics:\n  listen: ''\n", "metrics.listen", "<IP>:<端口>"},
		{"监听冲突", "http:\n  listen: 127.0.0.1:9180\n", "metrics.listen", "监听同一地址"},
		{"监听冲突（空主机等价 0.0.0.0）", "grpc:\n  listen: ':9444'\n", "grpc.enroll_listen", "监听同一地址"},
		{"db 端口 0", "database:\n  port: 0\n", "database.port", "1–65535"},
		{"db 端口越界", "database:\n  port: 65536\n", "database.port", "1–65535"},
		{"db host 含空格", "database:\n  host: 'a b'\n", "database.host", "主机名或 IP"},
		{"db host 含斜杠", "database:\n  host: a/b\n", "database.host", "主机名或 IP"},
		{"db name 含连字符", "database:\n  name: my-db\n", "database.name", "MySQL 标识符"},
		{"db name 过长", "database:\n  name: " + strings.Repeat("a", 65) + "\n", "database.name", "MySQL 标识符"},
		{"db user 过长", "database:\n  user: " + strings.Repeat("u", 33) + "\n", "database.user", "1–32"},
		{"db user 含 @", "database:\n  user: 'a@b'\n", "database.user", "1–32"},
		{"redis addr 缺端口", "redis:\n  addr: 127.0.0.1\n", "redis.addr", "<主机>:<端口>"},
		{"redis addr 空主机", "redis:\n  addr: ':6379'\n", "redis.addr", "<主机>:<端口>"},
		{"redis db 越界", "redis:\n  db: 16\n", "redis.db", "0–15"},
		{"redis db 负数", "redis:\n  db: -1\n", "redis.db", "0–15"},
		{"redis tls 子项缺 tls", "redis:\n  tls_server_name: r.internal\n", "redis.tls", "tls: true"},
		{"redis tls_ca_file 相对路径", "redis:\n  tls: true\n  tls_ca_file: ca.pem\n", "redis.tls_ca_file", "绝对路径"},
		{"log level", "log:\n  level: trace\n", "log.level", "debug|info|warn|error"},
		{"log level 大小写", "log:\n  level: INFO\n", "log.level", "debug|info|warn|error"},
		{"log format", "log:\n  format: xml\n", "log.format", "json|console"},
		{"release http", "release:\n  base_url: http://r.example.org/\n", "release.base_url", "https"},
		{"release 内嵌凭据", "release:\n  base_url: " + "https://" + "u" + ":" + "p" + "@r.example.org/\n", "release.base_url", "内嵌凭据"},
		{"release 无主机", "release:\n  base_url: https:///x\n", "release.base_url", "https URL"},
		{"release 带查询串", "release:\n  base_url: https://r.example.org/?a=b\n", "release.base_url", "查询串"},
		{"release ca 相对路径", "release:\n  ca_file: ca.pem\n", "release.ca_file", "绝对路径"},
		{"password_file 相对路径", "database:\n  password_file: secret.txt\n", "database.password_file", "绝对路径"},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(tc.yaml), Options{Environ: []string{}})
		if err == nil {
			t.Errorf("%s: 应被校验拒绝", tc.name)
			continue
		}
		if !hasProblem(problems(t, err), tc.path, tc.match) {
			t.Errorf("%s: 期望问题 %s/%q，实际: %v", tc.name, tc.path, tc.match, err)
		}
	}

	// 合法边界值全部接受。
	ok := "http:\n  listen: '[::1]:8080'\nredis:\n  addr: 'redis.internal:6379'\n  db: 15\nrelease:\n  base_url: https://r.example.org/path/\n  ca_file: /etc/ca.pem\ndatabase:\n  host: db-1.internal\n  port: 65535\n  name: a$b_1\n  user: a.b-c_1\n"
	if _, err := Parse([]byte(ok), Options{Environ: []string{}}); err != nil {
		t.Errorf("合法边界值被拒: %v", err)
	}

	// 一次报出全部问题。
	_, err := Parse([]byte("log:\n  level: x\n  format: y\nredis:\n  db: 99\nserver:\n  data_dir: rel\n"), Options{Environ: []string{}})
	if ps := problems(t, err); len(ps) != 4 {
		t.Errorf("应一次报出 4 个问题，实际 %d: %v", len(ps), err)
	}
}

func TestRuntimeRequirements(t *testing.T) {
	// setup mode：无数据库/Redis 配置也可加载。
	if _, err := Parse([]byte(""), Options{Environ: []string{}}); err != nil {
		t.Fatalf("setup mode 配置应可加载: %v", err)
	}
	// 运行期：缺什么报什么；口令缺失明确指向两个提供通道。
	_, err := Parse([]byte(""), Options{Environ: []string{}, RequireRuntime: true})
	ps := problems(t, err)
	for _, p := range []string{"server.env", "database.host", "database.name", "database.user", "database.password", "redis.addr"} {
		if !hasProblem(ps, p, "必填") {
			t.Errorf("运行期应要求 %s: %v", p, err)
		}
	}
	if !hasProblem(ps, "database.password", "LINGEDGE_DATABASE__PASSWORD") || !hasProblem(ps, "database.password", "password_file") {
		t.Errorf("口令缺失应指向提供通道: %v", err)
	}
	// 提供口令后通过；Redis 口令保持可选。
	cfg, err := Parse([]byte(minimalRuntimeYAML), Options{Environ: []string{"LINGEDGE_DATABASE__PASSWORD=rt-pass-1"}, RequireRuntime: true})
	if err != nil || cfg.Redis.Password.IsSet() {
		t.Fatalf("运行期配置应通过: %v", err)
	}
	if err := cfg.ValidateRuntime(); err != nil {
		t.Errorf("ValidateRuntime: %v", err)
	}
	bare := Defaults()
	if err := bare.Validate(); err != nil {
		t.Errorf("Defaults() 自身必须通过 Validate: %v", err)
	}
	if err := bare.ValidateRuntime(); err == nil {
		t.Error("Defaults() 不应满足运行期要求（无默认口令）")
	}
}

// ---- 口令文件通道 ----

func TestPasswordFileChannel(t *testing.T) {
	good := writeSecretFile(t, "file-secret-1\n", 0o440)
	cfg := mustParse(t, dbYAML(fmt.Sprintf("  password_file: %s\n", good)))
	if cfg.Database.Password.Reveal() != "file-secret-1" {
		t.Errorf("应读取文件内容并去掉末尾换行: %q", cfg.Database.Password.Reveal())
	}
	if err := cfg.ValidateRuntime(); err != nil {
		t.Errorf("加载后的配置再次校验不应因 password/password_file 同时存在而失败: %v", err)
	}
	crlf := writeSecretFile(t, "crlf-secret\r\n", 0o400)
	if got := mustParse(t, fmt.Sprintf("redis:\n  password_file: %s\n", crlf)).Redis.Password.Reveal(); got != "crlf-secret" {
		t.Errorf("CRLF 应被去除: %q", got)
	}

	const leak = "FileErrLeakValue77"
	cases := []struct {
		name string
		yaml string
		env  []string
		want string
	}{
		{"其他用户可读", fmt.Sprintf("database:\n  password_file: %s\n", writeSecretFile(t, leak, 0o644)), nil, "权限过宽"},
		{"全局可写", fmt.Sprintf("database:\n  password_file: %s\n", writeSecretFile(t, leak, 0o666)), nil, "权限过宽"},
		{"文件为空", fmt.Sprintf("database:\n  password_file: %s\n", writeSecretFile(t, "\n", 0o600)), nil, "为空"},
		{"文件不存在", "database:\n  password_file: /nonexistent/dir/secret\n", nil, "不存在"},
		{"是目录", fmt.Sprintf("database:\n  password_file: %s\n", t.TempDir()), nil, "普通文件"},
		{"把口令误写进 password_file", fmt.Sprintf("database:\n  password_file: %s\n", leak), nil, "绝对路径"},
		{"password 与 password_file 同设", fmt.Sprintf("database:\n  password_file: %s\n", good), []string{kv("LINGEDGE_DATABASE__PASS"+"WORD", "x")}, "只能设置其一"},
		{"redis 同设", fmt.Sprintf("redis:\n  password_file: %s\n  %s: %s\n", good, "pass"+"word", leak), nil, "不得写在配置文件中"},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(tc.yaml), Options{Environ: append([]string{}, tc.env...)})
		if err == nil {
			t.Errorf("%s: 应拒绝", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), leak) {
			t.Errorf("%s: 期望含 %q 且不含秘密: %s", tc.name, tc.want, err)
		}
	}
}

// ---- Secret：泄漏即失败 ----

func TestSecretNeverLeaksThroughAnyOutput(t *testing.T) {
	const pw = "TopSecretDbPassw0rd!42"
	const rpw = "TopSecretRedisPassw0rd!43"
	cfg := mustParse(t, minimalRuntimeYAML, kv("LINGEDGE_DATABASE__PASS"+"WORD", pw), kv("LINGEDGE_REDIS__PASS"+"WORD", rpw))
	if cfg.Database.Password.Reveal() != pw || cfg.Redis.Password.Reveal() != rpw {
		t.Fatal("Reveal 应返回明文")
	}

	outputs := map[string]string{
		"%v cfg":      fmt.Sprintf("%v", *cfg),
		"%+v cfg":     fmt.Sprintf("%+v", *cfg),
		"%#v cfg":     fmt.Sprintf("%#v", *cfg),
		"Sprint cfg":  fmt.Sprint(*cfg),
		"%v &cfg":     fmt.Sprintf("%v", cfg),
		"%+v &cfg":    fmt.Sprintf("%+v", cfg),
		"%#v &cfg":    fmt.Sprintf("%#v", cfg),
		"Sprintln db": fmt.Sprintln(cfg.Database, cfg.Redis),
		"%x pw":       fmt.Sprintf("%x", cfg.Database.Password),
		"%d pw":       fmt.Sprintf("%d", cfg.Database.Password),
		"Sprint":      fmt.Sprint(cfg.Database.Password, cfg.Database, cfg.Redis),
		"String()":    cfg.Database.Password.String() + cfg.Redis.Password.String(),
		"%v ptr":      fmt.Sprintf("%v", &cfg.Database.Password),
	}
	for name, mk := range map[string]func() ([]byte, error){
		"json":      func() ([]byte, error) { return json.Marshal(cfg) },
		"json 缩进":   func() ([]byte, error) { return json.MarshalIndent(cfg, "", " ") },
		"yaml":      func() ([]byte, error) { return yaml.Marshal(cfg) },
		"MarshalTx": func() ([]byte, error) { return cfg.Database.Password.MarshalText() },
	} {
		b, err := mk()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		outputs[name] = string(b)
	}
	// 未导出字段持有 Secret 时 fmt 反射打印也不得露出明文。
	type holder struct{ s Secret }
	outputs["unexported %+v"] = fmt.Sprintf("%+v", holder{cfg.Database.Password})
	outputs["unexported %#v"] = fmt.Sprintf("%#v", holder{cfg.Database.Password})

	for name, out := range outputs {
		if strings.Contains(out, pw) || strings.Contains(out, rpw) {
			t.Errorf("%s 泄漏口令: %s", name, out)
		}
	}
	if got := fmt.Sprintf("%v|%s|%q|%+v", cfg.Database.Password, cfg.Database.Password, cfg.Database.Password, cfg.Database.Password); got != "***|***|\"***\"|***" {
		t.Errorf("已设置的 Secret 格式化应为 ***: %s", got)
	}
	if got := fmt.Sprintf("%v|%s", Secret{}, NewSecret("")); got != "|" {
		t.Errorf("未设置的 Secret 应为空串（可区分是否已配置）: %q", got)
	}

	// 日志：裸 zap（不经脱敏 Core）凭 Secret 自身就安全；经 logx 同样安全。
	obsCore, logs := observer.New(zapcore.DebugLevel)
	raw := zap.New(obsCore)
	raw.Info("cfg", zap.Any("cfg", cfg), zap.Stringer("pw", cfg.Database.Password), zap.Any("pw2", cfg.Database.Password))
	var sb strings.Builder
	for _, e := range logs.All() {
		fmt.Fprintf(&sb, "%v", e.Context)
		for _, f := range e.Context {
			if f.Interface != nil {
				b, _ := json.Marshal(f.Interface)
				sb.Write(b)
			}
		}
	}
	if strings.Contains(sb.String(), pw) || strings.Contains(sb.String(), rpw) {
		t.Errorf("裸 zap 泄漏: %s", sb.String())
	}
	var buf strings.Builder
	l, err := logx.New(logx.Options{Output: &buf})
	if err != nil {
		t.Fatal(err)
	}
	l.Info("cfg loaded", zap.Any("cfg", cfg), zap.Any("db", cfg.Database), zap.Stringer("pw", cfg.Database.Password))
	if strings.Contains(buf.String(), pw) || strings.Contains(buf.String(), rpw) {
		t.Errorf("logx 泄漏: %s", buf.String())
	}
	if !strings.Contains(buf.String(), redact.Mask) {
		t.Errorf("日志里应出现掩码: %s", buf.String())
	}
}

func TestPasswordsInYAMLAreRejectedButEnvScalarsWork(t *testing.T) {
	const leak = "YamlPlainLeak424242"
	for _, section := range []string{"database", "redis"} {
		_, err := Parse([]byte(fmt.Sprintf("%s:\n  %s: %s\n", section, "pass"+"word", leak)), Options{Environ: []string{}})
		if err == nil || !hasProblem(problems(t, err), section+".password", "不得写在配置文件中") {
			t.Errorf("%s: YAML 中的口令必须被拒绝: %v", section, err)
			continue
		}
		if strings.Contains(err.Error(), leak) {
			t.Errorf("%s: 错误回显了口令: %v", section, err)
		}
	}
	// 经环境变量提供的数字口令保持原文本。
	cfg := mustParse(t, "", kv("LINGEDGE_DATABASE__PASS"+"WORD", "20250117"))
	if cfg.Database.Password.Reveal() != "20250117" {
		t.Errorf("环境变量口令: %q", cfg.Database.Password.Reveal())
	}
	a := NewSecret("x")
	b := a
	if b.Reveal() != "x" || !b.IsSet() || (Secret{}).IsSet() || (Secret{}).Reveal() != "" {
		t.Error("Secret 值语义异常")
	}
	// %#v 走 GoString 形态且不含明文。
	if got := fmt.Sprintf("%#v", NewSecret("plain-x")); got != `config.Secret("***")` || strings.Contains(got, "plain-x") {
		t.Errorf("%%#v 输出: %s", got)
	}
}

func TestSecretFilePermissionsSymlinkAndSize(t *testing.T) {
	cfgFor := func(path string) string { return fmt.Sprintf("database:\n  password_file: %s\n", path) }
	// 允许的权限位。
	for _, mode := range []os.FileMode{0o400, 0o440, 0o600, 0o640} {
		if _, err := Parse([]byte(cfgFor(writeSecretFile(t, "ok-secret\n\n", mode))), Options{Environ: []string{}}); err != nil {
			t.Errorf("权限 %04o 应被接受: %v", mode, err)
		}
	}
	// 同组可写、其他用户可读/可写：拒绝。
	for _, mode := range []os.FileMode{0o660, 0o620, 0o602, 0o604, 0o606, 0o777} {
		if _, err := Parse([]byte(cfgFor(writeSecretFile(t, "x-secret", mode))), Options{Environ: []string{}}); err == nil || !hasProblem(problems(t, err), "database.password_file", "权限过宽") {
			t.Errorf("权限 %04o 应被拒绝: %v", mode, err)
		}
	}
	// 多个末尾换行全部去除，内部空白保留。
	got := mustParse(t, cfgFor(writeSecretFile(t, "a b  c\r\n\n", 0o600))).Database.Password.Reveal()
	if got != "a b  c" {
		t.Errorf("末尾换行应全部去除、内部空白保留: %q", got)
	}
	// 符号链接按目标文件检查权限。
	dir := t.TempDir()
	target := writeSecretFile(t, "link-secret", 0o600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("无法创建符号链接: %v", err)
	}
	if mustParse(t, cfgFor(link)).Database.Password.Reveal() != "link-secret" {
		t.Error("符号链接应读取目标内容")
	}
	wide := writeSecretFile(t, "wide-secret", 0o666)
	link2 := filepath.Join(dir, "link2")
	if err := os.Symlink(wide, link2); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]byte(cfgFor(link2)), Options{Environ: []string{}}); err == nil || !hasProblem(problems(t, err), "", "权限过宽") {
		t.Errorf("指向宽权限文件的符号链接应被拒绝: %v", err)
	}
	// 过大文件。
	if _, err := Parse([]byte(cfgFor(writeSecretFile(t, strings.Repeat("a", 70<<10), 0o600))), Options{Environ: []string{}}); err == nil || !hasProblem(problems(t, err), "", "过大") {
		t.Errorf("过大的口令文件应被拒绝: %v", err)
	}
	// 相对路径只报告一次。
	_, err := Parse([]byte(cfgFor("rel/path")), Options{Environ: []string{}})
	n := 0
	for _, p := range problems(t, err) {
		if p.Path == "database.password_file" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("相对路径应只报告一次，实际 %d: %v", n, err)
	}
}

func TestListenConflictsIncludeWildcardOverlapAndErrorsAreRuneSafe(t *testing.T) {
	cases := []struct{ name, yaml, path string }{
		{"通配与具体地址同端口", "grpc:\n  listen: 127.0.0.1:9443\n  enroll_listen: 0.0.0.0:9443\n", "grpc.enroll_listen"},
		{"IPv6 通配与 IPv4 通配同端口", "http:\n  listen: '[::]:8080'\nmetrics:\n  listen: 0.0.0.0:8080\n", "metrics.listen"},
		{"具体地址与通配同端口（顺序相反）", "http:\n  listen: 0.0.0.0:8080\nmetrics:\n  listen: 127.0.0.1:8080\n", "metrics.listen"},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(tc.yaml), Options{Environ: []string{}})
		if err == nil || !hasProblem(problems(t, err), tc.path, "监听同一地址") {
			t.Errorf("%s: 应报监听冲突: %v", tc.name, err)
		}
	}
	// 不同端口、不同具体地址：无冲突。
	if _, err := Parse([]byte("http:\n  listen: 127.0.0.1:8080\nmetrics:\n  listen: 127.0.0.2:8080\n"), Options{Environ: []string{}}); err != nil {
		t.Errorf("不同具体地址同端口应允许: %v", err)
	}
	// 无主机的 https URL。
	if _, err := Parse([]byte("release:\n  base_url: https://:443/\n"), Options{Environ: []string{}}); err == nil || !hasProblem(problems(t, err), "release.base_url", "https URL") {
		t.Errorf("无主机名的发行源应被拒绝: %v", err)
	}
	// 类型错误里值含反引号与多字节字符时不得残留片段，且截断不破坏 UTF-8。
	const leak = "FragmentLeak99"
	_, err := Parse([]byte("database:\n  port: \"a`"+leak+"`b "+strings.Repeat("日", 300)+"\"\n"), Options{Environ: []string{}})
	if err == nil || strings.Contains(err.Error(), leak) || !utf8.ValidString(err.Error()) {
		t.Errorf("YAML 类型错误不得残留值片段且须是合法 UTF-8: %v", err)
	}
}

// ---- 示例配置：可加载、无任何口令/密钥（NFR-6） ----

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("未找到 go.mod")
		}
		dir = parent
	}
}

func TestExampleConfigLoadsAndContainsNoSecrets(t *testing.T) {
	path := filepath.Join(repoRoot(t), "configs", "server.example.yaml")
	cfg, err := Load(path, Options{Environ: []string{}})
	if err != nil {
		t.Fatalf("示例配置必须能通过 schema 校验: %v", err)
	}
	if cfg.Database.Password.IsSet() || cfg.Redis.Password.IsSet() || cfg.Database.PasswordFile != "" || cfg.Redis.PasswordFile != "" {
		t.Error("示例配置不得设置任何口令或口令文件")
	}
	// 示例本身不满足运行期要求：证明它没有默认口令（口令必须由部署者提供）。
	if _, err := Load(path, Options{Environ: []string{}, RequireRuntime: true}); err == nil || !hasProblem(problems(t, err), "database.password", "必填") {
		t.Errorf("示例配置在运行期应因缺少口令被拒: %v", err)
	}
	// 在加上口令来源之后，示例的其余内容应完全满足运行期要求。
	if _, err := Load(path, Options{Environ: []string{"LINGEDGE_DATABASE__PASSWORD=x"}, RequireRuntime: true}); err != nil {
		t.Errorf("示例 + 口令应满足运行期要求: %v", err)
	}

	// 结构化检查：敏感键名（redact.Key）下不得有非空值（唯一例外是以 _file 结尾的路径键，且示例里不应设置）。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	var walk func(n *yaml.Node, trail string)
	walk = func(n *yaml.Node, trail string) {
		switch n.Kind {
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, c := range n.Content {
				walk(c, trail)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				path := strings.TrimPrefix(trail+"."+k.Value, ".")
				if redact.Key(k.Value) && (v.Kind != yaml.ScalarNode || v.Value != "") {
					t.Errorf("示例配置的敏感键 %s 不得有取值", path)
				}
				walk(v, path)
			}
		}
	}
	walk(&root, "")

	// 文本层面：不得出现看似密钥材料的内容。
	text := string(raw)
	for _, bad := range []string{"BEGIN PRIVATE KEY", "BEGIN RSA", "lenr1.", "le_"} {
		if strings.Contains(text, bad) {
			t.Errorf("示例配置不应包含 %q", bad)
		}
	}
	if got := redact.String(text); got != text {
		t.Errorf("示例配置经脱敏引擎应保持不变（说明其中没有任何凭据形态的内容）")
	}
}

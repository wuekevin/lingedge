// Package config 加载并校验 lingedge-server 的 YAML 配置（Story 1.4 AC1、NFR-6）。
//
// 规则：
//   - schema：结构体即 schema；未知键、重复键、多文档、类型不符一律拒绝（KnownFields）；缺省值只来自 Defaults()，
//     不虚构待基线的参数（UD-27）；
//   - 优先级：环境变量 > YAML 文件 > Defaults()。环境变量名 LINGEDGE_<节>__<键>（双下划线分隔层级，键内下划线保留，
//     全大写），如 LINGEDGE_DATABASE__PASSWORD、LINGEDGE_GRPC__ENROLL_LISTEN；带 LINGEDGE_ 前缀但不对应任何键的变量拒绝；
//   - 秘密：口令不得写在 server.yaml（NFR-6、AA-7；写了即拒绝）。通道有二：环境变量 / `<x>.password_file`（文件内容为口令，
//     不得被「其他用户」读取）。Secret 类型在任何格式化、序列化、日志里都是 ***；
//   - 错误文本只含键路径与规则说明，永不回显任何配置值（避免把写错位置的口令带进错误与日志）。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/pkg/redact"
)

// EnvPrefix 是环境变量覆盖的前缀。
const EnvPrefix = "LINGEDGE_"

// Config 是 lingedge-server 的完整配置。后续 Story 新增配置节须在此登记（未登记的键会被拒绝）。
type Config struct {
	Server   Server   `yaml:"server"`
	HTTP     HTTP     `yaml:"http"`
	GRPC     GRPC     `yaml:"grpc"`
	Metrics  Metrics  `yaml:"metrics"`
	Database Database `yaml:"database"`
	Redis    Redis    `yaml:"redis"`
	Log      Log      `yaml:"log"`
	Release  Release  `yaml:"release"`
}

// Server 是进程级设置。
type Server struct {
	// Env 是环境名，进入 Redis 键空间 le:<env>:…（ADR-025 §4）；必填，避免共用 Redis 的两套环境互相污染。
	Env string `yaml:"env"`
	// DataDir 是数据目录（tools-design §5：/var/lib/lingedge/server）。
	DataDir string `yaml:"data_dir"`
}

// HTTP 是 REST 监听。
type HTTP struct {
	Listen string `yaml:"listen"`
}

// GRPC 是节点 gRPC 监听（mTLS :9443；注册 :9444，仅有未过期注册令牌时才监听，ADR-003）。
type GRPC struct {
	Listen       string `yaml:"listen"`
	EnrollListen string `yaml:"enroll_listen"`
}

// Metrics 是 Prometheus 指标监听（9180，tools-design §10.4）。
type Metrics struct {
	Listen string `yaml:"listen"`
}

// Database 是 MySQL 连接（运行期 app 账号；三类账号的连接池拆分随 Story 1.5 扩展）。
type Database struct {
	Host         string `yaml:"host"`
	Port         int    `yaml:"port"`
	Name         string `yaml:"name"`
	User         string `yaml:"user"`
	Password     Secret `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
}

// Redis 是 Redis 连接（非唯一真相源，ADR-025）。
type Redis struct {
	Addr         string `yaml:"addr"`
	DB           int    `yaml:"db"`
	Password     Secret `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
	// TLS 为 true 时以 TLS 连接 Redis（Story 1.10）；TLSCAFile（绝对路径，可选）追加信任的根证书；
	// TLSServerName 覆盖证书校验用的服务器名（缺省取 addr 的主机）。证书校验不可关闭。
	TLS           bool   `yaml:"tls"`
	TLSCAFile     string `yaml:"tls_ca_file"`
	TLSServerName string `yaml:"tls_server_name"`
}

// Log 是日志设置。
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Release 是发行源设置（ADR-012；官方发行源尚未确定，UD-3，故无默认值）。
type Release struct {
	BaseURL string `yaml:"base_url"`
	CAFile  string `yaml:"ca_file"`
}

// Defaults 返回缺省配置：仅含架构文档已明确的监听/路径/日志取值，不含任何口令或待基线的参数。
func Defaults() Config {
	return Config{
		Server:   Server{DataDir: "/var/lib/lingedge/server"},
		HTTP:     HTTP{Listen: "127.0.0.1:8080"},
		GRPC:     GRPC{Listen: "0.0.0.0:9443", EnrollListen: "0.0.0.0:9444"},
		Metrics:  Metrics{Listen: "127.0.0.1:9180"},
		Database: Database{Port: 3306},
		Log:      Log{Level: "info", Format: "json"},
	}
}

// Problem 是一条校验问题：Path 为键路径（或 env 变量名），Reason 为规则说明（不含任何配置值）。
type Problem struct {
	Path   string
	Reason string
}

// Error 汇总全部问题。
type Error struct{ Problems []Problem }

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "config: %d 个问题", len(e.Problems))
	for _, p := range e.Problems {
		if p.Path != "" {
			fmt.Fprintf(&b, "\n  %s: %s", p.Path, p.Reason)
		} else {
			fmt.Fprintf(&b, "\n  %s", p.Reason)
		}
	}
	return b.String()
}

// Options 控制加载行为。零值可用。
type Options struct {
	// Environ 是 KEY=VALUE 形式的环境变量（默认 os.Environ()）。
	Environ []string
	// EnvIgnore 列出带 LINGEDGE_ 前缀、但不是配置覆盖项的变量名（如 LINGEDGE_CONFIG），不会被当作未知覆盖拒绝。
	EnvIgnore []string
	// RequireRuntime 为 true 时额外要求运行期必需项（数据库连接与口令、Redis 地址）；setup mode 不需要。
	RequireRuntime bool
}

// Load 读取 path 指向的 YAML 并按 Options 加载。文件不存在视为错误（setup mode 的 server.yaml 由 Tools 安装时生成）。
func Load(path string, o Options) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Problems: []Problem{{Reason: "无法读取配置文件（" + fsReason(err) + "）"}}}
	}
	return Parse(data, o)
}

// Parse 从内存中的 YAML 加载（流程：解码 → 环境变量覆盖 → 口令来源互斥 → 读取 *_file → 校验）。
func Parse(data []byte, o Options) (*Config, error) {
	cfg := Defaults()
	var probs []Problem

	probs = append(probs, decodeYAML(data, &cfg)...)
	// 口令不得写在配置文件里（NFR-6、AA-7）：此刻 Password 若非空，只可能来自 YAML（Defaults 不含口令，环境变量尚未应用）。
	if cfg.Database.Password.IsSet() {
		probs = append(probs, Problem{Path: "database.password", Reason: "不得写在配置文件中；请改用环境变量 LINGEDGE_DATABASE__PASSWORD 或 database.password_file"})
	}
	if cfg.Redis.Password.IsSet() {
		probs = append(probs, Problem{Path: "redis.password", Reason: "不得写在配置文件中；请改用环境变量 LINGEDGE_REDIS__PASSWORD 或 redis.password_file"})
	}
	probs = append(probs, applyEnv(&cfg, o)...)
	if len(probs) > 0 { // 结构性问题时不再继续（避免后续规则在无意义的值上产生噪声）
		return nil, &Error{Problems: probs}
	}

	probs = append(probs, checkSecretSources(&cfg)...)
	if len(probs) == 0 {
		probs = append(probs, resolveSecretFiles(&cfg)...)
	}
	probs = append(probs, cfg.validateProblems()...)
	if o.RequireRuntime {
		probs = append(probs, cfg.runtimeProblems()...)
	}
	if len(probs) > 0 {
		return nil, &Error{Problems: probs}
	}
	return &cfg, nil
}

// Validate 校验当前配置的取值（不含运行期必需项）。
func (c *Config) Validate() error {
	if p := c.validateProblems(); len(p) > 0 {
		return &Error{Problems: p}
	}
	return nil
}

// ValidateRuntime 在 Validate 之外要求运行期必需项：数据库 host/name/user 与口令、Redis 地址。
func (c *Config) ValidateRuntime() error {
	p := append(c.validateProblems(), c.runtimeProblems()...)
	if len(p) > 0 {
		return &Error{Problems: p}
	}
	return nil
}

// ---- YAML 解码 ----

// yaml 在类型错误里用反引号回显标量；取「首个到最后一个反引号」整段抹掉（值本身含反引号时也不会残留片段）。
var backtickValue = regexp.MustCompile("(?s)`.*`")

// sanitizeYAMLMsg 去掉 yaml 错误文本里可能回显的标量内容（形如 `abc`），并截断、过一遍脱敏。
func sanitizeYAMLMsg(s string) string {
	s = strings.TrimPrefix(s, "yaml: ")
	s = backtickValue.ReplaceAllString(s, "<值已隐去>")
	s = redact.String(s)
	if r := []rune(s); len(r) > 200 {
		s = string(r[:200]) + "…"
	}
	return s
}

func decodeYAML(data []byte, cfg *Config) []Problem {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		var te *yaml.TypeError
		if errors.As(err, &te) {
			out := make([]Problem, 0, len(te.Errors))
			for _, m := range te.Errors {
				out = append(out, Problem{Reason: "YAML: " + sanitizeYAMLMsg(m)})
			}
			return out
		}
		return []Problem{{Reason: "YAML: " + sanitizeYAMLMsg(err.Error())}}
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return []Problem{{Reason: "YAML: 只允许单个文档"}}
	}
	return nil
}

// ---- 环境变量覆盖 ----

type leaf struct {
	path string // 点分键路径（database.host）
	val  reflect.Value
}

var secretType = reflect.TypeOf(Secret{})

func envName(path string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(path, ".", "__"))
}

// collectLeaves 以 yaml 标签为准遍历结构体，得到「环境变量名 → 叶子字段」。
func collectLeaves(v reflect.Value, prefix string, out map[string]leaf) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if !f.IsExported() || tag == "" || tag == "-" {
			continue
		}
		path := tag
		if prefix != "" {
			path = prefix + "." + tag
		}
		fv := v.Field(i)
		if f.Type.Kind() == reflect.Struct && f.Type != secretType {
			collectLeaves(fv, path, out)
			continue
		}
		out[envName(path)] = leaf{path: path, val: fv}
	}
}

func applyEnv(cfg *Config, o Options) []Problem {
	environ := o.Environ
	if environ == nil {
		environ = os.Environ()
	}
	leaves := map[string]leaf{}
	collectLeaves(reflect.ValueOf(cfg).Elem(), "", leaves)
	ignore := map[string]bool{}
	for _, n := range o.EnvIgnore {
		ignore[n] = true
	}

	var probs []Problem
	var names []string
	vals := map[string]string{}
	for _, kv := range environ {
		name, val, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, EnvPrefix) || ignore[name] {
			continue
		}
		names = append(names, name)
		vals[name] = val
	}
	sort.Strings(names)
	for _, name := range names {
		l, ok := leaves[name]
		if !ok {
			probs = append(probs, Problem{Path: name, Reason: "未知的覆盖项（不对应任何配置键；变量名只含键路径，值未被读取）"})
			continue
		}
		if err := setLeaf(l.val, vals[name]); err != nil {
			probs = append(probs, Problem{Path: name, Reason: err.Error()})
		}
	}
	return probs
}

func setLeaf(v reflect.Value, s string) error {
	switch {
	case v.Type() == secretType:
		v.Set(reflect.ValueOf(NewSecret(s)))
	case v.Kind() == reflect.String:
		v.SetString(s)
	case v.Kind() == reflect.Int:
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return errors.New("必须是整数")
		}
		v.SetInt(int64(n))
	case v.Kind() == reflect.Bool:
		b, err := strconv.ParseBool(strings.TrimSpace(s))
		if err != nil {
			return errors.New("必须是布尔值（true/false）")
		}
		v.SetBool(b)
	default:
		return fmt.Errorf("不支持经环境变量覆盖的类型 %s", v.Type())
	}
	return nil
}

// ---- 秘密来源 ----

func checkSecretSources(c *Config) []Problem {
	var out []Problem
	if c.Database.Password.IsSet() && c.Database.PasswordFile != "" {
		out = append(out, Problem{Path: "database", Reason: "password 与 password_file 只能设置其一"})
	}
	if c.Redis.Password.IsSet() && c.Redis.PasswordFile != "" {
		out = append(out, Problem{Path: "redis", Reason: "password 与 password_file 只能设置其一"})
	}
	return out
}

func resolveSecretFiles(c *Config) []Problem {
	var out []Problem
	for _, t := range []struct {
		path string
		file *string
		dst  *Secret
	}{
		{"database.password_file", &c.Database.PasswordFile, &c.Database.Password},
		{"redis.password_file", &c.Redis.PasswordFile, &c.Redis.Password},
	} {
		if *t.file == "" || !filepath.IsAbs(*t.file) { // 相对路径由 validateProblems 报告，此处不重复
			continue
		}
		s, reason := readSecretFile(*t.file)
		if reason != "" {
			out = append(out, Problem{Path: t.path, Reason: reason})
			continue
		}
		*t.dst = s
	}
	return out
}

// maxSecretFileSize 限制口令文件大小（口令不会有这么大，防止误指向大文件）。
const maxSecretFileSize = 64 << 10

// readSecretFile 读取口令文件：绝对路径、普通文件、权限不得允许「其他用户」任何访问且不得被同组写入
// （允许 0400/0440/0600/0640；root:lingedge 0440 即 AA-7 建议形态）、非空。打开后对同一句柄 fstat，避免检查与读取之间被替换。
func readSecretFile(path string) (Secret, string) {
	if !filepath.IsAbs(path) {
		return Secret{}, "必须是绝对路径"
	}
	f, err := os.Open(path)
	if err != nil {
		return Secret{}, "无法读取口令文件（" + fsReason(err) + "）"
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return Secret{}, "无法读取口令文件（" + fsReason(err) + "）"
	}
	if !st.Mode().IsRegular() {
		return Secret{}, "必须是普通文件"
	}
	if st.Mode().Perm()&0o027 != 0 {
		return Secret{}, fmt.Sprintf("口令文件权限过宽（当前 %04o；其他用户不得访问、同组不得写入，应为 0400/0440/0600/0640）", st.Mode().Perm())
	}
	if st.Size() > maxSecretFileSize {
		return Secret{}, "口令文件过大"
	}
	b, err := io.ReadAll(io.LimitReader(f, maxSecretFileSize+1))
	if err != nil {
		return Secret{}, "无法读取口令文件（" + fsReason(err) + "）"
	}
	v := strings.TrimRight(string(b), "\r\n")
	if v == "" {
		return Secret{}, "口令文件为空"
	}
	return NewSecret(v), ""
}

func fsReason(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "不存在"
	case errors.Is(err, fs.ErrPermission):
		return "权限不足"
	default:
		return "读取失败"
	}
}

// ---- 取值校验 ----

var (
	reEnv    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	reDBName = regexp.MustCompile(`^[A-Za-z0-9_$]{1,64}$`)
	reDBUser = regexp.MustCompile(`^[A-Za-z0-9_.\-]{1,32}$`)
	reHost   = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.\-]{0,251}[A-Za-z0-9])?$`)
)

func validHostOrIP(h string) bool {
	return net.ParseIP(h) != nil || reHost.MatchString(h)
}

// validListen：IP 字面量（或空主机）+ 1–65535 端口。
func validListen(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host != "" && net.ParseIP(host) == nil {
		return false
	}
	return validPort(port)
}

func validPort(p string) bool {
	n, err := strconv.Atoi(p)
	return err == nil && n >= 1 && n <= 65535
}

func cleanAbs(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p && p != "/"
}

func (c *Config) validateProblems() []Problem {
	var out []Problem
	add := func(path, reason string) { out = append(out, Problem{Path: path, Reason: reason}) }

	// server
	if c.Server.Env != "" && !reEnv.MatchString(c.Server.Env) {
		add("server.env", "必须是小写字母开头、仅含小写字母/数字/连字符、最长 32 位的环境名")
	}
	if !cleanAbs(c.Server.DataDir) {
		add("server.data_dir", "必须是规范化的绝对路径（不得含 .. 或多余分隔符，且不得为 /）")
	}

	// 监听地址：格式 + 互不冲突
	listens := []struct{ path, val string }{
		{"http.listen", c.HTTP.Listen}, {"grpc.listen", c.GRPC.Listen},
		{"grpc.enroll_listen", c.GRPC.EnrollListen}, {"metrics.listen", c.Metrics.Listen},
	}
	seen := map[string]string{}
	for _, l := range listens {
		if !validListen(l.val) {
			add(l.path, "必须是 <IP>:<端口>（端口 1–65535；主机可为空表示全部地址）")
			continue
		}
		if other, dup := conflictingListen(seen, l.val); dup {
			add(l.path, "与 "+other+" 监听同一地址或同一端口的通配地址")
			continue
		}
		seen[normalizeListen(l.val)] = l.path
	}

	// database（字段为空表示「尚未配置」，必需性由 ValidateRuntime 判定）
	d := c.Database
	if d.Host != "" && !validHostOrIP(d.Host) {
		add("database.host", "必须是主机名或 IP")
	}
	if d.Port < 1 || d.Port > 65535 {
		add("database.port", "必须在 1–65535")
	}
	if d.Name != "" && !reDBName.MatchString(d.Name) {
		add("database.name", "必须是 MySQL 标识符（字母/数字/_/$，最长 64）")
	}
	if d.User != "" && !reDBUser.MatchString(d.User) {
		add("database.user", "必须是 1–32 位的字母/数字/_/./-")
	}

	// redis
	r := c.Redis
	if r.Addr != "" {
		if host, port, err := net.SplitHostPort(r.Addr); err != nil || host == "" || !validHostOrIP(host) || !validPort(port) {
			add("redis.addr", "必须是 <主机>:<端口>")
		}
	}
	if r.DB < 0 || r.DB > 15 {
		add("redis.db", "必须在 0–15")
	}

	// log
	if !contains(logx.Levels, c.Log.Level) {
		add("log.level", "必须是 "+strings.Join(logx.Levels, "|")+" 之一")
	}
	if !contains(logx.Formats, c.Log.Format) {
		add("log.format", "必须是 "+strings.Join(logx.Formats, "|")+" 之一")
	}

	// release（官方发行源未定，UD-3：可不配置；配置时必须是 HTTPS 且不得内嵌凭据）
	if c.Release.BaseURL != "" {
		u, err := url.Parse(c.Release.BaseURL)
		switch {
		case err != nil || u.Hostname() == "":
			add("release.base_url", "必须是合法的 https URL")
		case u.Scheme != "https":
			add("release.base_url", "必须使用 https（下载仅 HTTPS，tools-design §10）")
		case u.User != nil:
			add("release.base_url", "不得在 URL 中内嵌凭据")
		case u.RawQuery != "" || u.Fragment != "":
			add("release.base_url", "不得含查询串或片段")
		}
	}
	if c.Release.CAFile != "" && !filepath.IsAbs(c.Release.CAFile) {
		add("release.ca_file", "必须是绝对路径")
	}
	if (r.TLSCAFile != "" || r.TLSServerName != "") && !r.TLS {
		add("redis.tls", "设置了 tls_ca_file/tls_server_name 时必须 tls: true")
	}
	for _, f := range []struct{ path, val string }{{"database.password_file", d.PasswordFile}, {"redis.password_file", r.PasswordFile}, {"redis.tls_ca_file", r.TLSCAFile}} {
		if f.val != "" && !filepath.IsAbs(f.val) {
			add(f.path, "必须是绝对路径")
		}
	}
	return out
}

// conflictingListen：同主机同端口冲突；通配地址（空/0.0.0.0/::）与同端口的任何地址冲突（后者启动时会 EADDRINUSE）。
func conflictingListen(seen map[string]string, addr string) (string, bool) {
	host, port, _ := net.SplitHostPort(addr)
	for k, path := range seen {
		h, p, _ := net.SplitHostPort(k)
		if p != port {
			continue
		}
		if h == normalizeHost(host) || isWildcard(host) || isWildcard(h) {
			return path, true
		}
	}
	return "", false
}

func isWildcard(h string) bool {
	if h == "" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsUnspecified()
}

func normalizeHost(h string) string {
	if h == "" {
		return "0.0.0.0"
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.String()
	}
	return h
}

func normalizeListen(addr string) string {
	host, port, _ := net.SplitHostPort(addr)
	return net.JoinHostPort(normalizeHost(host), port)
}

func (c *Config) runtimeProblems() []Problem {
	var out []Problem
	req := func(path string, missing bool) {
		if missing {
			out = append(out, Problem{Path: path, Reason: "运行期必填"})
		}
	}
	req("server.env", c.Server.Env == "")
	req("database.host", c.Database.Host == "")
	req("database.name", c.Database.Name == "")
	req("database.user", c.Database.User == "")
	if !c.Database.Password.IsSet() {
		out = append(out, Problem{Path: "database.password", Reason: "运行期必填：经环境变量 LINGEDGE_DATABASE__PASSWORD 或 database.password_file 提供（不提供默认口令，NFR-6）"})
	}
	req("redis.addr", c.Redis.Addr == "")
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

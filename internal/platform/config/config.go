// Package config 加载并校验 lingedge-server 的 YAML 配置（Story 1.4 AC1、NFR-6）。
//
// 规则：
//   - schema：结构体即 schema；未知键、重复键、多文档、类型不符一律拒绝（KnownFields）；缺省值只来自 Defaults()，
//     不虚构待基线的参数（UD-27）；
//   - 优先级：环境变量 > YAML 文件 > Defaults()。环境变量名 LINGEDGE_<节>__<键>（双下划线分隔层级，键内下划线保留，
//     全大写），如 LINGEDGE_DATABASE__PASSWORD、LINGEDGE_GRPC__ENROLL_LISTEN；带 LINGEDGE_ 前缀但不对应任何键的变量拒绝；
//   - 秘密：口令不得写在 server.yaml（NFR-6、AA-7/ADR-026；写了即拒绝）。显式通道有二：环境变量 / `<x>.password_file`（文件内容为口令，
//     不得被「其他用户」读取），它们优先；缺省时由安装向导写入的运行期凭据文件 `secrets.runtime_credentials_file`（信封加密，
//     ADR-026 §3）经 internal/platform/dbcred 合并进来。Secret 类型在任何格式化、序列化、日志里都是 ***；
//   - 错误文本只含键路径与规则说明，永不回显任何配置值（避免把写错位置的口令带进错误与日志）。
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
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
	Secrets  Secrets  `yaml:"secrets"`
	Auth     Auth     `yaml:"auth"`
}

// Server 是进程级设置。
type Server struct {
	// Env 是环境名，进入 Redis 键空间 le:<env>:…（ADR-025 §4）；必填，避免共用 Redis 的两套环境互相污染。
	Env string `yaml:"env"`
	// DataDir 是数据目录（tools-design §5：/var/lib/lingedge/server）。
	DataDir string `yaml:"data_dir"`
	// TrustedProxies 是可信反代（IP 或 CIDR 列表）：只有 TCP 对端属于这些范围时才采信 X-Forwarded-For（取链上最右侧的非可信地址）。
	// 它是 REST 来源 IP 的唯一来源——访问日志、登录日志/节流、API Token 的 allowed_cidrs、审计的 ip 全部经 httpx.ClientIPFrom 取同一个值（Story 1.22b）。
	// 缺省仅 127.0.0.1 与 ::1（同机 Nginx 反代，tools-design §10.4）；环境变量用逗号分隔。
	TrustedProxies []string `yaml:"trusted_proxies"`
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
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// Socket 是 MySQL Unix 域套接字的绝对路径（CC-13）；与 host 互斥（设置时 port 被忽略）。套接字连接无需 TLS。
	Socket       string `yaml:"socket"`
	Name         string `yaml:"name"`
	User         string `yaml:"user"`
	Password     Secret `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
	// TLS 是 app/maint 连接的 TLS 模式（true|false|skip-verify|preferred，缺省空）；非回环主机必须显式设置（db.Config 校验）。
	// 缺省由运行期凭据文件提供（ADR-026 §3），可经环境变量 LINGEDGE_DATABASE__TLS 覆盖（CC-12，加法）。
	TLS string `yaml:"tls"`
	// Maint 是 lingedge_maint 账号（分区维护 partition.maintain 专用，database/README §5.1；Story 1.15）。
	// 可选：未配置（user 为空）时 Scheduler 的分区维护作业明确跳过并告警，绝不假装成功；连接目标（host/port/name）与 app 相同。
	Maint DatabaseMaint `yaml:"maint"`
}

// DatabaseMaint 是维护账号的凭据。口令只能经环境变量（LINGEDGE_DATABASE__MAINT__PASSWORD）或 password_file 显式提供（优先），
// 缺省时由运行期凭据文件（ADR-026 §3，安装向导写入）合并；二者都没有则分区维护作业明确跳过并告警。
type DatabaseMaint struct {
	User         string `yaml:"user"`
	Password     Secret `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
}

// Configured 报告维护账号是否已完整配置（用户名与口令都在）。
func (m DatabaseMaint) Configured() bool { return m.User != "" && m.Password.IsSet() }

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

// Secrets 是密钥保管设置（ADR-011、Story 1.11）。主密钥内容绝不出现在配置里，这里只指向文件。
type Secrets struct {
	// MasterKeyFile 是主密钥文件的绝对路径；留空则读取 $CREDENTIALS_DIRECTORY/master.key（systemd LoadCredential）。
	// 文件须 0400/0440 且属主正确，否则拒绝启动（pkg/secretbox.FileKeyProvider）。
	MasterKeyFile string `yaml:"master_key_file"`
	// RuntimeCredentialsFile 是运行期凭据文件的绝对路径（ADR-026 §3；缺省 `<server.data_dir>/secrets/runtime-credentials.json`）：
	// 安装向导把 lingedge_app/lingedge_maint/Redis 口令以信封加密写在这里（lingedge 属主 0600）。显式通道（环境变量/*.password_file）优先于它。
	RuntimeCredentialsFile string `yaml:"runtime_credentials_file"`
	// SetupTokenHashFile 是 setup token 的 SHA-256 哈希文件（tools-design §13；缺省 /etc/lingedge/server/setup-token.hash）。仅 setup mode 读取，
	// complete 之后删除（无权删除时清空）。可选键，供非标准部署与测试改路径（CC-12，加法）。
	SetupTokenHashFile string `yaml:"setup_token_hash_file"`
}

// Auth 是认证设置（Story 1.16、ADR-018 §4）。会话时长与登录节流阈值是系统设置（system_settings，UD-27），不在此处。
type Auth struct {
	Argon2 Argon2 `yaml:"argon2"`
}

// Argon2 是 argon2id 口令哈希参数（ADR-018 §4：参数以实现时基准测定并写入配置）。
// 缺省值来自开发机基准（方法与数值见 docs/bmad/stories/1-16-*.md 与 internal/auth/internal/service 的 BenchmarkArgon2idHash），
// **生产机须按其 CPU/内存复测后调整**（目标：单次哈希约 50–250 ms；参数低于 OWASP 下限会被拒绝）。
// 改参数只影响之后的哈希：登录成功时发现旧哈希参数不同会自动重新哈希（rehash）。
type Argon2 struct {
	MemoryKiB   int `yaml:"memory_kib"`
	Iterations  int `yaml:"iterations"`
	Parallelism int `yaml:"parallelism"`
}

// Argon2 参数边界：下限取 OWASP 对 argon2id 的最低建议（m=19 MiB, t=2, p=1）；上限防止误配置拖垮进程。
const (
	MinArgon2MemoryKiB  = 19 * 1024
	MaxArgon2MemoryKiB  = 1024 * 1024
	MinArgon2Iterations = 2
	MaxArgon2Iterations = 16
	MaxArgon2Parallel   = 16
)

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
		Server:   Server{DataDir: "/var/lib/lingedge/server", TrustedProxies: []string{"127.0.0.1", "::1"}},
		HTTP:     HTTP{Listen: "127.0.0.1:8080"},
		GRPC:     GRPC{Listen: "0.0.0.0:9443", EnrollListen: "0.0.0.0:9444"},
		Metrics:  Metrics{Listen: "127.0.0.1:9180"},
		Database: Database{Port: 3306},
		Log:      Log{Level: "info", Format: "json"},
		Auth:     Auth{Argon2: Argon2{MemoryKiB: 65536, Iterations: 4, Parallelism: 4}},
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
	// 口令不得写在配置文件里（NFR-6、ADR-026）：此刻 Password 若非空，只可能来自 YAML（Defaults 不含口令，环境变量尚未应用）。
	if cfg.Database.Password.IsSet() {
		probs = append(probs, Problem{Path: "database.password", Reason: "不得写在配置文件中；请改用环境变量 LINGEDGE_DATABASE__PASSWORD 或 database.password_file"})
	}
	if cfg.Database.Maint.Password.IsSet() {
		probs = append(probs, Problem{Path: "database.maint.password", Reason: "不得写在配置文件中；请改用环境变量 LINGEDGE_DATABASE__MAINT__PASSWORD 或 database.maint.password_file"})
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
	case v.Kind() == reflect.Slice && v.Type().Elem().Kind() == reflect.String:
		var items []string
		for _, it := range strings.Split(s, ",") {
			if it = strings.TrimSpace(it); it != "" {
				items = append(items, it)
			}
		}
		v.Set(reflect.ValueOf(items))
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
	if c.Database.Maint.Password.IsSet() && c.Database.Maint.PasswordFile != "" {
		out = append(out, Problem{Path: "database.maint", Reason: "password 与 password_file 只能设置其一"})
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
		{"database.maint.password_file", &c.Database.Maint.PasswordFile, &c.Database.Maint.Password},
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
// （允许 0400/0440/0600/0640）、非空。打开后对同一句柄 fstat，避免检查与读取之间被替换。
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

// IsLoopbackListen 报告 <IP>:<端口> 的主机部分是否为回环 IP 字面量（127.0.0.0/8、::1）。空主机（通配）、0.0.0.0、::、主机名、无法解析一律为 false。
// 配置校验与 server 启动共用它（后者是对「绕过 Parse 直接构造 Config」的纵深防御）。
func IsLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateTrustedProxies 校验 server.trusted_proxies：每项是 IP 或 CIDR；拒绝未指定地址、/0 与过短前缀（IPv4 < /8、IPv6 < /32，与 ADR-006 §5 同口径）、
// 带 zone 的地址；至少一项（空列表会让「可信」语义含糊——要不信任任何代理请只列一个不会出现的地址，而不是留空）。
func (c *Config) validateTrustedProxies(add func(path, reason string)) {
	if len(c.Server.TrustedProxies) == 0 {
		add("server.trusted_proxies", "不得为空：至少列出一个可信反代 IP/CIDR（缺省 127.0.0.1 与 ::1）")
		return
	}
	for i, raw := range c.Server.TrustedProxies {
		path := fmt.Sprintf("server.trusted_proxies[%d]", i)
		s := strings.TrimSpace(raw)
		if s == "" || s != raw || strings.Contains(s, "%") {
			add(path, "必须是 IP 或 CIDR（不得含空白或 zone）")
			continue
		}
		var ip net.IP
		bits, total := -1, 0
		if strings.Contains(s, "/") {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				add(path, "必须是合法的 IP 或 CIDR")
				continue
			}
			ip, bits, total = net.IP(p.Addr().AsSlice()), p.Bits(), p.Addr().BitLen()
			if p.Addr().Is4In6() {
				add(path, "不接受 IPv4 映射的 IPv6 CIDR；请直接写 IPv4 CIDR")
				continue
			}
		} else {
			a, err := netip.ParseAddr(s)
			if err != nil {
				add(path, "必须是合法的 IP 或 CIDR")
				continue
			}
			ip = net.IP(a.AsSlice())
		}
		if ip.IsUnspecified() || bits == 0 {
			add(path, "不得是未指定地址或 /0：这会让任何人都能伪造来源 IP")
			continue
		}
		if bits >= 0 && ((total == 32 && bits < 8) || (total == 128 && bits < 32)) {
			add(path, "前缀过短（IPv4 至少 /8、IPv6 至少 /32）：范围过大会让不可信主机伪造来源 IP")
		}
	}
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

	// 可信反代：IP 或 CIDR；拒绝会让任何人伪造来源 IP 的范围。
	c.validateTrustedProxies(add)

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

	// metrics.listen 只允许回环（architecture §7、tools-design §10.4：指标端口仅本机可达，由本机 Prometheus/Agent 抓取）。
	// 架构文档没有任何「受控的非回环」场景，因此没有放开开关：通配/公网/内网地址一律拒绝启动（Story 1.22b；此前仅告警）。
	if validListen(c.Metrics.Listen) && !IsLoopbackListen(c.Metrics.Listen) {
		add("metrics.listen", "必须是回环地址（127.0.0.0/8 或 ::1）：指标端口仅本机可达（architecture §7）；通配地址、公网/内网地址一律拒绝，需要远程抓取请经本机 Prometheus/Nginx 转发")
	}

	// database（字段为空表示「尚未配置」，必需性由 ValidateRuntime 判定）
	d := c.Database
	if d.Host != "" && !validHostOrIP(d.Host) {
		add("database.host", "必须是主机名或 IP")
	}
	if d.Socket != "" {
		if d.Host != "" {
			add("database.socket", "与 database.host 互斥：连接目标只能是 host（+port）或 socket 之一")
		}
		if !cleanAbs(d.Socket) || strings.ContainsAny(d.Socket, "\x00\r\n") || len(d.Socket) > 100 {
			add("database.socket", "必须是规范化的绝对路径（最长 100 字节，Unix 套接字路径长度上限）")
		}
	} else if d.Port < 1 || d.Port > 65535 {
		add("database.port", "必须在 1–65535")
	}
	if d.Name != "" && !reDBName.MatchString(d.Name) {
		add("database.name", "必须是 MySQL 标识符（字母/数字/_/$，最长 64）")
	}
	if d.User != "" && !reDBUser.MatchString(d.User) {
		add("database.user", "必须是 1–32 位的字母/数字/_/./-")
	}
	switch d.TLS {
	case "", "true", "false", "skip-verify", "preferred":
	default:
		add("database.tls", "必须是 true|false|skip-verify|preferred 之一或留空")
	}
	if c.Secrets.RuntimeCredentialsFile != "" && !cleanAbs(c.Secrets.RuntimeCredentialsFile) {
		add("secrets.runtime_credentials_file", "必须是规范化的绝对路径")
	}
	if c.Secrets.SetupTokenHashFile != "" && !cleanAbs(c.Secrets.SetupTokenHashFile) {
		add("secrets.setup_token_hash_file", "必须是规范化的绝对路径")
	}

	// database.maint：用户名与口令要么都不配（分区维护将被跳过并告警），要么都配（半配置是笔误，拒绝）。
	if m := d.Maint; m.User != "" || m.Password.IsSet() || m.PasswordFile != "" {
		if m.User == "" {
			add("database.maint.user", "配置了维护账号口令但缺少用户名")
		} else if !reDBUser.MatchString(m.User) {
			add("database.maint.user", "必须是 1–32 位的字母/数字/_/./-")
		}
		if !m.Password.IsSet() && m.PasswordFile == "" {
			add("database.maint.password", "配置了维护账号用户名但缺少口令：经环境变量 LINGEDGE_DATABASE__MAINT__PASSWORD 或 database.maint.password_file 提供")
		}
		if m.PasswordFile != "" && !filepath.IsAbs(m.PasswordFile) {
			add("database.maint.password_file", "必须是绝对路径")
		}
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

	// auth.argon2
	if a := c.Auth.Argon2; a.MemoryKiB < MinArgon2MemoryKiB || a.MemoryKiB > MaxArgon2MemoryKiB {
		add("auth.argon2.memory_kib", fmt.Sprintf("必须在 %d–%d（KiB；下限为 OWASP 对 argon2id 的最低建议）", MinArgon2MemoryKiB, MaxArgon2MemoryKiB))
	} else if a.Iterations < MinArgon2Iterations || a.Iterations > MaxArgon2Iterations {
		add("auth.argon2.iterations", fmt.Sprintf("必须在 %d–%d", MinArgon2Iterations, MaxArgon2Iterations))
	} else if a.Parallelism < 1 || a.Parallelism > MaxArgon2Parallel {
		add("auth.argon2.parallelism", fmt.Sprintf("必须在 1–%d", MaxArgon2Parallel))
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
	if c.Secrets.MasterKeyFile != "" && !filepath.IsAbs(c.Secrets.MasterKeyFile) {
		add("secrets.master_key_file", "必须是绝对路径")
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
	req("database.host", c.Database.Host == "" && c.Database.Socket == "")
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

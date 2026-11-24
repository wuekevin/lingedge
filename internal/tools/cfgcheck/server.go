package cfgcheck

import (
	"net"
	"reflect"
	"sort"
	"strconv"
)

// 下面的结构体只描述 server.yaml 的「形状」（键集合与种类），是 `lingedge-server config check` 不可用时的结构校验依据。
// 它与 internal/platform/config.Config 的键集合保持一致由 tests/tools/serverconfig 的反射对照测试守住（新增配置键而忘了同步这里会让该测试失败）。
type serverShape struct {
	Server   serverSection   `yaml:"server"`
	HTTP     listenSection   `yaml:"http"`
	GRPC     grpcSection     `yaml:"grpc"`
	Metrics  listenSection   `yaml:"metrics"`
	Database databaseSection `yaml:"database"`
	Redis    redisSection    `yaml:"redis"`
	Log      logSection      `yaml:"log"`
	Release  releaseSection  `yaml:"release"`
	Secrets  secretsSection  `yaml:"secrets"`
	Auth     authSection     `yaml:"auth"`
}

type serverSection struct {
	Env            string   `yaml:"env"`
	DataDir        string   `yaml:"data_dir"`
	TrustedProxies []string `yaml:"trusted_proxies"`
}
type listenSection struct {
	Listen string `yaml:"listen"`
}
type grpcSection struct {
	Listen       string `yaml:"listen"`
	EnrollListen string `yaml:"enroll_listen"`
}
type databaseSection struct {
	Host         string       `yaml:"host"`
	Port         int          `yaml:"port"`
	Socket       string       `yaml:"socket"`
	Name         string       `yaml:"name"`
	User         string       `yaml:"user"`
	Password     string       `yaml:"password"`
	PasswordFile string       `yaml:"password_file"`
	TLS          string       `yaml:"tls"`
	Maint        maintSection `yaml:"maint"`
}
type maintSection struct {
	User         string `yaml:"user"`
	Password     string `yaml:"password"`
	PasswordFile string `yaml:"password_file"`
}
type redisSection struct {
	Addr          string `yaml:"addr"`
	DB            int    `yaml:"db"`
	Password      string `yaml:"password"`
	PasswordFile  string `yaml:"password_file"`
	TLS           bool   `yaml:"tls"`
	TLSCAFile     string `yaml:"tls_ca_file"`
	TLSServerName string `yaml:"tls_server_name"`
}
type logSection struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}
type releaseSection struct {
	BaseURL string `yaml:"base_url"`
	CAFile  string `yaml:"ca_file"`
}
type secretsSection struct {
	MasterKeyFile          string `yaml:"master_key_file"`
	RuntimeCredentialsFile string `yaml:"runtime_credentials_file"`
	SetupTokenHashFile     string `yaml:"setup_token_hash_file"`
}
type authSection struct {
	Argon2 argon2Section `yaml:"argon2"`
}
type argon2Section struct {
	MemoryKiB   int `yaml:"memory_kib"`
	Iterations  int `yaml:"iterations"`
	Parallelism int `yaml:"parallelism"`
}

// ServerShapeKeys 返回结构校验认可的全部叶子键路径（排序）；供对照测试与文档使用。
func ServerShapeKeys() []string {
	var out []string
	var walk func(t reflect.Type, prefix string)
	walk = func(t reflect.Type, prefix string) {
		for k, ft := range yamlKeys(t) {
			p := join(prefix, k)
			if ft.Kind() == reflect.Struct {
				walk(ft, p)
				continue
			}
			out = append(out, p)
		}
	}
	walk(reflect.TypeOf(serverShape{}), "")
	sort.Strings(out)
	return out
}

// CheckServerShape 是 server.yaml 的结构校验（Server 二进制不可用时的退路；不能替代 `lingedge-server config check`）：
// 语法/重复键、未知键与种类、类型、口令不得写在文件里、监听地址形态。
func CheckServerShape(b []byte) []Problem {
	doc, probs := ParseYAML(b)
	if doc == nil {
		return probs
	}
	probs = append(probs, doc.CheckShape(reflect.TypeOf(serverShape{}))...)
	if len(probs) > 0 {
		return sortProblems(probs)
	}
	var s serverShape
	if ps := DecodeStrict(b, &s); len(ps) > 0 {
		return sortProblems(ps)
	}
	add := func(path, why string) { probs = append(probs, doc.At(Problem{Path: path, Reason: why})) }
	for _, pw := range []struct{ path, v, env string }{
		{"database.password", s.Database.Password, "LINGEDGE_DATABASE__PASSWORD"},
		{"database.maint.password", s.Database.Maint.Password, "LINGEDGE_DATABASE__MAINT__PASSWORD"},
		{"redis.password", s.Redis.Password, "LINGEDGE_REDIS__PASSWORD"},
	} {
		if pw.v != "" {
			add(pw.path, "不得写在配置文件中；请改用环境变量 "+pw.env+" 或对应的 password_file")
		}
	}
	for _, l := range []struct{ path, v string }{
		{"http.listen", s.HTTP.Listen}, {"grpc.listen", s.GRPC.Listen}, {"grpc.enroll_listen", s.GRPC.EnrollListen}, {"metrics.listen", s.Metrics.Listen}, {"redis.addr", s.Redis.Addr},
	} {
		if l.v == "" {
			continue
		}
		if _, port, err := net.SplitHostPort(l.v); err != nil {
			add(l.path, "必须是 host:port")
		} else if n, perr := strconv.Atoi(port); perr != nil || n < 0 || n > 65535 {
			add(l.path, "端口必须在 0–65535")
		}
	}
	return sortProblems(probs)
}

package installer

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/wuekevin/lingedge/internal/tools/downloader"
)

// ToolsConfig 是 tools.yaml（/etc/lingedge/tools/tools.yaml，tools-design §3 的 --config）里安装编排用到的部分：
//
//	release:
//	  base_url: https://releases.example.org   # 发行源（UD-3 未定，无默认值）；清单在 <base_url>/<channel>/manifest.json(.sig)
//	  ca_file: /etc/lingedge/release-ca.pem    # 追加根证书（绝对路径，可选）
//	  proxy: http://proxy.internal:3128        # 显式代理（http|https|socks5）；缺省遵循 HTTPS_PROXY/NO_PROXY
//	  resume_dir: /var/lib/lingedge/tools/downloads  # 跨调用断点续传缓存（绝对路径；缺省 <状态目录>/downloads）
//	install:
//	  health_timeout: 60s                      # 步骤 20 健康检查窗口（缺省 60s，[ASSUMPTION]：设计文档「待基线测试后确定」）
//	  health_interval: 1s
//
//	log:                                       # 全局 --log-level/--log-file 的缺省（命令行显式给出时以命令行为准）
//	  level: info                              # debug|info|warn|error
//	  file: /var/log/lingedge/tools.log        # 绝对路径
//	backup:
//	  dir: /var/lib/lingedge/backups           # backup list 与 doctor backup.* 检查的目录（绝对路径；缺省即该值）
//	  recent_max_age: 168h                     # doctor backup.recent 的阈值（缺省 168h = 7 天；tools-design §19 称「默认待用户确认」，这是 [ASSUMPTION]）
//	  custody_fingerprint: 0123456789abcdef    # 运营者确认「主密钥已独立保管」时记录的主密钥指纹（16–64 位十六进制前缀；doctor backup.master_key_separate 据此判定）
//	doctor:
//	  min_memory_mb: 256                       # 清单没有给出系统要求时的内存下限（MiB；清单要求优先）
//	  min_disk_mb: 512                         # 同上，磁盘（MiB）
//	  nofile_min: 4096                         # Tools 进程 RLIMIT_NOFILE 软限制下限（告警）
//	  time_skew_warn: 5s                       # sys.time_sync：时钟偏移告警阈值
//	  time_skew_fail: 5m                       # 失败阈值（也是「回拨」判定的容差）
//	  time_sync_servers: [ntp.example.org:123] # 可选：SNTP 时间源（缺省不联网，只读 timedatectl）
//	  restarts_warn: 3                         # svc.<c>.restarts：systemd NRestarts 告警阈值
//	  offline: false                           # true = doctor/list --available 不做任何联网（清单、SNTP）
//
// 全部顶层节与键都由本结构严格校验：未知键、重复键、类型不符、取值非法一律拒绝（不静默忽略拼写错误；位置见 cfgcheck 的逐行报告）。
type ToolsConfig struct {
	Release ReleaseConfig `yaml:"release"`
	Install InstallConfig `yaml:"install"`
	Log     LogConfig     `yaml:"log"`
	Backup  BackupConfig  `yaml:"backup"`
	Doctor  DoctorConfig  `yaml:"doctor"`
}

// LogConfig 是 log 节。
type LogConfig struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

// BackupConfig 是 backup 节。
type BackupConfig struct {
	Dir                string `yaml:"dir"`
	RecentMaxAge       string `yaml:"recent_max_age"`
	CustodyFingerprint string `yaml:"custody_fingerprint"`
}

// DoctorConfig 是 doctor 节。
type DoctorConfig struct {
	MinMemoryMB     int      `yaml:"min_memory_mb"`
	MinDiskMB       int      `yaml:"min_disk_mb"`
	NofileMin       int      `yaml:"nofile_min"`
	TimeSkewWarn    string   `yaml:"time_skew_warn"`
	TimeSkewFail    string   `yaml:"time_skew_fail"`
	TimeSyncServers []string `yaml:"time_sync_servers"`
	RestartsWarn    int      `yaml:"restarts_warn"`
	Offline         bool     `yaml:"offline"`
}

// ReleaseConfig 是 release 节。
type ReleaseConfig struct {
	BaseURL   string `yaml:"base_url"`
	CAFile    string `yaml:"ca_file"`
	Proxy     string `yaml:"proxy"`
	ResumeDir string `yaml:"resume_dir"`
}

// InstallConfig 是 install 节。
type InstallConfig struct {
	HealthTimeout  string `yaml:"health_timeout"`
	HealthInterval string `yaml:"health_interval"`
}

const (
	defaultHealthTimeout  = 60 * time.Second
	defaultHealthInterval = time.Second
	maxToolsConfigBytes   = 1 << 20
)

// LoadToolsConfig 读取并校验 tools.yaml。explicit 为 true（用户传了 --config）时文件必须存在；否则不存在 = 缺省配置。
func LoadToolsConfig(path string, explicit bool) (ToolsConfig, error) {
	var cfg ToolsConfig
	if path == "" {
		return cfg, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && !explicit {
			return cfg, nil
		}
		return cfg, wrap(KindConfig, err, "无法读取 tools.yaml（%s）", path)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxToolsConfigBytes+1))
	if err != nil {
		return cfg, wrap(KindConfig, err, "无法读取 tools.yaml（%s）", path)
	}
	if len(b) > maxToolsConfigBytes {
		return cfg, newErr(KindConfig, "tools.yaml 过大（>%d 字节）", maxToolsConfigBytes)
	}
	return ParseToolsConfig(b)
}

// ParseToolsConfig 解析并校验 tools.yaml 内容。
func ParseToolsConfig(b []byte) (ToolsConfig, error) {
	var cfg ToolsConfig
	var top map[string]yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&top); err != nil && !errors.Is(err, io.EOF) {
		return cfg, newErr(KindConfig, "tools.yaml 不是合法的 YAML 映射：%s", sanitizeYAML(err))
	}
	for k := range top {
		if !ToolsConfigSections[k] {
			return cfg, newErr(KindConfig, "tools.yaml 含未知的顶层节 %q（可用：release、install、log、backup、doctor）", k)
		}
	}
	for _, sec := range []struct {
		name string
		dst  any
	}{{"release", &cfg.Release}, {"install", &cfg.Install}, {"log", &cfg.Log}, {"backup", &cfg.Backup}, {"doctor", &cfg.Doctor}} {
		n, ok := top[sec.name]
		if !ok {
			continue
		}
		raw, err := yaml.Marshal(&n)
		if err != nil {
			return cfg, newErr(KindConfig, "tools.yaml 的 %s 节无法解析", sec.name)
		}
		d := yaml.NewDecoder(bytes.NewReader(raw))
		d.KnownFields(true)
		if err := d.Decode(sec.dst); err != nil && !errors.Is(err, io.EOF) {
			return cfg, newErr(KindConfig, "tools.yaml 的 %s 节不合法：%s", sec.name, sanitizeYAML(err))
		}
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// sanitizeYAML 去掉 yaml 错误里可能回显的标量取值（形如 `abc`）。
func sanitizeYAML(err error) string {
	s := strings.TrimPrefix(err.Error(), "yaml: ")
	if i := strings.IndexByte(s, '`'); i >= 0 {
		if j := strings.LastIndexByte(s, '`'); j > i {
			s = s[:i] + "<值已隐去>" + s[j+1:]
		}
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ToolsConfigSections 是 tools.yaml 允许的顶层节。
var ToolsConfigSections = map[string]bool{"release": true, "install": true, "log": true, "backup": true, "doctor": true}

// FieldProblem 是 tools.yaml 一个键的取值问题（Path 为点分键路径；Reason 不含任何取值）。
type FieldProblem struct {
	Path   string
	Reason string
}

var reHexPrefix = regexp.MustCompile(`^[0-9a-f]{16,64}$`)

// Problems 返回全部取值问题（不联网、不读文件、按固定顺序）；Validate 返回其中第一个。
func (c ToolsConfig) Problems() []FieldProblem {
	var out []FieldProblem
	add := func(path, why string) { out = append(out, FieldProblem{Path: path, Reason: why}) }
	r := c.Release
	if r.BaseURL != "" {
		if err := downloader.CheckURL(r.BaseURL); err != nil {
			add("release.base_url", "必须是不含凭据的 https 地址")
		}
	}
	if r.Proxy != "" {
		if u, err := url.Parse(r.Proxy); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5") {
			add("release.proxy", "必须是 http|https|socks5 代理地址")
		}
	}
	for _, p := range []struct{ k, v string }{{"release.ca_file", r.CAFile}, {"release.resume_dir", r.ResumeDir}, {"log.file", c.Log.File}, {"backup.dir", c.Backup.Dir}} {
		if p.v != "" && (!filepath.IsAbs(p.v) || strings.ContainsRune(p.v, 0) || filepath.Clean(p.v) != p.v) {
			add(p.k, "必须是规范的绝对路径")
		}
	}
	if _, err := c.HealthTimeout(); err != nil {
		add("install.health_timeout", "必须是 (0, 24h] 内的时长（如 60s）")
	}
	if _, err := c.HealthInterval(); err != nil {
		add("install.health_interval", "必须是 (0, 24h] 内的时长（如 1s）")
	}
	if l := c.Log.Level; l != "" && l != "debug" && l != "info" && l != "warn" && l != "error" {
		add("log.level", "必须是 debug|info|warn|error")
	}
	if v := c.Backup.RecentMaxAge; v != "" {
		if d, err := time.ParseDuration(v); err != nil || d < time.Hour || d > 24*365*time.Hour {
			add("backup.recent_max_age", "必须是 [1h, 8760h] 内的时长（如 168h）")
		}
	}
	if v := c.Backup.CustodyFingerprint; v != "" && !reHexPrefix.MatchString(v) {
		add("backup.custody_fingerprint", "必须是 16–64 位小写十六进制（主密钥指纹前缀）")
	}
	d := c.Doctor
	for _, n := range []struct {
		k      string
		v      int
		lo, hi int
	}{{"doctor.min_memory_mb", d.MinMemoryMB, 0, 1 << 22}, {"doctor.min_disk_mb", d.MinDiskMB, 0, 1 << 30}, {"doctor.nofile_min", d.NofileMin, 0, 1 << 24}, {"doctor.restarts_warn", d.RestartsWarn, 0, 10000}} {
		if n.v < n.lo || n.v > n.hi {
			add(n.k, "超出允许范围")
		}
	}
	warn, werr := c.TimeSkewWarn()
	fail, ferr := c.TimeSkewFail()
	if werr != nil {
		add("doctor.time_skew_warn", "必须是 (0, 24h] 内的时长（如 5s）")
	}
	if ferr != nil {
		add("doctor.time_skew_fail", "必须是 (0, 24h] 内的时长（如 5m）")
	}
	if werr == nil && ferr == nil && warn >= fail {
		add("doctor.time_skew_warn", "必须小于 doctor.time_skew_fail")
	}
	if len(d.TimeSyncServers) > 8 {
		add("doctor.time_sync_servers", "最多 8 个时间源")
	}
	for _, sv := range d.TimeSyncServers {
		if err := CheckHostPort(sv); err != nil {
			add("doctor.time_sync_servers", "每项必须是 host:port（主机名或 IP，端口 1–65535）")
			break
		}
	}
	return out
}

// CheckHostPort 校验 host:port（host 为 IP 或主机名；不含空白/控制字符）。
func CheckHostPort(v string) error {
	h, p, err := net.SplitHostPort(v)
	if err != nil || h == "" {
		return errors.New("不是 host:port")
	}
	if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
		return errors.New("端口非法")
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return nil
	}
	if len(h) > 253 || !reNTPHost.MatchString(h) {
		return errors.New("主机名非法")
	}
	return nil
}

var reNTPHost = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// Validate 校验取值（不联网、不读文件）：返回第一个问题。
func (c ToolsConfig) Validate() error {
	if ps := c.Problems(); len(ps) > 0 {
		return newErr(KindConfig, "%s %s", ps[0].Path, ps[0].Reason)
	}
	return nil
}

func parsePositiveDuration(key, v string, def time.Duration) (time.Duration, error) {
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 || d > 24*time.Hour {
		return 0, newErr(KindConfig, "%s 必须是 (0, 24h] 内的时长（如 60s）", key)
	}
	return d, nil
}

// HealthTimeout 是步骤 20 的窗口。
func (c ToolsConfig) HealthTimeout() (time.Duration, error) {
	return parsePositiveDuration("install.health_timeout", c.Install.HealthTimeout, defaultHealthTimeout)
}

// HealthInterval 是步骤 20 的探测间隔。
func (c ToolsConfig) HealthInterval() (time.Duration, error) {
	return parsePositiveDuration("install.health_interval", c.Install.HealthInterval, defaultHealthInterval)
}

// ManifestURL 返回通道清单的地址：<base_url>/<channel>/manifest.json（[ASSUMPTION]：UD-3 发行源布局未定，签名在其后加 .sig）。
func (c ToolsConfig) ManifestURL(channel string) (string, error) {
	if c.Release.BaseURL == "" {
		return "", &Error{Kind: KindPrecondition, Msg: "未配置 release.base_url，无法在线获取清单",
			Hint: "在 tools.yaml 里设置 release.base_url（发行源地址，UD-3），或用 --offline <bundle.tar> 从离线发行包安装"}
	}
	return strings.TrimRight(c.Release.BaseURL, "/") + "/" + channel + "/manifest.json", nil
}

// DownloaderConfig 构造 downloader 的传输配置：release.ca_file、release.proxy、断点续传目录。
// withResume 为 false（--dry-run）时不启用续传缓存——dry-run 不得创建任何目录。
func (c ToolsConfig) DownloaderConfig(defaultResumeDir string, withResume bool) downloader.Config {
	cfg := downloader.Config{CAFile: c.Release.CAFile, ProxyURL: c.Release.Proxy}
	if withResume {
		cfg.ResumeDir = c.Release.ResumeDir
		if cfg.ResumeDir == "" {
			cfg.ResumeDir = defaultResumeDir
		}
	}
	return cfg
}

func (c ToolsConfig) skew(key, v string, def time.Duration) (time.Duration, error) {
	return parsePositiveDuration(key, v, def)
}

// TimeSkewWarn 是 sys.time_sync 的偏移告警阈值（缺省 5s）。
func (c ToolsConfig) TimeSkewWarn() (time.Duration, error) {
	return c.skew("doctor.time_skew_warn", c.Doctor.TimeSkewWarn, 5*time.Second)
}

// TimeSkewFail 是 sys.time_sync 的失败阈值与回拨容差（缺省 5m）。
func (c ToolsConfig) TimeSkewFail() (time.Duration, error) {
	return c.skew("doctor.time_skew_fail", c.Doctor.TimeSkewFail, 5*time.Minute)
}

// BackupRecentMaxAge 是 backup.recent 的阈值（缺省 168h）。
func (c ToolsConfig) BackupRecentMaxAge() time.Duration {
	if d, err := time.ParseDuration(c.Backup.RecentMaxAge); err == nil && c.Backup.RecentMaxAge != "" {
		return d
	}
	return 168 * time.Hour
}

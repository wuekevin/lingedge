package installer

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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
// 本 Story 只读取并严格校验上述两节；其他顶层节（log、backup、doctor…）属 1.42（config validate）的 schema，此处忽略而不拒绝。
// 两节内出现未知键、重复键、类型不符一律拒绝（不静默忽略拼写错误）。
type ToolsConfig struct {
	Release ReleaseConfig `yaml:"release"`
	Install InstallConfig `yaml:"install"`
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
	for _, sec := range []struct {
		name string
		dst  any
	}{{"release", &cfg.Release}, {"install", &cfg.Install}} {
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

// Validate 校验取值（不联网、不读文件）。
func (c ToolsConfig) Validate() error {
	r := c.Release
	if r.BaseURL != "" {
		if err := downloader.CheckURL(r.BaseURL); err != nil {
			return wrap(KindConfig, err, "release.base_url 不合法（必须是不含凭据的 https 地址）")
		}
	}
	for _, p := range []struct{ k, v string }{{"release.ca_file", r.CAFile}, {"release.resume_dir", r.ResumeDir}} {
		if p.v != "" && (!filepath.IsAbs(p.v) || strings.ContainsRune(p.v, 0)) {
			return newErr(KindConfig, "%s 必须是绝对路径", p.k)
		}
	}
	if _, err := c.HealthTimeout(); err != nil {
		return err
	}
	if _, err := c.HealthInterval(); err != nil {
		return err
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

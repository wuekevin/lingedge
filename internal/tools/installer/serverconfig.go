package installer

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/internal/tools/security"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// 本文件是步骤 18 对 server 的内建生成（Story 1.32 留给本 Story 的三件事）：
//   - server.yaml：格式与 internal/platform/config 一致（tests/tools/serverconfig 用真实 config.Parse 加载并校验生成物）；
//     不含任何口令（NFR-6）；secrets.master_key_file 由 service.MasterKeyConfigValue 决定（LoadCredential 模式必须留空，降级模式填主密钥文件路径，ADR-027）。
//   - master.key：crypto/rand 的 32 字节 KEK，格式见 docs/operations/secrets.md §2（k1:<base64url>），经 service.FileWriter（O_EXCL，从不覆盖）创建。
//   - setup-token.hash：pkg/setuptoken 的 SHA-256 哈希文件（token 仅在步骤 22 显示一次，不落盘、不进日志与事件）。
// Tools 不 import Server 的 internal 包（architecture §5.3 规则 5），所以这里手写 YAML 而不复用 config.Config；一致性由上面的跨包测试保证。

// ServerYAMLParams 是 server.yaml 的变量。
type ServerYAMLParams struct {
	Env            string   // 环境名（Redis 键空间 le:<env>:…）
	DataDir        string   // /var/lib/lingedge/server
	HTTPListen     string   // 安装向导/REST 监听，默认 127.0.0.1:8080
	GRPCListen     string   // 节点 mTLS gRPC，默认 0.0.0.0:9443
	EnrollListen   string   // 节点注册（有未过期注册令牌时才真正监听），默认 0.0.0.0:9444
	MetricsListen  string   // Prometheus 指标，必须是回环地址，默认 127.0.0.1:9180
	TrustedProxies []string // IP 或 CIDR
	// MasterKeyFile 是 secrets.master_key_file 的取值（service.MasterKeyConfigValue 的结果；空 = LoadCredential）。
	MasterKeyFile string
}

var reEnvName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)

// DefaultServerYAMLParams 返回缺省值（env=prod，与 configs/server.example.yaml 一致）。
func DefaultServerYAMLParams() ServerYAMLParams {
	return ServerYAMLParams{Env: "prod", DataDir: service.ServerDataDir, HTTPListen: "127.0.0.1:8080", GRPCListen: "0.0.0.0:9443", EnrollListen: "0.0.0.0:9444",
		MetricsListen: "127.0.0.1:9180", TrustedProxies: []string{"127.0.0.1", "::1"}}
}

// Validate 校验全部取值（任何不合法即失败，不渲染部分结果）。
func (p ServerYAMLParams) Validate() error {
	if !reEnvName.MatchString(p.Env) {
		return fmt.Errorf("env 须匹配 %s", reEnvName)
	}
	if err := security.ValidateUnitPath(p.DataDir); err != nil {
		return fmt.Errorf("data_dir 不合法：%w", err)
	}
	for _, l := range []struct{ name, v string }{{"http.listen", p.HTTPListen}, {"grpc.listen", p.GRPCListen}, {"grpc.enroll_listen", p.EnrollListen}, {"metrics.listen", p.MetricsListen}} {
		if err := validListen(l.name, l.v); err != nil {
			return err
		}
	}
	if !isLoopbackListen(p.MetricsListen) {
		return fmt.Errorf("metrics.listen 必须是回环地址（Server 拒绝启动其他绑定，Story 1.22b）")
	}
	if len(p.TrustedProxies) == 0 || len(p.TrustedProxies) > 32 {
		return fmt.Errorf("trusted_proxies 须有 1–32 项")
	}
	for _, t := range p.TrustedProxies {
		if _, perr := netip.ParsePrefix(t); perr != nil {
			if _, aerr := netip.ParseAddr(t); aerr != nil {
				return fmt.Errorf("trusted_proxies 含非法的 IP/CIDR")
			}
		}
	}
	if p.MasterKeyFile != "" {
		if err := security.ValidateUnitPath(p.MasterKeyFile); err != nil {
			return fmt.Errorf("master_key_file 不合法：%w", err)
		}
	}
	return nil
}

// validListen 校验 <主机>:<端口>（主机为空/IP 字面量/主机名；端口 1–65535）。
func validListen(name, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s 须是 host:port", name)
	}
	if n, perr := strconv.Atoi(port); perr != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%s 的端口不合法", name)
	}
	if host != "" {
		if _, perr := netip.ParseAddr(host); perr != nil && !reHostName.MatchString(host) {
			return fmt.Errorf("%s 的主机不合法", name)
		}
	}
	return nil
}

// isLoopbackListen 报告 <IP>:<端口> 的主机是否为回环 IP 字面量（空主机/通配/主机名一律否）。
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	return err == nil && ip.IsLoopback()
}

var reHostName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$`)

// RenderServerYAML 渲染 server.yaml（确定性：同样的输入逐字节相同）。
func RenderServerYAML(p ServerYAMLParams) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("# lingedge-server 配置——由 lingedge install server 生成（已存在的配置绝不会被覆盖，新内容会写成 server.yaml.new）。\n")
	b.WriteString("# 不含任何口令或密钥：数据库/Redis 口令由安装向导写入运行期凭据文件（ADR-026）；主密钥见 secrets 节。\n")
	b.WriteString("server:\n")
	fmt.Fprintf(&b, "  env: %s\n", p.Env)
	fmt.Fprintf(&b, "  data_dir: %s\n", p.DataDir)
	b.WriteString("  trusted_proxies: [")
	for i, t := range p.TrustedProxies {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "'%s'", t)
	}
	b.WriteString("]\n")
	b.WriteString("http:\n")
	fmt.Fprintf(&b, "  listen: %s\n", p.HTTPListen)
	b.WriteString("grpc:\n")
	fmt.Fprintf(&b, "  listen: %s\n", p.GRPCListen)
	fmt.Fprintf(&b, "  enroll_listen: %s\n", p.EnrollListen)
	b.WriteString("metrics:\n")
	fmt.Fprintf(&b, "  listen: %s\n", p.MetricsListen)
	b.WriteString("secrets:\n")
	if p.MasterKeyFile != "" {
		b.WriteString("  # 降级模式（systemd < 247，无 LoadCredential=）：主密钥文件由 Server 直接读取（ADR-027）。\n")
		fmt.Fprintf(&b, "  master_key_file: %s\n", p.MasterKeyFile)
	} else {
		b.WriteString("  # 留空：Server 读取 $CREDENTIALS_DIRECTORY/master.key（systemd LoadCredential，ADR-011）。\n")
		b.WriteString("  master_key_file: ''\n")
	}
	return []byte(b.String()), nil
}

// serverSettingKeys 是 server 的内建 --set 键。
var serverSettingKeys = map[string]bool{"env": true, "trusted-proxies": true}

// ServerYAMLFromSettings 由 --set 的内建键与向导监听地址构造 server.yaml 的变量。
// 键：env（环境名）、trusted-proxies（逗号分隔的 IP/CIDR）。未知键返回 (_, false, nil)，由 ComponentSpec.ValidateSettings 决定。
func ServerYAMLFromSettings(settings map[string]string, setupListen string, cred service.CredentialMode) (ServerYAMLParams, error) {
	return ServerYAMLFromPlan(settings, setupListen, cred, nil)
}

// DefaultSetupListen 是安装向导的缺省监听（仅本机，SSH 隧道访问）。
const DefaultSetupListen = "127.0.0.1:8080"

// ServerListens 是 Server 的四个监听地址（由计划里的端口决定，--set port.server… 换端口后 server.yaml、健康检查与访问地址随之一致）。
type ServerListens struct{ HTTP, GRPC, Enroll, Metrics string }

// ServerListensFromPlan 由 --setup-listen 与计划端口（resolver.Plan.Ports 里 component=server 的项，按 Purpose 识别）得到监听地址。
// 规则：ports 为空 → 缺省值（http 取 setupListen）；REST 端口被 --set port.server.tcp.8080=… 换过 → http 监听取其绑定地址与新端口，
// 此时同时给出与之不一致的 --setup-listen 是矛盾（错误）；其余端口取计划里的绑定地址与端口。
func ServerListensFromPlan(setupListen string, ports []resolver.PlannedPort) (ServerListens, error) {
	if setupListen == "" {
		setupListen = DefaultSetupListen
	}
	l := ServerListens{HTTP: setupListen, GRPC: "0.0.0.0:9443", Enroll: "0.0.0.0:9444", Metrics: "127.0.0.1:9180"}
	for _, p := range ports {
		if p.Component != "server" || p.Proto != "tcp" {
			continue
		}
		addr := net.JoinHostPort(p.Bind, strconv.Itoa(p.Port))
		switch strings.ToLower(p.Purpose) {
		case "rest":
			if p.Overridden {
				if setupListen != DefaultSetupListen && setupListen != addr {
					return l, fmt.Errorf("--setup-listen %s 与 --set 换端口后的 REST 监听 %s 矛盾，二者只能给一个", setupListen, addr)
				}
				l.HTTP = addr
			}
		case "grpc-mtls":
			l.GRPC = addr
		case "grpc-enroll":
			l.Enroll = addr
		case "metrics":
			l.Metrics = addr
		}
	}
	return l, nil
}

// ServerYAMLFromPlan 同 ServerYAMLFromSettings，并让四个监听随计划端口（见 ServerListensFromPlan）。
func ServerYAMLFromPlan(settings map[string]string, setupListen string, cred service.CredentialMode, ports []resolver.PlannedPort) (ServerYAMLParams, error) {
	p := DefaultServerYAMLParams()
	ls, err := ServerListensFromPlan(setupListen, ports)
	if err != nil {
		return p, err
	}
	p.HTTPListen, p.GRPCListen, p.EnrollListen, p.MetricsListen = ls.HTTP, ls.GRPC, ls.Enroll, ls.Metrics
	if v, ok := settings["env"]; ok {
		p.Env = v
	}
	if v, ok := settings["trusted-proxies"]; ok {
		p.TrustedProxies = nil
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				p.TrustedProxies = append(p.TrustedProxies, t)
			}
		}
	}
	p.MasterKeyFile = service.MasterKeyConfigValue(cred, filepath.ToSlash(service.ServerConfigDir+"/master.key"))
	return p, p.Validate()
}

// GenerateMasterKey 生成 master.key 内容：一个 kek_id=k1 的 32 字节 crypto/rand 密钥（docs/operations/secrets.md §2）。
func GenerateMasterKey() ([]byte, error) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("生成主密钥的随机数失败: %w", err)
	}
	defer func() {
		for i := range k {
			k[i] = 0
		}
	}()
	return []byte("# LingEdge 主密钥（lingedge install 生成）。丢失即无法解密已加密的数据：请妥善备份，勿提交到任何仓库。\n@current k1\nk1:" +
		base64.RawURLEncoding.EncodeToString(k) + "\n"), nil
}

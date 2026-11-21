package service

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/security"
)

// CredentialMode 是 Server 主密钥的交付方式（ADR-027）。
type CredentialMode string

// CredentialMode 取值。
const (
	// CredentialSystemd：经 systemd LoadCredential= 注入（systemd >= 247，ADR-011 §2）；master.key 为 root:root 0400。
	CredentialSystemd CredentialMode = "systemd-credentials"
	// CredentialKeyFile：降级。不写 LoadCredential=，Server 经 secrets.master_key_file 读取 root:<运行组> 0440 的主密钥文件。
	CredentialKeyFile CredentialMode = "keyfile"
)

// ServerRoles 是 --roles 的合法取值（与 cmd/lingedge-server 一致）。
var ServerRoles = []string{"api", "grpc", "scheduler", "worker"}

// ServerParams 是 lingedge-server.service 的全部变量（其余内容是模板里的固定文本）。
type ServerParams struct {
	User, Group   string
	ExecPath      string   // 如 /opt/lingedge/server/current/bin/lingedge-server
	ConfigPath    string   // 如 /etc/lingedge/server/server.yaml
	Roles         []string // ServerRoles 的非空子集
	MasterKeyPath string   // 如 /etc/lingedge/server/master.key
	Credential    CredentialMode
	DocsHost      string // 可空；非空则渲染 Documentation=https://<host>/
	LimitNOFILE   int
}

// DefaultServerParams 返回 tools-design §15.1 的默认值。
func DefaultServerParams() ServerParams {
	return ServerParams{
		User: UserServer, Group: UserServer,
		ExecPath:      "/opt/lingedge/server/current/bin/lingedge-server",
		ConfigPath:    ServerConfigDir + "/server.yaml",
		Roles:         []string{"api", "grpc", "scheduler", "worker"},
		MasterKeyPath: ServerConfigDir + "/master.key",
		Credential:    CredentialSystemd,
		LimitNOFILE:   65536,
	}
}

// NodeParams 是 Node 侧四个单元（agent / openresty / 两个 path / 升级 service）的全部变量。
type NodeParams struct {
	User, Group     string
	AgentExec       string // 如 /opt/lingedge/node/current/lingedge-node
	AgentConfig     string // 如 /etc/lingedge/node/node.yaml
	OpenrestyBin    string // 如 /opt/lingedge/openresty/current/bin/openresty
	ToolsBin        string // 如 /usr/local/bin/lingedge（升级单元的入口）
	AgentNOFILE     int
	OpenrestyNOFILE int
}

// DefaultNodeParams 返回 tools-design §15.2/§15.3 的默认值。
func DefaultNodeParams() NodeParams {
	return NodeParams{
		User: UserNode, Group: UserNode,
		AgentExec:       "/opt/lingedge/node/current/lingedge-node",
		AgentConfig:     NodeConfigDir + "/node.yaml",
		OpenrestyBin:    "/opt/lingedge/openresty/current/bin/openresty",
		ToolsBin:        "/usr/local/bin/lingedge",
		AgentNOFILE:     65536,
		OpenrestyNOFILE: 262144,
	}
}

var (
	reAccount = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	reHost    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}(:[0-9]{1,5})?$`)
)

const (
	minNOFILE = 1024
	maxNOFILE = 1 << 24
)

// checkValue 先过 security.ValidateUnitValue（唯一的字符集与路径规则），再叠加字段自己的约束。
func checkValue(field, v string, extra func(string) error) error {
	if err := security.ValidateUnitValue(v); err != nil {
		return paramErr(field, err)
	}
	if extra != nil {
		if err := extra(v); err != nil {
			return paramErr(field, err)
		}
	}
	return nil
}

func isPathField(v string) error {
	if err := security.ValidateUnitPath(v); err != nil {
		return err
	}
	return nil
}

func isAccount(v string) error {
	if v == "root" {
		return errors.New("运行用户/组不得是 root")
	}
	if !reAccount.MatchString(v) {
		return errors.New("须匹配 ^[a-z_][a-z0-9_-]{0,31}$")
	}
	return nil
}

func checkNOFILE(field string, n int) error {
	if n < minNOFILE || n > maxNOFILE {
		return paramErr(field, fmt.Errorf("须在 %d..%d 内", minNOFILE, maxNOFILE))
	}
	return nil
}

// Validate 校验 Server 参数；任何一项不合法即失败（不渲染部分结果）。
func (p ServerParams) Validate() error {
	for _, c := range []struct {
		f, v  string
		extra func(string) error
	}{
		{"User", p.User, isAccount}, {"Group", p.Group, isAccount},
		{"ExecPath", p.ExecPath, isPathField}, {"ConfigPath", p.ConfigPath, isPathField}, {"MasterKeyPath", p.MasterKeyPath, isPathField},
	} {
		if err := checkValue(c.f, c.v, c.extra); err != nil {
			return err
		}
	}
	if p.DocsHost != "" {
		if err := checkValue("DocsHost", p.DocsHost, func(v string) error {
			if !reHost.MatchString(v) || strings.HasSuffix(v, "-") || strings.HasSuffix(v, ".") {
				return errors.New("须是主机名[:端口]")
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if len(p.Roles) == 0 {
		return paramErr("Roles", errors.New("不能为空"))
	}
	seen := map[string]bool{}
	for _, r := range p.Roles {
		if err := checkValue("Roles", r, func(v string) error {
			if !containsStr(ServerRoles, v) {
				return fmt.Errorf("%q 不是合法角色（%s）", v, strings.Join(ServerRoles, ","))
			}
			return nil
		}); err != nil {
			return err
		}
		if seen[r] {
			return paramErr("Roles", fmt.Errorf("角色 %q 重复", r))
		}
		seen[r] = true
	}
	switch p.Credential {
	case CredentialSystemd, CredentialKeyFile:
	default:
		return paramErr("Credential", fmt.Errorf("未知交付方式 %q", string(p.Credential)))
	}
	return checkNOFILE("LimitNOFILE", p.LimitNOFILE)
}

// Validate 校验 Node 参数。
func (p NodeParams) Validate() error {
	for _, c := range []struct {
		f, v  string
		extra func(string) error
	}{
		{"User", p.User, isAccount}, {"Group", p.Group, isAccount},
		{"AgentExec", p.AgentExec, isPathField}, {"AgentConfig", p.AgentConfig, isPathField},
		{"OpenrestyBin", p.OpenrestyBin, isPathField}, {"ToolsBin", p.ToolsBin, isPathField},
	} {
		if err := checkValue(c.f, c.v, c.extra); err != nil {
			return err
		}
	}
	if err := checkNOFILE("AgentNOFILE", p.AgentNOFILE); err != nil {
		return err
	}
	return checkNOFILE("OpenrestyNOFILE", p.OpenrestyNOFILE)
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

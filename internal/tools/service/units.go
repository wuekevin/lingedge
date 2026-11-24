package service

import (
	"embed"
	"fmt"
	"strings"
	"text/template"
)

// UnitName 是 LingEdge 管理的 systemd 单元名。systemctl 封装与撤销只认下面这个固定集合——
// 因此单元名永远不会来自自由文本，也不可能以 `-` 开头被当成选项。
type UnitName string

// 单元名（tools-design §15）。
const (
	UnitServer         UnitName = "lingedge-server.service"
	UnitNode           UnitName = "lingedge-node.service"
	UnitOpenresty      UnitName = "lingedge-openresty.service"
	UnitOpenrestyPath  UnitName = "lingedge-openresty.path"
	UnitUpgradePath    UnitName = "lingedge-node-upgrade.path"
	UnitUpgradeService UnitName = "lingedge-node-upgrade.service"
)

// AllUnits 是全部已知单元（顺序固定）。
var AllUnits = []UnitName{UnitServer, UnitNode, UnitOpenresty, UnitOpenrestyPath, UnitUpgradePath, UnitUpgradeService}

// Valid 报告 u 是否是已知单元。
func (u UnitName) Valid() bool {
	for _, k := range AllUnits {
		if u == k {
			return true
		}
	}
	return false
}

// String 返回单元文件名。
func (u UnitName) String() string { return string(u) }

//go:embed templates/*.tmpl
var templateFS embed.FS

var tmpls = func() map[UnitName]*template.Template {
	out := map[UnitName]*template.Template{}
	for _, u := range AllUnits {
		b, err := templateFS.ReadFile("templates/" + string(u) + ".tmpl")
		if err != nil {
			panic(fmt.Sprintf("service: 缺少内置模板 %s: %v", u, err)) // 编译期嵌入，不可能缺失
		}
		t, err := template.New(string(u)).Option("missingkey=error").Funcs(template.FuncMap{"join": strings.Join}).Parse(string(b))
		if err != nil {
			panic(fmt.Sprintf("service: 模板 %s 解析失败: %v", u, err))
		}
		out[u] = t
	}
	return out
}()

// Unit 是一个渲染好的单元。
type Unit struct {
	Name    UnitName
	Content []byte
}

// renderData 是模板的输入：调用方的（已校验的）变量 + 固定路径常量。模板访问任何其他字段都会失败（missingkey=error + 结构体）。
type renderData struct {
	// Server
	User, Group, ExecPath, ConfigPath, MasterKeyPath, DocsHost string
	Roles                                                      []string
	Credential                                                 CredentialMode
	LimitNOFILE                                                int
	// Node
	AgentExec, AgentConfig, OpenrestyBin, ToolsBin string
	AgentNOFILE, OpenrestyNOFILE                   int
	NodeCurrentDir, NodeStagingDir                 string
}

func serverData(p ServerParams) renderData {
	return renderData{User: p.User, Group: p.Group, ExecPath: p.ExecPath, ConfigPath: p.ConfigPath, MasterKeyPath: p.MasterKeyPath,
		DocsHost: p.DocsHost, Roles: p.Roles, Credential: p.Credential, LimitNOFILE: p.LimitNOFILE}
}

func nodeData(p NodeParams) renderData {
	return renderData{User: p.User, Group: p.Group, AgentExec: p.AgentExec, AgentConfig: p.AgentConfig, OpenrestyBin: p.OpenrestyBin, ToolsBin: p.ToolsBin,
		AgentNOFILE: p.AgentNOFILE, OpenrestyNOFILE: p.OpenrestyNOFILE, NodeCurrentDir: NodeCurrentDir, NodeStagingDir: NodeStagingDir}
}

func execute(u UnitName, d renderData) (Unit, error) {
	var sb strings.Builder
	if err := tmpls[u].Execute(&sb, d); err != nil {
		return Unit{}, wrapErr(KindInvalidParam, err, "渲染 %s 失败", u)
	}
	return Unit{Name: u, Content: []byte(sb.String())}, nil
}

// RenderServer 渲染 lingedge-server.service。参数先整体校验，任何一项不合法即失败。
func RenderServer(p ServerParams) (Unit, error) {
	if err := p.Validate(); err != nil {
		return Unit{}, err
	}
	return execute(UnitServer, serverData(p))
}

// RenderNode 渲染 Node 侧的一个单元（agent / openresty / openresty.path / upgrade.path / upgrade.service）。
func RenderNode(u UnitName, p NodeParams) (Unit, error) {
	switch u {
	case UnitNode, UnitOpenresty, UnitOpenrestyPath, UnitUpgradePath, UnitUpgradeService:
	default:
		return Unit{}, newErr(KindInvalidParam, "%s 不是 Node 单元", u)
	}
	if err := p.Validate(); err != nil {
		return Unit{}, err
	}
	return execute(u, nodeData(p))
}

// NodeUnits 渲染 Node 的全部五个单元（顺序固定）。
func NodeUnits(p NodeParams) ([]Unit, error) {
	var out []Unit
	for _, u := range []UnitName{UnitNode, UnitOpenresty, UnitOpenrestyPath, UnitUpgradePath, UnitUpgradeService} {
		r, err := RenderNode(u, p)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// EnableTargets 返回安装后应 enable 的单元。注意 OpenResty 用 .path 而不是 .service（tools-design §15.2：
// 首个配置代出现前不能直接启动 OpenResty；此后重启自启由 path 单元 + Restart=always 保证，FR-57/FR-62）。
func EnableTargets(component string) []UnitName {
	switch component {
	case "server":
		return []UnitName{UnitServer}
	case "node":
		return []UnitName{UnitNode, UnitOpenrestyPath, UnitUpgradePath}
	}
	return nil
}

// RunUnits 返回 `service start|stop|restart` 的作用对象：组件自己的服务单元。
// node 只含 Agent（lingedge-node.service）：OpenResty 数据面按 ADR-020「节点自治」独立于 Agent 运行，Tools 不会因为停止 Agent 而停掉数据面。
// 没有服务单元的组件（静态前端、尚未交付的组件）返回 nil。
func RunUnits(component string) []UnitName {
	switch component {
	case "server":
		return []UnitName{UnitServer}
	case "node":
		return []UnitName{UnitNode}
	}
	return nil
}

// StatusUnits 返回 `status`/`service status`/`doctor` 观察的全部单元（含 node 的 OpenResty 与 path 单元）。
func StatusUnits(component string) []UnitName {
	switch component {
	case "server":
		return []UnitName{UnitServer}
	case "node":
		return []UnitName{UnitNode, UnitOpenresty, UnitOpenrestyPath, UnitUpgradePath}
	}
	return nil
}

// LogUnits 返回 `logs` 读取的单元：Agent 与它的数据面（OpenResty 的 stdout/stderr 在 journal 里）。
func LogUnits(component string) []UnitName {
	switch component {
	case "server":
		return []UnitName{UnitServer}
	case "node":
		return []UnitName{UnitNode, UnitOpenresty}
	}
	return nil
}

package service

// 固定路径（tools-design §8、ADR-020 §2）。它们是架构约定而不是可配置项：单元模板里写死，目录创建与 doctor 检查共用同一组常量。
const (
	UnitDir = "/etc/systemd/system"

	ServerConfigDir = "/etc/lingedge/server"
	ServerDataDir   = "/var/lib/lingedge/server"
	ServerLogDir    = "/var/log/lingedge"

	NodeConfigDir  = "/etc/lingedge/node"
	NodeDataDir    = "/var/lib/lingedge-node"
	NodeLogDir     = "/var/log/lingedge-node"
	NodeRunDir     = "/run/lingedge-node"
	NodeCurrentDir = NodeDataDir + "/current"
	NodeStagingDir = NodeDataDir + "/staging"
	// NodeUpgradeDir 是升级结果目录（BLK-07）：root:root 0755，Agent 无写权限，Tools 安装 node 时创建。
	NodeUpgradeDir = "/var/lib/lingedge-node-upgrade"
)

// 运行用户（tools-design §8；均为 --system --no-create-home --shell /usr/sbin/nologin）。
const (
	UserServer   = "lingedge"
	UserNode     = "lingedge-node"
	UserDNS      = "lingedge-dns"
	UserWeb      = "lingedge-web"
	UserReporter = "lingedge-reporter"
)

// RunUsers 是 Tools 会创建的全部运行用户（撤销 userdel 只认这个集合）。
var RunUsers = []string{UserServer, UserNode, UserDNS, UserWeb, UserReporter}

// NodeRuntimeFiles 是 /run/lingedge-node 下的运行期文件（ADR-020 §2；由 systemd RuntimeDirectory 与进程创建，不由 Tools 创建）。
var NodeRuntimeFiles = []string{
	NodeRunDir + "/openresty.pid",
	NodeRunDir + "/openresty-ctl.sock",
	NodeRunDir + "/agent.sock",
}

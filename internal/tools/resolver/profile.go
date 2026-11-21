package resolver

import (
	"path"
	"strings"
)

// 部署画像：每个已知组件的运行用户、systemd 单元、将创建的文件（tools-design §8、§11.3、§15）。
// 这是「Tools 对组件落地形态的知识」，不是清单内容（清单只管版本/依赖/端口/包）；未知组件取最小画像（只有 release 目录）。

type unitSpec struct {
	name     string
	kind     string   // service | path
	variants []string // 空 = 所有变体
}

type fileSpec struct {
	path     string // 以 {etc} {var} {opt} 开头的模板，{c} = 组件名
	kind     string
	mode     string
	owner    string
	policy   string
	note     string
	variants []string
}

type profile struct {
	runUser      string
	service      bool // 有 systemd 服务（需要 start/health 步骤）
	units        []unitSpec
	files        []fileSpec
	dataDir      string   // 模板；空 = 无数据目录
	portVariants []string // 清单的 ports 只在这些变体上生效（空 = 所有变体；website 的 3000 只属于 ssr）
	remoteOK     []string // 这些外部依赖未探测到时允许继续（向导里配置远端实例，§10.3 步骤 5）
	static       bool     // 静态前端：产出 Nginx 片段而不是服务
}

func nginxFiles() []fileSpec {
	return []fileSpec{
		{path: "{etc}/nginx/{c}.conf", kind: "file", mode: "0644", owner: "root:root", policy: PolicyManaged, note: "Nginx 站点片段；只生成，不写入任何既有 Nginx 配置（§11.3）"},
		{path: "{etc}/nginx/{c}-security-headers.conf", kind: "file", mode: "0644", owner: "root:root", policy: PolicyManaged, note: "安全头集中文件，每个含 add_header 的 location 显式 include"},
	}
}

var profiles = map[string]profile{
	"server": {
		runUser: "lingedge", service: true, dataDir: "{var}/{c}", remoteOK: []string{"mysql", "redis"},
		units: []unitSpec{{name: "lingedge-server.service", kind: "service"}},
		files: []fileSpec{
			{path: "{etc}/server/server.yaml", kind: "file", mode: "0640", owner: "root:lingedge", policy: PolicyCreateIfAbsent, note: "监听与数据目录；不含口令"},
			{path: "{etc}/server/master.key", kind: "file", mode: "0400", owner: "root:root", policy: PolicyCreateIfAbsent, note: "步骤 18 由 Tools 生成；已存在绝不覆盖"},
			{path: "{etc}/server/setup-token.hash", kind: "file", mode: "0600", owner: "lingedge:lingedge", policy: PolicyCreateIfAbsent, note: "setup token 的 SHA-256"},
		},
	},
	"node": {
		runUser: "lingedge-node", service: true, dataDir: "/var/lib/lingedge-node",
		units: []unitSpec{{name: "lingedge-node.service", kind: "service"}},
		files: []fileSpec{{path: "{etc}/node/node.yaml", kind: "file", mode: "0640", owner: "root:lingedge-node", policy: PolicyCreateIfAbsent}},
	},
	"openresty": {
		runUser: "lingedge-node", service: true,
		units: []unitSpec{{name: "lingedge-openresty.service", kind: "service"}, {name: "lingedge-openresty.path", kind: "path"}},
	},
	"dns": {
		runUser: "lingedge-dns", service: true, dataDir: "{var}/{c}",
		units: []unitSpec{{name: "lingedge-dns.service", kind: "service"}},
		files: []fileSpec{{path: "{etc}/dns/dns.yaml", kind: "file", mode: "0640", owner: "root:lingedge-dns", policy: PolicyCreateIfAbsent}},
	},
	"reporter": {
		runUser: "lingedge-reporter", service: true, dataDir: "{var}/{c}",
		units: []unitSpec{{name: "lingedge-reporter.service", kind: "service"}},
		files: []fileSpec{{path: "{etc}/reporter/reporter.yaml", kind: "file", mode: "0640", owner: "root:lingedge-reporter", policy: PolicyCreateIfAbsent}},
	},
	"website": {
		runUser: "lingedge-web", portVariants: []string{"ssr"},
		units: []unitSpec{{name: "lingedge-website.service", kind: "service", variants: []string{"ssr"}}},
		files: append(nginxFiles(), fileSpec{path: "{etc}/website/revalidate.secret", kind: "file", mode: "0440", owner: "root:lingedge-web", policy: PolicyCreateIfAbsent,
			note: "SSR revalidate 共享密钥（32 字节随机；不写入 state.json 或日志）", variants: []string{"ssr"}}),
	},
	"admin-web": {static: true, files: nginxFiles()},
	"console":   {static: true, files: nginxFiles()},
	"docs":      {static: true, files: nginxFiles()},
	"status":    {static: true, files: nginxFiles()},
}

func profileFor(component string) profile { return profiles[component] }

func inVariants(list []string, variant string) bool {
	if len(list) == 0 {
		return true
	}
	for _, v := range list {
		if v == variant {
			return true
		}
	}
	return false
}

func expand(l Layout, tmpl, component string) string {
	return path.Clean(strings.NewReplacer("{etc}", l.EtcRoot, "{var}", l.VarRoot, "{opt}", l.OptRoot, "{c}", component).Replace(tmpl))
}

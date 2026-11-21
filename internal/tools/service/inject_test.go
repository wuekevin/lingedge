package service

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// injectionVectors 是针对「变量被写进单元文件」这一入口的攻击串：换行注入指令、systemd 说明符、空白/引号/反斜杠/变量展开、
// 路径穿越、控制字符、NUL、非 ASCII 同形字符等。每个向量放进每一个字符串变量里都必须被拒绝。
var injectionVectors = []string{
	"x\nExecStartPre=/bin/evil",
	"x\r\nExecStartPre=/bin/evil",
	"/opt/x\nExecStartPre=/bin/evil",
	"/opt/x\n[Service]\nExecStartPre=/bin/evil",
	"%n", "/opt/%n/bin", "/opt/%h", "%%", "/opt/%i", "%t", "%U",
	"/opt/with space/bin", "/opt/tab\tbin", " /opt/bin", "/opt/bin ",
	`/opt/"quoted"/bin`, "/opt/'q'/bin", `/opt/back\slash`, "/opt/a;b", "/opt/a|b", "/opt/a&b",
	"$HOME", "/opt/$MAINPID", "/opt/${X}", "/opt/`id`", "/opt/$(id)",
	"/opt/../etc/shadow", "../../etc", "/opt/..", "/opt/a..b",
	"relative/path", "opt/bin", "", "/opt//bin", "/opt/./bin", "/opt/bin/",
	"/opt/a\x00b", "/opt/\x01x", "/opt/\x7f", "/opt/é", "/opt/ＡＢ", "/opt/a b", "/opt/a\u0085b",
	strings.Repeat("a", 5000),
	"-rf", "--help",
}

func serverMutators() map[string]func(*ServerParams, string) {
	return map[string]func(*ServerParams, string){
		"User":          func(p *ServerParams, v string) { p.User = v },
		"Group":         func(p *ServerParams, v string) { p.Group = v },
		"ExecPath":      func(p *ServerParams, v string) { p.ExecPath = v },
		"ConfigPath":    func(p *ServerParams, v string) { p.ConfigPath = v },
		"MasterKeyPath": func(p *ServerParams, v string) { p.MasterKeyPath = v },
		"DocsHost":      func(p *ServerParams, v string) { p.DocsHost = v },
		"Roles":         func(p *ServerParams, v string) { p.Roles = []string{v} },
		"Credential":    func(p *ServerParams, v string) { p.Credential = CredentialMode(v) },
	}
}

func nodeMutators() map[string]func(*NodeParams, string) {
	return map[string]func(*NodeParams, string){
		"User":         func(p *NodeParams, v string) { p.User = v },
		"Group":        func(p *NodeParams, v string) { p.Group = v },
		"AgentExec":    func(p *NodeParams, v string) { p.AgentExec = v },
		"AgentConfig":  func(p *NodeParams, v string) { p.AgentConfig = v },
		"OpenrestyBin": func(p *NodeParams, v string) { p.OpenrestyBin = v },
		"ToolsBin":     func(p *NodeParams, v string) { p.ToolsBin = v },
	}
}

func TestServerInjectionVectorsRejected(t *testing.T) {
	for field, set := range serverMutators() {
		for _, v := range injectionVectors {
			if field == "DocsHost" && v == "" {
				continue // DocsHost 可空（省略 Documentation=）
			}
			p := DefaultServerParams()
			set(&p, v)
			u, err := RenderServer(p)
			if err == nil {
				t.Errorf("%s=%q 未被拒绝，渲染出：\n%s", field, v, u.Content)
				continue
			}
			if KindOf(err) != KindInvalidParam {
				t.Errorf("%s=%q：错误类别 %q，应为 invalid_param", field, v, KindOf(err))
			}
		}
	}
}

func TestNodeInjectionVectorsRejected(t *testing.T) {
	for field, set := range nodeMutators() {
		for _, v := range injectionVectors {
			p := DefaultNodeParams()
			set(&p, v)
			us, err := NodeUnits(p)
			if err == nil {
				t.Errorf("%s=%q 未被拒绝：%d 个单元已渲染", field, v, len(us))
			}
		}
	}
}

func TestRolesRejectCommaAndUnknown(t *testing.T) {
	for _, roles := range [][]string{{"api,grpc"}, {"api", "api"}, {"nope"}, {}, nil, {"api", "grpc\nExecStartPre=/x"}, {"API"}} {
		p := DefaultServerParams()
		p.Roles = roles
		if _, err := RenderServer(p); err == nil {
			t.Errorf("Roles=%q 未被拒绝", roles)
		}
	}
}

func TestAccountRules(t *testing.T) {
	for _, bad := range []string{"root", "Root", "1abc", "-abc", strings.Repeat("a", 33), "a b", "a.b", "a@b", "a:b", ""} {
		p := DefaultServerParams()
		p.User = bad
		if _, err := RenderServer(p); err == nil {
			t.Errorf("User=%q 未被拒绝", bad)
		}
	}
	p := DefaultServerParams()
	p.User, p.Group = "svc_x-1", "svc_x-1"
	if _, err := RenderServer(p); err != nil {
		t.Errorf("合法账号被拒绝：%v", err)
	}
}

func TestNOFILEBounds(t *testing.T) {
	for _, n := range []int{-1, 0, 1023, 1<<24 + 1} {
		p := DefaultServerParams()
		p.LimitNOFILE = n
		if _, err := RenderServer(p); err == nil {
			t.Errorf("LimitNOFILE=%d 未被拒绝", n)
		}
		np := DefaultNodeParams()
		np.OpenrestyNOFILE = n
		if _, err := RenderNode(UnitOpenresty, np); err == nil {
			t.Errorf("OpenrestyNOFILE=%d 未被拒绝", n)
		}
	}
}

func TestDocsHostRules(t *testing.T) {
	for _, ok := range []string{"docs.example.com", "a", "docs.example.com:8443", "X-1.y"} {
		p := DefaultServerParams()
		p.DocsHost = ok
		u, err := RenderServer(p)
		if err != nil {
			t.Errorf("DocsHost=%q 被拒绝：%v", ok, err)
			continue
		}
		if !strings.Contains(string(u.Content), "Documentation=https://"+ok+"/\n") {
			t.Errorf("DocsHost=%q 未渲染 Documentation", ok)
		}
	}
	for _, bad := range []string{"-a", "a-", "a.", ".a", "a/b", "a b", "a:", "a:99999999", "http://a", "a..b"} {
		p := DefaultServerParams()
		p.DocsHost = bad
		if _, err := RenderServer(p); err == nil {
			t.Errorf("DocsHost=%q 未被拒绝", bad)
		}
	}
}

// 合法输入：每个变量（含自定义安装根）原样出现在渲染结果中，并且不改变指令集合。
func TestCustomValuesRenderVerbatim(t *testing.T) {
	p := DefaultServerParams()
	p.User, p.Group = "ling-x", "ling-g"
	p.ExecPath, p.ConfigPath, p.MasterKeyPath = "/srv/ling/server/bin/lingedge-server", "/srv/ling/etc/server.yaml", "/srv/ling/etc/master.key"
	p.Roles, p.LimitNOFILE = []string{"worker", "api"}, 4096
	u, err := RenderServer(p)
	if err != nil {
		t.Fatal(err)
	}
	es := parseINI(t, string(u.Content))
	if one(t, es, "Service", "User") != "ling-x" || one(t, es, "Service", "Group") != "ling-g" || one(t, es, "Service", "LimitNOFILE") != "4096" ||
		one(t, es, "Service", "ExecStart") != "/srv/ling/server/bin/lingedge-server --config /srv/ling/etc/server.yaml --roles=worker,api" ||
		one(t, es, "Service", "LoadCredential") != "master.key:/srv/ling/etc/master.key" {
		t.Errorf("自定义值未原样渲染：%v", es)
	}
}

// 属性：对任意（含恶意）输入，只要 Render 成功，其指令集合（段、键、重复次数）必须与默认渲染完全相同，
// 且渲染结果能被严格的单元解析器解析（无多余行、无换行注入）。
func checkRenderedShape(t *testing.T, got, ref Unit) {
	t.Helper()
	if !utf8.Valid(got.Content) {
		t.Fatalf("渲染结果不是合法 UTF-8")
	}
	g, r := parseINI(t, string(got.Content)), parseINI(t, string(ref.Content))
	count := func(es []iniEntry) map[string]int {
		m := map[string]int{}
		for _, e := range es {
			m[e.Section+"/"+e.Key]++
		}
		return m
	}
	gc, rc := count(g), count(r)
	for k, n := range gc {
		if k == "Unit/Documentation" && n == 1 { // 唯一允许出现、而默认渲染里没有的可选指令
			continue
		}
		if rc[k] != n {
			t.Fatalf("渲染结果的指令 %s 出现 %d 次，参考渲染为 %d 次（注入？）\n%s", k, n, rc[k], got.Content)
		}
	}
	for k, n := range rc {
		if gc[k] != n && k != "Service/LoadCredential" && k != "Unit/Documentation" {
			t.Fatalf("渲染结果缺少指令 %s", k)
		}
	}
	if strings.Contains(string(got.Content), "%") {
		t.Fatalf("渲染结果含 %% 说明符")
	}
}

func FuzzRenderServer(f *testing.F) {
	for _, v := range injectionVectors {
		f.Add(v, v, v, v, v)
	}
	f.Add("lingedge", "/opt/lingedge/server/current/bin/lingedge-server", "/etc/lingedge/server/server.yaml", "api", "docs.example.com")
	ref := mustRender(f, UnitServer)
	f.Fuzz(func(t *testing.T, user, exec, cfg, role, docs string) {
		p := DefaultServerParams()
		p.User, p.Group, p.ExecPath, p.ConfigPath, p.MasterKeyPath, p.Roles, p.DocsHost = user, user, exec, cfg, cfg, []string{role}, docs
		u, err := RenderServer(p)
		if err != nil {
			return
		}
		checkRenderedShape(t, u, ref)
	})
}

func FuzzRenderNode(f *testing.F) {
	for _, v := range injectionVectors {
		f.Add(v, v, v, v)
	}
	f.Add("lingedge-node", "/opt/lingedge/node/current/lingedge-node", "/etc/lingedge/node/node.yaml", "/usr/local/bin/lingedge")
	refs := map[UnitName]Unit{}
	for _, u := range []UnitName{UnitNode, UnitOpenresty, UnitOpenrestyPath, UnitUpgradePath, UnitUpgradeService} {
		refs[u] = mustRender(f, u)
	}
	f.Fuzz(func(t *testing.T, user, agent, cfg, tools string) {
		p := DefaultNodeParams()
		p.User, p.Group, p.AgentExec, p.AgentConfig, p.OpenrestyBin, p.ToolsBin = user, user, agent, cfg, agent, tools
		us, err := NodeUnits(p)
		if err != nil {
			return
		}
		for _, u := range us {
			checkRenderedShape(t, u, refs[u.Name])
		}
	})
}

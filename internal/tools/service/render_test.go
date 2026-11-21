package service

import (
	"strings"
	"testing"
)

// TestGoldenByteForByte：渲染结果与人工对照 tools-design §15 / ADR-024 逐行写成的 golden 文件逐字节一致。
// golden 不是由 Render 生成的（没有 -update 开关）；改模板必须同时手工改 golden 并对照文档。
func TestGoldenByteForByte(t *testing.T) {
	docs := DefaultServerParams()
	docs.Credential, docs.DocsHost, docs.Roles = CredentialKeyFile, "docs.example.com", []string{"api"}
	cases := []struct {
		golden string
		render func() (Unit, error)
	}{
		{"lingedge-server.service", func() (Unit, error) { return RenderServer(DefaultServerParams()) }},
		{"lingedge-server.keyfile.service", func() (Unit, error) { return RenderServer(docs) }},
		{"lingedge-node.service", func() (Unit, error) { return RenderNode(UnitNode, DefaultNodeParams()) }},
		{"lingedge-openresty.service", func() (Unit, error) { return RenderNode(UnitOpenresty, DefaultNodeParams()) }},
		{"lingedge-openresty.path", func() (Unit, error) { return RenderNode(UnitOpenrestyPath, DefaultNodeParams()) }},
		{"lingedge-node-upgrade.path", func() (Unit, error) { return RenderNode(UnitUpgradePath, DefaultNodeParams()) }},
		{"lingedge-node-upgrade.service", func() (Unit, error) { return RenderNode(UnitUpgradeService, DefaultNodeParams()) }},
	}
	for _, c := range cases {
		t.Run(c.golden, func(t *testing.T) {
			u, err := c.render()
			if err != nil {
				t.Fatal(err)
			}
			if want := readFile(t, goldenPath(c.golden)); string(u.Content) != want {
				t.Fatalf("与 golden 不一致\n--- 渲染 ---\n%s\n--- golden ---\n%s", u.Content, want)
			}
		})
	}
}

// 属性测试：解析渲染结果，逐条断言加固项（tools-design §15、ADR-024、ADR-011 §9）。
func TestServerHardening(t *testing.T) {
	es := parseINI(t, string(mustRender(t, UnitServer).Content))
	for k, want := range map[string]string{
		"NoNewPrivileges": "yes", "ProtectSystem": "strict", "ProtectHome": "yes", "PrivateTmp": "yes", "PrivateDevices": "yes",
		"ProtectKernelTunables": "yes", "ProtectKernelModules": "yes", "ProtectControlGroups": "yes",
		"RestrictNamespaces": "yes", "RestrictRealtime": "yes", "LockPersonality": "yes",
		"MemoryDenyWriteExecute": "yes", "SystemCallArchitectures": "native",
		"LimitCORE": "0", // ADR-011 §9：禁止 core dump（L24）
	} {
		if got := one(t, es, "Service", k); got != want {
			t.Errorf("%s=%q，应为 %q", k, got, want)
		}
	}
	if got := one(t, es, "Service", "CapabilityBoundingSet"); got != "" {
		t.Errorf("CapabilityBoundingSet 应为空（零能力），得到 %q", got)
	}
	if got := one(t, es, "Service", "RestrictAddressFamilies"); got != "AF_INET AF_INET6 AF_UNIX" {
		t.Errorf("Server 的 RestrictAddressFamilies=%q", got)
	}
	if got := one(t, es, "Service", "ReadWritePaths"); got != "/var/lib/lingedge/server /var/log/lingedge" {
		t.Errorf("ReadWritePaths=%q", got)
	}
	if got := one(t, es, "Service", "LoadCredential"); got != "master.key:/etc/lingedge/server/master.key" {
		t.Errorf("LoadCredential=%q", got)
	}
	if one(t, es, "Service", "Type") != "notify" || one(t, es, "Service", "NotifyAccess") != "main" {
		t.Error("Server 必须是 Type=notify NotifyAccess=main（sd_notify READY=1）")
	}
	if one(t, es, "Service", "User") != "lingedge" || one(t, es, "Service", "Group") != "lingedge" {
		t.Error("Server 以 lingedge 运行")
	}
	if got := one(t, es, "Service", "ExecStart"); got != "/opt/lingedge/server/current/bin/lingedge-server --config /etc/lingedge/server/server.yaml --roles=api,grpc,scheduler,worker" {
		t.Errorf("ExecStart=%q", got)
	}
}

func TestServerKeyFileVariantHasNoLoadCredential(t *testing.T) {
	p := DefaultServerParams()
	p.Credential = CredentialKeyFile
	u, err := RenderServer(p)
	if err != nil {
		t.Fatal(err)
	}
	es := parseINI(t, string(u.Content))
	if has(es, "Service", "LoadCredential") || has(es, "Service", "EnvironmentFile") {
		t.Error("降级变体不得含 LoadCredential/EnvironmentFile（ADR-027）")
	}
	// 降级只改主密钥交付；其余加固项一条不少（与默认变体逐键比较）。
	def := parseINI(t, string(mustRender(t, UnitServer).Content))
	for _, e := range def {
		if e.Key == "LoadCredential" {
			continue
		}
		if got := get(es, e.Section, e.Key); len(got) != 1 || got[0] != e.Value {
			t.Errorf("降级变体的 [%s] %s 与默认变体不同：%q vs %q", e.Section, e.Key, got, e.Value)
		}
	}
	if one(t, es, "Service", "LimitCORE") != "0" {
		t.Error("降级变体同样必须 LimitCORE=0")
	}
}

func TestAgentHardening(t *testing.T) {
	es := parseINI(t, string(mustRender(t, UnitNode).Content))
	if has(es, "Service", "MemoryDenyWriteExecute") {
		t.Error("Agent 单元不得设 MemoryDenyWriteExecute（会被 openresty -t 子进程继承，IMP-20/ADR-024 §3）")
	}
	if got := one(t, es, "Service", "RestrictAddressFamilies"); got != "AF_INET AF_INET6 AF_UNIX AF_NETLINK" {
		t.Errorf("Agent 的 RestrictAddressFamilies 须含 AF_NETLINK（L24）：%q", got)
	}
	if got := one(t, es, "Service", "ReadWritePaths"); got != "/var/lib/lingedge-node /var/log/lingedge-node /run/lingedge-node" {
		t.Errorf("Agent ReadWritePaths=%q（须覆盖 cache/runtime/staging，BLK-04）", got)
	}
	if got := one(t, es, "Service", "CapabilityBoundingSet"); got != "" {
		t.Errorf("Agent 零能力，CapabilityBoundingSet 应为空：%q", got)
	}
	if has(es, "Service", "AmbientCapabilities") {
		t.Error("Agent 不得有 AmbientCapabilities")
	}
	for k, want := range map[string]string{"NoNewPrivileges": "yes", "ProtectSystem": "strict", "ProtectHome": "yes", "PrivateTmp": "yes",
		"PrivateDevices": "yes", "ProtectKernelTunables": "yes", "ProtectKernelModules": "yes", "ProtectControlGroups": "yes",
		"RestrictNamespaces": "yes", "LockPersonality": "yes", "Restart": "always", "RuntimeDirectory": "lingedge-node", "User": "lingedge-node"} {
		if got := one(t, es, "Service", k); got != want {
			t.Errorf("%s=%q，应为 %q", k, got, want)
		}
	}
}

func TestOpenrestyHardeningAndPrefixPair(t *testing.T) {
	es := parseINI(t, string(mustRender(t, UnitOpenresty).Content))
	if has(es, "Service", "MemoryDenyWriteExecute") {
		t.Error("OpenResty 单元不得设 MemoryDenyWriteExecute（LuaJIT 需要可写可执行内存）")
	}
	if got := one(t, es, "Service", "CapabilityBoundingSet"); got != "CAP_NET_BIND_SERVICE" {
		t.Errorf("CapabilityBoundingSet=%q", got)
	}
	if got := one(t, es, "Service", "AmbientCapabilities"); got != "CAP_NET_BIND_SERVICE" {
		t.Errorf("AmbientCapabilities=%q（ADR-024 §2）", got)
	}
	if got := one(t, es, "Service", "RestrictAddressFamilies"); got != "AF_INET AF_INET6 AF_UNIX" {
		t.Errorf("OpenResty 的 RestrictAddressFamilies=%q", got)
	}
	if one(t, es, "Service", "NoNewPrivileges") != "yes" || one(t, es, "Service", "ProtectSystem") != "strict" {
		t.Error("NoNewPrivileges/ProtectSystem=strict 必须保留")
	}
	if got := one(t, es, "Service", "ReadWritePaths"); got != "/var/lib/lingedge-node /var/log/lingedge-node /run/lingedge-node" {
		t.Errorf("ReadWritePaths=%q", got)
	}
	// BLK-04：ExecStartPre 与 ExecStart 使用同一对 -p/-c。
	const pair = "-p /var/lib/lingedge-node/current/ -c conf/nginx.conf"
	pre, start := one(t, es, "Service", "ExecStartPre"), one(t, es, "Service", "ExecStart")
	if pre != "/opt/lingedge/openresty/current/bin/openresty -t "+pair {
		t.Errorf("ExecStartPre=%q", pre)
	}
	if start != `/opt/lingedge/openresty/current/bin/openresty -g "daemon off;" `+pair {
		t.Errorf("ExecStart=%q", start)
	}
	if !strings.HasSuffix(pre, pair) || !strings.HasSuffix(start, pair) {
		t.Error("ExecStartPre 与 ExecStart 必须以同一对 -p/-c 结尾")
	}
	// StartLimitIntervalSec 必须在 [Unit]（置于 [Service] 会被 systemd 忽略，崩溃循环将触发限流并停止拉起）。
	if one(t, es, "Unit", "StartLimitIntervalSec") != "0" || has(es, "Service", "StartLimitIntervalSec") {
		t.Error("StartLimitIntervalSec=0 须位于 [Unit]")
	}
	if one(t, es, "Service", "Restart") != "always" {
		t.Error("OpenResty 须 Restart=always（FR-62）")
	}
}

func TestPathUnits(t *testing.T) {
	es := parseINI(t, string(mustRender(t, UnitOpenrestyPath).Content))
	if one(t, es, "Path", "PathExists") != "/var/lib/lingedge-node/current/conf/nginx.conf" || one(t, es, "Path", "Unit") != "lingedge-openresty.service" {
		t.Errorf("openresty.path 不符：%v", es)
	}
	es = parseINI(t, string(mustRender(t, UnitUpgradePath).Content))
	if one(t, es, "Path", "PathExists") != "/var/lib/lingedge-node/staging/upgrade.request" || one(t, es, "Path", "Unit") != "lingedge-node-upgrade.service" {
		t.Errorf("node-upgrade.path 不符：%v", es)
	}
	if one(t, es, "Path", "MakeDirectory") != "no" {
		t.Error("upgrade.path 须 MakeDirectory=no（不得由 systemd 以 root 创建 Agent 属主域内的目录）")
	}
}

func TestUpgradeServiceIsRootOneshotWithoutPathArgs(t *testing.T) {
	es := parseINI(t, string(mustRender(t, UnitUpgradeService).Content))
	if one(t, es, "Service", "Type") != "oneshot" || one(t, es, "Service", "User") != "root" {
		t.Error("升级单元为 root oneshot")
	}
	// ADR-024 §7.3：ExecStart 固定，不携带任何路径参数。
	if got := one(t, es, "Service", "ExecStart"); got != "/usr/local/bin/lingedge update node --apply-staged --non-interactive --json" {
		t.Errorf("ExecStart=%q", got)
	}
	for _, k := range []string{"NoNewPrivileges", "ProtectHome", "PrivateTmp", "ProtectKernelTunables", "ProtectKernelModules", "ProtectControlGroups", "RestrictNamespaces", "LockPersonality"} {
		if one(t, es, "Service", k) != "yes" {
			t.Errorf("升级单元缺少加固 %s=yes", k)
		}
	}
	if has(es, "Service", "ReadWritePaths") || has(es, "Service", "ProtectSystem") {
		t.Error("升级单元需要写 /opt 与 /var/lib/lingedge-node-upgrade，不得设 ProtectSystem=strict（§15.3 原文）")
	}
}

// 模板里出现的每个指令都必须登记 systemd 引入版本（或属于早于 209 的老指令），且 MinSystemd 恒等于表中最大值（不含降级前提）。
func TestDirectiveVersionTableCoversTemplates(t *testing.T) {
	old := map[string]bool{"Description": true, "Documentation": true, "After": true, "Wants": true, "Type": true, "NotifyAccess": true, "User": true, "Group": true,
		"ExecStart": true, "ExecStartPre": true, "ExecReload": true, "Restart": true, "RestartSec": true, "TimeoutStopSec": true, "TimeoutStartSec": true,
		"LimitNOFILE": true, "LimitCORE": true, "NoNewPrivileges": true, "PrivateTmp": true, "CapabilityBoundingSet": true,
		"WantedBy": true, "PathExists": true, "Unit": true, "MakeDirectory": true}
	maxSince := 0
	seen := map[string]bool{}
	for _, u := range AllUnits {
		for _, e := range parseINI(t, string(mustRender(t, u).Content)) {
			seen[e.Key] = true
			if since, ok := DirectiveSince[e.Key]; ok {
				if since > maxSince {
					maxSince = since
				}
				continue
			}
			if !old[e.Key] && e.Key != "LoadCredential" {
				t.Errorf("指令 %s（%s）未在 DirectiveSince 登记引入版本", e.Key, u)
			}
		}
	}
	if maxSince != MinSystemd {
		t.Errorf("MinSystemd=%d 与模板用到的指令中最晚引入的版本 %d 不符", MinSystemd, maxSince)
	}
	for k := range DirectiveSince {
		if !seen[k] {
			t.Errorf("DirectiveSince 登记了模板未使用的指令 %s", k)
		}
	}
}

// 所有渲染结果中不出现 systemd 说明符 `%`；任何值里也没有换行导致的额外指令。
func TestNoSpecifiersInRenderedUnits(t *testing.T) {
	for _, u := range AllUnits {
		c := string(mustRender(t, u).Content)
		if strings.Contains(c, "%") {
			t.Errorf("%s 含 %% 说明符", u)
		}
		if !strings.HasSuffix(c, "\n") || strings.Contains(c, "\r") {
			t.Errorf("%s 须以单个换行结尾且无 CR", u)
		}
	}
}

func TestEnableTargets(t *testing.T) {
	if got := EnableTargets("node"); len(got) != 3 || got[0] != UnitNode || got[1] != UnitOpenrestyPath || got[2] != UnitUpgradePath {
		t.Errorf("node 应 enable agent.service、openresty.path、upgrade.path：%v", got)
	}
	for _, u := range EnableTargets("node") {
		if u == UnitOpenresty {
			t.Error("不得直接 enable lingedge-openresty.service（首个配置代出现前无 current/，§15.2）")
		}
	}
	if got := EnableTargets("server"); len(got) != 1 || got[0] != UnitServer {
		t.Errorf("server：%v", got)
	}
	if EnableTargets("dns") != nil {
		t.Error("未知组件不应有 enable 目标")
	}
}

func TestNodeUnitsOrderAndNames(t *testing.T) {
	us, err := NodeUnits(DefaultNodeParams())
	if err != nil {
		t.Fatal(err)
	}
	want := []UnitName{UnitNode, UnitOpenresty, UnitOpenrestyPath, UnitUpgradePath, UnitUpgradeService}
	if len(us) != len(want) {
		t.Fatalf("得到 %d 个单元", len(us))
	}
	for i := range want {
		if us[i].Name != want[i] {
			t.Errorf("第 %d 个单元 %s，应为 %s", i, us[i].Name, want[i])
		}
	}
	if _, err := RenderNode(UnitServer, DefaultNodeParams()); err == nil {
		t.Error("RenderNode 不得渲染 Server 单元")
	}
}

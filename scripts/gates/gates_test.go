package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 本文件的违规样例以明文出现是有意的：scripts/gates 目录整体被扫描类门禁跳过（见 gateSelfDir）。

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// baseRoot 构造一个满足全部结构约束的最小仓库骨架。
func baseRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, "go.mod", "module "+modulePath+"\n\ngo 1.27.0\n")
	for _, m := range serverModules {
		write(t, root, "internal/"+m+"/doc.go", "package "+m+"\n")
	}
	write(t, root, "internal/tools/doc.go", "package tools\n")
	write(t, root, "internal/platform/ports/doc.go", "package ports\n")
	var cmds []string
	cmds = append(cmds, standaloneBinaries...)
	cmds = append(cmds, serverBinaries...)
	cmds = append(cmds, toolsBinary)
	for _, c := range cmds {
		write(t, root, "cmd/"+c+"/main.go", "package main\n\nfunc main() {}\n")
	}
	write(t, root, lintConfigName, renderLintConfig())
	return root
}

func mustRun(t *testing.T, g func(string, options) ([]Finding, error), root string, opts options) []Finding {
	t.Helper()
	fs, err := g(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func expectRule(t *testing.T, fs []Finding, rule, fileSub string) {
	t.Helper()
	for _, f := range fs {
		if f.Rule == rule && strings.Contains(f.File, fileSub) {
			return
		}
	}
	t.Errorf("期望出现规则 %q（文件含 %q），实际 %v", rule, fileSub, fs)
}

// ---- 依赖图规格 → depguard 规则（机械生成）----

func denied(rs []depRule, ruleName, pkgSuffix string) bool {
	for _, r := range rs {
		if r.name != ruleName {
			continue
		}
		for _, d := range r.deny {
			if d.pkg == modulePath+"/"+pkgSuffix {
				return true
			}
		}
	}
	return false
}

func TestGeneratedDepRulesMatchGraph(t *testing.T) {
	rs := buildDepRules()
	// 对每个 (模块 m, 目标 x)：当且仅当 x 为 platform 或图中的箭头时，根包 import 才不被拒绝。
	for _, m := range serverModules {
		for _, x := range append(append([]string{}, serverModules...), "tools") {
			if x == m {
				continue
			}
			allowed := x != "tools" && m != "platform" && (x == "platform" || contains(allowedDeps[m], x))
			if got := denied(rs, "module-"+m, "internal/"+x); got == allowed {
				t.Errorf("internal/%s → internal/%s：允许=%v，但 depguard 拒绝=%v", m, x, allowed, got)
			}
			if allowed && !denied(rs, "module-"+m, "internal/"+x+"/internal") {
				t.Errorf("internal/%s → internal/%s 允许根包，但未拒绝其 internal 子包", m, x)
			}
		}
	}
	// 关键用例（AC1/AC6）显式断言。
	for _, c := range []struct {
		from, to string
		want     bool
	}{
		{"node", "certificate", true}, {"node", "website", true}, {"node", "origin", true}, {"node", "cache", true}, {"node", "monitoring", true},
		{"origin", "cache", true}, {"cache", "origin", true}, {"cache", "node", true}, {"ruleengine", "origin", true}, {"ruleengine", "cache", true},
		{"cms", "billing", true}, {"monitoring", "website", true}, {"website", "auth", true},
		{"origin", "website", false}, {"website", "tenant", false}, {"reporter", "monitoring", false}, {"origin", "platform", false},
		{"platform", "website", true}, {"origin", "tools", true},
	} {
		if got := denied(rs, "module-"+c.from, "internal/"+c.to); got != c.want {
			t.Errorf("internal/%s → internal/%s：期望被拒绝=%v，实际 %v", c.from, c.to, c.want, got)
		}
	}
	for _, b := range standaloneBinaries {
		if !denied(rs, "cmd-"+b, "internal") {
			t.Errorf("cmd/%s 必须整体禁止 import internal", b)
		}
	}
	if !denied(rs, "pkg-no-internal", "internal") {
		t.Error("pkg 必须禁止 import internal")
	}
	for _, x := range serverModules {
		if !denied(rs, "tools-no-server", "internal/"+x) || !denied(rs, "cmd-lingedge", "internal/"+x) {
			t.Errorf("internal/tools 与 cmd/lingedge 必须禁止 import internal/%s", x)
		}
	}
	for _, b := range serverBinaries {
		if !denied(rs, "cmd-"+b, "internal/tools") {
			t.Errorf("cmd/%s 必须禁止 import internal/tools", b)
		}
	}
}

// 钉住的依赖图副本：直接抄自 architecture §5.3 图与 ADR-001 §4–§5。spec.go 被无意改动时此处失败；
// 有意改图须先修订 ADR，再同时改 spec.go 与本表。
var pinnedGraph = map[string][]string{
	"platform": nil, "auth": nil, "tenant": nil, "plugin": nil,
	"website": {"tenant"}, "origin": {"website"}, "cache": {"website"}, "ruleengine": {"website"}, "certificate": {"website"},
	"waf": {"website"}, "security": {"website"}, "l4proxy": {"tenant"}, "dns": {"tenant"}, "billing": {"tenant"},
	"cms": nil, "seo": nil, "media": nil, "monitoring": nil, "notification": nil,
	"node": nil, "cluster": nil, "scheduler": nil, "reporter": {"monitoring"},
}

func TestSpecMatchesPinnedGraph(t *testing.T) {
	if len(allowedDeps) != len(pinnedGraph) || len(serverModules) != len(pinnedGraph) {
		t.Fatalf("模块数不一致：spec %d/%d，钉住 %d", len(allowedDeps), len(serverModules), len(pinnedGraph))
	}
	for m, want := range pinnedGraph {
		got := allowedDeps[m]
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("模块 %s 的允许依赖 spec=%v，钉住=%v", m, got, want)
		}
		if !isServerModule(m) {
			t.Errorf("模块 %s 不在 serverModules", m)
		}
	}
}

func TestGenLintCheck(t *testing.T) {
	root := baseRoot(t)
	if fs := mustRun(t, runGenLint, root, options{check: true}); len(fs) != 0 {
		t.Fatalf("新生成的配置应一致: %v", fs)
	}
	cfg := filepath.Join(root, lintConfigName)
	b, _ := os.ReadFile(cfg)
	// 负向：手工删除一条禁止项（模拟「临时豁免」）必须被发现。
	tampered := strings.Replace(string(b), `- pkg: "`+modulePath+`/internal/cache"`, `- pkg: "`+modulePath+`/internal/cacheX"`, 1)
	if tampered == string(b) {
		t.Fatal("测试前提不成立：未找到可篡改的规则")
	}
	write(t, root, lintConfigName, tampered)
	expectRule(t, mustRun(t, runGenLint, root, options{check: true}), "gen-lint", lintConfigName)
	if err := os.Remove(cfg); err != nil {
		t.Fatal(err)
	}
	expectRule(t, mustRun(t, runGenLint, root, options{check: true}), "gen-lint", lintConfigName)
	if fs := mustRun(t, runGenLint, root, options{}); len(fs) != 0 { // 写入模式
		t.Fatal(fs)
	}
	if fs := mustRun(t, runGenLint, root, options{check: true}); len(fs) != 0 {
		t.Fatalf("重新生成后应一致: %v", fs)
	}
}

// ---- arch ----

func TestArch(t *testing.T) {
	if fs := mustRun(t, runArch, baseRoot(t), options{}); len(fs) != 0 {
		t.Fatalf("基线骨架应通过: %v", fs)
	}
	cases := []struct {
		name, rel, content, rule, fileSub string
		remove                            string // 非空则删除该相对路径
	}{
		{name: "根包含实现文件", rel: "internal/origin/service.go", content: "package origin\n", rule: "arch-module-root", fileSub: "internal/origin/service.go"},
		{name: "实现包不在 internal/ 下", rel: "internal/origin/service/s.go", content: "package service\n", rule: "arch-module-layout", fileSub: "internal/origin/service"},
		{name: "非法的内部包名", rel: "internal/origin/internal/util/u.go", content: "package util\n", rule: "arch-module-layout", fileSub: "internal/origin/internal/util"},
		{name: "未登记模块", rel: "internal/newmod/doc.go", content: "package newmod\n", rule: "arch-unregistered-module", fileSub: "internal/newmod"},
		{name: "未归类二进制", rel: "cmd/lingedge-extra/main.go", content: "package main\n", rule: "arch-unregistered-binary", fileSub: "cmd/lingedge-extra"},
		{name: "ports 含函数实现", rel: "internal/platform/ports/impl.go", content: "package ports\n\nfunc New() int { return 1 }\n", rule: "arch-ports", fileSub: "ports/impl.go"},
		{name: "ports 含方法实现", rel: "internal/platform/ports/impl.go", content: "package ports\n\ntype D struct{}\n\nfunc (D) Do() {}\n", rule: "arch-ports", fileSub: "ports/impl.go"},
		{name: "ports 含包级变量", rel: "internal/platform/ports/v.go", content: "package ports\n\nvar X = 1\n", rule: "arch-ports", fileSub: "ports/v.go"},
		{name: "ports 含清单外接口", rel: "internal/platform/ports/x.go", content: "package ports\n\ntype Sneaky interface{ Do() }\n", rule: "arch-ports", fileSub: "ports/x.go"},
		{name: "ports 含子包", rel: "internal/platform/ports/impl/i.go", content: "package impl\n", rule: "arch-ports", fileSub: "ports/impl"},
		{name: "api.go 含函数实现", rel: "internal/origin/api.go", content: "package origin\n\nfunc New() int { return 1 }\n", rule: "arch-module-root", fileSub: "internal/origin/api.go"},
		{name: "doc.go 含变量", rel: "internal/origin/doc.go", content: "package origin\n\nvar X = 1\n", rule: "arch-module-root", fileSub: "internal/origin/doc.go"},
		{name: "ports 含未导出的清单外接口", rel: "internal/platform/ports/x.go", content: "package ports\n\ntype sneaky interface{ Do() }\n", rule: "arch-ports", fileSub: "ports/x.go"},
		{name: "ports 含类型别名", rel: "internal/platform/ports/x.go", content: "package ports\n\nimport \"time\"\n\ntype T = time.Time\n", rule: "arch-ports", fileSub: "ports/x.go"},
		{name: "独立二进制直接依赖 internal", rel: "cmd/lingedge-node/dep.go", content: "package main\n\nimport _ \"" + modulePath + "/internal/origin\"\n", rule: "arch-standalone-deps", fileSub: "cmd/lingedge-node"},
		{name: "缺少 ports", remove: "internal/platform/ports/doc.go", rule: "arch-ports", fileSub: "internal/platform/ports"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := baseRoot(t)
			if c.remove != "" {
				if err := os.RemoveAll(filepath.Join(root, "internal", "platform", "ports")); err != nil {
					t.Fatal(err)
				}
			} else {
				write(t, root, c.rel, c.content)
			}
			expectRule(t, mustRun(t, runArch, root, options{}), c.rule, c.fileSub)
		})
	}
	// 正向：清单内接口、DTO、api.go、各实现包均通过。
	t.Run("合法布局", func(t *testing.T) {
		root := baseRoot(t)
		write(t, root, "internal/platform/ports/site.go", "package ports\n\ntype SiteInfo struct{ UID string }\n\ntype SiteDirectory interface{ Lookup(uid string) (SiteInfo, error) }\n\ntype Status string\n\nconst Active Status = \"active\"\n")
		write(t, root, "internal/origin/api.go", "package origin\n\ntype Service interface{}\n")
		for _, p := range []string{"handler", "service", "repository", "domain", "testdouble"} {
			write(t, root, "internal/origin/internal/"+p+"/x.go", "package "+p+"\n")
		}
		write(t, root, "internal/origin/internal/service/x_test.go", "package service\n")
		write(t, root, "internal/origin/origin_test.go", "package origin\n")
		write(t, root, "internal/origin/testdata/x.go", "package x\n")
		if fs := mustRun(t, runArch, root, options{}); len(fs) != 0 {
			t.Fatalf("合法布局不应失败: %v", fs)
		}
	})
}

// ---- testdouble（源码层）----

const tdTag = "//go:build " + testdoubleTag + "\n\n"

func TestTestdoubleSource(t *testing.T) {
	skip := options{skipBuild: true}
	t.Run("合法替身与测试", func(t *testing.T) {
		root := baseRoot(t)
		write(t, root, "internal/origin/internal/testdouble/fake.go", tdTag+"package testdouble\n\nconst Name = \"fake-node-1\"\n")
		write(t, root, "internal/origin/internal/service/s_test.go", "package service\n\nconst n = \"fake-node-2\"\n")
		write(t, root, "tests/fixtures/nodes.json", `{"name":"fake-node-3"}`)
		write(t, root, "docs/readme.md", "fake-node-4 仅用于说明\n")
		write(t, root, "frontend/admin/src/a.spec.ts", "const n = 'fake-node-5'\n")
		write(t, root, "edge/tests/golden/x.json", `{"n":"fake-node-6"}`)
		write(t, root, "internal/origin/internal/testdouble/and.go", "//go:build "+testdoubleTag+" && linux\n\npackage testdouble\n")
		if fs := mustRun(t, runTestdouble, root, skip); len(fs) != 0 {
			t.Fatalf("不应失败: %v", fs)
		}
	})
	neg := []struct{ name, rel, content, rule string }{
		{"替身包缺构建标签", "internal/origin/internal/testdouble/fake.go", "package testdouble\n", "testdouble-missing-tag"},
		{"否定构建标签不算带标签", "internal/origin/internal/testdouble/fake.go", "//go:build !" + testdoubleTag + "\n\npackage testdouble\n", "testdouble-missing-tag"},
		{"或约束不算带标签", "internal/origin/internal/testdouble/fake.go", "//go:build " + testdoubleTag + " || linux\n\npackage testdouble\n", "testdouble-missing-tag"},
		{"生产 embed testdata", "internal/origin/internal/service/s.go", "package service\n\nimport _ \"embed\"\n\n//go:embed testdata/x.json\nvar x string\n", "testdouble-fixture-embed"},
		{"替身包位置非法", "pkg/testdouble/fake.go", tdTag + "package testdouble\n", "testdouble-location"},
		{"带标签文件位置非法", "internal/origin/internal/service/s.go", tdTag + "package service\n", "testdouble-tag-location"},
		{"生产代码 import 替身", "internal/origin/internal/service/s.go", "package service\n\nimport _ \"" + modulePath + "/internal/origin/internal/testdouble\"\n", "testdouble-import"},
		{"生产代码含假数据标记", "internal/origin/internal/service/s.go", "package service\n\nconst n = \"Sample-Node-01\"\n", "testdouble-fake-data"},
		{"生产配置含假数据标记", "configs/server.yaml", "nodes:\n  - name: mock-node-1\n", "testdouble-fake-data"},
		{"迁移种子含假数据标记", "migrations/000001.sql", "INSERT INTO nodes VALUES ('demo-node-1');\n", "testdouble-fake-data"},
		{"生产代码 embed fixtures", "internal/origin/internal/service/s.go", "package service\n\nimport _ \"embed\"\n\n//go:embed ../../../../tests/fixtures/x.json\nvar x string\n", "testdouble-fixture-embed"},
		{"生产代码检测测试环境", "internal/origin/internal/service/s.go", "package service\n\nimport \"testing\"\n\nvar _ = testing.Testing()\n", "testdouble-test-branch"},
	}
	for _, c := range neg {
		t.Run(c.name, func(t *testing.T) {
			root := baseRoot(t)
			write(t, root, c.rel, c.content)
			expectRule(t, mustRun(t, runTestdouble, root, skip), c.rule, c.rel)
		})
	}
}

// ---- passwords ----

func TestPasswordsReportsRealLine(t *testing.T) {
	root := t.TempDir()
	write(t, root, "configs/server.yaml", "a: 1\npassword: \"hunter2\"\nb: 2\n")
	fs := mustRun(t, runPasswords, root, options{})
	if len(fs) != 1 || fs[0].Line != 2 {
		t.Fatalf("期望仅在第 2 行报告一次，实际 %v", fs)
	}
}

func TestPasswords(t *testing.T) {
	neg := []struct{ name, rel, content string }{
		{"Go 赋值", "internal/auth/seed.go", "package auth\n\nvar cfg = struct{ Password string }{Password: \"hunter2\"}\n"},
		{"Go := 赋值", "internal/auth/seed.go", "package auth\n\nfunc f() { adminPassword := \"Welcome1\"; _ = adminPassword }\n"},
		{"yaml 模板", "configs/server.yaml", "admin:\n  password: \"S3cret!\"\n"},
		{"yaml secret", "configs/server.yaml", "jwt_secret: 'abcdef'\n"},
		{"迁移种子弱口令", "migrations/000001_seed.sql", "-- 初始管理员 password 为 admin123\n"},
		{"SQL IDENTIFIED BY", "migrations/000002.sql", "CREATE USER 'app'@'%' IDENTIFIED BY 'lingedge';\n"},
		{"redis requirepass", "configs/redis.conf", "requirepass foobared\n"},
		{"命令行 mysql -p", "deploy/install/init.sh", "mysql -uroot -pRootPass1 -e 'select 1'\n"},
		{"命令行 --password", "deploy/install/init.sh", "tool --password=hunter2\n"},
		{"DSN 内嵌口令", "internal/platform/db.go", "package platform\n\nconst dsn = \"app:hunter2@tcp(127.0.0.1:3306)/lingedge\"\n"},
		{"URL 内嵌口令", "configs/server.yaml", "redis: redis://default:hunter2@127.0.0.1:6379/0\n"},
		{"默认账号对（文档）", "docs/ops/install.md", "安装后使用 admin/admin 登录。\n"},
		{"弱口令（文档）", "docs/ops/install.md", "默认密码：123456\n"},
		{"无引号 yaml", "configs/server.yaml", "db:\n  password: hunter2\n"},
		{"env 文件", "deploy/install/app.env", "DB_PASSWORD=hunter2\n"},
		{"shell export", "deploy/install/init.sh", "export REDIS_PASSWORD=hunter2\n"},
		{"systemd Environment", "deploy/systemd/x.service", "Environment=APP_SECRET=hunter2\n"},
		{"env 展开默认值", "configs/server.yaml", "password: \"${DB_PASSWORD:-Hunter2}\"\n"},
		{"空用户 redis URL", "configs/server.yaml", "redis: redis://:hunter2@127.0.0.1:6379/0\n"},
		{"redis-cli -a", "deploy/install/init.sh", "redis-cli -h 127.0.0.1 -a hunter2 ping\n"},
		{"fixtures 缺少标注", "tests/fixtures/users.yaml", "password: \"hunter2\"\n"},
		{"标注不在 fixtures 目录", "internal/auth/seed.go", "// 仅测试\npackage auth\n\nvar p = \"password: \\\"hunter2\\\"\"\n\nvar _ = map[string]string{\"password\": \"hunter2\"}\n"},
	}
	for _, c := range neg {
		t.Run("负向/"+c.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, c.rel, c.content)
			expectRule(t, mustRun(t, runPasswords, root, options{}), "password", c.rel)
		})
	}
	pos := []struct{ name, rel, content string }{
		{"环境变量", "internal/auth/cfg.go", "package auth\n\nvar password = os.Getenv(\"DB_PASSWORD\")\n"},
		{"空值与占位符", "configs/server.example.yaml", "password: \"\"\nsecret: \"${LINGEDGE_SECRET}\"\ndb_password: \"<填写>\"\ntoken: \"abc\"\n"},
		{"掩码", "internal/auth/log.go", "package auth\n\nconst masked = `password: \"****\"`\n"},
		{"文档中的叙述", "docs/ops/install.md", "初始密码由安装者设定，不存在默认口令。\n"},
		{"chown 属主", "docs/ops/files.md", "master.key 为 root:root 0400\n"},
		{"带标注的 fixtures", "tests/fixtures/users.yaml", "# 仅测试：不得用于任何运行环境\npassword: \"hunter2\"\n"},
		{"IDENTIFIED BY 占位符", "migrations/000002.sql", "CREATE USER 'app'@'%' IDENTIFIED BY '${APP_DB_PASSWORD}';\n"},
		{"mysql 端口参数", "deploy/install/init.sh", "mysql -h 127.0.0.1 -P3306 -u app -p\n"},
		{"admin 路由", "internal/auth/routes.go", "package auth\n\nconst route = \"/admin/password-reset\"\n"},
		{"requirepass 空串", "configs/redis.conf", "requirepass \"\"\n"},
		{"env 引用", "deploy/install/app.env", "DB_PASSWORD=${DB_PASSWORD}\nREDIS_PASSWORD=\n"},
		{"mysql 交互口令", "deploy/install/init.sh", "mysql -uroot -p\n"},
	}
	for _, c := range pos {
		t.Run("正向/"+c.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, c.rel, c.content)
			if fs := mustRun(t, runPasswords, root, options{}); len(fs) != 0 {
				t.Fatalf("不应失败: %v", fs)
			}
		})
	}
}

// ---- brand ----

func TestBrand(t *testing.T) {
	name := "Smart" + "CDN"
	neg := []struct{ name, rel, content string }{
		{"代码", "internal/origin/doc.go", "// 参考 " + name + " 的实现\npackage origin\n"},
		{"配置", "configs/server.yaml", "brand: " + name + "\n"},
		{"README", "README.md", name + " 兼容\n"},
		{"文档中的非隔离引用", "docs/arch/x.md", "本系统沿用 " + name + " 的节点协议。\n"},
		{"其他产品名", "internal/origin/doc.go", "// Chat" + "Sphere 同款\npackage origin\n"},
		{"docs 中仅含泛化措辞", "docs/arch/x.md", "本次改动较大。\n沿用 " + name + " 的节点协议。\n不影响其他项目。\n"},
		{"路径", "internal/" + strings.ToLower(name) + "/x.txt", "x\n"},
	}
	for _, c := range neg {
		t.Run("负向/"+c.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, c.rel, c.content)
			expectRule(t, mustRun(t, runBrand, root, options{}), "brand", "")
		})
	}
	t.Run("正向/文档隔离说明", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "docs/bmad/epics.md", "均不触碰已在运行的 MySQL 实例与 "+name+" 数据\n")
		write(t, root, "docs/bmad/s.yaml", "禁止触碰既有实例与\n  "+name+" 数据\n")
		write(t, root, "docs/bmad/review.md", "出现外部项目名 "+name+"，与 NFR-29 冲突\n")
		write(t, root, "README.md", "LingEdge（凌界云）\n")
		if fs := mustRun(t, runBrand, root, options{}); len(fs) != 0 {
			t.Fatalf("不应失败: %v", fs)
		}
	})
	t.Run("隔离措辞不能豁免 docs 之外", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "configs/x.yaml", "# 不触碰 "+name+" 数据\n")
		expectRule(t, mustRun(t, runBrand, root, options{}), "brand", "configs/x.yaml")
	})
}

// ---- shell ----

func TestShell(t *testing.T) {
	neg := []struct{ name, rel, content, rule string }{
		{"Go 字面量 bash -c", "internal/node/x.go", "package node\n\nconst c = \"bash -c 'rm -rf /tmp/x'\"\n", "shell-c"},
		{"exec sh 参数数组", "internal/tools/security/run.go", "package security\n\nimport \"os/exec\"\n\nfunc f(s string) { _ = exec.Command(\"sh\", \"-c\", s) }\n", "shell-exec"},
		{"exec /bin/bash", "internal/tools/security/run.go", "package security\n\nimport \"os/exec\"\n\nfunc f(s string) { _ = exec.Command(\"/bin/bash\", s) }\n", "shell-exec"},
		{"exec 命令名拼接", "internal/tools/security/run.go", "package security\n\nimport \"os/exec\"\n\nfunc f(s string) { _ = exec.Command(\"systemctl \" + s) }\n", "shell-exec"},
		{"exec Sprintf", "internal/tools/security/run.go", "package security\n\nimport (\n\t\"fmt\"\n\t\"os/exec\"\n)\n\nfunc f(s string) { _ = exec.Command(fmt.Sprintf(\"systemctl %s\", s)) }\n", "shell-exec"},
		{"exec 命令名含空白", "internal/tools/security/run.go", "package security\n\nimport \"os/exec\"\n\nfunc f() { _ = exec.Command(\"ls -l /tmp\") }\n", "shell-exec"},
		{"别名 + Context", "internal/tools/security/run.go", "package security\n\nimport (\n\t\"context\"\n\tx \"os/exec\"\n)\n\nfunc f(ctx context.Context, s string) { _ = x.CommandContext(ctx, \"sh\", \"-c\", s) }\n", "shell-exec"},
		{"命令名变量为 shell", "internal/tools/security/run.go", "package security\n\nimport \"os/exec\"\n\nfunc f(s string) {\n\tsh := \"/bin/sh\"\n\t_ = exec.Command(sh, \"-c\", s)\n}\n", "shell-exec"},
		{"dot import", "internal/tools/security/run.go", "package security\n\nimport . \"os/exec\"\n\nfunc f(s string) { _ = Command(\"bash\", s) }\n", "shell-exec"},
		{"解释器与 -c 间夹选项", "internal/node/x.go", "package node\n\nvar a = []string{\"bash\", \"--noprofile\", \"-c\", \"x\"}\n", "shell-c"},
		{"无法解析的 Go 文件", "internal/node/x.go", "package node\n\nfunc (\n", "shell-unparseable"},
		{"bin 目录下的源码不被跳过", "deploy/install/bin/run.sh", "bash -c 'x'\n", "shell-c"},
		{"systemd unit", "deploy/systemd/lingedge.service", "[Service]\nExecStart=/bin/sh -c 'lingedge-server run'\n", "shell-c"},
		{"cmd /c", "internal/node/x.go", "package node\n\nconst c = \"cmd.exe /c dir\"\n", "shell-c"},
	}
	for _, c := range neg {
		t.Run("负向/"+c.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, c.rel, c.content)
			expectRule(t, mustRun(t, runShell, root, options{}), c.rule, c.rel)
		})
	}
	pos := []struct{ name, rel, content string }{
		{"参数数组", "internal/tools/security/run.go", "package security\n\nimport (\n\t\"context\"\n\t\"os/exec\"\n\t\"path/filepath\"\n)\n\nfunc f(ctx context.Context, dir, arg string) {\n\t_ = exec.Command(\"systemctl\", \"restart\", arg)\n\t_ = exec.CommandContext(ctx, filepath.Join(dir, \"bin\", \"tool\"), \"--flag=\"+arg)\n}\n"},
		{"测试代码不受限", "internal/node/x_test.go", "package node\n\nimport \"os/exec\"\n\nfunc f() { _ = exec.Command(\"sh\", \"-c\", \"true\") }\n"},
		{"文档说明", "docs/security/x.md", "禁止 bash -c 拼接\n"},
		{"无关的 exec 包", "internal/node/y.go", "package node\n\nimport \"example.com/exec\"\n\nfunc f() { _ = exec.Command(\"a b\") }\n"},
	}
	for _, c := range pos {
		t.Run("正向/"+c.name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, c.rel, c.content)
			if fs := mustRun(t, runShell, root, options{}); len(fs) != 0 {
				t.Fatalf("不应失败: %v", fs)
			}
		})
	}
}

func TestRunDispatch(t *testing.T) {
	root := baseRoot(t)
	if c := run([]string{"arch", "-root", root}); c != 0 {
		t.Errorf("arch 退出码 %d", c)
	}
	write(t, root, "internal/origin/impl.go", "package origin\n")
	if c := run([]string{"arch", "-root", root}); c != 1 {
		t.Errorf("违规时 arch 退出码应为 1，实际 %d", c)
	}
	if c := run([]string{"nope"}); c != 2 {
		t.Errorf("未知子命令退出码应为 2，实际 %d", c)
	}
	if c := run(nil); c != 2 {
		t.Errorf("无参数退出码应为 2，实际 %d", c)
	}
}

// ---- Repository Scope 门禁的元数据（Story 1.5 AC5）----

// 平台级表清单里的每张表都必须在 docs/database 里有对应的表定义标题（防拼写错误与悬空条目），
// 且文档里该表若带 tenant_id 列则不得列入（租户表不能豁免 Scope 首参）。
func TestPlatformTablesMatchDocs(t *testing.T) {
	docs, err := filepath.Glob("../../docs/database/*.md")
	if err != nil || len(docs) == 0 {
		t.Skip("未找到 docs/database（在仓库副本中运行时跳过）")
	}
	var text strings.Builder
	for _, d := range docs {
		b, err := os.ReadFile(d)
		if err != nil {
			t.Fatal(err)
		}
		text.Write(b)
		text.WriteString("\n")
	}
	all := text.String()
	for tbl := range platformTables() {
		heading := regexp.MustCompile("(?m)^#{3,4} [0-9.]+ `" + regexp.QuoteMeta(tbl) + "`")
		loc := heading.FindStringIndex(all)
		if loc == nil {
			t.Errorf("平台级表清单含 %q，但 docs/database 中没有该表的定义标题", tbl)
			continue
		}
		section := all[loc[1]:]
		if next := regexp.MustCompile(`(?m)^#{2,4} `).FindStringIndex(section); next != nil {
			section = section[:next[0]]
		}
		if regexp.MustCompile(`(?m)^\|\s*tenant_id\s*\|`).MatchString(section) {
			t.Errorf("表 %q 在文档中有 tenant_id 列，不得列为平台级表", tbl)
		}
	}
}

func TestRepositoryPathRegex(t *testing.T) {
	yes := []string{
		"internal/website/internal/repository/site.go",
		"internal/platform/task/repository/task.go",
		"internal/auth/internal/repository/sub/x.go",
	}
	no := []string{
		"internal/website/internal/service/site.go",
		"internal/website/internal/repositoryx/site.go",
		"internal/platform/db/pool.go",
		"cmd/lingedge-server/repository/x.go",
		"internal/website/repository.go",
	}
	for _, p := range yes {
		if !inRepositoryPackage(p) {
			t.Errorf("%s 应判定为 repository 包", p)
		}
	}
	for _, p := range no {
		if inRepositoryPackage(p) {
			t.Errorf("%s 不应判定为 repository 包", p)
		}
	}
}

// ---- tasktype（G-12：任务类型白名单，Story 1.14）----

const ttOpenAPI = "openapi: 3.1.0\ncomponents:\n  schemas:\n    Other:\n      type: string\n    TaskType:\n      type: string\n      enum:\n      - cache.purge_url\n      - retention.cleanup\n      - system.backup\n      description: x\n    Next:\n      type: string\n"

func ttRoot(t *testing.T, typesGo string, extra map[string]string) string {
	t.Helper()
	root := baseRoot(t)
	write(t, root, "docs/api/openapi.yaml", ttOpenAPI)
	write(t, root, "internal/platform/taskrt/types.go", typesGo)
	for rel, c := range extra {
		write(t, root, rel, c)
	}
	return root
}

const ttGoodTypes = "package taskrt\n\ntype Type string\n\nconst (\n\tA Type = \"cache.purge_url\"\n\tB Type = \"retention.cleanup\"\n)\n"

func TestOpenapiTaskTypesParser(t *testing.T) {
	got := openapiTaskTypes([]byte(ttOpenAPI))
	if strings.Join(got, ",") != "cache.purge_url,retention.cleanup,system.backup" {
		t.Fatalf("解析结果: %v", got)
	}
	if len(openapiTaskTypes([]byte("components:\n  schemas:\n    X:\n      type: string\n"))) != 0 {
		t.Fatal("没有 TaskType 时应返回空")
	}
}

func TestTasktypeGate(t *testing.T) {
	neg := []struct {
		name, types string
		extra       map[string]string
		rule, file  string
		noSpec      bool
	}{
		{"常量不在 OpenAPI 枚举内", ttGoodTypes + "\nconst C Type = \"cert.issue\"\n", nil, "tasktype-unknown", "internal/platform/taskrt/types.go", false},
		{"命令类常量（即使有人把它加进了枚举也不行）", "package taskrt\n\ntype Type string\n\nconst X Type = \"shell.exec\"\n", nil, "tasktype-command", "internal/platform/taskrt/types.go", false},
		{"带空白的 sh -c 形态常量", "package taskrt\n\ntype Type string\n\nconst X Type = \"sh -c\"\n", nil, "tasktype-command", "internal/platform/taskrt/types.go", false},
		{"run 片段", "package taskrt\n\ntype Type string\n\nconst X Type = \"node.run_command\"\n", nil, "tasktype-command", "internal/platform/taskrt/types.go", false},
		{"登记字面量不在枚举内", ttGoodTypes, map[string]string{"cmd/lingedge-server/tasks.go": "package main\n\nimport \"example.com/x/taskrt\"\n\nvar _ = taskrt.Spec{Type: \"dns.sync\"}\n"}, "tasktype-unknown", "cmd/lingedge-server/tasks.go", false},
		{"入队字面量为命令类", ttGoodTypes, map[string]string{"internal/cache/x.go": "package cache\n\nimport \"example.com/x/taskrt\"\n\nvar _ = taskrt.EnqueueRequest{Type: taskrt.Type(\"exec.command\")}\n"}, "tasktype-command", "internal/cache/x.go", false},
		{"包内裸 Spec 字面量", ttGoodTypes, map[string]string{"internal/platform/taskrt/reg.go": "package taskrt\n\nvar _ = Spec{Type: \"bogus.type\"}\n"}, "tasktype-unknown", "internal/platform/taskrt/reg.go", false},
		{"缺少 OpenAPI 事实源不得放行", ttGoodTypes, nil, "tasktype-spec", "docs/api/openapi.yaml", true},
		{"无法解析的 Go 文件", ttGoodTypes, map[string]string{"internal/cache/bad.go": "package cache\n\nfunc (\n"}, "tasktype-unparseable", "internal/cache/bad.go", false},
	}
	for _, c := range neg {
		t.Run("负向/"+c.name, func(t *testing.T) {
			root := ttRoot(t, c.types, c.extra)
			if c.noSpec {
				write(t, root, "docs/api/openapi.yaml", "openapi: 3.1.0\n")
			}
			expectRule(t, mustRun(t, runTasktype, root, options{}), c.rule, c.file)
		})
	}
	pos := []struct {
		name, types string
		extra       map[string]string
	}{
		{"合法常量与登记", ttGoodTypes, map[string]string{"cmd/lingedge-server/tasks.go": "package main\n\nimport \"example.com/x/taskrt\"\n\nvar _ = taskrt.Spec{Type: \"system.backup\"}\n"}},
		{"测试代码不受限", ttGoodTypes, map[string]string{"internal/cache/x_test.go": "package cache\n\nimport \"example.com/x/taskrt\"\n\nvar _ = taskrt.Spec{Type: \"shell.exec\"}\n"}},
		{"无关包的 Spec 字面量", ttGoodTypes, map[string]string{"internal/cache/y.go": "package cache\n\nimport \"example.com/other\"\n\nvar _ = other.Spec{Type: \"anything goes\"}\n"}},
	}
	for _, c := range pos {
		t.Run("正向/"+c.name, func(t *testing.T) {
			if fs := mustRun(t, runTasktype, ttRoot(t, c.types, c.extra), options{}); len(fs) != 0 {
				t.Fatalf("不应失败: %v", fs)
			}
		})
	}
	// 没有任务运行时的仓库：门禁无事可检（不要求 openapi）。
	if fs := mustRun(t, runTasktype, baseRoot(t), options{}); len(fs) != 0 {
		t.Fatalf("无 taskrt 时不应失败: %v", fs)
	}
}

// 运行时与门禁各有一份「命令语义片段」表：两者必须一致，否则任一处放宽都会让另一处形同虚设。
func TestTasktypeSegmentsMatchRuntime(t *testing.T) {
	b, err := os.ReadFile("../../internal/platform/taskrt/types.go")
	if err != nil {
		t.Skip("无运行时源码: " + err.Error())
	}
	m := regexp.MustCompile(`(?s)var forbiddenSegments = map\[string\]bool\{(.*?)\}`).FindSubmatch(b)
	if m == nil {
		t.Fatal("找不到运行时的 forbiddenSegments")
	}
	rt := map[string]bool{}
	for _, s := range regexp.MustCompile(`"([a-z_]+)":`).FindAllSubmatch(m[1], -1) {
		rt[string(s[1])] = true
	}
	if len(rt) != len(taskTypeForbiddenSegments) {
		t.Fatalf("片段表不一致：运行时 %v 门禁 %v", rt, taskTypeForbiddenSegments)
	}
	for k := range rt {
		if !taskTypeForbiddenSegments[k] {
			t.Errorf("门禁缺少片段 %s", k)
		}
	}
}

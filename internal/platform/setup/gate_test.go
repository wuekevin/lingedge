package setup

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("未找到 go.mod")
		}
		dir = parent
	}
}

// ADR-026 §2 / AD-26：Server 进程里没有任何代码路径可以在 setup 窗口之外构造 RoleMigrate 池。
// 静态扫描全仓库的非测试 Go 代码：db.OpenMigrate 只允许由 migrate 包（CLI 与 Window）调用；migrate.Window.OpenRunner 只允许由 setup 服务调用。
// 这是对 forbidigo [db-pool-migrate] 白名单（internal/platform/{db,migrate}、cmd/lingedge-server）的更窄一层收紧：cmd/lingedge-server 不得直接调用。
func TestMigratePoolOnlyConstructedInsideSetupWindowOrMigrateCLI(t *testing.T) {
	root := repoRoot(t)
	allowOpenMigrate := map[string]bool{
		"internal/platform/db/pool.go":            true, // 定义
		"internal/platform/migrate/window.go":     true, // setup 窗口
		"internal/platform/migrate/credsource.go": true, // credential init/reset 的账号预检（root 侧 CLI）
		"internal/platform/migrate/cli.go":        true, // migrate 子命令
	}
	allowOpenRunner := map[string]bool{
		"internal/platform/migrate/window.go":   true, // 定义
		"internal/platform/setup/steps_db.go":   true,
		"internal/platform/setup/steps_post.go": true,
	}
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "frontend", "docs", "bin", "dist", "_bmad", "_bmad-output", "tests", "scripts": // tests/ 与 scripts/ 是测试基础设施与开发脚本，不进入发行产物（forbidigo 同样豁免）
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "OpenMigrate":
				if !allowOpenMigrate[filepath.ToSlash(rel)] {
					t.Errorf("%s 调用了 OpenMigrate：migrate 池只能由 migrate 包构造（ADR-026 §2）", rel)
				}
				found[rel] = true
			case "OpenRunner":
				if !allowOpenRunner[filepath.ToSlash(rel)] {
					t.Errorf("%s 调用了 Window.OpenRunner：只有 setup 服务可以在 setup 窗口内构造 migrate runner（ADR-026 §2）", rel)
				}
				found[rel] = true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatal("扫描没有发现任何调用点：门禁空转")
	}
	var names []string
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names)
	t.Logf("OpenMigrate/OpenRunner 调用点：%v", names)
	// setup 窗口只在 setup mode 创建：已安装（正常模式）的 Service 没有窗口，Window() 为 nil。
	svc, err := New(Options{Installed: true})
	if err != nil {
		t.Fatal(err)
	}
	if svc.Window() != nil {
		t.Error("正常模式的 Service 不得持有 migrate 窗口")
	}
}

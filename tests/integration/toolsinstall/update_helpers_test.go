package toolsinstall

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

// Story 1.43 联调的基础设施：
//   - 「新版本的 server」是**真实的 lingedge-server 二进制**，用 `go build -overlay` 往内嵌迁移集合里追加测试用的真实 SQL 迁移（reversible / expand / irreversible / 中途失败），
//     不改任何冻结的迁移文件与源码；
//   - 伪 systemd（sysd）在 systemctl start/stop 时真实地拉起/终止 lingedge-server 进程（按已安装单元文件的 ExecStart 与 current 链接），
//     使「停服务 → 迁移 → 切换 → 启动 → 健康检查 → 回滚」全部作用在真实进程与真实 MySQL 上。

func repoRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		if d == filepath.Dir(d) {
			t.Fatal("找不到 go.mod")
		}
	}
}

// 测试迁移（序号接在冻结的 000018 之后；仅存在于 overlay 构建的测试二进制里）。
const (
	migHeaderRev = "-- lingedge:class=reversible\n-- lingedge:summary=Story 1.43 测试迁移（reversible）\n"
	migRev19     = migHeaderRev + "-- +goose Up\nALTER TABLE users ADD COLUMN t43_rev INT NULL;\n-- +goose Down\nALTER TABLE users DROP COLUMN t43_rev;\n"
	migExp19     = "-- lingedge:class=expand\n-- lingedge:summary=Story 1.43 测试迁移（expand）\n-- +goose Up\nALTER TABLE users ADD COLUMN t43_exp INT NOT NULL DEFAULT 0;\n"
	migIrr19     = "-- lingedge:class=irreversible\n-- lingedge:summary=Story 1.43 测试迁移（irreversible）\n-- lingedge:reason=测试：新增后立即删除列，不可由 down 复原\n-- +goose Up\n-- lingedge:guard=测试迁移：一次性临时库，不重跑\nALTER TABLE users ADD COLUMN t43_irr INT NULL;\n-- lingedge:guard=测试迁移：一次性临时库，不重跑\nALTER TABLE users DROP COLUMN t43_irr;\n"
	// 第二条迁移的第二条语句重复添加已存在的列 → 迁移中途失败，库留在 dirty 状态（MySQL DDL 不是事务性的）。
	migBad20 = migHeaderRev + "-- +goose Up\n-- lingedge:guard=测试迁移：一次性临时库，不重跑\nALTER TABLE users ADD COLUMN t43_bad INT NULL;\n-- lingedge:guard=测试迁移：故意重复列，使迁移中途失败\nALTER TABLE users ADD COLUMN permission_version BIGINT NULL;\n-- +goose Down\n-- lingedge:guard=测试迁移：一次性临时库，不重跑\nALTER TABLE users DROP COLUMN t43_bad;\n"
)

var (
	ovMu    sync.Mutex
	ovCache = map[string]string{}
)

// buildServerWith 构建带额外迁移的真实 lingedge-server（同一组迁移只构建一次）。files：文件名 → 内容。
func buildServerWith(t testing.TB, name string, files map[string]string) string {
	t.Helper()
	ovMu.Lock()
	defer ovMu.Unlock()
	if p, ok := ovCache[name]; ok {
		return p
	}
	root := repoRoot(t)
	dir, err := os.MkdirTemp("", "letoolsov")
	if err != nil {
		t.Fatal(err)
	}
	repl := map[string]string{}
	for fn, body := range files {
		src := filepath.Join(dir, fn)
		if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		repl[filepath.Join(root, "migrations", fn)] = src
	}
	ovJSON := "{\"Replace\":{"
	i := 0
	for k, v := range repl {
		if i > 0 {
			ovJSON += ","
		}
		ovJSON += strconv.Quote(k) + ":" + strconv.Quote(v)
		i++
	}
	ovJSON += "}}"
	ov := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(ov, []byte(ovJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "lingedge-server")
	cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-overlay", ov, "-o", out, "github.com/wuekevin/lingedge/cmd/lingedge-server")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建带测试迁移的 lingedge-server 失败: %v\n%s", err, b)
	}
	// 该二进制自带的迁移 lint 必须通过（测试迁移是合规的真实迁移，不是绕过 lint 的东西）。
	if b, err := exec.Command(out, "migrate", "lint").CombinedOutput(); err != nil {
		t.Fatalf("测试迁移未通过 lint: %v\n%s", err, b)
	}
	ovCache[name] = out
	ovDirs = append(ovDirs, dir)
	return out
}

var ovDirs []string

// cleanupBuilt 删除 overlay 构建目录与基础二进制目录（TestMain 结束时调用）。
func cleanupBuilt() {
	ovMu.Lock()
	defer ovMu.Unlock()
	for _, d := range ovDirs {
		_ = os.RemoveAll(d)
	}
	if binPath != "" {
		_ = os.RemoveAll(filepath.Dir(binPath))
	}
}

func serverWithRev(t testing.TB) string {
	return buildServerWith(t, "rev", map[string]string{"000019_t43_rev.sql": migRev19})
}
func serverWithExp(t testing.TB) string {
	return buildServerWith(t, "exp", map[string]string{"000019_t43_exp.sql": migExp19})
}
func serverWithIrr(t testing.TB) string {
	return buildServerWith(t, "irr", map[string]string{"000019_t43_irr.sql": migIrr19})
}
func serverWithBad(t testing.TB) string {
	return buildServerWith(t, "bad", map[string]string{"000019_t43_rev.sql": migRev19, "000020_t43_bad.sql": migBad20})
}

// ---- 伪 systemd：start/stop 时真实地拉起/终止进程 ----

type sysd struct {
	inner service.Runner
	spec  *liveServer
}

func (r *sysd) Run(ctx context.Context, c safeexec.Cmd) (safeexec.Result, error) {
	res, err := r.inner.Run(ctx, c)
	if err != nil || filepath.Base(c.Path) != "systemctl" || len(c.Args) < 2 {
		return res, err
	}
	mine := false
	for _, a := range c.Args[1:] {
		if a == "lingedge-server.service" {
			mine = true
		}
	}
	if !mine {
		return res, err
	}
	switch c.Args[0] {
	case "stop":
		r.spec.terminate()
	case "start":
		if lerr := r.spec.launch(); lerr != nil {
			return res, lerr
		}
	case "restart":
		r.spec.terminate()
		if lerr := r.spec.launch(); lerr != nil {
			return res, lerr
		}
	}
	return res, nil
}

// terminate 发送 SIGTERM 并等待进程退出（优雅停机）；没有运行中的进程则什么都不做。
func (s *liveServer) terminate() {
	s.mu.Lock()
	cmd, done := s.cmd, s.done
	s.mu.Unlock()
	if cmd == nil {
		return
	}
	if ex, _ := s.exited(); ex {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-done:
		done <- err
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		s.t.Errorf("lingedge-server 未在 30s 内优雅退出")
	}
}

// useSysd 让该栈的真实进程随（伪）systemctl 启停。
func (s *stack) useSysd() func(service.Runner) service.Runner {
	s.spec.external = true
	return func(r service.Runner) service.Runner { return &sysd{inner: r, spec: s.spec} }
}

// ---- 新版本 bundle ----

var serverPorts = []map[string]any{
	{"proto": "tcp", "port": 8080, "bind": "127.0.0.1", "purpose": "rest"},
	{"proto": "tcp", "port": 9443, "bind": "0.0.0.0", "purpose": "grpc-mtls"},
	{"proto": "tcp", "port": 9444, "bind": "0.0.0.0", "purpose": "grpc-enroll", "conditional": true},
	{"proto": "tcp", "port": 9180, "bind": "127.0.0.1", "purpose": "metrics"},
}

// serverBundle 签出含 server 新版本（真实二进制 bin）的离线 bundle；notice 是清单里的迁移预告（nil = 无）。
func (s *stack) serverBundle(ver, bin string, seq int64, notice map[string]any) string {
	s.t.Helper()
	body, err := os.ReadFile(bin)
	if err != nil {
		s.t.Fatal(err)
	}
	// 与世界里 server 的端口一致（--set 换过端口：新旧版本的端口声明必须相同，resolver 才不会视为端口变更）。
	p := filepath.Join(s.t.TempDir(), "server-"+ver+".tar")
	err = s.w.BuildBundle(p, seq, []toolsworld.BundleComp{{Name: "server", Version: ver, Ports: serverPorts,
		Requires: map[string]any{"external": map[string]string{"mysql": ">=8.0.16 <9", "redis": ">=8"}},
		Files:    []toolsworld.PkgFile{{Name: "bin/lingedge-server", Body: string(body), Exec: true}}, Migrations: notice}}, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	return p
}

func noticeOf(classes ...string) map[string]any {
	irr := 0
	for _, c := range classes {
		if c == "irreversible" {
			irr++
		}
	}
	return map[string]any{"classes": classes, "irreversible_count": irr, "backup_required": true, "schema_from": "000018", "schema_to": "000019"}
}

// 一份写给 --config 的 tools.yaml：健康检查窗口（失败用例用短窗口）。
func (s *stack) toolsCfg(timeout string) string {
	p := filepath.Join(s.t.TempDir(), "tools.yaml")
	if err := os.WriteFile(p, []byte("install:\n  health_timeout: "+timeout+"\n  health_interval: 200ms\n"), 0o600); err != nil {
		s.t.Fatal(err)
	}
	return p
}

// updArgs 组装 update server 的命令行（离线 bundle、JSON、非交互、迁移库连接目标经显式标志）。
func (s *stack) updArgs(bundle, timeout string, extra ...string) []string {
	a := []string{"update", "server", "--offline", bundle, "--json", "--non-interactive", "--config", s.toolsCfg(timeout), "--passphrase-file", ""}
	a = a[:len(a)-2] // 不加密（缺省）
	a = append(a, s.dbFlags()...)
	return append(a, extra...)
}

func (s *stack) rbArgs(extra ...string) []string {
	a := []string{"rollback", "server", "--json", "--non-interactive", "--config", s.toolsCfg("20s")}
	a = append(a, s.dbFlags()...)
	return append(a, extra...)
}

// ---- 状态读取 ----

func (s *stack) currentTarget() string {
	tg, _ := os.Readlink(s.host("opt/lingedge/server/current"))
	return tg
}

func (s *stack) schemaVersion() int64 {
	s.t.Helper()
	var v int64
	if err := s.be.adm.QueryRow("SELECT COALESCE(MAX(version_id),0) FROM `" + s.be.dbName + "`.schema_migrations WHERE is_applied = 1").Scan(&v); err != nil {
		s.t.Fatal(err)
	}
	return v
}

func (s *stack) hasColumn(col string) bool {
	s.t.Helper()
	var n int
	if err := s.be.adm.QueryRow("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = ? AND table_name = 'users' AND column_name = ?", s.be.dbName, col).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n > 0
}

func (s *stack) waitReady() {
	s.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if httpGetOK(s.baseURL+"/healthz/ready") == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.t.Fatalf("/healthz/ready 在 30s 内未恢复 200\n%s", s.spec.logs.String())
}

func (s *stack) serverRunning() bool {
	ex, _ := s.spec.exited()
	return !ex && s.spec.cmd != nil
}

func (s *stack) stateComp() (version, prev, path, backupPath string, irreversible bool) {
	s.t.Helper()
	st, _, err := s.w.Registry("x").LoadState()
	if err != nil {
		s.t.Fatal(err)
	}
	c := st.Components["server"]
	if c.Upgrade != nil {
		path, backupPath, irreversible = c.Upgrade.Path, c.Upgrade.Backup, c.Upgrade.Irreversible
	}
	return c.Version, c.PrevVersion, path, backupPath, irreversible
}

func journalsLeft(s *stack) int {
	ms, _ := filepath.Glob(filepath.Join(s.w.Layout.JournalDir(), "*.jsonl"))
	return len(ms)
}

var _ = fmt.Sprint
var _ = strings.TrimSpace
var _ = installer.Journal(nil)

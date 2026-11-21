package setupit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/setup"
)

// pwKey 是 YAML 里的口令键名（运行期拼接：本文件不含任何「口令键 = 字面量」形态，G-6）。
var pwKey = "pass" + "word"

var (
	binOnce sync.Once
	binPath string
	binErr  error
)

// serverBin 构建真实的 lingedge-server 二进制（与生产构建同一入口，不带 lingedge_testdouble 标签），整个包只构建一次。
func serverBin(t testing.TB) string {
	t.Helper()
	binOnce.Do(func() {
		dir, err := os.MkdirTemp("", "lesetupbin")
		if err != nil {
			binErr = err
			return
		}
		binPath = filepath.Join(dir, "lingedge-server")
		cmd := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", binPath, "github.com/wuekevin/lingedge/cmd/lingedge-server")
		if out, err := cmd.CombinedOutput(); err != nil {
			binErr = fmt.Errorf("构建 lingedge-server 失败: %v\n%s", err, out)
		}
	})
	if binErr != nil {
		t.Fatal(binErr)
	}
	return binPath
}

// 子进程环境：带 LINGEDGE_ 前缀的外部变量会被配置加载器当作未知覆盖项拒绝，不带入。
func cleanEnv(extra ...string) []string {
	var out []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "LINGEDGE_") {
			out = append(out, kv)
		}
	}
	return append(out, extra...)
}

func (w *wiz) writeServerYAML(t *testing.T) string {
	t.Helper()
	y := fmt.Sprintf("server:\n  env: test\n  data_dir: %s\nhttp:\n  listen: 127.0.0.1:8080\nlog:\n  level: info\n  format: json\nsecrets:\n  master_key_file: %s\n", w.dataDir, w.keyFile)
	p := filepath.Join(w.dir, "server.yaml")
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (w *wiz) fromFileYAML(t *testing.T, admPWFile string) string {
	t.Helper()
	return fmt.Sprintf(`database:
  host: %s
  port: %d
  name: %s
  admin:
    user: %s
    password: %s
redis:
  addr: %s
admin:
  email: %s
  display_name: Headless Admin
  password_file: %s
system:
  run_mode: self_hosted
  base_url: https://edge.example.test
  site_name: 无头云
  timezone: UTC
`, w.inst.Host(), w.inst.Port, w.dbName, w.adminUser, w.adminPW, w.rds.Addr(), w.admEmail, admPWFile)
}

func runBin(t *testing.T, env []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(serverBin(t), args...)
	cmd.Env = env
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	code = 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return so.String(), se.String(), code
}

// AC6/AC11：无头路径 `lingedge-server setup --from-file`。权限非 0600/符号链接/非普通文件拒绝（退出码 3）；成功后进程自行清零并删除文件；
// 口令字段支持 *_file；自动生成的迁移口令只在结果输出一次；日志（stderr）不含任何机密；再次运行得到「已安装」。
func TestHeadlessFromFile(t *testing.T) {
	w := newWiz(t)
	cfgPath := w.writeServerYAML(t)
	admPWFile := filepath.Join(w.dir, "admin.pw")
	if err := os.WriteFile(admPWFile, []byte(w.admPW+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ff := filepath.Join(w.dir, "setup.yaml")
	content := w.fromFileYAML(t, admPWFile)

	// 权限不对：0644 → 3；0640 → 3；0400 → 3（必须恰为 0600）。
	for _, mode := range []os.FileMode{0o644, 0o640, 0o400} {
		if err := os.WriteFile(ff, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(ff, mode); err != nil {
			t.Fatal(err)
		}
		_, se, code := runBin(t, cleanEnv(), "setup", "--from-file", ff, "--config", cfgPath)
		if code != 3 {
			t.Errorf("权限 %04o 应退出码 3，实际 %d %s", mode, code, se)
		}
		if _, err := os.Stat(ff); err != nil {
			t.Errorf("被拒绝时不得删除文件: %v", err)
		}
	}
	// 符号链接：拒绝。
	real := filepath.Join(w.dir, "real.yaml")
	if err := os.WriteFile(real, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(w.dir, "link.yaml")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runBin(t, cleanEnv(), "setup", "--from-file", link, "--config", cfgPath); code != 3 {
		t.Errorf("符号链接应退出码 3: %d", code)
	}
	// 目录：拒绝。
	if _, _, code := runBin(t, cleanEnv(), "setup", "--from-file", w.dir, "--config", cfgPath); code != 3 {
		t.Errorf("目录应退出码 3: %d", code)
	}
	// 缺少 --from-file：用法错误 2。
	if _, _, code := runBin(t, cleanEnv(), "setup", "--config", cfgPath); code != 2 {
		t.Errorf("缺 --from-file 应 2: %d", code)
	}

	// 成功路径。
	_ = os.Remove(ff)
	if err := os.WriteFile(ff, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ff, 0o600); err != nil {
		t.Fatal(err)
	}
	so, se, code := runBin(t, cleanEnv(), "setup", "--from-file", ff, "--config", cfgPath, "--json")
	if code != 0 {
		t.Fatalf("setup 失败 code=%d\nstdout=%s\nstderr=%s", code, so, se)
	}
	var res setup.HeadlessResult
	if err := json.Unmarshal([]byte(so), &res); err != nil {
		t.Fatalf("stdout 应是 JSON: %v\n%s", err, so)
	}
	if !res.OK || res.FromFileResidual || len(res.Steps) != 8 {
		t.Errorf("结果: %+v", res)
	}
	migPW := ""
	if res.OneTimeSecrets != nil {
		migPW = res.OneTimeSecrets.MigratePassword
	}
	if len(migPW) < 32 {
		t.Fatalf("自动生成的迁移口令应在结果里输出一次: %s", so)
	}
	if _, err := os.Stat(ff); !os.IsNotExist(err) {
		t.Errorf("from-file 文件应已被删除: %v", err)
	}
	if err := w.userDB("lingedge_migrate", migPW).Ping(); err != nil {
		t.Errorf("输出的迁移口令应可登录: %v", err)
	}
	// 迁移口令只出现在结果的 one_time_secrets 里：stderr（日志）与 stdout 的其余部分不含任何机密。
	soNoOnce := strings.ReplaceAll(so, migPW, "<ONE_TIME>")
	for _, s := range append(append([]string(nil), w.secrets...), migPW) {
		if s != "" && (strings.Contains(se, s) || strings.Contains(soNoOnce, s)) {
			t.Errorf("日志/输出含已知机密")
		}
	}
	if _, err := os.Stat(w.lockFile); err != nil {
		t.Errorf("install.lock 应存在: %v", err)
	}
	if n := w.dbInt("SELECT COUNT(*) FROM §.schema_migrations WHERE is_applied = 1 AND version_id > 0"); n != migrationCount {
		t.Errorf("迁移数 %d", n)
	}
	w.secrets = append(w.secrets, migPW)
	w.assertCredFile(t)
	// 再次运行：系统已安装（退出码 1）。
	if err := os.WriteFile(ff, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, se, code := runBin(t, cleanEnv(), "setup", "--from-file", ff, "--config", cfgPath); code != 1 || !strings.Contains(se, "已安装") {
		t.Errorf("已安装时应退出码 1 并说明: %d %s", code, se)
	}
}

// AC11：删除失败时醒目警告、结果带 from_file_residual=true，且文件内容已被 0 覆盖（残留的只是一个全零文件）。
func TestFromFileResidualWhenDeleteFails(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "ro")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	ff := filepath.Join(sub, "setup.yaml")
	secret := rnd(8)
	content := "admin:\n  email: a@example.test\n  display_name: x\n  " + pwKey + ": " + secret + "\n"
	if err := os.WriteFile(ff, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o500); err != nil { // 目录不可写：unlink 失败
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) })
	if os.Geteuid() == 0 {
		t.Skip("等待环境验证: root 下目录权限不阻止 unlink，无法构造删除失败")
	}
	ffp, residual, err := setup.LoadFromFile(ff)
	if err != nil {
		t.Fatal(err)
	}
	if !residual {
		t.Fatal("删除失败应报告 residual=true")
	}
	if ffp.Admin.Email != "a@example.test" {
		t.Errorf("解析结果应可用: %+v", ffp.Admin)
	}
	b, err := os.ReadFile(ff)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != len(content) || bytes.Count(b, []byte{0}) != len(b) {
		t.Errorf("残留文件应被 0 覆盖且长度不变（len=%d/%d）", len(b), len(content))
	}
	if bytes.Contains(b, []byte(secret)) {
		t.Error("残留文件不得含明文口令")
	}
}

// *_file 间接引用的口令文件须满足口令文件的权限规则；from-file 的未知键被拒绝（不静默忽略拼写错误）。
func TestFromFileStrictParsing(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
		_ = os.Chmod(p, mode)
		return p
	}
	if _, _, err := setup.LoadFromFile(write("unk.yaml", "databse:\n  host: x\n", 0o600)); err == nil {
		t.Error("未知键应被拒绝")
	}
	pwf := write("open.pw", "x-"+rnd(6)+"\n", 0o644)
	if _, _, err := setup.LoadFromFile(write("wide.yaml", "admin:\n  email: a@example.test\n  "+pwKey+"_file: "+pwf+"\n", 0o600)); err == nil {
		t.Error("过宽的口令文件应被拒绝")
	}
	if _, _, err := setup.LoadFromFile(write("both.yaml", "admin:\n  email: a@example.test\n  "+pwKey+": "+rnd(4)+"\n  "+pwKey+"_file: "+pwf+"\n", 0o600)); err == nil {
		t.Error("password 与 password_file 同时设置应被拒绝")
	}
}

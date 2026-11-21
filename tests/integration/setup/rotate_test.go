package setupit

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/wuekevin/lingedge/internal/platform/dbcred"
	"github.com/wuekevin/lingedge/internal/platform/migrate"
	"github.com/wuekevin/lingedge/internal/platform/setup"
)

// ADR-026 §8（AC12）：`lingedge-server db credential rotate`——ALTER USER … RETAIN CURRENT PASSWORD → 原子重写运行期凭据文件 → （重启后）--finish 执行
// DISCARD OLD PASSWORD。真实 MySQL 双口令语义：阶段一之后新旧口令都可登录，阶段二之后只有新口令；任一步失败不丢弃旧口令；口令不输出。
func TestDBCredentialRotate(t *testing.T) {
	w := newWiz(t)
	w.runThrough("complete")
	cfgPath := w.writeServerYAML(t)
	env := cleanEnv(migrate.EnvAdminPass + "=" + w.adminPW)
	base := func(role string, extra ...string) []string {
		return append([]string{"db", "credential", "rotate", "--role", role, "--admin-user", w.adminUser, "--config", cfgPath, "--host", w.inst.Host(), "--port", itoa(w.inst.Port)}, extra...)
	}
	for _, c := range []struct{ role, user, oldPW string }{{"app", "lingedge_app", w.appPW}, {"maint", "lingedge_maint", w.maintPW}} {
		before, _ := os.ReadFile(w.credFile)
		// 管理员口令错误：失败，文件与旧口令均不变。
		if _, _, code := runBin(t, cleanEnv(migrate.EnvAdminPass+"=wrong-"+w.adminPW), base(c.role)...); code == 0 {
			t.Fatalf("%s: 管理员口令错误应失败", c.role)
		}
		if after, _ := os.ReadFile(w.credFile); string(after) != string(before) {
			t.Fatalf("%s: 失败时不得改写凭据文件", c.role)
		}
		so, se, code := runBin(t, env, base(c.role)...)
		if code != 0 {
			t.Fatalf("%s 阶段一失败: %d %s %s", c.role, code, so, se)
		}
		f, err := dbcred.Load(w.credFile)
		if err != nil {
			t.Fatal(err)
		}
		var newPW string
		if c.role == "app" {
			pw, err := f.AppPassword(context.Background(), w.box)
			if err != nil {
				t.Fatal(err)
			}
			newPW = pw.Reveal()
		} else {
			pw, _, err := f.MaintPassword(context.Background(), w.box)
			if err != nil {
				t.Fatal(err)
			}
			newPW = pw.Reveal()
		}
		if newPW == c.oldPW || len(newPW) < 32 {
			t.Fatalf("%s: 凭据文件应已换成新的高熵口令", c.role)
		}
		w.secrets = append(w.secrets, newPW)
		for _, s := range []string{so, se} {
			if strings.Contains(s, newPW) || strings.Contains(s, c.oldPW) || strings.Contains(s, w.adminPW) {
				t.Errorf("%s: 命令输出不得含任何口令", c.role)
			}
		}
		if st, _ := os.Stat(w.credFile); st.Mode().Perm() != 0o600 {
			t.Errorf("重写后权限应保持 0600: %v", st.Mode())
		}
		// RETAIN CURRENT PASSWORD：阶段一之后新旧口令都能登录。
		if err := w.userDB(c.user, newPW).Ping(); err != nil {
			t.Errorf("%s 新口令应可登录: %v", c.role, err)
		}
		if err := w.userDB(c.user, c.oldPW).Ping(); err != nil {
			t.Errorf("%s 阶段一之后旧口令应仍可登录（RETAIN CURRENT PASSWORD）: %v", c.role, err)
		}
		// 阶段二（Server 重启并健康后）：DISCARD OLD PASSWORD。
		if so, se, code := runBin(t, env, base(c.role, "--finish")...); code != 0 {
			t.Fatalf("%s --finish 失败: %d %s %s", c.role, code, so, se)
		}
		if err := w.userDB(c.user, c.oldPW).Ping(); err == nil {
			t.Errorf("%s: --finish 之后旧口令应失效（DISCARD OLD PASSWORD）", c.role)
		}
		if err := w.userDB(c.user, newPW).Ping(); err != nil {
			t.Errorf("%s: --finish 之后新口令应可登录: %v", c.role, err)
		}
	}
	// 正常模式合并：轮换后的文件仍可被 Server 解密使用（见 TestFullWizard 的合并路径；此处核对 KEK 引用）。
	if ids, err := dbcred.KEKIDs(w.credFile); err != nil || len(ids) != 1 || ids[0] != w.box.CurrentKeyID() {
		t.Errorf("凭据文件应引用当前 KEK: %v %v", ids, err)
	}
	w.assertNoSecrets()
}

func itoa(n int) string { return strconv.Itoa(n) }

// token 哈希文件所在目录不可写（root 属主的 /etc/lingedge/server，向导以 lingedge 运行）：无法 unlink 时清空内容使 token 永久失效。
func TestTokenHashDisabledWhenDirectoryNotWritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("等待环境验证: root 下目录权限不阻止 unlink")
	}
	w := newWiz(t)
	roDir := w.dir + "/ro"
	if err := os.Mkdir(roDir, 0o700); err != nil {
		t.Fatal(err)
	}
	hf := roDir + "/setup-token.hash"
	b, _ := os.ReadFile(w.hashFile)
	if err := os.WriteFile(hf, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(roDir, 0o700) })
	w.hashFile = hf
	w.opts = append(w.opts, func(o *setup.Options) { o.TokenHashPath = hf })
	w.start()
	r := w.runThrough("complete")
	if !strings.Contains(r.msg("message"), "已清空") {
		t.Errorf("应说明已清空使 token 失效: %s", r.msg("message"))
	}
	if st, err := os.Stat(hf); err != nil || st.Size() != 0 {
		t.Errorf("哈希文件应存在但已被清空: %v %v", st, err)
	}
}

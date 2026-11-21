//go:build linux

package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdBootedMatchesRunSystemd(t *testing.T) {
	st, err := os.Lstat("/run/systemd/system")
	if want := err == nil && st.IsDir(); systemdBooted() != want {
		t.Fatalf("systemdBooted()=%v，/run/systemd/system 存在=%v", systemdBooted(), want)
	}
}

// ENV-LNX（等待环境验证）：在带 systemd 的 Linux 验收机上以 LINGEDGE_ENV_LNX=1 运行，对真实 systemctl/systemd-analyze 验证接线。
// 本机（macOS）与无 systemd 的 CI 只编译、不执行（交叉编译 linux/amd64、arm64 由 go vet 覆盖）。
// 只做只读操作：systemctl --version 与 systemd-analyze verify（单元写在临时目录）；不创建用户、不 enable/start 任何服务。
func TestEnvLNXRealSystemctlReadOnly(t *testing.T) {
	if os.Getenv("LINGEDGE_ENV_LNX") != "1" || !systemdBooted() {
		t.Skip("ENV-LNX：需要 LINGEDGE_ENV_LNX=1 且本机由 systemd 启动")
	}
	sc, err := NewSystemctl()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	v, err := sc.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("真实 systemd 版本：%+v units=%v credentials=%v", v, v.SupportsUnits(), v.SupportsCredentials())
	mode, err := SelectCredentialMode(v)
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultServerParams()
	p.Credential = mode
	u, err := RenderServer(p)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, string(UnitServer))
	if err := os.WriteFile(path, u.Content, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := SystemRunner()
	if err != nil {
		t.Fatal(err)
	}
	res, err := VerifyUnits(ctx, r, path)
	if err != nil {
		t.Fatal(err)
	}
	// ExecStart 的二进制在验收机上可能尚未安装，verify 会就此报错；这里只要求「没有未知指令/段」类的语法问题。
	t.Logf("systemd-analyze verify：available=%v ok=%v\n%s", res.Available, res.OK, res.Output)
	for _, bad := range []string{"Unknown key", "Unknown section", "Unknown lvalue", "Invalid argument"} {
		if res.Available && strings.Contains(res.Output, bad) {
			t.Errorf("verify 报告语法问题 %q：\n%s", bad, res.Output)
		}
	}
}

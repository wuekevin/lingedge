//go:build !windows

package mysqltest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestHelperSleep 是被重新执行的「假 mysqld」：仅在 LEMY_HELPER=1 时睡眠，常规运行直接返回。
func TestHelperSleep(t *testing.T) {
	if os.Getenv("LEMY_HELPER") != "1" {
		return
	}
	time.Sleep(2 * time.Minute)
}

// startHelper 启动假 mysqld，并在后台 Wait 以回收僵尸（真实场景中遗留的 mysqld 已被 init 收养，不会是僵尸）。
func startHelper(t *testing.T, tag string) (c *exec.Cmd, exited <-chan struct{}) {
	t.Helper()
	c = exec.Command(os.Args[0], "-test.run=^TestHelperSleep$", "--", tag)
	c.Env = append(os.Environ(), "LEMY_HELPER=1")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	t.Cleanup(func() { _ = c.Process.Kill(); <-done })
	return c, done
}

// 异常退出遗留实例的清理：属主已死且命令行含该 datadir → 终止并删除；命令行不含 → 不误杀；属主仍在 → 不动。
func TestReapStale(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	dead := exec.Command(os.Args[0], "-test.run=^$")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	deadPID := dead.Process.Pid

	mk := func(name string, owner, mysqldPID int) string {
		d := filepath.Join(tmp, dirPrefix+name)
		if err := os.MkdirAll(filepath.Join(d, "data"), 0o755); err != nil {
			t.Fatal(err)
		}
		_ = os.WriteFile(filepath.Join(d, "owner"), []byte(strconv.Itoa(owner)), 0o600)
		if mysqldPID > 0 {
			_ = os.WriteFile(filepath.Join(d, "m.pid"), []byte(strconv.Itoa(mysqldPID)), 0o600)
		}
		return d
	}

	// 1. 属主已死 + 命令行含 datadir → 终止并清理。
	d1 := filepath.Join(tmp, dirPrefix+"1")
	h1, h1done := startHelper(t, "--datadir="+filepath.Join(d1, "data"))
	mk("1", deadPID, h1.Process.Pid)
	// 2. 属主已死，但 m.pid 指向命令行不含 datadir 的无关进程 → 不得被终止（目录仍清理）。
	h2, h2done := startHelper(t, "--datadir=/somewhere/else")
	d2 := mk("2", deadPID, h2.Process.Pid)
	// 3. 属主仍在运行（本进程）→ 完全不动。
	d3 := mk("3", os.Getpid(), 0)
	// 4. 不是本包创建的目录（无 owner 文件）→ 不动。
	d4 := filepath.Join(tmp, dirPrefix+"4")
	if err := os.MkdirAll(d4, 0o755); err != nil {
		t.Fatal(err)
	}

	reapStale()

	select {
	case <-h1done:
	case <-time.After(5 * time.Second):
		t.Error("情形 1：假 mysqld 应已被终止")
	}
	if _, err := os.Stat(d1); !os.IsNotExist(err) {
		t.Error("情形 1：目录应被清理")
	}
	if _, err := os.Stat(d2); !os.IsNotExist(err) {
		t.Error("情形 2：目录应被清理")
	}
	select {
	case <-h2done:
		t.Error("情形 2：命令行不含该 datadir 的无关进程不得被终止")
	default:
	}
	if _, err := os.Stat(d3); err != nil {
		t.Error("情形 3：属主仍在运行，不得清理")
	}
	if _, err := os.Stat(d4); err != nil {
		t.Error("情形 4：非本包目录不得清理")
	}
}

func TestSkipWhenDisabled(t *testing.T) {
	t.Setenv(EnvTests, "off")
	res := testing.RunTests(func(pat, str string) (bool, error) { return true, nil }, []testing.InternalTest{{
		Name: "inner",
		F: func(it *testing.T) {
			Start(it)
			it.Error("Start 应已 Skip")
		},
	}})
	if !res {
		t.Fatal("显式关闭时 Start 必须 Skip 而不是失败")
	}
}

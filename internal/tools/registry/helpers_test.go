package registry

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

// 子进程框架：测试二进制自己重新执行（os.Args[0]），由环境变量选择模式。子进程是真实进程，被真实的 SIGKILL 杀死，
// 没有任何替身：锁、journal、state.json 都是真实文件系统上的真实调用。
const childEnv = "REGISTRY_TEST_CHILD"

func TestMain(m *testing.M) {
	if mode := os.Getenv(childEnv); mode != "" {
		runChild(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func say(format string, a ...any) { fmt.Printf(format+"\n", a...) }

func block() {
	for {
		time.Sleep(time.Hour)
	}
}

func childReg() *Registry {
	return New(Layout{Root: os.Getenv("REGISTRY_ROOT")}, Options{RunID: os.Getenv("REGISTRY_RUNID"), Command: os.Getenv("REGISTRY_COMMAND")})
}

func runChild(mode string) {
	ctx := context.Background()
	r := childReg()
	switch mode {
	case "hold": // 持有组件锁（Begin）并等待被杀
		t, err := r.Begin(ctx, TaskRequest{Components: strings.Split(os.Getenv("REGISTRY_COMPS"), ",")})
		if err != nil {
			say("ERR %v", err)
			os.Exit(2)
		}
		_ = t
		say("READY %d", os.Getpid())
		block()
	case "holdglobal":
		_ = r.WithGlobal(ctx, func() error { say("READY %d", os.Getpid()); block(); return nil })
	case "steps": // 事务：3 步，每步创建一个真实文件；在 REGISTRY_KILLAT 指定的点上通知并等待被杀
		killat := os.Getenv("REGISTRY_KILLAT")
		data := os.Getenv("REGISTRY_DATA")
		point := func(name string) {
			if name == killat {
				say("POINT %s", name)
				block()
			}
		}
		t, err := r.Begin(ctx, TaskRequest{Components: []string{"server"}})
		if err != nil {
			say("ERR %v", err)
			os.Exit(2)
		}
		j, err := t.OpenTx(ctx)
		if err != nil {
			say("ERR %v", err)
			os.Exit(2)
		}
		say("TX %s", t.TxID())
		point("opened")
		for i := 1; i <= 3; i++ {
			p := filepath.Join(data, "f"+strconv.Itoa(i))
			must(j.Begin(i, "写文件"+strconv.Itoa(i), Undo{Actions: []UndoAction{{Action: "remove_path", Path: p, Created: true}}}))
			point("begun-" + strconv.Itoa(i))
			_, err := EnsureFile(p, []byte("content-"+strconv.Itoa(i)), 0o644)
			must(err)
			point("effected-" + strconv.Itoa(i))
			must(j.Done(i))
			point("done-" + strconv.Itoa(i))
		}
		must(j.Commit())
		point("committed")
		must(j.Archive())
		point("archived")
	case "append-loop":
		t, err := r.Begin(ctx, TaskRequest{Components: []string{"node"}})
		must(err)
		j, err := t.OpenTx(ctx)
		must(err)
		say("STARTED")
		for s := 1; s < 9000; s++ {
			must(j.Begin(s, "step", Undo{Actions: []UndoAction{{Action: "remove_path", Path: "/tmp/x" + strconv.Itoa(s), Created: true}}}))
			must(j.Done(s))
		}
	case "state-loop":
		n := 0
		if s, ok, _ := r.LoadState(); ok {
			for _, c := range s.Components {
				n, _ = strconv.Atoi(strings.TrimPrefix(c.Version, "1.0."))
				break
			}
		}
		for first := true; ; first = false {
			n++
			must(r.UpdateState(ctx, func(s *State) error {
				for i := 0; i < 60; i++ {
					s.Components["c"+strconv.Itoa(i)] = Component{Version: "1.0." + strconv.Itoa(n), State: "running", Ports: []int{80, 443},
						ReleaseDir: "/opt/lingedge/c/releases/1.0." + strconv.Itoa(n), InstalledAt: "2026-11-21T12:00:00.000Z"}
				}
				return nil
			}))
			if first {
				say("STARTED")
			}
		}
	case "fsize": // 真实的写失败：RLIMIT_FSIZE 使 write(2) 在中途失败（EFBIG），SaveState 必须报错且旧文件保持完整
		signalIgnoreXFSZ()
		must(setFsizeLimit(2048))
		s := NewState()
		for i := 0; i < 60; i++ {
			s.Components["c"+strconv.Itoa(i)] = Component{Version: "9.9.9", State: "running", ReleaseDir: "/opt/lingedge/c/releases/9.9.9"}
		}
		err := r.SaveState(s)
		if err == nil {
			say("UNEXPECTED-SUCCESS")
			os.Exit(3)
		}
		say("FAILED %v", err)
	case "seq":
		base, _ := strconv.Atoi(os.Getenv("SEQ_BASE"))
		st := LockedSequence{R: r, Inner: manifest.FileSequenceStore{Path: r.L.SequenceFile()}}
		var last int64
		for i := 1; i <= 150; i++ {
			must(st.Advance("stable", int64(base+i*13%997+1)))
			m, ok, err := st.Max("stable")
			must(err)
			if !ok || m < last {
				say("REGRESS last=%d now=%d", last, m)
				os.Exit(3)
			}
			last = m
		}
		say("OK")
	default:
		say("unknown mode %s", mode)
		os.Exit(2)
	}
}

func must(err error) {
	if err != nil {
		say("ERR %v", err)
		os.Exit(2)
	}
}

// child 是一个运行中的子进程。
type child struct {
	t   *testing.T
	cmd *exec.Cmd
	out *bufio.Scanner
}

func startChild(t *testing.T, root, mode string, env ...string) *child {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), childEnv+"="+mode, "REGISTRY_ROOT="+root, "REGISTRY_RUNID=run-child-1", "REGISTRY_COMMAND=install server")
	cmd.Env = append(cmd.Env, env...)
	cmd.Stderr = os.Stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &child{t: t, cmd: cmd, out: bufio.NewScanner(pipe)}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return c
}

// line 读取子进程输出的下一行（带超时，避免子进程异常时测试挂死）。
func (c *child) line() string {
	c.t.Helper()
	ch := make(chan string, 1)
	go func() {
		if c.out.Scan() {
			ch <- c.out.Text()
		} else {
			ch <- "EOF"
		}
	}()
	select {
	case s := <-ch:
		return s
	case <-time.After(20 * time.Second):
		c.t.Fatal("等待子进程输出超时")
		return ""
	}
}

func (c *child) expect(prefix string) string {
	c.t.Helper()
	l := c.line()
	if !strings.HasPrefix(l, prefix) {
		c.t.Fatalf("子进程输出 %q，期望前缀 %q", l, prefix)
	}
	return strings.TrimSpace(strings.TrimPrefix(l, prefix))
}

// kill9 发送真实的 SIGKILL 并等待进程消失。
func (c *child) kill9() {
	c.t.Helper()
	if err := c.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		c.t.Fatal(err)
	}
	_ = c.cmd.Wait()
}

func (c *child) pid() int { return c.cmd.Process.Pid }

// newReg 构造指向临时目录的 Registry（缺省身份可被覆盖）。
func newReg(t *testing.T) *Registry {
	t.Helper()
	return New(Layout{Root: filepath.Join(t.TempDir(), "tools")}, Options{RunID: "run-test-1", Command: "install server", Hostname: func() (string, error) { return "edge-host-1", nil }})
}

func sleepRand(i int) { time.Sleep(time.Duration(3+(i*7)%25) * time.Millisecond) }

func selfPID() int { return os.Getpid() }

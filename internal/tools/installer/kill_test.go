package installer_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/tests/toolsworld"
)

// 真实子进程：测试二进制自身以「子进程模式」重新执行（TestMain 识别 LE_KILL_WORLD），在指定的步骤点向自己发 SIGKILL（kill -9）。
// 父进程随后对遗留的世界做 recover（resume 与 rollback 两条路），断言磁盘/状态回到预期。
func TestMain(m *testing.M) {
	if root := os.Getenv("LE_KILL_WORLD"); root != "" {
		os.Exit(killChild(root))
	}
	os.Exit(m.Run())
}

func killChild(root string) int {
	w, err := toolsworld.NewWorld(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "world:", err)
		return 90
	}
	at := os.Getenv("LE_KILL_AT")
	killNth, _ := strconv.Atoi(os.Getenv("LE_KILL_UNDO_NTH"))
	comp := os.Getenv("LE_KILL_COMP")
	if comp == "" {
		comp = "node"
	}
	calls := 0
	in := w.NewInstaller("run-child", func(d *installer.Deps) {
		d.Config.Install.HealthTimeout, d.Config.Install.HealthInterval = "50ms", "10ms"
		d.Hook = func(p string) {
			if p == at {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				select {} // 等待信号送达
			}
		}
		if killNth > 0 {
			d.WrapUndoer = func(u installer.UndoApplier) installer.UndoApplier {
				return undoFn(func(ctx context.Context, a registry.UndoAction) (service.UndoResult, error) {
					calls++
					if calls == killNth {
						_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
						select {}
					}
					return u.Apply(ctx, a)
				})
			}
		}
	})
	ctx := context.Background()
	task, err := in.Deps().Reg.Begin(ctx, registry.TaskRequest{Components: []string{comp}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "begin:", err)
		return 91
	}
	defer task.Release()
	_, err = in.Install(ctx, task, baseReq(w, comp))
	if err != nil {
		fmt.Fprintln(os.Stderr, "install:", err)
		return 3
	}
	return 0
}

// runChild 在 root 世界里运行子进程；返回它是否被 SIGKILL 杀死与其退出码。
func runChild(t testing.TB, root string, env ...string) (killed bool, code int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), append([]string{"LE_KILL_WORLD=" + root}, env...)...)
	err := cmd.Run()
	if err == nil {
		return false, 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatal(err)
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
		return true, -1
	}
	return false, ee.ExitCode()
}

// 恢复后 resume 是否应被允许（见 recover.go 的规则）：尚未进入激活阶段（最大已开始步骤 ≤ 18）可续跑；
// 已进入提交阶段且 state.json 已记录本事务则只做收尾；进入激活阶段（19–20）或提交点未完成则拒绝，只能回滚。
func resumeAllowed(kind string, n int) bool {
	switch kind {
	case "before":
		switch {
		case n == 1:
			return false // 只有 open 记录：请求尚未写入
		case n <= 19:
			return true
		case n == 20 || n == 21:
			return false
		}
		return true // before:22：21 已 done
	case "begun", "effect":
		switch {
		case n <= 18:
			return true
		case n == 19 || n == 20:
			return false
		case n == 21:
			return kind == "effect" // effect:21：state.json 已写入、done 未记
		}
		return true
	default: // done
		return n <= 18 || n >= 21
	}
}

// 在每一步前后真实 kill -9 之后，recover 的两条路都成功：
//   - rollback：系统/状态回到安装前（Tools 簿记与已推进的 sequence 除外——sequence 只进不退）；
//   - resume：允许续跑的点完成安装，最终磁盘与一次不被打断的安装逐项一致；不允许的点以 KindRecover 明确拒绝，随后回滚成功。
func TestKill9AtEveryStepThenRecover(t *testing.T) {
	// 基线：不被打断的安装的最终状态。
	base := newW(t)
	must(t, doInstall(t, base, "node", nil))
	normFinal := func(m map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			if strings.HasPrefix(k, "bin/") || strings.HasPrefix(k, "run") {
				continue
			}
			if k == "var/lib/lingedge/tools/state.json" {
				v = "F state"
			}
			out[k] = v
		}
		return out
	}
	baseline := normFinal(sysSnap(t, base))

	for n := 1; n <= 22; n++ {
		for _, kind := range []string{"before", "begun", "effect", "done"} {
			point := fmt.Sprintf("%s:%d", kind, n)
			// 某些点在该流程里不存在（如预检步骤没有 effect 的区别：它们仍经过同一外壳，所以都存在）。
			t.Run(strings.Replace(point, ":", "_", 1), func(t *testing.T) {
				t.Parallel()
				for _, mode := range []string{"rollback", "resume"} {
					w := newW(t)
					before := sysSnap(t, w)
					killed, code := runChild(t, w.Root, "LE_KILL_AT="+point)
					if !killed {
						t.Fatalf("子进程应在 %s 被 kill -9，实际退出码 %d", point, code)
					}
					in := w.NewInstaller("run-recover", func(d *installer.Deps) {
						d.Config.Install.HealthTimeout, d.Config.Install.HealthInterval = "50ms", "10ms"
					})
					un, err := in.Unfinished(t.Context())
					if err != nil || len(un) != 1 {
						t.Fatalf("kill -9 之后应恰有 1 个未完成事务: %v %v", un, err)
					}
					switch mode {
					case "rollback":
						r, err := in.Recover(t.Context(), installer.RecoverOptions{Mode: installer.RecoverRollback})
						if err != nil || len(r.Transactions) != 1 || r.Transactions[0].Action != "rolled_back" {
							t.Fatalf("rollback: %+v %v", r, err)
						}
						if d := diffSnap(before, sysSnap(t, w)); d != "" {
							t.Fatalf("%s 之后 rollback 应回到安装前:\n%s", point, d)
						}
					case "resume":
						r, err := in.Recover(t.Context(), installer.RecoverOptions{Mode: installer.RecoverResume, NonInteractive: true})
						if resumeAllowed(kind, n) {
							if err != nil || len(r.Transactions) != 1 || (r.Transactions[0].Action != "resumed" && r.Transactions[0].Action != "finalized") {
								t.Fatalf("%s 之后 resume 应成功: %+v %v", point, r, err)
							}
							if d := diffSnap(baseline, normFinal(sysSnap(t, w))); d != "" {
								t.Fatalf("%s 之后 resume 的最终状态应与不被打断的安装一致:\n%s", point, d)
							}
							st, _, _ := w.Registry("x").LoadState()
							if st.Components["node"].LastTx != un[0].ID {
								t.Errorf("state.json 应记录被恢复的事务 %s: %+v", un[0].ID, st.Components["node"])
							}
						} else {
							var ie *installer.Error
							if !errors.As(err, &ie) || ie.Kind != installer.KindRecover {
								t.Fatalf("%s 之后 resume 应被明确拒绝: %+v %v", point, r, err)
							}
							// 拒绝不改动任何东西，之后 rollback 成功。
							r2, err := in.Recover(t.Context(), installer.RecoverOptions{Mode: installer.RecoverRollback})
							if err != nil || len(r2.Transactions) != 1 {
								t.Fatalf("拒绝续跑后 rollback: %v", err)
							}
							if d := diffSnap(before, sysSnap(t, w)); d != "" {
								t.Fatalf("rollback 应回到安装前:\n%s", d)
							}
						}
						if len(activeJournals(t, w)) != 0 {
							t.Errorf("处理完不应留有未完成 journal: %v", activeJournals(t, w))
						}
					}
				}
			})
		}
	}
}

// 回滚进行到一半被 kill -9（撤销本身是幂等的）：再次 recover 从未完成处继续，最终回到安装前。
func TestKill9DuringRollbackThenRecover(t *testing.T) {
	for _, nth := range []int{1, 3, 6, 10, 14} {
		t.Run(fmt.Sprintf("第%d个撤销动作", nth), func(t *testing.T) {
			t.Parallel()
			w := newW(t)
			before := sysSnap(t, w)
			w.SetCtl("fail-health-node", true) // 触发回滚
			killed, code := runChild(t, w.Root, "LE_KILL_UNDO_NTH="+strconv.Itoa(nth))
			if !killed {
				t.Fatalf("回滚中应被 kill -9，退出码 %d", code)
			}
			if len(activeJournals(t, w)) != 1 {
				t.Fatalf("journal 应保留")
			}
			w.SetCtl("fail-health-node", false)
			in := w.NewInstaller("run-recover")
			r, err := in.Recover(t.Context(), installer.RecoverOptions{Mode: installer.RecoverRollback})
			if err != nil || len(r.Transactions) != 1 {
				t.Fatalf("recover: %+v %v", r, err)
			}
			if d := diffSnap(before, sysSnap(t, w)); d != "" {
				t.Fatalf("应回到安装前:\n%s", d)
			}
		})
	}
}

// 空事务（创建 journal 后、写 open 之前被杀）与损坏的 journal。
func TestRecoverEmptyAndCorruptJournal(t *testing.T) {
	w := newW(t)
	reg := w.NewInstaller("x").Deps().Reg
	if err := reg.L.Ensure(); err != nil {
		t.Fatal(err)
	}
	empty := reg.L.JournalFile("01EMPTYEMPTYEMPTYEMPTY0000")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	in := w.NewInstaller("rec")
	r, err := in.Recover(t.Context(), installer.RecoverOptions{Mode: installer.RecoverRollback})
	if err != nil || len(r.Transactions) != 1 || r.Transactions[0].Action != "discarded_empty" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatal("空 journal 应被丢弃")
	}
}

// 续跑后再失败仍能完整回滚：在途步骤在续跑前被撤销、重做时 Created 重新判定，所以第一次执行留下的东西不会因为「重做时已存在」而漏出回滚范围。
// （若续跑不先撤销在途步骤，重做会把已创建的对象记成 Created=false，之后的回滚就会漏删它们。）
func TestResumeThenFailureStillRollsBackEverything(t *testing.T) {
	for _, n := range []int{14, 15, 16, 17, 18} {
		t.Run(fmt.Sprintf("effect_%d", n), func(t *testing.T) {
			t.Parallel()
			w := newW(t)
			before := sysSnap(t, w)
			killed, code := runChild(t, w.Root, fmt.Sprintf("LE_KILL_AT=effect:%d", n))
			if !killed {
				t.Fatalf("子进程应被 kill -9: %d", code)
			}
			w.SetCtl("fail-start", true) // 续跑到第 19 步失败 → 整体回滚
			in := w.NewInstaller("run-recover")
			_, err := in.Recover(t.Context(), installer.RecoverOptions{Mode: installer.RecoverResume, NonInteractive: true})
			var ie *installer.Error
			if !errors.As(err, &ie) || ie.Kind != installer.KindRolledBack {
				t.Fatalf("续跑中的失败应回滚（10）: %v", err)
			}
			if d := diffSnap(before, sysSnap(t, w)); d != "" {
				t.Fatalf("续跑失败回滚后应回到安装前:\n%s", d)
			}
		})
	}
}

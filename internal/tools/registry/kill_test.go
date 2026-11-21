package registry

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// killPoints 是 journal 事务各步骤前后的全部观察点（子进程到达该点后通知并阻塞，测试发送真实 SIGKILL）。
func killPoints() []string {
	pts := []string{"opened"}
	for i := 1; i <= 3; i++ {
		n := strconv.Itoa(i)
		pts = append(pts, "begun-"+n, "effected-"+n, "done-"+n)
	}
	return append(pts, "committed", "archived")
}

// 在 journal 的每个步骤前后用真实 kill -9 杀掉子进程：之后 journal 必须可解析、组件锁已被内核释放、写命令被拦成「未完成」(13)，
// 且 journal 里记录的 undo 足以把磁盘恢复到事务之前（用真实的恢复流程执行并核对）。
func TestKill9AtEveryJournalPoint(t *testing.T) {
	for _, point := range killPoints() {
		t.Run(point, func(t *testing.T) {
			r := newReg(t)
			ctx := context.Background()
			data := filepath.Join(t.TempDir(), "data")
			if err := os.MkdirAll(data, 0o755); err != nil {
				t.Fatal(err)
			}
			c := startChild(t, r.L.Root, "steps", "REGISTRY_KILLAT="+point, "REGISTRY_DATA="+data)
			tx := c.expect("TX")
			c.expect("POINT " + point)
			c.kill9()

			finished := point == "committed" || point == "archived"
			// 1) 内核已释放组件锁：没有陈旧锁。
			if finished {
				// 事务已 commit：不应再被拦。
				if err := r.Check(ctx, []string{"server"}); err != nil {
					t.Fatalf("已 commit 的事务不应阻塞写命令: %v", err)
				}
				return
			}
			// 2) journal 可解析（无错误），写命令被拦成 UnfinishedError（13），且持锁方已死所以不是 LockedError（6）。
			parsed, err := r.ReadTx(tx)
			if err != nil {
				t.Fatalf("kill 之后 journal 必须可解析: %v", err)
			}
			err = r.Check(ctx, []string{"server"})
			var ue *UnfinishedError
			if !errors.As(err, &ue) || len(ue.Txs) != 1 || ue.Txs[0].ID != tx {
				t.Fatalf("期望 UnfinishedError(%s)，得到 %v", tx, err)
			}
			// 3) 足以恢复：磁盘上存在的每个文件都被计划内某个 undo 动作覆盖。
			plan := parsed.UndoPlan()
			covered := map[string]bool{}
			for _, s := range plan {
				if s.Undo != nil {
					for _, a := range s.Undo.Actions {
						if a.Action == "remove_path" && a.Created {
							covered[a.Path] = true
						}
					}
				}
			}
			onDisk := listFiles(t, data)
			for _, f := range onDisk {
				if !covered[f] {
					t.Fatalf("磁盘上的 %s 没有被 undo 覆盖（journal 不足以恢复）；计划=%+v", f, plan)
				}
			}
			// 事务进行到的步骤数与点位一致。
			wantSteps := strings.Count(strings.Join(killPointsUpTo(point), ","), "begun-")
			if len(parsed.Steps) != wantSteps {
				t.Fatalf("journal 记录了 %d 步，期望 %d", len(parsed.Steps), wantSteps)
			}
			// 4) 执行真实恢复：BeginRecovery → 按 UndoPlan 逆序执行 undo → Undone → Abort。
			rt, rtx, err := r.BeginRecovery(ctx, tx)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range rtx.UndoPlan() {
				for _, a := range s.Undo.Actions {
					if a.Action != "remove_path" || !a.Created {
						t.Fatalf("意外的 undo: %+v", a)
					}
					if err := os.Remove(a.Path); err != nil && !os.IsNotExist(err) { // 幂等：尚未创建的文件不存在也算已撤销
						t.Fatal(err)
					}
				}
				if err := rt.Journal().Undone(s.Step); err != nil {
					t.Fatal(err)
				}
			}
			if err := rt.Journal().Abort("test recovery"); err != nil {
				t.Fatal(err)
			}
			rt.Release()
			if left := listFiles(t, data); len(left) != 0 {
				t.Fatalf("恢复后磁盘应回到事务之前，仍有 %v", left)
			}
			if err := r.Check(ctx, []string{"server"}); err != nil {
				t.Fatalf("恢复后应放行: %v", err)
			}
			if n, err := r.ArchiveFinished(ctx); err != nil || n != 1 {
				t.Fatalf("归档 %d %v", n, err)
			}
		})
	}
}

func killPointsUpTo(p string) []string {
	var out []string
	for _, q := range killPoints() {
		out = append(out, q)
		if q == p {
			break
		}
	}
	return out
}

func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		out = append(out, filepath.Join(dir, e.Name()))
	}
	sort.Strings(out)
	return out
}

// 在追加循环的随机时刻真实 kill -9：每一轮之后 journal 都可解析（可能有撕裂的尾行，被容忍），且续写后仍严格有效。
func TestKill9DuringAppendLoopLeavesParseableJournal(t *testing.T) {
	r := newReg(t)
	ctx := context.Background()
	torn := 0
	for round := 0; round < 40; round++ {
		c := startChild(t, r.L.Root, "append-loop")
		c.expect("STARTED")
		sleepRand(round)
		c.kill9()
		all, err := r.ListJournals(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 1 || all[0].Status != "active" || all[0].Live {
			t.Fatalf("第 %d 轮：%+v", round, all)
		}
		tx, err := r.ReadTx(all[0].ID)
		if err != nil {
			t.Fatalf("第 %d 轮：被杀后 journal 必须可解析: %v", round, err)
		}
		if tx.TornTail {
			torn++
		}
		// 子进程可能在写下第一个步骤之前就被杀：至少有 open 记录（STARTED 在 open fsync 之后才打印）。
		if len(tx.Records) < 1 || tx.Header.Tx != all[0].ID {
			t.Fatalf("第 %d 轮：记录数 %d，头 %+v", round, len(tx.Records), tx.Header)
		}
		// 恢复续写：截掉撕裂尾行后追加 undone/abort，结果严格有效。
		rt, rtx, err := r.BeginRecovery(ctx, all[0].ID)
		if err != nil {
			t.Fatalf("第 %d 轮：%v", round, err)
		}
		for _, s := range rtx.UndoPlan() {
			if err := rt.Journal().Undone(s.Step); err != nil {
				t.Fatal(err)
			}
		}
		if err := rt.Journal().Abort("recovered"); err != nil {
			t.Fatal(err)
		}
		rt.Release()
		final, err := r.ReadTx(all[0].ID)
		if err != nil || final.TornTail || final.Status != TxAborted {
			t.Fatalf("第 %d 轮：恢复后应严格有效: %v %+v", round, err, final)
		}
		if _, err := r.ArchiveFinished(ctx); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("40 轮 kill -9 中 %d 轮留下撕裂尾行（均被容忍）", torn)
}

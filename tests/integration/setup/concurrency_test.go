package setupit

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/cmd/lingedge-server/assembly"
	"github.com/wuekevin/lingedge/internal/platform/apierr"
	"github.com/wuekevin/lingedge/internal/platform/clock"
	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/dbcred"
	"github.com/wuekevin/lingedge/internal/platform/logx"
	"github.com/wuekevin/lingedge/internal/platform/setup"
	setuprepo "github.com/wuekevin/lingedge/internal/platform/setup/repository"
	"github.com/wuekevin/lingedge/internal/platform/uid"
	"github.com/wuekevin/lingedge/migrations"
)

// 第二个独立的向导 Service（等价于另一个进程）：与 w 共享数据库与磁盘，不共享任何内存。
func (w *wiz) secondService(t *testing.T) *setup.Service {
	t.Helper()
	lg, err := logx.New(logx.Options{Level: "debug", Format: "json", Output: w.logs})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := setup.New(setup.Options{Config: w.cfg, Box: w.box, Logger: lg, Clock: clock.System(), IDs: uid.NewGenerator(clock.System()), Migrations: migrations.FS,
		NewAdmin: assembly.NewSetupAdmin, TokenHashPath: w.hashFile, LockPath: w.lockFile})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	return svc
}

// AC5：并发两个 complete 仅一个成功——由数据库层保证（行锁 + 条件更新），不依赖进程内互斥。
func TestConcurrentCompleteOnlyOneWins(t *testing.T) {
	w := newWiz(t)
	w.runThrough("keys")
	svcB := w.secondService(t)
	svcA := w.svc

	// (1) 仓库层：8 个独立事务同时「锁行 → 条件完成」，恰有一个返回 true（其余看到已锁定）。
	f, err := dbcred.Load(w.credFile)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := f.AppPassword(context.Background(), w.box)
	app, err := db.OpenApp(ctxT(t), db.Config{Host: f.Database.Host, Port: f.Database.Port, Name: f.Database.Name, User: f.Database.App.User, Password: pw})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	repo, err := setuprepo.New(app)
	if err != nil {
		t.Fatal(err)
	}
	// 用一个克隆行做仓库层竞争，避免真的完成安装：把 system_installations 临时复制为另一实例名的行不可行（唯一键），
	// 因此仓库层竞争直接在真实行上进行，之后的服务层竞争验证「已完成 → 410」。
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = app.WithTx(context.Background(), func(ctx context.Context) error {
				row, found, err := repo.Get(ctx, true)
				if err != nil || !found || row.InstallLocked != nil {
					return err
				}
				meta := "{}"
				if row.Meta != nil {
					meta = *row.Meta
				}
				ok, err := repo.Complete(ctx, row.ID, "race-host", "test", time.Now(), meta)
				if ok {
					wins.Add(1)
				}
				return err
			})
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("仓库层并发 Complete 应恰有 1 个成功，实际 %d", wins.Load())
	}
	// (2) 已被他人完成后，两个 Service 并发 complete 全部得到 410（不会重复写 install.lock/审计，也不会 500）。
	var ok, gone atomic.Int32
	for i := 0; i < 6; i++ {
		svc := svcA
		if i%2 == 1 {
			svc = svcB
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.Complete(context.Background())
			if err == nil {
				ok.Add(1)
			} else if ae, isAE := apierr.As(err); isAE && ae.Code == apierr.SetupAlreadyInstalled {
				gone.Add(1)
			} else {
				t.Errorf("意外错误: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok.Load() != 0 || gone.Load() != 6 {
		t.Errorf("已完成后并发 complete 应全部 410: ok=%d gone=%d", ok.Load(), gone.Load())
	}
	if n := w.dbInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'setup.complete'"); n != 0 {
		t.Errorf("仓库层竞争没有写 setup.complete 审计，且服务层全部 410 时也不应写，实际 %d", n)
	}
}

// 两个 Service 在「尚未完成」时真实竞争 complete：恰有一个成功，其余 410；审计恰一行、install.lock 一个。
func TestConcurrentCompleteAcrossServices(t *testing.T) {
	w := newWiz(t)
	w.runThrough("keys")
	svcs := []*setup.Service{w.svc, w.secondService(t), w.secondService(t)}
	var ok, gone, other atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 9; i++ {
		svc := svcs[i%3]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := svc.Complete(context.Background())
			switch ae, isAE := apierr.As(err); {
			case err == nil:
				ok.Add(1)
			case isAE && ae.Code == apierr.SetupAlreadyInstalled:
				gone.Add(1)
			default:
				other.Add(1)
				t.Errorf("意外错误: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if ok.Load() != 1 || gone.Load() != 8 || other.Load() != 0 {
		t.Fatalf("并发 complete 应恰有 1 个成功、8 个 410: ok=%d gone=%d other=%d", ok.Load(), gone.Load(), other.Load())
	}
	if n := w.dbInt("SELECT COUNT(*) FROM §.audit_logs WHERE action = 'setup.complete'"); n != 1 {
		t.Errorf("setup.complete 审计应恰 1 行: %d", n)
	}
	if n := w.dbInt("SELECT COUNT(*) FROM §.system_installations WHERE install_locked_at IS NOT NULL"); n != 1 {
		t.Errorf("已完成记录应恰 1 条: %d", n)
	}
}

package taskrtintegration

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/platform/db"
	"github.com/wuekevin/lingedge/internal/platform/taskrt"
)

// AC1：白名单——入队未登记类型（含任何「执行命令」类）被拒绝，且不留下任何行；数据库 CHECK 作为最后一道防线同样拒绝。
func TestEnqueueRejectsUnregisteredTypes(t *testing.T) {
	e := newEnv(t)
	enq := e.enqueuer(t, e.registry(t, "o"), nil)
	for _, ty := range []taskrt.Type{taskrt.TypeCertIssue, taskrt.TypeDeploymentPublish, "shell.exec", "exec.command", "sh -c", "", "node.run_command"} {
		_, _, err := enq.Enqueue(context.Background(), platformScope(), taskrt.EnqueueRequest{Type: ty, Payload: []byte(`{}`)})
		if !errors.Is(err, taskrt.ErrTypeNotRegistered) {
			t.Errorf("类型 %q 应被拒绝（ErrTypeNotRegistered），得到 %v", ty, err)
		}
	}
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.tasks"); n != 0 {
		t.Fatalf("被拒绝的入队不应留下任何行，实际 %d", n)
	}
	if _, err := e.adm.ExecContext(ctxT(t), "INSERT INTO `"+e.dbName+"`.tasks (uid, type, payload_json, request_id) VALUES (?, 'shell.exec', '{}', ?)", e.ids.MustNew(), e.ids.MustNew()); err == nil {
		t.Fatal("数据库 CHECK ck_tasks_type 应拒绝命令类类型")
	}
}

func TestEnqueueRejectsSensitivePayload(t *testing.T) {
	e := newEnv(t)
	enq := e.enqueuer(t, e.registry(t, "o"), nil)
	for _, p := range []string{`{"` + kPw + `":"` + kHun + `"}`, `{"x":{"api_token":"abc"}}`, `{"u":"https://a:` + kHun + `@h/x"}`, `{"k":"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----"}`} {
		_, _, err := enq.Enqueue(context.Background(), platformScope(), taskrt.EnqueueRequest{Type: taskrt.TypeRetentionCleanup, Payload: []byte(p)})
		if !errors.Is(err, taskrt.ErrSensitivePayload) {
			t.Errorf("payload %s 应被拒绝，得到 %v", p, err)
		}
	}
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.tasks"); n != 0 {
		t.Fatalf("敏感 payload 不得落库，实际 %d 行", n)
	}
	// 引用 ID 形态放行，且落库的 payload 不含任何敏感明文。
	task := enqueue(t, enq, platformScope(), `{"credential_uid":"`+e.ids.MustNew()+`","site_id":5}`)
	if strings.Contains(string(task.Payload), "hunter2") {
		t.Fatal("不应出现明文")
	}
}

// 幂等键：同 (type, key) 只有一条任务（周期任务的确定性入队，ADR-019 §6）；并发入队也只生成一条；他租户占用的键不泄漏。
func TestEnqueueIdempotencyKey(t *testing.T) {
	e := newEnv(t)
	enq := e.enqueuer(t, e.registry(t, "o"), nil)
	key := func(k string) func(*taskrt.EnqueueRequest) {
		return func(r *taskrt.EnqueueRequest) { r.IdempotencyKey = k }
	}
	a := enqueue(t, enq, platformScope(), `{}`, key("rollup:2026-11-21T10"))
	b, created, err := enq.Enqueue(context.Background(), platformScope(), taskrt.EnqueueRequest{Type: taskrt.TypeRetentionCleanup, Payload: []byte(`{}`), IdempotencyKey: "rollup:2026-11-21T10"})
	if err != nil || created || b.UID != a.UID {
		t.Fatalf("同键应返回既有任务: created=%v uid=%s/%s err=%v", created, b.UID, a.UID, err)
	}
	var wg sync.WaitGroup
	uids := make([]string, 12)
	for i := range uids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tk, _, err := enq.Enqueue(context.Background(), platformScope(), taskrt.EnqueueRequest{Type: taskrt.TypeRetentionCleanup, Payload: []byte(`{}`), IdempotencyKey: "concurrent"})
			if err != nil {
				t.Error(err)
				return
			}
			uids[i] = tk.UID
		}()
	}
	wg.Wait()
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.tasks WHERE idempotency_key = 'concurrent'"); n != 1 {
		t.Fatalf("并发入队同键应只有 1 行，实际 %d", n)
	}
	for _, u := range uids {
		if u != uids[0] {
			t.Fatalf("并发入队应返回同一任务: %v", uids)
		}
	}
	tid, _ := e.tenant(t, "tenant-a")
	_, _, err = enq.Enqueue(context.Background(), tenantScope(t, tid, 0), taskrt.EnqueueRequest{Type: taskrt.TypeRetentionCleanup, Payload: []byte(`{}`), IdempotencyKey: "rollup:2026-11-21T10"})
	if !errors.Is(err, taskrt.ErrIdempotencyKeyTaken) {
		t.Fatalf("他租户/平台占用的键不得返回对方任务，得到 %v", err)
	}
}

func TestEnqueueScopeAndParent(t *testing.T) {
	e := newEnv(t)
	enq := e.enqueuer(t, e.registry(t, "o"), nil)
	ta, _ := e.tenant(t, "ta")
	tb, _ := e.tenant(t, "tb")
	uid1, uuid1 := e.user(t, "tenant", "a@example.com")
	// 租户 Scope：tenant_id 被强制为 Scope 的租户，请求里的 TenantID 被忽略；created_by 取 Scope 的操作者。
	other := tb
	tk, _, err := enq.Enqueue(context.Background(), tenantScope(t, ta, uid1), taskrt.EnqueueRequest{Type: taskrt.TypeRetentionCleanup, Payload: []byte(`{}`), TenantID: &other})
	if err != nil {
		t.Fatal(err)
	}
	if tk.TenantID == nil || *tk.TenantID != ta || tk.CreatedByUID == nil || *tk.CreatedByUID != uuid1 || tk.TenantUID == nil {
		t.Fatalf("租户 Scope 下入队结果不符: tenant=%v created_by=%v", tk.TenantID, tk.CreatedByUID)
	}
	// 平台 Scope：可指定租户，也可不带（平台任务）。
	pt, _, err := enq.Enqueue(context.Background(), platformScope(), taskrt.EnqueueRequest{Type: taskrt.TypeRetentionCleanup, Payload: []byte(`{}`), TenantID: &tb})
	if err != nil || pt.TenantID == nil || *pt.TenantID != tb {
		t.Fatalf("平台为租户入队: %+v %v", pt.TenantID, err)
	}
	plat := enqueue(t, enq, platformScope(), `{}`)
	if plat.TenantID != nil {
		t.Fatal("平台任务不应有租户")
	}
	// 子任务：父任务对调用方不可见（他租户）→ 拒绝；可见则成功。
	_, _, err = enq.Enqueue(context.Background(), tenantScope(t, tb, 0), taskrt.EnqueueRequest{Type: taskrt.TypeRetentionCleanup, Payload: []byte(`{}`), ParentTask: tk.ID})
	if !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("把子任务挂到他租户的任务下应被拒绝，得到 %v", err)
	}
	child, _, err := enq.Enqueue(context.Background(), tenantScope(t, ta, 0), taskrt.EnqueueRequest{Type: taskrt.TypeRetentionCleanup, Payload: []byte(`{}`), ParentTask: tk.ID})
	if err != nil || child.ParentTaskUID == nil || *child.ParentTaskUID != tk.UID {
		t.Fatalf("同租户子任务: %+v %v", child.ParentTaskUID, err)
	}
}

// 优先级（数值小者优先）、run_after 未到期不被领取。
func TestClaimOrderAndRunAfter(t *testing.T) {
	e := newEnv(t)
	enq := e.enqueuer(t, e.registry(t, "o"), nil)
	low := enqueue(t, enq, platformScope(), `{}`, func(r *taskrt.EnqueueRequest) { r.Priority = 200 })
	hi := enqueue(t, enq, platformScope(), `{}`, func(r *taskrt.EnqueueRequest) { r.Priority = 10 })
	mid := enqueue(t, enq, platformScope(), `{}`)
	future := enqueue(t, enq, platformScope(), `{}`, func(r *taskrt.EnqueueRequest) { r.RunAfter = time.Now().Add(time.Hour) })
	got, err := e.store.Claim(context.Background(), platformScope(), "w1", []taskrt.Type{taskrt.TypeRetentionCleanup}, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].UID != hi.UID || got[1].UID != mid.UID || got[2].UID != low.UID {
		var us []string
		for _, g := range got {
			us = append(us, g.UID)
		}
		t.Fatalf("领取顺序应为 priority 升序且排除未到期: %v (hi=%s mid=%s low=%s)", us, hi.UID, mid.UID, low.UID)
	}
	for _, g := range got {
		if g.Status != taskrt.StatusRunning || g.Fence != 1 || g.Attempts != 1 || g.LeaseOwner == nil || *g.LeaseOwner != "w1" || g.LeaseUntil == nil || g.StartedAt == nil {
			t.Fatalf("领取后字段不符: %+v", g)
		}
	}
	if e.status(t, future.UID) != "pending" {
		t.Fatal("run_after 未到期的任务不得被领取")
	}
	// 未登记类型不被领取：只领取 types 内的类型（这里用空匹配的类型）。
	enqueue(t, enq, platformScope(), `{}`)
	none, err := e.store.Claim(context.Background(), platformScope(), "w1", []taskrt.Type{taskrt.TypeCachePurgeURL}, 10, time.Minute)
	if err != nil || len(none) != 0 {
		t.Fatalf("只应领取指定类型: %d %v", len(none), err)
	}
	// 租户 Scope 不得调用 Worker 侧方法。
	tid, _ := e.tenant(t, "tx")
	if _, err := e.store.Claim(context.Background(), tenantScope(t, tid, 0), "w", []taskrt.Type{taskrt.TypeRetentionCleanup}, 1, time.Minute); !errors.Is(err, taskrt.ErrScope) {
		t.Fatalf("租户 Scope 不得领取: %v", err)
	}
}

// AC3：N 个 Worker 竞争一批任务——每个任务恰好被领取/处理一次（先在存储层，再在完整 Worker 层）。
func TestClaimExactlyOnceStoreLevel(t *testing.T) {
	e := newEnv(t)
	enq := e.enqueuer(t, e.registry(t, "o"), nil)
	const total, workers = 120, 12
	for i := 0; i < total; i++ {
		enqueue(t, enq, platformScope(), `{}`)
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				got, err := e.store.Claim(context.Background(), platformScope(), "w"+string(rune('a'+w)), []taskrt.Type{taskrt.TypeRetentionCleanup}, 5, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if len(got) == 0 {
					return
				}
				mu.Lock()
				for _, g := range got {
					seen[g.UID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != total {
		t.Fatalf("应恰好领取 %d 个任务，实际 %d", total, len(seen))
	}
	for u, n := range seen {
		if n != 1 {
			t.Fatalf("任务 %s 被领取 %d 次", u, n)
		}
	}
	if bad := e.rootInt(t, "SELECT COUNT(*) FROM §.tasks WHERE NOT (status='running' AND fence=1 AND attempts=1)"); bad != 0 {
		t.Fatalf("%d 个任务的 fence/attempts 不是 1/1", bad)
	}
}

func TestWorkersProcessEachTaskExactlyOnce(t *testing.T) {
	e := newEnv(t)
	enq := e.enqueuer(t, e.registry(t, "o"), nil)
	const total, workers = 150, 8
	uids := make([]string, total)
	for i := range uids {
		uids[i] = enqueue(t, enq, platformScope(), `{"sleep_ms":5}`).UID
	}
	for w := 0; w < workers; w++ {
		owner := "w" + string(rune('a'+w))
		e.startWorker(t, e.registry(t, owner), workerOpts{owner: owner, concurrency: 4, lease: 5 * time.Second})
	}
	eventually(t, "全部任务完成", 90*time.Second, func() bool {
		return e.rootInt(t, "SELECT COUNT(*) FROM §.tasks WHERE status = 'success'") == total
	})
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.exec_log"); n != total {
		t.Fatalf("exec_log 应恰好 %d 条（每任务一次），实际 %d", total, n)
	}
	if n := e.rootInt(t, "SELECT COUNT(DISTINCT task_uid) FROM §.exec_log"); n != total {
		t.Fatalf("存在被重复处理的任务: distinct=%d", n)
	}
	if bad := e.rootInt(t, "SELECT COUNT(*) FROM §.tasks WHERE NOT (attempts = 1 AND fence = 1 AND progress = 100 AND lease_owner IS NULL AND finished_at IS NOT NULL AND error_code IS NULL)"); bad != 0 {
		t.Fatalf("%d 个任务终态字段不符", bad)
	}
	if n := e.rootInt(t, "SELECT COUNT(DISTINCT owner) FROM §.exec_log"); n < 2 {
		t.Fatalf("应有多个 Worker 参与处理，实际 %d", n)
	}
}

// 执行期每 lease/3 续约：处理函数运行时间远超租约，任务不被回收（有另一个 Worker 在不停回收），fence/attempts 保持 1。
func TestLeaseRenewalKeepsTaskOwned(t *testing.T) {
	e := newEnv(t)
	enq := e.enqueuer(t, e.registry(t, "o"), nil)
	task := enqueue(t, enq, platformScope(), `{"sleep_ms":2500}`)
	e.startWorker(t, e.registry(t, "A"), workerOpts{owner: "A", lease: 600 * time.Millisecond})
	eventually(t, "A 开始执行", 10*time.Second, func() bool { return e.execCount(t, task.UID) == 1 })
	// 旁观者 B 同时运行回收（同类型，但任务一直被 A 续约，不应被回收或抢走）。
	e.startWorker(t, e.registry(t, "B"), workerOpts{owner: "B", lease: 600 * time.Millisecond})
	eventually(t, "任务完成", 20*time.Second, func() bool { return e.status(t, task.UID) == "success" })
	if n := e.execCount(t, task.UID); n != 1 {
		t.Fatalf("续约生效时任务只应执行一次，实际 %d", n)
	}
	if v := e.rootInt(t, "SELECT fence FROM §.tasks WHERE uid = ?", task.UID); v != 1 {
		t.Fatalf("fence 应保持 1，实际 %d", v)
	}
	if v := e.rootInt(t, "SELECT COUNT(*) FROM §.task_events WHERE task_id = (SELECT id FROM §.tasks WHERE uid = ?) AND message LIKE 'lease expired%'", task.UID); v != 0 {
		t.Fatalf("不应出现租约过期事件: %d", v)
	}
}

// 失败语义：可重试错误退避重试直至成功；超限 failed 且含原因；永久失败不重试；panic 被捕获按失败处理；错误信息写入前脱敏；partial 终态。
func TestFailureSemantics(t *testing.T) {
	e := newEnv(t)
	enq := e.enqueuer(t, e.registry(t, "o"), nil)
	maxAtt := func(n uint32) func(*taskrt.EnqueueRequest) {
		return func(r *taskrt.EnqueueRequest) { r.MaxAttempts = n }
	}
	retry := enqueue(t, enq, platformScope(), `{"fail_until_attempt":2}`)
	exhaust := enqueue(t, enq, platformScope(), `{"fail_until_attempt":99}`, maxAtt(2))
	perm := enqueue(t, enq, platformScope(), `{"permanent":true}`)
	pan := enqueue(t, enq, platformScope(), `{"panic":true}`, maxAtt(1))
	leak := enqueue(t, enq, platformScope(), `{"leak":true}`, maxAtt(1))
	part := enqueue(t, enq, platformScope(), `{"partial":true}`)
	e.startWorker(t, e.registry(t, "W"), workerOpts{owner: "W", lease: 5 * time.Second})
	eventually(t, "全部进入终态", 40*time.Second, func() bool {
		return e.rootInt(t, "SELECT COUNT(*) FROM §.tasks WHERE status IN ('pending','running')") == 0
	})
	get := func(u string) taskrt.Task {
		tk, err := e.store.Get(context.Background(), platformScope(), u)
		if err != nil {
			t.Fatal(err)
		}
		return tk
	}
	if tk := get(retry.UID); tk.Status != taskrt.StatusSuccess || tk.Attempts != 3 || tk.Fence != 3 || tk.ErrorCode != nil {
		t.Errorf("重试至成功: %+v", tk)
	}
	if tk := get(exhaust.UID); tk.Status != taskrt.StatusFailed || tk.Attempts != 2 || tk.ErrorCode == nil || *tk.ErrorCode != "task.failed" || tk.ErrorMessage == nil || !strings.Contains(*tk.ErrorMessage, "transient") {
		t.Errorf("超限失败应含原因: %+v", tk)
	}
	if tk := get(perm.UID); tk.Status != taskrt.StatusFailed || tk.Attempts != 1 || tk.ErrorCode == nil || *tk.ErrorCode != "test.permanent" {
		t.Errorf("永久失败不重试: %+v", tk)
	}
	if tk := get(pan.UID); tk.Status != taskrt.StatusFailed || tk.ErrorMessage == nil || !strings.Contains(*tk.ErrorMessage, "panic") {
		t.Errorf("panic 按失败处理: %+v", tk)
	}
	if tk := get(leak.UID); tk.Status != taskrt.StatusFailed || tk.ErrorMessage == nil || strings.Contains(*tk.ErrorMessage, "hunter2") || strings.Contains(*tk.ErrorMessage, "abcdefghijklmnop") {
		t.Errorf("错误信息必须脱敏: %+v", tk.ErrorMessage)
	}
	if tk := get(part.UID); tk.Status != taskrt.StatusPartial || tk.Progress != 100 && tk.Progress != 0 {
		t.Errorf("partial 终态: %+v", tk)
	}
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.task_events WHERE task_id = (SELECT id FROM §.tasks WHERE uid = ?) AND level = 'warn'", retry.UID); n != 2 {
		t.Errorf("重试任务应有 2 条 warn 事件，实际 %d", n)
	}
	if n := e.rootInt(t, "SELECT COUNT(*) FROM §.task_events WHERE message LIKE '%hunter2%' OR message LIKE '%abcdefghijklmnop%' OR IFNULL(CAST(data_json AS CHAR),'') LIKE '%hunter2%'"); n != 0 {
		t.Errorf("事件中不得出现敏感明文: %d", n)
	}
}

// 重试退避：失败后 run_after 被推迟（至少 Base），之前不会被再次领取。
func TestRetryBackoffDelaysReclaim(t *testing.T) {
	e := newEnv(t)
	enq := e.enqueuer(t, e.registry(t, "o"), nil)
	task := enqueue(t, enq, platformScope(), `{}`)
	got, _ := e.store.Claim(context.Background(), platformScope(), "w", []taskrt.Type{taskrt.TypeRetentionCleanup}, 1, time.Minute)
	if len(got) != 1 {
		t.Fatal("应领取到任务")
	}
	if err := e.store.Retry(context.Background(), platformScope(), got[0].ID, got[0].Fence, "w", "task.failed", "boom", taskrt.BackoffConfig{Base: 800 * time.Millisecond, Max: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if again, _ := e.store.Claim(context.Background(), platformScope(), "w", []taskrt.Type{taskrt.TypeRetentionCleanup}, 1, time.Minute); len(again) != 0 {
		t.Fatal("退避期内不应再被领取")
	}
	time.Sleep(900 * time.Millisecond)
	again, err := e.store.Claim(context.Background(), platformScope(), "w", []taskrt.Type{taskrt.TypeRetentionCleanup}, 1, time.Minute)
	if err != nil || len(again) != 1 || again[0].UID != task.UID || again[0].Attempts != 2 || again[0].Fence != 2 {
		t.Fatalf("退避后应可再次领取（attempts=2,fence=2）: %+v %v", again, err)
	}
}

// 协作式取消：pending 立即 canceled；running 置 cancel_requested，处理函数在检查点终止为 canceled；终态 409；他租户 404。
func TestCancel(t *testing.T) {
	e := newEnv(t)
	enq := e.enqueuer(t, e.registry(t, "o"), nil)
	ta, _ := e.tenant(t, "ta")
	tb, _ := e.tenant(t, "tb")
	scA, scB := tenantScope(t, ta, 0), tenantScope(t, tb, 0)

	pending := enqueue(t, enq, scA, `{}`, func(r *taskrt.EnqueueRequest) { r.RunAfter = time.Now().Add(time.Hour) })
	if _, err := e.store.RequestCancel(context.Background(), scB, pending.UID); !errors.Is(err, db.ErrNotFound) {
		t.Fatalf("他租户取消应表现为 404（ErrNotFound），得到 %v", err)
	}
	if e.status(t, pending.UID) != "pending" {
		t.Fatal("越权取消不得改变任务")
	}
	got, err := e.store.RequestCancel(context.Background(), scA, pending.UID)
	if err != nil || got.Status != taskrt.StatusCanceled || got.FinishedAt == nil || !got.CancelRequested {
		t.Fatalf("pending 取消应直接 canceled: %+v %v", got, err)
	}
	if _, err := e.store.RequestCancel(context.Background(), scA, pending.UID); !errors.Is(err, taskrt.ErrNotCancelable) {
		t.Fatalf("终态不可取消（409）: %v", err)
	}

	running := enqueue(t, enq, scA, `{"loop":true}`)
	e.startWorker(t, e.registry(t, "W"), workerOpts{owner: "W", lease: 600 * time.Millisecond})
	eventually(t, "任务运行中", 10*time.Second, func() bool { return e.status(t, running.UID) == "running" && e.execCount(t, running.UID) == 1 })
	got, err = e.store.RequestCancel(context.Background(), platformScope(), running.UID)
	if err != nil || got.Status != taskrt.StatusRunning || !got.CancelRequested {
		t.Fatalf("running 取消应只置 cancel_requested: %+v %v", got, err)
	}
	eventually(t, "协作式取消生效", 10*time.Second, func() bool { return e.status(t, running.UID) == "canceled" })
	fin, _ := e.store.Get(context.Background(), platformScope(), running.UID)
	if fin.FinishedAt == nil || fin.LeaseOwner != nil {
		t.Fatalf("取消后应释放租约并记录结束时间: %+v", fin)
	}
}

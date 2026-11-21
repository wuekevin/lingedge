package downloader_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

// 延迟 Commit（1.28b）：调用方在安装步骤成功之后再提交信任状态；失败则不提交并可回滚（清理 staging）。

func deferParams(w *world, s scenario) downloader.Params {
	p := w.params()
	p.BundlePath = w.writeBundle(s)
	p.DeferCommit = true
	p.TxID = "tx-d"
	return p
}

func rotationEdit(t *testing.T) func(map[string]any) {
	newKey := deriveKey("rel-dl-b", "b")
	return func(m map[string]any) {
		nk := manifest.Key{KeyID: newKey.id, Alg: "ed25519", PublicKey: b64(newKey.pub), Status: manifest.StatusActive, NotBefore: "2026-01-01T00:00:00Z", NotAfter: "2028-01-01T00:00:00Z"}
		m["key_rotation"] = map[string]any{
			"keys":         []any{map[string]any{"key_id": nk.KeyID, "alg": nk.Alg, "public_key": nk.PublicKey, "status": nk.Status, "not_before": nk.NotBefore, "not_after": nk.NotAfter}},
			"endorsements": []any{map[string]any{"key_id": nk.KeyID, "endorsed_by": "rel-dl-a", "sig": b64(signRotation(t, deriveKey("rel-dl-a", "a"), nk))}},
		}
	}
}

func TestDeferCommitNothingPersistedUntilCommit(t *testing.T) {
	w := newWorld(t)
	s := scenario{pkg: goodPkg(t, w, nil), seq: 21, mfEdit: rotationEdit(t)}
	trustBefore := tree(t, w.trustDir)
	a, err := downloader.Acquire(t.Context(), deferParams(w, s))
	if err != nil {
		t.Fatal(err)
	}
	// 整条链已验证、已解压；但 sequence 与轮换公钥都还没有落盘。
	if a.Committed() {
		t.Fatal("DeferCommit 下不应已提交")
	}
	equalTree(t, "trust", trustBefore, tree(t, w.trustDir))
	if readState(w) != "" {
		t.Fatalf("sequence 不应推进：%q", readState(w))
	}
	if _, err := os.Stat(filepath.Join(w.keysDir, "rel-dl-b.json")); err == nil {
		t.Fatal("轮换公钥不应落盘")
	}
	if _, err := os.Stat(filepath.Join(a.PkgDir, "payload", "README")); err != nil {
		t.Fatalf("解压结果应在 staging 中可用：%v", err)
	}
	// 提交：sequence 推进、公钥 0600 落盘；幂等。
	if err := a.Commit(); err != nil {
		t.Fatal(err)
	}
	if !a.Committed() || readState(w) == "" {
		t.Fatalf("提交后应推进 sequence：%q", readState(w))
	}
	fi, err := os.Stat(filepath.Join(w.keysDir, "rel-dl-b.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("轮换公钥未按 0600 落盘：%v %v", fi, err)
	}
	st := readState(w)
	if err := a.Commit(); err != nil || readState(w) != st {
		t.Fatalf("Commit 应幂等：%v", err)
	}
	// 回滚保护：sequence 已推进到 21，更小的清单被拒绝（与立即提交时同一个防回滚语义）。
	w2 := s
	w2.seq = 20
	p := deferParams(w, w2)
	p.TxID = "tx-e"
	if _, err := downloader.Acquire(t.Context(), p); err == nil {
		t.Fatal("提交后的回滚清单必须被拒绝")
	}
}

func TestDeferCommitDiscardCleansStagingAndNeverCommits(t *testing.T) {
	w := newWorld(t)
	s := scenario{pkg: goodPkg(t, w, nil), seq: 33}
	stagingBefore, trustBefore := tree(t, w.staging), tree(t, w.trustDir)
	a, err := downloader.Acquire(t.Context(), deferParams(w, s))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := a.Discard(); err != nil {
		t.Fatalf("Discard 应幂等：%v", err)
	}
	equalTree(t, "staging", stagingBefore, tree(t, w.staging))
	equalTree(t, "trust", trustBefore, tree(t, w.trustDir))
	err = a.Commit()
	wantKind(t, err, downloader.KindOptions)
	if readState(w) != "" || a.Committed() {
		t.Fatal("Discard 之后不得再提交")
	}
	// 安装失败后可以用同一份清单重来（sequence 没有被推进）。
	a2, err := downloader.Acquire(t.Context(), deferParams(w, s))
	if err != nil {
		t.Fatalf("放弃后同一份清单应可再次获取：%v", err)
	}
	if err := a2.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestDeferCommitFinish(t *testing.T) {
	boom := errors.New("install failed")
	t.Run("安装成功才提交", func(t *testing.T) {
		w := newWorld(t)
		a, err := downloader.Acquire(t.Context(), deferParams(w, scenario{pkg: goodPkg(t, w, nil), seq: 40}))
		if err != nil {
			t.Fatal(err)
		}
		var sawUncommitted bool
		if err := a.Finish(func(x *downloader.Acquired) error {
			sawUncommitted = !x.Committed() && readState(w) == ""
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if !sawUncommitted || !a.Committed() || readState(w) == "" {
			t.Fatalf("应在安装之后才提交：saw=%v committed=%v", sawUncommitted, a.Committed())
		}
		if _, err := os.Stat(a.PkgDir); err != nil {
			t.Fatal("成功后 staging 保留给调用方")
		}
	})
	t.Run("安装失败不提交并清理", func(t *testing.T) {
		w := newWorld(t)
		before := tree(t, w.staging)
		trustBefore := tree(t, w.trustDir)
		a, err := downloader.Acquire(t.Context(), deferParams(w, scenario{pkg: goodPkg(t, w, nil), seq: 41}))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Finish(func(*downloader.Acquired) error { return boom }); !errors.Is(err, boom) {
			t.Fatalf("应原样返回安装错误：%v", err)
		}
		equalTree(t, "staging", before, tree(t, w.staging))
		equalTree(t, "trust", trustBefore, tree(t, w.trustDir))
	})
	t.Run("安装 panic 也不提交并清理", func(t *testing.T) {
		w := newWorld(t)
		before := tree(t, w.staging)
		a, err := downloader.Acquire(t.Context(), deferParams(w, scenario{pkg: goodPkg(t, w, nil), seq: 42}))
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() {
				if recover() == nil {
					t.Error("panic 应继续向上传播")
				}
			}()
			_ = a.Finish(func(*downloader.Acquired) error { panic("x") })
		}()
		equalTree(t, "staging", before, tree(t, w.staging))
		if readState(w) != "" || a.Committed() {
			t.Fatal("panic 后不得提交")
		}
	})
}

// 默认（不延迟）行为与 1.28 相同：链成功即提交；Commit()/Finish 仍可安全调用。
func TestDefaultAcquireStillCommitsImmediately(t *testing.T) {
	w := newWorld(t)
	p := w.params()
	p.BundlePath = w.writeBundle(scenario{pkg: goodPkg(t, w, nil), seq: 50})
	a, err := downloader.Acquire(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Committed() || readState(w) == "" {
		t.Fatal("默认应立即提交")
	}
	if err := a.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestDeferCommitConcurrentCommitIsSafe(t *testing.T) {
	w := newWorld(t)
	a, err := downloader.Acquire(t.Context(), deferParams(w, scenario{pkg: goodPkg(t, w, nil), seq: 60}))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.Commit(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if !a.Committed() {
		t.Fatal("应已提交")
	}
}

// 把真实的 FileSequenceStore 包一层只计数的装饰器（行为完全委托给真实实现）：并发/重复 Commit 只推进一次。
type countingSeq struct {
	manifest.SequenceStore
	mu       sync.Mutex
	advances int
}

func (c *countingSeq) Advance(ch string, seq int64) error {
	c.mu.Lock()
	c.advances++
	c.mu.Unlock()
	return c.SequenceStore.Advance(ch, seq)
}

func TestDeferCommitAdvancesExactlyOnce(t *testing.T) {
	w := newWorld(t)
	p := deferParams(w, scenario{pkg: goodPkg(t, w, nil), seq: 70})
	cs := &countingSeq{SequenceStore: p.Verifier.Seen}
	p.Verifier.Seen = cs
	a, err := downloader.Acquire(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if cs.advances != 0 {
		t.Fatalf("提交前不应推进：%d", cs.advances)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = a.Commit() }()
	}
	wg.Wait()
	_ = a.Commit()
	if cs.advances != 1 {
		t.Fatalf("Advance 应恰好一次：%d", cs.advances)
	}
}

package secretbox_test

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// spyProvider 包装真实的 FileKeyProvider，记录交给/取回的 DEK 切片，用于断言使用后已清零。
type spyProvider struct {
	secretbox.KeyProvider
	mu      sync.Mutex
	wrapped [][]byte // Wrap 收到的 dek（调用方持有）
	unwrap  [][]byte // Unwrap 返回的 dek
	nonZero []bool   // Wrap 收到时的内容快照（确认不是一直全零）
}

func (s *spyProvider) Wrap(ctx context.Context, id string, dek, aad []byte) ([]byte, error) {
	s.mu.Lock()
	s.wrapped = append(s.wrapped, dek)
	s.nonZero = append(s.nonZero, !bytes.Equal(dek, make([]byte, len(dek))))
	s.mu.Unlock()
	return s.KeyProvider.Wrap(ctx, id, dek, aad)
}

func (s *spyProvider) Unwrap(ctx context.Context, id string, w, aad []byte) ([]byte, error) {
	d, err := s.KeyProvider.Unwrap(ctx, id, w, aad)
	s.mu.Lock()
	s.unwrap = append(s.unwrap, d)
	s.mu.Unlock()
	return d, err
}

func allZeroBytes(b []byte) bool { return len(b) > 0 && bytes.Equal(b, make([]byte, len(b))) }

// AC4：DEK 使用后清零（Seal / Open / Rewrap 三条路径）。
func TestDEKZeroedAfterUse(t *testing.T) {
	ctx := context.Background()
	spy := &spyProvider{KeyProvider: newProvider(t, keyFile(map[string]string{"k1": newKey(t), "k2": newKey(t)}, "k1"))}
	box := newBox(t, spy)
	enc, err := box.Seal(ctx, []byte("p"), rc)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := box.Open(ctx, enc, rc)
	if err != nil || string(pt) != "p" {
		t.Fatal(err)
	}
	if _, _, err := box.Rewrap(ctx, enc, rc, "k2"); err != nil {
		t.Fatal(err)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.wrapped) != 2 || len(spy.unwrap) != 3 { // Seal:1 wrap；Open:1 unwrap；Rewrap:1 unwrap+1 wrap+1 回读 unwrap（共 wrap2 / unwrap3）
		t.Fatalf("调用次数 wrap=%d unwrap=%d", len(spy.wrapped), len(spy.unwrap))
	}
	for i, ok := range spy.nonZero {
		if !ok {
			t.Errorf("wrap[%d] 收到全零 DEK（测试无意义）", i)
		}
	}
	for i, d := range spy.wrapped {
		if !allZeroBytes(d) {
			t.Errorf("wrap[%d] 的 DEK 使用后未清零", i)
		}
	}
	for i, d := range spy.unwrap {
		if !allZeroBytes(d) {
			t.Errorf("unwrap[%d] 返回的 DEK 使用后未清零", i)
		}
	}
}

func TestZeroHelper(t *testing.T) {
	b := []byte{1, 2, 3}
	secretbox.Zero(b)
	if !allZeroBytes(b) {
		t.Fatal("Zero 未清零")
	}
	secretbox.Zero(nil)
}

// Close 之后所有操作 ErrClosed，且并发使用 + Close 在 -race 下无数据竞争。
func TestCloseAndConcurrency(t *testing.T) {
	ctx := context.Background()
	kp, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: writeKeyFile(t, keyFile(map[string]string{"k1": newKey(t), "k2": newKey(t)}, "k1"), 0o400)})
	if err != nil {
		t.Fatal(err)
	}
	box := newBox(t, kp)
	enc, _ := box.Seal(ctx, []byte("x"), rc)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				e, err := box.Seal(ctx, []byte("y"), rc)
				if err == nil {
					if pt, err := box.Open(ctx, e, rc); err == nil && string(pt) != "y" {
						t.Error("并发解密结果错误")
					}
					_, _, _ = box.Rewrap(ctx, e, rc, "k2")
				}
				_ = kp.CurrentKeyID()
				_ = kp.KeyIDs()
			}
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); kp.Close() }()
	wg.Wait()
	kp.Close() // 幂等
	if _, err := box.Open(ctx, enc, rc); err == nil || err.Error() != secretbox.ErrClosed.Error() {
		t.Fatalf("Close 后应 ErrClosed: %v", err)
	}
	if _, err := box.Seal(ctx, []byte("x"), rc); err == nil {
		t.Fatal("Close 后 Seal 应失败")
	}
}

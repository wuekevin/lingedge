package downloader_test

import (
	"errors"
	"os"
	"testing"

	"github.com/wuekevin/lingedge/internal/tools/downloader"
)

// Params.OnStage（Story 1.33）：四个阶段按序在各自动作开始前被调用（步骤 10–13）；回调返回错误即中止 Acquire，staging/<tx> 被清理且信任库不变。
func TestOnStageOrderAndAbort(t *testing.T) {
	w := newWorld(t)
	pkg := pkgSpec{signer: w.key}.build(t)
	bundle := w.writeBundle(scenario{pkg: pkg})
	var got []downloader.Stage
	p := w.params()
	p.BundlePath = bundle
	p.OnStage = func(s downloader.Stage) error { got = append(got, s); return nil }
	if _, err := downloader.Acquire(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	want := []downloader.Stage{downloader.StageManifest, downloader.StageVerify, downloader.StagePackage, downloader.StageCheck}
	if len(got) != 4 {
		t.Fatalf("阶段: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("阶段顺序: %v", got)
		}
	}
	// 在每个阶段中止：错误原样返回，staging 不留残渣，sequence 不推进。
	for _, abortAt := range want {
		w2 := newWorld(t)
		b2 := w2.writeBundle(scenario{pkg: pkgSpec{signer: w2.key}.build(t)})
		boom := errors.New("abort at " + string(abortAt))
		p2 := w2.params()
		p2.BundlePath = b2
		p2.TxID = "tx-abort"
		p2.OnStage = func(s downloader.Stage) error {
			if s == abortAt {
				return boom
			}
			return nil
		}
		if _, err := downloader.Acquire(t.Context(), p2); !errors.Is(err, boom) {
			t.Fatalf("%s: 应原样返回回调的错误: %v", abortAt, err)
		}
		assertEmptyDir(t, w2.staging)
		if _, ok, _ := w2.verifier().Seen.Max("stable"); ok {
			t.Errorf("%s: 中止后不得推进 sequence", abortAt)
		}
	}
}

func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) != 0 {
		t.Fatalf("%s 应为空: %v %v", dir, ents, err)
	}
}

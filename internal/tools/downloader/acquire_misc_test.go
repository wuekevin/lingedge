package downloader_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/cli"
	"github.com/wuekevin/lingedge/internal/tools/downloader"
)

func TestAcquireParamValidation(t *testing.T) {
	w := newWorld(t)
	s := scenario{pkg: goodPkg(t, w, nil)}
	bundle := w.writeBundle(s)
	base := func() downloader.Params { p := w.params(); p.BundlePath = bundle; return p }
	online := &downloader.OnlineSource{Client: w.client(downloader.Config{}), ManifestURL: w.base + "/manifest.json"}
	cases := map[string]func(*downloader.Params){
		"没有来源":          func(p *downloader.Params) { p.BundlePath = "" },
		"两个来源":          func(p *downloader.Params) { p.Online = online },
		"缺 Verifier":    func(p *downloader.Params) { p.Verifier = nil },
		"缺 StagingRoot": func(p *downloader.Params) { p.StagingRoot = "" },
		"缺 KeysDir":     func(p *downloader.Params) { p.KeysDir = "" },
		"缺 Component":   func(p *downloader.Params) { p.Component = "" },
		"在线缺 Client": func(p *downloader.Params) {
			p.BundlePath = ""
			p.Online = &downloader.OnlineSource{ManifestURL: "https://x/y"}
		},
		"TxID 含路径": func(p *downloader.Params) { p.TxID = "../x" },
		"TxID 空格":  func(p *downloader.Params) { p.TxID = "a b" },
	}
	for name, mut := range cases {
		p := base()
		mut(&p)
		before := tree(t, w.staging)
		_, err := downloader.Acquire(t.Context(), p)
		wantKind(t, err, downloader.KindOptions)
		if exitOf(err) != cli.ExitUsage {
			t.Errorf("%s: 退出码 %d", name, exitOf(err))
		}
		equalTree(t, name+" staging", before, tree(t, w.staging))
	}
}

func TestAcquireTxDirAlreadyExists(t *testing.T) {
	w := newWorld(t)
	if err := os.Mkdir(filepath.Join(w.staging, "tx-dup"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(w.staging, "tx-dup", "keep")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := w.params()
	p.BundlePath = w.writeBundle(scenario{pkg: goodPkg(t, w, nil)})
	p.TxID = "tx-dup"
	_, err := downloader.Acquire(t.Context(), p)
	wantKind(t, err, downloader.KindStaging)
	if _, e := os.Stat(marker); e != nil {
		t.Fatal("已存在的 tx 目录不得被清理（它不属于本次调用）")
	}
}

// 下载中途被取消（Ctrl-C）：返回 context.Canceled，staging/<tx> 被清理，信任库不变。
func TestAcquireCancelledMidDownloadCleansUp(t *testing.T) {
	w := newWorld(t)
	good := goodPkg(t, w, nil)
	s := scenario{pkg: good}
	mf, sig := s.build(w)
	w.serveBytes("/manifest.json", mf)
	w.serveBytes("/manifest.json.sig", sig)
	started := make(chan struct{})
	w.route("/dl/"+pkgFile, func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(200)
		_, _ = rw.Write(good[:10])
		rw.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(t.Context())
	go func() { <-started; time.Sleep(30 * time.Millisecond); cancel() }()
	p := w.params()
	p.Online = &downloader.OnlineSource{Client: w.client(downloader.Config{}), ManifestURL: w.base + "/manifest.json"}
	stagingBefore, trustBefore := tree(t, w.staging), tree(t, w.trustDir)
	_, err := downloader.Acquire(ctx, p)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("期望 context.Canceled：%v", err)
	}
	equalTree(t, "staging", stagingBefore, tree(t, w.staging))
	equalTree(t, "trust", trustBefore, tree(t, w.trustDir))
}

// 在线路径：清单/签名取不到（404、被截断、过大）同样清理并按类别映射。
func TestOnlineManifestFetchFailures(t *testing.T) {
	w := newWorld(t)
	good := goodPkg(t, w, nil)
	s := scenario{pkg: good}
	mf, sig := s.build(w)
	stagingBefore := tree(t, w.staging)
	run := func() error {
		p := w.params()
		p.Online = &downloader.OnlineSource{Client: w.client(downloader.Config{MaxAttempts: 2}), ManifestURL: w.base + "/manifest.json"}
		_, err := downloader.Acquire(t.Context(), p)
		return err
	}
	// 404
	wantKind(t, run(), downloader.KindNetwork)
	// 清单过大（> 8 MiB）
	w.route("/manifest.json", func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write(make([]byte, 9<<20)) })
	w.serveBytes("/manifest.json.sig", sig)
	err := run()
	wantKind(t, err, downloader.KindSize)
	if exitOf(err) != 5 {
		t.Fatalf("exit=%d", exitOf(err))
	}
	// 签名文件过大（> 4 KiB）
	w.serveBytes("/manifest.json", mf)
	w.route("/manifest.json.sig", func(rw http.ResponseWriter, r *http.Request) { _, _ = rw.Write(make([]byte, 5000)) })
	wantKind(t, run(), downloader.KindSize)
	equalTree(t, "staging", stagingBefore, tree(t, w.staging))
}

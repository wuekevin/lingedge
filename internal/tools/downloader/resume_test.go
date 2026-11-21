package downloader_test

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/downloader"
)

// 跨调用断点续传（1.28b）：partial 放在 staging 之外的受限缓存目录；缓存不被信任，整体 SHA256 通过才合并。

func resumeDir(t testing.TB) string { return filepath.Join(t.TempDir(), "resume") }

func partPath(rd, raw string, exp downloader.Expected) string {
	return filepath.Join(rd, downloader.ResumeKey(raw, exp)+".part")
}

func cacheEntries(t testing.TB, rd string) []string {
	t.Helper()
	es, err := os.ReadDir(rd)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range es {
		if strings.HasSuffix(e.Name(), ".part") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestResumeAcrossCalls(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 120_000)
	var n atomic.Int32
	w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			hijackCut(rw, b, 50_000)
			return
		}
		http.ServeContent(rw, r, "blob", time.Time{}, bytes.NewReader(b))
	})
	rd, raw := resumeDir(t), w.base+"/blob"
	d := dst(t)
	// 第一次调用：连接被掐断且不重试 -> 失败，但 partial 留在缓存里（0700 目录、0600 文件）。
	err := w.client(downloader.Config{ResumeDir: rd, MaxAttempts: 1}).Download(t.Context(), raw, d, exp)
	wantKind(t, err, downloader.KindNetwork)
	assertNoLeftovers(t, d)
	fi, ferr := os.Stat(partPath(rd, raw, exp))
	if ferr != nil || fi.Size() != 50_000 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("缓存里应有 50000 字节、0600 的 partial：%v %v", fi, ferr)
	}
	if di, _ := os.Stat(rd); di.Mode().Perm() != 0o700 {
		t.Fatalf("缓存目录权限 = %v", di.Mode().Perm())
	}
	// 第二次调用（全新的客户端）：从 50000 续传，成功，缓存被消耗。
	if err := w.client(downloader.Config{ResumeDir: rd}).Download(t.Context(), raw, d, exp); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readFile(t, d), b) {
		t.Fatal("续传后的内容不符")
	}
	if hs := w.rangeHeaders("/blob"); len(hs) != 2 || hs[1] != "bytes=50000-" {
		t.Fatalf("第二次调用应以 Range 续传：%q", hs)
	}
	if e := cacheEntries(t, rd); len(e) != 0 {
		t.Fatalf("成功后缓存里不应残留 partial：%v", e)
	}
}

// 端到端：Acquire 失败 -> staging 与信任库保持不变（AC4）、缓存留存；再次 Acquire 续传并成功。
func TestAcquireResumesAcrossCallsKeepingAC4(t *testing.T) {
	w := newWorld(t)
	good := goodPkg(t, w, nil)
	w.publish(scenario{pkg: good})
	var n atomic.Int32
	w.route("/dl/"+pkgFile, func(rw http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			hijackCut(rw, good, len(good)/2)
			return
		}
		http.ServeContent(rw, r, "p", time.Time{}, bytes.NewReader(good))
	})
	rd := resumeDir(t)
	p := w.params()
	p.Online = &downloader.OnlineSource{Client: w.client(downloader.Config{ResumeDir: rd, MaxAttempts: 1}), ManifestURL: w.base + "/manifest.json"}
	stagingBefore, trustBefore := tree(t, w.staging), tree(t, w.trustDir)
	if _, err := downloader.Acquire(t.Context(), p); err == nil {
		t.Fatal("第一次应失败")
	}
	equalTree(t, "staging", stagingBefore, tree(t, w.staging)) // AC4：staging 被清理
	equalTree(t, "trust", trustBefore, tree(t, w.trustDir))
	if len(cacheEntries(t, rd)) != 1 {
		t.Fatalf("缓存里应保留 1 个 partial：%v", cacheEntries(t, rd))
	}
	p.Online = &downloader.OnlineSource{Client: w.client(downloader.Config{ResumeDir: rd}), ManifestURL: w.base + "/manifest.json"}
	a, err := downloader.Acquire(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if string(readFile(t, filepath.Join(a.PkgDir, "payload", "README"))) != "hello\n" {
		t.Fatal("解压内容不符")
	}
	if hs := w.rangeHeaders("/dl/" + pkgFile); len(hs) != 2 || !strings.HasPrefix(hs[1], "bytes=") {
		t.Fatalf("第二次 Acquire 应续传：%q", hs)
	}
	if len(cacheEntries(t, rd)) != 0 {
		t.Fatal("成功后缓存应被消耗")
	}
}

// 缓存里的前半段被篡改（磁盘/他人写入），服务器诚实：整体 SHA256 不符 -> 失败、缓存 partial 被删除、不被复用；下一次干净重来成功。
func TestResumeTamperedCacheFirstHalfRejectedAndCacheCleared(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 120_000)
	w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
		http.ServeContent(rw, r, "blob", time.Time{}, bytes.NewReader(b))
	})
	rd, raw := resumeDir(t), w.base+"/blob"
	if err := os.MkdirAll(rd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partPath(rd, raw, exp), flip(b, 10)[:50_000], 0o600); err != nil {
		t.Fatal(err)
	}
	d := dst(t)
	err := w.client(downloader.Config{ResumeDir: rd}).Download(t.Context(), raw, d, exp)
	wantKind(t, err, downloader.KindSHA256)
	if exitOf(err) != 5 {
		t.Fatalf("exit=%d", exitOf(err))
	}
	assertNoLeftovers(t, d)
	if e := cacheEntries(t, rd); len(e) != 0 {
		t.Fatalf("校验失败必须删除缓存 partial，而不是复用：%v", e)
	}
	if hs := w.rangeHeaders("/blob"); len(hs) != 1 || hs[0] != "bytes=50000-" {
		t.Fatalf("第一次应尝试续传：%q", hs)
	}
	if err := w.client(downloader.Config{ResumeDir: rd}).Download(t.Context(), raw, d, exp); err != nil {
		t.Fatalf("清除后重新开始应成功：%v", err)
	}
	if hs := w.rangeHeaders("/blob"); hs[len(hs)-1] != "" {
		t.Fatalf("清除后应整包重来（无 Range）：%q", hs)
	}
}

// 续传的后半段被篡改：同样失败并清缓存。
func TestResumeTamperedSecondHalfAcrossCalls(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 120_000)
	var n atomic.Int32
	w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			hijackCut(rw, b, 50_000)
			return
		}
		http.ServeContent(rw, r, "blob", time.Time{}, bytes.NewReader(flip(b, 90_000)))
	})
	rd, raw, d := resumeDir(t), w.base+"/blob", dst(t)
	wantKind(t, w.client(downloader.Config{ResumeDir: rd, MaxAttempts: 1}).Download(t.Context(), raw, d, exp), downloader.KindNetwork)
	err := w.client(downloader.Config{ResumeDir: rd}).Download(t.Context(), raw, d, exp)
	wantKind(t, err, downloader.KindSHA256)
	assertNoLeftovers(t, d)
	if e := cacheEntries(t, rd); len(e) != 0 {
		t.Fatalf("缓存应被清除：%v", e)
	}
}

// 策略类失败（实际流超过 size）不保留 partial；超大的缓存 partial（> size）被丢弃并整包重来。
func TestResumePolicyFailureDropsPartialAndOversizeCacheDiscarded(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 60_000)
	var mode atomic.Int32
	w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
		if mode.Load() == 0 {
			rw.WriteHeader(200)
			_, _ = rw.Write(append(append([]byte(nil), b...), make([]byte, 5000)...)) // 超量，分块无 Content-Length
			return
		}
		http.ServeContent(rw, r, "blob", time.Time{}, bytes.NewReader(b))
	})
	rd, raw, d := resumeDir(t), w.base+"/blob", dst(t)
	wantKind(t, w.client(downloader.Config{ResumeDir: rd}).Download(t.Context(), raw, d, exp), downloader.KindSize)
	if e := cacheEntries(t, rd); len(e) != 0 {
		t.Fatalf("策略类失败不得保留 partial：%v", e)
	}
	mode.Store(1)
	if err := os.WriteFile(partPath(rd, raw, exp), make([]byte, exp.Size+10), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := w.client(downloader.Config{ResumeDir: rd}).Download(t.Context(), raw, d, exp); err != nil {
		t.Fatalf("超大的缓存 partial 应被丢弃后整包重来：%v", err)
	}
	if !bytes.Equal(readFile(t, d), b) {
		t.Fatal("内容不符")
	}
}

// 缓存被他人写/被换成符号链接/硬链接/权限过宽：拒绝使用，不触碰符号链接目标，不发出任何请求。
func TestResumeCacheUnsafeRejected(t *testing.T) {
	b, exp := blob(t, 30_000)
	setup := func(t *testing.T) (*world, string, string) {
		w := newWorld(t)
		w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
			http.ServeContent(rw, r, "b", time.Time{}, bytes.NewReader(b))
		})
		return w, resumeDir(t), w.base + "/blob"
	}
	run := func(t *testing.T, w *world, rd, raw string) error {
		return w.client(downloader.Config{ResumeDir: rd}).Download(t.Context(), raw, dst(t), exp)
	}
	t.Run("目录是符号链接", func(t *testing.T) {
		w, rd, raw := setup(t)
		real := t.TempDir()
		if err := os.Symlink(real, rd); err != nil {
			t.Fatal(err)
		}
		err := run(t, w, rd, raw)
		wantKind(t, err, downloader.KindStaging)
		if !strings.Contains(err.Error(), "符号链接") {
			t.Fatalf("应以「符号链接」专门拒绝：%v", err)
		}
		if es, _ := os.ReadDir(real); len(es) != 0 {
			t.Fatalf("不得在符号链接目标里写任何东西：%v", es)
		}
		if w.hitCount("/blob") != 0 {
			t.Fatal("拒绝后不应发请求")
		}
	})
	t.Run("目录对 group/other 开放", func(t *testing.T) {
		w, rd, raw := setup(t)
		if err := os.Mkdir(rd, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(rd, 0o755); err != nil {
			t.Fatal(err)
		}
		wantKind(t, run(t, w, rd, raw), downloader.KindStaging)
		if w.hitCount("/blob") != 0 {
			t.Fatal("拒绝后不应发请求")
		}
	})
	t.Run("目录属于别的用户", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root 下无法构造")
		}
		var other string
		for _, c := range []string{"/private/var/install", "/private/var/audit", "/private/var/backups", "/root"} {
			if fi, err := os.Lstat(c); err == nil && fi.IsDir() && fi.Mode().Perm()&0o077 == 0 {
				other = c
				break
			}
		}
		if other == "" {
			t.Skip("本机没有 root 拥有的 0700 目录可用")
		}
		w, _, raw := setup(t)
		err := run(t, w, other, raw)
		wantKind(t, err, downloader.KindStaging)
		if !strings.Contains(err.Error(), "不属于当前用户") {
			t.Fatalf("应因属主被拒绝：%v", err)
		}
	})
	t.Run("partial 是符号链接", func(t *testing.T) {
		w, rd, raw := setup(t)
		if err := os.Mkdir(rd, 0o700); err != nil {
			t.Fatal(err)
		}
		decoy := filepath.Join(t.TempDir(), "decoy")
		if err := os.WriteFile(decoy, []byte("DECOY"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(decoy, partPath(rd, raw, exp)); err != nil {
			t.Fatal(err)
		}
		wantKind(t, run(t, w, rd, raw), downloader.KindStaging)
		if string(readFile(t, decoy)) != "DECOY" {
			t.Fatal("符号链接目标被改动")
		}
		if _, err := os.Lstat(partPath(rd, raw, exp)); err == nil {
			t.Fatal("不安全的条目应被摘除")
		}
		if w.hitCount("/blob") != 0 {
			t.Fatal("拒绝后不应发请求")
		}
		if err := run(t, w, rd, raw); err != nil { // 摘除后下一次干净重来
			t.Fatalf("摘除后应能成功：%v", err)
		}
	})
	t.Run("partial 权限过宽(他人可写)", func(t *testing.T) {
		w, rd, raw := setup(t)
		if err := os.Mkdir(rd, 0o700); err != nil {
			t.Fatal(err)
		}
		p := partPath(rd, raw, exp)
		if err := os.WriteFile(p, b[:1000], 0o666); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o666); err != nil {
			t.Fatal(err)
		}
		wantKind(t, run(t, w, rd, raw), downloader.KindStaging)
		if w.hitCount("/blob") != 0 {
			t.Fatal("拒绝后不应发请求")
		}
	})
	t.Run("partial 是硬链接", func(t *testing.T) {
		w, rd, raw := setup(t)
		if err := os.Mkdir(rd, 0o700); err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(t.TempDir(), "other")
		if err := os.WriteFile(other, b[:1000], 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(other, partPath(rd, raw, exp)); err != nil {
			t.Skip("文件系统不支持硬链接")
		}
		wantKind(t, run(t, w, rd, raw), downloader.KindStaging)
		if string(readFile(t, other)) != string(b[:1000]) {
			t.Fatal("硬链接的另一端被改动")
		}
	})
	t.Run("partial 是目录", func(t *testing.T) {
		w, rd, raw := setup(t)
		if err := os.MkdirAll(partPath(rd, raw, exp), 0o700); err != nil {
			t.Fatal(err)
		}
		wantKind(t, run(t, w, rd, raw), downloader.KindStaging)
	})
}

// ResumeDir 必须是规范化的绝对路径。
func TestResumeDirMustBeAbsolute(t *testing.T) {
	for _, bad := range []string{"relative/dir", "./x", "/a/../b", "/a//b"} {
		if _, err := downloader.New(downloader.Config{ResumeDir: bad}); !downloader.IsKind(err, downloader.KindOptions) {
			t.Errorf("ResumeDir %q 应被拒绝：%v", bad, err)
		}
	}
}

var reCacheName = regexp.MustCompile(`^[0-9a-f]{64}\.(part|lock)$`)

// 命名安全：缓存条目名只由 SHA256(域前缀+URL+sha+size) 决定，URL 里的 ../、分隔符、控制字符无法影响路径；
// 不同的 URL/sha/size 得到不同的名字。
func TestResumeCacheNamingIsHashOnly(t *testing.T) {
	_, e1 := blob(t, 100)
	k := func(raw string, e downloader.Expected) string { return downloader.ResumeKey(raw, e) }
	base := "https://h/x"
	k1, k2 := k(base, e1), k(base, downloader.Expected{Size: e1.Size, SHA256: e1.SHA256})
	if k1 != k2 {
		t.Fatal("同输入应得同名")
	}
	e2, e3 := e1, e1
	e2.Size++
	e3.SHA256 = strings.Repeat("0", 64)
	seen := map[string]bool{k(base, e1): true}
	for _, v := range []string{k(base+"2", e1), k(base, e2), k(base, e3)} {
		if seen[v] {
			t.Fatal("不同输入不得得到相同的名字")
		}
		seen[v] = true
	}
	// 邻接歧义：把字段边界挪动不能产生同名。
	if k("https://h/x\n"+e1.SHA256, downloader.Expected{Size: 1, SHA256: e1.SHA256}) == k("https://h/x", downloader.Expected{Size: 1, SHA256: e1.SHA256}) {
		t.Fatal("字段边界歧义")
	}
	w := newWorld(t)
	b, exp := blob(t, 5000)
	w.route("/a/../../b", func(rw http.ResponseWriter, r *http.Request) { hijackCut(rw, b, 100) })
	w.route("/b", func(rw http.ResponseWriter, r *http.Request) { hijackCut(rw, b, 100) })
	rd := resumeDir(t)
	_ = w.client(downloader.Config{ResumeDir: rd, MaxAttempts: 1}).Download(t.Context(), w.base+"/a/../../b?x=../../etc/passwd%0a", dst(t), exp)
	es, _ := os.ReadDir(rd)
	if len(es) == 0 {
		t.Fatal("应有缓存条目")
	}
	for _, e := range es {
		if !reCacheName.MatchString(e.Name()) {
			t.Fatalf("缓存条目名 %q 不是纯哈希", e.Name())
		}
	}
	if es, _ := os.ReadDir(filepath.Dir(rd)); len(es) != 1 {
		t.Fatalf("缓存目录之外不得出现文件：%v", es)
	}
}

// 并发：同一个包被多个调用同时下载——缓存槽位由 flock 独占，其余调用退回各自私有的 .part，
// 绝不会有两个写者写同一个文件（否则交错写入会使 SHA256 失败）。服务器用栅栏保证它们真的同时在途。
func TestResumeConcurrentSameKeyNeverSharesPartial(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 200_000)
	const N = 6
	var arrived atomic.Int32
	gate := make(chan struct{})
	w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
		if arrived.Add(1) == N {
			close(gate)
		}
		select {
		case <-gate:
		case <-time.After(10 * time.Second):
		}
		http.ServeContent(rw, r, "blob", time.Time{}, bytes.NewReader(b))
	})
	rd, raw := resumeDir(t), w.base+"/blob"
	var wg sync.WaitGroup
	errs := make([]error, N)
	dsts := make([]string, N)
	for i := range N {
		dsts[i] = dst(t)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = w.client(downloader.Config{ResumeDir: rd}).Download(context.Background(), raw, dsts[i], exp)
		}()
	}
	wg.Wait()
	for i := range N {
		if errs[i] != nil {
			t.Fatalf("并发下载 %d 失败：%v", i, errs[i])
		}
		if !bytes.Equal(readFile(t, dsts[i]), b) {
			t.Fatalf("并发下载 %d 的内容不符", i)
		}
	}
	if arrived.Load() != N {
		t.Fatalf("应有 %d 个并发请求（栅栏已放行）：%d", N, arrived.Load())
	}
	if e := cacheEntries(t, rd); len(e) != 0 {
		t.Fatalf("缓存应干净：%v", e)
	}
}

// 过期清理：超过 TTL 的其它 key 条目被清理；未过期的、以及名称不符合 <64 位十六进制>.part|.lock 的文件不动。
func TestResumePrunesStaleEntriesOnly(t *testing.T) {
	w := newWorld(t)
	b, exp := blob(t, 3000)
	w.route("/blob", func(rw http.ResponseWriter, r *http.Request) {
		http.ServeContent(rw, r, "b", time.Time{}, bytes.NewReader(b))
	})
	rd := resumeDir(t)
	if err := os.Mkdir(rd, 0o700); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, age time.Duration) string {
		p := filepath.Join(rd, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-age)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}
	h := func(c string) string { return strings.Repeat(c, 64) }
	stale, staleLock := mk(h("a")+".part", 8*24*time.Hour), mk(h("a")+".lock", 8*24*time.Hour)
	fresh := mk(h("b")+".part", time.Hour)
	foreign := mk("notes.txt", 30*24*time.Hour)
	// 同一个 key 的 partial 即使很旧（超过 TTL）也不能被清理线程误删：它正是要续传的那个。
	sameKey := partPath(rd, w.base+"/blob", exp)
	if err := os.WriteFile(sameKey, b[:1000], 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-9 * 24 * time.Hour)
	if err := os.Chtimes(sameKey, old, old); err != nil {
		t.Fatal(err)
	}
	if err := w.client(downloader.Config{ResumeDir: rd}).Download(t.Context(), w.base+"/blob", dst(t), exp); err != nil {
		t.Fatal(err)
	}
	if hs := w.rangeHeaders("/blob"); len(hs) != 1 || hs[0] != "bytes=1000-" {
		t.Fatalf("同 key 的旧 partial 应被续传而不是被清理：%q", hs)
	}
	for _, p := range []string{stale, staleLock} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("过期条目 %s 应被清理", filepath.Base(p))
		}
	}
	for _, p := range []string{fresh, foreign} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s 不应被清理：%v", filepath.Base(p), err)
		}
	}
}

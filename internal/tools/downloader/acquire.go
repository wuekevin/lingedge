package downloader

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/security"
)

// 包内固定路径（§9.3）。
const (
	metaPackageJSON = "META/package.json"
	metaSums        = "META/SHA256SUMS"
	metaSumsSig     = "META/SHA256SUMS.sig"
	payloadPrefix   = "payload/"

	maxSumsBytes = 16 << 20
	maxMetaBytes = 64 << 10
	maxSigBytes  = 4 << 10
)

// OnlineSource 是在线来源：清单 URL（签名为 ManifestURL+".sig"）与已构造的 HTTPS 客户端。
type OnlineSource struct {
	Client      *Client
	ManifestURL string
}

// Params 是一次「取得并验证一个包」的全部输入。Online 与 BundlePath 恰好给一个（--offline <bundle.tar>）。
type Params struct {
	Verifier *manifest.Verifier
	// KeysDir 是 trust/keys.d：整条校验链成功后 Verifier.Commit 把 key_rotation 的新公钥落盘到这里并推进 sequence。
	KeysDir string

	Online     *OnlineSource
	BundlePath string

	Channel      string
	AllowExpired bool // --allow-expired-manifest：只与 BundlePath 一起使用（在线路径永不放行过期清单）

	Component string
	Version   string // 精确版本（版本选择/依赖求解属 resolver，Story 1.30/1.33）
	Variant   string
	Platform  string // linux-amd64|linux-arm64；清单只有 any 时回落到 any

	// StagingRoot 下创建 <tx>/；TxID 为空则随机。失败时 <tx>/ 整个删除；成功时保留 <tx>/pkg（解压结果），其余中间文件删除。
	StagingRoot string
	TxID        string
	// Executable 是需要 0755 的 payload 路径（相对包根，如 payload/bin/lingedge-server）。冻结的 META/package.json 没有此字段，
	// 由安装器（Story 1.33）按组件约定提供；缺省全部 0644。
	Executable []string

	// DeferCommit 为 true 时，Acquire 在整条校验链成功后**不**调用 Verifier.Commit（不推进 sequence、不落盘 key_rotation），
	// 而是把它交给调用方：安装步骤成功后调用 Acquired.Commit()，失败则 Acquired.Discard()（不提交并清理 staging）；
	// 或一步到位用 Acquired.Finish(install)。默认 false：行为与 1.28 相同（链成功即提交）。
	// 代价（已知、有意）：提交之前 sequence 没有推进，同一份清单可被再次 Acquire；这正是「安装失败可重试/可回滚」所需要的。
	DeferCommit bool

	// OnStage 在 Acquire 的每个阶段**开始之前**调用（§12.1 的步骤 10–13 对应的四个阶段，见 Stage）。
	// 它是 installer 把「先写 undo 再动手」落到下载链上的挂钩：返回非 nil 即中止 Acquire（与其他失败一样清理 staging/<tx>，信任库不变）。
	// nil = 不观察。回调不得修改 Params 指向的任何对象。
	OnStage func(Stage) error
}

// Stage 是 Acquire 的阶段（与 tools-design §12.1 的步骤 10–13 一一对应）。
type Stage string

// Stage 取值。
const (
	// StageManifest（步骤 10）：取得签名清单；此阶段开始前 staging/<tx> 尚未创建。
	StageManifest Stage = "manifest"
	// StageVerify（步骤 11）：验证清单签名/schema/sequence/有效期/吊销（不推进 sequence，提交由 Commit 完成）。
	StageVerify Stage = "verify"
	// StagePackage（步骤 12）：取得包并核对长度与外层 SHA256。
	StagePackage Stage = "package"
	// StageCheck（步骤 13）：包内 META 验签、逐文件 SHA256 校验与安全解压、META 与清单一致。
	StageCheck Stage = "check"
)

func (p Params) stage(s Stage) error {
	if p.OnStage == nil {
		return nil
	}
	return p.OnStage(s)
}

// Acquired 是成功的结果。
type Acquired struct {
	Result   *manifest.Result
	Version  string
	Platform string // 实际使用的清单平台键（可能是 any）
	Artifact manifest.Artifact
	Meta     *manifest.PackageMeta
	TxDir    string // staging/<tx>
	PkgDir   string // staging/<tx>/pkg：META/ 与 payload/
	Extract  security.ExtractResult
	Warnings []string

	// 延迟提交状态（DeferCommit）。
	mu        sync.Mutex
	v         *manifest.Verifier
	keysDir   string
	committed bool
	gone      bool
}

// Committed 报告信任状态（sequence/key_rotation）是否已经提交。
func (a *Acquired) Committed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.committed
}

// Commit 提交信任状态：落盘 key_rotation 的新公钥并推进 sequence。幂等（已提交则直接成功）；
// 已 Discard 的结果不能再提交；底层失败时保持「未提交」，可重试（manifest.Verifier.Commit 本身幂等）。
func (a *Acquired) Commit() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.committed {
		return nil
	}
	if a.gone {
		return newErr(KindOptions, "该结果已被 Discard，不能再提交")
	}
	if err := a.v.Commit(a.Result, a.keysDir); err != nil {
		return err
	}
	a.committed = true
	return nil
}

// Discard 放弃该结果：删除整个 staging/<tx>，不提交（已提交的信任状态不会被回滚——sequence 只进不退是安全属性）。幂等。
func (a *Acquired) Discard() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gone = true
	if err := os.RemoveAll(a.TxDir); err != nil {
		return wrapErr(KindStaging, err, "无法清理 staging/<tx>")
	}
	return nil
}

// Finish 是「安装成功后才提交」的一步到位写法：运行 install；成功则 Commit；install 失败（或 panic）则 Discard 并原样返回其错误，
// 信任状态不变。Commit 自身失败时同样 Discard 并返回 Commit 的错误。
func (a *Acquired) Finish(install func(*Acquired) error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			_ = a.Discard()
			panic(r)
		}
		if err != nil {
			_ = a.Discard()
		}
	}()
	if err = install(a); err != nil {
		return err
	}
	return a.Commit()
}

// Acquire 执行 §9.5 的完整校验链并解压到 staging：
// 1–4 清单验签/schema/sequence/valid_until/channel/吊销（manifest.Verifier）→ 5 取得包并核对长度与外层 SHA256 →
// 6 解压前读取 META/SHA256SUMS(.sig) 并验签，SafeExtract 逐文件校验 → 7 META/package.json 与清单一致 → Verifier.Commit。
// 在线与离线只在「取得字节」处不同。任一步失败：整个 staging/<tx> 被删除，信任库（sequence、keys.d）不被改动。
// 验证类失败返回 *manifest.Error 或 *Error，cli 映射为退出码 5。
func Acquire(ctx context.Context, p Params) (_ *Acquired, err error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	if err := p.stage(StageManifest); err != nil {
		return nil, err
	}
	src, err := p.openSource()
	if err != nil {
		return nil, err
	}
	defer src.close()

	tx := p.TxID
	if tx == "" {
		var rnd [8]byte
		if _, e := rand.Read(rnd[:]); e != nil {
			return nil, wrapErr(KindStaging, e, "无法生成事务 ID")
		}
		tx = hex.EncodeToString(rnd[:])
	}
	if !reTx.MatchString(tx) {
		return nil, newErr(KindOptions, "TxID 非法")
	}
	if e := security.MkdirAllPerm(p.StagingRoot, 0o700); e != nil {
		return nil, wrapErr(KindStaging, e, "无法创建 staging 根目录")
	}
	txDir := filepath.Join(p.StagingRoot, tx)
	if e := os.Mkdir(txDir, 0o700); e != nil {
		return nil, wrapErr(KindStaging, e, "无法创建 staging/<tx>")
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(txDir)
		}
	}()

	// 步骤 1–4
	mf, sig, err := src.manifest(ctx)
	if err != nil {
		return nil, err
	}
	if err := p.stage(StageVerify); err != nil {
		return nil, err
	}
	res, err := p.Verifier.Verify(mf, sig, manifest.VerifyOptions{Channel: p.Channel, Offline: src.offline(), AllowExpired: p.AllowExpired})
	if err != nil {
		return nil, err
	}
	pv, err := manifest.ParseVersion(p.Version)
	if err != nil {
		return nil, newErr(KindOptions, "版本号非法")
	}
	comp, ok := res.Manifest.Components[p.Component]
	if !ok {
		return nil, newErr(KindNoVersion, "清单中没有组件 %q", p.Component)
	}
	switch comp.Status {
	case manifest.CompUnreleased:
		return nil, newErr(KindUnreleased, "组件 %s 尚未发布", p.Component)
	case manifest.CompBuiltin:
		return nil, newErr(KindBuiltin, "组件 %s 已内置于 %s", p.Component, comp.BuiltinIn)
	}
	if err := res.Manifest.CheckTarget(p.Component, p.Version); err != nil {
		return nil, err
	}
	var ve *manifest.VersionEntry
	for i := range comp.Versions {
		if comp.Versions[i].Parsed.Compare(pv) == 0 {
			ve = &comp.Versions[i]
			break
		}
	}
	if ve == nil {
		return nil, newErr(KindNoVersion, "清单中没有 %s %s", p.Component, pv.String())
	}
	variant, ok := ve.Variants[p.Variant]
	if !ok {
		return nil, newErr(KindNoVersion, "%s %s 没有变体 %q", p.Component, pv.String(), p.Variant)
	}
	platform := p.Platform
	art, ok := variant.Artifacts[platform]
	if !ok {
		platform = "any"
		art, ok = variant.Artifacts[platform]
	}
	if !ok {
		return nil, newErr(KindNoVersion, "%s %s（%s）没有适用于 %s 的包", p.Component, pv.String(), p.Variant, p.Platform)
	}
	name, err := ArtifactFileName(art.URL)
	if err != nil {
		return nil, err
	}

	// 步骤 5：取得包，长度与外层 SHA256 必须等于清单。
	pkgFile := filepath.Join(txDir, name)
	if err := p.stage(StagePackage); err != nil {
		return nil, err
	}
	if err := src.fetch(ctx, art, name, pkgFile); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if err := p.stage(StageCheck); err != nil {
		return nil, err
	}
	// 步骤 6：解压前读取 META/SHA256SUMS(.sig)（META 必须先于 payload），验签，再解压并逐文件校验。
	f, err := os.Open(pkgFile)
	if err != nil {
		return nil, wrapErr(KindStaging, err, "无法读取已下载的包")
	}
	members, err := security.ReadMembers(f, security.ReadMembersOptions{
		Want:   map[string]int64{metaPackageJSON: maxMetaBytes, metaSums: maxSumsBytes, metaSumsSig: maxSigBytes},
		Before: payloadPrefix, Gzip: true,
		Limits: security.Limits{MaxFiles: 64, MaxTotalBytes: maxSumsBytes + 2*maxMetaBytes},
	})
	_ = f.Close()
	if err != nil {
		return nil, wrapErr(KindExtract, err, "无法读取包内 META")
	}
	sumsB, sigB, metaB := members[metaSums], members[metaSumsSig], members[metaPackageJSON]
	if _, err := p.Verifier.VerifyPackageSums(sigB, sumsB, p.Component, pv.String(), platform, res.Manifest.GeneratedAt); err != nil {
		return nil, err
	}
	sums, err := security.ParseSHA256SUMS(sumsB, security.Limits{})
	if err != nil {
		return nil, wrapErr(KindExtract, err, "META/SHA256SUMS 无法解析")
	}
	if len(sums) == 0 {
		return nil, newErr(KindExtract, "META/SHA256SUMS 为空")
	}
	for k := range sums {
		if !strings.HasPrefix(k, payloadPrefix) {
			return nil, newErr(KindExtract, "META/SHA256SUMS 含 payload/ 之外的路径 %q", trim(k, 64))
		}
	}
	meta, err := manifest.ParsePackageMeta(metaB)
	if err != nil {
		return nil, err
	}
	if meta.FilesCount != len(sums) {
		return nil, &manifest.Error{Kind: manifest.KindPackage, Msg: "META/package.json 的 files_count 与已签名的 SHA256SUMS 不符"}
	}
	metaBytes := int64(len(sumsB) + len(sigB) + len(metaB))
	lim := security.Limits{
		MaxFiles:      2*len(sums) + 16, // 目录条目也计入
		MaxTotalBytes: meta.UnpackedSize + metaBytes,
		MaxFileBytes:  meta.UnpackedSize + metaBytes,
	}
	f, err = os.Open(pkgFile)
	if err != nil {
		return nil, wrapErr(KindStaging, err, "无法读取已下载的包")
	}
	pkgDir := filepath.Join(txDir, "pkg")
	ex, err := security.SafeExtract(ctx, f, pkgDir, security.ExtractOptions{
		Limits: lim, Gzip: true, Checksums: sums,
		Unchecked:  []string{metaPackageJSON, metaSums, metaSumsSig},
		Executable: p.Executable,
	})
	_ = f.Close()
	if err != nil {
		if ce := ctx.Err(); ce != nil {
			return nil, ce
		}
		return nil, wrapErr(KindExtract, err, "包解压/逐文件校验失败")
	}
	// META 三个文件不在 SHA256SUMS 中：核对解压出的字节与验签时读到的字节完全一致。
	for n, want := range map[string][]byte{metaPackageJSON: metaB, metaSums: sumsB, metaSumsSig: sigB} {
		got, e := os.ReadFile(filepath.Join(pkgDir, filepath.FromSlash(n)))
		if e != nil || !bytes.Equal(got, want) {
			return nil, newErr(KindExtract, "解压出的 %s 与验签时读取的内容不一致", n)
		}
	}
	if payload := ex.Bytes - metaBytes; payload != meta.UnpackedSize {
		return nil, &manifest.Error{Kind: manifest.KindPackage, Msg: "META/package.json 的 unpacked_size 与实际 payload 字节数不符"}
	}

	// 步骤 7
	if err := meta.Matches(p.Component, pv.String(), p.Variant, platform); err != nil {
		return nil, err
	}

	// 整条链成功：（除非 DeferCommit）落盘轮换公钥并推进 sequence，随后清理中间文件。
	out := &Acquired{Result: res, Version: pv.String(), Platform: platform, Artifact: art, Meta: meta,
		TxDir: txDir, PkgDir: pkgDir, Extract: ex, Warnings: res.Warnings, v: p.Verifier, keysDir: p.KeysDir}
	if !p.DeferCommit {
		if err := out.Commit(); err != nil {
			return nil, err
		}
	}
	_ = os.Remove(pkgFile)
	return out, nil
}

func (p Params) check() error {
	switch {
	case p.Verifier == nil:
		return newErr(KindOptions, "Verifier 必填")
	case (p.Online == nil) == (p.BundlePath == ""):
		return newErr(KindOptions, "必须且只能指定一个来源：在线（Online）或 --offline <bundle.tar>")
	case p.AllowExpired && p.BundlePath == "":
		return newErr(KindOptions, "--allow-expired-manifest 只能与 --offline 一起使用")
	case p.StagingRoot == "" || p.KeysDir == "":
		return newErr(KindOptions, "StagingRoot 与 KeysDir 必填")
	case p.Component == "" || p.Variant == "" || p.Platform == "":
		return newErr(KindOptions, "Component/Variant/Platform 必填")
	}
	if p.Online != nil && (p.Online.Client == nil || p.Online.ManifestURL == "") {
		return newErr(KindOptions, "Online.Client 与 ManifestURL 必填")
	}
	return nil
}

// source 抽象「取得字节」：这是在线与离线唯一的差异。
type source interface {
	offline() bool
	manifest(ctx context.Context) (mf, sig []byte, err error)
	fetch(ctx context.Context, art manifest.Artifact, name, dst string) error
	close()
}

func (p Params) openSource() (source, error) {
	if p.Online != nil {
		if err := CheckURL(p.Online.ManifestURL); err != nil {
			return nil, err
		}
		return onlineSource{p.Online}, nil
	}
	b, err := OpenBundle(p.BundlePath)
	if err != nil {
		return nil, err
	}
	return bundleSource{b}, nil
}

type onlineSource struct{ o *OnlineSource }

func (onlineSource) offline() bool { return false }
func (s onlineSource) manifest(ctx context.Context) ([]byte, []byte, error) {
	mf, err := s.o.Client.FetchBytes(ctx, s.o.ManifestURL, bundleManifestMax)
	if err != nil {
		return nil, nil, err
	}
	sig, err := s.o.Client.FetchBytes(ctx, s.o.ManifestURL+".sig", bundleSigMax)
	if err != nil {
		return nil, nil, err
	}
	return mf, sig, nil
}
func (s onlineSource) fetch(ctx context.Context, art manifest.Artifact, _, dst string) error {
	return s.o.Client.Download(ctx, art.URL, dst, Expected{Size: art.Size, SHA256: art.SHA256})
}
func (onlineSource) close() {}

type bundleSource struct{ b *Bundle }

func (bundleSource) offline() bool { return true }
func (s bundleSource) manifest(context.Context) ([]byte, []byte, error) {
	return s.b.Manifest()
}
func (s bundleSource) fetch(_ context.Context, art manifest.Artifact, name, dst string) error {
	return s.b.copyPackage(name, dst, Expected{Size: art.Size, SHA256: art.SHA256})
}
func (s bundleSource) close() { _ = s.b.Close() }

var reTx = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

package installer

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/downloader"
	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/resolver"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
	"github.com/wuekevin/lingedge/internal/tools/service"
	"github.com/wuekevin/lingedge/internal/tools/updater"
	"github.com/wuekevin/lingedge/pkg/version"
)

// SelfExecutable 是 tools 包内的可执行文件（发行流水线的包布局：payload/bin/lingedge）。
const SelfExecutable = "payload/bin/lingedge"

// SelfRequest 是 `lingedge update self` 的请求。
type SelfRequest struct {
	Version      string
	Channel      string
	Offline      string
	AllowExpired bool
	DryRun       bool
}

// SelfResult 是 update self 的结果。
type SelfResult struct {
	Outcome string `json:"outcome"` // planned | updated | already_up_to_date
	From    string `json:"from"`
	To      string `json:"to"`
	Target  string `json:"target"`
	// Prev 是保留的旧版本（<target>.prev）；SelfCheck 是自检时新版本报告的版本行。
	Prev      string    `json:"prev,omitempty"`
	SelfCheck string    `json:"self_check,omitempty"`
	Warnings  []Warning `json:"warnings"`
}

func (in *Installer) selfPath() (string, error) {
	if in.d.SelfPath != nil {
		return in.d.SelfPath()
	}
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(p)
}

func (in *Installer) toolsVersion() string {
	if in.d.Update != nil && in.d.Update.ToolsVersion != "" {
		return in.d.Update.ToolsVersion
	}
	return version.Version
}

func (in *Installer) selfRunner() (service.Runner, error) {
	if in.d.SelfRun != nil {
		return in.d.SelfRun, nil
	}
	return safeexec.New(safeexec.Options{Allowed: []string{"lingedge.new"}, SearchDirs: []string{"/usr/local/bin"}, MaxOutput: 64 << 10})
}

// UpdateSelf 升级 Tools 自身（tools-design §17/§18.3）：同一条验证链（清单验签 → 包 SHA256 → 包内逐文件签名）取得 tools 包，
// 把新可执行文件先写成 <目标>.new 并执行 `<目标>.new --version` 自检（版本必须等于目标版本），再 rename 覆盖目标，旧版本保留为 <目标>.prev；
// 全部成功之后才提交信任状态（sequence 推进）。任何一步失败：目标文件原封不动、.new 被删除。不降级、不跳过 min_upgrade_from。
func (in *Installer) UpdateSelf(ctx context.Context, req SelfRequest) (*SelfResult, error) {
	if req.Channel == "" {
		req.Channel = "stable"
	}
	if req.Channel != "stable" && req.Channel != "beta" {
		return nil, newErr(KindSettings, "通道必须是 stable 或 beta")
	}
	target, err := in.selfPath()
	if err != nil {
		return nil, wrap(KindPrecondition, err, "无法确定当前可执行文件的位置")
	}
	res := &SelfResult{Target: target, From: in.toolsVersion(), Warnings: []Warning{}}
	mfRes, err := in.loadManifest(ctx, Request{Component: "tools", Channel: req.Channel, Offline: req.Offline, AllowExpired: req.AllowExpired})
	if err != nil {
		return nil, err
	}
	for _, w := range mfRes.Warnings {
		res.Warnings = append(res.Warnings, Warning{Code: "manifest.warning", Message: w})
	}
	entries := updater.Check(mfRes.Manifest, registryStateWith("tools", res.From))
	var e updater.Entry
	if len(entries) == 1 {
		e = entries[0]
	}
	comp, ok := mfRes.Manifest.Components["tools"]
	if !ok || comp.Status != manifest.CompAvailable {
		return nil, &Error{Kind: KindPrecondition, Msg: "清单里没有可用的 tools 组件", SystemState: "unchanged", Details: map[string]any{"code": "precondition.tools_unreleased"}}
	}
	cur, curErr := manifest.ParseVersion(res.From)
	if curErr != nil {
		res.Warnings = append(res.Warnings, Warning{Code: "self.version_unparsed", Message: "当前版本 " + res.From + " 不是 SemVer（开发构建）：按最旧版本比较"})
	}
	to := req.Version
	if to == "" {
		to = e.Latest
	}
	if to == "" {
		return nil, &Error{Kind: KindPrecondition, Msg: "清单里没有 tools 的可用稳定版本", SystemState: "unchanged"}
	}
	tv, err := manifest.ParseVersion(to)
	if err != nil {
		return nil, newErr(KindSettings, "目标版本 %q 不是严格 SemVer", to)
	}
	if err := mfRes.Manifest.CheckTarget("tools", tv.String()); err != nil {
		return nil, wrap(KindStepFailed, err, "目标版本不可用")
	}
	var entry *manifest.VersionEntry
	for i := range comp.Versions {
		if v, perr := manifest.ParseVersion(comp.Versions[i].Version); perr == nil && v.Compare(tv) == 0 {
			entry = &comp.Versions[i]
		}
	}
	if entry == nil {
		return nil, newErr(KindSettings, "清单里没有 tools@%s", tv.String())
	}
	res.To = tv.String()
	if curErr == nil {
		switch c := tv.Compare(cur); {
		case c == 0:
			res.Outcome = "already_up_to_date"
			return res, nil
		case c < 0:
			return nil, &Error{Kind: KindPrecondition, Msg: fmt.Sprintf("tools 当前 %s 高于目标 %s：不支持降级", cur.String(), tv.String()), SystemState: "unchanged"}
		}
		if entry.MinUpgradeFrom != "" {
			if mv, perr := manifest.ParseVersion(entry.MinUpgradeFrom); perr == nil && cur.Compare(mv) < 0 {
				return nil, &Error{Kind: KindPrecondition, SystemState: "unchanged", Msg: fmt.Sprintf("tools@%s 要求当前版本 ≥ %s，当前为 %s，不能直接升级", tv.String(), mv.String(), cur.String()),
					Hint: "须逐级升级：" + strings.Join(e.UpgradePath, " → ")}
			}
		}
	}
	if req.DryRun {
		res.Outcome = "planned"
		return res, nil
	}
	pr, err := in.d.Collect(ctx)
	if err != nil {
		return nil, wrap(KindStepFailed, err, "系统探测失败")
	}
	platform := pr.System.OS + "-" + pr.System.Arch
	if platform == "-" {
		platform = runtime.GOOS + "-" + runtime.GOARCH
	}
	v, err := in.verifier(true)
	if err != nil {
		return nil, err
	}
	var src downloader.Params
	if req.Offline != "" {
		src.BundlePath = req.Offline
	} else {
		url, uerr := in.d.Config.ManifestURL(req.Channel)
		if uerr != nil {
			return nil, uerr
		}
		cl, cerr := in.d.NewClient(in.d.Config.DownloaderConfig(filepath.Join(in.d.Reg.L.Root, "downloads"), true))
		if cerr != nil {
			return nil, cerr
		}
		defer cl.Close()
		src.Online = &downloader.OnlineSource{Client: cl, ManifestURL: url}
	}
	dp := src
	dp.Verifier, dp.KeysDir = v, filepath.Join(in.d.Reg.L.TrustDir(), "keys.d")
	dp.Channel, dp.AllowExpired = req.Channel, req.AllowExpired
	dp.Component, dp.Version, dp.Variant, dp.Platform = "tools", tv.String(), "default", platform
	dp.StagingRoot = in.d.Reg.L.StagingDir()
	dp.TxID = "self-" + string(in.d.Now().UTC().Format("20060102T150405")) + "-" + randSuffix()
	dp.DeferCommit = true
	dp.Executable = []string{SelfExecutable}
	a, err := downloader.Acquire(ctx, dp)
	if err != nil {
		return nil, &Error{Kind: KindStepFailed, Msg: "取得并验证 tools 包失败", Cause: err, SystemState: "unchanged"}
	}
	if err := a.Finish(func(a *downloader.Acquired) error {
		prev, err := updater.SwapBinary(ctx, target, filepath.Join(a.PkgDir, filepath.FromSlash(SelfExecutable)), func(cx context.Context, newPath string) error {
			line, cerr := in.selfCheck(cx, newPath, tv.String())
			res.SelfCheck = line
			return cerr
		})
		res.Prev = prev
		return err
	}); err != nil {
		return nil, &Error{Kind: KindStepFailed, Msg: "替换 Tools 自身失败，当前可执行文件保持不变", Cause: err, SystemState: "unchanged"}
	}
	_ = a.Discard() // 成功时 staging/<tx> 仍保留解压结果：清理
	res.Outcome = "updated"
	return res, nil
}

// selfCheck 执行 `<new> --version`：必须成功且输出包含目标版本号（防止包里放了别的版本/不是 lingedge 的东西）。
func (in *Installer) selfCheck(ctx context.Context, newPath, want string) (string, error) {
	r, err := in.selfRunner()
	if err != nil {
		return "", err
	}
	res, err := r.Run(ctx, safeexec.Cmd{Path: newPath, Args: []string{"--version"}})
	if err != nil {
		return "", fmt.Errorf("执行 %s --version 失败: %w", filepath.Base(newPath), err)
	}
	line := strings.TrimSpace(string(res.Stdout))
	if !strings.Contains(" "+line+" ", " "+want+" ") {
		return line, fmt.Errorf("自检输出 %q 不含目标版本 %s", clip(line, 80), want)
	}
	return line, nil
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

var _ = errors.New
var _ = resolver.PolicyManaged

func randSuffix() string {
	var b [4]byte
	_, _ = cryptorand.Read(b[:])
	return fmt.Sprintf("%x", b)
}

func registryStateWith(name, ver string) registry.State {
	st := registry.NewState()
	if _, err := manifest.ParseVersion(ver); err != nil {
		ver = "0.0.0"
	}
	st.Components[name] = registry.Component{Version: ver, State: "installed"}
	return st
}

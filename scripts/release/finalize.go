package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
)

type revokeFlags []string

func (r *revokeFlags) String() string     { return strings.Join(*r, ",") }
func (r *revokeFlags) Set(v string) error { *r = append(*r, v); return nil }

// outManifest 是写出的 manifest.json 结构（字段顺序固定，输出确定）。
type outManifest struct {
	SchemaVersion   int                       `json:"schema_version"`
	Channel         string                    `json:"channel"`
	Sequence        int64                     `json:"sequence"`
	GeneratedAt     string                    `json:"generated_at"`
	ValidUntil      string                    `json:"valid_until"`
	RevokedVersions []manifest.RevokedVersion `json:"revoked_versions"`
	Components      map[string]outComponent   `json:"components"`
	KeyRotation     *manifest.KeyRotation     `json:"key_rotation,omitempty"`
}

type outComponent struct {
	Status      string                  `json:"status"`
	Description string                  `json:"description,omitempty"`
	Internal    bool                    `json:"internal,omitempty"`
	BuiltinIn   string                  `json:"builtin_in,omitempty"`
	Versions    []manifest.VersionEntry `json:"versions,omitempty"`
}

func cmdFinalize(args []string, stdout, stderr io.Writer) error {
	fl := flag.NewFlagSet("finalize", flag.ContinueOnError)
	fl.SetOutput(stderr)
	out := fl.String("out", "", "build 的输出目录（必填）")
	keys := fl.String("keys", "", "发行公钥集 keys.json（必填；用于在组装前验证第 1 轮签名）")
	sigs := fl.String("sigs", "", "第 1 轮 .sig 所在目录（缺省 <out>/signatures）")
	channel := fl.String("channel", "stable", "通道 stable|beta")
	seq := fl.Int64("sequence", 0, "清单 sequence（单调递增整数，必填，≥1）")
	baseURL := fl.String("base-url", "", "发行源 release.base_url（必填，不含凭据的 https 地址，T-3）")
	genAt := fl.String("generated-at", "", "generated_at（RFC 3339；缺省为当前时间）")
	days := fl.Int("valid-days", 90, "清单有效天数（valid_until = generated_at + N 天，防冻结）")
	prev := fl.String("prev-manifest", "", "上一份已发布清单；提供则强制 sequence 严格大于它且通道一致")
	rotation := fl.String("key-rotation", "", "key_rotation 段 JSON 文件（由 release-sign endorse 在离线机产出）")
	var revoke revokeFlags
	fl.Var(&revoke, "revoke", "吊销版本 <组件>@<版本>=<原因>（可重复）")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *out == "" || *keys == "" || *baseURL == "" || *seq < 1 {
		return errors.New("finalize 需要 -out、-keys、-base-url 与 -sequence（≥1）")
	}
	if *channel != "stable" && *channel != "beta" {
		return errors.New("-channel 必须是 stable|beta")
	}
	if *days < 1 || *days > 366 {
		return errors.New("-valid-days 必须在 1..366")
	}
	gen := time.Now().UTC().Truncate(time.Second)
	if *genAt != "" {
		t, err := parseTime("-generated-at", *genAt)
		if err != nil {
			return err
		}
		gen = t
	}
	base, err := checkBaseURL(*baseURL)
	if err != nil {
		return err
	}
	o, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	sigDir := *sigs
	if sigDir == "" {
		sigDir = filepath.Join(o, "signatures")
	}
	plan, err := loadPlan(o)
	if err != nil {
		return err
	}
	if *prev != "" {
		if err := checkAgainstPrevious(*prev, *channel, *seq); err != nil {
			return err
		}
	}
	kb, err := os.ReadFile(*keys)
	if err != nil {
		return err
	}
	kf, err := manifest.ParseKeysFile(kb)
	if err != nil {
		return fmt.Errorf("-keys: %w", err)
	}
	trust, err := manifest.NewTrustSet(kf)
	if err != nil {
		return fmt.Errorf("-keys: %w", err)
	}
	ver := &manifest.Verifier{Trust: trust}

	// 1. 逐包：验证第 1 轮签名（用 Tools 同一实现）→ 复核 stage 未被改动 → 写最终 .tar.gz。
	artifacts := map[string]manifest.Artifact{} // pkg id → artifact
	for _, p := range plan.Packages {
		stageDir := filepath.Join(o, filepath.FromSlash(p.Stage))
		sums, err := os.ReadFile(filepath.Join(stageDir, "META", "SHA256SUMS"))
		if err != nil {
			return err
		}
		sigBytes, err := os.ReadFile(filepath.Join(sigDir, p.ID+".sig"))
		if err != nil {
			return fmt.Errorf("缺少 %s 的包内签名（第 1 轮离线签名未完成？）: %w", p.ID, err)
		}
		if _, err := ver.VerifyPackageSums(sigBytes, sums, p.Component, p.Version, p.Platform, gen); err != nil {
			return fmt.Errorf("%s 的包内 SHA256SUMS 签名验证失败（用 -keys 中的公钥）: %w", p.ID, err)
		}
		tarGz, err := writePackage(stageDir, sigBytes, plan.BuiltAt, sums)
		if err != nil {
			return fmt.Errorf("组装 %s 失败: %w", p.ID, err)
		}
		dst := filepath.Join(o, "packages", p.Component, p.Version, p.Filename)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, tarGz, 0o644); err != nil {
			return err
		}
		artifacts[p.ID] = manifest.Artifact{URL: base + "/" + p.Component + "/" + p.Version + "/" + p.Filename, SHA256: sha256Hex(tarGz), Size: int64(len(tarGz))}
	}

	// 2. 清单。
	m := outManifest{SchemaVersion: 1, Channel: *channel, Sequence: *seq, GeneratedAt: gen.Format(time.RFC3339),
		ValidUntil: gen.Add(time.Duration(*days) * 24 * time.Hour).Format(time.RFC3339), RevokedVersions: []manifest.RevokedVersion{}, Components: map[string]outComponent{}}
	for _, r := range revoke {
		rv, err := parseRevoke(r)
		if err != nil {
			return err
		}
		m.RevokedVersions = append(m.RevokedVersions, rv)
	}
	for _, c := range plan.Components {
		oc := outComponent{Status: c.Status, Description: c.Description, Internal: c.Internal, BuiltinIn: c.BuiltinIn}
		if c.Version != nil {
			ve := *c.Version
			ve.Variants = map[string]manifest.Variant{}
			for vn, v := range c.Version.Variants {
				arts := map[string]manifest.Artifact{}
				for _, p := range plan.Packages {
					if p.Component == c.Name && p.Variant == vn {
						arts[p.Platform] = artifacts[p.ID]
					}
				}
				if len(arts) == 0 {
					return fmt.Errorf("组件 %s 变体 %s 没有任何包", c.Name, vn)
				}
				ve.Variants[vn] = manifest.Variant{Artifacts: arts, Requires: v.Requires}
			}
			oc.Versions = []manifest.VersionEntry{ve}
		}
		m.Components[c.Name] = oc
	}
	if *rotation != "" {
		rb, err := os.ReadFile(*rotation)
		if err != nil {
			return err
		}
		var kr manifest.KeyRotation
		if err := json.Unmarshal(rb, &kr); err != nil || len(kr.Keys) == 0 {
			return fmt.Errorf("-key-rotation 文件不是有效的 key_rotation 段: %v", err)
		}
		m.KeyRotation = &kr
	}
	mb, err := marshalIndent(m)
	if err != nil {
		return err
	}
	// 自检：生成的清单必须通过 Tools 自己的 schema 校验（含版本约束语法、URL、sha256、size、必填字段）。
	if _, err := manifest.ParseManifest(mb); err != nil {
		return fmt.Errorf("生成的 manifest.json 未通过 Tools 的 schema 校验: %w", err)
	}
	if err := os.WriteFile(filepath.Join(o, "manifest.json"), mb, 0o644); err != nil {
		return err
	}
	ts := toSign{Schema: toSignSchema, Release: plan.Release, Commit: plan.Commit, Round: 2, Items: []toSignItem{{
		ID: "manifest", Kind: kindManifest, File: "manifest.json", SHA256: sha256Hex(mb), Sig: "manifest.json.sig",
		Channel: *channel, Sequence: *seq, GeneratedAt: m.GeneratedAt, ValidUntil: m.ValidUntil}}}
	tb, err := writeJSON(filepath.Join(o, "to-sign-2.json"), ts)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "最终包已组装（%d 个），待签 manifest.json 已生成（通道 %s，sequence %d，有效至 %s）。\n  to-sign-2.json sha256=%s\n  把 manifest.json 与 to-sign-2.json 带到离线机，运行：\n    release-sign sign -key <私钥> -to-sign %s/to-sign-2.json -expect-digest %s\n  然后把 manifest.json.sig 放回 %s，执行 make release-verify。\n",
		len(plan.Packages), *channel, *seq, m.ValidUntil, sha256Hex(tb), o, sha256Hex(tb), o)
	return nil
}

func marshalIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// checkBaseURL 校验发行源：不含凭据、不含查询/片段的 https 地址；返回去掉尾部斜杠的形式。
func checkBaseURL(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(s, " \t\n\\") {
		return "", fmt.Errorf("-base-url %q 必须是不含凭据/查询/片段的 https 地址", s)
	}
	return strings.TrimRight(s, "/"), nil
}

func parseRevoke(s string) (manifest.RevokedVersion, error) {
	left, reason, _ := strings.Cut(s, "=")
	comp, ver, ok := strings.Cut(left, "@")
	if !ok || !reComponentFlag.MatchString(comp) {
		return manifest.RevokedVersion{}, fmt.Errorf("-revoke 格式应为 <组件>@<版本>=<原因>，收到 %q", s)
	}
	v, err := manifest.ParseVersion(ver)
	if err != nil {
		return manifest.RevokedVersion{}, fmt.Errorf("-revoke 版本非法: %w", err)
	}
	return manifest.RevokedVersion{Component: comp, Version: v.String(), Reason: reason}, nil
}

func checkAgainstPrevious(path, channel string, seq int64) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	pm, err := manifest.ParseManifest(b)
	if err != nil {
		return fmt.Errorf("-prev-manifest 不是合法清单: %w", err)
	}
	if pm.Channel != channel {
		return fmt.Errorf("-prev-manifest 通道 %s 与本次 %s 不同", pm.Channel, channel)
	}
	if seq <= pm.Sequence {
		return fmt.Errorf("sequence %d 必须严格大于上一份清单的 %d（Tools 对回退硬失败，且没有放行开关）", seq, pm.Sequence)
	}
	return nil
}

// writePackage 写确定性的 .tar.gz：META/package.json、META/SHA256SUMS、META/SHA256SUMS.sig 在最前，随后是按路径排序的 payload/ 文件。
// 同一 stage 树与签名永远得到相同字节（固定时间戳、uid/gid=0、无用户名、gzip 头清零）。
func writePackage(stageDir string, sigBytes []byte, builtAt string, sums []byte) ([]byte, error) {
	mt, err := time.Parse(time.RFC3339, builtAt)
	if err != nil {
		return nil, err
	}
	files, err := listPayload(filepath.Join(stageDir, "payload"))
	if err != nil {
		return nil, err
	}
	// 复核：stage 里的 payload 必须与已签名的 SHA256SUMS 完全一致（签名后到组装前 stage 被改动 → 失败）。
	var re strings.Builder
	var total int64
	for _, f := range files {
		h, n, err := sha256File(f.Path)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&re, "%s  payload/%s\n", h, f.Rel)
		total += n
	}
	if re.String() != string(sums) {
		return nil, errors.New("stage 的 payload 与已签名的 SHA256SUMS 不一致（签名之后被改动？）")
	}
	metaBytes, err := os.ReadFile(filepath.Join(stageDir, "META", "package.json"))
	if err != nil {
		return nil, err
	}
	meta, err := manifest.ParsePackageMeta(metaBytes)
	if err != nil {
		return nil, err
	}
	if meta.FilesCount != len(files) || meta.UnpackedSize != total {
		return nil, errors.New("META/package.json 的 files_count/unpacked_size 与 payload 不符")
	}

	var raw bytes.Buffer
	tw := tar.NewWriter(&raw)
	add := func(name string, body []byte, mode int64) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: mode, Size: int64(len(body)), ModTime: mt, Format: tar.FormatPAX}); err != nil {
			return err
		}
		_, err := tw.Write(body)
		return err
	}
	for _, e := range []struct {
		n string
		b []byte
	}{{"META/package.json", metaBytes}, {"META/SHA256SUMS", sums}, {"META/SHA256SUMS.sig", sigBytes}} {
		if err := add(e.n, e.b, 0o644); err != nil {
			return nil, err
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Rel < files[j].Rel })
	for _, f := range files {
		body, err := os.ReadFile(f.Path)
		if err != nil {
			return nil, err
		}
		mode := int64(0o644)
		if f.Exec {
			mode = 0o755
		}
		if err := add("payload/"+f.Rel, body, mode); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return gz.Bytes(), nil
}

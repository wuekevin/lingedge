package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// SBOM（Story 1.34 AC3、NFR-15、ADR-012 §1.10、G-7）：CycloneDX 1.5 JSON。
//
// 工具选型（[ASSUMPTION]，实现时核对）：不引入 syft/cyclonedx-gomod 等外部工具，直接由流水线用标准库生成——
//   - Go：对每个**实际发行的二进制**执行 `go version -m`，得到构建它所用的 toolchain、构建设置与全部依赖模块（路径/版本/h1 校验和），
//     这是「该二进制真正包含了什么」的权威来源（比 go.mod 更准：不含未链接进去的模块）；
//   - npm：pnpm-lock.yaml（精确版本 + integrity）；仓库目前没有前端 lockfile（Story 1.36 才交付前端），此时如实写入「无 lockfile」而不虚构条目。
//
// SBOM 不含时间戳之外的任何非确定性内容：timestamp 取提交时间，serialNumber 由内容摘要派生，使同源两次构建的 SBOM 逐字节一致。
// 每个包的 SBOM 放在包内 payload/share/sbom.cdx.json，因此被包内 SHA256SUMS 签名与清单的外层 sha256 覆盖（随清单发布且被签名保护）；
// 另有汇总的 SBOM.cdx.json 放在发行目录供人阅读。

type bomDoc struct {
	BOMFormat    string    `json:"bomFormat"`
	SpecVersion  string    `json:"specVersion"`
	SerialNumber string    `json:"serialNumber"`
	Version      int       `json:"version"`
	Metadata     bomMeta   `json:"metadata"`
	Components   []bomComp `json:"components"`
	Properties   []bomProp `json:"properties,omitempty"`
	index        map[string]bool
}

type bomMeta struct {
	Timestamp  string    `json:"timestamp"`
	Tools      bomTools  `json:"tools"`
	Component  *bomComp  `json:"component,omitempty"`
	Properties []bomProp `json:"properties,omitempty"`
}

type bomTools struct {
	Components []bomComp `json:"components"`
}

type bomComp struct {
	Type       string    `json:"type"`
	BOMRef     string    `json:"bom-ref,omitempty"`
	Name       string    `json:"name"`
	Version    string    `json:"version,omitempty"`
	PURL       string    `json:"purl,omitempty"`
	Hashes     []bomHash `json:"hashes,omitempty"`
	Properties []bomProp `json:"properties,omitempty"`
}

type bomHash struct {
	Alg     string `json:"alg"`
	Content string `json:"content"`
}

type bomProp struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

const toolName = "lingedge release pipeline (scripts/release)"

func newBOM(timestamp, subjectName, subjectVersion string) *bomDoc {
	return &bomDoc{
		BOMFormat: "CycloneDX", SpecVersion: "1.5", Version: 1,
		Metadata: bomMeta{Timestamp: timestamp, Tools: bomTools{Components: []bomComp{{Type: "application", Name: toolName}}},
			Component: &bomComp{Type: "application", Name: subjectName, Version: subjectVersion}},
		Components: []bomComp{}, index: map[string]bool{},
	}
}

func (b *bomDoc) add(c bomComp) {
	key := c.BOMRef
	if key == "" {
		key = c.Type + "/" + c.Name + "@" + c.Version
	}
	if b.index[key] {
		return
	}
	b.index[key] = true
	b.Components = append(b.Components, c)
}

// finish 排序并填充确定性的 serialNumber。
func (b *bomDoc) finish() ([]byte, error) {
	sort.Slice(b.Components, func(i, j int) bool {
		if b.Components[i].Type != b.Components[j].Type {
			return b.Components[i].Type < b.Components[j].Type
		}
		if b.Components[i].Name != b.Components[j].Name {
			return b.Components[i].Name < b.Components[j].Name
		}
		return b.Components[i].Version < b.Components[j].Version
	})
	b.SerialNumber = ""
	pre, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	h := sha256.Sum256(pre)
	// 由内容摘要派生 UUID（版本位 5 的形式；仅用于确定性，不是随机数）。
	u := h[:16]
	u[6] = (u[6] & 0x0f) | 0x50
	u[8] = (u[8] & 0x3f) | 0x80
	b.SerialNumber = fmt.Sprintf("urn:uuid:%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
	out, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// goModInfo 是 `go version -m` 的解析结果。
type goModInfo struct {
	GoVersion string
	Path      string
	Main      string
	Deps      []goDep
	Build     map[string]string
}

type goDep struct{ Path, Version, Sum, ReplacedBy string }

// parseGoVersionM 解析 `go version -m <binary>` 的输出。
func parseGoVersionM(out string) (*goModInfo, error) {
	info := &goModInfo{Build: map[string]string{}}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 {
		return nil, fmt.Errorf("go version -m 输出为空")
	}
	first := strings.SplitN(lines[0], ": ", 2)
	if len(first) != 2 || !strings.HasPrefix(first[1], "go") {
		return nil, fmt.Errorf("无法解析 go version -m 首行: %q", lines[0])
	}
	info.GoVersion = strings.TrimSpace(first[1])
	for _, l := range lines[1:] {
		f := strings.Split(strings.TrimPrefix(l, "\t"), "\t")
		switch f[0] {
		case "path":
			if len(f) > 1 {
				info.Path = f[1]
			}
		case "mod":
			if len(f) > 1 {
				info.Main = f[1]
			}
		case "dep":
			if len(f) < 3 {
				return nil, fmt.Errorf("dep 行格式非法: %q", l)
			}
			d := goDep{Path: f[1], Version: f[2]}
			if len(f) > 3 {
				d.Sum = f[3]
			}
			info.Deps = append(info.Deps, d)
		case "=>":
			if len(info.Deps) == 0 || len(f) < 3 {
				return nil, fmt.Errorf("=> 行位置/格式非法: %q", l)
			}
			info.Deps[len(info.Deps)-1].ReplacedBy = f[1] + "@" + f[2]
			if len(f) > 3 {
				info.Deps[len(info.Deps)-1].Sum = f[3]
			}
		case "build":
			if len(f) > 1 {
				kv := strings.SplitN(f[1], "=", 2)
				if len(kv) == 2 {
					info.Build[kv[0]] = kv[1]
				} else {
					info.Build[f[1]] = ""
				}
			}
		}
	}
	if info.Main == "" || info.Path == "" {
		return nil, fmt.Errorf("go version -m 输出缺少 path/mod（不是 Go 二进制？）")
	}
	return info, nil
}

func goPURL(path, version string) string {
	segs := strings.Split(path, "/")
	for i := range segs {
		segs[i] = url.PathEscape(segs[i])
	}
	return "pkg:golang/" + strings.Join(segs, "/") + "@" + url.PathEscape(version)
}

// goSBOM 为一个二进制生成 SBOM。
func goSBOM(binPath, name, version, platform, timestamp string) (*bomDoc, error) {
	out, err := goRun("", nil, "version", "-m", binPath)
	if err != nil {
		return nil, err
	}
	info, err := parseGoVersionM(string(out))
	if err != nil {
		return nil, err
	}
	sum, _, err := sha256File(binPath)
	if err != nil {
		return nil, err
	}
	b := newBOM(timestamp, name, version)
	b.Metadata.Component.BOMRef = "bin:" + name + "@" + version + "/" + platform
	b.Metadata.Component.Properties = append(b.Metadata.Component.Properties, bomProp{Name: "release:platform", Value: platform})
	b.Metadata.Component.Hashes = []bomHash{{Alg: "SHA-256", Content: sum}}
	b.Metadata.Component.Properties = append(b.Metadata.Component.Properties, bomProp{Name: "go:main-package", Value: info.Path}, bomProp{Name: "go:main-module", Value: info.Main})
	var keys []string
	for k := range info.Build {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.Metadata.Properties = append(b.Metadata.Properties, bomProp{Name: "go:build:" + k, Value: info.Build[k]})
	}
	b.add(bomComp{Type: "platform", BOMRef: "pkg:golang/go@" + info.GoVersion, Name: "go", Version: info.GoVersion, PURL: "pkg:golang/go@" + info.GoVersion})
	for _, d := range info.Deps {
		c := bomComp{Type: "library", BOMRef: goPURL(d.Path, d.Version), Name: d.Path, Version: d.Version, PURL: goPURL(d.Path, d.Version)}
		if d.Sum != "" {
			c.Properties = append(c.Properties, bomProp{Name: "go:module-sum", Value: d.Sum})
		}
		if d.ReplacedBy != "" {
			c.Properties = append(c.Properties, bomProp{Name: "go:replaced-by", Value: d.ReplacedBy})
		}
		b.add(c)
	}
	return b, nil
}

// --- npm（pnpm-lock.yaml）---------------------------------------------------------------------------------------

type pnpmLock struct {
	LockfileVersion any `yaml:"lockfileVersion"`
	Packages        map[string]struct {
		Resolution struct {
			Integrity string `yaml:"integrity"`
		} `yaml:"resolution"`
	} `yaml:"packages"`
}

// npmComponents 解析 pnpm-lock.yaml 的 packages 段（精确版本 + integrity）。
func npmComponents(lock []byte) ([]bomComp, error) {
	var l pnpmLock
	dec := yaml.NewDecoder(bytes.NewReader(lock))
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("pnpm-lock.yaml 解析失败: %w", err)
	}
	if l.LockfileVersion == nil {
		return nil, fmt.Errorf("pnpm-lock.yaml 缺少 lockfileVersion")
	}
	var out []bomComp
	for key, p := range l.Packages {
		name, ver, ok := splitPnpmKey(key)
		if !ok {
			return nil, fmt.Errorf("无法解析 pnpm packages 键 %q", key)
		}
		purlName := strings.Replace(name, "@", "%40", 1)
		if !strings.HasPrefix(name, "@") {
			purlName = name
		}
		c := bomComp{Type: "library", BOMRef: "pkg:npm/" + purlName + "@" + ver, Name: name, Version: ver, PURL: "pkg:npm/" + purlName + "@" + ver}
		if h := integrityHash(p.Resolution.Integrity); h != nil {
			c.Hashes = []bomHash{*h}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BOMRef < out[j].BOMRef })
	return out, nil
}

// splitPnpmKey 解析 "/name@1.2.3"（v6）、"name@1.2.3"（v9）、"@scope/name@1.2.3(peer)"。
func splitPnpmKey(key string) (name, version string, ok bool) {
	k := strings.TrimPrefix(key, "/")
	if i := strings.Index(k, "("); i >= 0 {
		k = k[:i]
	}
	at := strings.LastIndex(k, "@")
	if at <= 0 {
		return "", "", false
	}
	name, version = k[:at], k[at+1:]
	if name == "" || version == "" || strings.ContainsAny(version, "^~*<>= ") {
		return "", "", false
	}
	return name, version, true
}

func integrityHash(s string) *bomHash {
	alg, b64, ok := strings.Cut(s, "-")
	if !ok {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil
	}
	switch alg {
	case "sha512":
		return &bomHash{Alg: "SHA-512", Content: hex.EncodeToString(raw)}
	case "sha256":
		return &bomHash{Alg: "SHA-256", Content: hex.EncodeToString(raw)}
	}
	return nil
}

// npmSBOM 为前端组件（静态/SSR 包）生成 SBOM；lockPath 不存在时返回 (nil, nil)，由调用方如实记录「无 lockfile」。
func npmSBOM(lockPath, name, version, timestamp string) (*bomDoc, error) {
	b, err := os.ReadFile(lockPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	comps, err := npmComponents(b)
	if err != nil {
		return nil, err
	}
	doc := newBOM(timestamp, name, version)
	doc.Metadata.Component.BOMRef = "app:" + name + "@" + version
	sum := sha256.Sum256(b)
	doc.Metadata.Properties = []bomProp{{Name: "npm:lockfile-sha256", Value: hex.EncodeToString(sum[:])}}
	for _, c := range comps {
		doc.add(c)
	}
	return doc, nil
}

// mergeBOMs 汇总多个包的 SBOM（按 bom-ref 去重）。
func mergeBOMs(timestamp, name, release string, parts map[string]*bomDoc) *bomDoc {
	m := newBOM(timestamp, name, release)
	m.Metadata.Component.BOMRef = "release:" + name + "@" + release
	var names []string
	for n := range parts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := parts[n]
		if p.Metadata.Component != nil {
			sub := *p.Metadata.Component
			sub.Properties = append(append([]bomProp(nil), sub.Properties...), bomProp{Name: "release:package", Value: n})
			m.add(sub)
		}
		for _, c := range p.Components {
			m.add(c)
		}
	}
	return m
}

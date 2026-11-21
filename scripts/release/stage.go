package main

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wuekevin/lingedge/internal/tools/manifest"
	"github.com/wuekevin/lingedge/internal/tools/security"
)

// scanBinaryForTestKeys 在构建出的二进制里查找测试发行公钥（原始 32 字节 / base64 / hex）：发行产物不得内置测试公钥（NFR-1）。
// 与 gates release-keys 对源码目录的检查互补——这里检查的是**真正要发行的字节**。
func scanBinaryForTestKeys(srcRoot, bin string) error {
	b, err := os.ReadFile(filepath.Join(srcRoot, filepath.FromSlash(testPubKeysRel)))
	if err != nil {
		return fmt.Errorf("读取测试公钥指纹表失败: %w", err)
	}
	data, err := os.ReadFile(bin)
	if err != nil {
		return err
	}
	low := bytes.ToLower(data)
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(strings.Fields(l)[0])
		if err != nil || len(raw) != 32 {
			return fmt.Errorf("test-release-pubkeys.txt 含非法行: %q", l)
		}
		if bytes.Contains(data, raw) || bytes.Contains(data, []byte(base64.StdEncoding.EncodeToString(raw))) || bytes.Contains(low, []byte(hex.EncodeToString(raw))) {
			return fmt.Errorf("%s 内含测试发行公钥（%s）：发行产物不得内置测试公钥（NFR-1）", filepath.Base(bin), strings.TrimSpace(l))
		}
	}
	return nil
}

// stagePackage 组装一个包的 stage 树：payload/、META/package.json、META/SHA256SUMS（SHA256SUMS.sig 由离线签名后在 finalize 放入）。
func (o *buildOpts) stagePackage(c compDef, plat, buildDir string, commitTime time.Time, rec *buildRecord) (*planPackage, *bomDoc, []linkRecord, error) {
	id := c.Name + "-" + o.version + "-" + plat
	stageRel := "stage/" + id
	stageDir := filepath.Join(o.out, filepath.FromSlash(stageRel))
	payload := filepath.Join(stageDir, "payload")
	if err := os.MkdirAll(payload, 0o755); err != nil {
		return nil, nil, nil, err
	}
	ts := commitTime.Format(time.RFC3339)
	parts := map[string]*bomDoc{}
	var links []linkRecord

	for _, bn := range c.Binaries {
		src := filepath.Join(buildDir, plat, bn)
		if err := copyFile(src, filepath.Join(payload, "bin", bn), 0o755); err != nil {
			return nil, nil, nil, err
		}
		bom, err := goSBOM(src, bn, o.version, plat, ts)
		if err != nil {
			return nil, nil, nil, err
		}
		parts[bn+"/"+plat] = bom
	}
	for _, f := range c.Files {
		src := filepath.Join(o.src, filepath.FromSlash(f.Src))
		fi, err := os.Lstat(src)
		if err != nil {
			return nil, nil, nil, err
		}
		if !fi.Mode().IsRegular() {
			return nil, nil, nil, fmt.Errorf("%s 必须是普通文件（不接受符号链接）", f.Src)
		}
		if err := copyFile(src, filepath.Join(payload, filepath.FromSlash(f.Dst)), 0o644); err != nil {
			return nil, nil, nil, err
		}
	}
	if c.Name == "openresty" {
		var err error
		if links, err = copyTree(o.openrestyTrees[plat], payload, ""); err != nil {
			return nil, nil, nil, fmt.Errorf("OpenResty 目录 %s: %w", o.openrestyTrees[plat], err)
		}
	}

	// SBOM：Go 包用二进制的 SBOM 合并；无 Go 二进制的包（OpenResty）记录目录树摘要。
	var bom *bomDoc
	if len(parts) > 0 {
		bom = mergeBOMs(ts, "lingedge-"+c.Name, o.version, parts)
	} else {
		files, err := listPayload(payload)
		if err != nil {
			return nil, nil, nil, err
		}
		var sb strings.Builder
		for _, f := range files {
			h, _, err := sha256File(f.Path)
			if err != nil {
				return nil, nil, nil, err
			}
			fmt.Fprintf(&sb, "%s  %s\n", h, f.Rel)
		}
		bom = newBOM(ts, "lingedge-"+c.Name, o.version)
		bom.Metadata.Component.BOMRef = "pkg:" + c.Name + "@" + o.version + "/" + plat
		bom.Metadata.Component.Properties = []bomProp{{Name: "release:platform", Value: plat}, {Name: "release:payload-tree-sha256", Value: sha256Hex([]byte(sb.String()))}}
		bom.Metadata.Component.Hashes = nil
		bom.add(bomComp{Type: "application", BOMRef: bom.Metadata.Component.BOMRef + "#content", Name: c.Name, Version: o.version,
			Properties: []bomProp{{Name: "release:note", Value: "第三方发行目录（来源与校验和见 manifests/）；本 SBOM 只记录目录树摘要，不枚举其内部组件"}}})
	}
	sbom, err := bom.finish()
	if err != nil {
		return nil, nil, nil, err
	}
	sbomPath := filepath.Join(payload, "share", "lingedge-sbom.cdx.json")
	if _, err := os.Lstat(sbomPath); err == nil {
		return nil, nil, nil, fmt.Errorf("payload 已含 share/lingedge-sbom.cdx.json，与流水线 SBOM 冲突")
	}
	if err := os.MkdirAll(filepath.Dir(sbomPath), 0o755); err != nil {
		return nil, nil, nil, err
	}
	if err := os.WriteFile(sbomPath, sbom, 0o644); err != nil {
		return nil, nil, nil, err
	}

	if err := scanTreeForPrivateKeys(payload); err != nil {
		return nil, nil, nil, fmt.Errorf("payload 含私钥材料: %w", err)
	}
	files, err := listPayload(payload)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(files) == 0 {
		return nil, nil, nil, fmt.Errorf("payload 为空")
	}
	var sums strings.Builder
	var total int64
	for _, f := range files {
		if _, err := security.CleanEntryName("payload/"+f.Rel, false, security.Limits{}); err != nil {
			return nil, nil, nil, fmt.Errorf("payload 路径 %q 不能通过 SafeExtract: %w", f.Rel, err)
		}
		h, n, err := sha256File(f.Path)
		if err != nil {
			return nil, nil, nil, err
		}
		fmt.Fprintf(&sums, "%s  payload/%s\n", h, f.Rel)
		total += n
	}
	meta := manifest.PackageMeta{Component: c.Name, Version: o.version, Variant: c.variant(), Platform: plat, BuiltAt: ts,
		ConfigSchemaVersion: c.ConfigSchemaVersion, FilesCount: len(files), UnpackedSize: total}
	mb, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, nil, nil, err
	}
	mb = append(mb, '\n')
	if _, err := manifest.ParsePackageMeta(mb); err != nil {
		return nil, nil, nil, fmt.Errorf("生成的 META/package.json 未通过 Tools 的校验: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(stageDir, "META"), 0o755); err != nil {
		return nil, nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(stageDir, "META", "package.json"), mb, 0o644); err != nil {
		return nil, nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(stageDir, "META", "SHA256SUMS"), []byte(sums.String()), 0o644); err != nil {
		return nil, nil, nil, err
	}
	sort.Slice(links, func(i, j int) bool { return links[i].Path < links[j].Path })
	return &planPackage{ID: id, Component: c.Name, Version: o.version, Variant: c.variant(), Platform: plat, Stage: stageRel,
		Filename: packageFilename(c.Name, c.variant(), o.version, plat), FilesCount: len(files), UnpackedSize: total}, bom, links, nil
}

package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 静态前端发行包（Story 1.40，tools-design §9.2/§11.3）：
//   - 组件 admin-web / console / docs / status / website(static)，平台 any（与 CPU 架构无关）；
//   - 包布局 payload/public/**（站点根）+ payload/share/lingedge-sbom.cdx.json；Tools 解压到 /opt/lingedge/<c>/releases/<v>/；
//   - 流水线为可压缩资源生成确定性的预压缩 .gz（配合 Nginx gzip_static）；
//   - 前端目录必须是真实的生产构建（pnpm -C frontend run build，--frozen-lockfile 安装）：流水线再跑一遍 mock 入口检查，并把 pnpm-lock.yaml 的哈希写入 SBOM 与 build-record。
// 目录由 -frontend-dist <组件>=<目录> 提供（与 -openresty-tree 同一模式）：没有提供的组件在清单里如实保持 unreleased，不发布空壳包。

// frontendComponents 是可经 -frontend-dist 提供的静态前端组件及其变体。
var frontendComponents = map[string]string{
	"admin-web": "default", "console": "default", "docs": "default", "status": "default", "website": "static",
}

type frontendRecord struct {
	Component     string `json:"component"`
	Variant       string `json:"variant"`
	Files         int    `json:"files"`
	Precompressed int    `json:"precompressed"`
	TreeSHA256    string `json:"tree_sha256"`
	MockCheck     string `json:"mock_check"`
}

const (
	pnpmLockRel     = "frontend/pnpm-lock.yaml"
	mockCheckRel    = "frontend/scripts/check-mock.mjs"
	minGzipBytes    = 256
	frontendMaxDist = 1 << 30
)

// compressible 是值得预压缩的文本类资源（已压缩的图片/字体/wasm 不在其中）。
var compressible = map[string]bool{
	".html": true, ".js": true, ".mjs": true, ".css": true, ".svg": true, ".json": true, ".txt": true, ".xml": true, ".map": true, ".webmanifest": true,
}

// precompress 为 publicDir 下可压缩的文件生成 <file>.gz（gzip 9 级、头部时间为零 → 同输入逐字节确定）；
// 已有同名 .gz、压缩后不更小、小于 minGzipBytes 的不生成。返回生成数。
func precompress(publicDir string) (int, error) {
	var files []string
	err := filepath.WalkDir(publicDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() && compressible[strings.ToLower(filepath.Ext(p))] {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	sort.Strings(files)
	n := 0
	for _, p := range files {
		if _, err := os.Lstat(p + ".gz"); err == nil {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return n, err
		}
		if len(b) < minGzipBytes {
			continue
		}
		var buf bytes.Buffer
		zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return n, err
		}
		if _, err := zw.Write(b); err != nil {
			return n, err
		}
		if err := zw.Close(); err != nil {
			return n, err
		}
		if buf.Len() >= len(b) {
			continue
		}
		if err := os.WriteFile(p+".gz", buf.Bytes(), 0o644); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// treeDigest 返回目录树的摘要（相对路径 + 内容 SHA256 的排序清单再取 SHA256）与文件数。
func treeDigest(dir string) (string, int, error) {
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		h, _, err := sha256File(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		lines = append(lines, h+"  "+filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return "", 0, err
	}
	sort.Strings(lines)
	s := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\n"))
	return hex.EncodeToString(s[:]), len(lines), nil
}

// pnpmLockSHA256 读取源仓库的 pnpm-lock.yaml 哈希：前端包必须可追溯到锁文件（供应链：依赖精确版本由锁文件固定）。
func pnpmLockSHA256(srcRoot string) (string, error) {
	h, _, err := sha256File(filepath.Join(srcRoot, filepath.FromSlash(pnpmLockRel)))
	if err != nil {
		return "", fmt.Errorf("前端发行包必须可追溯到 %s: %w", pnpmLockRel, err)
	}
	return h, nil
}

// checkFrontendDist 校验 -frontend-dist 目录：存在 index.html；跑 frontend/scripts/check-mock.mjs（生产产物不得含 mock 入口，NFR-1）。
// node 缺失视为失败：不能因为工具缺席就放过检查。
func checkFrontendDist(srcRoot, comp, dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	st, err := os.Stat(filepath.Join(abs, "index.html"))
	if err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("-frontend-dist %s=%s：缺少 index.html（应为前端生产构建的输出目录）", comp, dir)
	}
	if out, err := execCmd(srcRoot, nil, "node", filepath.Join(srcRoot, filepath.FromSlash(mockCheckRel)), "--dist", abs); err != nil {
		return fmt.Errorf("-frontend-dist %s=%s 未通过生产产物 mock 入口检查：%w\n%s", comp, dir, err, out)
	}
	return nil
}

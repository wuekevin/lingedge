package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/security"
)

// linkRecord 记录一条在构建期被消除的符号链接（写入 build-record.json，供发行负责人审阅）。
type linkRecord struct {
	Path   string `json:"path"`   // 链接在 payload 里的相对路径
	Target string `json:"target"` // 解析后的真实目标，相对源树根
	Kind   string `json:"kind"`   // file | dir
}

// treeCopier 把一棵源目录树（例如 OpenResty 发行目录）物化到 payload/：**符号链接在构建期消除**（Story 1.34 AC6）。
//
// 为什么：Tools 的 SafeExtract 拒绝包内的任何链接（tools-design §18.1、G-4），且「不放宽」；而 OpenResty 之类的发行目录里
// 到处是 `libfoo.so -> libfoo.so.1`、`bin/openresty -> nginx` 这样的链接。因此流水线在组装 payload 时把每个链接替换为
// 它所指内容的**普通副本**（文件链接 → 复制文件；目录链接 → 复制整棵子树），并拒绝一切无法安全消除的情形：
//   - 目标在源树之外（逃逸）、悬空、循环（指回祖先目录）、链接链解析失败；
//   - 设备/FIFO/套接字等特殊文件；
//   - 条目名不能通过 SafeExtract 同款的路径规则（长度/深度/控制字符/百分号编码/仅大小写不同的重名）。
//
// 硬链接天然被复制为互相独立的普通文件。权限位归一化：任一可执行位 → 0755，否则 0644（包内权限位本来就被安装器忽略）。
type treeCopier struct {
	root     string // 源树根的 realpath
	dst      string // payload 目录（绝对路径）
	prefix   string // 在 payload 内的前缀（相对，斜杠分隔，可为空）
	links    []linkRecord
	files    int
	bytes    int64
	maxFiles int
	maxBytes int64
}

const (
	defaultMaxFiles = 40_000
	defaultMaxBytes = int64(1) << 30
	maxTreeDepth    = 14 // 加上 "payload/" 前缀后仍在 SafeExtract 缺省深度 16 之内
)

func copyTree(srcRoot, payloadDir, prefix string) ([]linkRecord, error) {
	real, err := filepath.EvalSymlinks(srcRoot)
	if err != nil {
		return nil, fmt.Errorf("源目录 %s: %w", srcRoot, err)
	}
	fi, err := os.Stat(real)
	if err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("源目录 %s 不是目录", srcRoot)
	}
	c := &treeCopier{root: real, dst: payloadDir, prefix: strings.Trim(prefix, "/"), maxFiles: defaultMaxFiles, maxBytes: defaultMaxBytes}
	if err := c.dir(real, "", []string{real}, 0); err != nil {
		return nil, err
	}
	sort.Slice(c.links, func(i, j int) bool { return c.links[i].Path < c.links[j].Path })
	return c.links, nil
}

func (c *treeCopier) rel(parts ...string) string {
	var p []string
	if c.prefix != "" {
		p = append(p, c.prefix)
	}
	for _, s := range parts {
		if s != "" {
			p = append(p, s)
		}
	}
	return strings.Join(p, "/")
}

func (c *treeCopier) dir(realDir, relDst string, stack []string, depth int) error {
	if depth > maxTreeDepth {
		return fmt.Errorf("目录 %s 嵌套超过 %d 层（SafeExtract 深度上限）", realDir, maxTreeDepth)
	}
	ents, err := os.ReadDir(realDir)
	if err != nil {
		return err
	}
	lower := map[string]string{}
	if err := os.MkdirAll(filepath.Join(c.dst, filepath.FromSlash(c.rel(relDst))), 0o755); err != nil {
		return err
	}
	for _, e := range ents {
		name := e.Name()
		rel := joinRel(relDst, name)
		if _, err := security.CleanEntryName("payload/"+c.rel(rel), e.IsDir(), security.Limits{}); err != nil {
			return fmt.Errorf("条目 %q 不能通过 SafeExtract 的路径规则: %w", rel, err)
		}
		if prev, dup := lower[strings.ToLower(name)]; dup {
			return fmt.Errorf("目录 %q 内 %q 与 %q 仅大小写不同（SafeExtract 拒绝）", relDst, name, prev)
		}
		lower[strings.ToLower(name)] = name

		path := filepath.Join(realDir, name)
		li, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case li.Mode()&os.ModeSymlink != 0:
			target, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("符号链接 %q 无法解析（悬空或循环）: %w", rel, err)
			}
			if !inside(c.root, target) {
				return fmt.Errorf("符号链接 %q 指向源树之外（%s），拒绝消除（逃逸）", rel, target)
			}
			ti, err := os.Stat(target)
			if err != nil {
				return err
			}
			kind := "file"
			if ti.IsDir() {
				kind = "dir"
			}
			rt, _ := filepath.Rel(c.root, target)
			c.links = append(c.links, linkRecord{Path: c.rel(rel), Target: filepath.ToSlash(rt), Kind: kind})
			switch {
			case ti.IsDir():
				for _, anc := range stack {
					if inside(target, anc) { // 目标等于或包含当前所在目录（祖先）：复制它会无限嵌套
						return fmt.Errorf("符号链接 %q 形成目录循环（目标 %s 包含它自己所在的目录）", rel, target)
					}
				}
				if err := c.dir(target, rel, append(stack, target), depth+1); err != nil {
					return err
				}
			case ti.Mode().IsRegular():
				if err := c.file(target, rel, ti); err != nil {
					return err
				}
			default:
				return fmt.Errorf("符号链接 %q 指向特殊文件", rel)
			}
		case li.IsDir():
			if err := c.dir(path, rel, append(stack, path), depth+1); err != nil {
				return err
			}
		case li.Mode().IsRegular():
			if err := c.file(path, rel, li); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%q 是特殊文件（设备/FIFO/套接字），不能进入发行包", rel)
		}
	}
	return nil
}

func (c *treeCopier) file(src, rel string, fi fs.FileInfo) error {
	c.files++
	c.bytes += fi.Size()
	if c.files > c.maxFiles || c.bytes > c.maxBytes {
		return fmt.Errorf("源树超过限额（%d 个文件 / %d 字节）", c.maxFiles, c.maxBytes)
	}
	mode := fs.FileMode(0o644)
	if fi.Mode()&0o111 != 0 {
		mode = 0o755
	}
	return copyFile(src, filepath.Join(c.dst, filepath.FromSlash(c.rel(rel))), mode)
}

func joinRel(a, b string) string {
	if a == "" {
		return b
	}
	return a + "/" + b
}

// inside 报告 p 是否在 root 之内（含 root 自身）。两者都应是 realpath。
func inside(root, p string) bool {
	r, err := filepath.Rel(root, p)
	return err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}

func copyFile(src, dst string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// payloadFile 是 payload/ 下的一个普通文件。
type payloadFile struct {
	Rel  string // 相对 payload/，斜杠分隔
	Path string
	Size int64
	Exec bool
}

// listPayload 遍历 payload 目录：只允许普通文件与目录；**任何符号链接/特殊文件都是错误**（构建期消除的最终保证）。
func listPayload(dir string) ([]payloadFile, error) {
	var out []payloadFile
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("payload 中残留符号链接 %q（必须在构建期消除）", rel)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("payload 中的 %q 不是普通文件", rel)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, payloadFile{Rel: rel, Path: p, Size: fi.Size(), Exec: fi.Mode()&0o111 != 0})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	return out, err
}

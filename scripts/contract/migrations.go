package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// 迁移哈希清单（Story 1.3 AC4）：migrations/CHECKSUMS 每行 `<sha256 十六进制>  <文件名>`，`#` 开头为注释。
// 清单内的迁移视为「已发布」：任何改动或删除都使 migrations-check 失败；已发布的迁移只能新增后续迁移来修正。
const checksumsName = "CHECKSUMS"

type checksumEntry struct {
	Name string
	Hash string
}

var checksumLineRe = regexp.MustCompile(`^([0-9a-f]{64})  (\S+)$`)

func parseChecksums(data string) ([]checksumEntry, error) {
	var out []checksumEntry
	seen := map[string]bool{}
	for i, line := range strings.Split(data, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := checksumLineRe.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("%s 第 %d 行格式错误（应为 `<sha256>  <文件名>`）: %q", checksumsName, i+1, line)
		}
		if seen[m[2]] {
			return nil, fmt.Errorf("%s 第 %d 行重复登记 %s", checksumsName, i+1, m[2])
		}
		seen[m[2]] = true
		out = append(out, checksumEntry{Name: m[2], Hash: m[1]})
	}
	return out, nil
}

// isMigrationFile 判断目录内文件是否为迁移文件（排除清单本身与占位文件）。
func isMigrationFile(name string) bool {
	// embed.go 是把迁移目录嵌入二进制的 Go 文件（Story 1.6），不是迁移。
	if name == checksumsName || name == "embed.go" || strings.HasPrefix(name, ".") {
		return false
	}
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".sql" || ext == ".go"
}

// listMigrations 递归列出目录下全部迁移文件（相对路径、斜杠分隔、排序）；子目录里的迁移同样受检，符号链接不跟随也不放过（按普通文件名纳入）。
func listMigrations(dir string) ([]string, error) {
	var names []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if isMigrationFile(d.Name()) {
			rel, rerr := filepath.Rel(dir, p)
			if rerr != nil {
				return rerr
			}
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(names)
	return names, err
}

func sha256File(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// classHookRe 是「新增迁移须带合法 lingedge:class 标注」的检查（Story 1.3 AC4，Story 1.6 收紧）：
// 文件第一行必须是 `-- lingedge:class=reversible|expand|irreversible`。其余规则（命名、Down、幂等、授权、DDL 预检）由
// internal/platform/migrate 的迁移 lint 执行（make migrate-test），本工具只守住「已发布迁移只追加」与标注这一底线。
var classHookRe = regexp.MustCompile(`\A-- lingedge:class=(reversible|expand|irreversible)[ \t]*\r?\n`)

// checkMigrations 校验目录与清单：已发布迁移哈希一致、未登记迁移带 class 标注、清单相对基线只增不改。
// baseChecksums 为基线 ref 中的清单内容（nil 表示无基线，跳过追加性检查）。
func checkMigrations(dir string, baseChecksums []byte) ([]finding, error) {
	var fs []finding
	raw, err := os.ReadFile(filepath.Join(dir, checksumsName))
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", filepath.Join(dir, checksumsName), err)
	}
	entries, err := parseChecksums(string(raw))
	if err != nil {
		return []finding{{"migrations", filepath.Join(dir, checksumsName), err.Error()}}, nil
	}
	listed := map[string]bool{}
	for _, e := range entries {
		listed[e.Name] = true
		got, herr := sha256File(filepath.Join(dir, e.Name))
		switch {
		case herr != nil:
			fs = append(fs, finding{"migrations", e.Name, "已发布迁移缺失（不得删除已发布迁移）"})
		case got != e.Hash:
			fs = append(fs, finding{"migrations", e.Name, fmt.Sprintf("已发布迁移被改动（清单 %s…，实际 %s…）；请新增后续迁移而非修改", e.Hash[:12], got[:12])})
		}
	}
	names, err := listMigrations(dir)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		if listed[n] {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, n))
		if rerr != nil {
			return nil, rerr
		}
		if !classHookRe.Match(b) {
			fs = append(fs, finding{"migrations", n, "新增迁移第一行必须是 `-- lingedge:class=reversible|expand|irreversible`（完整分类 lint 见 make migrate-test）"})
		}
	}
	if baseChecksums != nil {
		baseEntries, berr := parseChecksums(string(baseChecksums))
		if berr != nil {
			return nil, fmt.Errorf("基线清单无法解析: %w", berr)
		}
		cur := map[string]string{}
		for _, e := range entries {
			cur[e.Name] = e.Hash
		}
		for _, be := range baseEntries {
			h, ok := cur[be.Name]
			switch {
			case !ok:
				fs = append(fs, finding{"migrations", be.Name, "清单中的已发布条目被删除（清单只允许追加）"})
			case h != be.Hash:
				fs = append(fs, finding{"migrations", be.Name, "清单中的已发布条目哈希被改写（清单只允许追加）"})
			}
		}
	}
	return fs, nil
}

// sealMigrations 把未登记的迁移文件追加到清单（只追加，绝不改写已有行）。返回新登记的文件名。
func sealMigrations(dir string) ([]string, error) {
	path := filepath.Join(dir, checksumsName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	entries, err := parseChecksums(string(raw))
	if err != nil {
		return nil, err
	}
	listed := map[string]bool{}
	for _, e := range entries {
		listed[e.Name] = true
	}
	names, err := listMigrations(dir)
	if err != nil {
		return nil, err
	}
	var add []string
	for _, n := range names {
		if !listed[n] {
			add = append(add, n)
		}
	}
	if len(add) == 0 {
		return nil, nil
	}
	out := string(raw)
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	for _, n := range add {
		b, rerr := os.ReadFile(filepath.Join(dir, n))
		if rerr != nil {
			return nil, rerr
		}
		if !classHookRe.Match(b) {
			return nil, fmt.Errorf("%s 第一行不是合法的 `-- lingedge:class=…` 标注，不能登记为已发布", n)
		}
		h, _ := sha256File(filepath.Join(dir, n))
		out += h + "  " + n + "\n"
	}
	return add, os.WriteFile(path, []byte(out), 0o644)
}

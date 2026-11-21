package probe

import (
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/baota"
)

// Node.js 在非固定目录的识别（nvm/fnm/volta/n/asdf 与宝塔 Node 项目管理器）。
//
// 只读目录枚举，**不执行任何这些路径下的二进制**：版本取自目录名，并用 `include/node/node_version.h`
// （官方发行包自带的元数据）交叉校验；二者不一致时以头文件为准并在 VersionSource 里写明。
// 目录项必须是真实目录（不跟随符号链接），`bin/node` 必须是普通可执行文件且 group/other 不可写（与 safeexec 的二进制检查同标准），
// 全部经 baota.FS（可用临时目录树测试，也受 readGuard 约束）。

// NodeInstall 是一个识别到的 Node.js 安装。
type NodeInstall struct {
	Manager       string `json:"manager"` // nvm | fnm | volta | n | asdf | baota
	Version       string `json:"version"` // 带 v 前缀，如 v20.11.0
	Bin           string `json:"bin"`     // <dir>/bin/node（逻辑路径）
	Dir           string `json:"dir"`
	VersionSource string `json:"version_source"` // dirname | dirname+node_version.h | node_version.h
}

type nodeLayout struct {
	manager string
	// root 是版本目录的父目录（相对 home，或以 / 开头的绝对路径）；installRel 是版本目录下到安装前缀（含 bin/、include/）的相对路径。
	root       string
	installRel string
	home       bool
}

var nodeLayouts = []nodeLayout{
	{manager: "nvm", root: ".nvm/versions/node", home: true},
	{manager: "fnm", root: ".fnm/node-versions", installRel: "installation", home: true},
	{manager: "fnm", root: ".local/share/fnm/node-versions", installRel: "installation", home: true},
	{manager: "fnm", root: "Library/Application Support/fnm/node-versions", installRel: "installation", home: true},
	{manager: "volta", root: ".volta/tools/image/node", home: true},
	{manager: "n", root: "n/n/versions/node", home: true},
	{manager: "asdf", root: ".asdf/installs/nodejs", home: true},
	{manager: "n", root: "/usr/local/n/versions/node"},
	{manager: "baota", root: "/www/server/nodejs"},
}

var (
	reNodeVerDir = regexp.MustCompile(`^v?(\d{1,3})\.(\d{1,3})\.(\d{1,4})$`)
	reNodeHdr    = regexp.MustCompile(`(?m)^#define\s+NODE_(MAJOR|MINOR|PATCH)_VERSION\s+(\d{1,4})\s*$`)
)

const (
	maxNodeVersionsPerRoot = 64
	maxNodeInstalls        = 256
	maxNodeHeader          = 64 << 10
)

// ParseNodeVersionHeader 解析 node_version.h，返回 "vMAJOR.MINOR.PATCH"。
func ParseNodeVersionHeader(b []byte) (string, bool) {
	got := map[string]string{}
	for _, m := range reNodeHdr.FindAllSubmatch(b, -1) {
		got[string(m[1])] = string(m[2])
	}
	if got["MAJOR"] == "" || got["MINOR"] == "" || got["PATCH"] == "" {
		return "", false
	}
	return "v" + got["MAJOR"] + "." + got["MINOR"] + "." + got["PATCH"], true
}

// FindNodeInstalls 枚举 homes 下与固定位置的 Node.js 安装，按版本从高到低排序（同版本按路径）。永不 panic；单项失败直接跳过。
func FindNodeInstalls(fsys baota.FS, homes []string) []NodeInstall {
	var out []NodeInstall
	seen := map[string]bool{}
	scan := func(l nodeLayout, root string) {
		es, err := fsys.ReadDir(root)
		if err != nil {
			return
		}
		sort.Slice(es, func(i, j int) bool { return es[i].Name() < es[j].Name() })
		n := 0
		for _, e := range es {
			if n >= maxNodeVersionsPerRoot || len(out) >= maxNodeInstalls {
				return
			}
			m := reNodeVerDir.FindStringSubmatch(e.Name())
			if m == nil || !e.IsDir() { // IsDir 对符号链接为 false：不跟随
				continue
			}
			n++
			dir := path.Join(root, e.Name(), l.installRel) // fnm 的版本目录下还有 installation/
			bin := path.Join(dir, "bin", "node")
			if seen[bin] {
				continue
			}
			fi, err := fsys.Lstat(bin)
			if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 || fi.Mode().Perm()&0o022 != 0 {
				continue
			}
			ver := "v" + m[1] + "." + m[2] + "." + m[3]
			src := "dirname"
			if h, err := fsys.ReadFile(path.Join(dir, "include/node/node_version.h"), maxNodeHeader); err == nil {
				if hv, ok := ParseNodeVersionHeader(h); ok {
					if hv == ver {
						src = "dirname+node_version.h"
					} else {
						ver, src = hv, "node_version.h"
					}
				}
			}
			seen[bin] = true
			out = append(out, NodeInstall{Manager: l.manager, Version: ver, Bin: bin, Dir: dir, VersionSource: src})
		}
	}
	for _, l := range nodeLayouts {
		if l.home {
			for _, h := range homes {
				if path.IsAbs(h) && path.Clean(h) == h {
					scan(l, path.Join(h, l.root))
				}
			}
		} else {
			scan(l, l.root)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if c := cmpNodeVersion(out[i].Version, out[j].Version); c != 0 {
			return c > 0
		}
		return out[i].Bin < out[j].Bin
	})
	return out
}

func cmpNodeVersion(a, b string) int {
	pa, pb := nodeVerParts(a), nodeVerParts(b)
	for i := range pa {
		if pa[i] != pb[i] {
			if pa[i] > pb[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}

func nodeVerParts(v string) [3]int {
	var p [3]int
	for i, s := range strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3) {
		p[i], _ = strconv.Atoi(s)
	}
	return p
}

// nodeHomes 给出要扫描的用户主目录：调用方指定的；否则 /root、/home/* （经 FS 列目录，最多 64 个）与当前用户主目录。
func nodeHomes(fsys baota.FS, explicit []string, current string) []string {
	if explicit != nil {
		return explicit
	}
	var hs []string
	seen := map[string]bool{}
	add := func(h string) {
		if path.IsAbs(h) && path.Clean(h) == h && !seen[h] {
			seen[h] = true
			hs = append(hs, h)
		}
	}
	add("/root")
	if es, err := fsys.ReadDir("/home"); err == nil {
		n := 0
		for _, e := range es {
			if e.IsDir() && n < 64 { // IsDir 对符号链接为 false：不跟随
				add("/home/" + e.Name())
				n++
			}
		}
	}
	if current != "" {
		add(path.Clean(current))
	}
	return hs
}

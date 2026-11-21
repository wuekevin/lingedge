package baota

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// 受控的 nginx include 跟随（只读）。
//
// 宝塔/系统 nginx 的 server 块通常不在 nginx.conf 里，而是经 `include /www/server/panel/vhost/nginx/*.conf;`、
// `include /etc/nginx/conf.d/*.conf;`，以及 server 内的 `include enable-php-74.conf;`、rewrite/ssl 片段组织。
// 跟随规则（全部可测，经 FS 抽象）：
//   - 只读；只允许白名单根目录之下的文件（DefaultIncludeRoots，可经 IncludeOptions.Roots 替换）；
//   - 相对路径相对 nginx 前缀（主配置文件所在目录，与 nginx 的 conf_prefix 一致）解析，而不是相对「包含者」的目录；
//   - glob 支持任意路径段里的 * ? [..]（不支持 **），字面前缀必须落在白名单根内，展开有数量上限；
//   - 防环：以「当前 include 链」为已访问集合（同一文件被多个 server 重复 include 是正常的，不算环）；
//   - 符号链接：路径的每一级（含目录级）都逐级解析，每一跳的目标与最终文件都必须仍在白名单根内；只读普通文件；
//   - 不读 readGuard 禁止的文件（检查 include 路径与符号链接解析后的真实路径两处）；
//   - 上限：文件数、字节数、include 嵌套深度、展开后节点数；超限停止展开并记 warning；
//   - 畸形/不存在的 include 不 panic，记 warning（Report.Notes）。

// DefaultIncludeRoots 是默认的 include 白名单根目录。
var DefaultIncludeRoots = []string{
	"/www/server/nginx/conf",
	"/www/server/panel/vhost",
	"/usr/local/openresty/nginx/conf",
	"/etc/nginx",
	"/usr/local/etc/nginx",
	"/opt/homebrew/etc/nginx",
}

// IncludeOptions 配置 Includer。零值取默认。
type IncludeOptions struct {
	Roots    []string // 白名单根（规范绝对路径）；空取 DefaultIncludeRoots
	MaxFiles int      // 最多读取的不同文件数；默认 512
	MaxDepth int      // include 最大嵌套深度；默认 16
	MaxGlob  int      // 单个 glob 最多匹配的文件数；默认 256
	MaxBytes int64    // 累计最多读取的字节数；默认 8 MiB
	MaxNodes int      // 展开后指令节点数上限；默认 500000
	MaxHops  int      // 一次路径解析最多跟随的符号链接数；默认 16
}

func (o IncludeOptions) withDefaults() IncludeOptions {
	if len(o.Roots) == 0 {
		o.Roots = DefaultIncludeRoots
	}
	if o.MaxFiles <= 0 {
		o.MaxFiles = 512
	}
	if o.MaxDepth <= 0 {
		o.MaxDepth = 16
	}
	if o.MaxGlob <= 0 {
		o.MaxGlob = 256
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 8 << 20
	}
	if o.MaxNodes <= 0 {
		o.MaxNodes = 500000
	}
	if o.MaxHops <= 0 {
		o.MaxHops = 16
	}
	return o
}

const maxIncludeWarnings = 24

type loaded struct {
	tree  []directive
	nodes int
}

// Includer 在一次探测内共享预算（文件数/字节数/节点数）与缓存；非并发安全。
type Includer struct {
	fs     FS
	o      IncludeOptions
	roots  []string
	prefix string

	files, nodes int
	bytes        int64
	cache        map[string]*loaded // 规范路径 → 已解析（nil 表示读取失败，不重复读取与重复告警）
	bad          map[string]bool    // 解析出错（畸形）的文件
	warns        []string
	warnSeen     map[string]bool
	dropped      int
}

// NewIncluder 创建 Includer。fsys 为 nil 时取 OSFS{Root:"/"}。
func NewIncluder(fsys FS, o IncludeOptions) *Includer {
	if fsys == nil {
		fsys = OSFS{Root: "/"}
	}
	o = o.withDefaults()
	in := &Includer{fs: fsys, o: o, cache: map[string]*loaded{}, bad: map[string]bool{}, warnSeen: map[string]bool{}}
	for _, r := range o.Roots {
		if r = path.Clean(r); path.IsAbs(r) && r != "/" { // 根目录 "/" 等于没有白名单，拒绝
			in.roots = append(in.roots, r)
		}
	}
	return in
}

// SetPrefix 设置相对 include 的解析前缀（nginx 的 conf_prefix：主配置文件所在目录）。
func (in *Includer) SetPrefix(p string) {
	if p = path.Clean(p); path.IsAbs(p) {
		in.prefix = p
	} else {
		in.prefix = ""
	}
}

// Stats 返回已读取的文件数与字节数（供测试与诊断）。
func (in *Includer) Stats() (files int, bytes int64) { return in.files, in.bytes }

// Warnings 返回累计的 warning（已去重并限量；超出部分折叠成一条汇总）。
func (in *Includer) Warnings() []string {
	out := append([]string(nil), in.warns...)
	if in.dropped > 0 {
		out = append(out, fmt.Sprintf("另有 %d 条 include 警告被省略", in.dropped))
	}
	return out
}

func (in *Includer) warn(f string, a ...any) {
	m := fmt.Sprintf(f, a...)
	if in.warnSeen[m] {
		return
	}
	in.warnSeen[m] = true
	if len(in.warns) >= maxIncludeWarnings {
		in.dropped++
		return
	}
	in.warns = append(in.warns, m)
}

func short(p string) string {
	if len(p) > 160 {
		p = p[:160] + "…"
	}
	return fmt.Sprintf("%q", p)
}

func under(root, p string) bool { return p == root || strings.HasPrefix(p, root+"/") }

func (in *Includer) allowed(p string) bool {
	for _, r := range in.roots {
		if under(r, p) {
			return true
		}
	}
	return false
}

// canon 逐级解析 p 的全部符号链接（含目录级），返回真实逻辑路径。每一跳的目标都必须在白名单根内。
func (in *Includer) canon(p string) (string, error) {
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	resolved := "/"
	hops := 0
	for i := 0; i < len(parts); i++ {
		if parts[i] == "" {
			continue
		}
		next := path.Join(resolved, parts[i])
		fi, err := in.fs.Lstat(next)
		if err != nil {
			return "", err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			if hops++; hops > in.o.MaxHops {
				return "", errors.New("符号链接层数过多")
			}
			t, err := in.fs.Readlink(next)
			if err != nil {
				return "", err
			}
			if !path.IsAbs(t) {
				t = path.Join(resolved, t)
			}
			t = path.Clean(t)
			if !in.allowed(t) {
				return "", fmt.Errorf("符号链接 %s 的目标 %s 越出 include 白名单根目录", short(next), short(t))
			}
			parts = append(strings.Split(strings.TrimPrefix(t, "/"), "/"), parts[i+1:]...)
			resolved, i = "/", -1
			continue
		}
		resolved = next
	}
	return resolved, nil
}

// glob 展开 pattern（规范绝对路径）。返回按字典序排序的候选路径（未经 canon/roots 检查）与是否含通配。
func (in *Includer) glob(pattern string) (matches []string, isGlob bool, err error) {
	comps := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	firstMeta := -1
	for i, c := range comps {
		if strings.ContainsAny(c, "*?[\\") {
			firstMeta = i
			break
		}
	}
	if firstMeta < 0 {
		return []string{pattern}, false, nil
	}
	lit := "/" + strings.Join(comps[:firstMeta], "/")
	lit = path.Clean(lit)
	if !in.allowed(lit) {
		return nil, true, fmt.Errorf("通配 include %s 的固定前缀不在白名单根目录内", short(pattern))
	}
	cur := []string{lit}
	for i := firstMeta; i < len(comps); i++ {
		c := comps[i]
		last := i == len(comps)-1
		var nxt []string
		for _, dir := range cur {
			cd, err := in.canon(dir) // 列目录前先确认真实目录仍在白名单内（目录符号链接不越界）
			if err != nil {
				continue
			}
			if !in.allowed(cd) {
				continue
			}
			es, err := in.fs.ReadDir(cd)
			if err != nil {
				continue
			}
			names := make([]string, 0, len(es))
			for _, e := range es {
				if !last && !e.IsDir() && e.Type()&fs.ModeSymlink == 0 {
					continue
				}
				names = append(names, e.Name())
			}
			sort.Strings(names)
			for _, n := range names {
				if strings.HasPrefix(n, ".") && !strings.HasPrefix(c, ".") { // glob(3)：* 不匹配前导点
					continue
				}
				ok, merr := path.Match(c, n)
				if merr != nil {
					return nil, true, fmt.Errorf("通配 include %s 的模式非法", short(pattern))
				}
				if ok {
					nxt = append(nxt, path.Join(dir, n))
					if len(nxt) > in.o.MaxGlob {
						return nxt[:in.o.MaxGlob], true, fmt.Errorf("通配 include %s 匹配过多（上限 %d），其余忽略", short(pattern), in.o.MaxGlob)
					}
				}
			}
		}
		cur = nxt
	}
	sort.Strings(cur)
	return cur, true, nil
}

// resolveFile 对一个候选路径做 白名单 → readGuard → canon → 真实路径白名单/readGuard → 普通文件 检查，返回真实路径。
func (in *Includer) resolveFile(p string) (string, error) {
	p = path.Clean(p)
	if !in.allowed(p) {
		return "", fmt.Errorf("%s 不在 include 白名单根目录内", short(p))
	}
	if err := readGuard(p); err != nil {
		return "", fmt.Errorf("%s 属于敏感文件，不读取", short(p))
	}
	c, err := in.canon(p)
	if err != nil {
		return "", err
	}
	if !in.allowed(c) {
		return "", fmt.Errorf("%s 解析后的真实路径 %s 越出白名单根目录", short(p), short(c))
	}
	if err := readGuard(c); err != nil {
		return "", fmt.Errorf("%s 解析后指向敏感文件，不读取", short(p))
	}
	fi, err := in.fs.Lstat(c)
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s 不是普通文件", short(p))
	}
	return c, nil
}

// load 读取并解析一个真实路径的文件（带缓存，预算内）。
func (in *Includer) load(c string) *loaded {
	if l, ok := in.cache[c]; ok {
		return l
	}
	if in.files >= in.o.MaxFiles {
		in.warn("include 文件数超过上限 %d，不再读取 %s", in.o.MaxFiles, short(c))
		return nil // 不缓存：预算耗尽的状态是全局的
	}
	if in.bytes >= in.o.MaxBytes {
		in.warn("include 累计字节数超过上限 %d，不再读取 %s", in.o.MaxBytes, short(c))
		return nil
	}
	in.files++
	b, err := in.fs.ReadFile(c, maxConfBytes+1)
	if err != nil {
		in.warn("无法读取 include 文件 %s", short(c))
		in.cache[c] = nil
		return nil
	}
	in.bytes += int64(len(b))
	tree, perr := lexParse(b)
	tagFile(tree, c)
	if perr != nil {
		in.bad[c] = true
		in.warn("include 文件 %s 解析不完整：%v", short(c), perr)
	}
	l := &loaded{tree: tree, nodes: countNodes(tree)}
	in.cache[c] = l
	return l
}

func countNodes(ds []directive) int {
	n := 0
	for _, d := range ds {
		n++
		if d.hasB {
			n += countNodes(d.block)
		}
	}
	return n
}

// expand 把树里的 include 指令就地替换为被包含文件的内容（递归）。stack 是当前 include 链（真实路径），用于防环。
func (in *Includer) expand(ds []directive, depth int, stack []string) []directive {
	out := make([]directive, 0, len(ds))
	for _, d := range ds {
		if d.name == "include" && !d.hasB {
			out = append(out, in.include(d, depth, stack)...)
			continue
		}
		if d.hasB {
			d.block = in.expand(d.block, depth, stack)
		}
		out = append(out, d)
	}
	return out
}

func (in *Includer) include(d directive, depth int, stack []string) []directive {
	if len(d.args) != 1 || d.args[0] == "" {
		in.warn("%s 中的 include 指令参数个数不对，忽略", short(d.file))
		return nil
	}
	pat := d.args[0]
	if strings.Contains(pat, "$") {
		in.warn("%s 中的 include %s 含变量，不求值，忽略", short(d.file), short(pat))
		return nil
	}
	if depth >= in.o.MaxDepth {
		in.warn("include 嵌套深度超过上限 %d（在 %s 处停止）", in.o.MaxDepth, short(d.file))
		return nil
	}
	if in.nodes > in.o.MaxNodes {
		in.warn("include 展开后的指令数超过上限 %d，停止展开", in.o.MaxNodes)
		return nil
	}
	if !path.IsAbs(pat) {
		if in.prefix == "" {
			in.warn("相对 include %s 没有 nginx 前缀可用，忽略", short(pat))
			return nil
		}
		pat = path.Join(in.prefix, pat)
	}
	pat = path.Clean(pat)
	cands, isGlob, gerr := in.glob(pat)
	if gerr != nil {
		in.warn("%v", gerr)
	}
	var res []directive
	for _, cand := range cands {
		c, err := in.resolveFile(cand)
		if err != nil {
			if isGlob && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if errors.Is(err, fs.ErrNotExist) {
				in.warn("include 的文件不存在：%s", short(cand))
			} else if !isGlob || !strings.Contains(err.Error(), "不是普通文件") {
				in.warn("跳过 include %s：%v", short(cand), err)
			}
			continue
		}
		if contains(stack, c) {
			in.warn("检测到 include 循环：%s", short(c))
			continue
		}
		l := in.load(c)
		if l == nil {
			continue
		}
		if in.nodes += l.nodes; in.nodes > in.o.MaxNodes {
			in.warn("include 展开后的指令数超过上限 %d，停止展开", in.o.MaxNodes)
			return res
		}
		res = append(res, in.expand(l.tree, depth+1, append(stack[:len(stack):len(stack)], c))...)
	}
	return res
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// Canon 返回 file 的真实逻辑路径（用于去重）；无法解析时返回 file 本身。
func (in *Includer) Canon(file string) string {
	if c, err := in.canon(path.Clean(file)); err == nil {
		return c
	}
	return file
}

// ParseData 解析一个已读取的配置文件内容（file 为其逻辑路径），并跟随其中的 include。
func (in *Includer) ParseData(file, source string, data []byte) (sites []Site, err error) {
	defer func() {
		if r := recover(); r != nil {
			sites, err = nil, errors.New("baota: nginx 配置解析异常")
		}
	}()
	tree, err := lexParse(data)
	tagFile(tree, file)
	if err != nil {
		in.bad[file] = true
	}
	tree = in.expand(tree, 0, []string{file, in.Canon(file)})
	return collectSites(tree, source, in.bad), err
}

// ParseConf 读取（经白名单与 readGuard）并解析主配置文件 conf（如 /www/server/nginx/conf/nginx.conf），跟随全部 include。
// 相对 include 以 conf 所在目录为前缀。
func (in *Includer) ParseConf(conf, source string) ([]Site, error) {
	c, err := in.resolveFile(conf)
	if err != nil {
		return nil, err
	}
	in.SetPrefix(path.Dir(conf))
	b, err := in.fs.ReadFile(c, maxConfBytes+1)
	if err != nil {
		return nil, err
	}
	in.files++
	in.bytes += int64(len(b))
	return in.ParseData(c, source, b)
}

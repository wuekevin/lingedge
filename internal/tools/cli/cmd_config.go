package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/wuekevin/lingedge/internal/tools/backup"
	"github.com/wuekevin/lingedge/internal/tools/cfgcheck"
	"github.com/wuekevin/lingedge/internal/tools/doctor"
	"github.com/wuekevin/lingedge/internal/tools/installer"
	"github.com/wuekevin/lingedge/internal/tools/security"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
	"github.com/wuekevin/lingedge/internal/tools/service"
)

// config validate|show|edit（Story 1.42，tools-design §3/§19）。
//
// 目标（target）：`tools`（tools.yaml）或已装组件。每个目标的配置文件与校验来源：
//
//	tools                tools.yaml（--config 或缺省路径）        cfgcheck.CheckToolsYAML（与 install 读取时同一份规则）
//	server               /etc/lingedge/server/server.yaml        lingedge-server config check（与 Server 启动同一个 config.Parse）；二进制不可用时退回结构校验并标注「部分校验」
//	node/dns/reporter    /etc/lingedge/<c>/<c>.yaml              只做语法校验（schema 随 Epic 3 交付，如实标注）
//	静态前端             /etc/lingedge/nginx/<c>.conf + 安全头    installer 的片段静态检查（只读）；不可编辑（由 Tools 生成）
const (
	kindTools  = "tools"
	kindServer = "server"
	kindYAML   = "yaml"
	kindNginx  = "nginx"
	kindNone   = "none"
	maxCfgSize = 1 << 20
)

type cfgTarget struct {
	Name    string
	Kind    string
	File    string // 主机路径
	Virtual string // 展示用的虚拟路径
}

func (r *Run) displayPath(host string) string {
	if root := r.env.Install.Root; root != "" {
		if rel, err := filepath.Rel(root, host); err == nil && !strings.HasPrefix(rel, "..") {
			return "/" + filepath.ToSlash(rel)
		}
	}
	return host
}

func configKindOf(name string) string {
	switch {
	case name == "tools":
		return kindTools
	case name == "server":
		return kindServer
	case name == "node" || name == "dns" || name == "reporter":
		return kindYAML
	case installer.IsStaticComponent(name):
		return kindNginx
	}
	return kindNone
}

func (r *Run) cfgTargetFor(name string, comps []ComponentInfo, fileOverride string) (cfgTarget, *Error) {
	t := cfgTarget{Name: name, Kind: configKindOf(name)}
	if t.Kind == kindNone {
		return t, &Error{Exit: ExitPrecondition, Code: "config.no_config", Message: "组件 " + name + " 没有 Tools 管理的配置文件",
			Hint: "该组件的配置随其安装 Story 交付；可用 lingedge config validate 查看已有配置目标"}
	}
	var ci *ComponentInfo
	for i := range comps {
		if comps[i].Name == name {
			ci = &comps[i]
		}
	}
	if name != "tools" && ci == nil && fileOverride == "" {
		return t, &Error{Exit: ExitNotInstalled, Code: "config.not_installed", Message: "组件 " + name + " 尚未安装", Hint: "先 lingedge install " + name + "；或用 --file 校验一个文件"}
	}
	dir := "/etc/lingedge/" + name
	if ci != nil && ci.ConfigDir != "" {
		dir = ci.ConfigDir
	}
	var virtual string
	switch t.Kind {
	case kindTools:
		p, _ := r.toolsConfigPath()
		t.File, t.Virtual = p, r.displayPath(p)
	case kindNginx:
		virtual = installer.StaticConfPath(name)
	default:
		virtual = dir + "/" + name + ".yaml"
	}
	if t.Kind != kindTools {
		t.File, t.Virtual = r.hostPath(virtual), virtual
	}
	if fileOverride != "" {
		t.File, t.Virtual = fileOverride, fileOverride
	}
	return t, nil
}

// verifyTarget 校验一个目标（doctor 的 cfg.validate 与 config validate 共用）。
func (r *Run) verifyTarget(ctx context.Context, target, fileOverride string) doctor.ConfigVerdict {
	comps, _ := r.env.State.Components(ctx)
	t, e := r.cfgTargetFor(target, comps, fileOverride)
	v := doctor.ConfigVerdict{Target: target}
	if e != nil {
		v.Unavailable = e.Message
		return v
	}
	v.File = t.Virtual
	var data []byte
	var err error
	if t.Kind != kindNginx {
		data, _, err = readRegular(t.File, maxCfgSize, fileOverride != "")
		if err != nil {
			switch {
			case errors.Is(err, fs.ErrNotExist) && t.Kind == kindTools && fileOverride == "":
				v.Notes = append(v.Notes, "tools.yaml 不存在，使用缺省配置（不是错误）")
			case errors.Is(err, fs.ErrNotExist):
				v.Problems = []cfgcheck.Problem{{Reason: "配置文件不存在"}}
			case errors.Is(err, fs.ErrPermission):
				v.Unavailable = "无权读取 " + t.Virtual + "（需要 root 或配置属组）"
			default:
				v.Problems = []cfgcheck.Problem{{Reason: "无法读取配置文件：" + redactString(err.Error())}}
			}
			return v
		}
	}
	r.verifyBytes(ctx, t, data, t.File, &v, fileOverride != "")
	return v
}

// verifyBytes 校验配置内容 data。filePath 是 data 所在的文件（server 的权威校验需要把路径交给 lingedge-server）。
func (r *Run) verifyBytes(ctx context.Context, t cfgTarget, data []byte, filePath string, v *doctor.ConfigVerdict, explicitFile bool) {
	switch t.Kind {
	case kindTools:
		v.Problems = append(v.Problems, cfgcheck.CheckToolsYAML(data)...)
		if len(v.Problems) == 0 {
			v.Problems = append(v.Problems, r.crossCheckTools(data)...)
		}
	case kindServer:
		probs, ran, why := r.serverConfigCheck(ctx, filePath, data)
		if ran {
			v.Problems = append(v.Problems, probs...)
			return
		}
		v.Problems = append(v.Problems, cfgcheck.CheckServerShape(data)...)
		v.Partial = "只做了结构校验（" + why + "）；监听地址取值、metrics 回环、口令文件权限等需要 lingedge-server config check"
	case kindYAML:
		v.Problems = append(v.Problems, cfgcheck.CheckSyntaxOnly(data)...)
		v.Partial = "只做了语法校验（" + t.Name + " 的配置 schema 随 Epic 3 交付）"
	case kindNginx:
		conf, cerr := readCapped(t.File)
		hdrPath := r.hostPath(installer.StaticHeadersPath(t.Name))
		hdr, herr := readCapped(hdrPath)
		if cerr != nil || herr != nil {
			if errors.Is(cerr, fs.ErrPermission) || errors.Is(herr, fs.ErrPermission) {
				v.Unavailable = "无权读取站点片段（需要 root）"
				return
			}
			v.Problems = append(v.Problems, cfgcheck.Problem{Reason: "站点片段或安全头文件缺失/不可读"})
			return
		}
		v.Problems = append(v.Problems, cfgcheck.CheckNginxSite(t.Name, conf, hdr)...)
	}
}

func readCapped(p string) ([]byte, error) {
	b, _, err := readRegular(p, maxCfgSize, false)
	return b, err
}

// crossCheckTools 是 tools.yaml 的交叉校验：取值合法之外，引用的文件必须真实存在（release.ca_file）。
func (r *Run) crossCheckTools(data []byte) []cfgcheck.Problem {
	cfg, err := installer.ParseToolsConfig(data)
	if err != nil {
		return nil
	}
	doc, _ := cfgcheck.ParseYAML(data)
	var out []cfgcheck.Problem
	if p := cfg.Release.CAFile; p != "" {
		// 与 downloader 读取 release.ca_file 的语义一致：路径就是 tools.yaml 里写的绝对路径（不做根目录前缀）。
		if st, serr := os.Stat(p); serr != nil || !st.Mode().IsRegular() {
			pr := cfgcheck.Problem{Path: "release.ca_file", Reason: "指向的文件不存在或不是普通文件"}
			out = append(out, doc.At(pr))
		}
	}
	return out
}

type serverCheckOut struct {
	OK       bool `json:"ok"`
	Problems []struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
	} `json:"problems"`
}

// serverConfigCheck 用 lingedge-server config check 校验 server.yaml（权威来源）。ran=false：二进制不可用/不可执行（why 说明原因），调用方退回结构校验。
// 问题的行列由 Tools 用键路径在文件里定位（Server 的 Problem 只有路径与规则，不含取值）。
func (r *Run) serverConfigCheck(ctx context.Context, file string, data []byte) (probs []cfgcheck.Problem, ran bool, why string) {
	bin := r.hostPath(backup.ServerCurrentBin)
	if st, err := os.Stat(bin); err != nil || !st.Mode().IsRegular() {
		return nil, false, "未找到 lingedge-server（" + backup.ServerCurrentBin + "）"
	}
	run := r.env.Backup.ServerRun
	if run == nil {
		rr, err := backup.NewServerRunner()
		if err != nil {
			return nil, false, "无法构造 lingedge-server 执行器"
		}
		run = rr
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := run.Run(cctx, safeexec.Cmd{Path: bin, Args: []string{"config", "check", "--config", file, "--json"}, Env: r.env.Backup.ServerEnv})
	var ee *safeexec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return nil, false, "无法执行 lingedge-server"
	}
	if ee != nil && ee.Code != 1 {
		return nil, false, "lingedge-server config check 退出码 " + fmt.Sprint(ee.Code)
	}
	var out serverCheckOut
	if json.Unmarshal(bytes.TrimSpace(res.Stdout), &out) != nil {
		return nil, false, "lingedge-server config check 的输出无法解析（版本过旧？）"
	}
	doc, _ := cfgcheck.ParseYAML(data)
	for _, p := range out.Problems {
		pr := cfgcheck.Problem{Path: p.Path, Reason: p.Reason}
		if l := cfgcheck.LineFromReason(p.Reason); l > 0 {
			pr.Line = l
		}
		probs = append(probs, doc.At(pr))
	}
	if !out.OK && len(probs) == 0 {
		probs = append(probs, cfgcheck.Problem{Reason: "lingedge-server 判定配置不合法"})
	}
	return probs, true, ""
}

// ---- config validate ----

type validateTarget struct {
	Target      string             `json:"target"`
	File        string             `json:"file"`
	OK          bool               `json:"ok"`
	Problems    []cfgcheck.Problem `json:"problems"`
	Notes       []string           `json:"notes"`
	Partial     string             `json:"partial,omitempty"`
	Unavailable string             `json:"unavailable,omitempty"`
}

func cfgCompArg(allowTools bool) func(inv *invocation) *Error {
	return func(inv *invocation) *Error {
		for _, a := range inv.args {
			if allowTools && a == "tools" {
				continue
			}
			if e := checkComponent(a, false); e != nil {
				e.Hint = "可选：tools|" + join(components, "|")
				return e
			}
		}
		return nil
	}
}

func runConfigValidate(inv *invocation) (*Result, error) {
	r := inv.r
	file := inv.str("file")
	var names []string
	switch {
	case len(inv.args) == 1:
		names = []string{inv.args[0]}
	case file != "":
		names = []string{"tools"}
	default:
		names = []string{"tools"}
		comps, err := r.env.State.Components(r.ctx)
		if err != nil {
			return nil, err
		}
		for _, c := range comps {
			if configKindOf(c.Name) != kindNone {
				names = append(names, c.Name)
			}
		}
	}
	var items []validateTarget
	bad, unavail := 0, 0
	for _, n := range names {
		v := r.verifyTarget(r.ctx, n, file)
		it := validateTarget{Target: n, File: v.File, Problems: v.Problems, Notes: v.Notes, Partial: v.Partial, Unavailable: v.Unavailable}
		if it.Problems == nil {
			it.Problems = []cfgcheck.Problem{}
		}
		if it.Notes == nil {
			it.Notes = []string{}
		}
		it.OK = len(it.Problems) == 0 && it.Unavailable == ""
		if len(it.Problems) > 0 {
			bad++
		}
		if it.Unavailable != "" {
			unavail++
		}
		items = append(items, it)
	}
	res := &Result{
		Data: map[string]any{"ok": bad == 0 && unavail == 0, "targets": items},
		Human: func(w io.Writer) {
			for _, it := range items {
				switch {
				case it.Unavailable != "":
					wf(w, "- %s：无法校验（%s）\n", it.Target, it.Unavailable)
				case !it.OK:
					wf(w, "✗ %s（%s）\n", it.Target, it.File)
					for _, p := range it.Problems {
						wf(w, "    %s\n", p.String())
					}
				default:
					wf(w, "✓ %s（%s）通过\n", it.Target, it.File)
				}
				for _, n := range it.Notes {
					wf(w, "    说明：%s\n", n)
				}
				if it.Partial != "" && it.OK {
					wf(w, "    注意：%s\n", it.Partial)
				}
			}
		},
	}
	for _, it := range items {
		if it.Partial != "" && it.OK {
			res.Warnings = append(res.Warnings, Warning{Code: "config.partial_validation", Message: it.Target + "：" + it.Partial})
		}
	}
	switch {
	case bad > 0:
		var first []string
		for _, it := range items {
			for _, p := range it.Problems {
				first = append(first, it.Target+" "+p.String())
			}
		}
		if len(first) > 3 {
			first = first[:3]
		}
		return res, &Error{Exit: ExitPrecondition, Code: "config.invalid", Message: fmt.Sprintf("%d 个配置目标未通过校验：%s", bad, strings.Join(first, "；")),
			Hint: "按「第 N 行」修正后重新运行 lingedge config validate；lingedge config edit <组件> 会在替换前自动校验"}
	case unavail > 0:
		return res, &Error{Exit: ExitPrecondition, Code: "config.unreadable", Message: "有配置目标无权读取，无法校验", Hint: "以 root 运行（配置文件属 root:<运行组> 0640）"}
	}
	return res, nil
}

// ---- config show ----

func redactNode(n *yaml.Node) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			redactNode(c)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			if security.SensitiveKey(k.Value) && (v.Kind != yaml.ScalarNode || (v.Tag != "!!null" && v.Value != "")) {
				*v = yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "***", HeadComment: v.HeadComment, LineComment: v.LineComment}
				continue
			}
			redactNode(v)
		}
	case yaml.ScalarNode:
		if s := security.RedactString(n.Value); s != n.Value {
			n.Value, n.Tag, n.Style = s, "!!str", 0
		}
	}
}

// redactYAML 把敏感键的取值（密码/Token/私钥路径等）与值里内嵌的令牌/PEM/DSN 口令替换为 ***，保留注释与结构。
func redactYAML(b []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	redactNode(&doc)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	_ = enc.Close()
	return buf.Bytes(), nil
}

func redactLines(b []byte) []byte {
	var out bytes.Buffer
	inPEM := false
	for _, ln := range strings.Split(string(b), "\n") {
		switch {
		case strings.Contains(ln, "-----BEGIN "):
			inPEM = true
			out.WriteString("***[PEM 块已隐去]***\n")
		case inPEM:
			if strings.Contains(ln, "-----END ") {
				inPEM = false
			}
		default:
			out.WriteString(security.RedactString(ln) + "\n")
		}
	}
	return bytes.TrimRight(out.Bytes(), "\n")
}

func runConfigShow(inv *invocation) (*Result, error) {
	r := inv.r
	name := inv.args[0]
	comps, err := r.env.State.Components(r.ctx)
	if err != nil {
		return nil, err
	}
	t, e := r.cfgTargetFor(name, comps, "")
	if e != nil {
		return nil, e
	}
	var content []byte
	switch t.Kind {
	case kindNginx:
		conf, cerr := readCapped(t.File)
		hdr, herr := readCapped(r.hostPath(installer.StaticHeadersPath(name)))
		if err := firstReadErr(t.Virtual, cerr, herr); err != nil {
			return nil, err
		}
		content = append(append(append(redactLines(conf), []byte("\n\n# ---- "+installer.StaticHeadersPath(name)+" ----\n")...), redactLines(hdr)...), '\n')
	default:
		b, _, rerr := readRegular(t.File, maxCfgSize, false)
		if err := firstReadErr(t.Virtual, rerr); err != nil {
			if errors.Is(rerr, fs.ErrNotExist) && t.Kind == kindTools {
				return &Result{Data: map[string]any{"target": name, "file": t.Virtual, "exists": false, "content": ""},
					Human: func(w io.Writer) { wln(w, "# "+t.Virtual+" 不存在：使用缺省配置") }}, nil
			}
			return nil, err
		}
		red, yerr := redactYAML(b)
		if yerr != nil {
			// 语法有误的文件无法按结构脱敏：逐行脱敏兜底（值里的令牌/PEM/DSN 仍会被替换）。
			red = append(redactLines(b), '\n')
		}
		content = red
	}
	text := string(content)
	return &Result{
		Data:  map[string]any{"target": name, "file": t.Virtual, "exists": true, "content": text},
		Human: func(w io.Writer) { wf(w, "# %s（敏感值已脱敏）\n%s", t.Virtual, text) },
	}, nil
}

func firstReadErr(file string, errs ...error) *Error {
	for _, err := range errs {
		if err == nil {
			continue
		}
		switch {
		case errors.Is(err, fs.ErrPermission):
			return &Error{Exit: ExitPrecondition, Code: "config.unreadable", Message: "无权读取 " + file, Hint: "以 root 运行（配置文件属 root:<运行组> 0640）"}
		case errors.Is(err, fs.ErrNotExist):
			return &Error{Exit: ExitPrecondition, Code: "config.missing", Message: file + " 不存在", Hint: "lingedge doctor 的 state.drift 会指出安装与磁盘的差异"}
		}
		return &Error{Exit: ExitPrecondition, Code: "config.unreadable", Message: "无法读取 " + file + "：" + redactString(err.Error())}
	}
	return nil
}

// ---- config edit ----

var reEditorName = regexp.MustCompile(`^[A-Za-z0-9_./+-]{1,200}$`)

// editorAllowed 是允许启动的编辑器（经 safeexec 白名单；不支持带参数的编辑器命令行，也不经 shell）。
var editorAllowed = []string{"vi", "vim", "nano", "editor", "emacs"}

func (r *Run) editorRunner() (StreamRunner, error) {
	if e := r.ops().Editor; e != nil {
		return e, nil
	}
	dirs := r.ops().EditorDirs
	if len(dirs) == 0 {
		dirs = safeexec.DefaultSearchDirs
	}
	return safeexec.New(safeexec.Options{Allowed: editorAllowed, SearchDirs: dirs, MaxOutput: 1 << 10})
}

// pickEditor 选编辑器：$VISUAL ＞ $EDITOR ＞ editor/vi/nano（取第一个能解析到的）。返回传给 safeexec 的 Path。
func (r *Run) pickEditor(run StreamRunner) (string, *Error) {
	for _, env := range []string{"VISUAL", "EDITOR"} {
		v := strings.TrimSpace(r.env.Getenv(env))
		if v == "" {
			continue
		}
		if !reEditorName.MatchString(v) || !inList(editorAllowed, filepath.Base(v)) {
			return "", &Error{Exit: ExitUsage, Code: "usage.editor_unsupported", Message: "$" + env + " 指定的编辑器不被支持",
				Hint: "设置为 vi、vim、nano、editor、emacs 之一（可带绝对路径，不支持带参数的命令行）；Tools 以白名单启动编辑器，不经 shell"}
		}
		return v, nil
	}
	if rr, ok := run.(*safeexec.Runner); ok {
		for _, n := range []string{"editor", "vi", "nano"} {
			if _, err := rr.Resolve(n); err == nil {
				return n, nil
			}
		}
		return "", &Error{Exit: ExitPrecondition, Code: "precondition.no_editor", Message: "找不到可用的编辑器", Hint: "设置 EDITOR（vi/vim/nano/editor/emacs），或安装 vi/nano"}
	}
	return "vi", nil
}

func runConfigEdit(inv *invocation) (*Result, error) {
	r := inv.r
	name := inv.args[0]
	comps, err := r.env.State.Components(r.ctx)
	if err != nil {
		return nil, err
	}
	t, e := r.cfgTargetFor(name, comps, "")
	if e != nil {
		return nil, e
	}
	if t.Kind == kindNginx || t.Kind == kindNone {
		return nil, &Error{Exit: ExitPrecondition, Code: "config.not_editable", Message: name + " 的站点片段由 Tools 生成，不提供 config edit",
			Hint: "站点域名/上游等设置在安装时经 --set 给定；已有片段不会被覆盖（新内容写入 .new），可直接比对合并"}
	}
	cur, st, rerr := readRegular(t.File, maxCfgSize, false)
	if err := firstReadErr(t.Virtual, rerr); err != nil {
		return nil, err
	}
	if r.opts.DryRun {
		return &Result{Data: map[string]any{"target": name, "file": t.Virtual, "dry_run": true, "plan": []string{"在临时副本上启动编辑器", "校验副本（" + kindLabel(t.Kind) + "）", "原子替换并保留 " + t.Virtual + ".bak", "可选重启 " + name}},
			Human: func(w io.Writer) {
				wf(w, "将编辑 %s：临时副本 → 校验 → 原子替换（保留 .bak）→ 可选重启（--dry-run：未做任何改动）\n", t.Virtual)
			}}, nil
	}
	run, err := r.editorRunner()
	if err != nil {
		return nil, err
	}
	editor, ee := r.pickEditor(run)
	if ee != nil {
		return nil, ee
	}
	reg := r.registry()
	stage := reg.L.StagingDir()
	if err := security.MkdirAllPerm(stage, 0o700); err != nil {
		return nil, newErr(ExitPrecondition, "precondition.staging", "无法创建暂存目录："+redactString(err.Error()), "")
	}
	dir, err := os.MkdirTemp(stage, "config-edit-")
	if err != nil {
		return nil, newErr(ExitPrecondition, "precondition.staging", "无法创建暂存目录："+redactString(err.Error()), "")
	}
	_ = os.Chmod(dir, 0o700)
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(dir)
		}
	}()
	copyPath := filepath.Join(dir, filepath.Base(t.File))
	if err := security.WriteFileAtomic(copyPath, cur, 0o600); err != nil {
		return nil, newErr(ExitPrecondition, "precondition.staging", "无法创建编辑副本："+redactString(err.Error()), "")
	}

	var newData []byte
	var verdict doctor.ConfigVerdict
	for {
		cmd := safeexec.Cmd{Path: editor, Args: []string{copyPath}}
		if f, ok := r.env.Stdin.(*os.File); ok {
			cmd.Stdin = f // 只把真实终端句柄交给编辑器；非文件的 Stdin（管道/测试）不能交给子进程，否则会被 exec 的拷贝协程吞掉
		}
		if term := r.env.Getenv("TERM"); term != "" && reEditorName.MatchString(term) {
			cmd.Env = []string{"TERM=" + term}
		}
		if err := run.Stream(r.ctx, cmd, r.env.Stdout, r.env.Stderr); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, err
			}
			return nil, &Error{Exit: ExitGeneral, Code: "config.editor_failed", Message: "编辑器异常退出：" + redactString(err.Error()), Hint: "配置文件未改动"}
		}
		b, _, berr := readRegular(copyPath, maxCfgSize, false)
		if berr != nil {
			return nil, &Error{Exit: ExitPrecondition, Code: "config.edit_copy_unreadable", Message: "无法读取编辑后的副本：" + redactString(berr.Error()), Hint: "配置文件未改动"}
		}
		newData = b
		if bytes.Equal(newData, cur) {
			return &Result{Data: map[string]any{"target": name, "file": t.Virtual, "changed": false, "restarted": false},
				Human: func(w io.Writer) { wln(w, "配置没有改动，未做任何替换") }}, nil
		}
		verdict = doctor.ConfigVerdict{Target: name, File: t.Virtual}
		r.verifyBytes(r.ctx, t, newData, copyPath, &verdict, false)
		if len(verdict.Problems) == 0 {
			break
		}
		wf(r.env.Stderr, "校验未通过：\n")
		for _, p := range verdict.Problems {
			wf(r.env.Stderr, "  %s\n", redactText(p.String()))
		}
		again, _ := r.confirmPrompt(installer.Prompt{Message: "修改未通过校验，不会被应用。"})
		if !again {
			keep = true
			return nil, &Error{Exit: ExitPrecondition, Code: "config.invalid", Message: fmt.Sprintf("修改未通过校验（%d 个问题），未应用", len(verdict.Problems)),
				Hint: "你的修改保存在 " + copyPath + " ；文件为 root 0600，未应用；修正后可重新 lingedge config edit " + name, Details: map[string]any{"problems": verdict.Problems, "rejected_copy": copyPath}}
		}
	}

	// 原子替换：先保留 .bak（同权限同属主），再写新内容（临时文件先 chown 再写入再 rename）。
	mode := st.Mode().Perm()
	uid, gid, haveOwner := fileOwner(st)
	own := r.env.Install.Own
	if own == nil {
		own = service.SystemOwnership()
	}
	pre := func(tmp string) error {
		if !haveOwner {
			return nil
		}
		return own.Chown(tmp, uid, gid)
	}
	bak := t.File + ".bak"
	if err := security.WriteFileAtomicPre(bak, cur, mode, pre); err != nil {
		return nil, &Error{Exit: ExitGeneral, Code: "config.backup_failed", Message: "无法写入 " + t.Virtual + ".bak：" + redactString(err.Error()), Hint: "配置文件未改动"}
	}
	if err := security.WriteFileAtomicPre(t.File, newData, mode, pre); err != nil {
		return nil, &Error{Exit: ExitGeneral, Code: "config.replace_failed", Message: "无法替换 " + t.Virtual + "：" + redactString(err.Error()), Hint: "原文件保持不变（.bak 已写好）"}
	}
	res := &Result{Data: map[string]any{"target": name, "file": t.Virtual, "changed": true, "backup": t.Virtual + ".bak", "restarted": false, "partial_validation": verdict.Partial != ""}}
	if verdict.Partial != "" {
		res.Warnings = append(res.Warnings, Warning{Code: "config.partial_validation", Message: verdict.Partial})
	}
	msg := "已替换 " + t.Virtual + "（旧版本保留为 " + t.Virtual + ".bak）"
	restart := false
	if t.Kind != kindTools && len(service.RunUnits(name)) > 0 {
		switch {
		case inv.flag("restart"):
			restart = true
		case r.interactive():
			restart, _ = r.confirmPrompt(installer.Prompt{Message: "配置需要重启 " + name + " 才会生效。"})
		default:
			msg += "；需要 lingedge service restart " + name + " 才会生效"
		}
	}
	if restart {
		lerr := r.lockedRestart(inv, name)
		res.Data.(map[string]any)["restarted"] = lerr == nil
		if lerr != nil {
			res.Human = func(w io.Writer) { wln(w, msg) }
			return res, lerr
		}
		msg += "；已重启 " + name
	}
	res.Human = func(w io.Writer) { wln(w, msg) }
	return res, nil
}

func kindLabel(k string) string {
	switch k {
	case kindServer:
		return "lingedge-server config check"
	case kindTools:
		return "tools.yaml schema"
	}
	return "语法"
}

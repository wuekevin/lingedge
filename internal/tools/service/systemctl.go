package service

import (
	"context"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
)

// Systemctl 封装 systemctl：每个方法都构造固定形状的参数数组并经 Runner（safeexec）执行，没有命令串，
// 单元名只能是 AllUnits 中的已知单元。
type Systemctl struct {
	Run Runner
	// Booted 判定本机是否由 systemd 启动；nil 时用平台实现（Linux 为 sd_booted，其他平台恒 false）。
	Booted func() bool
}

// NewSystemctl 用生产 Runner 构造。
func NewSystemctl() (*Systemctl, error) {
	r, err := SystemRunner()
	if err != nil {
		return nil, err
	}
	return &Systemctl{Run: r}, nil
}

func (s *Systemctl) booted() bool {
	if s.Booted != nil {
		return s.Booted()
	}
	return systemdBooted()
}

func (s *Systemctl) ensure() error {
	if !s.booted() {
		return newErr(KindNotSystemd, "本机不是由 systemd 启动的系统，无法管理服务（Tools 仅支持 Linux + systemd）")
	}
	return nil
}

func checkUnits(units []UnitName) error {
	if len(units) == 0 {
		return newErr(KindInvalidParam, "未指定单元")
	}
	for _, u := range units {
		if !u.Valid() {
			return &Error{Kind: KindInvalidParam, Field: "unit", Msg: "不是 LingEdge 已知单元"}
		}
	}
	return nil
}

func unitArgs(verb string, units []UnitName, flags ...string) []string {
	a := append([]string{verb}, flags...)
	for _, u := range units {
		a = append(a, string(u))
	}
	return a
}

// do 执行并把 safeexec 的失败转成 KindExec；返回结果以便调用方读取 stdout。
func (s *Systemctl) do(ctx context.Context, args []string) (safeexec.Result, error) {
	res, err := s.Run.Run(ctx, safeexec.Cmd{Path: "systemctl", Args: args})
	if err != nil {
		return res, wrapErr(KindExec, err, "systemctl %s 失败", args[0])
	}
	return res, nil
}

// DaemonReload 执行 `systemctl daemon-reload`。
func (s *Systemctl) DaemonReload(ctx context.Context) error {
	if err := s.ensure(); err != nil {
		return err
	}
	_, err := s.do(ctx, []string{"daemon-reload"})
	return err
}

// Version 执行 `systemctl --version` 并解析。
func (s *Systemctl) Version(ctx context.Context) (Version, error) {
	if err := s.ensure(); err != nil {
		return Version{}, err
	}
	res, err := s.do(ctx, []string{"--version"})
	if err != nil {
		return Version{}, err
	}
	return ParseVersion(string(res.Stdout))
}

var reState = regexp.MustCompile(`^[a-z][a-z-]{0,31}$`)

// query 运行 is-active / is-enabled：这两个命令用退出码表达状态（非 0 并不是故障），状态词在 stdout 第一行。
func (s *Systemctl) query(ctx context.Context, verb string, u UnitName) (string, error) {
	if err := s.ensure(); err != nil {
		return "", err
	}
	if err := checkUnits([]UnitName{u}); err != nil {
		return "", err
	}
	res, err := s.Run.Run(ctx, safeexec.Cmd{Path: "systemctl", Args: []string{verb, string(u)}})
	if err != nil {
		var ee *safeexec.ExitError
		if !errors.As(err, &ee) {
			return "", wrapErr(KindExec, err, "systemctl %s 失败", verb)
		}
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(res.Stdout)), "\n")
	first = strings.TrimSpace(first)
	if !reState.MatchString(first) {
		return "", newErr(KindExec, "systemctl %s 的输出无法识别：%q", verb, truncate(first, 40))
	}
	return first, nil
}

// IsActive 返回 ActiveState 词（active / inactive / activating / failed …）。单元不存在时为 inactive。
func (s *Systemctl) IsActive(ctx context.Context, u UnitName) (string, error) {
	return s.query(ctx, "is-active", u)
}

// IsEnabled 返回 UnitFileState 词（enabled / disabled / static / masked / not-found …）。
func (s *Systemctl) IsEnabled(ctx context.Context, u UnitName) (string, error) {
	return s.query(ctx, "is-enabled", u)
}

// 撤销动作名（registry.UndoAction.Action；执行见 Undoer）。
const (
	ActSystemctlDisable = "systemctl_disable" // Created=true：本事务把单元从未启用变为已启用
	ActSystemctlStop    = "systemctl_stop"    // Created=true：本事务把单元从未运行变为已运行
)

// enabledStates 是「已经处于开机自启」的状态：此时 enable 是空操作，撤销不应 disable 用户原有的启用状态。
func alreadyEnabled(state string) bool {
	switch state {
	case "enabled", "enabled-runtime", "static", "alias", "indirect", "generated":
		return true
	}
	return false
}

// Enable 启用单元，并返回撤销信息：只有「此前未启用」才记为本事务所做（Created）。
func (s *Systemctl) Enable(ctx context.Context, units ...UnitName) ([]registry.UndoAction, error) {
	if err := s.ensure(); err != nil {
		return nil, err
	}
	if err := checkUnits(units); err != nil {
		return nil, err
	}
	var undo []registry.UndoAction
	for _, u := range units {
		st, err := s.IsEnabled(ctx, u)
		if err != nil {
			return nil, err
		}
		undo = append(undo, registry.UndoAction{Action: ActSystemctlDisable, Path: string(u), Created: !alreadyEnabled(st), Prev: st})
	}
	if _, err := s.do(ctx, unitArgs("enable", units)); err != nil {
		return nil, err
	}
	return undo, nil
}

// Disable 停用开机自启。
func (s *Systemctl) Disable(ctx context.Context, units ...UnitName) error {
	return s.simple(ctx, "disable", units)
}

// Start 启动单元，并返回撤销信息（此前未运行 → Created）。
func (s *Systemctl) Start(ctx context.Context, units ...UnitName) ([]registry.UndoAction, error) {
	if err := s.ensure(); err != nil {
		return nil, err
	}
	if err := checkUnits(units); err != nil {
		return nil, err
	}
	var undo []registry.UndoAction
	for _, u := range units {
		st, err := s.IsActive(ctx, u)
		if err != nil {
			return nil, err
		}
		undo = append(undo, registry.UndoAction{Action: ActSystemctlStop, Path: string(u), Created: st != "active" && st != "activating", Prev: st})
	}
	if _, err := s.do(ctx, unitArgs("start", units)); err != nil {
		return nil, err
	}
	return undo, nil
}

// Stop 停止单元。
func (s *Systemctl) Stop(ctx context.Context, units ...UnitName) error {
	return s.simple(ctx, "stop", units)
}

// Restart 重启单元。
func (s *Systemctl) Restart(ctx context.Context, units ...UnitName) error {
	return s.simple(ctx, "restart", units)
}

func (s *Systemctl) simple(ctx context.Context, verb string, units []UnitName) error {
	if err := s.ensure(); err != nil {
		return err
	}
	if err := checkUnits(units); err != nil {
		return err
	}
	_, err := s.do(ctx, unitArgs(verb, units))
	return err
}

// UnitProps 是 `systemctl show` 的受限属性集（只读；Show 只请求这些属性）。
type UnitProps struct {
	LoadState     string `json:"load_state"`
	ActiveState   string `json:"active_state"`
	SubState      string `json:"sub_state"`
	UnitFileState string `json:"unit_file_state"`
	Result        string `json:"result,omitempty"`
	MainPID       int    `json:"main_pid"`
	NRestarts     int    `json:"restarts"`
	// ActiveEnterTimestamp 是 systemd 给出的原文（本地时区文本，仅展示）；空 = 从未进入 active。
	ActiveEnterTimestamp string `json:"active_since,omitempty"`
}

// showProps 是 Show 请求的属性（固定，不接受调用方传入）。
const showProps = "LoadState,ActiveState,SubState,UnitFileState,Result,MainPID,NRestarts,ActiveEnterTimestamp"

var (
	reShowWord = regexp.MustCompile(`^[A-Za-z0-9._:@+-]{0,64}$`)
	reShowTime = regexp.MustCompile(`^[A-Za-z0-9 :+._()/-]{0,64}$`)
)

// Show 执行 `systemctl show <unit> --property=<固定集合> --no-pager`，解析为 UnitProps。单元不存在时 systemctl 仍返回 0 且 LoadState=not-found。
// 所有取值经枚举/长度校验（输出来自外部命令，不当作可信文本）。
func (s *Systemctl) Show(ctx context.Context, u UnitName) (UnitProps, error) {
	var p UnitProps
	if err := s.ensure(); err != nil {
		return p, err
	}
	if err := checkUnits([]UnitName{u}); err != nil {
		return p, err
	}
	res, err := s.do(ctx, []string{"show", "--no-pager", "--property=" + showProps, string(u)})
	if err != nil {
		return p, err
	}
	return parseShow(string(res.Stdout))
}

func parseShow(out string) (UnitProps, error) {
	var p UnitProps
	seen := 0
	for _, ln := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(ln, "\r"), "=")
		if !ok {
			continue
		}
		bad := func() (UnitProps, error) {
			return UnitProps{}, newErr(KindExec, "systemctl show 的 %s 输出无法识别", k)
		}
		switch k {
		case "LoadState", "ActiveState", "SubState", "UnitFileState", "Result":
			if !reShowWord.MatchString(v) {
				return bad()
			}
			switch k {
			case "LoadState":
				p.LoadState = v
			case "ActiveState":
				p.ActiveState = v
			case "SubState":
				p.SubState = v
			case "UnitFileState":
				p.UnitFileState = v
			default:
				p.Result = v
			}
			seen++
		case "MainPID", "NRestarts":
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 || n > 1<<30 {
				return bad()
			}
			if k == "MainPID" {
				p.MainPID = n
			} else {
				p.NRestarts = n
			}
			seen++
		case "ActiveEnterTimestamp":
			if !reShowTime.MatchString(v) {
				return bad()
			}
			p.ActiveEnterTimestamp = v
			seen++
		}
	}
	if seen == 0 || p.ActiveState == "" {
		return UnitProps{}, newErr(KindExec, "systemctl show 没有返回可识别的状态")
	}
	return p, nil
}

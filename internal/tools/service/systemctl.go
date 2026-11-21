package service

import (
	"context"
	"errors"
	"regexp"
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

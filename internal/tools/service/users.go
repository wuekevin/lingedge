package service

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
)

// NologinShell 是运行用户的登录 shell（tools-design §8）。
const NologinShell = "/usr/sbin/nologin"

// Provisioner 创建运行用户。查询（id / getent）与创建（useradd）都经 Runner（safeexec）。
type Provisioner struct {
	Run Runner
	// Geteuid 返回有效 uid；nil 时用 os.Geteuid。创建用户需要 root，非 root 时在动手前就明确失败，而不是留给 useradd 报晦涩的错。
	Geteuid func() int
}

// UserResult 是 EnsureUser 的结果。
type UserResult struct {
	Name         string
	Created      bool // 本次由我们创建（撤销时才会 userdel）
	GroupCreated bool // 同名主组是本次随用户一并创建的
	UID          int
	Warning      string
}

var errNoSuchUser = errors.New("no such user")

func isRunUser(name string) bool {
	for _, u := range RunUsers {
		if u == name {
			return true
		}
	}
	return false
}

func (p *Provisioner) euid() int {
	if p.Geteuid != nil {
		return p.Geteuid()
	}
	return os.Geteuid()
}

// lookupUID 用 `id -u <name>` 查询：存在返回 uid；不存在返回 errNoSuchUser；其他失败原样返回。
func (p *Provisioner) lookupUID(ctx context.Context, name string) (int, error) {
	res, err := p.Run.Run(ctx, safeexec.Cmd{Path: "id", Args: []string{"-u", name}})
	if err == nil {
		uid, perr := strconv.Atoi(strings.TrimSpace(string(res.Stdout)))
		if perr != nil || uid < 0 {
			return 0, newErr(KindExec, "id -u 的输出无法识别：%q", truncate(strings.TrimSpace(string(res.Stdout)), 40))
		}
		return uid, nil
	}
	var ee *safeexec.ExitError
	if errors.As(err, &ee) && ee.Code == 1 && (strings.Contains(ee.Stderr, "no such user") || strings.Contains(ee.Stderr, "unknown user")) {
		return 0, errNoSuchUser
	}
	return 0, wrapErr(KindExec, err, "查询用户 %s 失败", name)
}

// groupExists 用 `getent group <name>` 查询：退出码 0 存在，2 不存在。
func (p *Provisioner) groupExists(ctx context.Context, name string) (bool, error) {
	_, err := p.Run.Run(ctx, safeexec.Cmd{Path: "getent", Args: []string{"group", name}})
	if err == nil {
		return true, nil
	}
	var ee *safeexec.ExitError
	if errors.As(err, &ee) && ee.Code == 2 {
		return false, nil
	}
	return false, wrapErr(KindExec, err, "查询组 %s 失败", name)
}

// Exists 报告运行用户是否已存在（只读查询：id -u）。installer 在「先写 undo 再动手」时用它预先判定本事务是否会创建该用户
// （EnsureUser 的查询与创建是一次调用，来不及在创建前把 Created 写进 journal）。
func (p *Provisioner) Exists(ctx context.Context, name string) (bool, error) {
	if !isRunUser(name) {
		return false, &Error{Kind: KindInvalidParam, Field: "user", Msg: "不是 LingEdge 运行用户"}
	}
	if _, err := p.lookupUID(ctx, name); err != nil {
		if errors.Is(err, errNoSuchUser) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// EnsureUser 保证运行用户 name 存在：已存在则不动（Created=false）；不存在才 `useradd --system --no-create-home --shell /usr/sbin/nologin`。
// 同名组已存在时用 -g 复用，否则 --user-group 一并创建。name 必须是 RunUsers 之一。
// 既有用户 UID 为 0 视为冲突（绝不让服务以 root 的别名运行）；UID 在普通用户范围（>= 1000）只警告：它可能是人在用的账号。
func (p *Provisioner) EnsureUser(ctx context.Context, name string) (UserResult, error) {
	if !isRunUser(name) {
		return UserResult{}, &Error{Kind: KindInvalidParam, Field: "user", Msg: "不是 LingEdge 运行用户"}
	}
	res := UserResult{Name: name}
	uid, err := p.lookupUID(ctx, name)
	switch {
	case err == nil:
		if uid == 0 {
			return res, newErr(KindConflict, "用户 %s 的 UID 为 0，拒绝作为服务运行用户", name)
		}
		res.UID = uid
		if uid >= 1000 {
			res.Warning = "用户 " + name + "（uid " + strconv.Itoa(uid) + "）不是系统用户，已沿用；建议改为 --system 账号"
		}
		return res, nil
	case !errors.Is(err, errNoSuchUser):
		return res, err
	}
	if p.euid() != 0 {
		return res, newErr(KindPermission, "创建运行用户 %s 需要 root", name)
	}
	haveGroup, err := p.groupExists(ctx, name)
	if err != nil {
		return res, err
	}
	args := []string{"--system", "--no-create-home", "--shell", NologinShell}
	if haveGroup {
		args = append(args, "-g", name)
	} else {
		args = append(args, "--user-group")
	}
	args = append(args, name)
	if _, err := p.Run.Run(ctx, safeexec.Cmd{Path: "useradd", Args: args}); err != nil {
		return res, wrapErr(KindExec, err, "创建用户 %s 失败", name)
	}
	if uid, err = p.lookupUID(ctx, name); err != nil { // 创建后复核：必须真的存在
		return res, wrapErr(KindExec, err, "创建用户 %s 后复核失败", name)
	}
	res.Created, res.GroupCreated, res.UID = true, !haveGroup, uid
	return res, nil
}

// UserUndo 返回撤销信息：Created 记录「是否由本事务创建」，Extra 记录同名组是否随之创建。
func UserUndo(r UserResult) registry.UndoAction {
	return registry.UndoAction{Action: ActUserdel, Created: r.Created, Extra: map[string]string{
		"user": r.Name, "group_created": strconv.FormatBool(r.GroupCreated),
	}}
}

// userdel 删除本事务创建的用户（不加 -r：不碰家目录与邮件池，运行用户本来就没有家目录）。
func (p *Provisioner) userdel(ctx context.Context, name string) error {
	uid, err := p.lookupUID(ctx, name)
	if errors.Is(err, errNoSuchUser) {
		return nil // 已不存在：撤销是幂等的
	}
	if err != nil {
		return err
	}
	if uid == 0 || uid >= 1000 {
		return newErr(KindUndo, "拒绝删除用户 %s（uid %d 不是系统用户）", name, uid)
	}
	if p.euid() != 0 {
		return newErr(KindPermission, "删除运行用户 %s 需要 root", name)
	}
	if _, err := p.Run.Run(ctx, safeexec.Cmd{Path: "userdel", Args: []string{name}}); err != nil {
		return wrapErr(KindExec, err, "删除用户 %s 失败", name)
	}
	return nil
}

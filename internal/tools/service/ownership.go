package service

import (
	"os"
	"os/user"
	"strconv"
)

// Ownership 抽象「用户名 → uid/gid」与 chown。生产实现（SystemOwnership）用 os/user 与 os.Lchown；
// 非 root 的测试可注入记录调用的实现，断言属主逻辑而不必真的 chown（真实 chown 在 ENV-LNX 以 root 验证）。
type Ownership interface {
	// Resolve 解析用户与组名；组名为空表示只关心用户（gid 取用户主组）。
	Resolve(user, group string) (uid, gid int, err error)
	// Chown 修改 path 自身的属主（不跟随符号链接）。
	Chown(path string, uid, gid int) error
}

type sysOwnership struct{}

// SystemOwnership 返回生产实现。
func SystemOwnership() Ownership { return sysOwnership{} }

func (sysOwnership) Resolve(name, group string) (int, int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, wrapErr(KindConflict, err, "用户 %q 不存在", name)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, newErr(KindConflict, "用户 %q 的 uid 非数字", name)
	}
	gidStr := u.Gid
	if group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			return 0, 0, wrapErr(KindConflict, err, "组 %q 不存在", group)
		}
		gidStr = g.Gid
	}
	gid, err := strconv.Atoi(gidStr)
	if err != nil {
		return 0, 0, newErr(KindConflict, "组 %q 的 gid 非数字", group)
	}
	return uid, gid, nil
}

func (sysOwnership) Chown(path string, uid, gid int) error { return os.Lchown(path, uid, gid) }

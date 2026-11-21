package service

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/wuekevin/lingedge/internal/tools/security"
	"github.com/wuekevin/lingedge/internal/tools/security/safeexec"
)

// VerifyResult 是 `systemd-analyze verify` 的结果。
type VerifyResult struct {
	// Available 为 false 表示本机没有 systemd-analyze（tools-design §21：旧发行版可能没有 verify 子命令/程序）：调用方应跳过而不是失败。
	Available bool
	OK        bool   // 退出码 0
	Output    string // 合并的 stdout+stderr（已脱敏、截断）
}

// VerifyUnits 对已写入磁盘的单元文件执行 `systemd-analyze verify <路径…>`（参数数组，经 Runner）。
// 路径须是绝对规范路径。真实校验行为（含对 ExecStart 二进制存在性的检查）属 ENV-LNX。
func VerifyUnits(ctx context.Context, r Runner, paths ...string) (VerifyResult, error) {
	if len(paths) == 0 {
		return VerifyResult{}, newErr(KindInvalidParam, "未指定单元文件")
	}
	for _, p := range paths {
		if err := security.ValidateUnitPath(p); err != nil {
			return VerifyResult{}, &Error{Kind: KindInvalidParam, Field: "path", Msg: err.Error()}
		}
	}
	res, err := r.Run(ctx, safeexec.Cmd{Path: "systemd-analyze", Args: append([]string{"verify"}, paths...)})
	out := strings.TrimSpace(string(res.Stdout) + "\n" + string(res.Stderr))
	if err == nil {
		return VerifyResult{Available: true, OK: true, Output: out}, nil
	}
	var ee *safeexec.ExitError
	switch {
	case errors.Is(err, safeexec.ErrNotFound):
		return VerifyResult{}, nil
	case errors.As(err, &ee):
		return VerifyResult{Available: true, OK: false, Output: out}, nil
	}
	return VerifyResult{}, wrapErr(KindExec, err, "systemd-analyze verify 失败")
}

// VerifyDirs 只读检查目录是否与规格一致（doctor，FR-58）：缺失 → fail；权限宽于规格或属主不符 → fail；窄于规格 → warn。
func (e *Ensurer) VerifyDirs(specs []DirSpec) []Finding {
	var out []Finding
	for _, sp := range specs {
		f := Finding{ID: "service.dir", Category: "service", Level: LevelPass, Message: sp.Path + " 符合 " + fmtMode(sp.Mode) + " " + sp.Owner + ":" + sp.Group}
		st, err := os.Lstat(hostPath(e.Root, sp.Path))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			f.Level, f.Message, f.Hint = LevelFail, sp.Path+" 不存在", "重新运行安装或 lingedge doctor --fix-perms"
		case err != nil:
			f.Level, f.Message = LevelFail, sp.Path+" 无法检查："+err.Error()
		case st.Mode()&fs.ModeSymlink != 0 || !st.IsDir():
			f.Level, f.Message, f.Hint = LevelFail, sp.Path+" 不是目录（符号链接/文件）", "手工核对；Tools 不会跟随符号链接"
		default:
			got := st.Mode().Perm()
			if uid, gid, ok := ownerOf(st); ok {
				if wu, wg, rerr := e.Own.Resolve(sp.Owner, sp.Group); rerr == nil && (uid != wu || gid != wg) {
					f.Level, f.Message = LevelFail, sp.Path+" 属主 "+itoa(uid)+":"+itoa(gid)+" 应为 "+sp.Owner+":"+sp.Group
					f.Hint = "chown " + sp.Owner + ":" + sp.Group + " " + sp.Path
				}
			}
			switch {
			case got&^sp.Mode != 0:
				f.Level, f.Message = LevelFail, sp.Path+" 权限 "+fmtMode(got)+" 宽于规格 "+fmtMode(sp.Mode)
				f.Hint = "chmod " + strings.TrimPrefix(fmtMode(sp.Mode), "0") + " " + sp.Path
			case got != sp.Mode && f.Level == LevelPass:
				f.Level, f.Message = LevelWarn, sp.Path+" 权限 "+fmtMode(got)+" 窄于规格 "+fmtMode(sp.Mode)
			}
		}
		out = append(out, f)
	}
	return out
}

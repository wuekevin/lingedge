package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// SwapBinary 原子地用 newSrc（已验签包里解出的新可执行文件）替换 target（`update self`，tools-design §17/§18.3）：
//
//  1. 把 newSrc 写成与 target 同目录的 <target>.new（0755，fsync），保证后面的 rename 在同一文件系统内；
//  2. selfCheck(<target>.new) 自检（调用方用它执行 `<target>.new --version` 并核对版本）；失败 → 删除 .new，target 原封不动；
//  3. 保留旧版本为 <target>.prev（硬链接：旧 inode 因此不会被后面的 rename 释放；已有的 .prev 先删除），再 rename(.new → target) 一次性覆盖——
//     任何时刻 target 都是完整可执行的旧版本或新版本，不存在「target 缺失」的窗口。
//
// 返回 .prev 的路径。target 必须是普通文件（不跟随符号链接：调用方应先把 os.Executable 解析为真实路径）。
func SwapBinary(ctx context.Context, target, newSrc string, selfCheck func(ctx context.Context, newPath string) error) (prev string, err error) {
	fi, err := os.Lstat(target)
	if err != nil {
		return "", fmt.Errorf("无法确认当前可执行文件 %s：%w", target, err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s 不是普通文件，拒绝替换", target)
	}
	dir := filepath.Dir(target)
	newPath, prev := target+".new", target+".prev"
	_ = os.Remove(newPath)
	if err := copyExec(newSrc, newPath); err != nil {
		_ = os.Remove(newPath)
		return "", fmt.Errorf("写入 %s 失败：%w", newPath, err)
	}
	if selfCheck != nil {
		if err := selfCheck(ctx, newPath); err != nil {
			_ = os.Remove(newPath)
			return "", fmt.Errorf("新版本自检失败（%s 保持不变）：%w", target, err)
		}
	}
	if err := os.Remove(prev); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(newPath)
		return "", fmt.Errorf("无法清理旧的 %s：%w", prev, err)
	}
	if err := os.Link(target, prev); err != nil {
		_ = os.Remove(newPath)
		return "", fmt.Errorf("无法保留旧版本为 %s：%w", prev, err)
	}
	if err := os.Rename(newPath, target); err != nil {
		_ = os.Remove(newPath)
		return prev, fmt.Errorf("替换 %s 失败（旧版本仍在原处，也已保留为 %s）：%w", target, prev, err)
	}
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return prev, nil
}

func copyExec(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, 0o755)
}

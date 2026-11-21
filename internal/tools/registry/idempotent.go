package registry

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/wuekevin/lingedge/internal/tools/security"
)

// 每步幂等的落地助手（tools-design §12.3）：重复执行同一步，「已存在且与期望一致」视为已完成；
// 「已存在但不一致」是冲突（*Error{Kind: KindConflict}），**绝不覆盖**。返回的 created 告诉调用方这个效果是否由本次执行产生——
// 只有 created=true 的东西才应记入 undo（撤销时只删除本事务创建的东西）。

const maxEnsureFile = 16 << 20

func conflict(path, why string) error {
	return &Error{Kind: KindConflict, Msg: "已存在且不一致，拒绝覆盖：" + why, Path: path}
}

// EnsureFile 保证 path 是内容为 data、权限为 perm 的普通文件。
// 不存在 → 原子创建（临时文件经 security.WriteFileAtomic 写好并 fsync，再用硬链接 link(2) 落名：目标已存在时 link 失败，因此不可能覆盖并发创建的同名文件）；
// 已存在 → 必须是普通文件（符号链接算冲突）、内容与权限都相同才算完成。
func EnsureFile(path string, data []byte, perm fs.FileMode) (created bool, err error) {
	for range 3 {
		same, exists, cerr := fileMatches(path, data, perm)
		if cerr != nil {
			return false, cerr
		}
		if exists {
			if same {
				return false, nil
			}
			return false, conflict(path, "文件内容或权限与期望不同")
		}
		var rnd [6]byte
		if _, err := rand.Read(rnd[:]); err != nil {
			return false, err
		}
		tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".ensure-"+hex.EncodeToString(rnd[:]))
		if err := security.WriteFileAtomic(tmp, data, perm); err != nil {
			return false, wrapErr(KindIO, err, "写临时文件失败")
		}
		lerr := os.Link(tmp, path)
		_ = os.Remove(tmp)
		if lerr == nil {
			syncDirBestEffort(filepath.Dir(path))
			return true, nil
		}
		if !errors.Is(lerr, fs.ErrExist) {
			return false, &Error{Kind: KindIO, Msg: "落名失败", Path: path, Cause: lerr}
		}
		// 别人刚创建了同名文件：回到循环重新比对。
	}
	return false, conflict(path, "目标持续变化")
}

func fileMatches(path string, data []byte, perm fs.FileMode) (same, exists bool, err error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if isNotExist(err) {
			return false, false, nil
		}
		return false, false, &Error{Kind: KindIO, Msg: "无法检查", Path: path, Cause: err}
	}
	if !fi.Mode().IsRegular() {
		return false, true, nil
	}
	if fi.Mode().Perm() != perm || fi.Size() != int64(len(data)) || fi.Size() > maxEnsureFile {
		return false, true, nil
	}
	f, err := os.OpenFile(path, os.O_RDONLY|noFollowFlag, 0)
	if err != nil {
		return false, true, nil // 打不开（刚被换成链接等）按不一致处理
	}
	defer func() { _ = f.Close() }()
	got, err := io.ReadAll(io.LimitReader(f, int64(len(data))+1))
	if err != nil {
		return false, true, &Error{Kind: KindIO, Msg: "无法读取", Path: path, Cause: err}
	}
	return bytes.Equal(got, data), true, nil
}

// EnsureDirAll 保证 path 是目录（逐级创建缺失的层级，新建的目录权限为 perm）。
// 返回本次**新建**的目录（自上而下），撤销时应按逆序删除其中仍为空的目录。已存在但不是真实目录（含符号链接）是冲突。
func EnsureDirAll(path string, perm fs.FileMode) (created []string, err error) {
	path = filepath.Clean(path)
	var missing []string
	for p := path; ; p = filepath.Dir(p) {
		fi, lerr := os.Lstat(p)
		if lerr == nil {
			if !fi.IsDir() {
				return nil, conflict(p, "已存在但不是目录（符号链接/文件）")
			}
			break
		}
		if errors.Is(lerr, syscall.ENOTDIR) {
			return nil, conflict(p, "路径的上级不是目录")
		}
		if !isNotExist(lerr) {
			return nil, &Error{Kind: KindIO, Msg: "无法检查", Path: p, Cause: lerr}
		}
		missing = append([]string{p}, missing...)
		if filepath.Dir(p) == p {
			break
		}
	}
	for _, p := range missing {
		if err := security.MkdirAllPerm(p, perm); err != nil {
			return created, &Error{Kind: KindIO, Msg: "创建目录失败", Path: p, Cause: err}
		}
		created = append(created, p)
		syncDirBestEffort(filepath.Dir(p))
	}
	return created, nil
}

// EnsureSymlink 保证 link 是指向 target 的符号链接：不存在则创建；已是同一目标视为完成；指向别处或是别的类型是冲突（不替换；
// 切换 current 的原子替换属于升级步骤自己的算法，见 §8，不在「幂等创建」之内）。
func EnsureSymlink(link, target string) (created bool, err error) {
	for range 3 {
		fi, lerr := os.Lstat(link)
		switch {
		case lerr == nil:
			if fi.Mode()&fs.ModeSymlink == 0 {
				return false, conflict(link, "已存在且不是符号链接")
			}
			cur, rerr := os.Readlink(link)
			if rerr != nil {
				return false, &Error{Kind: KindIO, Msg: "无法读取链接", Path: link, Cause: rerr}
			}
			if cur == target {
				return false, nil
			}
			return false, conflict(link, "符号链接指向 "+cur+"，期望 "+target)
		case !isNotExist(lerr):
			return false, &Error{Kind: KindIO, Msg: "无法检查", Path: link, Cause: lerr}
		}
		if serr := os.Symlink(target, link); serr != nil {
			if errors.Is(serr, fs.ErrExist) {
				continue
			}
			return false, &Error{Kind: KindIO, Msg: "创建符号链接失败", Path: link, Cause: serr}
		}
		syncDirBestEffort(filepath.Dir(link))
		return true, nil
	}
	return false, conflict(link, "目标持续变化")
}

func syncDirBestEffort(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

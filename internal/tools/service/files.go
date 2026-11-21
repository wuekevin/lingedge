package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/wuekevin/lingedge/internal/tools/registry"
	"github.com/wuekevin/lingedge/internal/tools/security"
)

// FileSpec 描述一个要创建的文件的最终权限与属主。
type FileSpec struct {
	Mode  fs.FileMode
	Owner string
	Group string
}

// 文件规格（tools-design §8：配置 0640、master.key root 0400）。
var (
	// SpecUnit 是 systemd 单元文件：root:root 0644。
	SpecUnit = FileSpec{Mode: 0o644, Owner: "root", Group: "root"}
	// SpecRootSecret 是仅 root 可读的秘密（master.key 的默认交付方式，LoadCredential）：root:root 0400。
	SpecRootSecret = FileSpec{Mode: 0o400, Owner: "root", Group: "root"}
)

// SpecConfig 返回配置文件规格：root:<运行组> 0640（运行用户经属组读取，不可写）。
func SpecConfig(runGroup string) FileSpec {
	return FileSpec{Mode: 0o640, Owner: "root", Group: runGroup}
}

// SpecMasterKey 返回 master.key 的规格（ADR-027）：LoadCredential 模式 root:root 0400；
// 降级的密钥文件模式 root:<运行组> 0440——仍是 root 属主、不可写，运行组只读（pkg/secretbox 接受 0400/0440 且属主为 root 或运行用户）。
func SpecMasterKey(mode CredentialMode, runGroup string) FileSpec {
	if mode == CredentialKeyFile {
		return FileSpec{Mode: 0o440, Owner: "root", Group: runGroup}
	}
	return SpecRootSecret
}

// FileWriter 以「创建即最终权限」创建文件：目标文件由 security.CreateFile 创建（0600→显式 chmod，均先于写入内容），
// 随后先 chown、再写内容，所以内容从来不会在错误的属主下出现。Root 非空时所有路径前缀到该目录（测试与离线根目录用）。
type FileWriter struct {
	Root string
	Own  Ownership
}

// FileResult 是一次写入的结果与撤销信息。
type FileResult struct {
	Path    string
	Created bool // 本次新建（目标此前不存在）
	Undo    registry.UndoAction
}

func hostPath(root, p string) string {
	if root == "" {
		return p
	}
	return filepath.Join(root, p)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (w *FileWriter) chown(path string, spec FileSpec) error {
	uid, gid, err := w.Own.Resolve(spec.Owner, spec.Group)
	if err != nil {
		return err
	}
	if err := w.Own.Chown(path, uid, gid); err != nil {
		return wrapErr(KindPermission, err, "无法设置 %s 的属主为 %s:%s", path, spec.Owner, spec.Group)
	}
	return nil
}

// Create 仅在目标不存在时创建（O_EXCL，不跟随符号链接）；已存在返回 KindConflict 且不改动任何内容。
// master.key 必须走它：覆盖主密钥会让既有密文永久不可解。
func (w *FileWriter) Create(path string, data []byte, spec FileSpec) (FileResult, error) {
	real := hostPath(w.Root, path)
	f, err := security.CreateFile(real, spec.Mode)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return FileResult{}, &Error{Kind: KindConflict, Path: path, Msg: "文件已存在，拒绝覆盖"}
		}
		return FileResult{}, wrapErr(KindIO, err, "无法创建 %s", path)
	}
	fail := func(kind Kind, err error, msg string) (FileResult, error) {
		_ = f.Close()
		_ = os.Remove(real)
		return FileResult{}, wrapErr(kind, err, "%s %s", msg, path)
	}
	if err := w.chown(real, spec); err != nil {
		_ = f.Close()
		_ = os.Remove(real)
		return FileResult{}, err
	}
	if _, err := f.Write(data); err != nil {
		return fail(KindIO, err, "写入失败")
	}
	if err := f.Sync(); err != nil {
		return fail(KindIO, err, "fsync 失败")
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(real)
		return FileResult{}, wrapErr(KindIO, err, "关闭 %s 失败", path)
	}
	syncDir(filepath.Dir(real))
	return FileResult{Path: path, Created: true, Undo: registry.UndoAction{Action: ActRemovePath, Path: path, Created: true, SHA256: sha256Hex(data)}}, nil
}

func syncDir(dir string) { // 目录 fsync 失败不致命（部分文件系统不支持）
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// MasterKeyConfigValue 返回 server.yaml 的 secrets.master_key_file 应填的值（ADR-027）：
// LoadCredential 模式必须留空（Server 改读 $CREDENTIALS_DIRECTORY/master.key，pkg/secretbox 的路径优先级是「配置路径 > 凭据目录」，
// 填了就会绕过 LoadCredential）；降级的密钥文件模式必须填主密钥文件的绝对路径。
func MasterKeyConfigValue(mode CredentialMode, keyPath string) string {
	if mode == CredentialKeyFile {
		return keyPath
	}
	return ""
}

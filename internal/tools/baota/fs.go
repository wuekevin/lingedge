package baota

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ErrForbidden 表示读取被 readGuard 拒绝（敏感文件）。
var ErrForbidden = errors.New("baota: 拒绝读取敏感文件")

// FS 是探测所需的只读文件访问。路径是被探测主机上的绝对路径（如 /www/server/panel）。
type FS interface {
	// ReadFile 读取普通文件的前 max 字节（不跟随符号链接）。敏感文件返回 ErrForbidden。
	ReadFile(name string, max int64) ([]byte, error)
	ReadDir(name string) ([]fs.DirEntry, error)
	// Lstat 不跟随符号链接。
	Lstat(name string) (fs.FileInfo, error)
	// Readlink 读取符号链接的目标文本（不跟随）。
	Readlink(name string) (string, error)
}

// OSFS 把逻辑路径映射到 Root 之下的真实文件（Root 为 "/" 或空即真实根）。
type OSFS struct{ Root string }

func (o OSFS) real(name string) (string, error) {
	if !path.IsAbs(name) || path.Clean(name) != name {
		return "", fmt.Errorf("baota: 路径必须是规范的绝对路径: %q", name)
	}
	if o.Root == "" || o.Root == "/" {
		return name, nil
	}
	return filepath.Join(o.Root, filepath.FromSlash(name)), nil
}

// ReadFile 实现 FS。
func (o OSFS) ReadFile(name string, max int64) ([]byte, error) {
	if err := readGuard(name); err != nil {
		return nil, err
	}
	p, err := o.real(name)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("baota: %s 不是普通文件（不跟随符号链接）", name)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, max))
}

// ReadDir 实现 FS。
func (o OSFS) ReadDir(name string) ([]fs.DirEntry, error) {
	p, err := o.real(name)
	if err != nil {
		return nil, err
	}
	return os.ReadDir(p)
}

// Lstat 实现 FS。
func (o OSFS) Lstat(name string) (fs.FileInfo, error) {
	p, err := o.real(name)
	if err != nil {
		return nil, err
	}
	return os.Lstat(p)
}

// Readlink 实现 FS。
func (o OSFS) Readlink(name string) (string, error) {
	p, err := o.real(name)
	if err != nil {
		return "", err
	}
	return os.Readlink(p)
}

// sensitiveBase / sensitiveSub：基名命中即拒绝读取。覆盖宝塔把 MySQL root 密码、面板登录密码、Redis requirepass 存放的位置，
// 以及一般的凭据/私钥文件。名单刻意宽：探测不需要这些文件的内容。
var (
	sensitiveBase = map[string]bool{
		"default.pl": true, "default.db": true, "panel.db": true, "mysql.db": true, "my.cnf": true, ".my.cnf": true,
		"redis.conf": true, "shadow": true, "gshadow": true, "authorized_keys": true, "id_rsa": true, "id_ed25519": true,
		"mysql_root.pl": true, "root_password.pl": true, "bt_user.pl": true, "admin_path.pl": true, "basic_auth.json": true,
	}
	sensitiveExt = []string{".db", ".sqlite", ".sqlite3", ".key", ".pem", ".pfx", ".p12", ".pass", ".passwd", ".secret"}
	sensitiveSub = []string{"password", "passwd", "secret", "credential", "root_pass", "mysql_root"}
)

// readGuard 拒绝读取敏感路径（FR-7：不读取宝塔存储的 MySQL root 密码）。
func readGuard(name string) error {
	low := strings.ToLower(name)
	base := path.Base(low)
	if sensitiveBase[base] {
		return fmt.Errorf("%w: %s", ErrForbidden, base)
	}
	for _, e := range sensitiveExt {
		if strings.HasSuffix(base, e) {
			return fmt.Errorf("%w: %s", ErrForbidden, base)
		}
	}
	for _, s := range sensitiveSub {
		if strings.Contains(low, s) {
			return fmt.Errorf("%w: %s", ErrForbidden, base)
		}
	}
	return nil
}

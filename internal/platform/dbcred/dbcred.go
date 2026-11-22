// Package dbcred 读写运行期凭据文件 runtime-credentials.json 并把它合并进服务端配置（ADR-026 §3、AD-26）。
//
// 文件由安装向导（setup mode 的 Server 进程，运行用户 lingedge）写入，JSON 结构（v=1）：
//
//	database: host / port / name / tls，以及 app 与 maint 两个账号（user + 信封密文字段 password_enc）
//	redis:    addr / db / tls，以及可选的信封密文字段 password_enc
//
// 其中 password_enc 的取值是 pkg/secretbox 五段密文（v1:<kek_id>:…），口令明文从不出现在文件里。
// 机密性靠 pkg/secretbox 信封加密（KEK = master.key）而非文件属主：向导无权写 root 属主文件。口令字段 AAD =
// `runtime_credentials|<JSON 路径>|<DB 用户名或 redis>`，密文不能在字段间挪用。其余字段明文，便于 doctor 在无主密钥时只读检查。
//
// 本包**不**包含 lingedge_migrate 的任何东西：迁移口令只在内存（向导期）或 root:root 0400 的 migrate.yaml（root 侧命令），
// root 侧 migrate 的连接目标绝不取自本文件（ADR-026 §4 信任规则：lingedge 可写它）。
//
// 读取校验（fail-closed）：普通文件、非符号链接（O_NOFOLLOW）、属主 = 运行用户或 root、权限无 group/other 位、≤64KiB、v 已知、
// DisallowUnknownFields。写入：同目录临时文件 + fsync + rename（原子），写前 Open 往返自检（主密钥不可用则拒绝且不留残文件），
// 目录 0700、文件 0600。
package dbcred

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/internal/platform/config"
	"github.com/wuekevin/lingedge/pkg/secretbox"
)

// FileVersion 是当前文件格式版本。
const FileVersion = 1

// DefaultRelPath 是缺省文件相对 server.data_dir 的路径。
const DefaultRelPath = "secrets/runtime-credentials.json"

const (
	maxFileSize = 64 << 10
	aadTable    = "runtime_credentials"
	colApp      = "database.app.password"
	colMaint    = "database.maint.password"
	colRedis    = "redis.password"
	uidRedis    = "redis"
)

// 错误。
var (
	// ErrNotFound：文件不存在（尚未由向导写入）。
	ErrNotFound = errors.New("dbcred: 运行期凭据文件不存在")
	// ErrInvalid：文件存在但不满足校验（符号链接/权限/属主/大小/版本/未知字段）。信息不含任何口令。
	ErrInvalid = errors.New("dbcred: 运行期凭据文件无效")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// Account 是一个数据库账号（口令字段为信封密文）。
type Account struct {
	User        string `json:"user"`
	PasswordEnc string `json:"password_enc"`
}

// Database 是 database 段。
type Database struct {
	Host string `json:"host,omitempty"`
	Port int    `json:"port,omitempty"`
	// Socket 是 MySQL Unix 域套接字路径（CC-13）；与 host/port 互斥。
	Socket string   `json:"socket,omitempty"`
	Name   string   `json:"name"`
	TLS    string   `json:"tls,omitempty"`
	App    Account  `json:"app"`
	Maint  *Account `json:"maint,omitempty"`
}

// Redis 是 redis 段。
type Redis struct {
	Addr        string `json:"addr"`
	DB          int    `json:"db"`
	TLS         bool   `json:"tls"`
	PasswordEnc string `json:"password_enc,omitempty"`
}

// File 是文件内容。
type File struct {
	V        int       `json:"v"`
	Database *Database `json:"database,omitempty"`
	Redis    *Redis    `json:"redis,omitempty"`
}

// Path 返回运行期凭据文件路径：配置键优先，缺省 `<server.data_dir>/secrets/runtime-credentials.json`。
func Path(cfg *config.Config) string {
	if cfg.Secrets.RuntimeCredentialsFile != "" {
		return cfg.Secrets.RuntimeCredentialsFile
	}
	return filepath.Join(cfg.Server.DataDir, filepath.FromSlash(DefaultRelPath))
}

// Load 读取并校验文件。文件不存在返回 ErrNotFound；校验失败返回 ErrInvalid（包装，不含口令）。
func Load(path string) (*File, error) {
	f, err := openNoFollow(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, invalid("无法打开（%s；符号链接或无权限均被拒绝）", reason(err))
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, invalid("无法 stat")
	}
	if !st.Mode().IsRegular() {
		return nil, invalid("必须是普通文件")
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, invalid("权限过宽（当前 %04o；仅允许属主访问，应为 0600/0400）", st.Mode().Perm())
	}
	if err := checkOwner(st); err != nil {
		return nil, err
	}
	if st.Size() > maxFileSize {
		return nil, invalid("文件过大")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil || len(b) > maxFileSize {
		return nil, invalid("读取失败或过大")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var out File
	if err := dec.Decode(&out); err != nil {
		return nil, invalid("JSON 解析失败或含未知字段")
	}
	if dec.More() {
		return nil, invalid("含多余内容")
	}
	if out.V != FileVersion {
		return nil, invalid("未知版本 %d", out.V)
	}
	if d := out.Database; d != nil {
		if d.Socket != "" && (d.Host != "" || d.Port != 0) {
			return nil, invalid("database.socket 与 host/port 互斥")
		}
		if d.Socket != "" && !validSocket(d.Socket) {
			return nil, invalid("database.socket 不是规范化的绝对路径")
		}
	}
	return &out, nil
}

func reason(err error) string {
	switch {
	case errors.Is(err, os.ErrPermission):
		return "权限不足"
	case errors.Is(err, os.ErrNotExist):
		return "不存在"
	default:
		return "打开失败"
	}
}

func ctxFor(col, uid string) secretbox.RecordContext {
	return secretbox.RecordContext{Table: aadTable, Column: col, UID: uid}
}

// AppPassword 解密 app 口令。
func (f *File) AppPassword(ctx context.Context, box *secretbox.Box) (config.Secret, error) {
	if f.Database == nil {
		return config.Secret{}, errors.New("dbcred: 文件没有 database 段")
	}
	return open(ctx, box, f.Database.App.PasswordEnc, ctxFor(colApp, f.Database.App.User))
}

// MaintPassword 解密 maint 口令；文件没有 maint 账号时 ok=false。
func (f *File) MaintPassword(ctx context.Context, box *secretbox.Box) (s config.Secret, ok bool, err error) {
	if f.Database == nil || f.Database.Maint == nil {
		return config.Secret{}, false, nil
	}
	s, err = open(ctx, box, f.Database.Maint.PasswordEnc, ctxFor(colMaint, f.Database.Maint.User))
	return s, err == nil, err
}

// RedisPassword 解密 Redis 口令；未设置口令时返回空 Secret。
func (f *File) RedisPassword(ctx context.Context, box *secretbox.Box) (config.Secret, error) {
	if f.Redis == nil || f.Redis.PasswordEnc == "" {
		return config.Secret{}, nil
	}
	return open(ctx, box, f.Redis.PasswordEnc, ctxFor(colRedis, uidRedis))
}

func open(ctx context.Context, box *secretbox.Box, enc string, rc secretbox.RecordContext) (config.Secret, error) {
	if box == nil {
		return config.Secret{}, secretbox.ErrMasterKeyMissing
	}
	pt, err := box.Open(ctx, enc, rc)
	if err != nil {
		return config.Secret{}, fmt.Errorf("dbcred: 解密 %s 失败（主密钥不匹配或文件被篡改）: %w", rc.Column, err)
	}
	s := config.NewSecret(string(pt))
	secretbox.Zero(pt)
	return s, nil
}

func seal(ctx context.Context, box *secretbox.Box, pw config.Secret, rc secretbox.RecordContext) (string, error) {
	if box == nil {
		return "", secretbox.ErrMasterKeyMissing
	}
	pt := []byte(pw.Reveal())
	defer secretbox.Zero(pt)
	enc, err := box.Seal(ctx, pt, rc)
	if err != nil {
		return "", fmt.Errorf("dbcred: 加密 %s 失败: %w", rc.Column, err)
	}
	// 写前往返自检：主密钥不可用/加解密不一致则拒绝，绝不写出一个读不回来的文件。
	back, err := box.Open(ctx, enc, rc)
	ok := err == nil && string(back) == pw.Reveal()
	secretbox.Zero(back)
	if !ok {
		return "", fmt.Errorf("dbcred: %s 加解密往返自检失败", rc.Column)
	}
	return enc, nil
}

// DatabaseInput 是写入 database 段所需的明文输入。Maint 口令为空表示不写 maint 账号。
type DatabaseInput struct {
	Host      string
	Port      int
	Socket    string // 与 Host/Port 互斥（CC-13）
	Name      string
	TLS       string
	AppUser   string
	AppPass   config.Secret
	MaintUser string
	MaintPass config.Secret
}

// WriteDatabase 写（或替换）database 段，保留已有 redis 段。
func WriteDatabase(ctx context.Context, path string, box *secretbox.Box, in DatabaseInput) error {
	if in.Name == "" || in.AppUser == "" || !in.AppPass.IsSet() {
		return errors.New("dbcred: database 段缺少必填字段")
	}
	if in.Socket != "" {
		if in.Host != "" || in.Port != 0 || !validSocket(in.Socket) {
			return errors.New("dbcred: database.socket 与 host/port 互斥，且必须是规范化的绝对路径")
		}
	} else if in.Host == "" || in.Port < 1 {
		return errors.New("dbcred: database 段缺少必填字段")
	}
	cur, err := loadForUpdate(path)
	if err != nil {
		return err
	}
	appEnc, err := seal(ctx, box, in.AppPass, ctxFor(colApp, in.AppUser))
	if err != nil {
		return err
	}
	sec := &Database{Host: in.Host, Port: in.Port, Socket: in.Socket, Name: in.Name, TLS: in.TLS, App: Account{User: in.AppUser, PasswordEnc: appEnc}}
	if in.MaintUser != "" && in.MaintPass.IsSet() {
		mEnc, err := seal(ctx, box, in.MaintPass, ctxFor(colMaint, in.MaintUser))
		if err != nil {
			return err
		}
		sec.Maint = &Account{User: in.MaintUser, PasswordEnc: mEnc}
	}
	cur.Database = sec
	return writeAtomic(path, cur)
}

// RedisInput 是写入 redis 段所需的明文输入。
type RedisInput struct {
	Addr string
	DB   int
	TLS  bool
	Pass config.Secret
}

// WriteRedis 写（或替换）redis 段，保留已有 database 段。
func WriteRedis(ctx context.Context, path string, box *secretbox.Box, in RedisInput) error {
	if in.Addr == "" {
		return errors.New("dbcred: redis 段缺少 addr")
	}
	cur, err := loadForUpdate(path)
	if err != nil {
		return err
	}
	sec := &Redis{Addr: in.Addr, DB: in.DB, TLS: in.TLS}
	if in.Pass.IsSet() {
		if sec.PasswordEnc, err = seal(ctx, box, in.Pass, ctxFor(colRedis, uidRedis)); err != nil {
			return err
		}
	}
	cur.Redis = sec
	return writeAtomic(path, cur)
}

func loadForUpdate(path string) (*File, error) {
	f, err := Load(path)
	switch {
	case err == nil:
		return f, nil
	case errors.Is(err, ErrNotFound):
		return &File{V: FileVersion}, nil
	default:
		return nil, err // 已存在但无效：不静默覆盖，交给操作者处理
	}
}

// Role 是运行期账号角色（轮换用）。
type Role string

// 角色取值。
const (
	RoleApp   Role = "app"
	RoleMaint Role = "maint"
)

// ReplacePassword 只替换某一角色的口令密文（其余字段、另一角色的密文原样保留），原子重写；用于 `db credential rotate`（ADR-026 §8）。
// 角色账号必须已存在于文件里（轮换不创建账号）。
func ReplacePassword(ctx context.Context, path string, box *secretbox.Box, role Role, pw config.Secret) error {
	f, err := Load(path)
	if err != nil {
		return err
	}
	if f.Database == nil {
		return errors.New("dbcred: 文件没有 database 段")
	}
	switch role {
	case RoleApp:
		enc, err := seal(ctx, box, pw, ctxFor(colApp, f.Database.App.User))
		if err != nil {
			return err
		}
		f.Database.App.PasswordEnc = enc
	case RoleMaint:
		if f.Database.Maint == nil {
			return errors.New("dbcred: 文件没有 maint 账号")
		}
		enc, err := seal(ctx, box, pw, ctxFor(colMaint, f.Database.Maint.User))
		if err != nil {
			return err
		}
		f.Database.Maint.PasswordEnc = enc
	default:
		return fmt.Errorf("dbcred: 未知角色 %q", role)
	}
	return writeAtomic(path, f)
}

// Users 返回文件里 app 与 maint 账号名（maint 可能为空）。
func (f *File) Users() (app, maint string) {
	if f.Database == nil {
		return "", ""
	}
	app = f.Database.App.User
	if f.Database.Maint != nil {
		maint = f.Database.Maint.User
	}
	return app, maint
}

// MergeOptions 控制合并。
type MergeOptions struct {
	// NeedMaint 为 true 时才解密 maint 口令（api 角色不需要它，ADR-026 §3：只解当前角色需要的字段）。
	NeedMaint bool
}

// Merged 说明合并从文件取了什么（供启动日志；不含任何值）。
type Merged struct {
	Used []string
}

// Merge 把运行期凭据文件合并进 cfg：显式通道（server.yaml/环境变量/*.password_file 已填入 cfg 的值）优先，缺省才读文件。
// 全部显式给出时不读文件；文件不存在时不做任何事（缺失项交给 cfg.ValidateRuntime 报告）；文件存在但无效返回错误（fail-closed）。
func Merge(ctx context.Context, cfg *config.Config, box *secretbox.Box, o MergeOptions) (Merged, error) {
	var m Merged
	d := &cfg.Database
	needDB := (d.Host == "" && d.Socket == "") || d.Name == "" || d.User == "" || !d.Password.IsSet()
	needMaint := o.NeedMaint && !d.Maint.Configured()
	needRedis := cfg.Redis.Addr == "" || !cfg.Redis.Password.IsSet()
	if !needDB && !needMaint && !needRedis {
		return m, nil
	}
	f, err := Load(Path(cfg))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return m, nil
		}
		return m, err
	}
	if fd := f.Database; fd != nil {
		if d.Host == "" && d.Socket == "" && d.Name == "" { // 连接目标作为一组取用：不把显式的 host 与文件里的 name 拼成一个从未存在过的目标
			d.Host, d.Port, d.Socket, d.Name = fd.Host, fd.Port, fd.Socket, fd.Name
			if fd.Socket != "" {
				d.Port = 0
			}
			if d.TLS == "" {
				d.TLS = fd.TLS
			}
			m.Used = append(m.Used, "database.target")
		}
		// 口令只与它所属的连接目标配套使用：显式给出了另一个目标（host/port/name 与文件不同）时，不把文件里的口令发往那个目标。
		sameTarget := d.Socket == fd.Socket && d.Name == fd.Name
		if fd.Socket == "" { // TCP 目标：socket 为空已比较；再比 host/port
			sameTarget = sameTarget && d.Host == fd.Host && d.Port == fd.Port
		}
		if needDB && !d.Password.IsSet() && sameTarget {
			if d.User == "" || d.User == fd.App.User {
				pw, err := f.AppPassword(ctx, box)
				if err != nil {
					return m, err
				}
				d.User, d.Password = fd.App.User, pw
				m.Used = append(m.Used, "database.app")
			}
		}
		if needMaint && sameTarget && fd.Maint != nil && (d.Maint.User == "" || d.Maint.User == fd.Maint.User) && !d.Maint.Password.IsSet() {
			pw, _, err := f.MaintPassword(ctx, box)
			if err != nil {
				return m, err
			}
			d.Maint.User, d.Maint.Password = fd.Maint.User, pw
			m.Used = append(m.Used, "database.maint")
		}
	}
	if fr := f.Redis; fr != nil && needRedis {
		if cfg.Redis.Addr == "" {
			cfg.Redis.Addr, cfg.Redis.DB, cfg.Redis.TLS = fr.Addr, fr.DB, fr.TLS
			m.Used = append(m.Used, "redis.target")
		}
		if !cfg.Redis.Password.IsSet() && fr.PasswordEnc != "" && cfg.Redis.Addr == fr.Addr {
			pw, err := f.RedisPassword(ctx, box)
			if err != nil {
				return m, err
			}
			cfg.Redis.Password = pw
			m.Used = append(m.Used, "redis.password")
		}
	}
	return m, nil
}

// KEKIDs 返回文件里密文仍在使用的 kek_id（去重、升序）：KEK 轮换的「仍被引用」计数与备份清单 kek_ids_required 使用（ADR-026 §8）。
// 文件不存在返回空。
func KEKIDs(path string) ([]string, error) {
	f, err := Load(path)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	set := map[string]bool{}
	for _, enc := range f.encs() {
		id, err := secretbox.KeyID(enc.val)
		if err != nil {
			return nil, invalid("%s 不是合法的信封密文", enc.col)
		}
		set[id] = true
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// RewrapFile 把文件内所有密文的 DEK 重包装到 toKEK（secrets.rewrap 的额外目标，ADR-026 §8）；仅在有变更时原子重写，可重入。
// 返回被重包装的字段数。文件不存在返回 0。
func RewrapFile(ctx context.Context, box *secretbox.Box, path, toKEK string) (int, error) {
	f, err := Load(path)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, e := range f.encPtrs() {
		next, changed, err := box.Rewrap(ctx, *e.ptr, e.rc, toKEK)
		if err != nil {
			return 0, fmt.Errorf("dbcred: 重包装 %s: %w", e.rc.Column, err)
		}
		if changed {
			*e.ptr = next
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	return n, writeAtomic(path, f)
}

type encRef struct {
	col string
	val string
	ptr *string
	rc  secretbox.RecordContext
}

func (f *File) encPtrs() []encRef {
	var out []encRef
	if d := f.Database; d != nil {
		out = append(out, encRef{col: colApp, ptr: &d.App.PasswordEnc, rc: ctxFor(colApp, d.App.User)})
		if d.Maint != nil {
			out = append(out, encRef{col: colMaint, ptr: &d.Maint.PasswordEnc, rc: ctxFor(colMaint, d.Maint.User)})
		}
	}
	if r := f.Redis; r != nil && r.PasswordEnc != "" {
		out = append(out, encRef{col: colRedis, ptr: &r.PasswordEnc, rc: ctxFor(colRedis, uidRedis)})
	}
	return out
}

func (f *File) encs() []encRef {
	out := f.encPtrs()
	for i := range out {
		out[i].val = *out[i].ptr
	}
	return out
}

// HasEncryptedPlaintext 是转储自检辅助：报告 raw 是否含任一 needle（调用方传入已知口令明文）。测试与 doctor 使用。
func HasEncryptedPlaintext(raw []byte, needles ...string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(string(raw), n) {
			return true
		}
	}
	return false
}

// validSocket 报告 p 是否是可接受的 Unix 套接字路径（规范化绝对路径、无 NUL/换行、长度 ≤100）。
func validSocket(p string) bool {
	return p != "" && len(p) <= 100 && filepath.IsAbs(p) && filepath.Clean(p) == p && !strings.ContainsAny(p, "\x00\r\n")
}

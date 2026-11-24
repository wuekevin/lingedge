package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/wuekevin/lingedge/pkg/secretbox"
)

func newHash() hash.Hash { return sha256.New() }

func hashReader(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func parseMeta(raw []byte) (*Meta, error) {
	var m Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errors.New("加密备份头部的摘要无法解析")
	}
	if m.FormatVersion != FormatVersion || (m.Component != "server" && m.Component != "node") {
		return nil, errors.New("加密备份头部的摘要不合法")
	}
	for _, id := range m.KEKIDsRequired {
		if !secretbox.ValidKEKID(id) {
			return nil, errors.New("加密备份头部的 kek_ids_required 不合法")
		}
	}
	return &m, nil
}

// encTarget 是 Server 的密文列（与 internal/platform/secrets/repository.targets 一致：enc 值第 2 段即 kek_id）。
// Tools 不 import internal/platform，这份清单由 TestEncTargetsMatchMigrations 对照迁移文件里的列核对。
var encTargets = []struct{ table, column string }{
	{"certificates", "private_key_enc"},
	{"signing_keys", "private_key_enc"},
	{"system_settings", "value_enc"},
	{"users", "totp_secret_enc"},
}

// EncTargets 返回备份扫描的密文列（表, 列），供测试对照真实迁移后的库里全部 *_enc 列。
func EncTargets() [][2]string {
	out := make([][2]string, len(encTargets))
	for i, t := range encTargets {
		out[i] = [2]string{t.table, t.column}
	}
	return out
}

type dbMetaInfo struct {
	schema  int64
	history []MigrationRec
	lineage *int64
	keks    []string
}

// dbMeta 在备份时读取数据库的元数据：schema 版本与迁移级别历史、deploy.lineage、全部密文涉及的 kek_id（含运行期凭据文件里的密文）。
func (s *Service) dbMeta(ctx context.Context, conn DBConn, runtimeCreds string) (dbMetaInfo, error) {
	var out dbMetaInfo
	my := s.MySQL
	has, err := my.tableExists(ctx, conn, "schema_migrations")
	if err != nil {
		return out, err
	}
	if has {
		rows, err := my.Query(ctx, conn, "SELECT COALESCE(MAX(version_id), 0) FROM schema_migrations WHERE is_applied = 1")
		if err != nil {
			return out, err
		}
		if len(rows) == 1 && len(rows[0]) == 1 {
			if out.schema, err = atoi64(rows[0][0]); err != nil {
				return out, errors.New("schema_migrations 的版本不是整数")
			}
		}
		if ok, _ := my.tableExists(ctx, conn, "schema_migrations_runs"); ok {
			rows, err := my.Query(ctx, conn, "SELECT version, name, class FROM schema_migrations_runs WHERE status = 'applied' AND direction = 'up' ORDER BY version")
			if err != nil {
				return out, err
			}
			for _, r := range rows {
				if len(r) != 3 {
					return out, errors.New("schema_migrations_runs 输出格式异常")
				}
				v, err := atoi64(r[0])
				if err != nil {
					return out, errors.New("schema_migrations_runs 的版本不是整数")
				}
				out.history = append(out.history, MigrationRec{Version: v, Name: r[1], Class: r[2]})
			}
		}
	}
	if ok, err := my.tableExists(ctx, conn, "system_settings"); err != nil {
		return out, err
	} else if ok {
		rows, err := my.Query(ctx, conn, "SELECT value_json FROM system_settings WHERE setting_key = 'deploy.lineage'")
		if err != nil {
			return out, err
		}
		if len(rows) == 1 && len(rows[0]) == 1 {
			v, err := atoi64(rows[0][0])
			if err != nil || v < 1 {
				return out, errors.New("system_settings 的 deploy.lineage 不是正整数")
			}
			out.lineage = &v
		}
	}
	set := map[string]bool{}
	for _, t := range encTargets {
		ok, err := my.tableExists(ctx, conn, t.table)
		if err != nil {
			return out, err
		}
		if !ok {
			continue
		}
		q := fmt.Sprintf("SELECT DISTINCT SUBSTRING_INDEX(SUBSTRING_INDEX(%s, ':', 2), ':', -1) FROM %s WHERE %s IS NOT NULL", t.column, t.table, t.column)
		rows, err := my.Query(ctx, conn, q)
		if err != nil {
			return out, err
		}
		for _, r := range rows {
			if len(r) != 1 || !secretbox.ValidKEKID(r[0]) {
				return out, fmt.Errorf("%s.%s 含无法识别的密文（kek_id 不合法），拒绝生成不完整的 kek_ids_required", t.table, t.column)
			}
			set[r[0]] = true
		}
	}
	rc, err := runtimeCredKEKs(runtimeCreds)
	if err != nil {
		return out, err
	}
	for _, id := range rc {
		set[id] = true
	}
	for id := range set {
		out.keks = append(out.keks, id)
	}
	sort.Strings(out.keks)
	return out, nil
}

// runtimeCredFile 是 runtime-credentials.json 里与本包相关的部分（格式见 ADR-026 §3；Tools 不 import internal/platform/dbcred）。
type runtimeCredFile struct {
	Database *struct {
		App struct {
			User        string `json:"user"`
			PasswordEnc string `json:"password_enc"`
		} `json:"app"`
		Maint *struct {
			User        string `json:"user"`
			PasswordEnc string `json:"password_enc"`
		} `json:"maint"`
	} `json:"database"`
	Redis *struct {
		PasswordEnc string `json:"password_enc"`
	} `json:"redis"`
}

type encRef struct {
	enc string
	rc  secretbox.RecordContext
}

func loadRuntimeCreds(path string) (*runtimeCredFile, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取运行期凭据文件失败: %w", err)
	}
	var f runtimeCredFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, errors.New("运行期凭据文件不是合法 JSON")
	}
	return &f, nil
}

func (f *runtimeCredFile) refs() []encRef {
	var out []encRef
	if f == nil {
		return out
	}
	if f.Database != nil {
		if f.Database.App.PasswordEnc != "" {
			out = append(out, encRef{f.Database.App.PasswordEnc, secretbox.RecordContext{Table: "runtime_credentials", Column: "database.app.password", UID: f.Database.App.User}})
		}
		if f.Database.Maint != nil && f.Database.Maint.PasswordEnc != "" {
			out = append(out, encRef{f.Database.Maint.PasswordEnc, secretbox.RecordContext{Table: "runtime_credentials", Column: "database.maint.password", UID: f.Database.Maint.User}})
		}
	}
	if f.Redis != nil && f.Redis.PasswordEnc != "" {
		out = append(out, encRef{f.Redis.PasswordEnc, secretbox.RecordContext{Table: "runtime_credentials", Column: "redis.password", UID: "redis"}})
	}
	return out
}

// runtimeCredKEKs 返回运行期凭据文件里密文涉及的 kek_id（文件不存在 → 空）。
func runtimeCredKEKs(path string) ([]string, error) {
	f, err := loadRuntimeCreds(path)
	if err != nil || f == nil {
		return nil, err
	}
	var ids []string
	for _, r := range f.refs() {
		id, err := secretbox.KeyID(r.enc)
		if err != nil {
			return nil, errors.New("运行期凭据文件含无法识别的密文")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// ProbeResult 是「恢复后能解密探针记录」的校验结果。
type ProbeResult struct {
	Skipped bool     `json:"skipped"`
	Reason  string   `json:"reason,omitempty"`
	Probed  []string `json:"probed"` // 每个被核对的 kek_id 及来源，如 "k1@system_settings.value_enc"
}

// probe 对 required 里的每个 kek_id 找一条仍以它包装的密文并用当前主密钥解密（DB 优先，其次运行期凭据文件）。
// 任何一个 kek_id 找不到探针或解密失败都返回错误：这是「恢复后数据与密钥一致可用」的权威判据（ADR-011 §8）。
func (s *Service) probe(ctx context.Context, conn DBConn, required []string, keyPath, runtimeCreds string) (ProbeResult, error) {
	res := ProbeResult{Probed: []string{}}
	if len(required) == 0 {
		res.Skipped, res.Reason = true, "备份中没有任何密文，无探针可核对"
		return res, nil
	}
	kp, err := secretbox.NewFileKeyProvider(secretbox.FileOptions{Path: keyPath, AllowedUIDs: s.AllowedUIDs})
	if err != nil {
		return res, fmt.Errorf("%w: %v", ErrMasterKeyInvalid, err)
	}
	defer kp.Close()
	box, err := secretbox.New(kp)
	if err != nil {
		return res, err
	}
	rcf, err := loadRuntimeCreds(runtimeCreds)
	if err != nil {
		return res, err
	}
	for _, id := range required {
		if !secretbox.ValidKEKID(id) {
			return res, fmt.Errorf("kek_id %q 不合法", id)
		}
		prefix := "v1:" + id + ":"
		done := false
		for _, t := range encTargets {
			ok, err := s.MySQL.tableExists(ctx, conn, t.table)
			if err != nil {
				return res, err
			}
			if !ok {
				continue
			}
			q := fmt.Sprintf("SELECT uid, %s FROM %s WHERE BINARY LEFT(%s, %d) = '%s' ORDER BY id LIMIT 1", t.column, t.table, t.column, len(prefix), qs(prefix))
			rows, err := s.MySQL.Query(ctx, conn, q)
			if err != nil {
				return res, err
			}
			if len(rows) == 0 {
				continue
			}
			if len(rows[0]) != 2 {
				return res, errors.New("探针查询输出格式异常")
			}
			pt, err := box.Open(ctx, rows[0][1], secretbox.RecordContext{Table: t.table, Column: t.column, UID: rows[0][0]})
			if err != nil {
				return res, fmt.Errorf("%w：用当前主密钥解密 %s.%s 的探针记录失败（kek_id=%s）", ErrProbeFailed, t.table, t.column, id)
			}
			secretbox.Zero(pt)
			res.Probed = append(res.Probed, id+"@"+t.table+"."+t.column)
			done = true
			break
		}
		if done {
			continue
		}
		for _, r := range rcf.refs() {
			if kid, err := secretbox.KeyID(r.enc); err != nil || kid != id {
				continue
			}
			pt, err := box.Open(ctx, r.enc, r.rc)
			if err != nil {
				return res, fmt.Errorf("%w：用当前主密钥解密运行期凭据文件的探针记录失败（kek_id=%s）", ErrProbeFailed, id)
			}
			secretbox.Zero(pt)
			res.Probed = append(res.Probed, id+"@runtime-credentials.json")
			done = true
			break
		}
		if !done {
			return res, fmt.Errorf("%w：找不到以 kek_id=%s 包装的探针记录", ErrProbeFailed, id)
		}
	}
	return res, nil
}

// ErrProbeFailed：恢复后无法用当前主密钥解密探针记录。
var ErrProbeFailed = errors.New("backup: 恢复后的探针解密失败")

// RequiredKEKs 汇总目录里全部备份（含 pre-restore 安全备份）所需的 kek_id（kek → 需要它的备份文件名，升序）。
// 无法解析的备份文件不被忽略：它们的 kek 需求未知，返回在 unreadable 里（调用方须保守处理）。
func (s *Service) RequiredKEKs(dir string) (need map[string][]string, unreadable []string, err error) {
	list, err := s.List(dir)
	if err != nil {
		return nil, nil, err
	}
	need = map[string][]string{}
	for _, e := range list {
		if e.Status != "ok" || e.Meta == nil {
			unreadable = append(unreadable, e.Name)
			continue
		}
		for _, id := range e.Meta.KEKIDsRequired {
			need[id] = append(need[id], e.Name)
		}
	}
	for id := range need {
		sort.Strings(need[id])
	}
	sort.Strings(unreadable)
	return need, unreadable, nil
}

// GuardKEKRemoval 是「移除 KEK 前」对保留备份的核对（MIN-05、ADR-011 §3）：kekID 仍被某个保留备份需要 → 拒绝（返回包装了
// secretbox.ErrKEKRequiredByBackup 的错误并列出备份），除非 dropCompat（--drop-backup-compat，调用方须提示这些旧备份此后在不带旧主密钥时不可解密）。
// 存在无法解析的备份时同样拒绝（保守：不知道它是否需要该 KEK），除非 dropCompat。返回的 required 可直接填入 secretbox.RemovalCheck.BackupRequired。
func (s *Service) GuardKEKRemoval(dir, kekID string, dropCompat bool) (required []string, err error) {
	need, unreadable, err := s.RequiredKEKs(dir)
	if err != nil {
		return nil, err
	}
	for id := range need {
		required = append(required, id)
	}
	sort.Strings(required)
	if dropCompat {
		return required, nil
	}
	if files := need[kekID]; len(files) > 0 {
		return required, fmt.Errorf("%w：%s", secretbox.ErrKEKRequiredByBackup, strings.Join(files, "、"))
	}
	if len(unreadable) > 0 {
		return required, fmt.Errorf("%w：存在无法解析的备份 %s，无法确认它们不需要 kek_id=%s", secretbox.ErrKEKRequiredByBackup, strings.Join(unreadable, "、"), kekID)
	}
	return required, nil
}
